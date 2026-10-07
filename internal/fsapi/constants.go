// Package futuresearch 接入 FutureSearch（futuresearch.ai）的公开 v0 API。
//
// 渠道特性（2026-10-06 实测，见 docs/futuresearch渠道接入备忘.md）：
//
//   - 凭据是**平台 API key**（`sk-cho-…`），由面板粘贴后落盘 <data_dir>/keys.json；
//     上游无 refresh 端点、key 不轮换 ⇒ RefreshToken 空实现、ExpiresAt 置远期。
//   - 上游**不是 chat completions**，而是异步任务式研究 API：
//     POST /api/v0/operations/{agent-map|multi-agent|forecast} → {task_id, session_id, status}
//     GET  /api/v0/tasks/{id}/status  轮询（pending|running|completed|failed|revoked）
//     GET  /api/v0/tasks/{id}/result  取结果（data: [{answer, research?}]）
//     故本渠道把「OpenAI chat 请求」翻译成一个任务，再把结果**合成 OpenAI SSE** 回放。
//   - 单次 low 投入实测 ≈23s，medium/high 更久 ⇒ 轮询期间持续发 keepalive 空 delta 帧，
//     避免客户端空闲超时。**不伪造 usage**：上游不返回 token 计量，宁可让调用方
//     记「0 token + 1 次请求」，也不把估算值伪装成上游口径（R23b 的同一纪律）。
package fsapi

import (
	"time"
)

const (
	// DefaultBase 上游公开 v0 API 基址（OpenAPI: https://futuresearch.ai/api/v0/openapi.json）。
	DefaultBase = "https://futuresearch.ai/api/v0"

	// userAgent 出站 UA。上游未见校验，仅便于服务端排障。
	userAgent = "github.com/hey345437-boop/futuresearch-gateway/1.0"

	// requestTimeout 单次 HTTP 调用的整体上限（投任务 / 轮询 / 取结果）。
	// **只用于非流式 client**：对话流的等待期由本包自己的轮询循环承担，
	// 不能套 http.Client.Timeout（长任务会被整请求超时从中间掐断，R35）。
	requestTimeout = 120 * time.Second

	// pollInterval 任务状态轮询间隔。上游 low 档实测 23s 完成，
	// 2s 轮询足以让首帧结果尽快回放，又不至于把状态接口打爆。
	pollInterval = 1 * time.Second

	// maxStatusFailures 状态查询连续失败多少次才放弃一个任务。
	// 上游偶发 5xx 时任务通常还在跑，判太早等于白白丢掉一次已经付过钱的研究。
	maxStatusFailures = 20

	// progressEvery 进度事件的最小重发间隔。必须**明显小于**客户端的流空闲超时
	// （dsh/pi-ai 是 300s），否则任务跑长一点就会被判超时；45s 留了充足余量。
	progressEvery = 45 * time.Second

	// maxWait 单任务最长等待。high 档 + 多行任务可能十几分钟，
	// 超过即按超时失败（发 error 帧，不补 [DONE]）。
	maxWait = 30 * time.Minute

	// noExpiry 凭据远期过期时刻（2100-01-01）。必须 > 0：
	// 旧宿主项目的鉴权层对「零值过期时间」会反复走刷新路径 —— 这里干脆没有这个概念。
	noExpiry int64 = 4102444800

	// chunkRunes 回放正文时的分片长度（rune）。上游一次性给全文，
	// 切成小片再发，客户端才能边收边渲染（否则整段文本一次落地）。
	chunkRunes = 240
)
