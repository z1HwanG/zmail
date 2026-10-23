// Package secret 提供基于 AES-256-GCM 的凭据加解密。
//
// 存储格式： nonce(12B) || ciphertext || tag（GCM 输出已包含 tag）。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidCiphertext 表示密文长度不足或不完整。
var ErrInvalidCiphertext = errors.New("secret: 密文格式非法")

// Box 封装一个 AES-256-GCM 加解密器，可安全并发使用。
type Box struct {
	aead cipher.AEAD
}

// New 用 32 字节密钥构造 Box。
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secret: 密钥必须是 32 字节，当前 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: 初始化 AES 失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: 初始化 GCM 失败: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Encrypt 加密明文，返回 nonce||ciphertext。
func (b *Box) Encrypt(plain []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secret: 生成 nonce 失败: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(plain)+b.aead.Overhead())
	out = append(out, nonce...)
	out = b.aead.Seal(out, nonce, plain, nil)
	return out, nil
}

// EncryptString 加密字符串。
func (b *Box) EncryptString(plain string) ([]byte, error) { return b.Encrypt([]byte(plain)) }

// Decrypt 解密 nonce||ciphertext。
func (b *Box) Decrypt(enc []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(enc) < ns+b.aead.Overhead() {
		return nil, ErrInvalidCiphertext
	}
	nonce, ct := enc[:ns], enc[ns:]
	plain, err := b.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("secret: 解密失败（密钥不匹配或数据损坏）: %w", err)
	}
	return plain, nil
}

// DecryptString 解密为字符串。
func (b *Box) DecryptString(enc []byte) (string, error) {
	b2, err := b.Decrypt(enc)
	if err != nil {
		return "", err
	}
	return string(b2), nil
}

// GenerateKey 生成一个新的 32 字节随机密钥。
func GenerateKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	return key, nil
}
