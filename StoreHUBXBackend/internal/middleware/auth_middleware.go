package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/yuwanshb005/storehubx/internal/auth"
	"github.com/yuwanshb005/storehubx/internal/utils"
)

// parseBearerClaims extracts user_id/email from a "Bearer <jwt>" Authorization
// header. Only API-audience tokens are accepted: preview or bundle tokens are
// rejected even though they are signed with the same secret.
func parseBearerClaims(authHeader string) (userID, email string, ok bool) {
	tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
	if tokenStr == "" || tokenStr == authHeader {
		return "", "", false
	}
	return auth.VerifyAPIToken(tokenStr)
}

// JWTProtected requires a valid bearer JWT, rejecting the request if it's missing/invalid.
func JWTProtected(c *fiber.Ctx) error {
	userID, email, ok := parseBearerClaims(c.Get("Authorization"))
	if !ok {
		return utils.Error(c, fiber.StatusUnauthorized, "missing or invalid token")
	}
	c.Locals("user_id", userID)
	c.Locals("email", email)
	return c.Next()
}
