package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/hey345437-boop/futuresearch-gateway/internal/localfs"
)

// LocalToolSet 本地项目工具（read_file / write_file / list_dir / search / run_command）。
//
// 为什么单独一组：这一组**会碰你的磁盘**，跟「云端研究」性质完全不同。
// 分开挂（各自的 URL）能让客户端与人都一眼看清哪些工具是有副作用的。
type LocalToolSet struct{ fs *localfs.FS }

// NewLocal 建本地工具集。
func NewLocal(f *localfs.FS) *LocalToolSet { return &LocalToolSet{fs: f} }

// ServerName MCP server 名。
func (l *LocalToolSet) ServerName() string { return "localfs" }

// Tools 工具定义（按开关动态收窄 —— 没开写就不给 write_file，免得模型反复试）。
func (l *LocalToolSet) Tools() []map[string]any {
	cfg := l.fs.Config()
	out := []map[string]any{
		{
			"name": "list_dir",
			"description": "列出项目目录树（相对根目录的路径）。跳过 .git / node_modules / 隐藏项。" +
				fmt.Sprintf("根目录是 %s。", l.fs.Root()),
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"dir":   map[string]any{"type": "string", "description": "相对根目录的路径，留空 = 根目录"},
					"depth": map[string]any{"type": "integer", "description": "递归层数，1-4，默认 2"},
				},
			},
		},
		{
			"name":        "read_file",
			"description": "读一个文本文件（带行号）。二进制文件会被拒绝。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":   map[string]any{"type": "string", "description": "相对根目录的路径"},
					"offset": map[string]any{"type": "integer", "description": "从第几行开始（0 基），默认 0"},
					"limit":  map[string]any{"type": "integer", "description": "读多少行，默认全部"},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "search",
			"description": "用正则搜索文件内容，返回「文件:行号: 内容」。跳过 .git / node_modules / 二进制。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "Go 正则"},
					"dir":     map[string]any{"type": "string", "description": "搜索起点（相对根目录），留空 = 根目录"},
					"glob":    map[string]any{"type": "string", "description": "可选文件名过滤，如 *.go"},
				},
				"required": []string{"pattern"},
			},
		},
	}
	if cfg.AllowWrite {
		out = append(out,
			map[string]any{
				"name":        "write_file",
				"description": "写一个文本文件（父目录会自动创建，原子写）。用来把生成的页面/代码真正落到项目里。",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string", "description": "相对根目录的路径"},
						"content": map[string]any{"type": "string", "description": "完整文件内容"},
					},
					"required": []string{"path", "content"},
				},
			},
			map[string]any{
				"name":        "mkdir",
				"description": "创建目录。",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []string{"path"},
				},
			},
		)
	}
	if cfg.AllowExec {
		out = append(out, map[string]any{
			"name": "run_command",
			"description": "在项目根目录（或指定子目录）里执行一条命令。**不走 shell** —— " +
				"argv 逐项传入，所以 `;`、`|`、`&&` 都不会被解释。有超时和输出上限。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"argv": map[string]any{
						"type": "array", "items": map[string]any{"type": "string"},
						"description": "命令与参数，如 [\"npm\",\"test\"]",
					},
					"dir": map[string]any{"type": "string", "description": "工作目录（相对根目录），默认根目录"},
				},
				"required": []string{"argv"},
			},
		})
	}
	return out
}

// Call 执行工具。
func (l *LocalToolSet) Call(name string, args map[string]any) (string, error) {
	switch name {
	case "list_dir":
		return l.fs.ListDir(str(args["dir"]), int(num(args["depth"])))
	case "read_file":
		return l.fs.ReadFile(str(args["path"]), int(num(args["offset"])), int(num(args["limit"])))
	case "search":
		return l.fs.Search(str(args["pattern"]), str(args["dir"]), str(args["glob"]))
	case "write_file":
		return l.fs.WriteFile(str(args["path"]), str(args["content"]))
	case "mkdir":
		return l.fs.Mkdir(str(args["path"]))
	case "run_command":
		return l.fs.RunCommand(context.Background(), strSlice(args["argv"]), str(args["dir"]))
	default:
		return "", fmt.Errorf("未知工具：%s", name)
	}
}

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		var f float64
		_, _ = fmt.Sscanf(strings.TrimSpace(t), "%g", &f)
		return f
	}
	return 0
}

func strSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return strings.Fields(s) // 兜底：按空白切（仍然不走 shell）
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
