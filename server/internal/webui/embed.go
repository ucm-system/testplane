// Package webui 把前端构建产物暴露成只读 fs.FS，供 api 包挂在 "/" 上。
//
// 规格：design/views/05-development-view.md §5.3（`//go:embed all:dist` + `fs.Sub`，
// 只暴露 fs.FS；`/api/`、`/healthz`、`/readyz`、`/metrics` 不参与 SPA fallback）。
//
// 产物由 `make web` 从 web/dist 复制过来（`go:embed` 不能跨模块目录）。
// 仓库里保留一个占位 index.html，保证没有前端产物时 `go build ./...` 也能过；
// 构建镜像时 `make web` 会把它覆盖成真实产物。
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS 返回以 web/dist 为根的只读文件系统。调用方负责 SPA fallback 与缓存头。
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// 目录常量写错属于编程错误，直接 panic，不要静默返回空 FS。
		panic("webui: 无法定位内嵌的 dist 目录: " + err.Error())
	}
	return sub
}
