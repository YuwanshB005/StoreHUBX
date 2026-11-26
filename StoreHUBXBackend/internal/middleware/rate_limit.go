package middleware

import (
	"os"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"
)

// RateLimiter allows RATE_LIMIT_PER_MIN requests per IP per minute (default
// 100). Tests raise it so a suite of requests from 127.0.0.1 isn't throttled.
func RateLimiter() fiber.Handler {
	max := 100
	if v, err := strconv.Atoi(os.Getenv("RATE_LIMIT_PER_MIN")); err == nil && v > 0 {
		max = v
	}
	return limiter.New(limiter.Config{
		Max:               max,
		Expiration:        1 * time.Minute,
		LimiterMiddleware: limiter.SlidingWindow{},
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(429).JSON(fiber.Map{
				"success": false,
				"error":   "too many requests, please slow down",
			})
		},
	})
}
