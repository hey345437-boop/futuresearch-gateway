// Package tenant 租户层：给别人的 API key + 额度（美元口径）+ 档位上限 + 并发上限。
//
// 为什么额度按**美元**：FutureSearch 按任务计费（$0.2~5/次），不返回 token 用量。
// 唯一准确的口径是**账户余额差**（任务前后各读一次 /billing）。
// 按 token 记账的通用网关在这里会算出「0 token = 免费」，额度闸形同虚设。
//
// 两道闸：
//   - 预检：按档位**粗估**单价，`已用 + 预估 > 额度` 直接 402（不然余额只剩 $0.05 也能发起 $5 的任务）
//   - 归集：任务结束后读真实余额差，把**实测**成本记到该租户
package tenant

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Tenant 一把发给别人的 key。
type Tenant struct {
	Key         string    `json:"key"`         // fsg_…
	Name        string    `json:"name"`        // 展示名
	QuotaCents  int64     `json:"quota_cents"` // 额度（美分）；0 = 不限
	UsedCents   int64     `json:"used_cents"`  // 已用（美分，实测归集）
	MaxEffort   string    `json:"max_effort"`  // "low" | "medium" | "high"（默认 medium）
	Concurrency int       `json:"concurrency"` // 同时跑几个任务（默认 2）
	Enabled     bool      `json:"enabled"`
	Note        string    `json:"note"`
	CreatedAt   time.Time `json:"created_at"`
	Requests    int64     `json:"requests"`
	LastUsed    time.Time `json:"last_used"`

	// 运行时（不落盘）
	running int
}

// Running 当前在跑的任务数（面板展示）。
func (t *Tenant) Running() int { return t.running }

// RemainingCents 剩余额度（-1 = 不限）。
func (t *Tenant) RemainingCents() int64 {
	if t.QuotaCents <= 0 {
		return -1
	}
	r := t.QuotaCents - t.UsedCents
	if r < 0 {
		return 0
	}
	return r
}

// Store 租户表（一个 JSON 文件 + 内存）。
type Store struct {
	mu      sync.Mutex
	byKey   map[string]*Tenant
	order   []string
	file    string
	enabled bool
}

// New 建表并从磁盘读。
func New(file string) *Store {
	s := &Store{byKey: map[string]*Tenant{}, file: file}
	s.load()
	return s
}

// SetEnabled 总开关。
func (s *Store) SetEnabled(on bool) {
	s.mu.Lock()
	s.enabled = on
	s.mu.Unlock()
}

// Enabled 总开关状态。
func (s *Store) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

