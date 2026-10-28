package webui

import (
	"archive/zip"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"nethub/internal/config"
)

// 配置的导出与导入（「设置 → 配置：导出与导入」这张卡）。
//
// 设计取向：**一个文件就够**。
//   - 导出（含凭据）= 完整 config.yaml，新机器装好程序后「导入配置」选它即可开箱即用；
//   - 导出（无凭据）= 同样能导入的 config.yaml，只是把上游 URL 里的用户名/口令剔除
//     （适合发给人看、或存进仓库做版本记录）；
//   - 导入 = 选文件 → 先校验 → 自动备份当前 → 生效并重启；校验不过什么都不动。
//
// 刻意**不做**"装机包"（程序 + 驱动 + 配置 + 脚本打成一个 zip）：
// 程序怎么分发是发布流程的事（Release 附件），配置怎么给才是这个页面的职责，
// 混在一起只会让两件事都变复杂。

// ExportConfigRedacted 导出"能导入、但没凭据"的配置：上游 URL 里的用户名/口令被剔除。
func (b *Backend) ExportConfigRedacted() (string, error) {
	name := "nethub-config-无凭据.yaml"
	if p := b.a.Cfg.Path(); p != "" {
		name = filepath.Join(dirOf(p), name)
	}
	path, err := b.uiOrNoop().SaveFile(wruntime.SaveDialogOptions{
		Title:           "导出配置（无凭据，仍可导入）",
		DefaultFilename: name,
		Filters: []wruntime.FileFilter{
			{DisplayName: "配置文件 (*.yaml)", Pattern: "*.yaml"},
			{DisplayName: "所有文件 (*.*)", Pattern: "*.*"},
		},
	})
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil
	}
	if !strings.HasSuffix(strings.ToLower(path), ".yaml") && !strings.HasSuffix(strings.ToLower(path), ".yml") {
		path += ".yaml"
	}
	if err := b.a.Cfg.SaveAsRedacted(path); err != nil {
		return "", err
	}
	b.a.Bus.Info("已导出无凭据配置：%s（可直接导入，需补上游口令）", filepath.Base(path))
	return path, nil
}

// ImportConfig 导入一个配置文件（.yaml/.yml，或含 config.yaml 的 .zip）。
//
// 顺序很讲究：**先校验 → 再备份 → 才落盘**。校验不过就什么都不动，
// 免得把一台正在干活的机器改成起不来。
func (b *Backend) ImportConfig() (string, error) {
	dir := dirOf(b.a.Cfg.Path())
	path, err := b.uiOrNoop().OpenFile(wruntime.OpenDialogOptions{
		Title:            "导入配置（.yaml 或含 config.yaml 的 .zip）",
		DefaultDirectory: dir,
		Filters: []wruntime.FileFilter{
			{DisplayName: "配置/压缩包 (*.yaml;*.yml;*.zip)", Pattern: "*.yaml;*.yml;*.zip"},
			{DisplayName: "所有文件 (*.*)", Pattern: "*.*"},
		},
	})
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil // 用户取消
	}

	raw, err := readConfigFile(path)
	if err != nil {
		return "", err
	}
	if _, err := b.importConfigFromRaw(path, raw); err != nil {
		return "", err
	}
	// 生效：重启服务（这一步在测试里会失败——引擎要驱动；配置本身已经落盘并生效）
	if err := b.a.Restart(); err != nil {
		return path, fmt.Errorf("配置已导入，但重启服务失败：%w", err)
	}
	return path, nil
}

// importConfigFrom 从文件导入（不含重启，便于测试）。
func (b *Backend) importConfigFrom(path string) (string, error) {
	raw, err := readConfigFile(path)
	if err != nil {
		return "", err
	}
	return b.importConfigFromRaw(path, raw)
}

