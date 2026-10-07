// Package server OpenAI 兼容端点 + 管理 API + 面板 + MCP 挂载。
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hey345437-boop/futuresearch-gateway/internal/config"
	"github.com/hey345437-boop/futuresearch-gateway/internal/fsapi"
	"github.com/hey345437-boop/futuresearch-gateway/internal/pool"
	"github.com/hey345437-boop/futuresearch-gateway/internal/tenant"
)

// BuildStamp 面板构建戳（编译时注入；没注入就用进程启动时间兜底）——
// 用来肉眼确认「浏览器打开的到底是不是刚部署的那一版」。
var BuildStamp = time.Now().Format("0102-1504")

// Server 网关。
type Server struct {
	cfg     *config.Config
	pool    *pool.Pool
	up      *fsapi.Client
	panel   http.Handler
	tenants *tenant.Store

	mcpHandler http.Handler            // 可空（默认挂在 cfg.MCP.Path）
	mcpExtra   map[string]http.Handler // 额外的 MCP 端点（如 /mcp/local）

	mu       sync.Mutex
	sessions map[string]time.Time // 面板会话 token（SHA-256）→ 过期时间
	logs     []LogLine
	usage    []UsageLine
	started  time.Time
	requests int64
	tasks    *taskRegistry

	cfgPathOverride string
}

// LogLine 面板日志。
type LogLine struct {
	Time string `json:"time"`
	Text string `json:"text"`
}

// UsageLine 一次调用（面板用量列表）。
type UsageLine struct {
	Time     string `json:"time"`
	Model    string `json:"model"`
	Account  string `json:"account"`
	Duration int64  `json:"duration_ms"`
	OK       bool   `json:"ok"`
	Note     string `json:"note"`
	Tokens   int64  `json:"tokens"`
}

// Options 构造参数。
type Options struct {
	Config  *config.Config
	Pool    *pool.Pool
	Up      *fsapi.Client
	Panel   http.Handler
	MCP     http.Handler
	Tenants *tenant.Store
}

// New 建服务。
func New(o Options) *Server {
	s := &Server{
		cfg: o.Config, pool: o.Pool, up: o.Up, panel: o.Panel,
		mcpHandler: o.MCP, tenants: o.Tenants,
		sessions: map[string]time.Time{},

		tasks:   newTaskRegistry(),
		started: time.Now(),
	}
	s.appendLog("网关启动，监听 " + s.cfg.Listen.Addr())
	return s
}

// Handler 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.apiAuth(s.handleChat))
	mux.HandleFunc("GET /v1/models", s.apiAuth(s.handleModels))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "accounts": s.pool.Len()})
	})

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/state", s.guard(s.handleState))
	mux.HandleFunc("POST /api/accounts/add", s.guard(s.handleAddAccount))
	mux.HandleFunc("POST /api/accounts/remove", s.guard(s.handleRemoveAccount))
	mux.HandleFunc("POST /api/accounts/disable", s.guard(s.handleDisableAccount))
	mux.HandleFunc("POST /api/accounts/refresh", s.guard(s.handleRefreshAccounts))
	mux.HandleFunc("POST /api/config", s.guard(s.handleConfig))
	mux.HandleFunc("GET /api/logs", s.guard(s.handleLogs))
	mux.HandleFunc("POST /api/setup/dsh", s.guard(s.handleSetupDsh))
	mux.HandleFunc("GET /api/preview", s.guard(s.handlePreviewList))
	mux.HandleFunc("POST /api/tenants/create", s.guard(s.handleTenantCreate))
	mux.HandleFunc("POST /api/tenants/update", s.guard(s.handleTenantUpdate))
	mux.HandleFunc("POST /api/tenants/remove", s.guard(s.handleTenantRemove))
	mux.HandleFunc("POST /api/tenants/reset", s.guard(s.handleTenantReset))

	// MCP 端点：**只认精确路径**。
	//
	// 为什么不能用 `mux.Handle("/mcp/", h)` 这种前缀注册：Go 的 ServeMux 里
	// `/mcp/` 会匹配 `/mcp/` 下的**一切** —— 于是本地工具没启用时，
	// 客户端 POST `/mcp/local` 会静默拿到**研究工具**（实测踩到过）。
	// 精确注册 + 最长匹配优先，才能保证「没挂的端点就是 404」。
	if s.mcpHandler != nil && s.cfg.MCP.Enabled {
		p := s.cfg.MCP.Path
		if p == "" {
			p = "/mcp"
		}
		mux.Handle(p, s.mcpHandler)
		mux.Handle(p+"/", exactPath(p+"/", s.mcpHandler))
	}
	for p, h := range s.mcpExtra {
		mux.Handle(p, h)
		mux.Handle(p+"/", exactPath(p+"/", h))
	}
	if s.cfg.Preview.Enabled {
		p := s.cfg.Preview.Path
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		mux.Handle(p, s.previewHandler())
	}
	if s.panel != nil {
		mux.Handle("/", s.panel)
	}
	// 访问日志：面板/接口的每一次请求都记一行（含 UA 与来源）。
	// 排查「浏览器到底有没有打到这个服务」时，这是唯一说得清的证据。
	return s.accessLog(mux)
}

