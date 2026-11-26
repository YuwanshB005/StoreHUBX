package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/yuwanshb005/storehubx/internal/config"
	"github.com/yuwanshb005/storehubx/internal/db"
	"github.com/yuwanshb005/storehubx/internal/utils"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const oauthStateCookie = "oauth_state"

// Redirect user to GitHub OAuth with required scopes.
//
// A random `state` value goes both into the GitHub URL and an HttpOnly
// cookie; the callback only proceeds when the two match. Without it, an
// attacker could send a victim a callback link carrying the attacker's own
// code and silently log the victim into the attacker's account (login CSRF).
func GitHubLogin(c *fiber.Ctx) error {
	clientID := os.Getenv("GITHUB_CLIENT_ID")
	if clientID == "" {
		return utils.Error(c, 500, "missing GitHub client ID")
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return utils.Error(c, 500, "failed to create login state")
	}
	state := hex.EncodeToString(buf)
	c.Cookie(&fiber.Cookie{
		Name:     oauthStateCookie,
		Value:    state,
		Path:     "/auth/github",
		MaxAge:   600,
		HTTPOnly: true,
		Secure:   c.Protocol() == "https",
		SameSite: fiber.CookieSameSiteLaxMode,
	})

	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("scope", strings.Join([]string{"user:email", "read:user", "repo", "read:org"}, " "))
	q.Set("state", state)
	return c.Redirect("https://github.com/login/oauth/authorize?" + q.Encode())
}

