package mcp

import (
	"strings"
	"testing"
)

// fakeGateway 只实现被调到的那些方法。
type fakeGateway struct {
	lastModel  string
	lastPrompt string
	lastEffort string
	decideQ    string
	decideOpts []string
}

func (f *fakeGateway) Complete(model, prompt string) (string, string, error) {
	f.lastModel, f.lastPrompt = model, prompt
	return "答案", "过程", nil
}
func (f *fakeGateway) Models() []string    { return []string{"agent-low", "claude-fable-5"} }
func (f *fakeGateway) PoolBalance() string { return "号池：1 个账号" }
func (f *fakeGateway) SubmitAsync(m, p, e string) (string, error) {
	f.lastModel, f.lastPrompt, f.lastEffort = m, p, e
	return "task-123", nil
}
func (f *fakeGateway) AsyncStatus(id string) (string, error) { return "状态: " + id, nil }
func (f *fakeGateway) AsyncResult(id string) (string, error) { return "结果: " + id, nil }
func (f *fakeGateway) AsyncCancel(id string) (string, error) { return "已取消 " + id, nil }
func (f *fakeGateway) AsyncCost(id string) (string, error)   { return "费用: " + id, nil }
func (f *fakeGateway) Decide(q string, opts []string, e, m string) (string, string, error) {
	f.decideQ, f.decideOpts = q, opts
	return "决策分析", "推理", nil
}

// TestToolListCoversOfficialGaps 工具集要覆盖官方那套里最值钱的几个
// （决策 / 异步任务 / 余额），别退回成只有 research/forecast/models。
func TestToolListCoversOfficialGaps(t *testing.T) {
	set := NewResearch(&fakeGateway{})
	names := map[string]bool{}
	for _, tl := range set.Tools() {
		names[tl["name"].(string)] = true
	}
	for _, want := range []string{
		"research", "forecast", "decision",
		"submit_research", "task_status", "task_result", "task_cancel", "task_cost",
		"balance", "models",
	} {
		if !names[want] {
			t.Errorf("缺工具 %q", want)
		}
	}
}

// TestModelSelection 模型选择：显式 model 优先，否则按 effort 选预设档。
func TestModelSelection(t *testing.T) {
	f := &fakeGateway{}
	set := NewResearch(f)
	if _, err := set.Call("research", map[string]any{"question": "x", "model": "claude-fable-5", "effort": "high"}); err != nil {
		t.Fatal(err)
	}
	if f.lastModel != "claude-fable-5" {
		t.Fatalf("显式 model 应优先，实际 %q", f.lastModel)
	}
	if _, err := set.Call("research", map[string]any{"question": "x", "effort": "high"}); err != nil {
		t.Fatal(err)
	}
	if f.lastModel != "agent-high" {
		t.Fatalf("只有 effort 时应映射成 agent-high，实际 %q", f.lastModel)
	}
	if _, err := set.Call("research", map[string]any{"question": "x"}); err != nil {
		t.Fatal(err)
	}
	if f.lastModel != "agent-medium" {
		t.Fatalf("都不给应落默认，实际 %q", f.lastModel)
	}
}

// TestSubmitResumeAsync 异步提交要带上 effort，并返回 task_id。
func TestSubmitAsync(t *testing.T) {
	f := &fakeGateway{}
	set := NewResearch(f)
	out, err := set.Call("submit_research", map[string]any{"question": "查 X", "effort": "high"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "task-123") {
		t.Fatalf("回执应带 task_id：%q", out)
	}
	if f.lastModel != "agent-high" || f.lastEffort != "high" {
		t.Fatalf("effort 没传对：model=%q effort=%q", f.lastModel, f.lastEffort)
	}
}

// TestDecisionOptionsTolerance 选项写成数组、逗号串、换行串都要认。
func TestDecisionOptionsTolerance(t *testing.T) {
	for _, in := range []any{
		[]any{"A", "B"},
		"A,B",
		"A\nB",
		"A; B",
	} {
		f := &fakeGateway{}
		set := NewResearch(f)
		if _, err := set.Call("decision", map[string]any{"question": "q", "options": in}); err != nil {
			t.Fatalf("%v → %v", in, err)
		}
		if len(f.decideOpts) != 2 {
			t.Fatalf("%v 应解析出 2 个选项，实际 %v", in, f.decideOpts)
		}
	}
}

// TestUnknownTool 未知工具要报错，不能静默返回空。
func TestUnknownTool(t *testing.T) {
	set := NewResearch(&fakeGateway{})
	if _, err := set.Call("no_such_tool", nil); err == nil {
		t.Fatal("未知工具应报错")
	}
}

// TestTaskToolsForwardID 任务类工具要把 id 原样转给网关。
func TestTaskToolsForwardID(t *testing.T) {
	set := NewResearch(&fakeGateway{})
	for _, c := range []struct{ tool, want string }{
		{"task_status", "状态: T"}, {"task_result", "结果: T"},
		{"task_cancel", "已取消 T"}, {"task_cost", "费用: T"},
	} {
		out, err := set.Call(c.tool, map[string]any{"task_id": "T"})
		if err != nil || !strings.Contains(out, c.want) {
			t.Fatalf("%s → %q %v", c.tool, out, err)
		}
	}
}