// exactPath 只在路径**完全相等**时放行，否则 404。
// 用来兜住 `mux.Handle(p+"/", …)` 的前缀语义。
func exactPath(want string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != want {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// accessLog 只记面板与管理接口（/v1 的流量另有 usage 流水）。
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, "/api/") && p != "/" && !strings.HasSuffix(p, ".html") {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		ua := r.Header.Get("User-Agent")
		if len(ua) > 60 {
			ua = ua[:60]
		}
		s.appendLog(fmt.Sprintf("%s %s → %d  ref=%q ua=%q",
			r.Method, p, rec.status, r.Header.Get("Referer"), ua))
	})
}

// statusRecorder 记录响应码。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

// ---------------------------------------------------------------------------
// 鉴权
// ---------------------------------------------------------------------------

// ctxTenant 把「这次请求用的是哪把租户 key」带进 context（空串 = 根 key）。
type ctxTenantKey struct{}

// apiAuth OpenAI 侧的 Bearer 鉴权。
//
// 三档：
//   - 开了租户层：必须是一把有效的租户 key（根 api_key 仍然放行，当管理员用，不计量）
//   - 没开租户层但有 api_key：必须等于它
//   - 都没有：不校验（仅环回监听时才安全，启动时已经拦过非环回的情况）
func (s *Server) apiAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		rootKey := strings.TrimSpace(s.cfg.APIKey)

		if s.tenants != nil && s.tenants.Enabled() {
			if got == "" {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "缺少 API key（这个网关开了租户层，请用发给你的 key）")
				return
			}
			if rootKey != "" && got == rootKey {
				next(w, r) // 根 key：管理员通道，不限额度
				return
			}
			t := s.tenants.Lookup(got)
			if t == nil {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "无效的 API key")
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), ctxTenantKey{}, t)))
			return
		}

		if rootKey != "" && got != rootKey {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// tenantOf 取本次请求的租户（nil = 根 key / 未开租户层 ⇒ 不限额度）。
func tenantOf(r *http.Request) *tenant.Tenant {
	t, _ := r.Context().Value(ctxTenantKey{}).(*tenant.Tenant)
	return t
}

// guard 面板鉴权（admin_password 为空 = 不校验）。
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authEnabled() || s.sessionValid(r) {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "需要登录"})
	}
}

func (s *Server) authEnabled() bool { return strings.TrimSpace(s.cfg.AdminPass) != "" }

