package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/yuwanshb005/storehubx/internal/auth"
	"github.com/yuwanshb005/storehubx/internal/db"
	"github.com/yuwanshb005/storehubx/internal/models"
)

var seq int64

func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano()%1e6, atomic.AddInt64(&seq, 1))
}

type testUser struct {
	ID    string
	Token string
}

// newUser inserts a user and returns its id (the GitHub providerId, which is
// what JWTs and ownerId carry) plus a valid API token.
func newUser(t *testing.T) testUser {
	t.Helper()
	id := uniq("u")
	_, err := db.DB().Collection("users").InsertOne(context.Background(), models.User{
		Name: id, Username: id, Provider: "github", ProviderID: id,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	tok, err := auth.GenerateJWT(id, id+"@test.local")
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}
	return testUser{ID: id, Token: tok}
}

type compOpts struct {
	Private       bool
	Collaborators []string
	WebhookSecret string
}

// newComponent inserts a component linked to a fake repo directly in Mongo.
func newComponent(t *testing.T, owner string, o compOpts) models.Component {
	t.Helper()
	vis := "public"
	if o.Private {
		vis = "private"
	}
	if o.Collaborators == nil {
		o.Collaborators = []string{}
	}
	slug := uniq("comp")
	c := models.Component{
		ID: primitive.NewObjectID(), Name: slug, Slug: slug,
		Frameworks: []string{"react"}, Tags: []string{}, OwnerID: owner,
		RepoLink: models.RepoLink{Owner: "acme", Repo: "widgets", Ref: "main",
			Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WebhookSecret: o.WebhookSecret},
		Visibility: vis, Collaborators: o.Collaborators,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if _, err := db.DB().Collection("components").InsertOne(context.Background(), c); err != nil {
		t.Fatalf("insert component: %v", err)
	}
	return c
}

type resp struct {
	Status int
	Body   map[string]any
	Raw    []byte
	Header http.Header
}

// do sends a request through the full middleware stack.
func do(t *testing.T, method, path string, body any, token string, headers ...string) resp {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		r = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := app.Test(req, 15000)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Raw: raw, Header: res.Header}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func (r resp) data() map[string]any {
	d, _ := r.Body["data"].(map[string]any)
	return d
}

func expectStatus(t *testing.T, r resp, want int, what string) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s: status %d, want %d; body: %s", what, r.Status, want, string(r.Raw))
	}
}

func redisZ(at time.Time, member string) redis.Z {
	return redis.Z{Score: float64(at.UnixMilli()), Member: member}
}
