// async.go 异步任务路径：投任务拿 ID、之后再来查状态 / 取结果 / 取消 / 查费用。
//
// 为什么需要它：ChatStream 是**同步**的（调用方一直等到出结果）。但上游任务可能跑
// 十几分钟甚至失败，MCP 那边一次工具调用挂十分钟是没法用的。所以额外给一条
// 「先投、后取」的路：submit → status → result/cancel。
package fsapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Prepared 一次任务的解析结果（模型规格 + 注入过指令的任务文本）。
type Prepared struct {
	Spec modelSpec
	Task string
}

// Prepare 解析模型名、套上前置指令，但**不投任务**。
//
// 与 ChatStream 的区别：那个吃的是 OpenAI chat 请求体，这个吃纯文本 ——
// 给 MCP / 管理接口用。
func (c *Client) Prepare(model, prompt string, effort string) (Prepared, error) {
	spec, ok := lookupSpec(model)
	if !ok {
		return Prepared{}, fmt.Errorf("未知模型：%s（可用：%s）", model, modelIDList())
	}
	if spec.Base != "" {
		if b, ok := lookupBase(spec.Base); ok {
			slug := b.pickTier(effort)
			if lm, ok := llmBySlug[slug]; ok {
				spec.LLM, spec.Name = lm.Enum, lm.Label
			}
		}
	} else if spec.LLM == "" && spec.Effort != "" {
		switch e := strings.ToLower(strings.TrimSpace(effort)); e {
		case "low", "medium", "high":
			spec.Effort = e
		}
	}
	task := strings.TrimSpace(prompt)
	if pre := c.promptFor(spec.ID); pre != "" {
		task = pre + "\n\n" + task
	}
	return Prepared{Spec: spec, Task: task}, nil
}

// Submit 投任务，立刻返回 task_id（不等待）。
func (c *Client) Submit(key string, p Prepared) (string, int, []byte, error) {
	ref, status, raw, err := c.createTask(key, p.Spec, p.Task)
	if err != nil || status >= 400 {
		return "", status, raw, err
	}
	return ref.TaskID, status, raw, nil
}

// TaskSnapshot 一次任务状态快照。
type TaskSnapshot struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	Label   string `json:"label"`
	Error   string `json:"error,omitempty"`
	Pending int    `json:"pending"`
	Running int    `json:"running"`
	Done    int    `json:"completed"`
	Failed  int    `json:"failed"`
	Total   int    `json:"total"`
}

// Snapshot 查任务状态。
func (c *Client) Snapshot(key, taskID string) (*TaskSnapshot, int, []byte, error) {
	ref, status, raw, err := c.taskStatus(key, taskID)
	if err != nil || status >= 400 {
		return nil, status, raw, err
	}
	s := &TaskSnapshot{TaskID: ref.TaskID, Status: ref.Status, Label: ref.Label}
	if ref.Error != nil {
		s.Error = *ref.Error
	}
	if ref.Progress != nil {
		s.Pending, s.Running = ref.Progress.Pending, ref.Progress.Running
		s.Done, s.Failed, s.Total = ref.Progress.Completed, ref.Progress.Failed, ref.Progress.Total
	}
	return s, status, raw, nil
}

// Result 取任务结果（已格式化成文本）。
func (c *Client) Result(key, taskID string) (answer, reasoning string, status int, raw []byte, err error) {
	res, st, body, err := c.taskResultOf(key, taskID)
	if err != nil || st >= 400 {
		return "", "", st, body, err
	}
	a, r := formatResult(res.Data)
	if strings.TrimSpace(a) == "" && strings.TrimSpace(r) == "" {
		if res.Error != nil && strings.TrimSpace(*res.Error) != "" {
			return "", "", st, body, fmt.Errorf("上游任务失败：%s", *res.Error)
		}
		return "", "", st, body, fmt.Errorf("上游没有返回内容（status=%s）", res.Status)
	}
	return a, r, st, body, nil
}

// Cancel 取消任务。
func (c *Client) Cancel(key, taskID string) (int, []byte, error) {
	return c.call(key, http.MethodPost, "/tasks/"+taskID+"/cancel", nil)
}

// Cost 查任务费用（美元 + 上游原文说明）。
func (c *Client) Cost(key, taskID string) (float64, string, int, []byte, error) {
	status, raw, err := c.call(key, http.MethodGet, "/tasks/"+taskID+"/cost", nil)
	if err != nil || status >= 400 {
		return 0, "", status, raw, err
	}
	// 上游这个端点形状不稳定（有时 {"cost_dollars":N}，有时 {"cost":N}，有时 pending）——
	// 用宽松解析，拿不到就原样把 JSON 交回去，别猜。
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return 0, string(raw), status, raw, nil
	}
	for _, k := range []string{"cost_dollars", "cost", "total_cost_dollars", "amount_dollars"} {
		if v, ok := m[k].(float64); ok {
			return v, "", status, raw, nil
		}
	}
	return 0, string(raw), status, raw, nil
}

// Sessions 最近会话（给「查历史」用）。
func (c *Client) Sessions(key string, limit int) ([]map[string]any, int, []byte, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	status, raw, err := c.call(key, http.MethodGet, fmt.Sprintf("/sessions?limit=%d", limit), nil)
	if err != nil || status >= 400 {
		return nil, status, raw, err
	}
	var list []map[string]any
	if json.Unmarshal(raw, &list) != nil {
		var wrap struct {
			Data []map[string]any `json:"data"`
		}
		if json.Unmarshal(raw, &wrap) == nil {
			list = wrap.Data
		}
	}
	return list, status, raw, nil
}

// runOpOnce 投一个操作并等到出结果（同步小操作走这条）。
func (c *Client) runOpOnce(key, op string, body map[string]any) (string, string, int, []byte, error) {
	status, raw, err := c.call(key, http.MethodPost, "/operations/"+op, body)
	if err != nil || status >= 400 {
		return "", "", status, raw, err
	}
	var ref taskRef
	if json.Unmarshal(raw, &ref) != nil || ref.TaskID == "" {
		return "", "", status, raw, fmt.Errorf("投 %s 任务响应异常：%s", op, snippet(raw))
	}
	// 轮询到完成（小操作通常几秒到一分钟）
	deadline := timeNowAdd(maxWait)
	for {
		st, s2, r2, e2 := c.taskStatus(key, ref.TaskID)
		if e2 != nil {
			return "", "", 0, nil, e2
		}
		if s2 >= 400 {
			return "", "", s2, r2, fmt.Errorf("查状态失败 http %d", s2)
		}
		switch st.Status {
		case "completed":
			a, r, _, _, e3 := c.Result(key, ref.TaskID)
			return a, r, 200, nil, e3
		case "failed", "revoked":
			msg := "任务失败"
			if st.Error != nil && *st.Error != "" {
				msg = *st.Error
			}
			return "", "", 200, nil, fmt.Errorf("%s：%s", st.Status, msg)
		}
		if timeNowAfter(deadline) {
			return "", "", 0, nil, fmt.Errorf("超过 %s 仍未完成", maxWait)
		}
		sleepFor(c.pollEvery())
	}
}

// timeNowAdd / timeNowAfter / sleepFor —— 小工具，便于测试替换。
func timeNowAdd(d time.Duration) time.Time { return time.Now().Add(d) }
func timeNowAfter(t time.Time) bool        { return time.Now().After(t) }
func sleepFor(d time.Duration)             { time.Sleep(d) }
