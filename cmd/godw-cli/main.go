// Package main 是 godw-cli 的入口，命令实现全部在 cmd/internal/cli。
// 拆出独立包是为了让命令层用例能以黑盒方式驱动 Run，测试统一放在 test/ 下。
package main

import (
	"os"

	"github.com/godw/godw/internal/cli"
)

// 发布构建用 -ldflags "-X main.version=v1.0.0" 覆盖，源码里保留当前发布号。
var version = "1.0.0"

func main() { os.Exit(cli.Run(os.Args[1:], version)) }
