package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hey345437-boop/futuresearch-gateway/internal/config"
	"github.com/hey345437-boop/futuresearch-gateway/internal/fsapi"
	"github.com/hey345437-boop/futuresearch-gateway/internal/pool"
)

func newTestServer(t *testing.T, withLocal bool) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Normalize()
	s := New(Options{
		Config: cfg,
		Pool:   pool.New(cfg.DataDir + "/keys.json"),
		Up:     fsapi.New(),
		MCP:    http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("research-mcp")) }),
	})
	if withLocal {
		s.AddMCPEndpoint("/mcp/local", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("local-mcp"))
		}))
	}
	return s
}

// TestMCPEndpointsAreExact ★ `/mcp/` 是前缀注册，会吃掉 `/mcp/local` ——
// 本地工具没启用时必须 404，**不能**静默把研究工具端出去。
func TestMCPEndpointsAreExact(t *testing.T) {
	s := newTestServer(t, false)
	h := s.Handler()
	cases := []struct {
		path string
		want int
		body string
	}{
		{"/mcp", 200, "research-mcp"},
		{"/mcp/", 200, "research-mcp"},
		{"/mcp/local", 404, ""}, // 没启用 → 必须 404
		{"/mcp/other", 404, ""}, // 未知子路径 → 404
		{"/mcp/local/x", 404, ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", c.path, nil))
		if rec.Code != c.want {
			t.Errorf("POST %s = %d，want %d", c.path, rec.Code, c.want)
		}
		if c.body != "" && !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("POST %s body = %q，want 含 %q", c.path, rec.Body.String(), c.body)
		}
	}
}

// TestMCPLocalWinsWhenMounted 挂上本地工具后，最长匹配应让它接管 /mcp/local。
func TestMCPLocalWinsWhenMounted(t *testing.T) {
	s := newTestServer(t, true)
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp/local", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "local-mcp") {
		t.Fatalf("/mcp/local 应交给本地工具集：%d %q", rec.Code, rec.Body.String())
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/mcp", nil))
	if !strings.Contains(rec2.Body.String(), "research-mcp") {
		t.Fatalf("/mcp 仍应是研究工具：%q", rec2.Body.String())
	}
}
