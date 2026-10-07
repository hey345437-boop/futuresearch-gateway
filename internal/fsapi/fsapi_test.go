package fsapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(base string) *Client {
	c := NewWithBase(base)
	c.PollInterval = time.Millisecond
	return c
}

func writeRaw(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("写假上游失败：%v", err)
	}
}

// fakeUpstream 最小 FutureSearch 假上游：投任务 → 状态 → 结果。
func fakeUpstream(t *testing.T, finalStatus, data string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /operations/", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k-1" {
			t.Errorf("投任务未带正确 Authorization：%q", got)
		}
		writeRaw(t, w, `{"task_id":"t-1","session_id":"s-1","status":"pending"}`)
	})
	mux.HandleFunc("GET /tasks/t-1/status", func(w http.ResponseWriter, r *http.Request) {
		writeRaw(t, w, `{"task_id":"t-1","status":"`+finalStatus+`",`+
			`"progress":{"pending":0,"running":0,"completed":1,"failed":0,"total":1}}`)
	})
	mux.HandleFunc("GET /tasks/t-1/result", func(w http.ResponseWriter, r *http.Request) {
		writeRaw(t, w, `{"task_id":"t-1","status":"completed","data":`+data+`}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testKey() *Key { return &Key{Key: "k-1", UID: "u-1", Email: "a@b.c"} }

// ---- 模型解析 ----

func TestLookupSpecPresetFamilyAndSlug(t *testing.T) {
	cases := map[string]string{
		"agent-medium":          "agent-medium",
		"default":               "agent-medium", // 别名
		"claude-fable-5":        "claude-fable-5",
		"claude-fable-5-max":    "claude-fable-5-max",
		"gpt-5.6-terra-high":    "gpt-5.6-terra-high",
		"futuresearch/grok-4.5": "grok-4.5",
	}
	for in, want := range cases {
		sp, ok := lookupSpec(in)
		if !ok || sp.ID != want {
			t.Fatalf("lookupSpec(%q) = (%q,%v)，want %q", in, sp.ID, ok, want)
		}
	}
	if _, ok := lookupSpec("no-such-model"); ok {
		t.Fatal("未知模型必须返回 false（本地 400），不得静默回落")
	}
}

func TestSplitTierMiniIsNotATier(t *testing.T) {
	if base, tier := splitTier("gpt-5-mini"); base != "gpt-5-mini" || tier != "" {
		t.Fatalf("GPT-5 Mini 是独立模型不是 GPT-5 的低档：(%q,%q)", base, tier)
	}
	if base, tier := splitTier("claude-fable-5-max"); base != "claude-fable-5" || tier != "max" {
		t.Fatalf("拆分错误：(%q,%q)", base, tier)
	}
}

// ---- 前置指令注入 ----

func TestPromptPrecedence(t *testing.T) {
	c := New()
	if c.PromptFor("agent-low") != "" {
		t.Fatal("未配置时应为空")
	}
	c.SetPrompts(map[string]string{"*": "默认", "agent-high": "专用", "empty": "   "})
	if got := c.PromptFor("agent-low"); got != "默认" {
		t.Fatalf("无精确命中应落 *：%q", got)
	}
	if got := c.PromptFor("agent-high"); got != "专用" {
		t.Fatalf("精确名应覆盖 *：%q", got)
	}
	if got := c.PromptFor("empty"); got != "" {
		t.Fatalf("显式空串 = 不注入（且不回落 *）：%q", got)
	}
	c.SetPrompts(nil)
	if c.PromptFor("agent-low") != "" {
		t.Fatal("SetPrompts(nil) 应清空")
	}
}

func TestChatStreamInjectsPromptFirst(t *testing.T) {
	var gotTask string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /operations/", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Task string `json:"task"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotTask = b.Task
		writeRaw(t, w, `{"task_id":"t-1","session_id":"s-1","status":"pending"}`)
	})
	mux.HandleFunc("GET /tasks/t-1/status", func(w http.ResponseWriter, r *http.Request) {
		writeRaw(t, w, `{"task_id":"t-1","status":"completed"}`)
	})
	mux.HandleFunc("GET /tasks/t-1/result", func(w http.ResponseWriter, r *http.Request) {
		writeRaw(t, w, `{"task_id":"t-1","status":"completed","data":[{"answer":"ok"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	c.SetPrompts(map[string]string{"*": "【最高优先级】禁止科普腔。"})
	rc, status, _, err := c.ChatStream(testKey(), []byte(`{"model":"agent-low","messages":[{"role":"user","content":"问题"}]}`))
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	_, _ = io.ReadAll(rc)
	rc.Close()
	if !strings.HasPrefix(gotTask, "【最高优先级】禁止科普腔。") {
		t.Fatalf("指令必须在任务最前面：%q", gotTask)
	}
	if !strings.Contains(gotTask, "问题") {
		t.Fatalf("用户问题不能丢：%q", gotTask)
	}
}

// ---- 家族 × 档位 ----

func TestFamilyEffortPicksEnum(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /operations/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeRaw(t, w, `{"task_id":"t-1","session_id":"s-1","status":"pending"}`)
	})
	mux.HandleFunc("GET /tasks/t-1/status", func(w http.ResponseWriter, r *http.Request) {
		writeRaw(t, w, `{"task_id":"t-1","status":"completed"}`)
	})
	mux.HandleFunc("GET /tasks/t-1/result", func(w http.ResponseWriter, r *http.Request) {
		writeRaw(t, w, `{"task_id":"t-1","status":"completed","data":[{"answer":"ok"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(srv.URL)

	call := func(body string) map[string]any {
		got = nil
		rc, status, _, err := c.ChatStream(testKey(), []byte(body))
		if err != nil || status != 200 {
			t.Fatalf("status=%d err=%v", status, err)
		}
		_, _ = io.ReadAll(rc)
		rc.Close()
		return got
	}
	if b := call(`{"model":"claude-fable-5","reasoning_effort":"high","messages":[{"role":"user","content":"x"}]}`); b["llm"] != "CLAUDE_FABLE_5_HIGH" {
		t.Fatalf("家族+档位没选中正确枚举：%v", b["llm"])
	}
	if b := call(`{"model":"claude-fable-5","messages":[{"role":"user","content":"x"}]}`); b["llm"] != "CLAUDE_FABLE_5_MEDIUM" {
		t.Fatalf("不带档位应落默认 medium：%v", b["llm"])
	}
	// 预设档：effort_level 与 llm 互斥，且 reasoning_effort 能覆盖档位
	b := call(`{"model":"agent-low","reasoning_effort":"high","messages":[{"role":"user","content":"x"}]}`)
	if _, has := b["llm"]; has {
		t.Fatalf("预设档不得带 llm：%v", b)
	}
	if b["effort_level"] != "high" {
		t.Fatalf("reasoning_effort 应覆盖预设档：%v", b["effort_level"])
	}
}

// ---- 收尾语义 ----

func TestStreamEmitsUsageBeforeDone(t *testing.T) {
	srv := fakeUpstream(t, "completed", `[{"answer":"巴黎。","research":{"answer":"据维基。"}}]`)
	c := newTestClient(srv.URL)
	rc, _, _, err := c.ChatStream(testKey(), []byte(`{"model":"agent-low","messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	out := string(raw)
	for _, want := range []string{`"role":"assistant"`, `"reasoning_content":"据维基。"`, `"content":"巴黎。"`, `"usage"`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("SSE 缺 %s：\n%s", want, out)
		}
	}
	if strings.Count(out, "[DONE]") != 1 {
		t.Fatalf("[DONE] 必须恰好一个：\n%s", out)
	}
	if strings.Index(out, `"usage"`) > strings.Index(out, "[DONE]") {
		t.Fatal("usage 帧必须在 [DONE] 之前")
	}
}

// TestFailureDoesNotFakeDone 任务失败时：读流报错，且**不补** [DONE]。
func TestFailureDoesNotFakeDone(t *testing.T) {
	srv := fakeUpstream(t, "failed", `null`)
	c := newTestClient(srv.URL)
	rc, status, _, err := c.ChatStream(testKey(), []byte(`{"model":"agent-low","messages":[{"role":"user","content":"x"}]}`))
	if err != nil || status != 200 {
		t.Fatalf("投任务阶段不该失败：%d %v", status, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err == nil {
		t.Fatal("任务失败必须让读流返回错误（否则会被补 [DONE] 伪装成成功）")
	}
	if strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("失败路径不得出现 [DONE]：\n%s", raw)
	}
}

// TestRelayNoDoneOnTruncation 统一出口：截断时发 error 帧、不补 [DONE]。
func TestRelayNoDoneOnTruncation(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, `data: {"choices":[{"delta":{"content":"半截"},"finish_reason":null}]}`+"\n\n")
		_ = pw.CloseWithError(io.ErrUnexpectedEOF)
	}()
	rec := httptest.NewRecorder()
	err := Relay(rec, pr, nil)
	if err == nil {
		t.Fatal("截断应返回错误")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("应发一帧 error：%s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("截断不得补 [DONE]：%s", body)
	}
}

// TestCollectAggregates 非流式：收敛出完整响应 + usage。
func TestCollectAggregates(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, `data: {"id":"chatcmpl-x","choices":[{"delta":{"reasoning_content":"想了"}}]}`+"\n\n")
		_, _ = io.WriteString(pw, `data: {"id":"chatcmpl-x","choices":[{"delta":{"content":"答"}}]}`+"\n\n")
		_, _ = io.WriteString(pw, `data: {"choices":[],"usage":{"total_tokens":7}}`+"\n\n")
		_, _ = io.WriteString(pw, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(pw, "data: [DONE]\n\n")
		_ = pw.Close()
	}()
	resp, err := Collect(pr, "agent-low")
	if err != nil {
		t.Fatal(err)
	}
	if resp["model"] != "agent-low" {
		t.Fatalf("model 应回填客户端请求名：%v", resp["model"])
	}
	ch := resp["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if msg["content"] != "答" || msg["reasoning_content"] != "想了" {
		t.Fatalf("内容聚合不对：%v", msg)
	}
	if ch["finish_reason"] != "stop" {
		t.Fatalf("finish_reason 不对：%v", ch["finish_reason"])
	}
	if resp["usage"] == nil {
		t.Fatal("usage 应保留")
	}
}

// TestCollectSurfacesErrorFrame error 帧必须变成 error，不能静默返回空。
func TestCollectSurfacesErrorFrame(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, `data: {"error":{"message":"上游炸了","code":"upstream_stream_error"}}`+"\n\n")
		_ = pw.Close()
	}()
	if _, err := Collect(pr, "m"); err == nil {
		t.Fatal("error 帧必须上抛")
	} else if !strings.Contains(err.Error(), "上游炸了") {
		t.Fatalf("错误信息应带上游原文：%v", err)
	}
}

// TestClassify 错误分类表。
func TestClassify(t *testing.T) {
	c := New()
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{401, `{"detail":"invalid api key"}`, ErrSessionDead},
		{402, `{"error":"Insufficient balance"}`, ErrHardCredit},
		{403, `{"error":"blocked"}`, ErrAccountFault},
		{404, `{}`, ErrNotFound},
		{429, `{}`, ErrSoftRate},
		{422, `{}`, ErrPassthrough},
		{500, `boom`, ErrServer},
	}
	for _, tc := range cases {
		if got := c.Classify(tc.status, tc.body); got != tc.want {
			t.Fatalf("Classify(%d) = %s, want %s", tc.status, got, tc.want)
		}
	}
}

// TestLookupSpecToleratesChannelPrefix 客户端带渠道前缀（从聚合器迁过来）也要能解析。
func TestLookupSpecToleratesChannelPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"futuresearch/agent-low":          "agent-low",
		"futuresearch/claude-fable-5":     "claude-fable-5",
		"futuresearch/claude-fable-5-max": "claude-fable-5-max",
	} {
		sp, ok := lookupSpec(in)
		if !ok || sp.ID != want {
			t.Fatalf("lookupSpec(%q) = (%q,%v)，want %q", in, sp.ID, ok, want)
		}
	}
}

// TestEffortsFor 档位表：家族给全、预设档只给 low/medium/high、forecast 不给。
func TestEffortsFor(t *testing.T) {
	fam := EffortsFor("claude-fable-5")
	if fam["max"] != "max" || fam["off"] != "nt" {
		t.Fatalf("家族档位表不对：%v", fam)
	}
	pre := EffortsFor("agent-medium")
	if len(pre) != 3 || pre["high"] != "high" {
		t.Fatalf("预设档应只给 low/medium/high：%v", pre)
	}
	if len(EffortsFor("forecast")) != 0 {
		t.Fatal("forecast 不该有档位")
	}
	if IsTieredSlug("claude-fable-5") {
		t.Fatal("家族名不是完整枚举")
	}
	if !IsTieredSlug("claude-fable-5-max") {
		t.Fatal("带档位后缀的是完整枚举，写客户端配置时要跳过")
	}
	if IsTieredSlug("gpt-5-mini") {
		t.Fatal("mini 是型号不是档位")
	}
}

// TestImagesRejectedExplicitly 带图片的请求必须**明确报错**，不能静默丢图。
func TestImagesRejectedExplicitly(t *testing.T) {
	srv := fakeUpstream(t, "completed", `[{"answer":"ok"}]`)
	c := newTestClient(srv.URL)
	body := `{"model":"agent-low","messages":[{"role":"user","content":[
		{"type":"text","text":"这是什么"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
	rc, status, raw, err := c.ChatStream(testKey(), []byte(body))
	if err != nil {
		t.Fatalf("不该是传输层错误：%v", err)
	}
	if rc != nil {
		rc.Close()
		t.Fatal("带图请求不该返回流")
	}
	if status != 400 {
		t.Fatalf("应 400，实际 %d", status)
	}
	if !strings.Contains(string(raw), "images_not_supported") {
		t.Fatalf("错误码应说明是图片不支持：%s", raw)
	}
}
