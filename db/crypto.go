package db

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// 平台密钥的落库加密。
//
// 为什么在应用层加密而不是用数据库的加密能力：拿到数据库口令的人本来就能
// 读明文列；PG 的 TDE 之类要额外的服务端配置，reArtex 的部署形态不保证有。
// 应用层加密的边界很清楚：口令能读库，但没有主密钥就解不开。
//
// 密文格式：base64(nonce ‖ ciphertext ‖ tag)，nonce 每次随机。
// AAD 绑定行标识（profile 名）：防止有库写权限的人把 A 行的密文挪到 B 行，
// 再配上自己可控的 base_url，把 key 送到别处去 —— 那等于把钥匙送出墙。
// 代价：行改名后 AAD 变了，密文解不开，故改名必须重加密（见 reencryptProfileKey）。

// encryptSecret 用 AES-256-GCM 加密，返回 base64 密文。
func encryptSecret(plaintext string, key []byte, aad string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if len(key) != 32 {
		return "", fmt.Errorf("主密钥长度应为 32 字节，得到 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), []byte(aad))
	return base64.StdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// decryptSecret 解开 base64 密文。
func decryptSecret(ciphertext string, key []byte, aad string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if len(key) != 32 {
		return "", errors.New("主密钥长度应为 32 字节")
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("密文不是合法 base64: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("密文长度不足")
	}
	nonce, sealed := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, sealed, []byte(aad))
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥不对，或密文被改过）: %w", err)
	}
	return string(plain), nil
}
