# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository layout

This is a two-service monorepo, each with its own dependency management and dev server:

- `StoreHUBXBackend/` — Go (Fiber) API + background worker, MongoDB + RustFS (S3-compatible) storage
- `StoreHUBClient/` — Next.js 15 (App Router, React 19) frontend

There is no root-level build tool; each service is run independently from its own directory.

## Commands

### Backend (`StoreHUBXBackend/`)

```bash
docker-compose up -d          # start MongoDB, RustFS (S3), Redis
go mod download
go run cmd/main.go            # API server on :8080 (Swagger at /docs/index.html)
go run cmd/worker/main.go     # background build worker (separate process)
```

`docker-compose.yml` also starts Redis (cache + build queue) and RustFS, an S3-compatible object store on `:9000` (web console at `http://localhost:9001/rustfs/console/`, `minioadmin`/`minioadmin`). RustFS replaced MinIO, which no longer publishes public images; the code only uses the generic S3 API via the `minio-go` client library, so any S3 server works.

Config comes from `.env` (copy `.env.example`), loaded via `internal/config`. Always reach MongoDB through `db.DB()` (database name from `MONGO_DB`, default `storehub`), never a hardcoded database name — the API, worker and tests share that one resolver. `MONGO_URI` is required — the process calls `log.Fatal` if it's unset. Required GitHub OAuth vars: `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `GITHUB_REDIRECT_URL`. `REDIS_ADDR` is optional — caching and the fast build-notification path degrade gracefully to no-cache/poll-only when Redis isn't reachable. `CDN_BASE_URL` is optional and overrides `S3_PUBLIC_BASE_URL` for bundle URLs when set.

One-off maintenance scripts live under `cmd/` (run with `go run cmd/<name>/main.go`). Both default to a dry run and only write when passed `--apply`:
- `cmd/normalize_frameworks` — backfills the lowercase/trim/de-dupe of `frameworks`/`tags` that `CreateComponent` now applies on write, so older docs are reachable by the list filter.
- `cmd/reap_orphans` — deletes `components/{slug}/` prefixes in S3 storage whose component no longer exists in Mongo. `DeleteComponent` does not do this itself.

Tests: `make test` (or `cd StoreHUBXBackend && go test -race -p 1 ./...` with the Docker infra running). `internal/integration` drives the real Fiber app (`internal/server.New()`) against real Mongo/Redis/S3 (RustFS) using its own database (`storehub_test`) and Redis DB 15, so it never touches dev data; it skips when Mongo is unreachable unless `STOREHUBX_REQUIRE_INFRA=1` (set in CI). CI (`.github/workflows/ci.yml`) runs vet + tests for the backend and lint + `tsc` for the frontend on every push.

### Frontend (`StoreHUBClient/`)

```bash
npm install
npm run dev      # localhost:3000
npm run build
npm run lint
```

Requires `NEXT_PUBLIC_API_BASE` in `.env.local` (e.g. `http://localhost:8080`), pointing at the backend API base URL used by `lib/api.ts`.

No test suite currently exists in the frontend.

## Architecture

### Backend: request flow and build pipeline

Fiber app (`cmd/main.go`) wires global middleware (CORS, recover, request logger, `middleware.RateLimiter`) and delegates routing to `internal/routes/routes.go`, which is the single source of truth for the route table — check it first when tracing any endpoint. Routes split into: fully public reads (components list/get, versions, comments, ratings, builds, preview redirect, the GitHub push webhook), public-but-identity-aware reads behind `middleware.OptionalAuth` (`GET /components/:slug`, `GET /users/:id` — parses a bearer token if present but never rejects, so an owner/collaborator sees their own private components while everyone else gets a 404), and an `/api` group behind `middleware.JWTProtected` for everything else (writes, likes, comments, ratings, GitHub linking, deploys, builds, visibility/collaborators, profile, analytics).

Auth is GitHub OAuth (`internal/auth/oauth.go`) exchanged for a JWT (`internal/auth/jwt.go`); the JWT is what protects the `/api` group and its claims (`user_id`, `email`) are attached to `c.Locals` by both `JWTProtected` and `OptionalAuth`.

Components can be linked to a GitHub repo/path/ref (`internal/handlers/component_link_handler.go`, which also generates a per-link `RepoLink.WebhookSecret`), and building a version is an async job, not a synchronous request:

