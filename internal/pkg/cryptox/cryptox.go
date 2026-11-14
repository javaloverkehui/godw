package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
)

// Cipher 用 AES-256-GCM 加密数据源凭证。密钥由主密钥派生，
// 每条密文带独立 nonce，相同明文不会产生相同密文。
type Cipher struct {
	aead cipher.AEAD
}

// 加密原语入口，默认指向标准库实现。导出它们只为让外部测试能注入
// 构造/熵池故障（正常密钥长度下这些错误不可达），生产代码不应改写。
var (
	NewBlockCipher = aes.NewCipher
	NewAEAD        = cipher.NewGCM
	RandReader     io.Reader = rand.Reader
)

func New(keyMaterial string) (*Cipher, error) {
	if len(keyMaterial) < 16 {
		return nil, fmt.Errorf("cryptox: 主密钥至少 16 字节")
	}
	sum := sha256.Sum256([]byte(keyMaterial))
	block, err := NewBlockCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := NewAEAD(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func (c *Cipher) Encrypt(v any) ([]byte, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(RandReader, nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plain, nil), nil
}

func (c *Cipher) Decrypt(data []byte, out any) error {
	if len(data) == 0 {
		return nil
	}
	ns := c.aead.NonceSize()
	if len(data) < ns {
		return fmt.Errorf("cryptox: 密文长度不足")
	}
	plain, err := c.aead.Open(nil, data[:ns], data[ns:], nil)
	if err != nil {
		return fmt.Errorf("cryptox: 解密失败（主密钥可能已更换）: %w", err)
	}
	return json.Unmarshal(plain, out)
}
