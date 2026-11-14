package auth_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/godw/godw/internal/auth"
)

const testSecret = "unit-test-secret-0123456789"

// forge 用已知密钥手工拼一个 token，payload 原样写入（可为非法 base64/JSON）。
// 这样无需导出内部实现也能覆盖验签通过后的解码错误分支。
func forge(secret, payload string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := header + "." + payload
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// 守住签发/校验往返：同密钥必须能还原 claims，且过期时间按 ttl 计算。
func TestSignVerifyRoundTrip(t *testing.T) {
	s := auth.NewSigner(testSecret, time.Hour)
	tok, err := s.Sign("user_1", "ws_1", "editor")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if n := len(strings.Split(tok, ".")); n != 3 {
		t.Fatalf("JWT 应为 3 段，实际 %d", n)
	}
	c, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if c.Sub != "user_1" || c.WorkspaceID != "ws_1" || c.Role != "editor" {
		t.Fatalf("claims 不一致: %+v", c)
	}
	// iat/exp 相差 ttl，且 exp 在未来。
	if c.Exp-c.Iat != 3600 {
		t.Fatalf("exp-iat = %d，期望 3600", c.Exp-c.Iat)
	}
	if c.Exp <= time.Now().Unix() {
		t.Fatalf("exp 应在未来，实际 %d", c.Exp)
	}
}

// 守住 token 格式校验：段数不对必须直接拒绝，不能数组越界 panic。
func TestVerifyMalformedShape(t *testing.T) {
	s := auth.NewSigner(testSecret, time.Hour)
	cases := []string{
		"",
		"only-one-part",
		"two.parts",
		"a.b.c.d",
	}
	for _, tok := range cases {
		if _, err := s.Verify(tok); err == nil {
			t.Errorf("%q 应被拒绝", tok)
		} else if !strings.Contains(err.Error(), "格式错误") {
			t.Errorf("%q 错误信息不符: %v", tok, err)
		}
	}
}

// 守住篡改检测：改 payload 或换密钥都必须验签失败，不能只靠 base64 解码。
func TestVerifyTampered(t *testing.T) {
	s := auth.NewSigner(testSecret, time.Hour)
	tok, err := s.Sign("u", "w", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	// 替换 payload 但保留原签名
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"evil"}`)) + "." + parts[2]
	if _, err := s.Verify(tampered); err == nil || !strings.Contains(err.Error(), "签名不匹配") {
		t.Fatalf("篡改 payload 应验签失败: %v", err)
	}
	// 换密钥
	other := auth.NewSigner("another-secret-9876543210", time.Hour)
	if _, err := other.Verify(tok); err == nil || !strings.Contains(err.Error(), "签名不匹配") {
		t.Fatalf("异密钥应验签失败: %v", err)
	}
}

// 守住验签通过后的解码分支：非法 base64 与非法 JSON 都必须返回错误而不是空 claims。
func TestVerifyDecodeErrors(t *testing.T) {
	s := auth.NewSigner(testSecret, time.Hour)

	// "!!!" 不是合法 RawURLEncoding 字符。
	badB64 := forge(testSecret, "!!!")
	if _, err := s.Verify(badB64); err == nil {
		t.Fatal("非法 base64 payload 应报错")
	}

	// 合法 base64 但不是 JSON 对象。
	badJSON := forge(testSecret, base64.RawURLEncoding.EncodeToString([]byte("not-json")))
	if _, err := s.Verify(badJSON); err == nil {
		t.Fatal("非法 JSON payload 应报错")
	}
}

// 守住过期判定：exp 早于当前时间必须拒绝，等于当前时间也拒绝（严格大于）。
func TestVerifyExpired(t *testing.T) {
	s := auth.NewSigner(testSecret, -time.Minute)
	tok, err := s.Sign("u", "w", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(tok); err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("过期 token 应被拒绝: %v", err)
	}
}

// 守住口令哈希：同一明文两次哈希不同（加盐），校验只认原明文。
func TestPasswordHashAndCheck(t *testing.T) {
	h1, err := auth.HashPassword("s3cret-pass")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	h2, err := auth.HashPassword("s3cret-pass")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if h1 == h2 {
		t.Fatal("bcrypt 必须带随机盐，两次哈希不应相同")
	}
	if !auth.CheckPassword(h1, "s3cret-pass") {
		t.Fatal("正确口令应校验通过")
	}
	if auth.CheckPassword(h1, "wrong-pass") {
		t.Fatal("错误口令不应通过")
	}
	if auth.CheckPassword("not-a-bcrypt-hash", "s3cret-pass") {
		t.Fatal("非法哈希不应通过")
	}
}

// 守住展示层脱敏：短串全遮，长串只露首尾各 2 个字符（按 rune 而非字节）。
func TestMask(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "****"},
		{"abc", "****"},
		{"abcd", "****"},
		{"abcde", "ab****de"},
		{"13800001111", "13****11"},
		// 多字节字符不能被按字节切断成乱码。
		{"密码密码密", "密码****码密"},
	}
	for _, c := range cases {
		if got := auth.Mask(c.in); got != c.want {
			t.Errorf("Mask(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
