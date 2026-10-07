// Package config 网关配置：一个 JSON 文件，热更新靠面板写回。
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Listen 监听地址。兼容对象形与旧的 ":7868" 字符串形。
type Listen struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (l Listen) Addr() string {
	h := l.Host
	if h == "" {
		h = "127.0.0.1"
	}
	return net.JoinHostPort(h, fmt.Sprint(l.Port))
}

// IsLoopback 是否只监听本机 —— 决定「不设 api_key / admin_password」是否安全。
func (l Listen) IsLoopback() bool {
	h := strings.TrimSpace(l.Host)
	if h == "" || h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Config 顶层配置。
type Config struct {
	Listen    Listen `json:"listen"`
	APIKey    string `json:"api_key"`        // 客户端要带的 Bearer；空 = 不鉴权（仅环回允许）
	AdminPass string `json:"admin_password"` // 面板密码；空 = 不鉴权（仅环回允许）
	DataDir   string `json:"data_dir"`       // keys.json / 日志

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 单次 HTTP 调用上限，默认 120
	} `json:"upstream"`

	Pool struct {
		HardCooldown string `json:"hard_cooldown"` // 402 余额不足，默认 12h
		SoftCooldown string `json:"soft_cooldown"` // 429/404/403，默认 60s
		MaxRotate    int    `json:"max_rotate"`    // 单请求最多试几个号，默认 3
	} `json:"pool"`

	// Prompts 模型 → 前置指令（上游没 system 字段，只能把指令前置拼进 task）。
	// "*" 是所有模型的兜底；精确模型名覆盖 "*"；空串 = 该模型不注入。
	Prompts map[string]string `json:"prompts,omitempty"`

	// Preview 本地预览目录：把 agent 生成的 HTML/项目直接挂到一个 URL 上，
	// 不用「吐一段代码让人自己存文件再打开」。
	Preview struct {
		Enabled bool   `json:"enabled"`
		Dir     string `json:"dir"`  // 默认 <data_dir>/preview
		Path    string `json:"path"` // 默认 /preview/
	} `json:"preview"`

	// LocalFS 本地项目工具（read_file / write_file / list_dir / search / run_command）。
	// 这是「让 AI 操作本地项目」的那一半 —— 默认**关闭**，且开了也要显式给 root。
	LocalFS struct {
		Enabled    bool   `json:"enabled"`
		Root       string `json:"root"`
		AllowWrite bool   `json:"allow_write"`
		AllowExec  bool   `json:"allow_exec"`
		MaxReadKB  int    `json:"max_read_kb"`
		MaxOutKB   int    `json:"max_out_kb"`
		TimeoutSec int    `json:"timeout_sec"`
		Path       string `json:"path"` // 默认 /mcp/local
	} `json:"localfs"`

	// MCP 把 FutureSearch 暴露成 MCP 工具（research / forecast / models）。
	MCP struct {
		Enabled bool   `json:"enabled"`
		Path    string `json:"path"` // 默认 /mcp
	} `json:"mcp"`

	// 解析后
	HardCooldownDur time.Duration `json:"-"`
	SoftCooldownDur time.Duration `json:"-"`
}

// Default 默认配置（只监听环回，安全）。
func Default() *Config {
	c := &Config{
		Listen:  Listen{Host: "127.0.0.1", Port: 7868},
		DataDir: "./data",
	}
	c.Upstream.TimeoutSeconds = 120
	c.Pool.HardCooldown = "12h"
	c.Pool.SoftCooldown = "60s"
	c.Pool.MaxRotate = 3
	c.Prompts = map[string]string{}
	c.MCP.Enabled = true
	c.MCP.Path = "/mcp"
	c.Preview.Enabled = true
	c.Preview.Path = "/preview/"
	c.LocalFS.Path = "/mcp/local"
	c.LocalFS.MaxReadKB = 256
	c.LocalFS.MaxOutKB = 64
	c.LocalFS.TimeoutSec = 30
	return c
}

// Load 读配置；文件不存在则写一份默认的。
func Load(path string) (*Config, error) {
	c := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件不存在：返回默认配置，由调用方在 ApplyEnv 之后落盘
			// （否则容器里 env 覆盖的值不会写进文件，用户会困惑）。
			return c, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.Normalize()
	return c, nil
}

// ApplyEnv 用环境变量覆盖配置（容器部署用 —— 这样 docker run 一行就能起，
// 不用先手写 config.json）。
//
//	FSGW_LISTEN_HOST / FSGW_LISTEN_PORT
//	FSGW_API_KEY / FSGW_ADMIN_PASSWORD
//	FSGW_DATA_DIR
func (c *Config) ApplyEnv() {
	if v := os.Getenv("FSGW_LISTEN_HOST"); v != "" {
		c.Listen.Host = v
	}
	if v := os.Getenv("FSGW_LISTEN_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 65535 {
			c.Listen.Port = n
		}
	}
	if v, ok := os.LookupEnv("FSGW_API_KEY"); ok {
		c.APIKey = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("FSGW_ADMIN_PASSWORD"); ok {
		c.AdminPass = v
	}
	if v := os.Getenv("FSGW_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	c.Normalize()
}

// Save 原子写配置。
func Save(c *Config, path string) error {
	c.Normalize()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Normalize 填默认值并解析时长。可重复调用。
func (c *Config) Normalize() {
	if c.Listen.Port == 0 {
		c.Listen.Port = 7868
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.Pool.MaxRotate <= 0 {
		c.Pool.MaxRotate = 3
	}
	c.HardCooldownDur = parseDur(c.Pool.HardCooldown, 12*time.Hour)
	c.SoftCooldownDur = parseDur(c.Pool.SoftCooldown, 60*time.Second)
	if c.Pool.HardCooldown == "" {
		c.Pool.HardCooldown = "12h"
	}
	if c.Pool.SoftCooldown == "" {
		c.Pool.SoftCooldown = "60s"
	}
	if c.Prompts == nil {
		c.Prompts = map[string]string{}
	}
	if c.MCP.Path == "" {
		c.MCP.Path = "/mcp"
	}
	if c.Preview.Dir == "" {
		c.Preview.Dir = filepath.Join(c.DataDir, "preview")
	}
	if c.Preview.Path == "" {
		c.Preview.Path = "/preview/"
	}
	if c.LocalFS.Path == "" {
		c.LocalFS.Path = "/mcp/local"
	}
	if c.LocalFS.MaxReadKB <= 0 {
		c.LocalFS.MaxReadKB = 256
	}
	if c.LocalFS.MaxOutKB <= 0 {
		c.LocalFS.MaxOutKB = 64
	}
	if c.LocalFS.TimeoutSec <= 0 {
		c.LocalFS.TimeoutSec = 30
	}
}

func parseDur(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
