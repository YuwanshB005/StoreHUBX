package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/yuwanshb005/storehubx/internal/cache"
	"github.com/yuwanshb005/storehubx/internal/db"
	"github.com/yuwanshb005/storehubx/internal/models"
	"github.com/yuwanshb005/storehubx/internal/notify"
	"github.com/yuwanshb005/storehubx/internal/storage"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Delivery model: at-least-once. A job can be started more than once
// (worker crash, lost lease, redelivered stream message), so every step is
// safe to repeat and only the attempt holding the current lease may record
// a result.
//
//   - Claim: an atomic FindOneAndUpdate moves queued -> running and stamps
//     a fresh leaseToken (fencing token) and leaseExpiresAt.
//   - Heartbeat: while working, the worker extends its lease. If the
//     renewal matches nothing, the lease was lost and the job is abandoned.
//   - Reaper: any worker requeues running jobs whose lease expired (their
//     worker crashed or stalled), counting it as a failed attempt.
//   - Fencing: every log, status and result write is filtered on
//     leaseToken, so a stale worker's late writes change nothing. Each
//     attempt also uploads to its own folder, so it can't overwrite the
//     files of the attempt that replaced it.
//   - Retries: transient failures are requeued with exponential backoff
//     and put in a Redis sorted set (builds:delayed) scored by due time;
//     a mover pushes them onto the stream when due. Permanent failures
//     (ErrPermanent) are not retried.

const (
	// BuildStreamKey/buildStreamGroup back the Redis Streams queue that
	// the API pushes new job ids onto (internal/handlers/build.go).
	BuildStreamKey   = "builds:stream"
	buildStreamGroup = "workers"
	// delayedRetryKey is a sorted set of job ids scored by when their
	// retry is due (unix milliseconds).
	delayedRetryKey = "builds:delayed"

	maxBuildBackoff = 2 * time.Minute
)

// SourceFetcher puts the source to build in a directory under workRoot and
// returns it. The default downloads the linked GitHub repo; tests replace it.
type SourceFetcher func(ctx context.Context, job *models.BuildJob, workRoot string) (string, error)

type Processor struct {
	uploader storage.Uploader
	tmpDir   string

	// Lease is how long a claim lasts without renewal; HeartbeatEvery is
	// how often a working job renews it. JobTimeout bounds one attempt.
	Lease          time.Duration
	HeartbeatEvery time.Duration
	JobTimeout     time.Duration
	FetchSource    SourceFetcher
}

func NewProcessor(uploader storage.Uploader) *Processor {
	tmp := os.Getenv("BUILD_TMP_DIR")
	if tmp == "" {
		tmp = os.TempDir()
	}
	lease := 60 * time.Second
	if s, err := strconv.Atoi(os.Getenv("BUILD_LEASE_SECONDS")); err == nil && s > 0 {
		lease = time.Duration(s) * time.Second
	}
	p := &Processor{
		uploader:       uploader,
		tmpDir:         tmp,
		Lease:          lease,
		HeartbeatEvery: lease / 3,
		JobTimeout:     loadSandboxConfig().Timeout + 5*time.Minute,
	}
	p.FetchSource = p.fetchFromGitHub
	return p
}

// fresh returns a short context independent of the job's, for writes that
// must happen even after the job context was cancelled or timed out (the
// final status). Writing those with the expired job context used to fail
// silently and leave jobs "running" forever.
func fresh() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// fenced is the filter that matches a job only while this attempt still
// holds its lease.
func fenced(job *models.BuildJob) bson.M {
	return bson.M{"_id": job.ID, "leaseToken": job.LeaseToken, "status": models.BuildRunning}
}

func (p *Processor) logPush(ctx context.Context, job *models.BuildJob, msg string) {
	_, _ = db.DB().Collection("build_jobs").UpdateOne(ctx, fenced(job),
		bson.M{"$set": bson.M{"updatedAt": time.Now()}, "$push": bson.M{"logs": msg}})
}