func (s *Server) sessionValid(r *http.Request) bool {
	c, err := r.Cookie("fsgw_session")
	if err != nil || c.Value == "" {
		return false
	}
	sum := sha256.Sum256([]byte(c.Value))
	key := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[key]
	if !ok || time.Now().After(exp) {
		delete(s.sessions, key)
		return false
	}
	s.sessions[key] = time.Now().Add(7 * 24 * time.Hour) // 滑动续期
	return true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !s.authEnabled() {
		writeJSON(w, 200, map[string]any{"ok": true, "auth": false})
		return
	}
	if req.Password != s.cfg.AdminPass {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "密码不正确"})
		return
	}
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	s.mu.Lock()
	s.sessions[hex.EncodeToString(sum[:])] = time.Now().Add(7 * 24 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: "fsgw_session", Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: time.Now().Add(7 * 24 * time.Hour),
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("fsgw_session"); err == nil {
		sum := sha256.Sum256([]byte(c.Value))
		s.mu.Lock()
		delete(s.sessions, hex.EncodeToString(sum[:]))
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "fsgw_session", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// OpenAI 端点
// ---------------------------------------------------------------------------

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := fsapi.StaticModels()
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "futuresearch",
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "读取请求体失败")
		return
	}
	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON")
		return
	}
	if s.pool.Len() == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_account",
			"还没有绑定任何账号：打开面板 http://"+s.cfg.Listen.Addr()+" 添加一个 FutureSearch API key")
		return
	}

	// 租户闸门：档位上限 → 并发 → 额度预检。
	// 顺序有意为之：先报「不允许」再报「额度不够」，用户知道该找谁。
	tnt := tenantOf(r)
	if tnt != nil {
		if !tnt.Allows(peek.Model) {
			writeOpenAIError(w, http.StatusForbidden, "effort_not_allowed",
				"这个 key 不允许用该档位："+peek.Model+"（上限 "+tnt.MaxEffort+"）")
			return
		}
		release, aerr := s.tenants.Acquire(tnt.Key, tenant.EstimateCents(peek.Model))
		if aerr != nil {
			writeOpenAIError(w, http.StatusPaymentRequired, "quota_exceeded", aerr.Error())
			return
		}
		defer release()
		defer s.tenants.Charge(tnt.Key, tenant.EstimateCents(peek.Model))
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < s.cfg.Pool.MaxRotate; i++ {
		e := s.pool.Pick(tried)
		if e == nil {
			break
		}
		uid := s.pool.UIDOf(e)
		tried[uid] = true
		t0 := time.Now()

		rc, status, raw, terr := s.up.ChatStream(e.Key, body)
		if terr != nil {
			lastErr = terr
			s.pool.Cooldown(uid, fsapi.ErrServer, s.cfg.SoftCooldownDur, "传输层错误: "+terr.Error())
			s.appendLog(fmt.Sprintf("传输层错误 uid=%s err=%v（换号重试）", short(uid), terr))
			continue // 传输层错误：同一请求内换号
		}
		if status >= 400 {
			kind := s.up.Classify(status, string(raw))
			s.penalize(uid, kind, string(raw))
			// 请求级问题（400/422）：换号没用，原文透传
			s.recordUsage(peek.Model, uid, time.Since(t0), false, kind.String(), 0)
			transparentError(w, status, raw)
			return
		}

		if !peek.Stream {
			resp, cerr := fsapi.Collect(rc, peek.Model)
			if cerr != nil {
				lastErr = cerr
				s.pool.Cooldown(uid, fsapi.ErrServer, s.cfg.SoftCooldownDur, "聚合失败: "+cerr.Error())
				s.recordUsage(peek.Model, uid, time.Since(t0), false, cerr.Error(), 0)
				s.appendLog(fmt.Sprintf("聚合失败 uid=%s err=%v", short(uid), cerr))
				continue
			}
			s.pool.NoteSuccess(e)
			var toks int64
			if u, ok := resp["usage"].(map[string]any); ok {
				if v, ok := u["total_tokens"].(float64); ok {
					toks = int64(v)
				}
			}
			s.recordUsage(peek.Model, uid, time.Since(t0), true, "", toks)
			writeJSON(w, 200, resp)
			go s.refreshBalance(uid)
			return
		}

		// 流式
		var toks int64
		serr := fsapi.Relay(w, rc, func(u map[string]any) {
			if v, ok := u["total_tokens"].(float64); ok {
				toks = int64(v)
			}
		})
		if serr != nil {
			lastErr = serr
			s.recordUsage(peek.Model, uid, time.Since(t0), false, serr.Error(), toks)
			s.appendLog(fmt.Sprintf("流中断 uid=%s err=%v", short(uid), serr))
			return // 已经写过响应头，不能再换号
		}
		s.pool.NoteSuccess(e)
		s.recordUsage(peek.Model, uid, time.Since(t0), true, "", toks)
		go s.refreshBalance(uid)
		return
	}

	msg := "没有可用账号（全部在冷却或已禁用）"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// penalize 按错误类型惩罚账号。
func (s *Server) penalize(uid string, kind fsapi.ErrKind, body string) {
	switch kind {
	case fsapi.ErrHardCredit:
		s.pool.Cooldown(uid, kind, s.cfg.HardCooldownDur, "余额不足")
	case fsapi.ErrSoftRate, fsapi.ErrNotFound, fsapi.ErrAccountFault:
		s.pool.Cooldown(uid, kind, s.cfg.SoftCooldownDur, snippet(body))
	case fsapi.ErrSessionDead:
		s.pool.SetDisabled(uid, true, "key 失效，请重新添加")
	case fsapi.ErrServer:
		s.pool.Cooldown(uid, kind, s.cfg.SoftCooldownDur, "上游 5xx")
	}
}

// ---------------------------------------------------------------------------
// 管理 API
// ---------------------------------------------------------------------------

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	type acct struct {
		UID          string `json:"uid"`
		Email        string `json:"email"`
		KeyMasked    string `json:"key_masked"`
		BalanceCents int64  `json:"balance_cents"`
		BalanceAt    string `json:"balance_at"`
		Disabled     bool   `json:"disabled"`
		Reason       string `json:"reason"`
		Cooling      bool   `json:"cooling"`
		CoolUntil    string `json:"cool_until"`
		CoolKind     string `json:"cool_kind"`
		Requests     int64  `json:"requests"`
		LastOK       string `json:"last_ok"`
	}
	out := make([]acct, 0)
	for _, e := range s.pool.List() {
		uid := s.pool.UIDOf(e)
		a := acct{
			UID: uid, Email: e.Key.Email, KeyMasked: maskKey(e.Key.Key),
			BalanceCents: e.BalanceCents, Disabled: e.Disabled, Reason: e.Reason,
			Cooling: e.Cooling(), CoolKind: e.CoolKind, Requests: e.Requests,
		}
		if !e.BalanceAt.IsZero() {
			a.BalanceAt = e.BalanceAt.Format("01-02 15:04")
		}
		if e.Cooling() {
			a.CoolUntil = e.CoolUntilStr()
		}
		if !e.LastOK.IsZero() {
			a.LastOK = e.LastOK.Format("01-02 15:04")
		}
		out = append(out, a)
	}

	models := make([]map[string]any, 0)
	for _, m := range fsapi.StaticModels() {
		models = append(models, map[string]any{"id": m.ID, "name": m.Name})
	}

	s.mu.Lock()
	usage := append([]UsageLine{}, s.usage...)
	s.mu.Unlock()
	// 倒序（最近在前）
	for i, j := 0, len(usage)-1; i < j; i, j = i+1, j-1 {
		usage[i], usage[j] = usage[j], usage[i]
	}
	if len(usage) > 100 {
		usage = usage[:100]
	}

	writeJSON(w, 200, map[string]any{
		"version":        "1.0.0",
		"build":          BuildStamp,
		"uptime":         time.Since(s.started).Round(time.Second).String(),
		"listen":         s.cfg.Listen.Addr(),
		"api_key_set":    s.cfg.APIKey != "",
		"admin_pass_set": s.authEnabled(),
		"auth_required":  s.authEnabled(),
		"accounts":       out,
		"total_cents":    s.pool.TotalBalanceCents(),
		"models":         models,
		"prompts":        s.up.Prompts(),
		"usage":          usage,
		"requests":       s.requests,
		"mcp":            map[string]any{"enabled": s.cfg.MCP.Enabled, "path": s.cfg.MCP.Path},
		"preview":        map[string]any{"enabled": s.cfg.Preview.Enabled, "path": s.cfg.Preview.Path, "dir": s.cfg.Preview.Dir},
		"localfs": map[string]any{
			"enabled": s.cfg.LocalFS.Enabled, "root": s.cfg.LocalFS.Root,
			"allow_write": s.cfg.LocalFS.AllowWrite, "allow_exec": s.cfg.LocalFS.AllowExec,
			"path": s.cfg.LocalFS.Path,
		},
		"upstream_proxy":  s.cfg.Upstream.Proxy,
		"hard_cooldown":   s.cfg.Pool.HardCooldown,
		"soft_cooldown":   s.cfg.Pool.SoftCooldown,
		"tenants_enabled": s.tenants != nil && s.tenants.Enabled(),
		"tenants":         s.tenantViews(),
	})
}

