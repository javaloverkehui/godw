// Package main 是 godw-server 的入口，启动逻辑在 internal/server。
package main

import (
	"os"

	"github.com/godw/godw/internal/server"
)

// 发布构建用 -ldflags "-X main.version=v1.0.0" 覆盖。
var version = "1.0.0"

func main() { os.Exit(server.Run(os.Args[1:], version)) }
