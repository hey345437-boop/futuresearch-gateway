// setup.go 两件「点一下就接入」的事：
//
//  1. 把本机 dsh 的 provider 配好（写 ~/.dsh/settings.yaml + .credentials.yaml，先备份）
//  2. 本地预览目录：agent 生成的 HTML/项目直接挂到 /preview/ 上，不用手存文件再打开
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 一键接入 dsh
// ---------------------------------------------------------------------------

// dshPaths 本机 dsh 的配置文件位置。
func dshPaths() (settings, creds string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", false
	}
	dir := filepath.Join(home, ".dsh")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", "", false
	}
	return filepath.Join(dir, "settings.yaml"), filepath.Join(dir, ".credentials.yaml"), true
}

// handleSetupDsh 把 provider 写进 dsh 配置。
//
// 做法是**文本级**替换（不引 YAML 依赖）：定位 `    <provider>:` 到下一个同级键，
// 整块换掉。写之前先备份成 .bak-<时间戳>，出错能立刻还原。
func (s *Server) handleSetupDsh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	name := strings.TrimSpace(req.Provider)
	if name == "" {
		name = "futuresearch"
	}
	if !validProviderName(name) {
		writeJSON(w, 400, map[string]any{"error": "provider 名只能是字母/数字/下划线/连字符"})
		return
	}
	settings, creds, ok := dshPaths()
	if !ok {
		writeJSON(w, 400, map[string]any{"error": "没找到 ~/.dsh 目录（这台机器上没装 dsh？）"})
		return
	}

	base := "http://" + s.cfg.Listen.Addr() + "/v1"
	if s.cfg.Listen.Host == "0.0.0.0" || s.cfg.Listen.Host == "" {
		base = "http://127.0.0.1:" + fmt.Sprint(s.cfg.Listen.Port) + "/v1"
	}
	keyEnv := "FSGW_KEY"
	key := strings.TrimSpace(s.cfg.APIKey)
	if key == "" {
		key = "no-key-needed" // 网关没设 key：占位，避免 dsh 报「缺凭据」
	}

	raw, err := os.ReadFile(settings)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "读 settings.yaml 失败：" + err.Error()})
		return
	}
	bak := fmt.Sprintf("%s.bak-%s", settings, timeStamp())
	if err := os.WriteFile(bak, raw, 0o600); err != nil {
		writeJSON(w, 500, map[string]any{"error": "备份失败：" + err.Error()})
		return
	}

	updated, action, err := spliceProvider(string(raw), name, base, keyEnv)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error(), "backup": bak})
		return
	}
	if err := os.WriteFile(settings, []byte(updated), 0o600); err != nil {
		writeJSON(w, 500, map[string]any{"error": "写 settings.yaml 失败：" + err.Error(), "backup": bak})
		return
	}
	if err := upsertCredential(creds, keyEnv, key); err != nil {
		writeJSON(w, 500, map[string]any{"error": "写凭据失败：" + err.Error(), "backup": bak})
		return
	}
	s.appendLog(fmt.Sprintf("已把 provider %q 写入 dsh 配置（%s），备份 %s", name, action, filepath.Base(bak)))
	writeJSON(w, 200, map[string]any{
		"ok": true, "provider": name, "base_url": base, "action": action,
		"settings": settings, "credentials": creds, "backup": bak,
		"models": len(modelsForDsh()),
	})
}

