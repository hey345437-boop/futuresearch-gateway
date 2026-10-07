// Package mcp 把 FutureSearch 暴露成 MCP 工具（research / forecast / models）。
//
// 为什么是这个方向：FutureSearch 的 agent **不支持工具调用**（上游 API 没有工具回调面），
// 所以它当不了 MCP 客户端。但反过来成立 —— 它是「被调用的工具」：
// 任何会说 MCP 的客户端（Claude Desktop / dsh / Cursor）都能调它做一次研究或预测。
//
// 两种传输都支持：
//   - Streamable HTTP：挂在网关的 /mcp 上（远程客户端用）
//   - stdio：`futuresearch-gateway --mcp-stdio`（本地客户端直接 spawn 用）
package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

// ToolSet 一组 MCP 工具。一个网关可以挂多组（研究 / 本地文件 / …），
// 每组是**独立的 MCP server**（各自的 URL、各自的 serverInfo.name）——
// 混在一起会让客户端分不清「哪些工具会碰我的磁盘」。
type ToolSet interface {
	ServerName() string
	Tools() []map[string]any
	Call(name string, args map[string]any) (string, error)
}

const (
	protocolDefault = "2025-06-18"
	serverVersion   = "1.0.0"
)

// Server MCP 服务端（一个 ToolSet 一个）。
type Server struct{ set ToolSet }

// New 用给定的工具集建 MCP 服务端。
func New(set ToolSet) *Server { return &Server{set: set} }

// ---------------------------------------------------------------------------
// JSON-RPC
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Handle 处理一条 JSON-RPC 消息；返回 nil 表示这是通知（不需要回）。
func (s *Server) Handle(req *rpcRequest) *rpcResponse {
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil // 通知
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		pv := p.ProtocolVersion
		if pv == "" {
			pv = protocolDefault
		}
		resp.Result = map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.set.ServerName(), "version": serverVersion},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": s.set.Tools()}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp
		}
		text, err := s.set.Call(p.Name, p.Arguments)
		if err != nil {
			// 工具级错误按 MCP 规范放在 result 里（isError），不是 JSON-RPC error
			resp.Result = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "调用失败：" + err.Error()}},
				"isError": true,
			}
			return resp
		}
		resp.Result = map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
			"isError": false,
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

// stripProgress 去掉网关的进度事件（那是给流式客户端保活用的，对一次性调用是噪声）。
//
// 进度文案形如：
//
//	[FutureSearch] 研究中（已完成 0/1，运行中 1，已等待 12s） · Answer factual question
//
// 整段剥掉：从 `[FutureSearch] 研究中` 起，到下一个 `[FutureSearch]` 或字符串末尾。
func stripProgress(text string) string {
	const mark = "[FutureSearch] 研究中"
	out := text
	for {
		i := strings.Index(out, mark)
		if i < 0 {
			break
		}
		next := strings.Index(out[i+len(mark):], mark)
		if next < 0 {
			out = out[:i]
			break
		}
		out = out[:i] + out[i+len(mark)+next:]
	}
	return strings.TrimSpace(out)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------------------
// 传输 1：Streamable HTTP（挂 /mcp）
// ---------------------------------------------------------------------------

// HTTPHandler 处理 POST /mcp（JSON-RPC in，JSON-RPC out）。
func (s *Server) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// 规范允许 GET 开 SSE 流；本实现不需要服务端推送，回 405 让客户端走 POST
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var req rpcRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeRPC(w, &rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			return
		}
		resp := s.Handle(&req)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, resp)
	})
}

func writeRPC(w http.ResponseWriter, resp *rpcResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// ---------------------------------------------------------------------------
// 传输 2：stdio（--mcp-stdio）
// ---------------------------------------------------------------------------

// ServeStdio 行分隔 JSON-RPC over stdin/stdout。
func (s *Server) ServeStdio() error {
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	enc := json.NewEncoder(os.Stdout)
	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			var req rpcRequest
			if json.Unmarshal(line, &req) == nil {
				if resp := s.Handle(&req); resp != nil {
					if e := enc.Encode(resp); e != nil {
						return e
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