func (s *Server) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败"})
		return
	}
	uid, email, err := s.up.ValidateKey(req.Key)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if err := s.pool.Add(&fsapi.Key{Key: strings.TrimSpace(req.Key), UID: uid, Email: email, AddedAt: time.Now()}); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	s.appendLog("添加账号 " + email + "（" + short(uid) + "）")
	go s.refreshBalance(uid)
	writeJSON(w, 200, map[string]any{"ok": true, "uid": uid, "email": email})
}

func (s *Server) handleRemoveAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.pool.Remove(req.UID)
	s.appendLog("删除账号 " + short(req.UID))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDisableAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID      string `json:"uid"`
		Disabled bool   `json:"disabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Disabled {
		s.pool.SetDisabled(req.UID, true, "手动停用")
	} else {
		s.pool.ClearPenalty(req.UID)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleRefreshAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ids := []string{}
	if req.UID != "" {
		ids = append(ids, req.UID)
	} else {
		for _, e := range s.pool.List() {
			ids = append(ids, s.pool.UIDOf(e))
		}
	}
	for _, uid := range ids {
		s.refreshBalance(uid)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "count": len(ids)})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompts      *map[string]string `json:"prompts"`
		APIKey       *string            `json:"api_key"`
		AdminPass    *string            `json:"admin_password"`
		HardCooldown *string            `json:"hard_cooldown"`
		SoftCooldown *string            `json:"soft_cooldown"`
		MCPEnabled   *bool              `json:"mcp_enabled"`
		PreviewOn    *bool              `json:"preview_enabled"`
		LocalFS      *struct {
			Enabled    *bool   `json:"enabled"`
			Root       *string `json:"root"`
			AllowWrite *bool   `json:"allow_write"`
			AllowExec  *bool   `json:"allow_exec"`
		} `json:"localfs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败"})
		return
	}
	if req.Prompts != nil {
		s.cfg.Prompts = *req.Prompts
		s.up.SetPrompts(*req.Prompts)
	}
	if req.APIKey != nil {
		s.cfg.APIKey = strings.TrimSpace(*req.APIKey)
	}
	if req.AdminPass != nil {
		s.cfg.AdminPass = *req.AdminPass
	}
	if req.HardCooldown != nil {
		s.cfg.Pool.HardCooldown = *req.HardCooldown
	}
	if req.SoftCooldown != nil {
		s.cfg.Pool.SoftCooldown = *req.SoftCooldown
	}
	if req.MCPEnabled != nil {
		s.cfg.MCP.Enabled = *req.MCPEnabled
	}
	if req.PreviewOn != nil {
		s.cfg.Preview.Enabled = *req.PreviewOn
	}
	if req.LocalFS != nil {
		if req.LocalFS.Enabled != nil {
			s.cfg.LocalFS.Enabled = *req.LocalFS.Enabled
		}
		if req.LocalFS.Root != nil {
			s.cfg.LocalFS.Root = strings.TrimSpace(*req.LocalFS.Root)
		}
		if req.LocalFS.AllowWrite != nil {
			s.cfg.LocalFS.AllowWrite = *req.LocalFS.AllowWrite
		}
		if req.LocalFS.AllowExec != nil {
			s.cfg.LocalFS.AllowExec = *req.LocalFS.AllowExec
		}
		if s.cfg.LocalFS.Enabled && s.cfg.LocalFS.Root == "" {
			writeJSON(w, 400, map[string]any{"error": "启用本地工具必须给 root 目录"})
			return
		}
	}
	s.cfg.Normalize()
	if err := config.Save(s.cfg, s.cfgPath()); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	s.appendLog("配置已更新")
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	logs := append([]LogLine{}, s.logs...)
	s.mu.Unlock()
	for i, j := 0, len(logs)-1; i < j; i, j = i+1, j-1 {
		logs[i], logs[j] = logs[j], logs[i]
	}
	writeJSON(w, 200, map[string]any{"logs": logs})
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// refreshBalance 刷一个号的余额（后台调用）。
func (s *Server) refreshBalance(uid string) {
	e := s.pool.Get(uid)
	if e == nil {
		return
	}
	dollars, err := s.up.Balance(e.Key.Key)
	if err != nil {
		return
	}
	s.pool.SetBalance(uid, int64(dollars*100+0.5))
}

