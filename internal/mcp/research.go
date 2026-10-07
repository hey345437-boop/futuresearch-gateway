package mcp

import (
	"fmt"
	"strings"
)

// Gateway 网关提供给 MCP 的能力。
//
// 比最初的 Completer 多了异步任务那套（submit/status/result/cancel/cost）——
// 因为上游任务可能跑十几分钟，一次工具调用挂那么久客户端会超时。
type Gateway interface {
	Complete(model, prompt string) (answer, reasoning string, err error)
	Models() []string
	SubmitAsync(model, prompt, effort string) (string, error)
	AsyncStatus(taskID string) (string, error)
	AsyncResult(taskID string) (string, error)
	AsyncCancel(taskID string) (string, error)
	AsyncCost(taskID string) (string, error)
	PoolBalance() string
	Decide(question string, options []string, effort, model string) (answer, reasoning string, err error)
}

// ResearchToolSet FutureSearch 的云端工具。
//
// 注意：这一组**碰不到本地文件** —— 上游 agent 不支持工具调用，
// 这些工具只是把「研究 / 预测 / 决策」包成可被 MCP 客户端调用的动作。
type ResearchToolSet struct{ gw Gateway }

// NewResearch 建研究工具集。
func NewResearch(gw Gateway) *ResearchToolSet { return &ResearchToolSet{gw: gw} }

// ServerName MCP server 名。
func (r *ResearchToolSet) ServerName() string { return "futuresearch" }

func strList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			// 容忍客户端把数组写成换行/逗号分隔的字符串
			f := strings.FieldsFunc(s, func(c rune) bool { return c == '\n' || c == ',' || c == ';' })
			out := make([]string, 0, len(f))
			for _, x := range f {
				if t := strings.TrimSpace(x); t != "" {
					out = append(out, t)
				}
			}
			return out
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func effortOf(v any) string {
	e := strings.ToLower(strings.TrimSpace(str(v)))
	switch e {
	case "low", "medium", "high":
		return e
	}
	return ""
}

