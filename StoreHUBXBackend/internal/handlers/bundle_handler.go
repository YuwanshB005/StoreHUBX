package handlers

import (
	"context"
	"errors"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/yuwanshb005/storehubx/internal/auth"
	"github.com/yuwanshb005/storehubx/internal/config"
	"github.com/yuwanshb005/storehubx/internal/models"
	"github.com/yuwanshb005/storehubx/internal/storage"
	"github.com/yuwanshb005/storehubx/internal/utils"
)

// Built bundles live in a private bucket. Browsers load them only through
// these two routes, which check the component's visibility on every
// request:
//
//	GET /bundles/components/:slug/:version/*           public components
//	GET /bundles/t/:token/components/:slug/:version/*  private, with a bundle token
//
// The token sits in the path rather than a query string or cookie so the
// bundle's relative asset URLs (assets/app.js) inherit it automatically.

// ServeBundle serves a public component's built files.
func ServeBundle(c *fiber.Ctx) error {
	return serveBundle(c, "")
}

// ServeBundleWithToken serves files for owners/collaborators of a private
// component (or any component) given a valid bundle token.
func ServeBundleWithToken(c *fiber.Ctx) error {
	return serveBundle(c, c.Params("token"))
}

func serveBundle(c *fiber.Ctx, token string) error {
	slug, version := c.Params("slug"), c.Params("version")
	rest, err := url.PathUnescape(c.Params("*"))
	if err != nil || rest == "" {
		rest = "index.html"
	}

	prefix := path.Join("components", slug, version) + "/"
	key := path.Clean(prefix + rest)
	if !strings.HasPrefix(key, prefix) {
		return utils.Error(c, 404, "not found") // path traversal attempt
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	comp, err := findComponentBySlug(ctx, slug)
	if err != nil {
		return utils.Error(c, 404, "not found")
	}
	if comp.Visibility == "private" {
		uid, ok := auth.VerifyBundleToken(token, slug, version)
		// Re-check membership on every request so removing a collaborator
		// revokes access without waiting for the token to expire.
		if !ok || !canAccess(&comp, uid) {
			return utils.Error(c, 404, "not found")
		}
		c.Set("Cache-Control", "private, no-store")
	} else {
		c.Set("Cache-Control", "public, max-age=300")
	}

	if storage.Bundles == nil {
		return utils.Error(c, 503, "bundle storage unavailable")
	}
	// The object is streamed after this handler returns, so it must not use
	// the request-scoped ctx (cancelled on return). Fiber closes the body
	// once it has been sent.
	body, info, err := storage.Bundles.Open(context.Background(), key)
	if errors.Is(err, storage.ErrNotFound) {
		return utils.Error(c, 404, "not found")
	}
	if err != nil {
		return utils.Error(c, 502, "failed to read bundle")
	}

	c.Set("Content-Type", info.ContentType)
	c.Set("X-Content-Type-Options", "nosniff")
	// Untrusted user code: never let it leak the URL (which may carry a
	// token) to third parties through the Referer header.
	c.Set("Referrer-Policy", "no-referrer")
	return c.SendStream(body, int(info.Size))
}

func canAccess(comp *models.Component, uid string) bool {
	return uid != "" && (uid == comp.OwnerID || contains(comp.Collaborators, uid))
}

// bundleProxyURL turns any stored bundle URL into the public proxy URL the
// browser should use. Older builds stored direct MinIO URLs.
func bundleProxyURL(stored string) string {
	key, ok := storage.KeyFromURL(stored)
	if !ok {
		return stored
	}
	return config.AppConfig.PublicURL + "/bundles/" + key
}

// withProxyURLs rewrites the artifact URLs of jobs about to be returned.
func withProxyURLs(jobs ...*models.BuildJob) {
	for _, j := range jobs {
		if j.Artifacts != nil && j.Artifacts.BundleURL != "" {
			a := *j.Artifacts
			a.BundleURL = bundleProxyURL(a.BundleURL)
			j.Artifacts = &a
		}
	}
}