func (s *Server) recordUsage(model, uid string, d time.Duration, ok bool, note string, toks int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	s.usage = append(s.usage, UsageLine{
		Time: time.Now().Format("15:04:05"), Model: model, Account: short(uid),
		Duration: d.Milliseconds(), OK: ok, Note: note, Tokens: toks,
	})
	if len(s.usage) > 500 {
		s.usage = s.usage[len(s.usage)-500:]
	}
}

func (s *Server) appendLog(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, LogLine{Time: time.Now().Format("01-02 15:04:05"), Text: text})
	if len(s.logs) > 500 {
		s.logs = s.logs[len(s.logs)-500:]
	}
	log.Printf("%s", text)
}

// cfgPath 配置文件路径（放在 data_dir 旁边）。
func (s *Server) cfgPath() string { return s.cfgPathOverride }

// AddMCPEndpoint 再挂一个 MCP 端点（如本地文件工具挂在 /mcp/local）。
// 分开挂是刻意的：客户端与人都该一眼看清哪些工具会碰本地磁盘。
func (s *Server) AddMCPEndpoint(path string, h http.Handler) {
	if s.mcpExtra == nil {
		s.mcpExtra = map[string]http.Handler{}
	}
	s.mcpExtra[path] = h
}

// SetConfigPath 由 main 注入实际路径。
func (s *Server) SetConfigPath(p string) { s.cfgPathOverride = p }

