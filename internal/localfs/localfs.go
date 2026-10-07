// Package localfs 把「本地项目」暴露成一组受限的文件/命令工具。
//
// 这是让 AI **真的能操作本地项目**的那一半 —— FutureSearch 的研究 agent 够不着你的磁盘
// （上游 API 没有工具回调面），所以本地手脚必须由本地进程提供，也就是这个包。
//
// 安全模型（默认收紧，显式放开）：
//   - 所有路径先解析成绝对路径 + 解符号链接，再校验**必须落在 root 之内**
//   - 写操作默认关闭（allow_write）
//   - 执行命令默认关闭（allow_exec），且**不走 shell**（显式 argv，避免 `;`/`|` 注入）
//   - 读取有大小上限、命令有超时与输出上限、搜索有结果上限
//   - 命令的环境变量清洗（去掉密钥类），工作目录强制在 root 内
package localfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Config 本地文件工具的开关与边界。
type Config struct {
	Enabled    bool   `json:"enabled"`
	Root       string `json:"root"`        // 允许操作的根目录（必填才能启用）
	AllowWrite bool   `json:"allow_write"` // 允许 write_file / mkdir
	AllowExec  bool   `json:"allow_exec"`  // 允许 run_command
	MaxReadKB  int    `json:"max_read_kb"` // 单次读取上限，默认 256
	MaxOutKB   int    `json:"max_out_kb"`  // 命令输出上限，默认 64
	TimeoutSec int    `json:"timeout_sec"` // 命令超时，默认 30
}

// FS 受限文件系统。
type FS struct {
	cfg  Config
	root string // 已 EvalSymlinks 的绝对路径
}

// New 建一个受限 FS。root 不存在会报错（不自动创建 —— 免得指错地方还悄悄建出来）。
func New(cfg Config) (*FS, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, errors.New("localfs: root 不能为空")
	}
	abs, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("localfs: root %s 不可用：%w", abs, err)
	}
	st, err := os.Stat(real)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("localfs: root %s 不是目录", real)
	}
	if cfg.MaxReadKB <= 0 {
		cfg.MaxReadKB = 256
	}
	if cfg.MaxOutKB <= 0 {
		cfg.MaxOutKB = 64
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 30
	}
	return &FS{cfg: cfg, root: real}, nil
}

// Root 生效的根目录。
func (f *FS) Root() string { return f.root }

// Config 当前配置副本。
func (f *FS) Config() Config { return f.cfg }

// resolve 把用户给的相对/绝对路径解析成 root 内的绝对路径。
//
// 关键点：对**已存在的部分**做 EvalSymlinks 再校验，防止 root 里放个软链指到外面去。
func (f *FS) resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	// 空 / "." / "/" 都表示根目录本身（工具的 schema 里写的就是「留空 = 根目录」）
	if p == "" || p == "." || p == string(filepath.Separator) {
		return f.root, nil
	}
	if filepath.IsAbs(p) {
		p = filepath.Clean(p)
		// 去掉根：Unix 的 "/"，Windows 的 "\" 以及盘符 "C:\"
		p = strings.TrimPrefix(p, string(filepath.Separator))
		if vol := filepath.VolumeName(p); vol != "" {
			p = strings.TrimPrefix(strings.TrimPrefix(p, vol), string(filepath.Separator))
		}
	}
	joined := filepath.Join(f.root, p)

	// 逐级向上找到第一个存在的祖先，对它 EvalSymlinks，再把剩余部分拼回去
	cur := joined
	var tail []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
	real, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", fmt.Errorf("路径不可用：%w", err)
	}
	full := filepath.Join(append([]string{real}, tail...)...)

	// 必须在 root 内（用分隔符边界比较，避免 /a/rootx 匹配 /a/root）。
	// Windows / macOS 默认大小写不敏感，比较时统一折一下，否则 C:\Root 会被判越界。
	if !withinRoot(full, f.root) {
		return "", fmt.Errorf("越界：%s 不在允许的根目录内", p)
	}
	return full, nil
}

