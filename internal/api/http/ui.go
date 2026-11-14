package http

import "embed"

// uiFS 内嵌控制台。不依赖 npm 构建链：下载二进制即可用完整界面，
// 也避免生产环境需要额外的静态资源服务。
//
//go:embed ui
var uiFS embed.FS
