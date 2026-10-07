// tasks.go 异步任务登记 + MCP 需要的那几个「池子级」操作。
//
// 为什么要登记表：异步路径里，投任务和取结果是**两次独立的调用**，
// 中间可能隔十几分钟。而「查状态/取结果」必须用**当初投任务那个账号的 key**
// （上游任务归属账号）。所以投的时候就得把 task_id → key 记下来。
package server

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hey345437-boop/futuresearch-gateway/internal/fsapi"
)

// asyncTask 一个在飞的任务。
type asyncTask struct {
	ID      string
	Key     string // 上游 key（查状态/取结果/取消都要用）
	UID     string
	Model   string
	Prompt  string
	Started time.Time
}

// taskRegistry 任务登记表（内存态，重启即失 —— 任务本来就是短命的）。
type taskRegistry struct {
	mu    sync.Mutex
	byID  map[string]*asyncTask
	order []string
}

func newTaskRegistry() *taskRegistry {
	return &taskRegistry{byID: map[string]*asyncTask{}}
}

func (r *taskRegistry) put(t *asyncTask) {
	if r == nil { // 防御：漏初始化时只丢登记，不该 panic 把服务打崩
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[t.ID] = t
	r.order = append(r.order, t.ID)
	if len(r.order) > 500 { // 只留最近 500 条，别无限涨
		drop := r.order[0]
		r.order = r.order[1:]
		delete(r.byID, drop)
	}
}

func (r *taskRegistry) get(id string) *asyncTask {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

// pickAccount 挑一个可用账号（与 chat 路径同一套挑号逻辑）。
func (s *Server) pickAccount() (*fsapi.Key, string, error) {
	if s.pool.Len() == 0 {
		return nil, "", fmt.Errorf("号池是空的：打开面板加一个 FutureSearch API key")
	}
	e := s.pool.Pick(nil)
	if e == nil {
		return nil, "", fmt.Errorf("没有可用账号（全部在冷却或已停用）")
	}
	uid := s.pool.UIDOf(e)
	return e.Key, uid, nil
}

// SubmitAsync 投一个异步任务，立刻返回 task_id。
func (s *Server) SubmitAsync(model, prompt, effort string) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", fmt.Errorf("prompt 不能为空")
	}
	if model == "" {
		model = "agent-medium"
	}
	prep, err := s.up.Prepare(model, prompt, effort)
	if err != nil {
		return "", err
	}
	// 挑号 → 投任务，失败就换号（与 chat 路径同规则）
	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < s.cfg.Pool.MaxRotate; i++ {
		e := s.pool.Pick(tried)
		if e == nil {
			break
		}
		uid := s.pool.UIDOf(e)
		tried[uid] = true
		id, status, raw, terr := s.up.Submit(e.Key.Key, prep)
		if terr != nil {
			lastErr = terr
			s.pool.Cooldown(uid, fsapi.ErrServer, s.cfg.SoftCooldownDur, "传输层错误")
			continue
		}
		if status >= 400 {
			kind := s.up.Classify(status, string(raw))
			s.penalize(uid, kind, string(raw))
			return "", fmt.Errorf("上游返回 %d：%s", status, snippet(string(raw)))
		}
		s.tasks.put(&asyncTask{ID: id, Key: e.Key.Key, UID: uid, Model: model, Prompt: prompt, Started: time.Now()})
		s.appendLog(fmt.Sprintf("异步任务已投 %s（模型 %s，账号 %s）", id[:8], model, short(uid)))
		return id, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用账号")
	}
	return "", lastErr
}

// taskOf 取任务（并给出人类可读的「找不到」）。
func (s *Server) taskOf(id string) (*asyncTask, error) {
	t := s.tasks.get(strings.TrimSpace(id))
	if t == nil {
		return nil, fmt.Errorf("不认识的 task_id：%s（本进程重启后登记会丢，用 submit 时返回的那个）", id)
	}
	return t, nil
}