// claim atomically takes one due queued job (a specific one when id is
// non-nil) and gives this attempt a new lease.
func (p *Processor) claim(ctx context.Context, id *primitive.ObjectID) (*models.BuildJob, error) {
	now := time.Now()
	filter := bson.M{
		"status": models.BuildQueued,
		"$or": []bson.M{
			{"nextAttemptAt": bson.M{"$exists": false}},
			{"nextAttemptAt": bson.M{"$lte": now}},
		},
	}
	if id != nil {
		filter["_id"] = *id
	}
	expires := now.Add(p.Lease)
	var job models.BuildJob
	err := db.DB().Collection("build_jobs").FindOneAndUpdate(ctx, filter,
		bson.M{"$set": bson.M{
			"status":         models.BuildRunning,
			"startedAt":      now,
			"updatedAt":      now,
			"leaseToken":     primitive.NewObjectID().Hex(),
			"leaseExpiresAt": expires,
		}},
		options.FindOneAndUpdate().SetReturnDocument(options.After).SetSort(bson.D{{Key: "createdAt", Value: 1}}),
	).Decode(&job)
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// ClaimNext claims the oldest due job without processing it. Used by tests
// to simulate a worker that claims a job and then dies.
func (p *Processor) ClaimNext(ctx context.Context) (*models.BuildJob, error) {
	return p.claim(ctx, nil)
}

// ProcessNext claims and fully processes one due job. It reports whether a
// job was found.
func (p *Processor) ProcessNext(ctx context.Context) bool {
	job, err := p.claim(ctx, nil)
	if err != nil || job == nil {
		return false
	}
	p.process(ctx, job)
	return true
}

// Run drives the worker until ctx is cancelled. With Redis, new jobs arrive
// instantly over the stream and due retries are promoted from the delayed
// set; a slow Mongo poll is the safety net for anything Redis lost. Without
// Redis it polls Mongo quickly.
func (p *Processor) Run(ctx context.Context) {
	go p.every(ctx, 30*time.Second, p.queueHeartbeat)
	go p.every(ctx, p.Lease/2, func(ctx context.Context) {
		if n, err := p.ReapExpired(ctx); err == nil && n > 0 {
			fmt.Printf("[WORKER] recovered %d job(s) with expired leases\n", n)
		}
	})

	if cache.Client != nil {
		go p.every(ctx, time.Second, func(ctx context.Context) { _, _ = p.PromoteDueRetries(ctx) })
		go p.pollLoop(ctx, 30*time.Second)
		p.streamLoop(ctx)
		return
	}

	pollMs, _ := strconv.Atoi(os.Getenv("JOB_POLL_INTERVAL_MS"))
	if pollMs <= 0 {
		pollMs = 1000
	}
	p.pollLoop(ctx, time.Duration(pollMs)*time.Millisecond)
}

func (p *Processor) every(ctx context.Context, d time.Duration, fn func(context.Context)) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

func (p *Processor) queueHeartbeat(ctx context.Context) {
	count, err := db.DB().Collection("build_jobs").CountDocuments(ctx, bson.M{"status": models.BuildQueued})
	if err == nil {
		fmt.Printf("[WORKER] Heartbeat: queued=%d\n", count)
	}
}

func (p *Processor) pollLoop(ctx context.Context, interval time.Duration) {
	p.every(ctx, interval, func(ctx context.Context) {
		for ctx.Err() == nil && p.ProcessNext(ctx) {
		}
	})
}

// streamLoop blocks on Redis Streams for job ids. A message is only a hint:
// claim still does the atomic Mongo transition, so a message for a job
// that is already taken is a harmless no-op.
func (p *Processor) streamLoop(ctx context.Context) {
	consumer := fmt.Sprintf("worker-%d", os.Getpid())
	if err := cache.Client.XGroupCreateMkStream(ctx, BuildStreamKey, buildStreamGroup, "$").Err(); err != nil &&
		!strings.Contains(err.Error(), "BUSYGROUP") {
		fmt.Printf("[WORKER] failed to create stream consumer group: %v\n", err)
	}
	for ctx.Err() == nil {
		res, err := cache.Client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    buildStreamGroup,
			Consumer: consumer,
			Streams:  []string{BuildStreamKey, ">"},
			Count:    1,
			Block:    5 * time.Second,
		}).Result()
		if err != nil {
			continue // timeout (no new messages) or transient Redis error
		}
		for _, stream := range res {
			for _, msg := range stream.Messages {
				p.handleStreamMessage(ctx, msg)
			}
		}
	}
}

