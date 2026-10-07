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
	TaskProgress(taskID string) (string, error)

	// 批量操作 / 数据与查询
	MultiAgent(task string, directions []string, effort string) (answer, reasoning string, err error)
	Classify(task string, categories, items []string) (string, error)
	Rank(task string, items []string, ascending bool) (string, error)
	Dedupe(equivalence string, items []string, strategy string) (answer, reasoning string, err error)
	Merge(task string, left, right []string, leftKey, rightKey string) (answer, reasoning string, err error)
	UploadData(rows []map[string]any) (string, error)
	BuiltInLists() (string, error)
	UseBuiltInList(nameOrID string) (string, error)
	Sessions(limit int) (string, error)
	SessionTasks(sessionID string) (string, error)
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
			"name": "multi_agent",
			"description": "**多 agent 并行**研究：几个方向同时开工再综合。比单 agent 的 research 更全但更慢更贵。" +
				"可以显式给研究方向（最多 6 个）。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task":       map[string]any{"type": "string", "description": "要研究什么"},
					"directions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "可选：最多 6 个研究方向"},
					"effort":     map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}, "description": "低=3 个方向、高=更深。默认 medium"},
				},
				"required": []string{"task"},
			},
		},
		{
			"name":        "classify",
			"description": "把一批条目分到给定的类别里（每条给出类别 + 理由）。适合打标签、分流、归档。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task":       map[string]any{"type": "string", "description": "分类标准（什么算哪一类）"},
					"categories": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "允许的类别清单"},
					"items":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "要分类的条目"},
				},
				"required": []string{"task", "categories", "items"},
			},
		},
		{
			"name":        "rank",
			"description": "按一个任务给一批条目**打分并排序**（0-100 之类的分 + 理由）。适合选优、排优先级。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task":      map[string]any{"type": "string", "description": "按什么打分（打分标准）"},
					"items":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "要排序的条目"},
					"ascending": map[string]any{"type": "boolean", "description": "true=升序，默认降序（分高的在前）"},
				},
				"required": []string{"task", "items"},
			},
		},
		{
			"name":        "dedupe",
			"description": "语义去重：说明「什么算重复」，它标出/合并重复项。比字符串精确匹配强得多。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"equivalence": map[string]any{"type": "string", "description": "什么算两条重复（如「同一个人的不同写法」）"},
					"items":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "要去重的条目（至少两条）"},
					"strategy":    map[string]any{"type": "string", "enum": []string{"identify", "select", "merge"}, "description": "identify=只标出；select=选一条；merge=合成一条。默认 identify"},
				},
				"required": []string{"equivalence", "items"},
			},
		},
		{
			"name":        "merge",
			"description": "语义表连接：按一个任务把两张表合起来（不是 SQL join，是「按语义对应」）。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task":      map[string]any{"type": "string", "description": "按什么把两张表合起来"},
					"left":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "左表（每行一个字符串）"},
					"right":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "右表"},
					"left_key":  map[string]any{"type": "string", "description": "可选：左表按哪列对"},
					"right_key": map[string]any{"type": "string", "description": "可选：右表按哪列对"},
				},
				"required": []string{"task", "left", "right"},
			},
		},
		{
			"name":        "upload_data",
			"description": "上传一批行（表格）成一个 artifact，返回 artifact_id —— 之后可以把它当别的操作的 input 用。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"rows": map[string]any{
						"type": "array", "items": map[string]any{"type": "object"},
						"description": "行对象列表，如 [{\"name\":\"A\",\"score\":1}]",
					},
				},
				"required": []string{"rows"},
			},
		},
		{
			"name":        "browse_lists",
			"description": "看上游内置的数据列表（人名/机构/公司之类的现成表，含行数与字段）。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "use_list",
			"description": "把一个内置列表复制成你自己的 artifact（返回 artifact_id），之后可以拿它做输入。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string", "description": "列表名或 artifact_id（用 browse_lists 看有哪些）"}},
				"required":   []string{"name"},
			},
		},
		{
			"name":        "list_sessions",
			"description": "列最近的会话（每次研究都会落在一个会话里）。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"limit": map[string]any{"type": "integer", "description": "取几个，默认 10，最多 50"}},
			},
		},
		{
			"name":        "list_session_tasks",
			"description": "列某个会话里的所有任务（含状态和类型）。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"session_id": map[string]any{"type": "string"}},
				"required":   []string{"session_id"},
			},
		},
		{
			"name":        "task_progress",
			"description": "看任务的**过程摘要**（上游生成的人类可读进度，比 task_status 更细）。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"task_id": map[string]any{"type": "string"}},
				"required":   []string{"task_id"},
			},
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
	case "multi_agent":
		t := strings.TrimSpace(str(args["task"]))
		if t == "" {
			return "", fmt.Errorf("task 不能为空")
		}
		a, r2, err := r.gw.MultiAgent(truncateRunes(t, 8000), strList(args["directions"]), effortOf(args["effort"]))
		if err != nil {
			return "", err
		}
		return withReasoning(a, r2), nil
	case "classify":
		// 参数在 MCP 边界就校验 —— 早点报清楚，别白跑一趟上游
		if err := needArgs(args, "task", "categories", "items"); err != nil {
			return "", err
		}
		return r.gw.Classify(str(args["task"]), strList(args["categories"]), strList(args["items"]))
	case "rank":
		if err := needArgs(args, "task", "items"); err != nil {
			return "", err
		}
		asc, _ := args["ascending"].(bool)
		return r.gw.Rank(str(args["task"]), strList(args["items"]), asc)
	case "dedupe":
		if err := needArgs(args, "equivalence", "items"); err != nil {
			return "", err
		}
		if len(strList(args["items"])) < 2 {
			return "", fmt.Errorf("items 至少要两条才谈得上去重")
		}
		a, r2, err := r.gw.Dedupe(str(args["equivalence"]), strList(args["items"]), str(args["strategy"]))
		if err != nil {
			return "", err
		}
		return withReasoning(a, r2), nil
	case "merge":
		if err := needArgs(args, "task", "left", "right"); err != nil {
			return "", err
		}
		a, r2, err := r.gw.Merge(str(args["task"]), strList(args["left"]), strList(args["right"]),
			str(args["left_key"]), str(args["right_key"]))
		if err != nil {
			return "", err
		}
		return withReasoning(a, r2), nil
	case "upload_data":
		rows := rowsOf(args["rows"])
		if len(rows) == 0 {
			return "", fmt.Errorf("rows 不能为空")
		}
		return r.gw.UploadData(rows)
	case "browse_lists":
		return r.gw.BuiltInLists()
	case "use_list":
		if strings.TrimSpace(str(args["name"])) == "" {
			return "", fmt.Errorf("name 不能为空")
		}
		return r.gw.UseBuiltInList(str(args["name"]))
	case "list_sessions":
		return r.gw.Sessions(int(num(args["limit"])))
	case "list_session_tasks":
		if strings.TrimSpace(str(args["session_id"])) == "" {
			return "", fmt.Errorf("session_id 不能为空")
		}
		return r.gw.SessionTasks(str(args["session_id"]))
	case "task_progress":
		return r.gw.TaskProgress(str(args["task_id"]))
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

// rowsOf 把客户端给的 rows 转成 []map[string]any（容忍单对象和字符串行）。
func rowsOf(v any) []map[string]any {
	switch t := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, x := range t {
			switch row := x.(type) {
			case map[string]any:
				out = append(out, row)
			case string:
				out = append(out, map[string]any{"text": row})
			}
		}
		return out
	case map[string]any:
		return []map[string]any{t}
	case []map[string]any:
		return t
	}
	return nil
}

// needArgs 校验必填参数在 MCP 边界就齐全（值不能是空串/空数组）。
//
// 为什么在边界校验而不是等上游：批量操作一次可能跑几十秒，
// 参数缺了白等一趟；而且上游的 422 报文对模型不友好。
func needArgs(args map[string]any, keys ...string) error {
	var missing []string
	for _, k := range keys {
		v, ok := args[k]
		if !ok || v == nil {
			missing = append(missing, k)
			continue
		}
		switch t := v.(type) {
		case string:
			if strings.TrimSpace(t) == "" {
				missing = append(missing, k)
			}
		case []any:
			if len(t) == 0 {
				missing = append(missing, k)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺必填参数：%s", strings.Join(missing, ", "))
	}
	return nil
}
