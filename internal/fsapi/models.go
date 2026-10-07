// models.go FutureSearch 的静态模型目录。
//
// 上游**没有**「模型」这个概念，只有「操作（operation）× 投入档（effort）」。
// 这里把常用组合暴露成模型名，让 `/v1/models` 与客户端的模型选择器有东西可选：
//
//	<channel>/agent-<effort>        单 agent 研究（operations/agent-map）
//	<channel>/multi-agent-<effort>  多 agent 并行研究后综合（operations/multi-agent）
//	<channel>/forecast              二值预测（operations/forecast，forecast_type=binary）
//
// 未知模型名一律本地 400（不静默回落到默认档）——上游对未知参数是宽容的，
// 若本地不校验，用户会以为在用 high、实际扣的是别的档（R42 同族教训）。
package fsapi

import (
	"strings"
)

// opKind 上游操作类型。
type opKind string

const (
	opAgentMap   opKind = "agent-map"
	opMultiAgent opKind = "multi-agent"
	opForecast   opKind = "forecast"
)

// modelSpec 一个可调用的模型 = 操作 + 档位，**或** 操作 + 显式 llm。
//
//	预设档：Effort 非空、LLM 空 → 走 effort_level（平台自己挑模型）
//	真模型：LLM 非空、Effort 空 → 走 llm + iteration_budget=0（单次调用，见 llmenum.go）
//
// 两者互斥是上游的硬约束（effort_level 与 llm/iteration_budget/include_reasoning
// 不能同时给，否则 422）。
type modelSpec struct {
	ID     string // 模型名（不含渠道前缀）
	Name   string // 面板/客户端展示名
	Op     opKind
	Effort string // low|medium|high（预设档）
	LLM    string // 上游 llm 枚举原文（真模型档）
	// Base 非空表示「这是模型家族名」，档位要在请求时按 reasoning_effort 再定
	// （lookupSpec 只给了默认档，ChatStream 会覆盖 LLM/Name）。
	Base string
	Desc string
}

// specs 静态目录。顺序即展示顺序。
var specs = []modelSpec{
	{ID: "agent-low", Name: "Agent Fast", Op: opAgentMap, Effort: "low",
		Desc: "单 agent 快速研究（0 轮迭代，无出处）。实测 ≈23s。"},
	{ID: "agent-medium", Name: "Agent", Op: opAgentMap, Effort: "medium",
		Desc: "单 agent 均衡研究（5 轮迭代，带出处）。"},
	{ID: "agent-high", Name: "Agent Deep", Op: opAgentMap, Effort: "high",
		Desc: "单 agent 深度研究（10 轮迭代，带出处）。耗时与成本最高。"},
	{ID: "multi-agent-low", Name: "Multi Fast", Op: opMultiAgent, Effort: "low",
		Desc: "3 个方向 agent 并行研究后综合。"},
	{ID: "multi-agent-medium", Name: "Multi", Op: opMultiAgent, Effort: "medium",
		Desc: "4 个方向 agent 并行研究后综合。"},
	{ID: "multi-agent-high", Name: "Multi Deep", Op: opMultiAgent, Effort: "high",
		Desc: "2 个前沿 agent 深挖后综合，最慢最贵。"},
	{ID: "forecast", Name: "Forecast", Op: opForecast, Effort: "low",
		Desc: "对问题给出 0–100 的概率（binary）。输入须是一个可判定真假的问题。"},
}

// aliases 别名 → 规范模型名。保留旧名/顺手名的目的是让客户端不必记住档位后缀。
var aliases = map[string]string{
	"agent":   "agent-medium",
	"default": "agent-medium",
	"auto":    "agent-medium",
	"multi":   "multi-agent-medium",
}

// specByID 规范名 → spec。
func specByID(id string) (modelSpec, bool) {
	for _, s := range specs {
		if s.ID == id {
			return s, true
		}
	}
	return modelSpec{}, false
}

// lookupSpec 解析客户端传来的模型名（可能带 `futuresearch/` 前缀）。
// 顺序：别名归一 → 预设档 → 上游 llm 枚举（143 个，见 llmenum.go）。
// 都未命中返回 false（调用方本地 400，不静默回落默认档）。
func lookupSpec(model string) (modelSpec, bool) {
	id := BareModelID(model)
	if canon, ok := aliases[id]; ok {
		id = canon
	}
	if sp, ok := specByID(id); ok {
		return sp, true
	}
	if b, ok := lookupBase(id); ok {
		// 家族名：档位由请求体的 reasoning_effort 决定，这里先给默认档。
		slug := b.pickTier("")
		lm := llmBySlug[slug]
		return modelSpec{ID: b.Slug, Name: b.Label, Op: opAgentMap, LLM: lm.Enum, Base: b.Slug,
			Desc: "模型家族（" + b.Label + "），档位由 reasoning_effort 决定；默认 " + slug + "。"}, true
	}
	if lm, ok := llmBySlug[id]; ok {
		// 真模型档：单次调用（iteration_budget=0），不做多轮研究。
		// 想研究就用 agent-* / multi-agent-* 预设。
		return modelSpec{ID: lm.Slug, Name: lm.Label, Op: opAgentMap, LLM: lm.Enum,
			Desc: "显式指定底层模型（" + lm.Enum + "），单次调用不做多轮研究。"}, true
	}
	return modelSpec{}, false
}

