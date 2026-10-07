// ops.go MCP 那批「批量操作 / 查询」工具的网关侧实现。
//
// 统一走 useAccount：挑号 → 执行 → 传输层错误换号重试；上游 ≥400 归类后把错误交回。
package server

import (
	"fmt"
	"strings"

	"github.com/hey345437-boop/futuresearch-gateway/internal/fsapi"
)

// useAccount 挑一个可用账号执行 fn。
//
// fn 返回 (retry, err)：
//
//	retry=true  → 传输层问题，换号重试
//	retry=false → 结束；err 非空表示失败（上游错误已归类并罚过号）
func (s *Server) useAccount(fn func(key string) (bool, error)) error {
	if s.pool.Len() == 0 {
		return fmt.Errorf("号池是空的：打开面板加一个 FutureSearch API key")
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
		retry, err := fn(e.Key.Key)
		if !retry {
			if err == nil {
				s.pool.NoteSuccess(e)
			}
			return err
		}
		lastErr = err
		s.pool.Cooldown(uid, fsapi.ErrServer, s.cfg.SoftCooldownDur, "传输层错误")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用账号（全部在冷却或已停用）")
	}
	return lastErr
}

// upstreamErr 把「上游 ≥400」统一成可读错误（并已由调用方罚过号）。
func upstreamErr(status int, raw []byte) error {
	return fmt.Errorf("上游返回 %d：%s", status, snippet(string(raw)))
}

// MultiAgent 多 agent 并行研究。
func (s *Server) MultiAgent(task string, directions []string, effort string) (string, string, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return "", "", fmt.Errorf("task 不能为空")
	}
	var answer, reasoning string
	err := s.useAccount(func(key string) (bool, error) {
		a, r, status, raw, err := s.up.MultiAgentOp(key, task, directions, effort)
		if err != nil {
			if status >= 400 {
				return false, upstreamErr(status, raw)
			}
			return true, err
		}
		answer, reasoning = a, r
		return false, nil
	})
	return answer, reasoning, err
}

// Classify 把一批条目分类。
func (s *Server) Classify(task string, categories, items []string) (string, error) {
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("task 不能为空")
	}
	if len(categories) == 0 {
		return "", fmt.Errorf("categories 不能为空")
	}
	if len(items) == 0 {
		return "", fmt.Errorf("items 不能为空")
	}
	var out string
	err := s.useAccount(func(key string) (bool, error) {
		a, r, status, raw, err := s.up.ClassifyOp(key, task, categories, items)
		if err != nil {
			if status >= 400 {
				return false, upstreamErr(status, raw)
			}
			return true, err
		}
		out = a
		if strings.TrimSpace(r) != "" {
			out = a + "\n\n---\n分类理由：\n" + r
		}
		return false, nil
	})
	return out, err
}

// Rank 给一批条目打分排序。
func (s *Server) Rank(task string, items []string, ascending bool) (string, error) {
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("task 不能为空")
	}
	if len(items) == 0 {
		return "", fmt.Errorf("items 不能为空")
	}
	var out string
	err := s.useAccount(func(key string) (bool, error) {
		a, r, status, raw, err := s.up.RankOp(key, task, items, ascending)
		if err != nil {
			if status >= 400 {
				return false, upstreamErr(status, raw)
			}
			return true, err
		}
		out = a
		if strings.TrimSpace(r) != "" {
			out = a + "\n\n---\n打分理由：\n" + r
		}
		return false, nil
	})
	return out, err
}

// Dedupe 去重。
func (s *Server) Dedupe(equivalence string, items []string, strategy string) (string, string, error) {
	if strings.TrimSpace(equivalence) == "" {
		return "", "", fmt.Errorf("equivalence 不能为空（说明什么算「重复」）")
	}
	if len(items) < 2 {
		return "", "", fmt.Errorf("items 至少要两条才谈得上去重")
	}
	var answer, reasoning string
	err := s.useAccount(func(key string) (bool, error) {
		a, r, status, raw, err := s.up.DedupeOp(key, equivalence, items, strategy)
		if err != nil {
			if status >= 400 {
				return false, upstreamErr(status, raw)
			}
			return true, err
		}
		answer, reasoning = a, r
		return false, nil
	})
	return answer, reasoning, err
}

// Merge 语义表连接。
func (s *Server) Merge(task string, left, right []string, leftKey, rightKey string) (string, string, error) {
	if strings.TrimSpace(task) == "" {
		return "", "", fmt.Errorf("task 不能为空（说明按什么把两张表合起来）")
	}
	if len(left) == 0 || len(right) == 0 {
		return "", "", fmt.Errorf("left 和 right 都不能为空")
	}
	var answer, reasoning string
	err := s.useAccount(func(key string) (bool, error) {
		a, r, status, raw, err := s.up.MergeOp(key, task, left, right, leftKey, rightKey)
		if err != nil {
			if status >= 400 {
				return false, upstreamErr(status, raw)
			}
			return true, err
		}
		answer, reasoning = a, r
		return false, nil
	})
	return answer, reasoning, err
}

