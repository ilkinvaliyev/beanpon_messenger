package services

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

type EncryptionService struct {
	aesKey []byte
}

// NewEncryptionService yeni şifreleme servisi oluştur
func NewEncryptionService(aesKey string) *EncryptionService {
	return &EncryptionService{
		aesKey: []byte(aesKey), // 32 karakterlik key
	}
}

// EncryptMessage mesajı AES256 ile şifrele
func (e *EncryptionService) EncryptMessage(plaintext string) (string, error) {
	// AES cipher oluştur
	block, err := aes.NewCipher(e.aesKey)
	if err != nil {
		return "", err
	}

	// GCM mode kullan (güvenli)
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	// Rastgele nonce oluştur
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	// Şifrele
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)

	// Base64 encode et
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptMessage şifrelenmiş mesajı çöz
func (e *EncryptionService) DecryptMessage(encryptedText string) (string, error) {
	// Base64 decode
	ciphertext, err := base64.StdEncoding.DecodeString(encryptedText)
	if err != nil {
		return "", err
	}

	// AES cipher oluştur
	block, err := aes.NewCipher(e.aesKey)
	if err != nil {
		return "", err
	}

	// GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	// Nonce boyutunu kontrol et
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", errors.New("şifrelenmiş metin çok kısa")
	}

	// Nonce ve ciphertext'i ayır
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]

	// Şifreyi çöz
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}
