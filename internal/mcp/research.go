package mcp

import (
	"fmt"
	"strings"
)

// ResearchToolSet FutureSearch 的云端研究工具（research / forecast / models）。
//
// 注意：这一组**碰不到本地文件** —— 上游 agent 不支持工具调用，
// 这些工具只是把「研究」这件事包成一个可被 MCP 客户端调用的动作。
type ResearchToolSet struct{ gw Completer }

// NewResearch 建研究工具集。
func NewResearch(gw Completer) *ResearchToolSet { return &ResearchToolSet{gw: gw} }

// ServerName MCP server 名。
func (r *ResearchToolSet) ServerName() string { return "futuresearch" }

// Tools 工具定义。
func (r *ResearchToolSet) Tools() []map[string]any {
	return []map[string]any{
		{
			"name": "research",
			"description": "用 FutureSearch 的研究 agent 调研一个问题，返回带出处的结论。" +
				"耗时较长（low 档 20~60 秒，high 档几分钟），适合需要事实核查、多来源交叉的问题。" +
				"不适合：闲聊、写代码、需要读取本地文件的活（那些用本地工具）。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string", "description": "要研究的问题，越具体越好"},
					"effort": map[string]any{
						"type": "string", "enum": []string{"low", "medium", "high"},
						"description": "研究深度：low 快、medium 均衡（带出处）、high 深入。默认 medium",
					},
					"model": map[string]any{
						"type": "string",
						"description": "可选：指定底层模型家族（如 claude-5.5-opus / gpt-5.6-terra / gemini-3.8-flash）。" +
							"给了 model 时 effort 被忽略",
					},
				},
				"required": []string{"question"},
			},
		},
		{
			"name":        "forecast",
			"description": "对一个是非问题给出 0–100 的概率预测 + 理由 + 出处。问题必须可判定真假。耗时最长（实测 ≈3 分钟）。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string", "description": "可判定真假的问题，如「X 会在 2027 年前发布吗」"},
				},
				"required": []string{"question"},
			},
		},
		{
			"name":        "models",
			"description": "列出这个网关当前可用的 FutureSearch 模型（预设档 + 模型家族 + 完整枚举名）。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// Call 执行工具。
func (r *ResearchToolSet) Call(name string, args map[string]any) (string, error) {
	if name == "models" {
		return strings.Join(r.gw.Models(), "\n"), nil
	}
	question := strings.TrimSpace(str(args["question"]))
	if question == "" {
		return "", fmt.Errorf("question 不能为空")
	}
	if rn := []rune(question); len(rn) > 8000 {
		question = string(rn[:8000])
	}
	model := strings.TrimSpace(str(args["model"]))
	switch {
	case name == "forecast":
		model = "forecast"
	case model != "":
	case true:
		effort := strings.TrimSpace(str(args["effort"]))
		if effort != "low" && effort != "medium" && effort != "high" {
			effort = "medium"
		}
		model = "agent-" + effort
	}
	answer, reasoning, err := r.gw.Complete(model, question)
	if err != nil {
		return "", err
	}
	reasoning = stripProgress(reasoning)
	if reasoning != "" {
		return answer + "\n\n---\n研究过程：\n" + reasoning, nil
	}
	return answer, nil
}
