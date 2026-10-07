// client.go FutureSearch 公开 v0 API 的客户端。
//
// 上游是**异步任务式**的：投任务 → 轮询 → 取结果，没有 chat completions。
// 所以这里把「OpenAI chat 请求」翻译成一个任务，再把结果**合成 OpenAI SSE**回放
// （合成逻辑在 stream.go）。
package fsapi

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client 上游 HTTP 客户端。Base 可覆盖以便测试。
type Client struct {
	// HTTP 非流式路径（投任务/轮询/取结果/余额）用：带整体 Timeout 兜底。
	HTTP *http.Client
	// StreamHTTP 对话流用：**不设** Client.Timeout。
	// 对话流的等待期由轮询循环承担，套整体超时会把长任务从中间掐断。
	StreamHTTP *http.Client
	Base       string
	// PollInterval 任务状态轮询间隔；零值 = pollInterval（测试注入更短的）。
	PollInterval time.Duration

	// prompts 按模型名注入的前置指令（config.prompts）。
	// 上游没有 system 字段，唯一能压过它自带输出风格的地方就是任务文本最前面。
	promptsMu sync.RWMutex
	prompts   map[string]string
}

// New 生产默认客户端。
func New() *Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	tr := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     30 * time.Second,
	}
	return &Client{
		HTTP:       &http.Client{Timeout: requestTimeout, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr}, // 共用 Transport，不设 Timeout
		Base:       DefaultBase,
	}
}

// NewWithBase 测试用：覆盖上游基址。
func NewWithBase(base string) *Client {
	c := New()
	c.Base = base
	return c
}

func (c *Client) base() string {
	if c.Base == "" {
		return DefaultBase
	}
	return c.Base
}

func (c *Client) pollEvery() time.Duration {
	if c.PollInterval > 0 {
		return c.PollInterval
	}
	return pollInterval
}

// SetPrompts 热更新「模型 → 前置指令」表。传 nil/空 map = 清除所有注入。
//
// 空值语义：**显式空串 = 该模型不注入**（且不再回落到 "*"）——
// 「没配过」与「配成空」必须是两回事。
func (c *Client) SetPrompts(m map[string]string) {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	c.promptsMu.Lock()
	c.prompts = cp
	c.promptsMu.Unlock()
}

// Prompts 当前注入表的副本（面板回显用）。
func (c *Client) Prompts() map[string]string {
	c.promptsMu.RLock()
	defer c.promptsMu.RUnlock()
	out := make(map[string]string, len(c.prompts))
	for k, v := range c.prompts {
		out[k] = v
	}
	return out
}

// PromptFor 导出给面板/调试用。
func (c *Client) PromptFor(model string) string { return c.promptFor(model) }

// promptFor 取某模型的前置指令：精确名优先（哪怕值为空），其次通配 "*"。
func (c *Client) promptFor(model string) string {
	c.promptsMu.RLock()
	defer c.promptsMu.RUnlock()
	if len(c.prompts) == 0 {
		return ""
	}
	if p, ok := c.prompts[model]; ok {
		return strings.TrimSpace(p)
	}
	return strings.TrimSpace(c.prompts["*"])
}

// ---------------------------------------------------------------------------
// 对话：OpenAI chat 请求 → 上游任务 → 合成 OpenAI SSE
// ---------------------------------------------------------------------------

