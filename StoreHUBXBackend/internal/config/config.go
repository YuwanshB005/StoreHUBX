package config

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Port           string
	MongoURI       string
	JWTSecret      string
	GithubClientID string
	GithubSecret   string
	GithubRedirect string
	FrontendURL    string
	// PublicURL is the API's own externally reachable base URL
	// (API_PUBLIC_URL), used to build absolute /bundles links.
	PublicURL string
	// CORSOrigins is the comma-separated list of browser origins allowed to
	// call the API (CORS_ALLOW_ORIGINS, default FrontendURL).
	CORSOrigins string
}

var AppConfig *Config

// LoadConfig loads .env vars into AppConfig and refuses to start with
// missing or weak secrets.
func LoadConfig() {
	_ = godotenv.Load() // loads from .env if present

	frontend := strings.TrimRight(getEnv("FRONTEND_URL", "http://localhost:3000"), "/")
	AppConfig = &Config{
		Port:           getEnv("PORT", "8080"),
		MongoURI:       getEnv("MONGO_URI", ""),
		JWTSecret:      getEnv("JWT_SECRET", ""),
		GithubClientID: getEnv("GITHUB_CLIENT_ID", ""),
		GithubSecret:   getEnv("GITHUB_CLIENT_SECRET", ""),
		GithubRedirect: getEnv("GITHUB_REDIRECT_URL", ""),
		FrontendURL:    frontend,
		PublicURL:      strings.TrimRight(getEnv("API_PUBLIC_URL", "http://localhost:"+getEnv("PORT", "8080")), "/"),
		CORSOrigins:    getEnv("CORS_ALLOW_ORIGINS", frontend),
	}
	if err := Validate(AppConfig, os.Getenv("TOKEN_ENC_KEY")); err != nil {
		log.Fatal("config: ", err)
	}
}

// Validate reports configuration that would make the app insecure or
// broken. An empty JWT_SECRET used to be accepted silently, which means
// anyone could forge login tokens.
func Validate(c *Config, tokenEncKey string) error {
	if c.MongoURI == "" {
		return fmt.Errorf("MONGO_URI not set")
	}
	if len(c.JWTSecret) < 32 {
		return fmt.Errorf("JWT_SECRET must be at least 32 characters (generate one with: openssl rand -base64 48)")
	}
	if len(tokenEncKey) != 32 {
		return fmt.Errorf("TOKEN_ENC_KEY must be exactly 32 bytes (generate one with: openssl rand -hex 16)")
	}
	return nil
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
