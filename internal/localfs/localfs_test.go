package localfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestFS(t *testing.T, cfg Config) *FS {
	t.Helper()
	root := t.TempDir()
	if cfg.Root == "" {
		cfg.Root = root
	}
	f, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

// TestResolveBlocksTraversal ★ 最关键的安全属性：任何越界路径都必须被拒。
func TestResolveBlocksTraversal(t *testing.T) {
	f := newTestFS(t, Config{})
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("别读我"), 0o644)
	_ = os.Symlink(outside, filepath.Join(f.Root(), "escape")) // root 里放个软链指到外面

	bad := []string{
		"../secret.txt",
		"../../etc/passwd",
		"escape/secret.txt", // 经软链逃逸
		"a/../../secret.txt",
	}
	for _, p := range bad {
		if _, err := f.resolve(p); err == nil {
			t.Errorf("resolve(%q) 应该被拒，却通过了", p)
		}
	}
	// 绝对路径不报错，但会被**解释成相对 root** —— 安全的方向：模型写 /etc/passwd
	// 只会落到 <root>/etc/passwd，碰不到真实系统文件。
	if got, err := f.resolve("/etc/passwd"); err != nil {
		t.Errorf("绝对路径应被当作 root 相对：%v", err)
	} else if !strings.HasPrefix(got, f.Root()) {
		t.Errorf("绝对路径必须仍落在 root 内：%s", got)
	}
	// 正常路径要能过
	if _, err := f.resolve("sub/dir/file.txt"); err != nil {
		t.Errorf("root 内的路径不该被拒：%v", err)
	}
}

// TestWriteRequiresOptIn 写操作默认关闭。
func TestWriteRequiresOptIn(t *testing.T) {
	f := newTestFS(t, Config{})
	if _, err := f.WriteFile("a.txt", "x"); err == nil {
		t.Fatal("allow_write=false 时写文件必须失败")
	}
	f2 := newTestFS(t, Config{AllowWrite: true})
	msg, err := f2.WriteFile("sub/a.txt", "hello")
	if err != nil {
		t.Fatalf("开了写就该成功：%v", err)
	}
	if !strings.Contains(msg, "sub/a.txt") {
		t.Fatalf("回执应带相对路径：%s", msg)
	}
	got, err := os.ReadFile(filepath.Join(f2.Root(), "sub", "a.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("文件内容不对：%q %v", got, err)
	}
}

// TestExecRequiresOptIn 执行命令默认关闭，且不走 shell。
func TestExecRequiresOptIn(t *testing.T) {
	f := newTestFS(t, Config{})
	if _, err := f.RunCommand(context.Background(), []string{"echo", "hi"}, ""); err == nil {
		t.Fatal("allow_exec=false 时执行命令必须失败")
	}

	f2 := newTestFS(t, Config{AllowExec: true})
	out, err := f2.RunCommand(context.Background(), []string{"echo", "hi"}, "")
	if err != nil || !strings.Contains(out, "hi") {
		t.Fatalf("开了执行就该成功：%q %v", out, err)
	}
	// 不走 shell：`;` 不会被解释成分隔符（真 shell 会输出两行 a / b）
	out2, _ := f2.RunCommand(context.Background(), []string{"echo", "a;", "echo", "b"}, "")
	body := out2[strings.Index(out2, "\n")+1:]
	if strings.TrimSpace(body) != "a; echo b" {
		t.Fatalf("`;` 被 shell 解释了（argv 没逐项传）：%q", out2)
	}
}

// TestRunCommandTimeout 超时会被杀掉，且回执里说明。
func TestRunCommandTimeout(t *testing.T) {
	f := newTestFS(t, Config{AllowExec: true, TimeoutSec: 1})
	out, err := f.RunCommand(context.Background(), []string{"sleep", "5"}, "")
	if err != nil {
		t.Fatalf("超时不该返回 error（要回执说明）：%v", err)
	}
	if !strings.Contains(out, "超时") {
		t.Fatalf("回执应说明超时：%q", out)
	}
}

// TestReadFileRejectsBinaryAndBig 二进制与大文件都拒。
func TestReadFileRejectsBinaryAndBig(t *testing.T) {
	f := newTestFS(t, Config{AllowWrite: true, MaxReadKB: 1})
	if _, err := f.WriteFile("bin.dat", "a\x00b"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadFile("bin.dat", 0, 0); err == nil {
		t.Fatal("二进制文件必须被拒")
	}
	big := strings.Repeat("x", 4096)
	if _, err := f.WriteFile("big.txt", big); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadFile("big.txt", 0, 0); err == nil {
		t.Fatal("超过 MaxReadKB 的文件必须被拒")
	}
	// 小文件带行号
	if _, err := f.WriteFile("s.txt", "第一行\n第二行"); err != nil {
		t.Fatal(err)
	}
	out, err := f.ReadFile("s.txt", 0, 0)
	if err != nil || !strings.Contains(out, "1│ 第一行") {
		t.Fatalf("读文件应带行号：%q %v", out, err)
	}
}

// TestSearch 搜内容，跳过 .git。
func TestSearch(t *testing.T) {
	f := newTestFS(t, Config{AllowWrite: true})
	_, _ = f.Mkdir(".git")
	_, _ = f.WriteFile(".git/config", "NEEDLE")
	_, _ = f.WriteFile("a.go", "package x\n// NEEDLE here\n")
	out, err := f.Search("NEEDLE", "", "*.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:2") {
		t.Fatalf("应命中 a.go 第 2 行：%q", out)
	}
	if strings.Contains(out, ".git") {
		t.Fatalf(".git 应被跳过：%q", out)
	}
	if _, err := f.Search("([", "", ""); err == nil {
		t.Fatal("非法正则应报错")
	}
}

// TestListDirSkipsNoise 列目录跳过隐藏项与 node_modules。
func TestListDirSkipsNoise(t *testing.T) {
	f := newTestFS(t, Config{AllowWrite: true})
	_, _ = f.WriteFile("keep.txt", "x")
	_, _ = f.WriteFile(".hidden", "x")
	_, _ = f.WriteFile("node_modules/pkg/index.js", "x")
	out, err := f.ListDir("", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "keep.txt") || strings.Contains(out, "node_modules") || strings.Contains(out, ".hidden") {
		t.Fatalf("列目录结果不对：\n%s", out)
	}
}
