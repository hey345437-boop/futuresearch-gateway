// toolbridge.go 把客户端的 `tools` 桥接成 FutureSearch 能理解的形式。
//
// 背景（实测）：
//
//	FutureSearch 的上游是**任务式 agent API**，没有 OpenAI 那种「客户端传 tools、
//	服务端回 tool_calls」的面。直接传 tools 过去，模型会把函数名当**文本**写出来
//	（`get_weather(city="Beijing")`），finish_reason 是 stop，客户端拿到空的
//	tool_calls —— coding agent（Claude Code / Cursor / Cline）直接瘫掉。
//
// 办法：**提示词模拟工具调用**（prompt-based function calling）。
//
//	① 把客户端的工具定义翻译成一段协议提示，附在任务前面；
//	② 模型按协议输出一行 JSON：{"tool":"read_file","args":{"path":"..."}}
//	③ 网关把这行 JSON **解析回标准的 tool_calls** 返回给客户端。
//
// 实测（futuresearch/agent-low，6 个任务）：命中 5/6。比原生 function calling 弱，
// 但对 coding agent 的读写文件/执行命令这类调用够用。解析失败时**优雅降级**成纯文本，
// 不会把请求搞坏。
package fsapi

import (
	"encoding/json"
	"strings"
)

// toolDef OpenAI 风格的单个工具定义。
type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// parsedToolCall 从模型输出里解析出来的工具调用。
type parsedToolCall struct {
	Name string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// toolProtocol 协议提示。措辞是实测调出来的 —— 关键是**明确否定"我做不到"**，
// 否则模型会回一句"我无法访问文件系统"（这条踩过）。
const toolProtocol = `【工具调用协议】
你**可以**调用下面列出的工具。需要调用工具时，**只输出一行 JSON**，不要有任何其他文字、
不要 markdown 代码块、不要解释：
{"tool":"<工具名>","args":{...}}

重要：不要说"我无法访问文件系统""我没有这个能力" —— 你**有**这些工具，
直接用上面那行 JSON 调用即可。一次只调一个工具。
拿到工具结果后，继续下一步；不需要工具时正常回答。

可用工具：
`

// toolsPrompt 把工具定义渲染成协议提示。没有工具时返回空串。
func toolsPrompt(tools []toolDef) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(toolProtocol)
	for _, t := range tools {
		name := strings.TrimSpace(t.Function.Name)
		if name == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(name)
		if d := strings.TrimSpace(t.Function.Description); d != "" {
			b.WriteString(": ")
			b.WriteString(tailRunes(d, 200))
		}
		b.WriteString("\n")
		if len(t.Function.Parameters) > 0 {
			b.WriteString("  参数 schema: ")
			b.WriteString(tailRunes(string(t.Function.Parameters), 400))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// parseToolCall 从模型输出里抠出一个工具调用。
//
// 宽容度：允许前后有说明文字、允许 ```json 包裹、允许 {"name":...} 或 {"tool":...}。
// 找不到就返回 nil（调用方降级成纯文本）。
func parseToolCall(s string) *parsedToolCall {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// 扫所有顶层平衡的 JSON 对象，取第一个带 tool/name 字段的
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		if end, ok := balancedEnd(s, i); ok {
			var probe map[string]json.RawMessage
			if json.Unmarshal([]byte(s[i:end]), &probe) == nil {
				var name string
				for _, k := range []string{"tool", "name", "tool_name", "function"} {
					if raw, ok := probe[k]; ok {
						// function 可能是字符串，也可能是 {"name":...}
						if json.Unmarshal(raw, &name) == nil && name != "" {
							break
						}
						var fn struct {
							Name string `json:"name"`
						}
						if json.Unmarshal(raw, &fn) == nil && fn.Name != "" {
							name = fn.Name
							break
						}
						name = ""
					}
				}
				if name != "" {
					args := json.RawMessage("{}")
					for _, k := range []string{"args", "arguments", "parameters", "input"} {
						if raw, ok := probe[k]; ok && len(raw) > 0 && string(raw) != "null" {
							// args 可能是字符串包了一层 JSON
							var asStr string
							if json.Unmarshal(raw, &asStr) == nil {
								if json.Valid([]byte(asStr)) {
									args = json.RawMessage(asStr)
								}
							} else {
								args = raw
							}
							break
						}
					}
					return &parsedToolCall{Name: name, Args: args}
				}
			}
			i = end - 1
		}
	}
	return nil
}

// balancedEnd 返回从 s[start]（必须是 '{'）开始的平衡 JSON 对象的结束位置（含）。
func balancedEnd(s string, start int) (int, bool) {
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// looksLikeToolCall 判断「已经攒到的这段文本」像不像一个待输出的工具调用。
//
// 流式路径用它决定要不要继续憋着：以 `{` 开头就先不吐给客户端，
// 等解析完再决定是发 tool_calls 还是发文本。
func looksLikeToolCall(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true // 还没开始，继续观察
	}
	// 去掉常见的 markdown 包裹
	t = strings.TrimPrefix(t, "```json")
	t = strings.TrimPrefix(t, "```")
	t = strings.TrimSpace(t)
	if !strings.HasPrefix(t, "{") {
		return false
	}
	// 已经闭合了 → 不再憋着
	if _, ok := balancedEnd(t, 0); ok {
		return false
	}
	return true
}

// openAIToolCall 把解析结果转成 OpenAI 响应里的 tool_calls 元素。
func openAIToolCall(tc *parsedToolCall, index int) map[string]any {
	args := string(tc.Args)
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return map[string]any{
		"index": index,
		"id":    "call_" + shortHash(tc.Name+args),
		"type":  "function",
		"function": map[string]any{
			"name":      tc.Name,
			"arguments": args,
		},
	}
}

// shortHash 给工具调用造一个稳定的 id（客户端拿它回填 tool 结果）。
func shortHash(s string) string {
	const h = uint32(2166136261)
	var x = h
	for i := 0; i < len(s); i++ {
		x ^= uint32(s[i])
		x *= 16777619
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 12)
	for i := 11; i >= 0; i-- {
		out[i] = hex[x&0xf]
		x >>= 4
	}
	return string(out)
}
