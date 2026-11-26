package auth

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yuwanshb005/storehubx/internal/config"
)

// Every token carries an audience ("aud") naming the one place it may be
// used. A token minted for previews can never pass as an API login (and
// vice versa), even though both are signed with the same secret.
const (
	AudienceAPI     = "storehubx-api"
	AudiencePreview = "storehubx-preview"
	AudienceBundle  = "storehubx-bundle"
)

func keyFunc(*jwt.Token) (interface{}, error) {
	return []byte(config.AppConfig.JWTSecret), nil
}

// parse validates signature, expiry, the HS256 algorithm (no algorithm
// switching) and the expected audience, returning the claims.
func parse(tokenString, audience string) (jwt.MapClaims, bool) {
	token, err := jwt.Parse(tokenString, keyFunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !token.Valid {
		return nil, false
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	return claims, ok
}

func sign(claims jwt.MapClaims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(config.AppConfig.JWTSecret))
}

// GenerateJWT mints the 72h API login token.
func GenerateJWT(userID, email string) (string, error) {
	now := time.Now()
	return sign(jwt.MapClaims{
		"user_id": userID,
		"email":   email,
		"aud":     AudienceAPI,
		"exp":     now.Add(72 * time.Hour).Unix(),
		"iat":     now.Unix(),
	})
}

// VerifyAPIToken validates an API login token and returns its identity.
func VerifyAPIToken(tokenString string) (userID, email string, ok bool) {
	claims, ok := parse(tokenString, AudienceAPI)
	if !ok {
		return "", "", false
	}
	userID, _ = claims["user_id"].(string)
	email, _ = claims["email"].(string)
	return userID, email, userID != ""
}

// GeneratePreviewToken mints a short-lived (2 minute) token scoped to a single
// component version's preview, for embedding as ?token= on
// GET /preview/:slug/:version — an iframe src can't carry an Authorization
// header, so this lets an owner/collaborator prove identity for a PRIVATE
// component's preview without exposing the 72h API token in a URL.
func GeneratePreviewToken(userID, slug, version string) (string, error) {
	now := time.Now()
	return sign(jwt.MapClaims{
		"user_id": userID,
		"aud":     AudiencePreview,
		"slug":    slug,
		"version": version,
		"exp":     now.Add(2 * time.Minute).Unix(),
		"iat":     now.Unix(),
	})
}

// VerifyPreviewToken checks a preview token is valid and scoped to exactly
// this slug/version. Any failure reports ok == false.
func VerifyPreviewToken(tokenString, slug, version string) (userID string, ok bool) {
	claims, ok := parse(tokenString, AudiencePreview)
	if !ok {
		return "", false
	}
	if s, _ := claims["slug"].(string); s != slug {
		return "", false
	}
	if v, _ := claims["version"].(string); v != version {
		return "", false
	}
	uid, _ := claims["user_id"].(string)
	return uid, uid != ""
}

// GenerateBundleToken mints the capability that lets a browser load a
// private component's built files. It travels in the URL path
// (/bundles/t/<token>/components/...), so the bundle's relative asset URLs
// carry it automatically. Scoped to one slug/version, 30 minutes.
func GenerateBundleToken(userID, slug, version string) (string, error) {
	now := time.Now()
	return sign(jwt.MapClaims{
		"user_id": userID,
		"aud":     AudienceBundle,
		"slug":    slug,
		"version": version,
		"exp":     now.Add(30 * time.Minute).Unix(),
		"iat":     now.Unix(),
	})
}

// VerifyBundleToken checks a bundle token for exactly this slug/version.
func VerifyBundleToken(tokenString, slug, version string) (userID string, ok bool) {
	claims, ok := parse(tokenString, AudienceBundle)
	if !ok {
		return "", false
	}
	if s, _ := claims["slug"].(string); s != slug {
		return "", false
	}
	if v, _ := claims["version"].(string); v != version {
		return "", false
	}
	uid, _ := claims["user_id"].(string)
	return uid, uid != ""
}
