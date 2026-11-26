package integration

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yuwanshb005/storehubx/internal/auth"
	"github.com/yuwanshb005/storehubx/internal/cache"
	"github.com/yuwanshb005/storehubx/internal/db"
	githubapi "github.com/yuwanshb005/storehubx/internal/github"
	"github.com/yuwanshb005/storehubx/internal/models"
	"github.com/yuwanshb005/storehubx/internal/storage"
)

// publishFakeBundle uploads a two-file bundle and records a successful build
// pointing at it, exactly as the worker would.
func publishFakeBundle(t *testing.T, c models.Component, version string) string {
	t.Helper()
	ctx := context.Background()
	up, err := storage.NewS3Uploader()
	if err != nil {
		t.Fatal(err)
	}
	base := "components/" + c.Slug + "/" + version + "/"
	bundleURL, err := up.Put(ctx, base+"index.html", []byte(`<html><script src="assets/app.js"></script>SECRET-`+c.Slug+`</html>`), "text/html")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.Put(ctx, base+"assets/app.js", []byte(`console.log("app")`), "application/javascript"); err != nil {
		t.Fatal(err)
	}
	_, err = db.DB().Collection("build_jobs").InsertOne(ctx, models.BuildJob{
		ComponentID: c.ID, Component: c.Slug, Version: version, Status: models.BuildSuccess,
		OwnerID: c.OwnerID, Artifacts: &models.BuildArtifact{BundleURL: bundleURL},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return bundleURL
}

func TestBucketIsNotPubliclyReadable(t *testing.T) {
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{Private: true})
	direct := publishFakeBundle(t, c, "1.0.0")

	for what, u := range map[string]string{
		"direct object GET": direct,
		"bucket listing":    os.Getenv("S3_PUBLIC_BASE_URL") + "?list-type=2",
	} {
		res, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("anonymous %s: status %d, want 403", what, res.StatusCode)
		}
	}
}

func TestPublicBundleServedThroughProxy(t *testing.T) {
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{})
	publishFakeBundle(t, c, "1.0.0")

	r := do(t, http.MethodGet, "/bundles/components/"+c.Slug+"/1.0.0/index.html", nil, "")
	expectStatus(t, r, 200, "public index.html")
	if !strings.Contains(string(r.Raw), "SECRET-"+c.Slug) || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("unexpected index body/type: %q %q", r.Header.Get("Content-Type"), r.Raw)
	}
	r = do(t, http.MethodGet, "/bundles/components/"+c.Slug+"/1.0.0/assets/app.js", nil, "")
	expectStatus(t, r, 200, "public asset")

	r = do(t, http.MethodGet, "/bundles/components/"+c.Slug+"/1.0.0/missing.js", nil, "")
	expectStatus(t, r, 404, "missing file")

	// Build API responses point at the proxy, not at MinIO.
	builds := do(t, http.MethodGet, "/components/"+c.Slug+"/versions/1.0.0/builds", nil, "")
	expectStatus(t, builds, 200, "list builds")
	b := builds.data()["builds"].([]any)[0].(map[string]any)
	got := b["artifacts"].(map[string]any)["bundleUrl"].(string)
	if !strings.HasSuffix(got, "/bundles/components/"+c.Slug+"/1.0.0/index.html") {
		t.Fatalf("bundleUrl not rewritten to proxy: %s", got)
	}
}

