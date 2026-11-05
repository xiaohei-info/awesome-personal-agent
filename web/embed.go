// Package web 把 Vite 静态构建（web/dist）嵌入二进制（设计 §18）。先 `npm --prefix web run build` 再 go build。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler 提供嵌入的前端：存在的静态文件原样返回，其余路径回落到 index.html（前端路由）。
func Handler() http.Handler {
	sub, _ := fs.Sub(dist, "dist")
	return handler(sub)
}

func handler(root fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if st, err := fs.Stat(root, name); err != nil || st.IsDir() {
			name = "index.html" // 前端路由：未知路径交给 SPA
		}
		if _, err := fs.Stat(root, name); err != nil {
			http.Error(w, "web UI is not built: run `npm --prefix web run build`", http.StatusServiceUnavailable)
			return
		}
		http.ServeFileFS(w, r, root, name)
	})
}
