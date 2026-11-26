package integration

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/yuwanshb005/storehubx/internal/auth"
)

// A preview token is minted for one component version, lives 2 minutes and
// travels in an iframe URL (logs, history). It must never work as a login.
func TestPreviewTokenIsNotAnAPIToken(t *testing.T) {
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{Private: true})

	pt, err := auth.GeneratePreviewToken(owner.ID, c.Slug, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/me", "/api/me/analytics", "/api/notifications"} {
		r := do(t, http.MethodGet, path, nil, pt)
		expectStatus(t, r, 401, "preview token used on "+path)
	}
	// OptionalAuth routes must not treat it as identity either.
	r := do(t, http.MethodGet, "/components/"+c.Slug, nil, pt)
	expectStatus(t, r, 404, "preview token on private component detail")

	// And an API token must not pass as a preview token.
	if _, ok := auth.VerifyPreviewToken(owner.Token, c.Slug, "1.0.0"); ok {
		t.Fatal("API token accepted as preview token")
	}
}

func TestOAuthLoginUsesStateCookie(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "test-client")
	t.Setenv("GITHUB_CLIENT_SECRET", "test-secret")

	r := do(t, http.MethodGet, "/auth/github/login", nil, "")
	if r.Status != http.StatusFound && r.Status != http.StatusSeeOther {
		t.Fatalf("login status %d", r.Status)
	}
	loc, _ := url.Parse(r.Header.Get("Location"))
	state := loc.Query().Get("state")
	if len(state) < 32 {
		t.Fatalf("login redirect has no strong state param: %q", r.Header.Get("Location"))
	}
	cookie := r.Header.Get("Set-Cookie")
	if !strings.Contains(cookie, "oauth_state="+state) || !strings.Contains(strings.ToLower(cookie), "httponly") {
		t.Fatalf("state cookie missing or not HttpOnly: %q", cookie)
	}

	// Callback with no / wrong state is rejected before any GitHub call.
	r = do(t, http.MethodGet, "/auth/github/callback?code=abc", nil, "")
	expectStatus(t, r, 400, "callback without state")
	r = do(t, http.MethodGet, "/auth/github/callback?code=abc&state="+state, nil, "", "Cookie", "oauth_state=somethingelse")
	expectStatus(t, r, 400, "callback with mismatched state")
}

func TestCORSOnlyAllowsFrontendOrigin(t *testing.T) {
	pre := func(origin string) string {
		r := do(t, http.MethodOptions, "/api/me", nil, "",
			"Origin", origin, "Access-Control-Request-Method", "GET")
		return r.Header.Get("Access-Control-Allow-Origin")
	}
	if got := pre("http://localhost:3000"); got != "http://localhost:3000" {
		t.Errorf("frontend origin: Access-Control-Allow-Origin = %q", got)
	}
	if got := pre("https://evil.example"); got != "" {
		t.Errorf("foreign origin allowed: Access-Control-Allow-Origin = %q", got)
	}
}