// ChatStream 把 OpenAI Chat 请求体翻成一个 FutureSearch 任务。
//
// 返回的 io.ReadCloser 是**本包合成的 OpenAI SSE**（上游是异步任务 API，
// 没有可透传的流）。首帧 role、轮询期进度事件、完成后正文 + usage + [DONE]；
// 失败时 CloseWithError，调用方据此发一帧 error 且**不补** [DONE]。
//
// 返回 (reader, 上游状态码, 上游原文, 传输层错误)：状态码 ≥400 时 reader 为 nil，
// 调用方应把状态码+原文原样回给客户端。
func (c *Client) ChatStream(k *Key, body []byte) (io.ReadCloser, int, []byte, error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, http.StatusBadRequest, localError("invalid_request", "请求体不是合法 JSON"), nil
	}
	spec, ok := lookupSpec(req.Model)
	if !ok {
		return nil, http.StatusBadRequest, localError("model_not_found",
			"未知模型："+req.Model+"（可用："+modelIDList()+"）"), nil
	}
	key := apiKeyOf(k)
	if key == "" {
		return nil, http.StatusUnauthorized, localError("invalid_api_key", "账号缺少 FutureSearch API key"), nil
	}
	if hasImagePart(req.Messages) {
		return nil, http.StatusBadRequest, localError("images_not_supported",
			"这个渠道不支持图片输入：FutureSearch 的 API 只吃文本与表格（CSV/JSON）。"+
				"请把图片里的信息转成文字描述，或改用支持视觉的模型。"), nil
	}
	task := buildTask(req.Messages)
	if task == "" {
		return nil, http.StatusBadRequest, localError("invalid_request", "请求里没有可用的用户消息"), nil
	}

	// 预设档也认 reasoning_effort：客户端选档时覆盖模型名里带的档。
	if spec.Base == "" && spec.LLM == "" && spec.Effort != "" {
		switch e := strings.ToLower(req.effort()); e {
		case "low", "medium", "high":
			spec.Effort = e
		}
	}
	// 家族名 + 档位 → 完整枚举。
	if spec.Base != "" {
		if b, ok := lookupBase(spec.Base); ok {
			slug := b.pickTier(req.effort())
			if lm, ok := llmBySlug[slug]; ok {
				spec.LLM, spec.Name = lm.Enum, lm.Label
			}
		}
	}
	// 注入前置指令（精确模型名 > "*"）。
	if pre := c.promptFor(spec.ID); pre != "" {
		task = pre + "\n\n" + task
	}

	ref, status, raw, err := c.createTask(key, spec, task)
	if err != nil {
		return nil, 0, nil, err // 传输层错误：调用方换号重试
	}
	if status >= 400 {
		return nil, status, raw, nil // 上游错误原文透传
	}

	pr, pw := io.Pipe()
	go c.pump(pw, key, spec, ref, req.Model, len([]rune(task)))
	return pr, http.StatusOK, nil, nil
}

// ---------------------------------------------------------------------------
// 面板：凭据校验与余额
// ---------------------------------------------------------------------------

// ValidateKey 用 /whoami 校验 API key，返回 (uid, email)。
func (c *Client) ValidateKey(key string) (string, string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", errEmptyKey
	}
	w, status, raw, err := c.validateKey(key)
	if err != nil {
		return "", "", err
	}
	if status >= 400 {
		return "", "", &KeyError{Status: status, Body: snippet(raw)}
	}
	if w.User.ID == "" {
		return "", "", &KeyError{Status: status, Body: "响应缺 user.id"}
	}
	return w.User.ID, w.User.Email, nil
}

// Balance 查余额（美元）。
func (c *Client) Balance(key string) (float64, error) {
	return c.balanceDollars(strings.TrimSpace(key))
}

// Classify 上游错误分类（驱动号池惩罚）。
//
//	401 → key 失效 → 禁用该号（要人工重加）
//	402 → 余额不足 → 硬冷却（换号有效）
//	403 → 账号级拒绝 → 短冷却轮换，不永久禁用
//	404 → 短冷却
//	429 → 短冷却
//	400/422 → 请求级问题 → 原文透传，**不罚号**（轮转纯属浪费额度）
//	5xx → 上游故障
func (c *Client) Classify(status int, body string) ErrKind {
	low := strings.ToLower(body)
	switch {
	case status == http.StatusPaymentRequired:
		return ErrHardCredit
	case status == http.StatusUnauthorized:
		return ErrSessionDead
	case status == http.StatusForbidden:
		return ErrAccountFault
	case status == http.StatusTooManyRequests:
		return ErrSoftRate
	case status == http.StatusNotFound:
		return ErrNotFound
	case status >= 500:
		return ErrServer
	case strings.Contains(low, "insufficient balance"), strings.Contains(low, "insufficient_balance"):
		return ErrHardCredit
	case status >= 400:
		return ErrPassthrough
	default:
		return ErrNone
	}
}

// KeyError 校验/查询失败的带状态错误。
type KeyError struct {
	Status int
	Body   string
}

func (e *KeyError) Error() string {
	return "上游返回 " + http.StatusText(e.Status) + "（" + itoa(e.Status) + "）：" + e.Body
}

var errEmptyKey = &KeyError{Status: 0, Body: "API key 不能为空"}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// localError 本地拒绝的 OpenAI 形状错误体。
func localError(code, msg string) []byte {
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "invalid_request_error",
		"code":    code,
	}})
	return raw
}

// modelIDList 可用模型名（拼进报错文案；llm 枚举只报个数，否则文案会撑爆客户端日志）。
func modelIDList() string {
	ids := make([]string, 0, len(specs))
	for _, s := range specs {
		ids = append(ids, s.ID)
	}
	return strings.Join(ids, " / ") + "，外加 35 个模型家族（档位走 reasoning_effort）"
}
