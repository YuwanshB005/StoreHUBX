package handlers

import (
	"context"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/yuwanshb005/storehubx/internal/auth"
	"github.com/yuwanshb005/storehubx/internal/db"
	"github.com/yuwanshb005/storehubx/internal/models"
	"github.com/yuwanshb005/storehubx/internal/utils"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// GET /preview/:slug/:version -> 302 to the latest successful build's bundle URL
func RedirectPreview(c *fiber.Ctx) error {
	slug := c.Params("slug")
	ver := c.Params("version")
	if slug == "" || ver == "" {
		return utils.Error(c, 400, "missing slug or version")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	comp, err := findComponentBySlug(ctx, slug)
	if err != nil {
		return utils.Error(c, 404, "component not found")
	}
	// A private component needs proof of identity. An iframe can't send an
	// Authorization header, so the frontend appends a 2-minute preview
	// token (?token=) minted by GET .../preview-token.
	uid, _ := c.Locals("user_id").(string)
	if token := c.Query("token"); token != "" {
		if tokenUID, ok := auth.VerifyPreviewToken(token, slug, ver); ok {
			uid = tokenUID
		}
	}
	if comp.Visibility == "private" && !canAccess(&comp, uid) {
		return utils.Error(c, 404, "component not found")
	}

	jobCol := db.DB().Collection("build_jobs")
	opts := options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})
	var job models.BuildJob
	err = jobCol.FindOne(ctx, bson.M{
		"componentId": comp.ID,
		"version":     ver,
		"status":      models.BuildSuccess,
	}, opts).Decode(&job)
	if err != nil || job.Artifacts == nil || job.Artifacts.BundleURL == "" {
		return utils.Error(c, 404, "preview not available for this version")
	}

	target := bundleProxyURL(job.Artifacts.BundleURL)
	if comp.Visibility == "private" {
		// Swap the one-shot preview token for a bundle token that rides in
		// the path, so the page's relative asset requests are authorised too.
		bt, err := auth.GenerateBundleToken(uid, slug, ver)
		if err != nil {
			return utils.Error(c, 500, "failed to authorise preview")
		}
		target = strings.Replace(target, "/bundles/", "/bundles/t/"+bt+"/", 1)
	}
	c.Set("Referrer-Policy", "no-referrer")
	return c.Redirect(target, fiber.StatusFound) // 302
}

// GET /api/components/:slug/versions/:version/preview-token -> mints a
// short-lived (2 minute) token an owner/collaborator can append as
// ?token= on GET /preview/:slug/:version, since a plain iframe src can't
// carry an Authorization header for a private component's preview.
func GetPreviewToken(c *fiber.Ctx) error {
	slug := c.Params("slug")
	versionStr := c.Params("version")
	if slug == "" || versionStr == "" {
		return utils.Error(c, 400, "missing slug or version")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	comp, err := findComponentBySlug(ctx, slug)
	if err != nil {
		return utils.Error(c, 404, "component not found")
	}

	uid, _ := c.Locals("user_id").(string)
	if uid == "" || (uid != comp.OwnerID && !contains(comp.Collaborators, uid)) {
		return utils.Error(c, 403, "not authorized to preview this component")
	}

	token, err := auth.GeneratePreviewToken(uid, slug, versionStr)
	if err != nil {
		return utils.Error(c, 500, "failed to generate preview token")
	}

	return utils.Success(c, fiber.Map{"token": token, "expiresIn": 120})
}