// BareModelID 剥掉 `channel/` 前缀（handler 传进来的 model 可能带前缀，
// 也可能已经被 runtimeForModel 剥过——两种都要能解析）。
func BareModelID(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return strings.ToLower(m)
}

// ---------------------------------------------------------------------------
// 模型家族 × 推理档（把 143 个枚举收敛成「家族 + 档位」两个维度）
// ---------------------------------------------------------------------------
//
// 上游的 143 个枚举 = 33 个家族 × 推理档。平铺给客户端太长（dsh 的选择器会展开成
// 一屏），所以额外提供「家族」这一层：模型名 = 家族 slug，档位走请求体里的
// `reasoning_effort`（OpenAI 风格，dsh 的 reasoningEfforts 就是发这个字段）。
//
// 两者都保留：完整 slug（`claude-fable-5-max`）仍然可直接调用 —— 家族名只是多了
// 一个「档位由参数决定」的入口。

// tierTokens 上游枚举里表示推理档的后缀 token（去掉它剩下的就是家族 slug）。
// 注意 "mini"/"nano"/"pro"/"lite"/"flash" 不是档位，是型号的一部分（GPT-5 Mini 是
// 另一个模型，不是 GPT-5 的低档），故不在表内。
var tierTokens = map[string]bool{
	"nt": true, "minimal": true, "low": true, "medium": true,
	"high": true, "xhigh": true, "max": true, "thinking": true,
}

// tierOrder 档位升级顺序（挑默认档用；"" = 枚举里不带后缀的那一个）。
var tierOrder = []string{"", "nt", "minimal", "low", "medium", "high", "thinking", "xhigh", "max"}

// llmBase 一个模型家族。
type llmBase struct {
	Slug  string            // 家族 slug（不含渠道前缀）
	Label string            // 展示名（不带档位）
	Tiers map[string]string // 档位 token → 完整枚举 slug
	All   []string          // 该家族全部枚举 slug（按上游顺序）
}

// llmBases 家族索引，由 llmModels 派生（加家族不用改生成文件）。
var llmBases = func() map[string]*llmBase {
	bases := make(map[string]*llmBase, 64)
	for _, m := range llmModels {
		base, tier := splitTier(m.Slug)
		b, ok := bases[base]
		if !ok {
			label := m.Label
			if tier != "" {
				label = strings.TrimSuffix(label, " "+tierLabel(tier))
			}
			b = &llmBase{Slug: base, Label: label, Tiers: map[string]string{}}
			bases[base] = b
		}
		b.Tiers[tier] = m.Slug
		b.All = append(b.All, m.Slug)
	}
	return bases
}()

// splitTier 把 slug 拆成 (家族, 档位)；末尾不是档位 token 时档位为空串。
func splitTier(slug string) (string, string) {
	i := strings.LastIndex(slug, "-")
	if i <= 0 {
		return slug, ""
	}
	if last := slug[i+1:]; tierTokens[last] {
		return slug[:i], last
	}
	return slug, ""
}

// tierLabel 档位 token 的展示写法（与 llmenum.go 的标签一致）。
func tierLabel(tier string) string {
	switch tier {
	case "nt":
		return "NT"
	case "xhigh":
		return "XHigh"
	default:
		return strings.ToUpper(tier[:1]) + tier[1:]
	}
}

// lookupBase 按家族 slug 查表。
func lookupBase(slug string) (*llmBase, bool) {
	b, ok := llmBases[slug]
	return b, ok
}

// pickTier 在家族里挑一个档位：优先请求的档位，其次 medium/low/high，再次
// 枚举里不带后缀的那个，最后按升级顺序取第一个可用的。
// 返回完整枚举 slug。
func (b *llmBase) pickTier(want string) string {
	want = strings.ToLower(strings.TrimSpace(want))
	// "off"/"none" = 关掉思考：优先「枚举里不带后缀的那个」，其次 NT。
	// 客户端用 reasoning_effort 表达这件事，wire 值就是 "off"（见 config.Prompts 同款约定）。
	if want == "off" || want == "none" || want == "disabled" {
		for _, t := range []string{"", "nt"} {
			if slug, ok := b.Tiers[t]; ok {
				return slug
			}
		}
		want = ""
	}
	if want != "" {
		if slug, ok := b.Tiers[want]; ok {
			return slug
		}
	}
	for _, t := range []string{"medium", "low", "high", "", "nt"} {
		if slug, ok := b.Tiers[t]; ok {
			return slug
		}
	}
	for _, t := range tierOrder {
		if slug, ok := b.Tiers[t]; ok {
			return slug
		}
	}
	// 兜底：不该发生（每个家族至少一个枚举）
	return b.All[0]
}