// UploadData 上传一批行，返回 artifact id。
func (s *Server) UploadData(rows []map[string]any) (string, error) {
	if len(rows) == 0 {
		return "", fmt.Errorf("rows 不能为空")
	}
	var id string
	err := s.useAccount(func(key string) (bool, error) {
		got, status, raw, err := s.up.UploadData(key, rows)
		if err != nil {
			if status >= 400 {
				return false, upstreamErr(status, raw)
			}
			return true, err
		}
		id = got
		return false, nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已上传 %d 行。artifact_id: %s\n（可以把它当其它操作的 input 用）", len(rows), id), nil
}

// BuiltInLists 上游内置的数据列表。
func (s *Server) BuiltInLists() (string, error) {
	var out string
	err := s.useAccount(func(key string) (bool, error) {
		lists, status, raw, err := s.up.BuiltInLists(key)
		if err != nil {
			if status >= 400 {
				return false, upstreamErr(status, raw)
			}
			return true, err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "上游内置列表 %d 个：\n", len(lists))
		for _, l := range lists {
			name, _ := l["name"].(string)
			cat, _ := l["category"].(string)
			n, _ := l["row_count"].(float64)
			fmt.Fprintf(&b, "  %-28s %-14s %d 行\n", name, cat, int(n))
		}
		out = b.String()
		return false, nil
	})
	return out, err
}

// UseBuiltInList 把内置列表复制成自己的 artifact。
func (s *Server) UseBuiltInList(nameOrID string) (string, error) {
	if strings.TrimSpace(nameOrID) == "" {
		return "", fmt.Errorf("name 不能为空")
	}
	var id string
	err := s.useAccount(func(key string) (bool, error) {
		got, status, raw, err := s.up.UseBuiltInList(key, nameOrID)
		if err != nil {
			if status >= 400 && status != 404 {
				return false, upstreamErr(status, raw)
			}
			return false, err
		}
		id = got
		return false, nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已复制成你的 artifact。artifact_id: %s", id), nil
}

// Sessions 最近会话。
func (s *Server) Sessions(limit int) (string, error) {
	var out string
	err := s.useAccount(func(key string) (bool, error) {
		list, status, raw, err := s.up.Sessions(key, limit)
		if err != nil {
			if status >= 400 {
				return false, upstreamErr(status, raw)
			}
			return true, err
		}
		if len(list) == 0 {
			out = "（还没有会话）"
			return false, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "最近 %d 个会话：\n", len(list))
		for _, s2 := range list {
			id, _ := s2["session_id"].(string)
			if id == "" {
				id, _ = s2["id"].(string)
			}
			name, _ := s2["name"].(string)
			at, _ := s2["created_at"].(string)
			fmt.Fprintf(&b, "  %s  %-24s %s\n", short(id), name, at)
		}
		out = b.String()
		return false, nil
	})
	return out, err
}

// SessionTasks 某个会话里的任务。
func (s *Server) SessionTasks(sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("session_id 不能为空")
	}
	var out string
	err := s.useAccount(func(key string) (bool, error) {
		list, status, raw, err := s.up.SessionTasks(key, sessionID)
		if err != nil {
			if status >= 400 {
				return false, upstreamErr(status, raw)
			}
			return true, err
		}
		if len(list) == 0 {
			out = "（这个会话里没有任务）"
			return false, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "会话 %s 里有 %d 个任务：\n", short(sessionID), len(list))
		for _, t := range list {
			id, _ := t["task_id"].(string)
			st, _ := t["status"].(string)
			ty, _ := t["task_type"].(string)
			fmt.Fprintf(&b, "  %s  %-10s %s\n", short(id), st, ty)
		}
		out = b.String()
		return false, nil
	})
	return out, err
}

// TaskProgress 任务的过程摘要（比 task_status 更细）。
func (s *Server) TaskProgress(taskID string) (string, error) {
	t, err := s.taskOf(taskID)
	if err != nil {
		return "", err
	}
	var out string
	err = s.useAccount(func(key string) (bool, error) {
		list, status, raw, err := s.up.TaskSummaries(key, t.ID)
		if err != nil {
			if status >= 400 {
				return false, upstreamErr(status, raw)
			}
			return true, err
		}
		if len(list) == 0 {
			out = "上游还没生成进度摘要（任务刚开始，或者太短没产生摘要）"
			return false, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "任务 %s 的进度摘要（%d 条）：\n", short(t.ID), len(list))
		for _, m := range list {
			txt, _ := m["summary"].(string)
			if txt == "" {
				txt, _ = m["text"].(string)
			}
			if txt == "" {
				txt = fmt.Sprint(m)
			}
			fmt.Fprintf(&b, "  · %s\n", snippet(txt))
		}
		out = b.String()
		return false, nil
	})
	return out, err
}