// importConfigFromRaw 校验 → 备份 → 落盘 → 重新载入（内存里生效）。
func (b *Backend) importConfigFromRaw(srcPath string, raw []byte) (string, error) {
	// ① 先落成临时文件做校验（走真正的 Load/Validate 路径，不做"简化版校验"）
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("nethub-import-%d.yaml", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	loaded, err := config.Load(tmp)
	if err != nil {
		return "", fmt.Errorf("这个配置不能用：%w", err)
	}
	if err := loaded.Validate(); err != nil {
		return "", fmt.Errorf("这个配置校验不过：%w", err)
	}

	// ② 备份当前（含时间戳，出问题能回滚）
	if err := b.a.Cfg.BackupNow(); err != nil {
		return "", err
	}
	// 本机偏好（日志档位）不跟着导入走：导入的是“要派发给谁的配置内容”，
	// 而“这台机器看日志的习惯”是操作人刚才自己选的。
	prefs := b.a.Cfg.LocalPrefs()
	// ③ 落盘并生效：保持 *b.a.Cfg 这个指针不变（引擎/规则都指着它）
	raw = ensurePathInFile(raw, b.a.Cfg.Path())
	if err := os.WriteFile(b.a.Cfg.Path(), raw, 0o600); err != nil {
		return "", err
	}
	cur, err := config.Load(b.a.Cfg.Path())
	if err != nil {
		return "", fmt.Errorf("导入后重新载入失败（已备份，可从 backups 目录回滚）：%w", err)
	}
	if cur.SetLocalPrefs(prefs) {
		// 导入的那份档位与本机不同 -> 写回文件（否则重启后又变成导入的那份的档位）
		if err := cur.Save(); err != nil {
			return "", fmt.Errorf("已导入，但本机日志档位（%s）写不回去：%w", prefs.LogLevel, err)
		}
		b.a.Bus.Info("导入的配置里日志档位是别的值，已保留本机设置（%s）", prefs.LogLevel)
	}
	b.a.Cfg.ReplaceFrom(cur)
	b.a.Bus.Info("已导入配置：%s（%d 条链，%d 条规则）", filepath.Base(srcPath), len(cur.Chains), len(cur.Routes))
	return srcPath, nil
}

// readConfigFile 读配置：直接 .yaml，或从 .zip 里取第一份 yaml（优先 config.yaml）。
func readConfigFile(path string) ([]byte, error) {
	if !strings.EqualFold(filepath.Ext(path), ".zip") {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("打开压缩包失败: %w", err)
	}
	defer zr.Close()
	var fallback []byte
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := strings.ToLower(filepath.Base(f.Name))
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(rc, 4<<20)) // 4MB 够放任何配置了
		rc.Close()
		if name == "config.yaml" || name == "config.yml" {
			return raw, nil
		}
		if fallback == nil {
			fallback = raw
		}
	}
	if fallback == nil {
		return nil, fmt.Errorf("压缩包里没有 .yaml/.yml 配置文件")
	}
	return fallback, nil
}

// ensurePathInFile 导入时不改对方的 relay 等字段，只做一件事：
// 把 exportedConfig 里可能残留的注释头补上，保持文件可读。
func ensurePathInFile(raw []byte, _ string) []byte {
	s := string(raw)
	if strings.HasPrefix(s, "#") {
		return raw
	}
	return []byte("# NetHub 配置 —— 由「导入配置」写入\n" + s)
}

// ImportConfigFromURL 从 URL 拉一份配置导入（集中更新配置的最小形态）。
//
// 刻意只做"手动触发"：自动轮询会引入"谁在什么时候把什么推到我机器上"的问题，
// 现场排障时最怕这种说不清的变化。要集中管理就先发个链接，让人点一下。
func (b *Backend) ImportConfigFromURL(rawURL string) (string, error) {
	u, err := parseImportURL(rawURL)
	if err != nil {
		return "", err
	}
	body, err := httpGet(u.String())
	if err != nil {
		return "", err
	}
	if _, err := b.importConfigFromRaw(u.String(), body); err != nil {
		return "", err
	}
	if err := b.a.Restart(); err != nil {
		return u.String(), fmt.Errorf("配置已导入，但重启服务失败：%w", err)
	}
	return u.String(), nil
}