// AsyncStatus 查任务状态（人类可读）。
func (s *Server) AsyncStatus(id string) (string, error) {
	t, err := s.taskOf(id)
	if err != nil {
		return "", err
	}
	snap, status, raw, err := s.up.Snapshot(t.Key, t.ID)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("上游返回 %d：%s", status, snippet(string(raw)))
	}
	waited := time.Since(t.Started).Round(time.Second)
	var b strings.Builder
	fmt.Fprintf(&b, "task_id: %s\n状态: %s\n模型: %s\n已等待: %s\n", snap.TaskID, snap.Status, t.Model, waited)
	if snap.Label != "" {
		fmt.Fprintf(&b, "当前步骤: %s\n", snap.Label)
	}
	if snap.Total > 0 {
		fmt.Fprintf(&b, "进度: 完成 %d/%d（运行中 %d，失败 %d）\n", snap.Done, snap.Total, snap.Running, snap.Failed)
	}
	if snap.Error != "" {
		fmt.Fprintf(&b, "错误: %s\n", snap.Error)
	}
	switch snap.Status {
	case "completed":
		b.WriteString("\n→ 已就绪，用 task_result 取结果")
	case "failed", "revoked":
		b.WriteString("\n→ 任务已失败，别等了")
	default:
		b.WriteString("\n→ 还在跑，过一会儿再查")
	}
	return b.String(), nil
}

// AsyncResult 取结果。
func (s *Server) AsyncResult(id string) (string, error) {
	t, err := s.taskOf(id)
	if err != nil {
		return "", err
	}
	answer, reasoning, status, raw, err := s.up.Result(t.Key, t.ID)
	if err != nil {
		if status >= 400 {
			return "", fmt.Errorf("上游返回 %d：%s", status, snippet(string(raw)))
		}
		return "", err
	}
	if reasoning != "" {
		return answer + "\n\n---\n研究过程：\n" + reasoning, nil
	}
	return answer, nil
}

// AsyncCancel 取消任务。
func (s *Server) AsyncCancel(id string) (string, error) {
	t, err := s.taskOf(id)
	if err != nil {
		return "", err
	}
	status, raw, err := s.up.Cancel(t.Key, t.ID)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("上游返回 %d：%s", status, snippet(string(raw)))
	}
	s.appendLog("已取消异步任务 " + short(t.ID))
	return fmt.Sprintf("已取消 %s", t.ID), nil
}

// AsyncCost 查任务费用。
func (s *Server) AsyncCost(id string) (string, error) {
	t, err := s.taskOf(id)
	if err != nil {
		return "", err
	}
	dollars, rawText, status, raw, err := s.up.Cost(t.Key, t.ID)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("上游返回 %d：%s", status, snippet(string(raw)))
	}
	if dollars > 0 {
		return fmt.Sprintf("任务 %s 费用：$%.4f", t.ID, dollars), nil
	}
	if strings.TrimSpace(rawText) != "" {
		return fmt.Sprintf("上游还没给出费用（原文：%s）", snippet(rawText)), nil
	}
	return "上游还没给出费用（任务可能还没跑完）", nil
}

// PoolBalance 号池余额汇总。
func (s *Server) PoolBalance() string {
	entries := s.pool.List()
	total := s.pool.TotalBalanceCents()
	usable, cooling, disabled := 0, 0, 0
	type row struct {
		email string
		cents int64
	}
	var rows []row
	for _, e := range entries {
		switch {
		case e.Disabled:
			disabled++
		case e.Cooling():
			cooling++
		default:
			usable++
		}
		rows = append(rows, row{e.Key.Email, e.BalanceCents})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].cents > rows[j].cents })
	var b strings.Builder
	fmt.Fprintf(&b, "号池：%d 个账号，可用 %d / 冷却 %d / 停用 %d\n", len(entries), usable, cooling, disabled)
	fmt.Fprintf(&b, "余额合计：$%.2f\n", float64(total)/100)
	if len(rows) > 0 {
		b.WriteString("\n余额最高的几个：\n")
		for i, r := range rows {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "  %-40s $%.2f\n", r.email, float64(r.cents)/100)
		}
	}
	return b.String()
}

// Decide 决策分析：把「你在权衡什么 + 有哪些选项」包成一个研究任务。
func (s *Server) Decide(question string, options []string, effort string, model string) (string, string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", "", fmt.Errorf("question 不能为空")
	}
	var b strings.Builder
	b.WriteString("用户正在权衡一个决策。请按下面的结构给出一份决策分析：\n\n")
	b.WriteString("1. 先复述他实际在选的选项（含「什么都不做」如果算一个）；如果他没列全，补上明显的遗漏项；\n")
	b.WriteString("2. 对**每个**选项分别分析：最可能的结果、关键量化估计（数字/区间/概率）、主要风险、以及什么条件下这个选项会更好；\n")
	b.WriteString("3. 最后给一张对比表（选项 × 关键维度），加一句明确的推荐和它的前提。\n\n")
	fmt.Fprintf(&b, "决策背景：\n%s\n", question)
	if len(options) > 0 {
		b.WriteString("\n他列的选项：\n")
		for _, o := range options {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(o))
		}
	}
	if model == "" {
		model = "agent-medium"
	}
	return s.Complete(model, b.String())
}
