package migrations

import "embed"

// FS 嵌入全部迁移脚本，二进制自带 schema，无需额外分发 SQL 文件。
//
//go:embed *.sql
var FS embed.FS