// rel 转回相对 root 的路径（展示用）。
func (f *FS) rel(p string) string {
	if r, err := filepath.Rel(f.root, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

// ---------------------------------------------------------------------------
// 工具实现
// ---------------------------------------------------------------------------

// ListDir 列目录。
func (f *FS) ListDir(dir string, depth int) (string, error) {
	full, err := f.resolve(dir)
	if err != nil {
		return "", err
	}
	if depth <= 0 {
		depth = 1
	}
	if depth > 4 {
		depth = 4
	}
	var b strings.Builder
	base := strings.Count(strings.Trim(f.rel(full), "/"), "/")
	entries := 0
	err = filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 没权限的跳过，不中断
		}
		rel := f.rel(p)
		lvl := strings.Count(strings.Trim(rel, "/"), "/") - base
		if lvl >= depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, _ := d.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		kind := "f"
		if d.IsDir() {
			kind = "d"
		}
		indent := strings.Repeat("  ", lvl)
		if d.IsDir() {
			fmt.Fprintf(&b, "%s%s%s/\n", indent, kind, name)
		} else {
			fmt.Fprintf(&b, "%s%s %-40s %8d B\n", indent, kind, name, size)
		}
		entries++
		if entries > 2000 {
			return errors.New("目录太大（超过 2000 项），请缩小范围")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if entries == 0 {
		return "（空目录）", nil
	}
	return b.String(), nil
}

// ReadFile 读文本文件。
func (f *FS) ReadFile(path string, offset, limit int) (string, error) {
	full, err := f.resolve(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(full)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s 是目录，用 list_dir", path)
	}
	maxBytes := int64(f.cfg.MaxReadKB) * 1024
	if st.Size() > maxBytes {
		return "", fmt.Errorf("文件 %d 字节，超过上限 %d KB（用 offset/limit 分段读）", st.Size(), f.cfg.MaxReadKB)
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if isBinary(raw) {
		return "", fmt.Errorf("%s 看起来是二进制文件（%d 字节），不读", path, len(raw))
	}
	lines := strings.Split(string(raw), "\n")
	if offset > 0 {
		if offset >= len(lines) {
			return "", fmt.Errorf("offset %d 超出文件行数 %d", offset, len(lines))
		}
		lines = lines[offset:]
	}
	if limit > 0 && limit < len(lines) {
		lines = lines[:limit]
	}
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%5d│ %s\n", offset+i+1, l)
	}
	return b.String(), nil
}

// WriteFile 写文件（需要 allow_write）。父目录会自动创建。
func (f *FS) WriteFile(path, content string) (string, error) {
	if !f.cfg.AllowWrite {
		return "", errors.New("写操作已禁用（config 里 localfs.allow_write = false）")
	}
	full, err := f.resolve(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	// 原子写：先写临时文件再 rename，避免写一半崩掉留下半个文件
	tmp := full + ".tmp-localfs"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return fmt.Sprintf("已写入 %s（%d 字节）", f.rel(full), len(content)), nil
}

// Mkdir 建目录（需要 allow_write）。
func (f *FS) Mkdir(path string) (string, error) {
	if !f.cfg.AllowWrite {
		return "", errors.New("写操作已禁用（config 里 localfs.allow_write = false）")
	}
	full, err := f.resolve(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		return "", err
	}
	return "已创建 " + f.rel(full), nil
}

// Search 正则搜索文件内容（跳过 .git / node_modules / 二进制）。
func (f *FS) Search(pattern, subdir, glob string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("正则不合法：%w", err)
	}
	full, err := f.resolve(subdir)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	hits, files := 0, 0
	err = filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, name); !ok {
				return nil
			}
		}
		info, _ := d.Info()
		if info != nil && info.Size() > 2*1024*1024 {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil || isBinary(raw) {
			return nil
		}
		files++
		for i, line := range strings.Split(string(raw), "\n") {
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d: %s\n", f.rel(p), i+1, strings.TrimSpace(truncate(line, 200)))
				hits++
				if hits >= 300 {
					return errors.New("命中超过 300 条，请把正则写具体些")
				}
			}
		}
		if files > 5000 {
			return errors.New("扫描文件超过 5000 个，请缩小范围")
		}
		return nil
	})
	if err != nil {
		return b.String() + "\n（" + err.Error() + "）", nil
	}
	if hits == 0 {
		return "没有命中", nil
	}
	return b.String(), nil
}

// RunCommand 执行命令（需要 allow_exec）。**不走 shell**：argv 逐项传入。
func (f *FS) RunCommand(ctx context.Context, argv []string, dir string) (string, error) {
	if !f.cfg.AllowExec {
		return "", errors.New("执行命令已禁用（config 里 localfs.allow_exec = false）")
	}
	if len(argv) == 0 {
		return "", errors.New("argv 不能为空")
	}
	wd := f.root
	if strings.TrimSpace(dir) != "" {
		full, err := f.resolve(dir)
		if err != nil {
			return "", err
		}
		wd = full
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(f.cfg.TimeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = wd
	cmd.Env = scrubbedEnv()

	out, err := cmd.CombinedOutput()
	text := string(out)
	if len(text) > f.cfg.MaxOutKB*1024 {
		text = text[:f.cfg.MaxOutKB*1024] + "\n…（输出被截断）"
	}
	head := fmt.Sprintf("$ %s   （cwd=%s）\n", strings.Join(argv, " "), f.rel(wd))
	if cctx.Err() == context.DeadlineExceeded {
		return head + text + fmt.Sprintf("\n（超时 %ds 被杀）", f.cfg.TimeoutSec), nil
	}
	if err != nil {
		return head + text + fmt.Sprintf("\n（退出码非 0：%v）", err), nil
	}
	if strings.TrimSpace(text) == "" {
		text = "（无输出）"
	}
	return head + text, nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// scrubbedEnv 清洗环境变量：去掉密钥类，保留运行命令**必需**的那些。
//
// 名单要跨平台：Windows 上 PATH/PATHEXT/SystemRoot/COMSPEC 缺一不可
// （没有 SystemRoot，很多程序连 socket 都起不来；没有 PATHEXT，找不到 .exe）。
func scrubbedEnv() []string {
	keep := map[string]bool{
		// 通用
		"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "TERM": true,
		"TMPDIR": true, "USER": true, "SHELL": true,
		// Windows
		"PATHEXT": true, "SYSTEMROOT": true, "SYSTEMDRIVE": true, "WINDIR": true,
		"COMSPEC": true, "TEMP": true, "TMP": true, "USERPROFILE": true,
		"APPDATA": true, "LOCALAPPDATA": true, "PROGRAMDATA": true, "PROGRAMFILES": true,
		"PROGRAMFILES(X86)": true, "OS": true, "NUMBER_OF_PROCESSORS": true,
		"PROCESSOR_ARCHITECTURE": true, "HOMEDRIVE": true, "HOMEPATH": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		k := kv[:strings.IndexByte(kv, '=')]
		if keep[strings.ToUpper(k)] {
			out = append(out, kv)
		}
	}
	return out
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// SortFiles 给面板列目录用。
func SortFiles(entries []fs.DirEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
}

// withinRoot 判断 p 是否在 root 之内（大小写不敏感平台上折大小写比较）。
func withinRoot(p, root string) bool {
	if p == root {
		return true
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		lp, lr := strings.ToLower(p), strings.ToLower(root)
		return strings.HasPrefix(lp, lr+strings.ToLower(string(filepath.Separator)))
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}