1. `POST /api/components/:slug/versions/:version/build` (or `AddVersion`/`AutoDeploy`/the GitHub push webhook) first checks `internal/handlers/build_cache.go`'s `findCachedBuild` — if the same component already has another version built successfully from the identical `commitSha`, its `previewUrl`/`buildState` are copied onto the new version and no job is created. Otherwise a `BuildJob` is inserted into MongoDB (`internal/models/build_job.go`) via `internal/handlers/build.go`, and `notifyWorker` pushes the job id onto the `builds:stream` Redis Stream for near-instant pickup.
2. The separate `cmd/worker/main.go` process (`internal/worker/worker.go`) primarily blocks on `XREADGROUP` against that stream, with a slower (30s) Mongo poll kept as a fallback sweep for retries and any dropped stream messages (falls back to poll-only if Redis isn't configured). It claims a job atomically via `FindOneAndUpdate`, then: downloads the linked GitHub repo as a zip (preferring `Repo.Commit`, then `Ref`), unzips it, runs an npm build (or falls back to serving static files) via `internal/worker/fs_build.go` — the npm build itself runs in a throwaway Docker container (`internal/worker/sandbox.go`: no worker env vars, CPU/memory/pids/time limits, all capabilities dropped, source copied in with `docker cp`, only `dist/`/`build/` copied out, symlinked output rejected), rewrites `index.html`/asset paths, and uploads the output to S3 storage (`internal/storage`).
3. Progress is streamed by pushing log lines onto the `BuildJob.logs` array in Mongo as the job runs; status transitions are `BuildQueued -> BuildRunning -> BuildSuccess|BuildError`. Delivery is at-least-once with leases (`internal/worker/worker.go`): a claim stamps a fresh `leaseToken` (fencing token) and `leaseExpiresAt`, the worker renews the lease while working, and `ReapExpired` requeues running jobs whose lease ran out (crashed/stalled worker). Every status/log/result write is filtered on `leaseToken`, and each attempt uploads to its own folder (`components/<slug>/<version>/<leaseToken>/`), so a stale attempt can't overwrite a newer one. Transient failures are requeued with exponential backoff (`10s * 2^attempt`, capped at 2m) and scheduled in the Redis sorted set `builds:delayed`, which `PromoteDueRetries` moves onto the stream when due; failures wrapping `worker.ErrPermanent` (failed build, missing commit, no output) are not retried. On shutdown a running job is released back to the queue without using an attempt. On success the job's `artifacts.bundleUrl` is set.
4. `GET /builds/:id` and `GET /components/:slug/versions/:version/builds` (both public, no auth) let the frontend poll job status/logs.

Automatic version numbers come from an atomic counter on the component (`components.autoVersion`, `internal/handlers/versioning.go`), never from read-latest-then-add-one; the unique indexes on `component_versions` are the final arbiter and duplicate-key errors map to 409. The push webhook records each `X-GitHub-Delivery` id in `webhook_deliveries` (unique `_id`, 7-day TTL) so redeliveries are no-ops.

Because the API server and worker are separate binaries/processes, any change to `BuildJob` fields or status semantics must stay in sync across `internal/handlers/build.go` and `internal/worker/worker.go`.

Storage is abstracted behind the `storage.Uploader` interface (`internal/storage/storage.go`, implemented by `internal/storage/s3.go` for any S3-compatible store) — build output publishing goes through `PublishComponentFromDist`, which also rewrites asset paths so uploaded bundles are served correctly. `publicURL()` honors `CDN_BASE_URL` when set, falling back to `S3_PUBLIC_BASE_URL`.

The bucket is private (no anonymous read/list). Browsers load bundles only through the API proxy in `internal/handlers/bundle_handler.go`: `GET /bundles/components/:slug/:version/*` for public components and `GET /bundles/t/:token/components/:slug/:version/*` for private ones, where the token is a 30-minute bundle JWT (minted by `RedirectPreview` from a 2-minute preview token) carried in the path so relative asset URLs inherit it. Build responses rewrite stored `artifacts.bundleUrl` to the proxy URL (`withProxyURLs`). JWTs carry an audience (`api`, `preview`, `bundle`) and are only accepted where that audience is expected — see `internal/auth/jwt.go`.

`internal/cache` (Redis) fronts `GET /components` and `GET /components/:slug`: the list cache is invalidated via an epoch counter bumped on any component write, the per-slug cache via `invalidateComponentCaches` on likes/ratings/repo-link/visibility changes. Both degrade gracefully to no-cache if Redis is unreachable. Private components (see below) skip the Redis fast-path read/write entirely since serving a cached blob would bypass the per-request authorization check.

Component visibility (`internal/handlers/component_visibility_handler.go`) adds a `Visibility` (`"public"|"private"`) and `Collaborators []string` to `Component`; `GET /components` always excludes private docs, `GET /components/:slug` and `GET /users/:id` use `OptionalAuth` to allow the owner/a collaborator through. Ratings (`internal/handlers/rating_handler.go`, `internal/models/rating.go`) are upserted one-per-user-per-component and denormalize `Component.AverageRating`/`RatingCount`, mirroring the existing likes pattern. Owner analytics (`internal/handlers/analytics_handler.go`) aggregate view/like/rating/comment totals per owned component — MVP totals only, no time-series tracking.

Response shape is consistent across all handlers: `{"success": true, "data": ...}` or `{"success": false, "error": ...}`, enforced via helpers in `internal/utils/response.go`.

### Frontend: API + auth pattern

`lib/api.ts` is the single typed API client (`apiFetch<T>`) — all backend calls go through it, using `NEXT_PUBLIC_API_BASE` and normalizing errors into `ApiError`. Request/response types live in `types/index.ts` and mirror the backend's Go models/response envelopes; when the backend response shape changes, update both.

Auth state (JWT + user) is a persisted Zustand store (`lib/store.ts`, `useAuth`), not React context — components read the token from this store and pass it as `authToken` into `apiFetch` calls for protected endpoints. `hooks/use-api.ts` wraps common data-fetching patterns on top of `lib/api.ts`.

Routing uses the App Router with a `(private)` route group (`app/(private)/`) for authenticated pages (component creation, `/me`, `/me/analytics`) versus public component/user browsing pages (`app/components`, `app/users`) — check which group a page belongs in before adding new routes, since it determines whether auth is expected to already be present client-side.

Icons are `lucide-react` throughout (no emoji, no ad-hoc inline SVGs for generic icons — hand-drawn brand marks like the GitHub logo are the exception). Shared brutalist-styled UI primitives live in `components/common/`: `badge.tsx` (framework/tag chips), `pagination.tsx`, `rating-stars.tsx`, `component-card-skeleton.tsx` — reuse these instead of hand-rolling equivalents.