// BaseModels 家族目录（面板/`/v1/models` 用）—— 每个家族一条，档位由 reasoning_effort 决定。
// 顺序沿用上游枚举的出场顺序（GPT → Claude → Gemini → …），保证菜单稳定。
func BaseModels() []ModelInfo {
	seen := make(map[string]bool, len(llmBases))
	out := make([]ModelInfo, 0, len(llmBases))
	for _, m := range llmModels {
		base, _ := splitTier(m.Slug)
		if seen[base] {
			continue
		}
		seen[base] = true
		b := llmBases[base]
		out = append(out, ModelInfo{ID: b.Slug, Name: b.Label, SupportsReasoning: true})
	}
	return out
}

// StaticModels 面板与 /v1/models 的静态目录（无动态拉取：上游没有模型列表接口）。
// = 7 个预设档 + 143 个上游 llm 枚举（真模型名）。
func StaticModels() []ModelInfo {
	out := make([]ModelInfo, 0, len(specs)+len(llmBases))
	for _, s := range specs {
		out = append(out, ModelInfo{
			ID:   s.ID,
			Name: s.Name,
			// ContextWindow/MaxTokens 保持 0 = 未知：上游不返回该口径，
			// 编一个数字会让客户端按错误容量截断请求（ContextFromAPI 亦为 false）。
			SupportsReasoning: true, // medium/high 档会带研究过程（reasoning_content）
			SupportsTools:     false,
			SupportsImages:    false,
		})
	}
	// 家族（档位走 reasoning_effort）。
	out = append(out, BaseModels()...)
	// 完整枚举名也列出来：**多租户网关会把请求体重建成自己的 struct，
	// `reasoning_effort` 这类非标字段可能被丢掉**（one-api / new-api 的
	// GeneralOpenAIRequest 就是这种写法）—— 一旦丢掉，家族名只能落默认档。
	// 所以把「档位写在名字里」的形式也暴露出来，网关那侧不依赖字段透传。
	for _, m := range llmModels {
		out = append(out, ModelInfo{ID: m.Slug, Name: m.Label, SupportsReasoning: true})
	}
	return out
}

// ---------------------------------------------------------------------------
// 给「一键接入」用的元信息
// ---------------------------------------------------------------------------

// IsTieredSlug 这个 slug 是不是「档位写在名字里」的完整枚举（而不是家族名）。
// 写客户端配置时要跳过它们 —— 否则模型选择器会被 143 条撑爆。
func IsTieredSlug(id string) bool {
	_, tier := splitTier(BareModelID(id))
	return tier != ""
}

// EffortsFor 返回该模型的「客户端档位 → 线上取值」映射。
//
// 预设档只认 low/medium/high（上游 effort_level 的全部取值）；模型家族按它实际有的档位。
// 返回空 map = 这个模型不该给客户端显示档位选择器。
func EffortsFor(id string) map[string]string {
	id = BareModelID(id)
	for _, s := range specs {
		if s.ID != id {
			continue
		}
		// forecast 的 effort 是上游必填参数，但**不是**用户可选的维度 —— 别给客户端显示档位。
		if s.Op == opForecast {
			return map[string]string{}
		}
		return map[string]string{"low": "low", "medium": "medium", "high": "high"}
	}
	b, ok := lookupBase(id)
	if !ok {
		return map[string]string{}
	}
	out := map[string]string{}
	used := map[string]bool{}
	// 先按 token 名字对号入座
	for tier := range b.Tiers {
		lv := ""
		switch tier {
		case "nt":
			lv = "off"
		case "minimal", "low", "medium", "high", "xhigh", "max":
			lv = tier
		case "":
			lv = "off"
		}
		if lv == "" {
			continue // thinking 之类：下面兜底
		}
		if used[lv] {
			continue
		}
		wire := tier
		if tier == "" {
			wire = "off" // 空值在客户端表示「不发这个字段」，所以用一个显式取值
		}
		out[lv], used[lv] = wire, true
	}
	// 没对上的档位（如 THINKING）占剩下的最高档
	for tier := range b.Tiers {
		switch tier {
		case "nt", "minimal", "low", "medium", "high", "xhigh", "max", "":
			continue
		}
		for _, cand := range []string{"xhigh", "max", "high", "medium", "low", "minimal"} {
			if !used[cand] {
				out[cand], used[cand] = tier, true
				break
			}
		}
	}
	return out
}
