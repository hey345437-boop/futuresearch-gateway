// Package pool 号池：多把上游 key 的挑号、粘性路由、冷却与余额。
//
// 上游按**任务**计费、key 不轮换，所以这里比通用聚合器的池子简单：
// 没有 token 刷新、没有过期时间，只有「余额 + 冷却 + 粘性」。
package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hey345437-boop/futuresearch-gateway/internal/fsapi"
)

// Entry 一个号 + 它的运行时状态。
type Entry struct {
	Key *fsapi.Key

	BalanceCents int64     // 余额（美分）；-1 = 未知
	BalanceAt    time.Time // 上次查询时间
	Disabled     bool
	Reason       string    // 禁用原因
	CooldownTil  time.Time // 冷却到什么时候
	CoolKind     string    // "hard" | "soft"
	ErrCount     int
	LastOK       time.Time
	Requests     int64
}

// CoolUntilStr 冷却结束时间（面板展示；不在冷却时为空串）。
func (e *Entry) CoolUntilStr() string {
	if !e.Cooling() {
		return ""
	}
	return e.CooldownTil.Format("01-02 15:04")
}

// Healthy 是否可用（未禁用、不在冷却中）。
func (e *Entry) Healthy() bool { return !e.Disabled && time.Now().After(e.CooldownTil) }

// Cooling 是否在冷却中。
func (e *Entry) Cooling() bool { return time.Now().Before(e.CooldownTil) }

// Pool 号池。
type Pool struct {
	mu      sync.Mutex
	entries map[string]*Entry
	order   []string // 稳定顺序（加入顺序）
	file    string   // keys.json 路径

	// 粘性路由：优先复用上次成功的号，直到连续成功 maxReqs 次或出错。
	stickyUID string
	stickyN   int
	maxReqs   int
}

// New 建池并从 file 读号（不存在则空池）。
func New(file string) *Pool {
	p := &Pool{entries: map[string]*Entry{}, file: file, maxReqs: 50}
	p.load()
	return p
}

// SetMaxReqs 粘性路由的连续成功上限。
func (p *Pool) SetMaxReqs(n int) {
	if n <= 0 {
		n = 50
	}
	p.mu.Lock()
	p.maxReqs = n
	p.mu.Unlock()
}

// diskKey 磁盘形态：凭据 + 余额快照。
//
// 余额一起落盘，是为了**重启后面板不至于全显示 $0** —— 内存里的余额一重启就没了，
// 而余额是面板上最常看的一列。
type diskKey struct {
	*fsapi.Key
	BalanceCents int64     `json:"balance_cents"`
	BalanceAt    time.Time `json:"balance_at"`
	Disabled     bool      `json:"disabled,omitempty"`
	Reason       string    `json:"reason,omitempty"`
}

type keysFile struct {
	Keys []*diskKey `json:"keys"`
}

func (p *Pool) load() {
	raw, err := os.ReadFile(p.file)
	if err != nil {
		return
	}
	var f keysFile
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	for _, k := range f.Keys {
		if k == nil || k.Key == nil || k.Key.Key == "" {
			continue
		}
		e := p.addLocked(k.Key)
		e.BalanceCents = k.BalanceCents
		e.BalanceAt = k.BalanceAt
		e.Disabled = k.Disabled
		e.Reason = k.Reason
	}
}

func (p *Pool) saveLocked() {
	keys := make([]*diskKey, 0, len(p.entries))
	for _, uid := range p.order {
		if e := p.entries[uid]; e != nil {
			keys = append(keys, &diskKey{
				Key: e.Key, BalanceCents: e.BalanceCents, BalanceAt: e.BalanceAt,
				Disabled: e.Disabled, Reason: e.Reason,
			})
		}
	}
	raw, err := json.MarshalIndent(keysFile{Keys: keys}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.file), 0o755); err != nil && filepath.Dir(p.file) != "." {
		return
	}
	tmp := p.file + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, p.file)
	}
}

func (p *Pool) addLocked(k *fsapi.Key) *Entry {
	uid := k.UID
	if uid == "" {
		uid = k.Key // 没 uid 就用 key 当标识（极端情况）
	}
	if e, ok := p.entries[uid]; ok {
		e.Key = k
		return e
	}
	e := &Entry{Key: k, BalanceCents: -1}
	p.entries[uid] = e
	p.order = append(p.order, uid)
	return e
}

