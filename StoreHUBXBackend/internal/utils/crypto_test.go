package utils

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"strings"
	"testing"
)

const testKey = "0123456789abcdef0123456789abcdef"

func TestEncryptRoundTrip(t *testing.T) {
	t.Setenv("TOKEN_ENC_KEY", testKey)
	enc := Encrypt("gho_secret_token")
	if !strings.HasPrefix(enc, "v2:") {
		t.Fatalf("expected v2 format, got %q", enc)
	}
	if got := Decrypt(enc); got != "gho_secret_token" {
		t.Fatalf("round trip = %q", got)
	}
	if Encrypt("same") == Encrypt("same") {
		t.Fatal("two encryptions of the same value must differ (random nonce)")
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	t.Setenv("TOKEN_ENC_KEY", testKey)
	raw, _ := base64.URLEncoding.DecodeString(strings.TrimPrefix(Encrypt("gho_secret_token"), "v2:"))
	raw[len(raw)-1] ^= 0x01
	if got := Decrypt("v2:" + base64.URLEncoding.EncodeToString(raw)); got != "" {
		t.Fatalf("tampered ciphertext decrypted to %q", got)
	}
}

func TestDecryptReadsLegacyCFB(t *testing.T) {
	t.Setenv("TOKEN_ENC_KEY", testKey)
	// Produce a value exactly as the previous implementation did.
	block, _ := aes.NewCipher([]byte(testKey))
	plain := []byte("gho_legacy_token")
	out := make([]byte, aes.BlockSize+len(plain))
	copy(out[:aes.BlockSize], []byte("fixed-iv-16bytes"))
	cipher.NewCFBEncrypter(block, out[:aes.BlockSize]).XORKeyStream(out[aes.BlockSize:], plain)
	legacy := base64.URLEncoding.EncodeToString(out)

	if got := Decrypt(legacy); got != "gho_legacy_token" {
		t.Fatalf("legacy decrypt = %q", got)
	}
}

func TestEncryptNeedsValidKey(t *testing.T) {
	t.Setenv("TOKEN_ENC_KEY", "too-short")
	if Encrypt("x") != "" || Decrypt("v2:abc") != "" {
		t.Fatal("expected failure with an invalid key")
	}
}