func (p *Processor) handleStreamMessage(ctx context.Context, msg redis.XMessage) {
	defer cache.Client.XAck(context.Background(), BuildStreamKey, buildStreamGroup, msg.ID)
	jobIDHex, _ := msg.Values["jobId"].(string)
	oid, err := primitive.ObjectIDFromHex(jobIDHex)
	if err != nil {
		return
	}
	if job, err := p.claim(ctx, &oid); err == nil && job != nil {
		p.process(ctx, job)
	}
}

// PromoteDueRetries moves retries whose backoff has elapsed from the
// delayed set onto the stream. ZREM decides the winner when several
// workers race, so each retry is pushed once.
func (p *Processor) PromoteDueRetries(ctx context.Context) (int, error) {
	if cache.Client == nil {
		return 0, nil
	}
	due, err := cache.Client.ZRangeByScore(ctx, delayedRetryKey, &redis.ZRangeBy{
		Min: "-inf", Max: strconv.FormatInt(time.Now().UnixMilli(), 10), Count: 100,
	}).Result()
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, id := range due {
		if n, _ := cache.Client.ZRem(ctx, delayedRetryKey, id).Result(); n == 1 {
			cache.Client.XAdd(ctx, &redis.XAddArgs{Stream: BuildStreamKey, Values: map[string]interface{}{"jobId": id}})
			moved++
		}
	}
	return moved, nil
}

// ReapExpired requeues (or fails, when out of attempts) running jobs whose
// lease has expired. Jobs from before leases existed have no
// leaseExpiresAt; they are reaped once they have run far longer than any
// attempt can.
func (p *Processor) ReapExpired(ctx context.Context) (int, error) {
	now := time.Now()
	cur, err := db.DB().Collection("build_jobs").Find(ctx, bson.M{
		"status": models.BuildRunning,
		"$or": []bson.M{
			{"leaseExpiresAt": bson.M{"$lt": now}},
			{"leaseExpiresAt": bson.M{"$exists": false}, "startedAt": bson.M{"$lt": now.Add(-p.JobTimeout - p.Lease)}},
		},
	})
	if err != nil {
		return 0, err
	}
	var expired []models.BuildJob
	if err := cur.All(ctx, &expired); err != nil {
		return 0, err
	}
	n := 0
	for i := range expired {
		job := &expired[i]
		filter := bson.M{"_id": job.ID, "status": models.BuildRunning}
		if job.LeaseToken != "" {
			filter["leaseToken"] = job.LeaseToken // don't reap a lease renewed meanwhile
		} else {
			filter["leaseToken"] = bson.M{"$exists": false}
		}
		if p.finishFailure(ctx, job, filter, "lease expired: the worker running this build stopped responding", false) {
			n++
		}
	}
	return n, nil
}

// heartbeat renews this attempt's lease until stop is closed. If renewal
// stops matching (lease lost to the reaper), it cancels the job.
func (p *Processor) heartbeat(ctx context.Context, job *models.BuildJob, cancel context.CancelFunc, stop <-chan struct{}) {
	t := time.NewTicker(p.HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			c, done := fresh()
			res, err := db.DB().Collection("build_jobs").UpdateOne(c, fenced(job),
				bson.M{"$set": bson.M{"leaseExpiresAt": time.Now().Add(p.Lease)}})
			done()
			if err == nil && res.MatchedCount == 0 {
				fmt.Printf("[WORKER] lost lease on job %s, abandoning it\n", job.ID.Hex())
				cancel()
				return
			}
		}
	}
}

