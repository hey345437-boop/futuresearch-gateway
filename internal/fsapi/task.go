// task.go FutureSearch v0 API 的薄封装：投任务 / 轮询 / 取结果 / 查余额。
//
// 所有请求都带 `Authorization: Bearer <api_key>`；上游错误一律**原文透传**
// （status + body 交回调用方，由 Classify 归类），不在本层包装成自定义错误，
// 以便 handler 按不变量 8「上游 ≥400 原样回给客户端」处理。
package fsapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// taskRef 投任务/查状态的公共字段。
type taskRef struct {
	TaskID    string  `json:"task_id"`
	SessionID string  `json:"session_id"`
	Status    string  `json:"status"`
	Error     *string `json:"error"`
	Label     string  `json:"label"`
	Progress  *struct {
		Pending   int `json:"pending"`
		Running   int `json:"running"`
		Completed int `json:"completed"`
		Failed    int `json:"failed"`
		Total     int `json:"total"`
	} `json:"progress"`
}

// taskResult 取结果的响应体。data 可能是「记录列表」「单条记录」或 null。
type taskResult struct {
	TaskID string          `json:"task_id"`
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
	Error  *string         `json:"error"`
}

// whoami 用于面板「测试」与添加账号时的凭据校验。
type whoami struct {
	User struct {
		ID         string `json:"id"`
		Email      string `json:"email"`
		Disabled   bool   `json:"disabled"`
		AuthMethod string `json:"auth_method"`
	} `json:"user"`
}

// billing 余额口径：美元。面板按「分」展示（×100 取整）。
type billing struct {
	CurrentBalanceDollars *float64 `json:"current_balance_dollars"`
}

// apiKeyOf 取账号的 API key（Key 为空时返回空串，调用方按 401 处理）。
func apiKeyOf(a *Key) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.Key)
}

// call 发一次 JSON 请求。返回 (HTTP 状态码, 原始响应体, 传输层错误)。
// 状态码 ≥400 **不**算 error：上游错误体要原样带回调用方。
func (c *Client) call(key, method, path string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base()+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// createTask 投一个任务。spec 决定操作与档位，task 是自然语言指令。
//
// 返回 (任务引用, 状态码, 原始响应体, 传输层错误)：
//   - 传输层错误 → 调用方换号重试（handler 的 errCount 路径）；
//   - 状态码 ≥400 → 调用方按 Classify 归类（402 余额不足 / 401 key 失效 / 422 参数）。
func (c *Client) createTask(key string, spec modelSpec, task string) (*taskRef, int, []byte, error) {
	status, raw, err := c.call(key, http.MethodPost, "/operations/"+string(spec.Op), buildOpBody(spec, task))
	if err != nil {
		return nil, 0, nil, err
	}
	if status >= 400 {
		return nil, status, raw, nil
	}
	var ref taskRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, status, raw, fmt.Errorf("futuresearch: 解析投任务响应失败: %w", err)
	}
	if ref.TaskID == "" {
		return nil, status, raw, fmt.Errorf("futuresearch: 投任务响应缺 task_id")
	}
	return &ref, status, raw, nil
}

// buildOpBody 按操作类型拼请求体。
//
// 三类操作的入参差异（OpenAPI 实测）：
//
//	agent-map / multi-agent：`input` 可为**空列表**，即「不喂数据，只按 task 生成一行结果」
//	                         —— 这正是 chat 请求的形态（用户问题 = task）。
//	forecast：`input` 必填且必须有行（task 只是整体上下文），
//	          故把问题塞进单行 `{"question": …}`，forecast_type 固定 binary。
func buildOpBody(spec modelSpec, task string) map[string]any {
	// 真模型档：显式 llm + iteration_budget=0（单次调用）。
	// 注意 **不能** 同时带 effort_level —— 上游把两者视为互斥，同时给会 422。
	if spec.LLM != "" {
		return map[string]any{
			"input":             []map[string]any{},
			"task":              task,
			"llm":               spec.LLM,
			"iteration_budget":  0,
			"include_reasoning": true,
		}
	}
	switch spec.Op {
	case opForecast:
		return map[string]any{
			"input":         []map[string]any{{"question": task}},
			"task":          "对每一行的 question 给出二值预测：一个 0–100 的概率，并附简短理由与出处。",
			"forecast_type": "binary",
			"effort_level":  spec.Effort,
		}
	case opMultiAgent:
		return map[string]any{
			"input":        []map[string]any{},
			"task":         task,
			"effort_level": spec.Effort,
		}
	default: // opAgentMap
		return map[string]any{
			"input":        []map[string]any{},
			"task":         task,
			"effort_level": spec.Effort,
		}
	}
}

// taskStatus 查任务状态。
func (c *Client) taskStatus(key, taskID string) (*taskRef, int, []byte, error) {
	status, raw, err := c.call(key, http.MethodGet, "/tasks/"+taskID+"/status", nil)
	if err != nil {
		return nil, 0, nil, err
	}
	if status >= 400 {
		return nil, status, raw, nil
	}
	var ref taskRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, status, raw, fmt.Errorf("futuresearch: 解析任务状态失败: %w", err)
	}
	return &ref, status, raw, nil
}

// taskResultOf 取任务结果。
func (c *Client) taskResultOf(key, taskID string) (*taskResult, int, []byte, error) {
	status, raw, err := c.call(key, http.MethodGet, "/tasks/"+taskID+"/result", nil)
	if err != nil {
		return nil, 0, nil, err
	}
	if status >= 400 {
		return nil, status, raw, nil
	}
	var res taskResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, status, raw, fmt.Errorf("futuresearch: 解析任务结果失败: %w", err)
	}
	return &res, status, raw, nil
}

// balanceDollars 查余额（美元）。
func (c *Client) balanceDollars(key string) (float64, error) {
	status, raw, err := c.call(key, http.MethodGet, "/billing", nil)
	if err != nil {
		return 0, err
	}
	if status >= 400 {
		return 0, fmt.Errorf("futuresearch: 余额查询失败 http %d: %s", status, snippet(raw))
	}
	var b billing
	if err := json.Unmarshal(raw, &b); err != nil {
		return 0, fmt.Errorf("futuresearch: 解析余额失败: %w", err)
	}
	if b.CurrentBalanceDollars == nil {
		return 0, nil
	}
	return *b.CurrentBalanceDollars, nil
}

// validateKey 用 /whoami 校验 API key，返回账号信息（面板添加账号 / 「测试」按钮）。
// 返回 (uid, email, 状态码, 原始响应体, 传输层错误)。
func (c *Client) validateKey(key string) (*whoami, int, []byte, error) {
	status, raw, err := c.call(key, http.MethodGet, "/whoami", nil)
	if err != nil {
		return nil, 0, nil, err
	}
	if status >= 400 {
		return nil, status, raw, nil
	}
	var w whoami
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, status, raw, fmt.Errorf("futuresearch: 解析 whoami 失败: %w", err)
	}
	return &w, status, raw, nil
}

// snippet 截断响应体用于错误文案（不泄漏长 body）。
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
