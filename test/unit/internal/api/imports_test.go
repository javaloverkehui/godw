package api_test

// 连接器通过 init() 注册到 connector 注册表，生产入口（cmd/godw-server）用
// 匿名导入把驱动链上；测试二进制不经过 cmd，所以必须在这里补同样的导入。
// 只导 postgres：本套用例只打真实 PostgreSQL，多导其它驱动只会拖慢编译。
import (
	_ "github.com/godw/godw/internal/connector/postgres"
)
