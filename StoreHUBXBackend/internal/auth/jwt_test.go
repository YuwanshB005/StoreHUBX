package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yuwanshb005/storehubx/internal/config"
)

func init() {
	config.AppConfig = &config.Config{JWTSecret: "unit-test-secret-0123456789abcdef0123"}
}

func TestAPITokenRoundTrip(t *testing.T) {
	tok, _ := GenerateJWT("42", "a@b.c")
	uid, email, ok := VerifyAPIToken(tok)
	if !ok || uid != "42" || email != "a@b.c" {
		t.Fatalf("got %q %q %v", uid, email, ok)
	}
}

func TestTokensOnlyWorkForTheirAudience(t *testing.T) {
	api, _ := GenerateJWT("42", "a@b.c")
	preview, _ := GeneratePreviewToken("42", "btn", "1.0.0")

	if _, _, ok := VerifyAPIToken(preview); ok {
		t.Error("preview token accepted as API token")
	}
	if _, ok := VerifyPreviewToken(api, "btn", "1.0.0"); ok {
		t.Error("API token accepted as preview token")
	}
	if _, ok := VerifyPreviewToken(preview, "btn", "2.0.0"); ok {
		t.Error("preview token accepted for another version")
	}
	if uid, ok := VerifyPreviewToken(preview, "btn", "1.0.0"); !ok || uid != "42" {
		t.Error("valid preview token rejected")
	}
}

func TestRejectsOtherAlgorithmsAndExpiry(t *testing.T) {
	secret := []byte(config.AppConfig.JWTSecret)
	claims := jwt.MapClaims{"user_id": "42", "aud": AudienceAPI, "exp": time.Now().Add(time.Hour).Unix()}

	hs384, _ := jwt.NewWithClaims(jwt.SigningMethodHS384, claims).SignedString(secret)
	if _, _, ok := VerifyAPIToken(hs384); ok {
		t.Error("HS384 token accepted; only HS256 is allowed")
	}
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, _, ok := VerifyAPIToken(none); ok {
		t.Error("alg=none token accepted")
	}

	expired := jwt.MapClaims{"user_id": "42", "aud": AudienceAPI, "exp": time.Now().Add(-time.Minute).Unix()}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, expired).SignedString(secret)
	if _, _, ok := VerifyAPIToken(tok); ok {
		t.Error("expired token accepted")
	}

	noExp, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"user_id": "42", "aud": AudienceAPI}).SignedString(secret)
	if _, _, ok := VerifyAPIToken(noExp); ok {
		t.Error("token without expiry accepted")
	}
}
