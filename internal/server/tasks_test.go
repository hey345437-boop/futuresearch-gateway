package server

import (
	"strings"
	"testing"
	"time"
)

// TestTaskRegistryBasic 登记表能存能取。
func TestTaskRegistryBasic(t *testing.T) {
	r := newTaskRegistry()
	r.put(&asyncTask{ID: "t-1", Key: "k", Model: "agent-low", Started: time.Now()})
	got := r.get("t-1")
	if got == nil || got.Key != "k" {
		t.Fatalf("取不到刚登记的任务：%+v", got)
	}
	if r.get("nope") != nil {
		t.Fatal("不存在的 id 应返回 nil")
	}
}

// TestTaskRegistryNilSafe ★ 这是真实踩过的坑：
// Server 结构体加了 tasks 字段但 New() 里漏了初始化 → put() 空指针 panic，
// **整个网关进程被打崩**（`http: panic serving ...`）。
// 现在漏初始化只丢登记，不该崩。
func TestTaskRegistryNilSafe(t *testing.T) {
	var r *taskRegistry
	r.put(&asyncTask{ID: "x"}) // 不该 panic
	if r.get("x") != nil {
		t.Fatal("nil 登记表应返回 nil")
	}
}

// TestTaskRegistryCapsGrowth 登记表不能无限涨（长跑服务里会吃内存）。
func TestTaskRegistryCapsGrowth(t *testing.T) {
	r := newTaskRegistry()
	for i := 0; i < 600; i++ {
		r.put(&asyncTask{ID: string(rune('a'+i%26)) + string(rune('0'+i/26%10)) + string(rune('0'+i/260))})
	}
	r.mu.Lock()
	n := len(r.byID)
	r.mu.Unlock()
	if n > 500 {
		t.Fatalf("登记表应被裁剪到 500 以内，实际 %d", n)
	}
}

// TestPoolBalanceOnEmptyPool 空池子也要给出可读回执，不能崩。
func TestPoolBalanceOnEmptyPool(t *testing.T) {
	s := newTestServer(t, false)
	out := s.PoolBalance()
	if !strings.Contains(out, "号池") || !strings.Contains(out, "余额合计") {
		t.Fatalf("回执格式不对：%q", out)
	}
}

// TestAsyncStatusUnknownTask 不认识的 task_id 要给可读错误，不是 panic。
func TestAsyncStatusUnknownTask(t *testing.T) {
	s := newTestServer(t, false)
	if _, err := s.AsyncStatus("no-such-id"); err == nil {
		t.Fatal("不存在的 task_id 应报错")
	} else if !strings.Contains(err.Error(), "task_id") {
		t.Fatalf("错误信息应提到 task_id：%v", err)
	}
}