// Add 加一个号（已存在则覆盖凭据并清掉禁用状态）。
func (p *Pool) Add(k *fsapi.Key) error {
	if k == nil || k.Key == "" {
		return fmt.Errorf("key 不能为空")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.addLocked(k)
	e.Disabled = false
	e.Reason = ""
	e.CooldownTil = time.Time{}
	e.ErrCount = 0
	p.saveLocked()
	return nil
}

// Remove 删一个号。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, uid)
	for i, u := range p.order {
		if u == uid {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	if p.stickyUID == uid {
		p.stickyUID, p.stickyN = "", 0
	}
	p.saveLocked()
}

// SetDisabled 停用/启用。
func (p *Pool) SetDisabled(uid string, disabled bool, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[uid]; e != nil {
		e.Disabled = disabled
		e.Reason = reason
		if disabled && p.stickyUID == uid {
			p.stickyUID, p.stickyN = "", 0
		}
		p.saveLocked()
	}
}

// SetBalance 更新余额（美分）并落盘。
func (p *Pool) SetBalance(uid string, cents int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[uid]; e != nil {
		e.BalanceCents = cents
		e.BalanceAt = time.Now()
		p.saveLocked()
	}
}

// UIDOf 反查一个 key 的 uid（server 侧拿到 Entry 后用）。
func (p *Pool) UIDOf(e *Entry) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uid, x := range p.entries {
		if x == e {
			return uid
		}
	}
	return ""
}

// Pick 挑一个号：粘性优先，否则按余额降序（未知余额排最后）。
func (p *Pool) Pick(exclude map[string]bool) *Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 粘性
	if p.stickyUID != "" && p.stickyN < p.maxReqs && !exclude[p.stickyUID] {
		if e := p.entries[p.stickyUID]; e != nil && e.Healthy() {
			return e
		}
	}
	cands := make([]*Entry, 0, len(p.entries))
	for uid, e := range p.entries {
		if e.Healthy() && !exclude[uid] {
			cands = append(cands, e)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i].BalanceCents, cands[j].BalanceCents
		if a < 0 {
			a = -1
		}
		if b < 0 {
			b = -1
		}
		return a > b
	})
	e := cands[0]
	p.stickyUID, p.stickyN = p.uidLocked(e), 0
	return e
}

func (p *Pool) uidLocked(e *Entry) string {
	for uid, x := range p.entries {
		if x == e {
			return uid
		}
	}
	return ""
}

// NoteSuccess 记一次成功（推进粘性计数）。
func (p *Pool) NoteSuccess(e *Entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	uid := p.uidLocked(e)
	if uid == "" {
		return
	}
	e.ErrCount = 0
	e.LastOK = time.Now()
	e.Requests++
	if p.stickyUID == uid {
		p.stickyN++
	} else {
		p.stickyUID, p.stickyN = uid, 1
	}
}

// Cooldown 记一次惩罚。
func (p *Pool) Cooldown(uid string, kind fsapi.ErrKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[uid]
	if e == nil {
		return
	}
	e.CooldownTil = time.Now().Add(d)
	e.CoolKind = kind.String()
	e.Reason = reason
	if p.stickyUID == uid {
		p.stickyUID, p.stickyN = "", 0
	}
}

// ClearPenalty 清掉冷却/计数（面板「启用」时用）。
func (p *Pool) ClearPenalty(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[uid]; e != nil {
		e.CooldownTil = time.Time{}
		e.CoolKind = ""
		e.ErrCount = 0
		e.Disabled = false
		e.Reason = ""
	}
}

// List 全部号（面板用），按加入顺序。
func (p *Pool) List() []*Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Entry, 0, len(p.order))
	for _, uid := range p.order {
		if e := p.entries[uid]; e != nil {
			out = append(out, e)
		}
	}
	return out
}

// Get 按 uid 取。
func (p *Pool) Get(uid string) *Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries[uid]
}

// Len 号数。
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// TotalBalanceCents 可用余额合计（未知的不计）。
func (p *Pool) TotalBalanceCents() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var sum int64
	for _, e := range p.entries {
		if e.BalanceCents > 0 {
			sum += e.BalanceCents
		}
	}
	return sum
}