func (s *Store) load() {
	raw, err := os.ReadFile(s.file)
	if err != nil {
		return
	}
	var f struct {
		Enabled bool      `json:"enabled"`
		Tenants []*Tenant `json:"tenants"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	s.enabled = f.Enabled
	for _, t := range f.Tenants {
		if t == nil || t.Key == "" {
			continue
		}
		s.byKey[t.Key] = t
		s.order = append(s.order, t.Key)
	}
}

func (s *Store) saveLocked() {
	list := make([]*Tenant, 0, len(s.order))
	for _, k := range s.order {
		if t := s.byKey[k]; t != nil {
			cp := *t
			cp.running = 0
			list = append(list, &cp)
		}
	}
	raw, _ := json.MarshalIndent(map[string]any{"enabled": s.enabled, "tenants": list}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.file), 0o755); err != nil && filepath.Dir(s.file) != "." {
		return
	}
	tmp := s.file + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, s.file)
	}
}

// Create 新建一把租户 key。
func (s *Store) Create(name string, quotaCents int64, maxEffort string, concurrency int) (*Tenant, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("名字不能为空")
	}
	if maxEffort == "" {
		maxEffort = "medium"
	}
	switch maxEffort {
	case "low", "medium", "high":
	default:
		return nil, fmt.Errorf("档位上限只能是 low/medium/high")
	}
	if concurrency <= 0 {
		concurrency = 2
	}
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	t := &Tenant{
		Key: "fsg_" + hex.EncodeToString(buf), Name: name,
		QuotaCents: quotaCents, MaxEffort: maxEffort, Concurrency: concurrency,
		Enabled: true, CreatedAt: time.Now(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKey[t.Key] = t
	s.order = append(s.order, t.Key)
	s.saveLocked()
	return t, nil
}

// Update 改额度/上限/备注/启停。
func (s *Store) Update(key string, quotaCents *int64, maxEffort *string, concurrency *int, enabled *bool, note *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byKey[key]
	if t == nil {
		return fmt.Errorf("租户不存在")
	}
	if quotaCents != nil {
		t.QuotaCents = *quotaCents
	}
	if maxEffort != nil {
		switch *maxEffort {
		case "low", "medium", "high":
			t.MaxEffort = *maxEffort
		default:
			return fmt.Errorf("档位上限只能是 low/medium/high")
		}
	}
	if concurrency != nil && *concurrency > 0 {
		t.Concurrency = *concurrency
	}
	if enabled != nil {
		t.Enabled = *enabled
	}
	if note != nil {
		t.Note = *note
	}
	s.saveLocked()
	return nil
}

// Remove 删。
func (s *Store) Remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byKey, key)
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.saveLocked()
}

// Reset 把已用量清零（发新一轮额度）。
func (s *Store) Reset(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.byKey[key]; t != nil {
		t.UsedCents = 0
		s.saveLocked()
	}
}

// Lookup 按 key 查。
func (s *Store) Lookup(key string) *Tenant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byKey[key]
}

// List 全部（按创建顺序）。
func (s *Store) List() []*Tenant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Tenant, 0, len(s.order))
	for _, k := range s.order {
		if t := s.byKey[k]; t != nil {
			out = append(out, t)
		}
	}
	return out
}

// Acquire 并发闸 + 额度预检。返回 (释放函数, 错误)。
//
// estimateCents 是这次请求的**粗估**单价（见 EstimateCents）——
// 不预检的话，余额只剩 $0.05 的租户照样能发起一次 $5 的任务。
func (s *Store) Acquire(key string, estimateCents int64) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byKey[key]
	if t == nil {
		return nil, fmt.Errorf("无效的 API key")
	}
	if !t.Enabled {
		return nil, fmt.Errorf("该 key 已被停用")
	}
	if t.running >= t.Concurrency {
		return nil, fmt.Errorf("并发超限（该 key 同时最多 %d 个任务）", t.Concurrency)
	}
	if t.QuotaCents > 0 && t.UsedCents+estimateCents > t.QuotaCents {
		return nil, fmt.Errorf("额度不足：剩余 %s，本次预估需要 %s",
			money(t.RemainingCents()), money(estimateCents))
	}
	t.running++
	return func() {
		s.mu.Lock()
		if t.running > 0 {
			t.running--
		}
		s.mu.Unlock()
	}, nil
}

// Charge 记一次实测成本。
func (s *Store) Charge(key string, cents int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byKey[key]
	if t == nil {
		return
	}
	if cents > 0 {
		t.UsedCents += cents
	}
	t.Requests++
	t.LastUsed = time.Now()
	s.saveLocked()
}

// EstimateCents 按模型**粗估**一次调用的成本（美分）。只用于额度预检，不是账单口径。
//
// 实测参考：agent-low ≈ 20~40 美分、medium ≈ 60~80、high 数美元；模型家族随档位浮动。
func EstimateCents(model string) int64 {
	switch model {
	case "agent-low":
		return 40
	case "agent-medium":
		return 80
	case "agent-high":
		return 400
	case "multi-agent-low":
		return 80
	case "multi-agent-medium":
		return 150
	case "multi-agent-high":
		return 600
	case "forecast":
		return 200
	}
	switch {
	case strings.HasSuffix(model, "-max"), strings.HasSuffix(model, "-xhigh"):
		return 400
	case strings.HasSuffix(model, "-high"):
		return 200
	case strings.HasSuffix(model, "-medium"), strings.HasSuffix(model, "-low"),
		strings.HasSuffix(model, "-minimal"), strings.HasSuffix(model, "-nt"):
		return 80
	}
	return 120 // 家族名（默认档）
}

// EffortOf 取模型对应的「档位」，用来跟 MaxEffort 比。
func EffortOf(model string) string {
	switch model {
	case "agent-low", "multi-agent-low":
		return "low"
	case "agent-medium", "multi-agent-medium", "forecast":
		return "medium"
	case "agent-high", "multi-agent-high":
		return "high"
	}
	switch {
	case strings.HasSuffix(model, "-max"), strings.HasSuffix(model, "-xhigh"), strings.HasSuffix(model, "-high"):
		return "high"
	case strings.HasSuffix(model, "-low"), strings.HasSuffix(model, "-minimal"), strings.HasSuffix(model, "-nt"):
		return "low"
	}
	return "medium"
}

// Allows 该租户的档位上限是否允许这个模型。
func (t *Tenant) Allows(model string) bool {
	rank := map[string]int{"low": 1, "medium": 2, "high": 3}
	return rank[EffortOf(model)] <= rank[t.MaxEffort]
}

func money(cents int64) string {
	return fmt.Sprintf("$%.2f", float64(cents)/100)
}