// Tools 工具定义。
func (r *ResearchToolSet) Tools() []map[string]any {
	return []map[string]any{
		{
			"name": "research",
			"description": "用 FutureSearch 的研究 agent 调研一个问题，**阻塞**等结果（low 档 20~60 秒，high 档几分钟）。" +
				"适合需要事实核查、多来源交叉的问题。长任务（可能超过 2 分钟）建议改用 submit_research + task_status。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string", "description": "要研究的问题，越具体越好"},
					"effort": map[string]any{
						"type": "string", "enum": []string{"low", "medium", "high"},
						"description": "研究深度：low 快、medium 均衡（带出处）、high 深入。默认 medium",
					},
					"model": map[string]any{
						"type":        "string",
						"description": "可选：指定底层模型家族（如 claude-5.5-opus / gpt-5.6-terra / gemini-3.8-flash）。给了 model 时 effort 被忽略",
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
			"name":        "decision",
			"description": "决策分析：给一个正在权衡的决策（+ 可选选项列表），产出「每个选项的结果预测 + 关键量化估计 + 风险 + 对比表 + 推荐」。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string", "description": "你在权衡什么（含背景）"},
					"options": map[string]any{
						"type": "array", "items": map[string]any{"type": "string"},
						"description": "可选：你实际在选的几个选项（不填则让它自己列）",
					},
					"effort": map[string]any{
						"type": "string", "enum": []string{"low", "medium", "high"},
						"description": "分析深度，默认 medium",
					},
					"model": map[string]any{"type": "string", "description": "可选：指定底层模型家族"},
				},
				"required": []string{"question"},
			},
		},
		{
			"name": "submit_research",
			"description": "**异步**投一个研究任务，立刻返回 task_id（不等待）。" +
				"适合可能跑很久的任务：投完先干别的，再用 task_status / task_result 取。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string", "description": "要研究的问题"},
					"effort":   map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}, "description": "研究深度，默认 medium"},
					"model":    map[string]any{"type": "string", "description": "可选：指定底层模型家族"},
				},
				"required": []string{"question"},
			},
		},
		{
			"name":        "task_status",
			"description": "查一个异步任务的当前状态（状态 / 进度 / 当前步骤 / 已等待多久）。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"task_id": map[string]any{"type": "string"}},
				"required":   []string{"task_id"},
			},
		},
		{
			"name":        "task_result",
			"description": "取一个已完成任务的完整结果（含研究过程）。没跑完会报错，先用 task_status 确认。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"task_id": map[string]any{"type": "string"}},
				"required":   []string{"task_id"},
			},
		},
		{
			"name":        "task_cancel",
			"description": "取消一个还在跑的异步任务（省额度，尤其是投错了的）。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"task_id": map[string]any{"type": "string"}},
				"required":   []string{"task_id"},
			},
		},
		{
			"name":        "task_cost",
			"description": "查一个任务的费用（美元）。上游计费有延迟，跑完立刻查可能还没数。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"task_id": map[string]any{"type": "string"}},
				"required":   []string{"task_id"},
			},
		},
		{
			"name":        "balance",
			"description": "查这个网关号池的余额（合计 + 可用/冷却/停用账号数 + 余额最高的几个）。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "models",
			"description": "列出这个网关当前可用的模型（预设档 + 模型家族 + 完整枚举名）。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// Call 执行工具。
func (r *ResearchToolSet) Call(name string, args map[string]any) (string, error) {
	switch name {
	case "models":
		return strings.Join(r.gw.Models(), "\n"), nil
	case "balance":
		return r.gw.PoolBalance(), nil
	case "task_status":
		return r.gw.AsyncStatus(str(args["task_id"]))
	case "task_result":
		return r.gw.AsyncResult(str(args["task_id"]))
	case "task_cancel":
		return r.gw.AsyncCancel(str(args["task_id"]))
	case "task_cost":
		return r.gw.AsyncCost(str(args["task_id"]))
	case "submit_research":
		q := strings.TrimSpace(str(args["question"]))
		if q == "" {
			return "", fmt.Errorf("question 不能为空")
		}
		id, err := r.gw.SubmitAsync(modelOf(args, "agent-medium"), truncateRunes(q, 8000), effortOf(args["effort"]))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已投递。task_id: %s\n用 task_status 查进度，task_result 取结果。", id), nil
	case "decision":
		answer, reasoning, err := r.gw.Decide(str(args["question"]), strList(args["options"]), effortOf(args["effort"]), str(args["model"]))
		if err != nil {
			return "", err
		}
		return withReasoning(answer, reasoning), nil
	case "forecast":
		q := strings.TrimSpace(str(args["question"]))
		if q == "" {
			return "", fmt.Errorf("question 不能为空")
		}
		answer, reasoning, err := r.gw.Complete("forecast", truncateRunes(q, 8000))
		if err != nil {
			return "", err
		}
		return withReasoning(answer, reasoning), nil
	case "research":
		q := strings.TrimSpace(str(args["question"]))
		if q == "" {
			return "", fmt.Errorf("question 不能为空")
		}
		answer, reasoning, err := r.gw.Complete(modelOf(args, "agent-medium"), truncateRunes(q, 8000))
		if err != nil {
			return "", err
		}
		return withReasoning(answer, reasoning), nil
	default:
		return "", fmt.Errorf("未知工具：%s", name)
	}
}

// modelOf 取模型：显式 model 优先，否则按 effort 选预设档。
func modelOf(args map[string]any, fallback string) string {
	if m := strings.TrimSpace(str(args["model"])); m != "" {
		return m
	}
	if e := effortOf(args["effort"]); e != "" {
		return "agent-" + e
	}
	return fallback
}

func withReasoning(answer, reasoning string) string {
	if r := stripProgress(reasoning); r != "" {
		return answer + "\n\n---\n研究过程：\n" + r
	}
	return answer
}

func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