// parseImportURL 把用户贴进来的配置地址收成安全的绝对 URL。
//
// 现场用法是运维手工贴内网地址（10/8、172.16/12、192.168/16 必须放行），
// 要挡的是解析把戏：带 userinfo 的 https://可信名@169.254.169.254/…
// 以及本机 / 链路本地 / 0.0.0.0/8。跨主机重定向在 CheckRedirect 里拦。
func parseImportURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("请填配置地址（http/https）")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("地址解析失败：%w", err)
	}
	if err := checkImportURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

// checkImportURL 协议 / userinfo / 主机：与重定向每一跳共用。
func checkImportURL(u *url.URL) error {
	if err := checkImportURLShape(u); err != nil {
		return err
	}
	return checkImportHost(u.Hostname())
}

// checkImportURLShape 只看写法（不解析 DNS），方便重定向先判跨主机再决定要不要查主机。
func checkImportURLShape(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("地址解析失败")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("只支持 http/https 地址")
	}
	if u.User != nil {
		return fmt.Errorf("地址里不能带用户名或口令")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("地址缺少主机名")
	}
	return nil
}

// checkImportHost 主机名：纯 IP 直接判；否则解析后每一条地址都过 classifyImportIP。
func checkImportHost(host string) error {
	if host == "" {
		return fmt.Errorf("地址缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		return classifyImportIP(ip)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("解析主机名失败：%w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("主机名解析不到地址")
	}
	for _, ip := range ips {
		if err := classifyImportIP(ip); err != nil {
			return err
		}
	}
	return nil
}

// classifyImportIP 判定这个 IP 能不能当配置下载目标。纯函数，方便单测，避免真去连 10.x。
//
// 拒：环回、链路本地（169.254/16、IPv6 fe80::/10）、0.0.0.0/8。
// 放行：RFC1918 与其它地址。
func classifyImportIP(ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("地址无效")
	}
	if ip.IsLoopback() {
		return fmt.Errorf("不能从本机地址下载配置")
	}
	if ip.IsLinkLocalUnicast() {
		return fmt.Errorf("不能从链路本地地址下载配置")
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 0 {
		return fmt.Errorf("不能从这个地址下载配置")
	}
	if ip.To4() == nil && ip.IsUnspecified() {
		return fmt.Errorf("不能从这个地址下载配置")
	}
	return nil
}

// checkImportRedirect 每一跳重跑同一套地址检查，并禁止跨主机跳转
// （避免「公网短链 → 本机 / 云 metadata」）。
func checkImportRedirect(req *http.Request, via []*http.Request) error {
	if req == nil || req.URL == nil {
		return fmt.Errorf("重定向地址看不懂")
	}
	if len(via) >= 10 {
		return fmt.Errorf("重定向次数过多")
	}
	if err := checkImportURLShape(req.URL); err != nil {
		return err
	}
	if len(via) > 0 {
		prev := via[len(via)-1]
		if prev == nil || prev.URL == nil {
			return fmt.Errorf("重定向地址看不懂")
		}
		if !strings.EqualFold(req.URL.Hostname(), prev.URL.Hostname()) {
			return fmt.Errorf("不允许跳转到其它主机（%s → %s）", prev.URL.Hostname(), req.URL.Hostname())
		}
	}
	return checkImportHost(req.URL.Hostname())
}

// httpGet 简单拉取（限时 20s、限 4MB）。先过 parseImportURL，重定向走 checkImportRedirect。
func httpGet(raw string) ([]byte, error) {
	u, err := parseImportURL(raw)
	if err != nil {
		return nil, err
	}
	cl := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: checkImportRedirect,
	}
	resp, err := cl.Get(u.String())
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, fmt.Errorf("下载失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取失败：%w", err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("下载到的是空文件")
	}
	return body, nil
}
