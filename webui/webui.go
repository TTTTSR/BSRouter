// Package webui 内嵌前端构建产物(webui/BSRouterWebUI/dist),由网关以静态资源提供。
// 注意:go:embed 要求 dist 已存在——构建前端后(`cd webui/BSRouterWebUI && npm run build`)
// 再执行 go build。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed BSRouterWebUI/dist
var dist embed.FS

// Handler 返回内嵌前端静态资源处理器(入口为 index.html,页面自身无客户端路由)。
// 不提供目录列表:目录请求(非根)一律 404。
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "BSRouterWebUI/dist")
	if err != nil {
		panic("webui: embedded dist unavailable: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 拒绝目录列表:解析到目录(含 /assets 与 /assets/)一律 404;根路径走 index.html。
		p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if p != "" {
			if st, err := fs.Stat(sub, p); err == nil && st.IsDir() {
				http.NotFound(w, r)
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}
