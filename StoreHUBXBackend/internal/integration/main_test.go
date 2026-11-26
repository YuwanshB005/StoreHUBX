// Package integration runs the real HTTP stack (internal/server) against real
// MongoDB, Redis and MinIO. It uses its own database (MONGO_DB=storehub_test)
// and Redis DB 15, so it never touches development data.
//
// Run locally after `make infra`:   go test -p 1 ./...
// If MongoDB isn't reachable the package skips, unless
// STOREHUBX_REQUIRE_INFRA=1 (set in CI), in which case it fails.
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/yuwanshb005/storehubx/internal/cache"
	"github.com/yuwanshb005/storehubx/internal/config"
	"github.com/yuwanshb005/storehubx/internal/db"
	"github.com/yuwanshb005/storehubx/internal/server"
	"github.com/yuwanshb005/storehubx/internal/storage"
)

var app *fiber.App

func setDefault(key, val string) {
	if os.Getenv(key) == "" {
		os.Setenv(key, val)
	}
}

func TestMain(m *testing.M) {
	setDefault("MONGO_URI", "mongodb://localhost:27017")
	os.Setenv("MONGO_DB", "storehub_test")
	setDefault("REDIS_ADDR", "localhost:6379")
	os.Setenv("REDIS_DB", "15")
	os.Setenv("JWT_SECRET", "integration-test-secret-0123456789abcdef0123456789")
	os.Setenv("TOKEN_ENC_KEY", "0123456789abcdef0123456789abcdef")
	os.Setenv("RATE_LIMIT_PER_MIN", "1000000")
	setDefault("S3_ENDPOINT", "http://localhost:9000")
	setDefault("AWS_ACCESS_KEY_ID", "minioadmin")
	setDefault("AWS_SECRET_ACCESS_KEY", "minioadmin")
	os.Setenv("S3_BUCKET", "storehub-test")
	os.Setenv("S3_PUBLIC_BASE_URL", "http://localhost:9000/storehub-test")
	os.Setenv("FRONTEND_URL", "http://localhost:3000")

	if !mongoReachable(os.Getenv("MONGO_URI")) {
		if os.Getenv("STOREHUBX_REQUIRE_INFRA") == "1" {
			fmt.Println("integration: MongoDB not reachable and STOREHUBX_REQUIRE_INFRA=1")
			os.Exit(1)
		}
		fmt.Println("integration: MongoDB not reachable, skipping (run `make infra`)")
		os.Exit(0)
	}

	config.LoadConfig()
	db.Init(config.AppConfig.MongoURI)
	resetState()
	if err := db.EnsureIndexes(db.Client); err != nil {
		fmt.Println("integration: ensure indexes:", err)
		os.Exit(1)
	}
	cache.Init()
	if cache.Client != nil {
		cache.Client.FlushDB(context.Background())
	}
	if err := storage.InitBundles(); err != nil {
		fmt.Println("integration: object storage:", err)
		os.Exit(1)
	}
	app = server.New()

	code := m.Run()

	resetState()
	db.Disconnect()
	os.Exit(code)
}

func mongoReachable(uri string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).SetServerSelectionTimeout(2*time.Second))
	if err != nil {
		return false
	}
	defer c.Disconnect(ctx)
	return c.Ping(ctx, nil) == nil
}

func resetState() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = db.DB().Drop(ctx)
}
