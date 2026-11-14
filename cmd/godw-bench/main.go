// Package main 是 godw-bench 的入口，压测逻辑在 internal/bench。
package main

import (
	"os"

	"github.com/godw/godw/internal/bench"
)

func main() { os.Exit(bench.Run(os.Args[1:])) }
