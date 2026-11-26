package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/yuwanshb005/storehubx/internal/db"
	"github.com/yuwanshb005/storehubx/internal/models"
)

const hookSecret = "webhook-test-secret"

func sign(body []byte) string {
	m := hmac.New(sha256.New, []byte(hookSecret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func sha(n int) string { return fmt.Sprintf("%040x", n+1) }

// push delivers a signed GitHub push webhook.
func push(t *testing.T, slug, commit, delivery string) resp {
	body := []byte(fmt.Sprintf(`{"ref":"refs/heads/main","after":%q,"repository":{"full_name":"acme/widgets"}}`, commit))
	return do(t, http.MethodPost, "/webhooks/github/"+slug, body, "",
		"X-GitHub-Event", "push", "X-GitHub-Delivery", delivery, "X-Hub-Signature-256", sign(body))
}

func versionsOf(t *testing.T, c models.Component) []models.ComponentVersion {
	t.Helper()
	cur, err := db.DB().Collection("component_versions").Find(context.Background(), bson.M{"componentId": c.ID})
	if err != nil {
		t.Fatal(err)
	}
	var vs []models.ComponentVersion
	if err := cur.All(context.Background(), &vs); err != nil {
		t.Fatal(err)
	}
	return vs
}

// Many pushes landing at once must each get their own version. Previously
// they all read the same "latest" version, computed the same next number,
// and all but one failed with a 500 — those commits were never deployed.
func TestConcurrentPushesAllDeploy(t *testing.T) {
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{WebhookSecret: hookSecret})

	const n = 50
	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = push(t, c.Slug, sha(i), fmt.Sprintf("delivery-%s-%d", c.Slug, i)).Status
		}(i)
	}
	wg.Wait()

	failed := 0
	for _, s := range statuses {
		if s != 200 {
			failed++
		}
	}
	vs := versionsOf(t, c)
	seen := map[string]bool{}
	for _, v := range vs {
		if seen[v.Version] {
			t.Errorf("duplicate version %s", v.Version)
		}
		seen[v.Version] = true
	}
	if failed > 0 || len(vs) != n {
		t.Fatalf("%d of %d pushes failed; %d versions created, want %d", failed, n, len(vs), n)
	}
}

// GitHub redelivers webhooks (timeouts, manual redelivery). The same
// delivery must be processed once and acknowledged with 200, not a 500.
func TestRedeliveredWebhookIsIdempotent(t *testing.T) {
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{WebhookSecret: hookSecret})

	var wg sync.WaitGroup
	statuses := make([]int, 5)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = push(t, c.Slug, sha(7), "same-delivery-"+c.Slug).Status
		}(i)
	}
	wg.Wait()
	for i, s := range statuses {
		if s != 200 {
			t.Errorf("delivery attempt %d: status %d, want 200", i, s)
		}
	}
	if vs := versionsOf(t, c); len(vs) != 1 {
		t.Fatalf("redelivery created %d versions, want 1", len(vs))
	}
}

// Two different deliveries for the same commit (e.g. push + re-push) still
// produce one version and no error.
func TestSameCommitDifferentDeliveries(t *testing.T) {
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{WebhookSecret: hookSecret})
	var wg sync.WaitGroup
	statuses := make([]int, 4)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = push(t, c.Slug, sha(9), fmt.Sprintf("d-%s-%d", c.Slug, i)).Status
		}(i)
	}
	wg.Wait()
	for i, s := range statuses {
		if s != 200 {
			t.Errorf("delivery %d: status %d, want 200", i, s)
		}
	}
	if vs := versionsOf(t, c); len(vs) != 1 {
		t.Fatalf("same commit created %d versions, want 1", len(vs))
	}
}

func TestBranchDeletionPushIsIgnored(t *testing.T) {
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{WebhookSecret: hookSecret})
	r := push(t, c.Slug, "0000000000000000000000000000000000000000", "del-"+c.Slug)
	expectStatus(t, r, 200, "branch deletion push")
	if vs := versionsOf(t, c); len(vs) != 0 {
		t.Fatalf("branch deletion created %d versions", len(vs))
	}
}

// A manually chosen version moves the automatic numbering forward.
func TestAutoVersionContinuesAfterManualVersion(t *testing.T) {
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{WebhookSecret: hookSecret})

	r := do(t, http.MethodPost, "/api/components/"+c.Slug+"/deploy",
		map[string]any{"commitSha": sha(100), "version": "2.3.0"}, owner.Token)
	expectStatus(t, r, 200, "manual deploy 2.3.0")

	r = push(t, c.Slug, sha(101), "after-manual-"+c.Slug)
	expectStatus(t, r, 200, "webhook after manual version")
	if got := r.data()["version"].(map[string]any)["version"]; got != "2.3.1" {
		t.Fatalf("auto version after 2.3.0 = %v, want 2.3.1", got)
	}

	// Re-using an existing version number is a clean 409, not a 500.
	r = do(t, http.MethodPost, "/api/components/"+c.Slug+"/deploy",
		map[string]any{"commitSha": sha(102), "version": "2.3.0"}, owner.Token)
	expectStatus(t, r, 409, "duplicate manual version")
}
