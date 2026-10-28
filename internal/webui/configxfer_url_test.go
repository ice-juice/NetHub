package webui

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 目标 host 分类：纯函数，不发起连接。内网 RFC1918 必须放行，
// 环回 / 链路本地 / 0.0.0.0/8 必须拒。
func TestClassifyImportIP(t *testing.T) {
	cases := []struct {
		ip   string
		ok   bool
		want string
	}{
		{"10.0.0.1", true, ""},
		{"10.255.255.254", true, ""},
		{"172.16.0.1", true, ""},
		{"172.31.255.1", true, ""},
		{"192.168.1.1", true, ""},
		{"192.168.0.1", true, ""},
		{"8.8.8.8", true, ""},
		{"1.1.1.1", true, ""},
		{"172.15.0.1", true, ""}, // 不在 172.16/12，但也不是危险段
		{"127.0.0.1", false, "本机"},
		{"127.0.0.2", false, "本机"},
		{"127.255.255.255", false, "本机"},
		{"169.254.169.254", false, "链路本地"},
		{"169.254.0.1", false, "链路本地"},
		{"169.254.255.255", false, "链路本地"},
		{"0.0.0.0", false, "这个地址"},
		{"0.1.2.3", false, "这个地址"},
		{"::1", false, "本机"},
		{"fe80::1", false, "链路本地"},
		{"::ffff:127.0.0.1", false, "本机"},
		{"::ffff:169.254.169.254", false, "链路本地"},
		{"::ffff:10.1.2.3", true, ""},
	}
	for _, tc := range cases {
		err := classifyImportIP(net.ParseIP(tc.ip))
		if tc.ok {
			if err != nil {
				t.Errorf("%s 应放行，得到 %v", tc.ip, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s 应拒绝", tc.ip)
			continue
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s 报错 %q 应含 %q", tc.ip, err.Error(), tc.want)
		}
	}
	if err := classifyImportIP(nil); err == nil {
		t.Error("空 IP 应拒绝")
	}
}

func TestParseImportURL(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want string
	}{
		{"", false, "请填"},
		{"  ", false, "请填"},
		{"ftp://10.0.0.1/c.yaml", false, "http/https"},
		{"file:///c:/nethub-config.yaml", false, "http/https"},
		{"https://user:pass@10.0.0.1/c.yaml", false, "用户名或口令"},
		{"https://trusted@169.254.169.254/latest/meta-data", false, "用户名或口令"},
		{"http://127.0.0.1/c.yaml", false, "本机"},
		{"http://127.0.0.1:8080/c.yaml", false, "本机"},
		{"http://169.254.169.254/latest/meta-data", false, "链路本地"},
		{"http://0.0.0.0/x", false, "这个地址"},
		{"http://10.1.2.3/nethub-config.yaml", true, ""},
		{"https://192.168.1.10:8443/cfg.yaml", true, ""},
		{"http://172.16.8.9/a.yml", true, ""},
	}
	for _, tc := range cases {
		u, err := parseImportURL(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("%q 应放行，得到 %v", tc.in, err)
			} else if u == nil || u.Hostname() == "" {
				t.Errorf("%q 解析结果缺主机", tc.in)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q 应拒绝", tc.in)
			continue
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q 报错 %q 应含 %q", tc.in, err.Error(), tc.want)
		}
	}
}

// 被拒的地址不能真的发出请求（userinfo / loopback / metadata）。
func TestImportURLBlockedDoesNotHitServer(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("relay: 127.0.0.1:0\nchains: []\nroutes: []\n"))
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	blocked := []string{
		srv.URL,
		"http://user:pass@" + hostPort + "/nethub-config.yaml",
		"https://trusted@" + hostPort + "/nethub-config.yaml",
		"http://169.254.169.254/latest/meta-data",
		"http://127.0.0.1/nethub-config.yaml",
	}
	for _, raw := range blocked {
		if _, err := httpGet(raw); err == nil {
			t.Errorf("%s 应该被拒绝", raw)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("不该对拦下的地址发起请求，hits=%d", n)
	}
}

// 外站 302 到 loopback / metadata / 另一 host 都拒，且不跟着跳。
func TestImportRedirectBlocked(t *testing.T) {
	var destHits atomic.Int32
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer dest.Close()

	cases := []struct {
		name string
		to   string
		want string
	}{
		{"loopback", dest.URL, "本机"},
		{"metadata", "http://169.254.169.254/latest/meta-data", "其它主机"},
		{"other-host", "http://example.com/nethub-config.yaml", "其它主机"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, tc.to, http.StatusFound)
			}))
			defer src.Close()

			cl := &http.Client{
				Timeout:       3 * time.Second,
				CheckRedirect: checkImportRedirect,
			}
			resp, err := cl.Get(src.URL)
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil {
				t.Fatal("危险重定向应该被拒绝")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错 %q 应含 %q", err.Error(), tc.want)
			}
		})
	}
	if n := destHits.Load(); n != 0 {
		t.Fatalf("不该跟着跳到 loopback，hits=%d", n)
	}
}

// 同主机内网重定向放行（构造请求，不真连 10.x）。
func TestCheckImportRedirectSameHostIntranet(t *testing.T) {
	req := mustImportReq(t, "http://10.1.2.3/new.yaml")
	via := []*http.Request{mustImportReq(t, "http://10.1.2.3/old.yaml")}
	if err := checkImportRedirect(req, via); err != nil {
		t.Fatalf("同主机内网重定向应放行：%v", err)
	}
	// http → https 升格、端口变了，主机没变：仍放行
	httpsReq := mustImportReq(t, "https://10.1.2.3:8443/new.yaml")
	if err := checkImportRedirect(httpsReq, via); err != nil {
		t.Fatalf("同主机升格 https 应放行：%v", err)
	}
}

func TestCheckImportRedirectCrossHostAndDanger(t *testing.T) {
	via := []*http.Request{mustImportReq(t, "https://cdn.example.com/nethub-config.yaml")}
	cases := []struct {
		name string
		to   string
		want string
	}{
		{"loopback", "http://127.0.0.1/c.yaml", "其它主机"},
		{"metadata", "http://169.254.169.254/latest/meta-data", "其它主机"},
		{"other-host", "https://other.example.com/c.yaml", "其它主机"},
		{"userinfo", "https://trusted@10.1.2.3/c.yaml", "用户名或口令"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkImportRedirect(mustImportReq(t, tc.to), via)
			if err == nil {
				t.Fatal("应拒绝")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错 %q 应含 %q", err.Error(), tc.want)
			}
		})
	}
}

func mustImportReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
