package ids_test

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/pkg/ids"
)

// crockfordAlphabet 与实现约定一致：26 位 ID 只允许出现这些字符。
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// 守住 ULID 的形态契约：26 位、只用 Crockford Base32 字典。
// 长度或字典错了会让按 ID 前缀扫描的查询失效。
func TestULIDShape(t *testing.T) {
	id := ids.ULID()
	if len(id) != 26 {
		t.Fatalf("ULID 长度 = %d，期望 26（%q）", len(id), id)
	}
	for i, c := range id {
		if !strings.ContainsRune(crockfordAlphabet, c) {
			t.Fatalf("第 %d 位字符 %q 不在 Crockford 字典内: %q", i, c, id)
		}
	}
}

// 守住单调可排序：不同毫秒生成的 ID 必须按字典序递增，
// 否则按 ID 排序取最新实例会取错。
func TestULIDSortsByTime(t *testing.T) {
	first := ids.ULID()
	time.Sleep(5 * time.Millisecond)
	second := ids.ULID()
	if second <= first {
		t.Fatalf("后生成的 ULID 应更大: %q vs %q", second, first)
	}
}

// 守住随机段的唯一性：连续生成不能重复。
func TestULIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		id := ids.ULID()
		if _, ok := seen[id]; ok {
			t.Fatalf("ULID 重复: %q", id)
		}
		seen[id] = struct{}{}
	}
}

// 守住 Short 的长度约定与 n<=0 的默认值：默认值写错会让幂等键后缀长度漂移。
func TestShort(t *testing.T) {
	cases := []struct {
		name    string
		n       int
		wantLen int
	}{
		{"显式长度", 4, 8},
		{"单字节", 1, 2},
		{"零走默认 8 字节", 0, 16},
		{"负数走默认 8 字节", -5, 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ids.Short(tc.n)
			if len(got) != tc.wantLen {
				t.Fatalf("Short(%d) 长度 = %d，期望 %d（%q）", tc.n, len(got), tc.wantLen, got)
			}
			if _, err := hex.DecodeString(got); err != nil {
				t.Fatalf("Short 输出应为十六进制: %q (%v)", got, err)
			}
		})
	}
}

// 守住随机性：连续两次 Short 不能相同。
func TestShortUnique(t *testing.T) {
	if ids.Short(8) == ids.Short(8) {
		t.Fatal("两次 Short(8) 不应相同")
	}
}

// 守住 New 的前缀拼接格式：漏掉下划线会破坏按前缀区分的路由与索引。
func TestNewPrefix(t *testing.T) {
	got := ids.New("ds")
	if !strings.HasPrefix(got, "ds_") {
		t.Fatalf("New(\"ds\") = %q，应以 ds_ 开头", got)
	}
	if len(got) != len("ds_")+26 {
		t.Fatalf("New 长度 = %d，期望 %d", len(got), len("ds_")+26)
	}
	if got == ids.New("ds") {
		t.Fatal("两次 New 不应相同")
	}
	if empty := ids.New(""); !strings.HasPrefix(empty, "_") {
		t.Fatalf("空前缀应产生 _ULID，实际 %q", empty)
	}
}

// 守住随机源降级：crypto/rand 不可用时 ULID 必须退化为纳秒填充而不是 panic，
// 同时仍要产出合法的 26 位 ID，否则加密子系统故障会连带拖垮建实例流程。
func TestULIDFallsBackWhenRandomFails(t *testing.T) {
	old := ids.RandRead
	t.Cleanup(func() { ids.RandRead = old })
	ids.RandRead = func([]byte) (int, error) { return 0, errors.New("熵池不可用") }

	id := ids.ULID()
	if len(id) != 26 {
		t.Fatalf("降级后 ULID 长度 = %d，期望 26（%q）", len(id), id)
	}
	for i, c := range id {
		if !strings.ContainsRune(crockfordAlphabet, c) {
			t.Fatalf("降级后第 %d 位字符 %q 不在字典内: %q", i, c, id)
		}
	}
}

// 守住 Short 的降级路径：随机源失败时退回纳秒十六进制，
// 返回值必须仍是合法十六进制且非空，调用方会直接拼进幂等键。
func TestShortFallsBackWhenRandomFails(t *testing.T) {
	old := ids.RandRead
	t.Cleanup(func() { ids.RandRead = old })
	ids.RandRead = func([]byte) (int, error) { return 0, errors.New("熵池不可用") }

	got := ids.Short(8)
	if got == "" {
		t.Fatal("降级后 Short 不应返回空串")
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("降级后 Short 应为十六进制: %q (%v)", got, err)
	}
}
