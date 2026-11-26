package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/yuwanshb005/storehubx/internal/cache"
	"github.com/yuwanshb005/storehubx/internal/db"
	"github.com/yuwanshb005/storehubx/internal/models"
	"github.com/yuwanshb005/storehubx/internal/storage"
	"github.com/yuwanshb005/storehubx/internal/worker"
)

// staticSource returns a fetcher that "downloads" a one-file static site
// (no package.json, so no npm build) containing marker.
func staticSource(marker string) worker.SourceFetcher {
	return func(ctx context.Context, job *models.BuildJob, root string) (string, error) {
		dir := filepath.Join(root, "src")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		return dir, os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html><head></head>"+marker+"</html>"), 0o644)
	}
}

func newProcessor(t *testing.T, src worker.SourceFetcher) *worker.Processor {
	t.Helper()
	up, err := storage.NewS3Uploader()
	if err != nil {
		t.Fatal(err)
	}
	p := worker.NewProcessor(up)
	p.Lease = time.Second
	p.HeartbeatEvery = 200 * time.Millisecond
	p.JobTimeout = 30 * time.Second
	p.FetchSource = src
	return p
}

// queueJob inserts a queued job for a fresh component. Older queued jobs
// from other tests are cleared so ProcessNext/ClaimNext pick this one.
func queueJob(t *testing.T) (models.Component, primitive.ObjectID) {
	t.Helper()
	ctx := context.Background()
	_, _ = db.DB().Collection("build_jobs").UpdateMany(ctx,
		bson.M{"status": bson.M{"$in": []models.BuildStatus{models.BuildQueued, models.BuildRunning}}},
		bson.M{"$set": bson.M{"status": models.BuildError}})
	owner := newUser(t)
	c := newComponent(t, owner.ID, compOpts{})
	res, err := db.DB().Collection("build_jobs").InsertOne(ctx, models.BuildJob{
		ComponentID: c.ID, Component: c.Slug, Version: "1.0.0", Status: models.BuildQueued,
		OwnerID: owner.ID, MaxAttempts: 3, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, res.InsertedID.(primitive.ObjectID)
}

func loadJob(t *testing.T, id primitive.ObjectID) models.BuildJob {
	t.Helper()
	var j models.BuildJob
	if err := db.DB().Collection("build_jobs").FindOne(context.Background(), bson.M{"_id": id}).Decode(&j); err != nil {
		t.Fatal(err)
	}
	return j
}

func bundleBody(t *testing.T, c models.Component, j models.BuildJob) string {
	t.Helper()
	key, ok := storage.KeyFromURL(j.Artifacts.BundleURL)
	if !ok {
		t.Fatalf("bad bundle url %s", j.Artifacts.BundleURL)
	}
	r := do(t, http.MethodGet, "/bundles/"+key, nil, "")
	expectStatus(t, r, 200, "bundle for "+c.Slug)
	return string(r.Raw)
}

func TestWorkerBuildsAndRecordsResult(t *testing.T) {
	c, id := queueJob(t)
	p := newProcessor(t, staticSource("hello-"+c.Slug))
	if !p.ProcessNext(context.Background()) {
		t.Fatal("no job processed")
	}
	j := loadJob(t, id)
	if j.Status != models.BuildSuccess || j.LeaseToken != "" || j.LeaseExpiresAt != nil {
		t.Fatalf("status=%s lease=%q expires=%v, want success with lease cleared", j.Status, j.LeaseToken, j.LeaseExpiresAt)
	}
	if !strings.Contains(bundleBody(t, c, j), "hello-"+c.Slug) {
		t.Fatal("published bundle has the wrong content")
	}
}

// kill -9 equivalent: a worker claims a job and disappears. Its lease
// expires, the reaper requeues it, and another worker finishes it.
func TestCrashedWorkersJobIsRecovered(t *testing.T) {
	c, id := queueJob(t)
	dead := newProcessor(t, staticSource("never"))
	if _, err := dead.ClaimNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if j := loadJob(t, id); j.Status != models.BuildRunning {
		t.Fatalf("claimed job status %s", j.Status)
	}

	time.Sleep(1200 * time.Millisecond) // lease (1s) runs out, no heartbeat
	survivor := newProcessor(t, staticSource("recovered-"+c.Slug))
	if n, err := survivor.ReapExpired(context.Background()); err != nil || n != 1 {
		t.Fatalf("reaped %d jobs (err %v), want 1", n, err)
	}
	j := loadJob(t, id)
	if j.Status != models.BuildQueued || j.Attempts != 1 {
		t.Fatalf("after reap: status=%s attempts=%d, want queued/1", j.Status, j.Attempts)
	}

	// Skip the backoff wait for the test.
	_, _ = db.DB().Collection("build_jobs").UpdateByID(context.Background(), id, bson.M{"$set": bson.M{"nextAttemptAt": time.Now()}})
	if !survivor.ProcessNext(context.Background()) {
		t.Fatal("survivor found no job")
	}
	j = loadJob(t, id)
	if j.Status != models.BuildSuccess {
		t.Fatalf("recovered job status %s, want success; logs: %v", j.Status, j.Logs)
	}
	if !strings.Contains(strings.Join(j.Logs, "\n"), "lease expired") {
		t.Errorf("logs don't record the recovery: %v", j.Logs)
	}
}

// A stalled worker (GC pause, network partition) loses its lease, another
// attempt takes over and succeeds, then the stalled worker wakes up and
// finishes. Its late writes must change nothing: fencing tokens.
func TestStaleWorkerCannotOverwriteNewerAttempt(t *testing.T) {
	c, id := queueJob(t)

	release := make(chan struct{})
	stalled := newProcessor(t, func(ctx context.Context, job *models.BuildJob, root string) (string, error) {
		<-release // stalled: no heartbeats land while it's stuck here
		return staticSource("STALE")(ctx, job, root)
	})
	stalled.HeartbeatEvery = time.Hour

	done := make(chan struct{})
	go func() {
		// Run without the job context being cancelled by lease loss.
		stalled.ProcessNext(context.Background())
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for loadJob(t, id).Status != models.BuildRunning && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1200 * time.Millisecond)

	winner := newProcessor(t, staticSource("WINNER"))
	if n, _ := winner.ReapExpired(context.Background()); n != 1 {
		t.Fatalf("expected the stalled attempt to be reaped, got %d", n)
	}
	_, _ = db.DB().Collection("build_jobs").UpdateByID(context.Background(), id, bson.M{"$set": bson.M{"nextAttemptAt": time.Now()}})
	if !winner.ProcessNext(context.Background()) {
		t.Fatal("winner found no job")
	}
	won := loadJob(t, id)

	close(release) // the stale worker wakes up and completes its build
	<-done

	final := loadJob(t, id)
	if final.Status != models.BuildSuccess || final.Artifacts.BundleURL != won.Artifacts.BundleURL {
		t.Fatalf("stale worker changed the result: %s -> %s", won.Artifacts.BundleURL, final.Artifacts.BundleURL)
	}
	if body := bundleBody(t, c, final); !strings.Contains(body, "WINNER") {
		t.Fatalf("served bundle was overwritten by the stale attempt: %s", body)
	}
	if len(final.Logs) != len(won.Logs) {
		t.Errorf("stale worker appended logs after losing its lease: %v", final.Logs[len(won.Logs):])
	}
}

func TestPermanentFailureIsNotRetried(t *testing.T) {
	_, id := queueJob(t)
	p := newProcessor(t, func(context.Context, *models.BuildJob, string) (string, error) {
		return "", fmt.Errorf("download failed: %w: 404 Not Found", worker.ErrPermanent)
	})
	p.ProcessNext(context.Background())
	j := loadJob(t, id)
	if j.Status != models.BuildError || j.Attempts != 1 || j.NextAttemptAt != nil {
		t.Fatalf("status=%s attempts=%d next=%v, want error/1/none", j.Status, j.Attempts, j.NextAttemptAt)
	}
}

func TestTransientFailureIsRetriedWithBackoff(t *testing.T) {
	_, id := queueJob(t)
	p := newProcessor(t, func(context.Context, *models.BuildJob, string) (string, error) {
		return "", errors.New("connection reset by peer")
	})
	before := time.Now()
	p.ProcessNext(context.Background())
	j := loadJob(t, id)
	if j.Status != models.BuildQueued || j.Attempts != 1 || j.NextAttemptAt == nil {
		t.Fatalf("status=%s attempts=%d, want queued/1 with nextAttemptAt", j.Status, j.Attempts)
	}
	if wait := j.NextAttemptAt.Sub(before); wait < 9*time.Second || wait > 12*time.Second {
		t.Errorf("backoff %s, want ~10s", wait)
	}
	// Not claimable before it's due.
	if p.ProcessNext(context.Background()) {
		t.Fatal("job was retried before its backoff elapsed")
	}
	if cache.Client != nil {
		if _, err := cache.Client.ZScore(context.Background(), "builds:delayed", id.Hex()).Result(); err != nil {
			t.Errorf("retry not scheduled in builds:delayed: %v", err)
		}
	}
}

func TestDueRetriesArePromotedToTheStream(t *testing.T) {
	if cache.Client == nil {
		t.Skip("needs Redis")
	}
	ctx := context.Background()
	id := primitive.NewObjectID().Hex()
	cache.Client.ZAdd(ctx, "builds:delayed", redisZ(time.Now().Add(-time.Second), id))
	future := primitive.NewObjectID().Hex()
	cache.Client.ZAdd(ctx, "builds:delayed", redisZ(time.Now().Add(time.Hour), future))

	p := newProcessor(t, staticSource("x"))
	if _, err := p.PromoteDueRetries(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Client.ZScore(ctx, "builds:delayed", id).Result(); err == nil {
		t.Error("due retry still in the delayed set")
	}
	if _, err := cache.Client.ZScore(ctx, "builds:delayed", future).Result(); err != nil {
		t.Error("future retry was promoted early")
	}
	msgs, _ := cache.Client.XRange(ctx, "builds:stream", "-", "+").Result()
	found := false
	for _, m := range msgs {
		if m.Values["jobId"] == id {
			found = true
		}
	}
	if !found {
		t.Error("due retry not pushed onto builds:stream")
	}
}

func TestShutdownReturnsJobWithoutUsingAnAttempt(t *testing.T) {
	_, id := queueJob(t)
	ctx, cancel := context.WithCancel(context.Background())
	p := newProcessor(t, func(c context.Context, job *models.BuildJob, root string) (string, error) {
		cancel() // SIGTERM arrives mid-build
		<-c.Done()
		return "", c.Err()
	})
	p.ProcessNext(ctx)
	j := loadJob(t, id)
	if j.Status != models.BuildQueued || j.Attempts != 0 || j.LeaseToken != "" {
		t.Fatalf("status=%s attempts=%d lease=%q, want queued/0/none", j.Status, j.Attempts, j.LeaseToken)
	}
}

// The job-level timeout used to cancel the context that the final status
// write then used, so the write failed and the job stayed "running".
func TestTimedOutJobIsNotLeftRunning(t *testing.T) {
	_, id := queueJob(t)
	p := newProcessor(t, func(c context.Context, job *models.BuildJob, root string) (string, error) {
		<-c.Done()
		return "", c.Err()
	})
	p.JobTimeout = 500 * time.Millisecond
	p.ProcessNext(context.Background())
	j := loadJob(t, id)
	if j.Status == models.BuildRunning {
		t.Fatal("timed-out job left in running state")
	}
	if j.Status != models.BuildQueued || j.Attempts != 1 {
		t.Fatalf("status=%s attempts=%d, want queued for retry", j.Status, j.Attempts)
	}
}

