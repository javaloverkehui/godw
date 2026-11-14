package cryptox_test

import (
	"crypto/cipher"
	"errors"
	"strings"
	"testing"

	"github.com/godw/godw/internal/pkg/cryptox"
)

type credential struct {
	Host     string `json:"host"`
	User     string `json:"user"`
	Password string `json:"password"`
}

const testKey = "0123456789abcdef-主密钥"

// failingReader 始终返回错误，用于验证熵池不可用时的错误透传。
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("熵池不可用") }

// 守住主密钥最小长度：短密钥（如空串、示例占位符）必须在启动时就报错，
// 否则会用低熵密钥静默加密全部数据源凭证。
func TestNewRequiresMinKeyLength(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"空串", "", true},
		{"15 字节边界", strings.Repeat("a", 15), true},
		{"16 字节边界", strings.Repeat("a", 16), false},
		{"中文多字节按字节计", "主密钥主密钥主密钥主密钥主密钥", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := cryptox.New(tc.key)
			if tc.wantErr {
				if err == nil {
					t.Fatal("短密钥应返回错误")
				}
				if !strings.Contains(err.Error(), "主密钥至少 16 字节") {
					t.Fatalf("错误信息不符: %v", err)
				}
				if c != nil {
					t.Fatal("失败时不应返回 Cipher")
				}
				return
			}
			if err != nil {
				t.Fatalf("合法密钥不应报错: %v", err)
			}
			if c == nil {
				t.Fatal("成功时应返回 Cipher")
			}
		})
	}
}

// 守住往返一致性：字段一个都不能丢，否则数据源连接会因缺少密码而失败。
func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, err := cryptox.New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := credential{Host: "db.local:5432", User: "godw", Password: "p@ss<>&"}

	ct, err := c.Encrypt(want)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(ct) == 0 {
		t.Fatal("密文不应为空")
	}
	if strings.Contains(string(ct), "p@ss") {
		t.Fatal("密文中不应出现明文")
	}

	var got credential
	if err := c.Decrypt(ct, &got); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != want {
		t.Fatalf("往返结果 = %#v，期望 %#v", got, want)
	}
}

// 守住每条密文独立 nonce：相同明文两次加密必须不同，
// 复用 nonce 会让 GCM 泄漏明文异或值。
func TestEncryptUsesFreshNonce(t *testing.T) {
	c, err := cryptox.New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, err := c.Encrypt(map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	second, err := c.Encrypt(map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if string(first) == string(second) {
		t.Fatal("相同明文两次加密结果不应相同")
	}
}

// 守住空密文的零值语义：数据库里未配置凭证时读出来是空字节，
// 必须按「保持目标为零值」处理，不能报错。
func TestDecryptEmptyLeavesTargetUntouched(t *testing.T) {
	c, err := cryptox.New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := credential{Host: "保持原值"}
	if err := c.Decrypt(nil, &got); err != nil {
		t.Fatalf("Decrypt(nil) 不应报错: %v", err)
	}
	if got.Host != "保持原值" {
		t.Fatalf("空密文不应改写目标，实际 %#v", got)
	}
}

// 守住短密文报错：截断的字段（小于 nonce 长度）必须显式报错，
// 直接切片会 panic 在请求处理路径里。AES-GCM 的 nonce 固定 12 字节，
// 这里覆盖 11 的边界。
func TestDecryptTruncatedCiphertext(t *testing.T) {
	c, err := cryptox.New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, n := range []int{1, 5, 11} {
		var out credential
		err := c.Decrypt(make([]byte, n), &out)
		if err == nil {
			t.Fatalf("长度 %d 的密文应报错", n)
		}
		if !strings.Contains(err.Error(), "密文长度不足") {
			t.Fatalf("长度 %d 的错误信息不符: %v", n, err)
		}
	}
}

// 守住换密钥后的可诊断性：解密失败必须带「主密钥可能已更换」提示，
// 不能把 GCM 的原始错误直接抛给用户。
func TestDecryptWithWrongKey(t *testing.T) {
	enc, err := cryptox.New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dec, err := cryptox.New("another-key-material")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ct, err := enc.Encrypt(credential{Password: "secret"})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	var out credential
	err = dec.Decrypt(ct, &out)
	if err == nil {
		t.Fatal("换密钥后解密应失败")
	}
	if !strings.Contains(err.Error(), "解密失败（主密钥可能已更换）") {
		t.Fatalf("错误信息不符: %v", err)
	}
}

// 守住篡改检测：改动密文任意字节必须被 GCM 认证拒绝。
func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	c, err := cryptox.New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ct, err := c.Encrypt(credential{Password: "secret"})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct[len(ct)-1] ^= 0xff
	var out credential
	if err := c.Decrypt(ct, &out); err == nil {
		t.Fatal("篡改后的密文应解密失败")
	}
}

// 守住不可序列化输入的错误返回：Encrypt 不能吞掉 json 错误产出空密文。
func TestEncryptMarshalError(t *testing.T) {
	c, err := cryptox.New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Encrypt(make(chan int)); err == nil {
		t.Fatal("chan 序列化应返回错误")
	}
}

// 守住目标类型不匹配的错误返回：密钥正确但调用方传错结构体时，
// Decrypt 必须把 json 错误抛出去，而不是返回零值成功。
func TestDecryptRejectsIncompatibleTarget(t *testing.T) {
	c, err := cryptox.New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ct, err := c.Encrypt(map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	var n int
	if err := c.Decrypt(ct, &n); err == nil {
		t.Fatal("对象密文解到 int 应返回错误")
	}
}

// 守住构造阶段的错误透传：aes.NewCipher 与 cipher.NewGCM 失败时
// 必须返回错误且不返回半成品 Cipher。
func TestNewPropagatesConstructionErrors(t *testing.T) {
	oldBlock, oldAEAD := cryptox.NewBlockCipher, cryptox.NewAEAD
	t.Cleanup(func() {
		cryptox.NewBlockCipher, cryptox.NewAEAD = oldBlock, oldAEAD
	})

	cryptox.NewBlockCipher = func([]byte) (cipher.Block, error) {
		return nil, errors.New("block 构造失败")
	}
	if c, err := cryptox.New(testKey); err == nil || c != nil || !strings.Contains(err.Error(), "block 构造失败") {
		t.Fatalf("NewCipher 失败应透传错误，实际 c=%v err=%v", c, err)
	}

	cryptox.NewBlockCipher = oldBlock
	cryptox.NewAEAD = func(cipher.Block) (cipher.AEAD, error) {
		return nil, errors.New("gcm 构造失败")
	}
	if c, err := cryptox.New(testKey); err == nil || c != nil || !strings.Contains(err.Error(), "gcm 构造失败") {
		t.Fatalf("NewGCM 失败应透传错误，实际 c=%v err=%v", c, err)
	}
}

// 守住熵池故障：随机源不可用时 Encrypt 必须返回错误，
// 绝不能退化成固定 nonce 继续加密。
func TestEncryptFailsWhenRandomSourceBroken(t *testing.T) {
	old := cryptox.RandReader
	t.Cleanup(func() { cryptox.RandReader = old })
	cryptox.RandReader = failingReader{}

	c, err := cryptox.New(testKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Encrypt(credential{Password: "x"}); err == nil {
		t.Fatal("随机源失败时 Encrypt 应报错")
	}
}