func (p *Processor) process(parent context.Context, job *models.BuildJob) {
	ctx, cancel := context.WithTimeout(parent, p.JobTimeout)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go p.heartbeat(ctx, job, cancel, stop)

	if err := p.build(ctx, job); err != nil {
		if parent.Err() != nil {
			// The worker is shutting down: hand the job back untouched
			// instead of burning one of its attempts.
			p.release(job)
			return
		}
		p.fail(job, err)
	}
}

// build runs one attempt and records success. It returns an error for the
// caller to classify.
func (p *Processor) build(ctx context.Context, job *models.BuildJob) error {
	p.logPush(ctx, job, "picked by worker")
	workRoot := filepath.Join(p.tmpDir, "job-"+job.ID.Hex()+"-"+job.LeaseToken)
	_ = os.RemoveAll(workRoot)
	_ = os.MkdirAll(workRoot, 0o755)
	defer os.RemoveAll(workRoot)

	working, err := p.FetchSource(ctx, job, workRoot)
	if err != nil {
		return err
	}

	p.logPush(ctx, job, "running build (npm) or static fallback...")
	logf := func(ctx context.Context, _ primitive.ObjectID, msg string) { p.logPush(ctx, job, msg) }
	if err := p.maybeBuildWithNode(ctx, job, working); err != nil {
		return err
	}
	outDir, err := pickOutputDir(working)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	if err := checkNoSymlinks(outDir); err != nil {
		return err
	}

	indexPath := filepath.Join(outDir, "index.html")
	if err := modifyIndexHTMLOnDisk(ctx, job.ID, indexPath, job, logf); err != nil {
		p.logPush(ctx, job, "[WARN] index.html modification skipped: "+err.Error())
	}

	// Each attempt publishes to its own folder (…/<version>/<leaseToken>/),
	// so a stale attempt that uploads late can't overwrite the files the
	// winning attempt recorded.
	p.logPush(ctx, job, "[STEP] Uploading build output...")
	bundleURL, err := p.uploader.PublishComponentFromDist(ctx, job.Component, job.Version+"/"+job.LeaseToken, outDir)
	if err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	c, done := fresh()
	defer done()
	res, err := db.DB().Collection("build_jobs").UpdateOne(c, fenced(job), bson.M{
		"$set": bson.M{
			"status":    models.BuildSuccess,
			"updatedAt": time.Now(),
			"endedAt":   time.Now(),
			"artifacts": models.BuildArtifact{BundleURL: bundleURL},
		},
		"$unset": bson.M{"leaseToken": "", "leaseExpiresAt": "", "nextAttemptAt": ""},
		"$push":  bson.M{"logs": "[SUCCESS] build complete: " + bundleURL},
	})
	if err != nil {
		return fmt.Errorf("recording result failed: %w", err)
	}
	if res.MatchedCount == 0 {
		fmt.Printf("[WORKER] job %s: lease lost before completion, result discarded\n", job.ID.Hex())
		return nil
	}
	notify.BuildCompleted(c, job.OwnerID, job.ComponentID, job.Component, job.Version, true)
	return nil
}

// fail records a failed attempt of the attempt holding job's lease.
func (p *Processor) fail(job *models.BuildJob, err error) {
	c, done := fresh()
	defer done()
	p.finishFailure(c, job, fenced(job), err.Error(), errors.Is(err, ErrPermanent))
}

