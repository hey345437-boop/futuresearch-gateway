// Package panel 把面板静态文件打进二进制（单文件分发，不需要额外的 web 目录）。
package panel

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:web
var files embed.FS

// Handler 返回面板的 http.Handler。
func Handler() http.Handler {
	sub, err := fs.Sub(files, "web")
	if err != nil {
		return http.NotFoundHandler()
	}
	srv := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// no-store：面板是单文件、体积很小，不值得为缓存省流量；
		// 而「浏览器拿旧页面」会让人以为改动没生效（踩过一次）。
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		srv.ServeHTTP(w, r)
	})
}
