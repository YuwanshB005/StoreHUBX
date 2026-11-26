// Package server builds the Fiber app (middleware + routes) so cmd/main.go
// and the integration tests run exactly the same HTTP stack.
package server

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/swagger"

	_ "github.com/yuwanshb005/storehubx/docs" // Swagger generated docs
	"github.com/yuwanshb005/storehubx/internal/config"
	"github.com/yuwanshb005/storehubx/internal/middleware"
	"github.com/yuwanshb005/storehubx/internal/routes"
)

// New returns a fully wired app. Database, cache and config must already be
// initialised by the caller.
func New() *fiber.App {
	app := fiber.New()

	// Only the frontend's own origin may call the API from a browser.
	app.Use(cors.New(cors.Config{
		AllowOrigins: config.AppConfig.CORSOrigins,
		AllowHeaders: "Origin, Content-Type, Authorization",
	}))
	app.Use(recover.New())
	app.Use(middleware.Logger())
	app.Use(middleware.RateLimiter())
	app.Get("/docs/*", swagger.HandlerDefault) // http://localhost:8080/docs/index.html

	routes.RegisterRoutes(app)
	return app
}
