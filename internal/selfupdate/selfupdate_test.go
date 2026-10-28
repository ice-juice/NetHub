package selfupdate

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 造一个"更新包"：nethub.exe（带假 PE 头）+ 驱动 + 一个不该被拿走的 config.yaml
func makeZip(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "NetHub.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	pe := make([]byte, 128)
	pe[0], pe[1] = 'M', 'Z'
	pe[0x3c] = 64 // e_lfanew = 64
	pe[64], pe[65], pe[66], pe[67] = 'P', 'E', 0, 0
	add := func(name string, data []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	add("nethub.exe", pe)
	add("WinDivert.dll", []byte("new-dll"))
	add("WinDivert64.sys", []byte("new-sys"))
	add("config.yaml", []byte("恶意内容：更新包绝不该覆盖配置"))
	add("装机说明.txt", []byte("无关文件"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

// 暂存 → 换 exe → 重启后收尾：整条链路走一遍，并确认**配置没被碰**。
func TestStageSwapApply(t *testing.T) {
	prog := t.TempDir()
	// 现状：旧程序 + 旧驱动 + 配置
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(prog, name), []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("nethub.exe", "old-exe")
	write("WinDivert.dll", "old-dll")
	write("WinDivert64.sys", "old-sys")
	write("config.yaml", "我的配置")
	write("secrets.dat", "我的口令箱")

	zp := makeZip(t, t.TempDir())
	p, err := Stage(zp, prog, "v9.9.9", 4242)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 3 {
		t.Fatalf("白名单过滤不对，应只取 3 个文件：%v", p.Files)
	}
	if len(p.Hashes) != 3 {
		t.Fatalf("Stage 应记下每个白名单文件的摘要：%v", p.Hashes)
	}
	for _, name := range p.Files {
		raw, err := os.ReadFile(filepath.Join(prog, StageDirName, name))
		if err != nil {
			t.Fatalf("读暂存 %s: %v", name, err)
		}
		if p.Hashes[name] != sha256hex(raw) {
			t.Errorf("%s 摘要对不上：pending=%s 文件=%s", name, p.Hashes[name], sha256hex(raw))
		}
	}
	if _, err := os.Stat(filepath.Join(prog, StageDirName, "config.yaml")); !os.IsNotExist(err) {
		t.Fatal("更新包里的 config.yaml 不该被解出来")
	}

	// 换 exe（运行中也能做）
	if err := SwapRunningExe(prog); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(prog, "nethub.exe")); string(got[:2]) != "MZ" {
		t.Errorf("exe 没换成新的：%q", got)
	}
	if _, err := os.Stat(filepath.Join(prog, "nethub.exe"+OldSuffix)); err != nil {
		t.Error("旧 exe 应改名保留（用于回滚）")
	}

	// 重启后收尾：替换驱动（DLL/SYS）
	done, err := ApplyPending(prog, func(pid int, max time.Duration) bool { return true })
	if err != nil {
		t.Fatalf("收尾失败：%v", err)
	}
	if len(done) != 2 {
		t.Errorf("应替换 DLL 与 SYS 两个文件（exe 已换过）：%v", done)
	}
	if got, _ := os.ReadFile(filepath.Join(prog, "WinDivert.dll")); string(got) != "new-dll" {
		t.Errorf("DLL 没换：%q", got)
	}
	// 用户数据一个都不能动
	for _, name := range []string{"config.yaml", "secrets.dat"} {
		if _, err := os.Stat(filepath.Join(prog, name)); err != nil {
			t.Errorf("%s 被更新弄丢了", name)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(prog, "config.yaml")); string(got) != "我的配置" {
		t.Errorf("配置被覆盖了：%q", got)
	}
	// 标记应被清掉，且保留 exe.old 供回滚
	if _, ok := HasPending(prog); ok {
		t.Error("收尾后不该再有 pending")
	}
	if !HasRollback(prog) {
		t.Error("应保留上一版本供回滚")
	}
}

// 回滚：把 .old 换回来。
func TestRollback(t *testing.T) {
	prog := t.TempDir()
	if err := os.WriteFile(filepath.Join(prog, "nethub.exe"), []byte("bad-new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prog, "nethub.exe"+OldSuffix), []byte("good-old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(prog); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(prog, "nethub.exe")); string(got) != "good-old" {
		t.Errorf("回滚没生效：%q", got)
	}
	if _, err := os.Stat(filepath.Join(prog, "nethub.exe.bad")); err != nil {
		t.Error("坏版本应改名保留（.bad）")
	}
}

// 坏包要拒绝：没有 exe / exe 不是 PE。
func TestStageRejectsBadPackages(t *testing.T) {
	prog := t.TempDir()
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("readme.txt")
	w.Write([]byte("没有 exe"))
	zw.Close()
	f.Close()
	if _, err := Stage(p, prog, "v1", 0); err == nil {
		t.Error("没有 nethub.exe 的包应该被拒绝")
	}

	p2 := filepath.Join(dir, "fake.zip")
	f2, _ := os.Create(p2)
	zw2 := zip.NewWriter(f2)
	w2, _ := zw2.Create("nethub.exe")
	w2.Write([]byte("这不是 PE 文件"))
	zw2.Close()
	f2.Close()
	if _, err := Stage(p2, prog, "v1", 0); err == nil {
		t.Error("nethub.exe 不是 PE 的包应该被拒绝")
	}
}

// 暂存驱动被改过：收尾必须拒绝，程序目录里的驱动不能被动。
func TestApplyPendingRejectsTamperedSys(t *testing.T) {
	prog := t.TempDir()
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(prog, name), []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("nethub.exe", "old-exe")
	write("WinDivert.dll", "old-dll")
	write("WinDivert64.sys", "old-sys")

	zp := makeZip(t, t.TempDir())
	if _, err := Stage(zp, prog, "v9.9.9", 1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prog, StageDirName, "WinDivert64.sys"), []byte("tampered-sys"), 0o755); err != nil {
		t.Fatal(err)
	}

	done, err := ApplyPending(prog, func(int, time.Duration) bool { return true })
	if err == nil {
		t.Fatal("篡改过的暂存驱动应收尾失败")
	}
	if len(done) != 0 {
		t.Errorf("对不上摘要时一个文件都不该写：%v", done)
	}
	if got, _ := os.ReadFile(filepath.Join(prog, "WinDivert64.sys")); string(got) != "old-sys" {
		t.Errorf("程序目录驱动不该被动：%q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(prog, "WinDivert.dll")); string(got) != "old-dll" {
		t.Errorf("程序目录 DLL 也不该被动：%q", got)
	}
	if _, ok := HasPending(prog); !ok {
		t.Error("失败应收下 pending，方便重新下载")
	}
}

// 暂存 exe 被改过：换 exe 必须拒绝，当前程序不能被动。
func TestSwapRunningExeRejectsTamperedExe(t *testing.T) {
	prog := t.TempDir()
	if err := os.WriteFile(filepath.Join(prog, "nethub.exe"), []byte("old-exe"), 0o755); err != nil {
		t.Fatal(err)
	}
	zp := makeZip(t, t.TempDir())
	if _, err := Stage(zp, prog, "v9.9.9", 1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prog, StageDirName, "nethub.exe"), []byte("tampered-exe"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := SwapRunningExe(prog); err == nil {
		t.Fatal("篡改过的暂存 exe 应拒绝替换")
	}
	if got, _ := os.ReadFile(filepath.Join(prog, "nethub.exe")); string(got) != "old-exe" {
		t.Errorf("当前 exe 不该被动：%q", got)
	}
	if _, err := os.Stat(filepath.Join(prog, "nethub.exe"+OldSuffix)); !os.IsNotExist(err) {
		t.Error("失败时不该把当前 exe 改名成 .old")
	}
	if _, ok := HasPending(prog); !ok {
		t.Error("失败应收下 pending，方便重新下载")
	}
}