// GitHub callback: exchange code → token → user → save → JWT
// GitHub callback: exchange code → token → user → save → JWT
func GitHubCallback(c *fiber.Ctx) error {
	state := c.Query("state")
	expected := c.Cookies(oauthStateCookie)
	c.ClearCookie(oauthStateCookie)
	if state == "" || expected == "" || subtle.ConstantTimeCompare([]byte(state), []byte(expected)) != 1 {
		return utils.Error(c, 400, "invalid or expired login state, please try logging in again")
	}

	code := c.Query("code")
	if code == "" {
		return utils.Error(c, 400, "missing code parameter")
	}

	if db.Client == nil {
		return utils.Error(c, 500, "database not initialized")
	}
	if os.Getenv("TOKEN_ENC_KEY") == "" {
		return utils.Error(c, 500, "encryption key not configured")
	}

	clientID := os.Getenv("GITHUB_CLIENT_ID")
	clientSecret := os.Getenv("GITHUB_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return utils.Error(c, 500, "GitHub client credentials not configured")
	}

	// --- 1) Exchange code -> token (form-encoded) ---
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)

	req, err := http.NewRequest("POST", "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return utils.Error(c, 500, "failed to create token request: "+err.Error())
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return utils.Error(c, 500, "GitHub token exchange failed: "+err.Error())
	}
	defer resp.Body.Close()

	var tokenResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return utils.Error(c, 500, "failed to decode token response: "+err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		if desc, ok := tokenResp["error_description"].(string); ok && desc != "" {
			return utils.Error(c, 400, "GitHub token exchange error: "+desc)
		}
		if errMsg, ok := tokenResp["error"].(string); ok && errMsg != "" {
			return utils.Error(c, 400, "GitHub token exchange error: "+errMsg)
		}
		return utils.Error(c, 400, fmt.Sprintf("GitHub token exchange failed (%d)", resp.StatusCode))
	}

	var token string
	if at, ok := tokenResp["access_token"].(string); ok && at != "" {
		token = at
	}
	if token == "" {
		return utils.Error(c, 400, "invalid access token response")
	}

	// --- 2) Fetch user ---
	client := &http.Client{}
	reqUser, _ := http.NewRequest("GET", "https://api.github.com/user", nil)
	reqUser.Header.Set("Authorization", "Bearer "+token)
	reqUser.Header.Set("Accept", "application/vnd.github+json")
	resUser, err := client.Do(reqUser)
	if err != nil {
		return utils.Error(c, 500, "failed to fetch GitHub user: "+err.Error())
	}
	defer resUser.Body.Close()
	if resUser.StatusCode != http.StatusOK {
		var errBody map[string]any
		_ = json.NewDecoder(resUser.Body).Decode(&errBody)
		return utils.Error(c, 401, "GitHub returned non-200 for /user")
	}
	var ghUser map[string]interface{}
	if err := json.NewDecoder(resUser.Body).Decode(&ghUser); err != nil {
		return utils.Error(c, 500, "failed to decode GitHub user response: "+err.Error())
	}

	// --- 3) Fetch emails (optional) ---
	reqEmail, _ := http.NewRequest("GET", "https://api.github.com/user/emails", nil)
	reqEmail.Header.Set("Authorization", "Bearer "+token)
	reqEmail.Header.Set("Accept", "application/vnd.github+json")
	resEmail, err := client.Do(reqEmail)
	if err != nil {
		return utils.Error(c, 500, "failed to fetch GitHub emails: "+err.Error())
	}
	defer resEmail.Body.Close()

	var emails []map[string]interface{}
	if resEmail.StatusCode == http.StatusOK {
		_ = json.NewDecoder(resEmail.Body).Decode(&emails)
	}

	// --- 4) Normalize fields + fallbacks ---
	email := ""
	for _, e := range emails {
		if primary, ok := e["primary"].(bool); ok && primary {
			if val, ok := e["email"].(string); ok && val != "" {
				email = val
				break
			}
		}
	}
	// derive providerID once and reuse everywhere
	providerID := providerIDFrom(ghUser) // e.g. "136720020"
	if email == "" {
		if providerID != "" {
			email = providerID + "@users.noreply.github.com"
		} else {
			email = "unknown@users.noreply.github.com"
		}
	}
	name, _ := ghUser["name"].(string)
	login, _ := ghUser["login"].(string)
	avatar, _ := ghUser["avatar_url"].(string)
	if name == "" {
		name = login
	}

	// --- 5) Encrypt token ---
	encToken := utils.Encrypt(token)
	if encToken == "" {
		return utils.Error(c, 500, "failed to encrypt token")
	}

	// --- 6) Upsert user (use the SAME providerID everywhere) ---
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	col := db.DB().Collection("users")

	update := bson.M{
		"$set": bson.M{
			"name":        name,
			"email":       email,
			"username":    login,
			"avatarUrl":   avatar,
			"provider":    "github",
			"providerId":  providerID, // ✅ normalized value
			"accessToken": encToken,
			"updatedAt":   time.Now(),
		},
		"$setOnInsert": bson.M{"createdAt": time.Now()},
	}
	opts := options.Update().SetUpsert(true)
	if _, err := col.UpdateOne(ctx, bson.M{"providerId": providerID}, update, opts); err != nil {
		return utils.Error(c, 500, "database upsert failed: "+err.Error())
	}

	// --- 7) JWT: user_id = providerId, email = email ---
	jwtToken, err := GenerateJWT(providerID, email)
	if err != nil {
		return utils.Error(c, 500, "failed to generate JWT: "+err.Error())
	}

	// return c.JSON(fiber.Map{
	// 	"status": "success",
	// 	"token":  jwtToken,
	// 	"user": fiber.Map{
	// 		"name":       name,
	// 		"email":      email,
	// 		"username":   login,
	// 		"avatar_url": avatar,
	// 	},
	// })

	// Get frontend URL from environment variable, defaulting to localhost:3000 for development
	frontendURL := config.AppConfig.FrontendURL

	// Create a user data object to pass along with the token. "id" is the
	// providerId (same value as JWT's user_id claim and Component.OwnerID) —
	// every ownership check in the frontend (isOwner, currentUserId) compares
	// against this, not the Mongo _id, so it must match here.
	userData, err := json.Marshal(map[string]interface{}{
		"id":         providerID,
		"providerId": providerID,
		"name":       name,
		"email":      email,
		"username":   login,
		"avatarUrl":  avatar,
	})
	if err != nil {
		return utils.Error(c, 500, "failed to marshal user data: "+err.Error())
	}

	// Base64 encode the user data (for atob() in frontend)
	encodedUserData := base64.StdEncoding.EncodeToString(userData)

	// Hand the token to the frontend in the URL fragment (#), not the query
	// string: a fragment is never sent to any server, so the 72h token can't
	// leak into server/proxy logs or Referer headers.
	frag := url.Values{}
	frag.Set("token", jwtToken)
	frag.Set("user", encodedUserData)
	return c.Redirect(fmt.Sprintf("%s/auth/callback#%s", frontendURL, frag.Encode()))
}

func providerIDFrom(gh map[string]interface{}) string {
	switch v := gh["id"].(type) {
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case json.Number:
		i, _ := v.Int64()
		return strconv.FormatInt(i, 10)
	case string:
		return v
	default:
		return ""
	}
}
