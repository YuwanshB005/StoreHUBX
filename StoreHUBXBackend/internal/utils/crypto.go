package utils

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log"
	"os"
	"strings"
)

// GitHub access tokens are stored encrypted with TOKEN_ENC_KEY (32 bytes,
// AES-256).
//
// Current format: "v2:" + base64url(nonce || AES-GCM ciphertext+tag). GCM is
// authenticated, so a tampered or truncated value fails to decrypt instead
// of silently producing garbage.
//
// Legacy format (no prefix): base64url(iv || AES-CFB ciphertext). CFB has no
// integrity check. Decrypt still reads it so existing users keep working;
// their token is rewritten in v2 format on next login.
const gcmPrefix = "v2:"

func encKey() []byte {
	key := []byte(os.Getenv("TOKEN_ENC_KEY"))
	if len(key) != 32 {
		log.Println("TOKEN_ENC_KEY must be 32 bytes")
		return nil
	}
	return key
}

// Encrypt returns the v2 ciphertext, or "" on failure.
func Encrypt(text string) string {
	key := encKey()
	if key == nil {
		return ""
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return ""
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ""
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return ""
	}
	sealed := gcm.Seal(nonce, nonce, []byte(text), nil)
	return gcmPrefix + base64.URLEncoding.EncodeToString(sealed)
}

// Decrypt returns the plaintext, or "" if the value is invalid or tampered.
func Decrypt(cryptoText string) string {
	key := encKey()
	if key == nil {
		return ""
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return ""
	}

	if strings.HasPrefix(cryptoText, gcmPrefix) {
		raw, err := base64.URLEncoding.DecodeString(strings.TrimPrefix(cryptoText, gcmPrefix))
		if err != nil {
			return ""
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil || len(raw) < gcm.NonceSize() {
			return ""
		}
		plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
		if err != nil {
			return ""
		}
		return string(plain)
	}

	// Legacy AES-CFB value.
	raw, err := base64.URLEncoding.DecodeString(cryptoText)
	if err != nil || len(raw) < aes.BlockSize {
		return ""
	}
	iv, data := raw[:aes.BlockSize], raw[aes.BlockSize:]
	cipher.NewCFBDecrypter(block, iv).XORKeyStream(data, data)
	return string(data)
}
