package integration

import (
	"net/http"
	"testing"
)

func TestHealth(t *testing.T) {
	r := do(t, http.MethodGet, "/health", nil, "")
	expectStatus(t, r, 200, "health")
}

func TestProtectedRoutesRequireAuth(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/components"},
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/notifications"},
		{http.MethodPost, "/api/components/x/like"},
	} {
		r := do(t, tc.method, tc.path, nil, "")
		expectStatus(t, r, 401, tc.method+" "+tc.path+" without token")
		r = do(t, tc.method, tc.path, nil, "not.a.jwt")
		expectStatus(t, r, 401, tc.method+" "+tc.path+" with garbage token")
		if r.Body["success"] != false {
			t.Errorf("%s %s: 401 body should use the {success:false} envelope, got %s", tc.method, tc.path, r.Raw)
		}
	}
}

func TestCreateComponent(t *testing.T) {
	u := newUser(t)

	r := do(t, http.MethodPost, "/api/components", map[string]any{"name": "No Frameworks"}, u.Token)
	expectStatus(t, r, 400, "create without frameworks")

	body := map[string]any{"name": "Fancy Button", "frameworks": []string{" React ", "react"}, "tags": []string{"UI"}}
	r1 := do(t, http.MethodPost, "/api/components", body, u.Token)
	expectStatus(t, r1, 200, "create")
	c1 := r1.data()["component"].(map[string]any)
	if fw := c1["frameworks"].([]any); len(fw) != 1 || fw[0] != "react" {
		t.Errorf("frameworks not normalized: %v", fw)
	}
	if c1["ownerId"] != u.ID {
		t.Errorf("ownerId = %v, want %s", c1["ownerId"], u.ID)
	}

	r2 := do(t, http.MethodPost, "/api/components", body, u.Token)
	expectStatus(t, r2, 200, "create duplicate name")
	if s1, s2 := c1["slug"], r2.data()["component"].(map[string]any)["slug"]; s1 == s2 {
		t.Errorf("second component reused slug %v", s1)
	}
}

func TestPrivateComponentVisibilityViaAPI(t *testing.T) {
	owner, collab, stranger := newUser(t), newUser(t), newUser(t)
	c := newComponent(t, owner.ID, compOpts{Private: true, Collaborators: []string{collab.ID}})

	cases := []struct {
		who   string
		token string
		want  int
	}{
		{"anonymous", "", 404},
		{"stranger", stranger.Token, 404},
		{"owner", owner.Token, 200},
		{"collaborator", collab.Token, 200},
	}
	for _, tc := range cases {
		for _, path := range []string{"/components/" + c.Slug, "/components/" + c.Slug + "/versions"} {
			r := do(t, http.MethodGet, path, nil, tc.token)
			expectStatus(t, r, tc.want, tc.who+" GET "+path)
		}
	}

	list := do(t, http.MethodGet, "/components?limit=100", nil, owner.Token)
	expectStatus(t, list, 200, "list")
	for _, it := range list.data()["components"].([]any) {
		if it.(map[string]any)["slug"] == c.Slug {
			t.Fatalf("private component %s appeared in public listing", c.Slug)
		}
	}
}

func TestLikeToggle(t *testing.T) {
	owner, fan := newUser(t), newUser(t)
	c := newComponent(t, owner.ID, compOpts{})

	r := do(t, http.MethodPost, "/api/components/"+c.Slug+"/like", nil, fan.Token)
	expectStatus(t, r, 200, "like")
	if got := r.data()["component"].(map[string]any)["likeCount"]; got != float64(1) {
		t.Fatalf("likeCount after like = %v, want 1", got)
	}
	r = do(t, http.MethodPost, "/api/components/"+c.Slug+"/like", nil, fan.Token)
	expectStatus(t, r, 200, "unlike")
	if got := r.data()["component"].(map[string]any)["likeCount"]; got != float64(0) {
		t.Fatalf("likeCount after unlike = %v, want 0", got)
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{WebhookSecret: "s3cret"})

	r := do(t, http.MethodPost, "/webhooks/github/"+c.Slug, []byte(`{"ref":"refs/heads/main","after":"abc"}`), "",
		"X-GitHub-Event", "push", "X-Hub-Signature-256", "sha256=00")
	expectStatus(t, r, 401, "bad signature")

	r = do(t, http.MethodPost, "/webhooks/github/does-not-exist", []byte(`{}`), "")
	expectStatus(t, r, 404, "unknown component")
}