// finishFailure requeues the job with backoff, or marks it failed when it
// is permanent or out of attempts. filter guards the write (fencing).
// It reports whether the write applied.
func (p *Processor) finishFailure(ctx context.Context, job *models.BuildJob, filter bson.M, reason string, permanent bool) bool {
	maxAttempts := job.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	attempts := job.Attempts + 1
	jobsCol := db.DB().Collection("build_jobs")
	now := time.Now()

	if !permanent && attempts < maxAttempts {
		backoff := 10 * time.Second * time.Duration(1<<(attempts-1))
		if backoff > maxBuildBackoff {
			backoff = maxBuildBackoff
		}
		next := now.Add(backoff)
		res, err := jobsCol.UpdateOne(ctx, filter, bson.M{
			"$set":   bson.M{"status": models.BuildQueued, "attempts": attempts, "nextAttemptAt": next, "updatedAt": now},
			"$unset": bson.M{"leaseToken": "", "leaseExpiresAt": ""},
			"$push": bson.M{"logs": bson.M{"$each": []string{
				"ERROR: " + reason,
				fmt.Sprintf("[RETRY] attempt %d/%d failed, retrying in %s", attempts, maxAttempts, backoff),
			}}},
		})
		if err != nil || res.MatchedCount == 0 {
			return false
		}
		if cache.Client != nil {
			cache.Client.ZAdd(ctx, delayedRetryKey, redis.Z{Score: float64(next.UnixMilli()), Member: job.ID.Hex()})
		}
		return true
	}

	final := "ERROR: " + reason
	if permanent {
		final += " (not retried: this failure won't change on retry)"
	}
	res, err := jobsCol.UpdateOne(ctx, filter, bson.M{
		"$set":   bson.M{"status": models.BuildError, "attempts": attempts, "endedAt": now, "updatedAt": now},
		"$unset": bson.M{"leaseToken": "", "leaseExpiresAt": "", "nextAttemptAt": ""},
		"$push":  bson.M{"logs": final},
	})
	if err != nil || res.MatchedCount == 0 {
		return false
	}
	notify.BuildCompleted(ctx, job.OwnerID, job.ComponentID, job.Component, job.Version, false)
	return true
}

// release returns a job to the queue without counting an attempt (used on
// worker shutdown) and nudges the stream so another worker picks it up.
func (p *Processor) release(job *models.BuildJob) {
	c, done := fresh()
	defer done()
	res, err := db.DB().Collection("build_jobs").UpdateOne(c, fenced(job), bson.M{
		"$set":   bson.M{"status": models.BuildQueued, "updatedAt": time.Now()},
		"$unset": bson.M{"leaseToken": "", "leaseExpiresAt": "", "nextAttemptAt": ""},
		"$push":  bson.M{"logs": "[RELEASED] worker shut down mid-build; job returned to the queue"},
	})
	if err == nil && res.MatchedCount == 1 && cache.Client != nil {
		cache.Client.XAdd(c, &redis.XAddArgs{Stream: BuildStreamKey, Values: map[string]interface{}{"jobId": job.ID.Hex()}})
	}
}

// fetchFromGitHub downloads and extracts the linked repo at the job's commit.
func (p *Processor) fetchFromGitHub(ctx context.Context, job *models.BuildJob, workRoot string) (string, error) {
	p.logPush(ctx, job, "downloading repository zip...")
	zipPath, err := downloadRepoZip(ctx, workRoot, job.Repo.Owner, job.Repo.Repo, firstNonEmpty(job.Repo.Commit, job.Repo.Ref, "main"), job.OwnerID)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	p.logPush(ctx, job, "extracting zip...")
	topDir, err := unzip(zipPath, workRoot)
	if err != nil {
		return "", fmt.Errorf("unzip failed: %w", err)
	}
	working := topDir
	if job.Repo.Path != "" {
		working = filepath.Join(topDir, job.Repo.Path)
		if rel, err := filepath.Rel(topDir, working); err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("%w: invalid path in repo: %s", ErrPermanent, job.Repo.Path)
		}
	}
	if _, err := os.Stat(working); err != nil {
		return "", fmt.Errorf("%w: invalid path in repo: %s", ErrPermanent, job.Repo.Path)
	}
	return working, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