// Logf 让 main 也能往面板日志里写。
func (s *Server) Logf(format string, a ...any) { s.appendLog(fmt.Sprintf(format, a...)) }

// StartBalanceLoop 周期性刷余额（后台）。
func (s *Server) StartBalanceLoop(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, e := range s.pool.List() {
					s.refreshBalance(s.pool.UIDOf(e))
				}
			}
		}
	}()
}

// timeNowStr 时间戳（给备份文件名用）。
func timeNowStr() string { return time.Now().Format("20060102-150405") }

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func maskKey(k string) string {
	if len(k) <= 12 {
		return "***"
	}
	return k[:8] + "…" + k[len(k)-4:]
}

func snippet(b string) string {
	b = strings.TrimSpace(b)
	if len(b) > 200 {
		return b[:200]
	}
	return b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": msg, "type": "invalid_request_error", "code": code,
	}})
}

func transparentError(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if len(body) == 0 {
		_, _ = w.Write([]byte(`{"error":{"message":"上游返回空响应","type":"upstream_error"}}`))
		return
	}
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// 给 MCP 用的进程内调用（不绕 HTTP 自环）
// ---------------------------------------------------------------------------

// Complete 跑一次对话并拿回完整结果（MCP 工具用）。
// 走与 /v1/chat/completions 同一套挑号/轮换/惩罚逻辑，只是出口是字符串不是 SSE。
func (s *Server) Complete(model, prompt string) (string, string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
	})
	if s.pool.Len() == 0 {
		return "", "", fmt.Errorf("还没有绑定任何账号：打开面板添加一个 FutureSearch API key")
	}
	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < s.cfg.Pool.MaxRotate; i++ {
		e := s.pool.Pick(tried)
		if e == nil {
			break
		}
		uid := s.pool.UIDOf(e)
		tried[uid] = true
		t0 := time.Now()
		rc, status, raw, terr := s.up.ChatStream(e.Key, body)
		if terr != nil {
			lastErr = terr
			s.pool.Cooldown(uid, fsapi.ErrServer, s.cfg.SoftCooldownDur, "传输层错误")
			continue
		}
		if status >= 400 {
			kind := s.up.Classify(status, string(raw))
			s.penalize(uid, kind, string(raw))
			s.recordUsage(model, uid, time.Since(t0), false, kind.String(), 0)
			return "", "", fmt.Errorf("上游返回 %d：%s", status, snippet(string(raw)))
		}
		resp, cerr := fsapi.Collect(rc, model)
		if cerr != nil {
			lastErr = cerr
			s.recordUsage(model, uid, time.Since(t0), false, cerr.Error(), 0)
			continue
		}
		s.pool.NoteSuccess(e)
		var toks int64
		if u, ok := resp["usage"].(map[string]any); ok {
			if v, ok := u["total_tokens"].(float64); ok {
				toks = int64(v)
			}
		}
		s.recordUsage(model, uid, time.Since(t0), true, "", toks)
		go s.refreshBalance(uid)
		msg, _ := resp["choices"].([]any)
		if len(msg) == 0 {
			return "", "", fmt.Errorf("上游没有返回内容")
		}
		m, _ := msg[0].(map[string]any)["message"].(map[string]any)
		answer, _ := m["content"].(string)
		reasoning, _ := m["reasoning_content"].(string)
		return answer, reasoning, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用账号（全部在冷却或已禁用）")
	}
	return "", "", lastErr
}

