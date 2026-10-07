// stream.go 把「OpenAI chat 请求」翻成 FutureSearch 任务，并把任务结果合成 OpenAI SSE。
//
// 为什么必须合成：上游是异步任务 API（投任务 → 轮询 → 取结果），没有可透传的 SSE。
// 出口帧仍走统一规范化层（internal/upstream），故收尾语义与其它渠道一致：
//   - 正常完成：正文分片 + finish_reason=stop + 恰好一个 [DONE]
//   - 失败/超时：**只** CloseWithError（统一层据此发一帧 error，且不补 [DONE]，R36/R40）
//
// 等待期每 pollInterval 发一帧空 delta keepalive：上游 low 档实测 23s、
// high 档可能十几分钟，不发心跳的话客户端会先于上游判空闲超时。
package fsapi

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 请求侧：OpenAI messages → 任务文本
// ---------------------------------------------------------------------------

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCallIn    `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

// toolCallIn 客户端回传的历史工具调用（assistant 轮里的）。
type toolCallIn struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	// ReasoningEffort OpenAI 风格的档位字段（dsh 的 reasoningEfforts 就发这个）。
	// 只对「模型家族」有意义：把家族名 + 档位合成完整的上游 llm 枚举。
	ReasoningEffort string `json:"reasoning_effort"`
	// Reasoning OpenRouter 风格的嵌套写法，做一次兼容。
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	// Tools 客户端传的工具定义。上游没有 function calling 面，所以这里走
	// 「提示词模拟」——见 toolbridge.go。
	Tools []toolDef `json:"tools"`
	// ToolChoice 目前只用于判断「客户端到底想不想调工具」，不改变协议。
	ToolChoice json.RawMessage `json:"tool_choice"`
}

// effort 取请求里的档位（两种写法都认，顶层优先）。
func (r chatRequest) effort() string {
	if s := strings.TrimSpace(r.ReasoningEffort); s != "" {
		return s
	}
	return strings.TrimSpace(r.Reasoning.Effort)
}

// contextBudget 多轮对话拼进任务的上下文上限（rune，取尾部）。
// 上游的 task 是「一次研究的指令」，不是聊天记录，塞太长只会稀释问题本身。
const contextBudget = 6000

// buildTask 把 messages 折成一条自然语言任务。
//
// 口径：
//   - **忽略 system/developer**：那是客户端（Claude Code / dsh）的 agent 提示词，
//     喂给研究 agent 会让它误以为自己是编码助手；用户的真实问题在 user 消息里。
//   - 最后一条 user 消息 = 本次要研究的问题；
//   - 若之前还有对话轮次，按时间顺序拼成「上下文」前置（尾部截断到 contextBudget）。
func buildTask(msgs []chatMessage) string {
	type turn struct{ role, text string }
	var turns []turn
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "user", "assistant":
			txt := contentText(m.Content)
			// assistant 轮里如果带 tool_calls，把它也写进上下文 ——
			// 否则模型看不到自己上一轮调了什么，会重复调。
			if role == "assistant" && len(m.ToolCalls) > 0 {
				var b strings.Builder
				if txt != "" {
					b.WriteString(txt)
					b.WriteString("\n")
				}
				for _, tc := range m.ToolCalls {
					b.WriteString("（已调用工具 ")
					b.WriteString(tc.Function.Name)
					b.WriteString("，参数 ")
					b.WriteString(tailRunes(tc.Function.Arguments, 300))
					b.WriteString("）")
				}
				txt = b.String()
			}
			if txt != "" {
				turns = append(turns, turn{role, txt})
			}
		case "tool":
			// 工具执行结果 —— 标成「工具结果」而不是助手发言，
			// 模型才知道这是它要的返回值。
			if txt := contentText(m.Content); txt != "" {
				turns = append(turns, turn{"tool", "【工具 " + m.Name + " 的结果】\n" + tailRunes(txt, 6000)})
			}
		}
	}
	if len(turns) == 0 {
		return ""
	}
	last := turns[len(turns)-1]
	prior := turns[:len(turns)-1]
	if len(prior) == 0 {
		return last.text
	}

	var b strings.Builder
	for _, t := range prior {
		who := "用户"
		switch t.role {
		case "assistant":
			who = "助手"
		case "tool":
			who = "工具结果"
		}
		b.WriteString(who)
		b.WriteString("：")
		b.WriteString(t.text)
		b.WriteString("\n")
	}
	ctx := tailRunes(strings.TrimSpace(b.String()), contextBudget)
	head := "以下是本次对话的上下文（按时间顺序，可能已截断）：\n\n" + ctx + "\n\n"
	// 最后一轮是**工具结果**时换个说法：那不是「用户的新问题」，
	// 而是「你上一步要的返回值」——措辞不对模型会当成新任务重头再来。
	if last.role == "tool" {
		return head + "这是你上一步请求的工具返回值，请据此继续完成任务（该再调工具就继续调，该收尾就收尾）：\n" + last.text
	}
	return head + "请基于以上上下文，研究并回答最后这个问题：\n" + last.text
}

// contentText 从 message.content 取纯文本。
// content 有两种合法形态：字符串，或 `[{"type":"text","text":…}, …]`（多模态分片）。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "" || p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// hasImagePart 请求里有没有图片。
//
// 为什么必须显式检查：上游 **不支持图片输入**（OpenAPI 规范里 png/jpeg/vision/base64
// 出现 0 次，上传只有 CSV/JSON）。而 contentText 只会拼文本部分 —— 图片会被**静默丢掉**，
// 于是模型对着一个「空问题」给出自信的胡答。宁可明确报错，也不要静默降级。
func hasImagePart(msgs []chatMessage) bool {
	for _, m := range msgs {
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			switch p.Type {
			case "image_url", "image", "input_image":
				return true
			}
		}
	}
	return false
}

// tailRunes 取字符串尾部 n 个 rune（rune 安全，不切碎多字节字符）。
func tailRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[len(rs)-n:])
}

// ---------------------------------------------------------------------------
// 回放侧：任务结果 → OpenAI SSE
// ---------------------------------------------------------------------------

// pump 轮询任务并写出 SSE，直到完成/失败/超时。
//
// 退出路径只有三种，且都**不再补 [DONE]**（除正常完成外）：
//   - 正常完成：正文 + finish_reason=stop + [DONE] + Close()
//   - 失败/超时：CloseWithError（统一层发 error 帧）
//   - 下游断开：写管道失败，直接退出（不再打上游）
func (c *Client) pump(pw *io.PipeWriter, key string, spec modelSpec, ref *taskRef, model string, promptRunes int, toolsActive bool) {
	id := "chatcmpl-fs-" + ref.TaskID
	started := time.Now()
	deadline := started.Add(maxWait)
	lastNote := "任务仍在运行"
	var lastEmit time.Time
	lastKey := ""
	failStreak := 0

	if err := writeChunk(pw, id, model, map[string]any{"role": "assistant"}, nil); err != nil {
		_ = pw.CloseWithError(err)
		return
	}

	for {
		if time.Now().After(deadline) {
			_ = pw.CloseWithError(fmt.Errorf("futuresearch: 任务 %s 超过 %s 仍未完成（%s）",
				ref.TaskID, maxWait, lastNote))
			return
		}

		// 状态查询失败的处理：**上游偶发 5xx / 网络抖动不该判死一个还在跑的任务**。
		// 2026-10-07 实测：任务跑到 65s 时 /tasks/{id}/status 回了一次 500，
		// 整个请求就被截断了 —— 而任务其实还在上游跑着。故：
		//   4xx（404 任务不存在等）= 终态，直接报；
		//   5xx / 传输层错误 = 连续 maxStatusFailures 次才放弃，其间照常发进度事件。
		st, status, raw, err := c.taskStatus(key, ref.TaskID)
		if err != nil || status >= 400 {
			if status >= 400 && status < 500 {
				_ = pw.CloseWithError(fmt.Errorf("futuresearch: 查询任务状态失败 http %d: %s", status, snippet(raw)))
				return
			}
			failStreak++
			if err == nil {
				err = fmt.Errorf("http %d: %s", status, snippet(raw))
			}
			if failStreak >= maxStatusFailures {
				_ = pw.CloseWithError(fmt.Errorf("futuresearch: 查询任务状态连续 %d 次失败：%w", failStreak, err))
				return
			}
			// 不 return：下面照常发进度事件，让客户端知道还在等
			if time.Since(lastEmit) >= progressEvery {
				note := fmt.Sprintf("[FutureSearch] 研究中（状态查询连续失败 %d 次，仍在校验任务 %s）", failStreak, ref.TaskID)
				if werr := writeChunk(pw, id, model, map[string]any{"reasoning_content": note}, nil); werr != nil {
					_ = pw.CloseWithError(werr)
					return
				}
				lastNote, lastKey, lastEmit = note, "status-fail-"+fmt.Sprint(failStreak), time.Now()
			}
			time.Sleep(c.pollEvery())
			continue
		}
		failStreak = 0
		if st.Progress != nil {
			lastNote = fmt.Sprintf("进度 %d/%d（运行中 %d，失败 %d）",
				st.Progress.Completed, st.Progress.Total, st.Progress.Running, st.Progress.Failed)
		}

		switch st.Status {
		case "completed":
			c.emitResult(pw, id, model, key, ref.TaskID, promptRunes, toolsActive)
			return
		case "failed", "revoked":
			reason := "任务被上游判定为失败"
			if st.Error != nil && strings.TrimSpace(*st.Error) != "" {
				reason = strings.TrimSpace(*st.Error)
			}
			_ = pw.CloseWithError(fmt.Errorf("futuresearch: 任务 %s %s：%s", ref.TaskID, st.Status, reason))
			return
		}

		// 进度事件（**不是**空心跳）。
		//
		// 为什么不能用空 delta 帧保活：客户端的「流空闲看门狗」按**解析出来的事件**计时，
		// 不按字节数。空 delta 解析不出任何事件 ⇒ 任务跑满 5 分钟时客户端照样判超时
		// （2026-10-07 实测：dsh 报 `pi-ai stream idle timeout after 300000ms`，
		// 而网关侧日志显示一直在写、直到客户端断开才 broken pipe）。
		//
		// 所以把进度当 reasoning_content 发出去：既是真实事件（重置看门狗），
		// 也让等待期在客户端的「思考」面板里可见（否则 20s~5min 完全是黑箱）。
		// 节流：进度有变化就发，否则每 progressEvery 发一次（远小于任何客户端的空闲上限）。
		// 节流键只取「进度本身」（计数 + 标签），不含等待秒数 —— 否则每轮都会变、
		// 变成每秒一条刷屏。真实进度变化立刻发，否则每 progressEvery 发一条带等待时长的。
		key := progressKey(st)
		if key != lastKey || time.Since(lastEmit) >= progressEvery {
			note := progressNote(st, time.Since(started))
			if err := writeChunk(pw, id, model, map[string]any{"reasoning_content": note}, nil); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			lastNote, lastKey, lastEmit = note, key, time.Now()
		}
		time.Sleep(c.pollEvery())
	}
}

// progressKey 进度的稳定标识（不含等待时长），用于节流比较。
func progressKey(st *taskRef) string {
	var b strings.Builder
	b.WriteString(st.Status)
	if st.Progress != nil {
		fmt.Fprintf(&b, "|%d/%d/%d/%d", st.Progress.Pending, st.Progress.Running, st.Progress.Completed, st.Progress.Failed)
	}
	b.WriteString("|" + st.Label)
	return b.String()
}

// progressNote 生成一条人类可读的进度文案（发给客户端当 reasoning_content）。
func progressNote(st *taskRef, waited time.Duration) string {
	var b strings.Builder
	b.WriteString("[FutureSearch] 研究中")
	if st.Progress != nil {
		fmt.Fprintf(&b, "（已完成 %d/%d", st.Progress.Completed, st.Progress.Total)
		if st.Progress.Running > 0 {
			fmt.Fprintf(&b, "，运行中 %d", st.Progress.Running)
		}
		if st.Progress.Failed > 0 {
			fmt.Fprintf(&b, "，失败 %d", st.Progress.Failed)
		}
		fmt.Fprintf(&b, "，已等待 %.0fs）", waited.Seconds())
	} else {
		fmt.Fprintf(&b, "（已等待 %.0fs）", waited.Seconds())
	}
	if st.Label != "" {
		b.WriteString(" · " + st.Label)
	}
	return b.String()
}

// emitResult 取结果并按 OpenAI 语义回放：研究过程 → 正文 → stop → [DONE]。
func (c *Client) emitResult(pw *io.PipeWriter, id, model, key, taskID string, promptRunes int, toolsActive bool) {
	res, status, raw, err := c.taskResultOf(key, taskID)
	if err != nil {
		_ = pw.CloseWithError(fmt.Errorf("futuresearch: 取任务结果失败：%w", err))
		return
	}
	if status >= 400 {
		_ = pw.CloseWithError(fmt.Errorf("futuresearch: 取任务结果失败 http %d: %s", status, snippet(raw)))
		return
	}
	if res.Error != nil && strings.TrimSpace(*res.Error) != "" {
		_ = pw.CloseWithError(fmt.Errorf("futuresearch: 任务结果含错误：%s", strings.TrimSpace(*res.Error)))
		return
	}
	content, reasoning := formatResult(res.Data)
	if content == "" && reasoning == "" {
		_ = pw.CloseWithError(fmt.Errorf("futuresearch: 任务 %s 完成但结果为空", taskID))
		return
	}

	// 研究过程先发（reasoning_content），再发正文：客户端若渲染思考，
	// 顺序与人类阅读一致；不渲染也不丢正文。
	for _, part := range chunkRunesOf(reasoning, chunkRunes) {
		if err := writeChunk(pw, id, model, map[string]any{"reasoning_content": part}, nil); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}
	// ★ 工具桥：客户端带了 tools 时，正文**先别当文本发** ——
	// 它可能是一行工具调用 JSON。解析得出来就发 tool_calls（coding agent 靠这个驱动），
	// 解析不出来再当普通文本发（优雅降级，不会把请求搞坏）。
	if toolsActive {
		if tc := parseToolCall(content); tc != nil {
			call := openAIToolCall(tc, 0)
			delta := map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": call["id"], "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": string(tc.Args)}},
			}}
			finish := "tool_calls"
			if err := writeChunk(pw, id, model, delta, &finish); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if err := writeUsageChunk(pw, id, model, promptRunes, len([]rune(content))+len([]rune(reasoning))); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			_, _ = io.WriteString(pw, "data: [DONE]\n\n")
			_ = pw.Close()
			return
		}
	}
	for _, part := range chunkRunesOf(content, chunkRunes) {
		if err := writeChunk(pw, id, model, map[string]any{"content": part}, nil); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}
	stop := "stop"
	if err := writeChunk(pw, id, model, map[string]any{}, &stop); err != nil {
		_ = pw.CloseWithError(err)
		return
	}
	// usage 帧（OpenAI 规范的「只有 usage、没有 choices」那一帧）。
	//
	// 为什么必须发：**多租户网关（one-api / new-api / sub2api 这类）靠 usage 记账**，
	// 没有这一帧它们记不到任何量、面板上永远是 0。而上游按任务计费、不返回 token 用量，
	// 所以这里给的是**估算值**（见 estimateTokens 的注释）—— 够网关展示相对用量，
	// 但**不是**上游口径，别拿它对账。
	if err := writeUsageChunk(pw, id, model, promptRunes, len([]rune(content))+len([]rune(reasoning))); err != nil {
		_ = pw.CloseWithError(err)
		return
	}
	_, _ = io.WriteString(pw, "data: [DONE]\n\n")
	_ = pw.Close()
}

// estimateTokens 粗估 token 数：中英混排按「2 个字符 ≈ 1 token」。
//
// ⚠️ 这是**估算**，不是上游口径 —— FutureSearch 按任务计费，不返回 token 用量。
// 唯一用途是让下游网关/客户端有量可记（否则它们的用量面板恒为 0）。
// 宁可标成估算也不编一个看起来精确的假数字（见 R23b 的纪律）。
func estimateTokens(runes int) int {
	if runes <= 0 {
		return 0
	}
	return (runes + 1) / 2
}

// writeUsageChunk 写一帧「只有 usage、choices 为空」的收尾帧。
func writeUsageChunk(pw *io.PipeWriter, id, model string, promptRunes, completionRunes int) error {
	pt, ct := estimateTokens(promptRunes), estimateTokens(completionRunes)
	frame := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     pt,
			"completion_tokens": ct,
			"total_tokens":      pt + ct,
		},
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_, err = io.WriteString(pw, "data: "+string(raw)+"\n\n")
	return err
}

// writeChunk 写一帧 OpenAI chat.completion.chunk。
// finish 为 nil 时不带 finish_reason（规范里它是 null，normalizeFrame 会补 null）。
func writeChunk(pw *io.PipeWriter, id, model string, delta map[string]any, finish *string) error {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != nil {
		choice["finish_reason"] = *finish
	}
	frame := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{choice},
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(pw, "data: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	return nil
}

// formatResult 把任务结果折成 (正文, 研究过程)。
//
// 结果形态（v0 API 实测）：
//
//	agent-map / multi-agent：data = [{"answer": "…", "research": {"answer": "…"}}]
//	                         （low 档无 research；medium/high 带）
//	forecast：               data = [{…概率字段…}]
//	data 也可能是单条记录而非列表。
//
// 有 `answer` 就用它（研究过程取 research/reasoning 子对象的 answer）；
// 没有就按字段平铺成 markdown 列表 —— 未知操作也能给出可读结果，不做静默丢弃。
func formatResult(data json.RawMessage) (string, string) {
	if len(data) == 0 || string(data) == "null" {
		return "", ""
	}
	recs := decodeRecords(data)
	if len(recs) == 0 {
		return strings.TrimSpace(string(data)), ""
	}

	var contents, reasonings []string
	for _, rec := range recs {
		contents = append(contents, recordAnswer(rec))
		if r := recordReasoning(rec); r != "" {
			reasonings = append(reasonings, r)
		}
	}
	return strings.Join(contents, "\n\n"), strings.Join(reasonings, "\n\n")
}

// decodeRecords 把 data 解成记录列表（兼容「列表」与「单条记录」两种形态）。
func decodeRecords(data json.RawMessage) []map[string]any {
	var recs []map[string]any
	if err := json.Unmarshal(data, &recs); err == nil {
		return recs
	}
	var one map[string]any
	if err := json.Unmarshal(data, &one); err == nil {
		return []map[string]any{one}
	}
	return nil
}

// recordAnswer 取一条记录的主答案。
//
// 三种形态：
//   - agent-map / multi-agent：有 `answer` 字段 → 直接用
//   - forecast：有 `probability`（0–100）→ 折成「概率 + 理由」的可读正文
//   - 其它未知操作：按字段平铺成 markdown 列表（不静默丢弃）
func recordAnswer(rec map[string]any) string {
	if s, ok := rec["answer"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if p, ok := rec["probability"]; ok && p != nil {
		var b strings.Builder
		b.WriteString("**概率：")
		b.WriteString(renderValue(p))
		b.WriteString("%**")
		if s, ok := rec["rationale"].(string); ok && strings.TrimSpace(s) != "" {
			b.WriteString("\n\n")
			b.WriteString(strings.TrimSpace(s))
		}
		if s, ok := rec["consistency_note"].(string); ok && strings.TrimSpace(s) != "" {
			b.WriteString("\n\n_一致性校准：")
			b.WriteString(strings.TrimSpace(s))
			b.WriteString("_")
		}
		return b.String()
	}
	return renderRecord(rec)
}

// recordReasoning 取一条记录的研究过程（research / reasoning 子对象的 answer 字段）。
// 上游在 low 档会下发空串子对象（实测 forecast），故空串一律跳过。
func recordReasoning(rec map[string]any) string {
	for _, key := range []string{"research", "reasoning"} {
		sub, ok := rec[key].(map[string]any)
		if !ok {
			continue
		}
		if s, ok := sub["answer"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		if rendered := renderRecord(sub); rendered != "" {
			return rendered
		}
	}
	return ""
}

// renderRecord 把一条记录平铺成 markdown 列表（无 answer/probability 时的兜底渲染）。
// 键按字典序稳定输出；nil 与空串字段跳过（上游常下发空占位）；嵌套对象/数组按紧凑 JSON。
func renderRecord(rec map[string]any) string {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v := rec[k]
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(renderValue(v))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// renderValue 渲染单个字段值。
func renderValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(raw)
	}
}

// chunkRunesOf 按 rune 切分文本（空串返回 nil）。
func chunkRunesOf(s string, n int) []string {
	if s == "" {
		return nil
	}
	if n <= 0 {
		return []string{s}
	}
	rs := []rune(s)
	var out []string
	for i := 0; i < len(rs); i += n {
		end := i + n
		if end > len(rs) {
			end = len(rs)
		}
		out = append(out, string(rs[i:end]))
	}
	return out
}
