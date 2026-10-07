package fsapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseToolCallForms 模型输出的各种形态都要能抠出来。
//
// 这条最关键：模型经常不老实 —— 前后加解释、包 markdown、字段名换一个。
// 解析失败就等于 coding agent 拿不到 tool_calls，直接瘫。
func TestParseToolCallForms(t *testing.T) {
	cases := []struct{ name, in, wantTool, wantArgs string }{
		{"裸 JSON", `{"tool":"read_file","args":{"path":"/tmp/a.txt"}}`, "read_file", `{"path":"/tmp/a.txt"}`},
		{"前后有解释", "好的，我来读一下。\n{\"tool\":\"read_file\",\"args\":{\"path\":\"/tmp/a.txt\"}}\n", "read_file", `{"path":"/tmp/a.txt"}`},
		{"markdown 包裹", "```json\n{\"tool\":\"run_command\",\"args\":{\"cmd\":\"ls\"}}\n```", "run_command", `{"cmd":"ls"}`},
		{"用 name 字段", `{"name":"write_file","args":{"path":"x","content":"hi"}}`, "write_file", `{"path":"x","content":"hi"}`},
		{"用 arguments 字段", `{"tool":"read_file","arguments":{"path":"/etc/hosts"}}`, "read_file", `{"path":"/etc/hosts"}`},
		{"args 是字符串包 JSON", `{"tool":"read_file","args":"{\"path\":\"/tmp/a.txt\"}"}`, "read_file", `{"path":"/tmp/a.txt"}`},
		{"嵌套对象（平衡括号）", `{"tool":"write_file","args":{"path":"a","content":"{\"k\":1}"}}`, "write_file", `{"path":"a","content":"{\"k\":1}"}`},
		{"没有 args", `{"tool":"list_dir"}`, "list_dir", `{}`},
	}
	for _, c := range cases {
		got := parseToolCall(c.in)
		if got == nil {
			t.Errorf("%s：没解析出来（输入 %q）", c.name, c.in)
			continue
		}
		if got.Name != c.wantTool {
			t.Errorf("%s：工具名 %q，期望 %q", c.name, got.Name, c.wantTool)
		}
		var a, b map[string]any
		if json.Unmarshal(got.Args, &a) != nil || json.Unmarshal([]byte(c.wantArgs), &b) != nil {
			t.Errorf("%s：参数不是合法 JSON：%s", c.name, got.Args)
			continue
		}
		if len(a) != len(b) {
			t.Errorf("%s：参数 %s，期望 %s", c.name, got.Args, c.wantArgs)
		}
	}
}

// TestParseToolCallRejectsPlainText 普通回答**不能**被误判成工具调用 ——
// 否则正常聊天会被吞掉、客户端收到一个假的 tool_call。
func TestParseToolCallRejectsPlainText(t *testing.T) {
	for _, s := range []string{
		"", "法国首都是巴黎。", "我无法访问文件系统。",
		`{"answer":"巴黎"}`,         // 有 JSON 但没有 tool/name
		`{"result":{"tool":"x"}}`, // 嵌套的不是顶层
		"这是一段普通文本，里面有 { 花括号",
	} {
		if got := parseToolCall(s); got != nil {
			t.Errorf("不该解析出工具调用，却得到 %+v（输入 %q）", got, s)
		}
	}
}

// TestBalancedEnd 括号平衡要正确处理字符串里的花括号和转义引号。
func TestBalancedEnd(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{`{"a":1}`, 7, true},
		{`{"a":"}"}`, 9, true},
		{`{"a":"\"}"}`, 11, true},
		{`{"a":{"b":1}}`, 13, true},
		{`{"a":1`, 0, false},
	}
	for _, c := range cases {
		got, ok := balancedEnd(c.in, 0)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("balancedEnd(%q) = (%d,%v)，期望 (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestToolsPromptRendersAll 工具提示要把每个工具和它的参数 schema 都写上 ——
// 漏了 schema 模型就不知道参数怎么填。
func TestToolsPromptRendersAll(t *testing.T) {
	var t1, t2 toolDef
	t1.Type = "function"
	t1.Function.Name = "read_file"
	t1.Function.Description = "读一个文件"
	t1.Function.Parameters = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	t2.Type = "function"
	t2.Function.Name = "run_command"
	t2.Function.Description = "执行命令"
	t2.Function.Parameters = json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}}}`)

	p := toolsPrompt([]toolDef{t1, t2})
	for _, want := range []string{"read_file", "run_command", "path", "cmd", `"tool"`, "无法访问文件系统"} {
		if !strings.Contains(p, want) {
			t.Errorf("提示里缺 %q", want)
		}
	}
	// 没有工具 → 空串（不能白加一段协议提示污染普通对话）
	if toolsPrompt(nil) != "" {
		t.Error("没有工具时应返回空串")
	}
}

// TestBuildTaskCarriesToolResults 工具结果要作为「工具结果」进上下文，
// 否则模型看不到自己上一步拿到了什么，会反复调同一个工具。
func TestBuildTaskCarriesToolResults(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: json.RawMessage(`"读一下 a.txt"`)},
		{Role: "assistant", ToolCalls: []toolCallIn{func() (tc toolCallIn) {
			tc.Function.Name = "read_file"
			tc.Function.Arguments = `{"path":"a.txt"}`
			return
		}()}},
		{Role: "tool", Name: "read_file", Content: json.RawMessage(`"文件内容：hello"`)},
	}
	task := buildTask(msgs)
	for _, want := range []string{"read_file", "hello", "工具返回值"} {
		if !strings.Contains(task, want) {
			t.Errorf("任务文本里缺 %q：\n%s", want, task)
		}
	}
}

// TestOpenAIToolCallShape 返回给客户端的 tool_calls 必须是标准形状 ——
// Claude Code / Cursor 按这个结构取 id、name、arguments。
func TestOpenAIToolCallShape(t *testing.T) {
	tc := &parsedToolCall{Name: "read_file", Args: json.RawMessage(`{"path":"/tmp/a.txt"}`)}
	got := openAIToolCall(tc, 0)
	if got["type"] != "function" {
		t.Errorf("type 应为 function，实际 %v", got["type"])
	}
	if id, _ := got["id"].(string); !strings.HasPrefix(id, "call_") || len(id) < 10 {
		t.Errorf("id 形状不对：%v", got["id"])
	}
	fn, _ := got["function"].(map[string]any)
	if fn["name"] != "read_file" {
		t.Errorf("name 不对：%v", fn["name"])
	}
	if fn["arguments"] != `{"path":"/tmp/a.txt"}` {
		t.Errorf("arguments 不对：%v", fn["arguments"])
	}
	// 同一个调用要生成同一个 id（客户端靠它回填结果）
	if openAIToolCall(tc, 0)["id"] != got["id"] {
		t.Error("同一个调用两次生成的 id 应一致")
	}
}
