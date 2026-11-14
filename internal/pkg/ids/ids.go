package ids

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// RandRead 是随机源入口，默认指向 crypto/rand.Read。
// 导出它只为让外部测试能模拟「熵池不可用」的降级路径，生产代码不应改写。
var RandRead = rand.Read

// ULID 生成 26 位字典序可排序 ID：前 10 位时间戳，后 16 位随机。
// 不引入第三方库，避免为一个函数增加依赖。
func ULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := RandRead(b[6:]); err != nil {
		// 随机源不可用时退化为纳秒填充，宁可重复也不 panic。
		binary.BigEndian.PutUint64(b[8:], uint64(time.Now().UnixNano()))
	}
	return encode(b[:])
}

func encode(src []byte) string {
	// 128 bit -> 26 字符：先取 2 位补齐，再每 5 bit 编码。
	var sb strings.Builder
	sb.Grow(26)
	bits := 0
	var acc uint32
	// 首字符只用低 3 bit（130 bit 编码中的对齐位）
	acc = uint32(src[0]) >> 5
	sb.WriteByte(crockford[acc])
	acc = uint32(src[0]) & 0x1f
	bits = 5
	for _, c := range src[1:] {
		acc = acc<<8 | uint32(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			sb.WriteByte(crockford[(acc>>uint(bits))&0x1f])
		}
	}
	if bits > 0 {
		sb.WriteByte(crockford[(acc<<uint(5-bits))&0x1f])
	}
	return sb.String()
}

// Short 生成 n 字节随机十六进制串，用于实例 ID、幂等键后缀。
func Short(n int) string {
	if n <= 0 {
		n = 8
	}
	b := make([]byte, n)
	if _, err := RandRead(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// New 生成带前缀的可读 ID，如 ds_01H...。
func New(prefix string) string {
	return prefix + "_" + ULID()
}
