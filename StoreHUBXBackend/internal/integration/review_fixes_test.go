package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yuwanshb005/storehubx/internal/auth"
	"github.com/yuwanshb005/storehubx/internal/db"
	"github.com/yuwanshb005/storehubx/internal/models"
	"github.com/yuwanshb005/storehubx/internal/server"
)

// A version whose build was reused from another version (same commit)
// points at the other version's files. The private preview must still
// work: the bundle token has to match the files' version, not the
// requested one.
func TestPrivatePreviewOfReusedBuild(t *testing.T) {
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{Private: true})
	original := publishFakeBundle(t, c, "1.0.0")

	// 1.0.1 reuses 1.0.0's output, exactly as reuseCachedBuild records it.
	_, err := db.DB().Collection("build_jobs").InsertOne(context.Background(), models.BuildJob{
		ComponentID: c.ID, Component: c.Slug, Version: "1.0.1", Status: models.BuildSuccess,
		OwnerID: owner.ID, Artifacts: &models.BuildArtifact{BundleURL: original},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	pt, _ := auth.GeneratePreviewToken(owner.ID, c.Slug, "1.0.1")
	r := do(t, http.MethodGet, "/preview/"+c.Slug+"/1.0.1?token="+url.QueryEscape(pt), nil, "")
	expectStatus(t, r, 302, "preview redirect for reused build")
	loc, _ := url.Parse(r.Header.Get("Location"))
	expectStatus(t, do(t, http.MethodGet, loc.Path, nil, ""), 200, "reused private bundle via token")
}

// Previews load every file through /bundles. Those requests must not eat
// the per-IP API rate limit, or a couple of previews lock the user out.
func TestBundleRequestsAreNotRateLimited(t *testing.T) {
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{})
	publishFakeBundle(t, c, "1.0.0")

	t.Setenv("RATE_LIMIT_PER_MIN", "3")
	limited := server.New()
	get := func(path string) int {
		res, err := limited.Test(httptest.NewRequest(http.MethodGet, path, nil), 15000)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	for i := 0; i < 10; i++ {
		if s := get("/bundles/components/" + c.Slug + "/1.0.0/assets/app.js"); s != 200 {
			t.Fatalf("bundle request %d: status %d, want 200", i+1, s)
		}
	}
	// The API itself is still limited.
	codes := []string{}
	for i := 0; i < 5; i++ {
		codes = append(codes, http.StatusText(get("/health")))
	}
	if !strings.Contains(strings.Join(codes, ","), http.StatusText(429)) {
		t.Fatalf("API no longer rate limited: %v", codes)
	}
}
