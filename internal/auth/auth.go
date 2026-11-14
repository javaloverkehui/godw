package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Claims 是 JWT 载荷。只放必要信息，不放权限列表（权限从库里读，可即时撤销）。
type Claims struct {
	Sub         string `json:"sub"`
	WorkspaceID string `json:"ws"`
	Role        string `json:"role"`
	Exp         int64  `json:"exp"`
	Iat         int64  `json:"iat"`
}

// Signer 使用 HS256 签发与校验 token。不引入 JWT 库，减少供应链面。
type Signer struct {
	secret []byte
	ttl    time.Duration
}

func NewSigner(secret string, ttl time.Duration) *Signer {
	return &Signer{secret: []byte(secret), ttl: ttl}
}

func (s *Signer) Sign(userID, workspaceID, role string) (string, error) {
	now := time.Now()
	claims := Claims{Sub: userID, WorkspaceID: workspaceID, Role: role, Iat: now.Unix(), Exp: now.Add(s.ttl).Unix()}
	header := base64URL([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	body := header + "." + base64URL(payload)
	sig := s.sign(body)
	return body + "." + sig, nil
}

func (s *Signer) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("token 格式错误")
	}
	body := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(s.sign(body)), []byte(parts[2])) {
		return nil, fmt.Errorf("签名不匹配")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if time.Now().Unix() > c.Exp {
		return nil, fmt.Errorf("token 已过期")
	}
	return &c, nil
}

func (s *Signer) sign(body string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(body))
	return base64URL(mac.Sum(nil))
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// HashPassword 使用 bcrypt，cost 10 在安全与登录延迟之间取平衡。
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), 10)
	return string(b), err
}

func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// Mask 对敏感字段做展示层脱敏：保留前 2 位与后 2 位。
func Mask(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return "****"
	}
	return string(r[:2]) + "****" + string(r[len(r)-2:])
}
