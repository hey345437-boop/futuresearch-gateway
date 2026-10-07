// ops.go 上游那几个「批量操作」的封装：分类 / 打分 / 去重 / 语义合并 / 多 agent。
//
// 这批操作在上游都标了 deprecated，但**端点还活着**（实测 422 校验错误 = 活着），
// 官方那个 futuresearch-mcp 也照常在用。所以照实现。
//
// 入参形态：这些操作都吃**表格**（input = 行列表或 artifact id）。
// 网关这边用 `{"text": …}` 当行，够覆盖「把一堆文本分类/排序」这类用法。
package fsapi

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// rowsOf 把字符串列表包成上游要的行。
func rowsOf(items []string) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, s := range items {
		out = append(out, map[string]any{"text": s})
	}
	return out
}

// MultiAgentOp 多 agent 并行研究（可以指定研究方向）。
func (c *Client) MultiAgentOp(key, task string, directions []string, effort string) (string, string, int, []byte, error) {
	body := map[string]any{"input": []map[string]any{}, "task": task}
	if len(directions) > 0 {
		if len(directions) > 6 { // 上游上限 6 个方向
			directions = directions[:6]
		}
		body["directions"] = directions
	}
	switch effort {
	case "low", "medium", "high":
		body["effort_level"] = effort
	}
	return c.runOpOnce(key, "multi-agent", body)
}

// ClassifyOp 把一批条目分到给定类别里。
func (c *Client) ClassifyOp(key, task string, categories, items []string) (string, string, int, []byte, error) {
	if len(categories) == 0 {
		return "", "", 400, nil, fmt.Errorf("categories 不能为空")
	}
	body := map[string]any{
		"input":                rowsOf(items),
		"task":                 task,
		"categories":           categories,
		"classification_field": "category",
		"include_reasoning":    true,
	}
	return c.runOpOnce(key, "classify", body)
}

// RankOp 按任务给一批条目打分排序。
//
// 上游要 `sort_by` 且必须是 response_schema 里的字段 —— 所以这里固定一个
// `{"score": number}` 的 schema，按 score 降序。
func (c *Client) RankOp(key, task string, items []string, ascending bool) (string, string, int, []byte, error) {
	body := map[string]any{
		"input": rowsOf(items),
		"task":  task,
		"response_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"score": map[string]any{"type": "number"}},
			"required":   []string{"score"},
		},
		"sort_by":   "score",
		"ascending": ascending,
	}
	return c.runOpOnce(key, "rank", body)
}

// DedupeOp 去重（identify = 只标出重复；其它策略 = 选/合并）。
func (c *Client) DedupeOp(key, equivalence string, items []string, strategy string) (string, string, int, []byte, error) {
	body := map[string]any{
		"input":                rowsOf(items),
		"equivalence_relation": equivalence,
	}
	if strategy != "" {
		body["strategy"] = strategy
	}
	return c.runOpOnce(key, "dedupe", body)
}

// MergeOp 语义表连接（按任务把两张表合起来）。
func (c *Client) MergeOp(key, task string, left, right []string, leftKey, rightKey string) (string, string, int, []byte, error) {
	body := map[string]any{
		"left_input":  rowsOf(left),
		"right_input": rowsOf(right),
		"task":        task,
	}
	if leftKey != "" {
		body["left_key"] = leftKey
	}
	if rightKey != "" {
		body["right_key"] = rightKey
	}
	return c.runOpOnce(key, "merge", body)
}

// UploadData 上传一批行，返回 artifact id（之后可以把它当 input 用）。
func (c *Client) UploadData(key string, data any) (string, int, []byte, error) {
	status, raw, err := c.call(key, http.MethodPost, "/artifacts/upload", map[string]any{"data": data})
	if err != nil || status >= 400 {
		return "", status, raw, err
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", status, raw, fmt.Errorf("上传响应解析失败：%s", snippet(raw))
	}
	for _, k := range []string{"artifact_id", "id"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v, status, raw, nil
		}
	}
	return "", status, raw, fmt.Errorf("上传响应里没有 artifact id：%s", snippet(raw))
}

// BuiltInLists 上游内置的数据列表（人名/机构/公司之类的现成表）。
func (c *Client) BuiltInLists(key string) ([]map[string]any, int, []byte, error) {
	status, raw, err := c.call(key, http.MethodGet, "/built-in-lists", nil)
	if err != nil || status >= 400 {
		return nil, status, raw, err
	}
	var wrap struct {
		Lists []map[string]any `json:"lists"`
	}
	if json.Unmarshal(raw, &wrap) != nil {
		return nil, status, raw, fmt.Errorf("内置列表响应解析失败")
	}
	return wrap.Lists, status, raw, nil
}

// UseBuiltInList 把内置列表复制成自己的 artifact（返回 artifact id）。
func (c *Client) UseBuiltInList(key, nameOrID string) (string, int, []byte, error) {
	// 先按名字/id 找
	lists, status, raw, err := c.BuiltInLists(key)
	if err != nil || status >= 400 {
		return "", status, raw, err
	}
	var target map[string]any
	for _, l := range lists {
		if n, _ := l["name"].(string); n == nameOrID {
			target = l
			break
		}
		if a, _ := l["artifact_id"].(string); a == nameOrID {
			target = l
			break
		}
	}
	if target == nil {
		return "", 404, nil, fmt.Errorf("没找到内置列表 %q（用 browse_lists 看有哪些）", nameOrID)
	}
	body := map[string]any{}
	if a, ok := target["artifact_id"].(string); ok {
		body["artifact_id"] = a
	}
	if n, ok := target["name"].(string); ok {
		body["name"] = n
	}
	st, rw, err := c.call(key, http.MethodPost, "/built-in-lists/use", body)
	if err != nil || st >= 400 {
		return "", st, rw, err
	}
	var m map[string]any
	if json.Unmarshal(rw, &m) == nil {
		for _, k := range []string{"artifact_id", "id"} {
			if v, ok := m[k].(string); ok && v != "" {
				return v, st, rw, nil
			}
		}
	}
	return "", st, rw, fmt.Errorf("响应里没有 artifact id：%s", snippet(rw))
}

// SessionTasks 某个会话里的任务。
func (c *Client) SessionTasks(key, sessionID string) ([]map[string]any, int, []byte, error) {
	status, raw, err := c.call(key, http.MethodGet, "/sessions/"+sessionID+"/tasks", nil)
	if err != nil || status >= 400 {
		return nil, status, raw, err
	}
	var wrap struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if json.Unmarshal(raw, &wrap) != nil {
		var list []map[string]any
		if json.Unmarshal(raw, &list) == nil {
			return list, status, raw, nil
		}
		return nil, status, raw, fmt.Errorf("会话任务响应解析失败")
	}
	return wrap.Tasks, status, raw, nil
}

// TaskSummaries 任务进度摘要（上游会生成人类可读的过程摘要）。
func (c *Client) TaskSummaries(key, taskID string) ([]map[string]any, int, []byte, error) {
	status, raw, err := c.call(key, http.MethodGet, "/tasks/"+taskID+"/summaries", nil)
	if err != nil || status >= 400 {
		return nil, status, raw, err
	}
	var list []map[string]any
	if json.Unmarshal(raw, &list) == nil {
		return list, status, raw, nil
	}
	var wrap struct {
		Summaries []map[string]any `json:"summaries"`
	}
	if json.Unmarshal(raw, &wrap) == nil {
		return wrap.Summaries, status, raw, nil
	}
	return nil, status, raw, nil // 拿不到就算了，调用方给「暂无摘要」
}