func TestPrivateBundleNeedsAuthorisedToken(t *testing.T) {
	owner, collab, stranger := newUser(t), newUser(t), newUser(t)
	c := newComponent(t, owner.ID, compOpts{Private: true, Collaborators: []string{collab.ID}})
	other := newComponent(t, owner.ID, compOpts{Private: true})
	publishFakeBundle(t, c, "1.0.0")
	publishFakeBundle(t, other, "1.0.0")
	idx := "/components/" + c.Slug + "/1.0.0/index.html"

	expectStatus(t, do(t, http.MethodGet, "/bundles"+idx, nil, ""), 404, "private bundle without token")
	expectStatus(t, do(t, http.MethodGet, "/preview/"+c.Slug+"/1.0.0", nil, ""), 404, "anonymous preview")

	// Owner flow: preview token -> redirect to a tokenised bundle path.
	pt, _ := auth.GeneratePreviewToken(owner.ID, c.Slug, "1.0.0")
	r := do(t, http.MethodGet, "/preview/"+c.Slug+"/1.0.0?token="+url.QueryEscape(pt), nil, "")
	expectStatus(t, r, 302, "owner preview redirect")
	loc, _ := url.Parse(r.Header.Get("Location"))
	if !strings.HasPrefix(loc.Path, "/bundles/t/") {
		t.Fatalf("redirect not to tokenised bundle path: %s", loc)
	}
	expectStatus(t, do(t, http.MethodGet, loc.Path, nil, ""), 200, "owner index via token")
	asset := strings.TrimSuffix(loc.Path, "index.html") + "assets/app.js"
	expectStatus(t, do(t, http.MethodGet, asset, nil, ""), 200, "owner asset via relative path")

	// The token is scoped: not valid for another component or another user.
	tok := strings.Split(loc.Path, "/")[3]
	expectStatus(t, do(t, http.MethodGet, "/bundles/t/"+tok+"/components/"+other.Slug+"/1.0.0/index.html", nil, ""), 404, "token reused on other component")
	st, _ := auth.GenerateBundleToken(stranger.ID, c.Slug, "1.0.0")
	expectStatus(t, do(t, http.MethodGet, "/bundles/t/"+st+idx, nil, ""), 404, "stranger's bundle token")
	ct, _ := auth.GenerateBundleToken(collab.ID, c.Slug, "1.0.0")
	expectStatus(t, do(t, http.MethodGet, "/bundles/t/"+ct+idx, nil, ""), 200, "collaborator's bundle token")
	// An API login token is not a bundle token.
	expectStatus(t, do(t, http.MethodGet, "/bundles/t/"+owner.Token+idx, nil, ""), 404, "API token in bundle path")

	// Path traversal out of the component's folder is refused.
	r = do(t, http.MethodGet, "/bundles/t/"+tok+"/components/"+c.Slug+"/1.0.0/..%2F..%2F"+other.Slug+"%2F1.0.0%2Findex.html", nil, "")
	if r.Status == 200 {
		t.Fatalf("path traversal served another component: %s", r.Raw)
	}
}

// stubTransport answers GitHub API calls without touching the network:
// authenticated requests see a private repo, anonymous ones get 404.
type stubTransport struct{}

func (stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Request: req,
			Body: io.NopCloser(strings.NewReader(`{"description":"TOP SECRET internal project","stargazers_count":3}`))}, nil
	}
	return &http.Response{StatusCode: 404, Header: http.Header{}, Request: req,
		Body: io.NopCloser(strings.NewReader(`{"message":"Not Found"}`))}, nil
}

func TestPrivateRepoInfoNotLeakedThroughSharedCache(t *testing.T) {
	if cache.Client == nil {
		t.Skip("needs Redis")
	}
	orig := http.DefaultClient.Transport
	http.DefaultClient.Transport = stubTransport{}
	defer func() { http.DefaultClient.Transport = orig }()

	owner, repo := uniq("acme"), "secret-repo"
	// Owner links/autofills a private repo using their own GitHub token.
	info, err := githubapi.FetchRepoInfo("gho_owner_token", owner, repo)
	if err != nil || info.Description != "TOP SECRET internal project" {
		t.Fatalf("owner fetch: %v %+v", err, info)
	}
	// An anonymous visitor asks for the same repo.
	r := do(t, http.MethodGet, "/github/repo-info?owner="+owner+"&repo="+repo, nil, "")
	if strings.Contains(string(r.Raw), "TOP SECRET") {
		t.Fatalf("anonymous request got private repo data: %s", r.Raw)
	}
}
