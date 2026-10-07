// relay.go 把 ChatStream 合成的 SSE 交给客户端（流式）或收敛成单个响应（非流式）。
//
// 为什么不复用通用网关的规范化层：这里的帧**本来就是本包按 OpenAI 规范造的**，
// 再过一遍解析/重建纯属浪费。唯一必须守住的是收尾语义：
// **失败只发 error 帧、绝不补 [DONE]**（补了等于把截断伪装成正常结束）。
package fsapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUpstream 流内错误（error 帧里带的原文）。
type ErrUpstream struct {
	Code    string
	Message string
}

func (e *ErrUpstream) Error() string { return e.Message }

// Relay 逐帧透传到 w（流式路径）。
//
// 返回 nil = 正常收尾（收到 [DONE]）；返回 error = 中途失败，此时**已经**写过一帧
// error 且**没有**补 [DONE]。
func Relay(w http.ResponseWriter, rc io.ReadCloser, onUsage func(map[string]any)) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	br := bufio.NewReaderSize(rc, 64*1024)
	sawDone := false
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimPrefix(trimmed, "data: ")
			if payload == "[DONE]" {
				sawDone = true
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				if fl != nil {
					fl.Flush()
				}
				break
			}
			if onUsage != nil {
				var obj map[string]any
				if json.Unmarshal([]byte(payload), &obj) == nil {
					if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
						onUsage(u)
					}
				}
			}
			if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	if sawDone {
		return nil
	}
	// 没收到 [DONE]：流被上游/自身中断。补一帧 error，**不补 [DONE]**。
	msg := "upstream stream interrupted"
	if err := rc.Close(); err != nil && !errors.Is(err, io.EOF) {
		msg = err.Error()
	}
	writeErrorFrame(w, fl, msg)
	return &ErrUpstream{Code: "upstream_stream_error", Message: msg}
}

// writeErrorFrame 写一帧 OpenAI 规范的 error（不补 [DONE]）。
func writeErrorFrame(w io.Writer, fl http.Flusher, msg string) {
	if len(msg) > 300 {
		msg = msg[:300]
	}
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "upstream_error",
		"code":    "upstream_stream_error",
	}})
	_, _ = io.WriteString(w, "data: "+string(raw)+"\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// Collect 把合成的 SSE 收敛成一个 chat.completion 对象（非流式路径）。
func Collect(rc io.ReadCloser, model string) (map[string]any, error) {
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 64*1024)
	var (
		content, reasoning strings.Builder
		id                 string
		finish             = "stop"
		usage              map[string]any
		gotAny             bool
	)
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimPrefix(trimmed, "data: ")
			if payload == "[DONE]" {
				break
			}
			var obj map[string]any
			if json.Unmarshal([]byte(payload), &obj) == nil {
				if e, ok := obj["error"].(map[string]any); ok {
					msg, _ := e["message"].(string)
					code, _ := e["code"].(string)
					return nil, &ErrUpstream{Code: code, Message: msg}
				}
				if v, ok := obj["id"].(string); ok && id == "" {
					id = v
				}
				if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
					usage = u
				}
				if chs, ok := obj["choices"].([]any); ok {
					for _, c := range chs {
						cm, ok := c.(map[string]any)
						if !ok {
							continue
						}
						if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
							finish = fr
						}
						dm, _ := cm["delta"].(map[string]any)
						if v, ok := dm["content"].(string); ok && v != "" {
							content.WriteString(v)
							gotAny = true
						}
						if v, ok := dm["reasoning_content"].(string); ok && v != "" {
							reasoning.WriteString(v)
							gotAny = true
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if !gotAny && content.Len() == 0 {
		return nil, fmt.Errorf("上游没有返回内容")
	}
	if id == "" {
		id = "chatcmpl-fs"
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": nowUnix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// nowUnix 当前实现直接取系统时间（留个缝方便测试替换）。
func nowUnix() int64 { return time.Now().Unix() }