// Models 返回模型 id 列表（MCP 的 models 工具用）。
func (s *Server) Models() []string {
	ms := fsapi.StaticModels()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// PromptInjected 该模型当前会带上哪段前置指令（面板/调试用）。
func (s *Server) PromptInjected(model string) string { return s.up.PromptFor(model) }

// ---------------------------------------------------------------------------
// 租户（发 key + 额度）
// ---------------------------------------------------------------------------

func (s *Server) tenantViews() []map[string]any {
	if s.tenants == nil {
		return nil
	}
	out := make([]map[string]any, 0)
	for _, t := range s.tenants.List() {
		out = append(out, map[string]any{
			"key": t.Key, "name": t.Name,
			"quota_cents": t.QuotaCents, "used_cents": t.UsedCents, "remaining_cents": t.RemainingCents(),
			"max_effort": t.MaxEffort, "concurrency": t.Concurrency,
			"enabled": t.Enabled, "note": t.Note, "requests": t.Requests,
			"running":    t.Running(),
			"created_at": t.CreatedAt.Format("01-02 15:04"),
			"last_used": func() string {
				if t.LastUsed.IsZero() {
					return ""
				}
				return t.LastUsed.Format("01-02 15:04")
			}(),
		})
	}
	return out
}

func (s *Server) handleTenantCreate(w http.ResponseWriter, r *http.Request) {
	if s.tenants == nil {
		writeJSON(w, 400, map[string]any{"error": "租户层未启用"})
		return
	}
	var req struct {
		Name        string `json:"name"`
		QuotaCents  int64  `json:"quota_cents"`
		MaxEffort   string `json:"max_effort"`
		Concurrency int    `json:"concurrency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败"})
		return
	}
	t, err := s.tenants.Create(req.Name, req.QuotaCents, req.MaxEffort, req.Concurrency)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	s.tenants.SetEnabled(true)
	s.appendLog("新建租户 " + t.Name)
	writeJSON(w, 200, map[string]any{"ok": true, "key": t.Key, "name": t.Name})
}

func (s *Server) handleTenantUpdate(w http.ResponseWriter, r *http.Request) {
	if s.tenants == nil {
		writeJSON(w, 400, map[string]any{"error": "租户层未启用"})
		return
	}
	var req struct {
		Key         string  `json:"key"`
		QuotaCents  *int64  `json:"quota_cents"`
		MaxEffort   *string `json:"max_effort"`
		Concurrency *int    `json:"concurrency"`
		Enabled     *bool   `json:"enabled"`
		Note        *string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败"})
		return
	}
	if err := s.tenants.Update(req.Key, req.QuotaCents, req.MaxEffort, req.Concurrency, req.Enabled, req.Note); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleTenantRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.tenants.Remove(req.Key)
	s.appendLog("删除租户 " + short(req.Key)) // 只留前缀；日志里不放完整 key
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleTenantReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.tenants.Reset(req.Key)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
