// types.go 这个包自己的最小类型 —— 不依赖任何宿主项目。
package fsapi

import "time"

// ModelInfo 一个可调用模型的元信息（喂给 /v1/models 与面板）。
type ModelInfo struct {
	ID   string
	Name string
	// ContextWindow / MaxTokens 恒为 0 = 未知：上游不返回这个口径，
	// 编一个数字会让客户端按错误容量截断请求。
	SupportsReasoning bool
	SupportsTools     bool
	SupportsImages    bool
}

// Key 一个上游账号（就是一把平台 API key）。
//
// 上游的 key 不轮换、没有 refresh、没有过期 —— 所以这里没有 ExpiresAt/RefreshToken
// 那套东西，比通用聚合器里的 Auth 简单得多。
type Key struct {
	Key     string    `json:"key"`   // sk-cho-…
	UID     string    `json:"uid"`   // 上游 user id（面板标识用）
	Email   string    `json:"email"` // 面板展示名
	AddedAt time.Time `json:"added_at"`
}

// ErrKind 上游错误分类 —— 驱动号池的冷却状态机。
type ErrKind int

const (
	ErrNone         ErrKind = iota // 成功
	ErrHardCredit                  // 402 余额不足 → 长冷却（换号有效）
	ErrSoftRate                    // 429 限流 → 短冷却
	ErrSessionDead                 // 401 key 失效 → 禁用（要人工重加）
	ErrAccountFault                // 403 账号级拒绝 → 短冷却轮换，不永久禁用
	ErrNotFound                    // 404 → 短冷却
	ErrServer                      // 5xx
	ErrPassthrough                 // 400/422 请求级问题 → 原文透传，**不罚号**
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrAccountFault:
		return "account_fault"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrPassthrough:
		return "passthrough"
	default:
		return "none"
	}
}
