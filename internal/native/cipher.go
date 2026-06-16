package native

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
)

// Cipher 实现米家网关的 AES-GCM 加密/解密
//
// 与 gateway.js 的 Cipher 类完全对标：
// - 16 字节 key + 8 字节 salt
// - IV = salt(8) || counter(4 LE)
// - 128-bit tag
// - counter 从 1 开始递增
type Cipher struct {
	block    cipher.Block
	salt     []byte // 8 字节
	encCount uint32 // 加密计数器
	decCount uint32 // 解密计数器（防重放）
}

// NewCipher 创建 Cipher
// key: 16 字节, salt: 8 字节
func NewCipher(key, salt []byte) (*Cipher, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("key length must be 16, got %d", len(key))
	}
	if len(salt) != 8 {
		return nil, fmt.Errorf("salt length must be 8, got %d", len(salt))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return &Cipher{
		block:    block,
		salt:     append([]byte(nil), salt...),
		encCount: 1,
		decCount: 0,
	}, nil
}

// Encrypt 加密数据
// 输出格式：[counter(4 LE)] + ciphertext + tag(16)
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	if c.encCount > 4294967295 {
		return nil, fmt.Errorf("self counter overflow")
	}
	counter := c.encCount
	c.encCount++

	// 构造 IV: salt(8) || counter(4 LE)
	iv := make([]byte, 12)
	copy(iv, c.salt)
	binary.LittleEndian.PutUint32(iv[8:], counter)

	aesGCM, err := cipher.NewGCMWithNonceSize(c.block, 12)
	if err != nil {
		return nil, err
	}

	// Seal 输出 = ciphertext + tag
	sealed := aesGCM.Seal(nil, iv, plaintext, nil)

	// 最终输出：counter(4) + sealed
	result := make([]byte, 4+len(sealed))
	binary.LittleEndian.PutUint32(result[0:4], counter)
	copy(result[4:], sealed)
	return result, nil
}

// Decrypt 解密数据
// 输入格式：[counter(4 LE)] + ciphertext + tag(16)
func (c *Cipher) Decrypt(data []byte) ([]byte, error) {
	if len(data) < 4+16 {
		return nil, fmt.Errorf("data too short")
	}

	counter := binary.LittleEndian.Uint32(data[0:4])
	if counter <= c.decCount {
		return nil, fmt.Errorf("replay attack!")
	}
	c.decCount = counter

	// 构造 IV
	iv := make([]byte, 12)
	copy(iv, c.salt)
	binary.LittleEndian.PutUint32(iv[8:], counter)

	aesGCM, err := cipher.NewGCMWithNonceSize(c.block, 12)
	if err != nil {
		return nil, err
	}

	sealed := data[4:]
	plaintext, err := aesGCM.Open(nil, iv, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("authentication or decryption failed: %w", err)
	}
	return plaintext, nil
}