func validProviderName(n string) bool {
	if n == "" || len(n) > 40 {
		return false
	}
	for _, c := range n {
		if !(c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func timeStamp() string {
	return timeNowStr()
}

// modelsForDsh 生成 dsh 的模型清单（预设档 + 家族，档位走 reasoningEfforts）。
func modelsForDsh() []map[string]any {
	out := []map[string]any{}
	for _, m := range staticModelsForSetup() {
		out = append(out, map[string]any{"id": m.ID, "name": m.Name})
	}
	return out
}

// renderDshProvider 生成 provider 的 YAML 块（缩进按 dsh 的约定：providers 下 4 空格）。
func renderDshProvider(indent, name, base, keyEnv string) string {
	var b strings.Builder
	i2, i3, i4 := indent+"  ", indent+"    ", indent+"      "
	fmt.Fprintf(&b, "%s%s:\n", indent, name)
	fmt.Fprintf(&b, "%sdisplayName: FutureSearch Gateway\n", i2)
	fmt.Fprintf(&b, "%sapiKeyEnv: %s\n", i2, keyEnv)
	fmt.Fprintf(&b, "%sapi: openai-completions\n", i2)
	fmt.Fprintf(&b, "%sbaseURL: %s\n", i2, base)
	fmt.Fprintf(&b, "%smodels:\n", i2)
	for _, m := range staticModelsForSetup() {
		fmt.Fprintf(&b, "%s- id: %s\n", i3, m.ID)
		fmt.Fprintf(&b, "%sname: %q\n", i4, m.Name)
		fmt.Fprintf(&b, "%scontextWindow: 200000\n", i4)
		fmt.Fprintf(&b, "%smaxTokens: 32000\n", i4)
		if len(m.Efforts) > 0 {
			fmt.Fprintf(&b, "%sreasoningEfforts:\n", i4)
			for _, lv := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
				if w, ok := m.Efforts[lv]; ok {
					fmt.Fprintf(&b, "%s%s: %s\n", i4+"  ", lv, w)
				}
			}
		} else {
			fmt.Fprintf(&b, "%sreasoningEfforts: false\n", i4)
		}
	}
	return b.String()
}

// spliceProvider 把 name 这个 provider 块换成 block；不存在就插到 providers: 下面。
//
// **不写死缩进** —— dsh 的 providers: 嵌在 `llm-pi-ai:` 下面，不同版本层级不同，
// 所以先找到 `providers:` 那一行，用它的缩进推出子项缩进。
func spliceProvider(doc, name, base, keyEnv string) (string, string, error) {
	lines := strings.Split(doc, "\n")
	provIdx, provIndent := -1, ""
	for i, l := range lines {
		if strings.TrimSpace(l) == "providers:" {
			provIdx = i
			provIndent = l[:len(l)-len(strings.TrimLeft(l, " \t"))]
			break
		}
	}
	if provIdx < 0 {
		return "", "", fmt.Errorf("没在 settings.yaml 里找到 providers: 段，请手动粘贴")
	}
	childIndent := provIndent + "  "
	block := strings.Split(strings.TrimRight(renderDshProvider(childIndent, name, base, keyEnv), "\n"), "\n")

	head := childIndent + name + ":"
	start, end := -1, -1
	for i := provIdx + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " \t"))
		if t != "" && indent <= len(provIndent) {
			break // 走出 providers 段
		}
		if start < 0 {
			if strings.TrimRight(lines[i], " \t") == head {
				start = i
			}
			continue
		}
		if t != "" && indent <= len(childIndent) {
			end = i
			break
		}
	}
	if start >= 0 {
		if end < 0 {
			end = len(lines)
		}
		merged := append(append([]string{}, lines[:start]...), block...)
		merged = append(merged, lines[end:]...)
		return strings.Join(merged, "\n"), "已更新", nil
	}
	merged := append(append([]string{}, lines[:provIdx+1]...), block...)
	merged = append(merged, lines[provIdx+1:]...)
	return strings.Join(merged, "\n"), "已新增", nil
}

// upsertCredential 在 .credentials.yaml 的 refs: 段里写入/更新一个键。
func upsertCredential(path, key, val string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	doc := string(raw)
	if doc == "" {
		doc = "version: 1\nrefs:\n"
	}
	lines := strings.Split(doc, "\n")
	replaced := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), key+":") {
			indent := l[:len(l)-len(strings.TrimLeft(l, " "))]
			lines[i] = indent + key + ": " + val
			replaced = true
			break
		}
	}
	if !replaced {
		for i, l := range lines {
			if strings.TrimRight(l, " ") == "refs:" {
				lines = append(lines[:i+1], append([]string{"  " + key + ": " + val}, lines[i+1:]...)...)
				replaced = true
				break
			}
		}
	}
	if !replaced {
		lines = append(lines, "refs:", "  "+key+": "+val)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// 本地预览目录
// ---------------------------------------------------------------------------

// handlePreview 列出预览目录里的文件（面板用）。
func (s *Server) handlePreviewList(w http.ResponseWriter, r *http.Request) {
	dir := s.cfg.Preview.Dir
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, 200, map[string]any{"dir": dir, "files": []any{}, "note": "目录还不存在"})
		return
	}
	type f struct {
		Name  string `json:"name"`
		Size  int64  `json:"size"`
		Mtime string `json:"mtime"`
		IsDir bool   `json:"is_dir"`
	}
	out := make([]f, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, f{Name: e.Name(), Size: info.Size(), Mtime: info.ModTime().Format("01-02 15:04"), IsDir: e.IsDir()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mtime > out[j].Mtime })
	writeJSON(w, 200, map[string]any{"dir": dir, "files": out, "url": s.cfg.Preview.Path})
}

// previewHandler 静态文件服务（生成的 HTML 直接能开）。
func (s *Server) previewHandler() http.Handler {
	dir := s.cfg.Preview.Dir
	_ = os.MkdirAll(dir, 0o755)
	fs := http.FileServer(http.Dir(dir))
	return http.StripPrefix(strings.TrimSuffix(s.cfg.Preview.Path, "/"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fs.ServeHTTP(w, r)
	}))
}
