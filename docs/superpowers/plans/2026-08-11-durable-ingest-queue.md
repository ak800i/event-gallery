# Durable Ingest Queue Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a completed tus upload durably owned by the application before the browser is told it succeeded, and never delete the only complete copy of a guest's file because of a transient error.

**Architecture:** A blocking tusd `pre-finish` hook fsyncs the uploaded source and commits a `pending` row in a new SQLite `upload_jobs` queue; only then may the final PATCH return success. A fixed pool of leased workers running on the application's lifetime context — never a request context — copies each source into permanent storage with a full fsync barrier, publishes it in one transaction, and deletes the source only after that transaction commits.

**Tech Stack:** Go 1.25, Chi v5.3.1, modernc.org/sqlite v1.54.0, tusd v2.10.0 (filestore), Docker Compose, Alpine 3.22 runtime.

## Global Constraints

- Source spec: `docs/superpowers/specs/2026-08-09-durable-media-ingest-heic-previews-design.md`. Where this plan and the spec disagree, the spec wins — stop and report the conflict.
- Exactly one `app` container and one `tusd` container per stack. Do not add coordination for multiple app instances.
- No external queue, broker, object store, or new service. No new secret.
- Go server stays `CGO_ENABLED=0`.
- Every `INTEGER` timestamp in the new `upload_jobs` table is signed UTC Unix **microseconds**. Existing `media_items` timestamps stay TEXT RFC3339Nano — do not change them.
- A source file may be deleted only after a `media_items` row for that content is committed, or after durable discard intent from deterministic client rejection or user cancellation. Nothing else authorizes deletion, ever.
- Absence of a file is never sufficient evidence to delete a row or another copy.
- The app never issues a tus termination for a job whose data path it has not just observed to exist.
- Transient failures retry indefinitely with capped backoff. There is no failure count that causes deletion.
- Timing inequality that must hold: `75s app budget < 90s hook timeout < 100s edge window`.
- Go is not installed on the developer workstation. Run backend commands inside the toolchain image from the repo root:
  `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 go test ./... -race`
  Use the Debian-based `golang:1.25` image, **not** `golang:1.25-alpine`: `-race` requires cgo and a C toolchain, and the Alpine image ships no compiler. Without it every `-race` command in this plan fails with "race is only supported on ..."/missing gcc. Plain (non-race) runs work on either image. CI runs `go test ./... && go vet ./... && go build ./cmd/server`.
- Commit after every task. Use Conventional Commit prefixes (`feat:`, `fix:`, `test:`, `chore:`).
- Code blocks show function bodies, not whole files. When a step adds a function to an existing file, add the imports it needs and remove any that become unused; `go build` will tell you which.
- Several existing tests assert behavior this plan deliberately removes. Each such task has an explicit step listing them. Do not "fix" them by restoring the old behavior.

## Plan Set

This spec is split into three plans. This is plan 1 of 3.

1. **Durable ingest queue (this plan)** — backend queue, durability barrier, workers, publication, recovery, status API.
2. **HEIC/HEIF previews** — `heif-preview` helper, build stage, preview endpoint, lightbox wiring.
3. **Upload client resilience and load-test oracle** — `Retry-After`-aware tus wrapper, status polling, `tus_battle.py` completion assertions.

Plans 2 and 3 depend on this one. Do not start them until this plan's tasks all pass.

## File Structure

**New files**

- `backend/internal/db/migrations/0004_durable_upload_jobs_and_previews.sql` — additive schema.
- `backend/internal/store/upload_jobs.go` — all `upload_jobs` data access. Single responsibility: durable queue state transitions, each one conditional and fenced by a lease token.
- `backend/internal/store/upload_jobs_test.go`
- `backend/internal/tussidecar/sidecar.go` — bounded parsing of tusd `.info` sidecars, shared by the reconciler and the existing incomplete-upload janitor.
- `backend/internal/tussidecar/sidecar_test.go`
- `backend/internal/media/durable.go` — fsync/copy/rename primitives and `PrepareOriginal`, which never removes its source.
- `backend/internal/media/durable_test.go`
- `backend/internal/ingest/manager.go` — lifecycle, config, wake channel, worker pool, readiness.
- `backend/internal/ingest/durability.go` — the idempotent pre-finish durability operation and its registry.
- `backend/internal/ingest/health.go` — storage health gate.
- `backend/internal/ingest/process.go` — claim → prepare → publish → cleanup.
- `backend/internal/ingest/recover.go` — startup inventory and periodic reconciliation.
- `backend/internal/ingest/*_test.go`
- `backend/internal/httpapi/uploads_status.go` — `POST /api/uploads/status`.
- `backend/internal/httpapi/uploads_status_test.go`

**Modified files**

- `backend/internal/db/db.go` — add `synchronous=FULL` to the DSN.
- `backend/internal/config/config.go` — 12 new settings.
- `backend/internal/httpapi/tus_hooks.go` — rewrite `pre-create`, add the blocking `pre-finish` barrier, demote `post-finish` to a wake signal.
- `backend/internal/httpapi/tus_proxy.go` — completion fence on HEAD/PATCH, DELETE as cancellation intent.
- `backend/internal/httpapi/server.go` — hold the manager, add `/readyz`, register the status route.
- `backend/internal/httpapi/storage_cleanup.go` — use the shared sidecar parser and the manager's claim API.
- `backend/cmd/server/main.go` — construct, start, and gracefully stop the manager.
- `deploy/tusd-entrypoint.sh` — new hooks, forwarded header, derived timeouts.
- `docker-compose.yml` — new environment variables.
- `ARCHITECTURE.md` — the tus volume is no longer transient.

---

### Task 1: Schema and durability pragmas

**Files:**
- Create: `backend/internal/db/migrations/0004_durable_upload_jobs_and_previews.sql`
- Modify: `backend/internal/db/db.go:26`
- Test: `backend/internal/db/db_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: table `upload_jobs`, column `media_items.has_preview`, and a database opened with `journal_mode=WAL` and `synchronous=FULL`.

- [ ] **Step 1: Write the failing test**

Append to `backend/internal/db/db_test.go`:

```go
func TestOpenAppliesDurabilityPragmas(t *testing.T) {
	sqlDB, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqlDB.Close()

	var journalMode string
	if err := sqlDB.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	// 2 == FULL. Without it a power loss can lose the very commit that
	// authorized deleting an upload's only source.
	var synchronous int
	if err := sqlDB.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatalf("synchronous: %v", err)
	}
	if synchronous != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL)", synchronous)
	}
}

func TestMigrationCreatesUploadJobs(t *testing.T) {
	sqlDB, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqlDB.Close()

	var hasPreview int
	err = sqlDB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('media_items') WHERE name = 'has_preview'`).Scan(&hasPreview)
	if err != nil || hasPreview != 1 {
		t.Fatalf("media_items.has_preview missing: count=%d err=%v", hasPreview, err)
	}

	for _, index := range []string{"idx_upload_jobs_due", "idx_upload_jobs_lease", "idx_upload_jobs_terminal"} {
		var n int
		if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s missing: count=%d err=%v", index, n, err)
		}
	}

	// The paired-lease CHECK must reject a half-set lease.
	_, err = sqlDB.Exec(`INSERT INTO upload_jobs
		(upload_id, media_id, status, original_filename, expected_size, lease_token, next_attempt_at, created_at, updated_at)
		VALUES ('u1', 'm1', 'pending', 'a.jpg', 10, 'tok', 0, 0, 0)`)
	if err == nil {
		t.Error("expected CHECK violation for lease_token without lease_until")
	}
}
```

Add `"strings"` to that file's imports if it is not already there.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/db/ -run 'Durability|UploadJobs' -v`
Expected: FAIL — `synchronous = 1, want 2` and `no such table: upload_jobs`.

- [ ] **Step 3: Create the migration**

Create `backend/internal/db/migrations/0004_durable_upload_jobs_and_previews.sql`:

```sql
-- Durable upload queue. A tus upload reported successful to the browser must
-- already have a committed row here, so that no server-side deadline can
-- destroy the guest's only copy.

ALTER TABLE media_items ADD COLUMN has_preview INTEGER NOT NULL DEFAULT 0;

CREATE TABLE upload_jobs (
    upload_id TEXT PRIMARY KEY,
    media_id TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (
        status IN (
            'uploading', 'pending', 'processing', 'cleanup',
            'complete', 'discarding', 'discarded'
        )
    ),
    original_filename TEXT NOT NULL,
    stored_filename TEXT,
    mime_type TEXT,
    expected_size INTEGER NOT NULL CHECK (expected_size > 0),
    declared_sha256 TEXT NOT NULL DEFAULT '',
    authoritative_sha256 TEXT,
    guest_name TEXT NOT NULL DEFAULT '',
    uploader_ip TEXT NOT NULL DEFAULT '',
    source_completed_at INTEGER,
    prepared_at INTEGER,
    cancellation_requested_at INTEGER,
    result_media_id TEXT,
    terminal_reason TEXT NOT NULL DEFAULT '',
    lease_token TEXT,
    lease_until INTEGER,
    next_attempt_at INTEGER NOT NULL,
    processing_failures INTEGER NOT NULL DEFAULT 0,
    conversion_failures INTEGER NOT NULL DEFAULT 0,
    cleanup_failures INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    terminal_at INTEGER,
    CHECK ((lease_token IS NULL) = (lease_until IS NULL))
);

CREATE INDEX idx_upload_jobs_due
  ON upload_jobs(status, next_attempt_at);

CREATE INDEX idx_upload_jobs_lease
  ON upload_jobs(status, lease_until);

CREATE INDEX idx_upload_jobs_terminal
  ON upload_jobs(terminal_at)
  WHERE status IN ('complete', 'discarded');
```

- [ ] **Step 4: Add the durability pragma**

In `backend/internal/db/db.go`, replace the `dsn` assignment:

```go
	// synchronous=FULL is mandatory, not tuning: the queue commits are what
	// authorize deleting a guest's only uploaded copy, so losing one to a
	// power cut would lose the file.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)", path)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/db/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/db/
git commit -m "feat: add durable upload_jobs schema and synchronous=FULL"
```

---

### Task 2: Ingest configuration

**Files:**
- Modify: `backend/internal/config/config.go`
- Test: `backend/internal/config/config_test.go`

**Interfaces:**
- Consumes: Task 1's schema (only conceptually).
- Produces: on `config.Config` — `MediaProcessingWorkers int`, `MediaProcessingTimeout time.Duration`, `UploadDurabilityWait time.Duration`, `UploadDurabilityWorkers int`, `UploadRetryMaxBackoff time.Duration`, `IngestReconcileInterval time.Duration`, `IngestMinFreeBytes int64`, `UploadJobRetention time.Duration`, `UploadStatusRateLimitPerMinute int`.

`IMAGE_MAX_SOURCE_PIXELS`, `HEIC_MAX_SOURCE_PIXELS`, `MEDIA_TOOL_MEMORY_BYTES`, and `MEDIA_TOOL_LOG_BYTES` are deliberately **not** added here. Their only consumers are derivative generation and the HEIC helper, which belong to plan 2; adding them now would ship four settings that nothing reads.

- [ ] **Step 1: Write the failing test**

Append to `backend/internal/config/config_test.go`. Note the existing `clearEnv(t)` helper and the two settings `Load()` requires — without them `Load()` fails validation before it ever reaches the new fields. Also add the twelve new keys to `clearEnv`'s list so these tests cannot be polluted by the developer's shell.

```go
func TestLoad_IngestDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	t.Setenv("MAX_UPLOAD_BYTES", "1000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MediaProcessingWorkers != 2 {
		t.Errorf("MediaProcessingWorkers = %d, want 2", cfg.MediaProcessingWorkers)
	}
	if cfg.UploadDurabilityWait != 75*time.Second {
		t.Errorf("UploadDurabilityWait = %v, want 75s", cfg.UploadDurabilityWait)
	}
	// Default floor is twice the largest single upload: the incoming copy
	// plus the permanent copy.
	if cfg.IngestMinFreeBytes != 2000 {
		t.Errorf("IngestMinFreeBytes = %d, want 2000", cfg.IngestMinFreeBytes)
	}
	if cfg.UploadJobRetention != 30*24*time.Hour {
		t.Errorf("UploadJobRetention = %v, want 720h", cfg.UploadJobRetention)
	}
}

func TestLoad_IngestOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	t.Setenv("MEDIA_PROCESSING_WORKERS", "6")
	t.Setenv("INGEST_MIN_FREE_BYTES", "12345")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MediaProcessingWorkers != 6 {
		t.Errorf("MediaProcessingWorkers = %d, want 6", cfg.MediaProcessingWorkers)
	}
	if cfg.IngestMinFreeBytes != 12345 {
		t.Errorf("IngestMinFreeBytes = %d, want 12345", cfg.IngestMinFreeBytes)
	}
}

func TestLoad_RejectsDurabilityWaitAboveHookTimeout(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	t.Setenv("UPLOAD_DURABILITY_WAIT_SECONDS", "120")

	// 75s < 90s < 100s is load-bearing: a budget above the hook timeout means
	// tusd cuts the request before we can relay a retryable 503.
	if _, err := Load(); err == nil {
		t.Fatal("expected a budget above the 90s hook timeout to be rejected")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/config/ -run Ingest -v`
Expected: FAIL — `cfg.MediaProcessingWorkers undefined`.
- [ ] **Step 3: Add the fields and parsing**

Add to the `Config` struct:

```go
	// Durable ingest queue.
	MediaProcessingWorkers         int
	MediaProcessingTimeout         time.Duration
	UploadDurabilityWait           time.Duration
	UploadDurabilityWorkers        int
	UploadRetryMaxBackoff          time.Duration
	IngestReconcileInterval        time.Duration
	IngestMinFreeBytes             int64
	UploadJobRetention             time.Duration
	UploadStatusRateLimitPerMinute int
```

Populate them in `Load()` **after** `MaxUploadBytes` is assigned, because the free-space default is derived from it. Note that `envInt` and `envInt64` in this file return `(value, error)` — they are not bare getters. Add a local helper next to them to keep the call sites readable:

```go
// minutes/seconds/days helpers keep the many duration settings from turning
// Load() into forty lines of identical error plumbing.
func envDuration(key string, defUnits int, unit time.Duration) (time.Duration, error) {
	units, err := envInt(key, defUnits)
	if err != nil {
		return 0, err
	}
	return time.Duration(units) * unit, nil
}
```

Then, following the file's existing `if x, err := ...; err != nil` house style:

```go
	var err error
	if cfg.MediaProcessingWorkers, err = envInt("MEDIA_PROCESSING_WORKERS", 2); err != nil {
		return nil, err
	}
	if cfg.MediaProcessingTimeout, err = envDuration("MEDIA_PROCESSING_TIMEOUT_MINUTES", 60, time.Minute); err != nil {
		return nil, err
	}
	if cfg.UploadDurabilityWait, err = envDuration("UPLOAD_DURABILITY_WAIT_SECONDS", 75, time.Second); err != nil {
		return nil, err
	}
	if cfg.UploadDurabilityWorkers, err = envInt("UPLOAD_DURABILITY_WORKERS", 2); err != nil {
		return nil, err
	}
	if cfg.UploadRetryMaxBackoff, err = envDuration("UPLOAD_RETRY_MAX_BACKOFF_MINUTES", 15, time.Minute); err != nil {
		return nil, err
	}
	if cfg.IngestReconcileInterval, err = envDuration("INGEST_RECONCILE_INTERVAL_SECONDS", 15, time.Second); err != nil {
		return nil, err
	}
	if cfg.IngestMinFreeBytes, err = envInt64("INGEST_MIN_FREE_BYTES", 2*cfg.MaxUploadBytes); err != nil {
		return nil, err
	}
	if cfg.UploadJobRetention, err = envDuration("UPLOAD_JOB_RETENTION_DAYS", 30, 24*time.Hour); err != nil {
		return nil, err
	}
	if cfg.UploadStatusRateLimitPerMinute, err = envInt("UPLOAD_STATUS_RATE_LIMIT_PER_MINUTE", 6000); err != nil {
		return nil, err
	}
```

If `Load()` already declares `err` in scope, drop the `var err error` line.

Then validate the timing inequality from the spec, near the other validation:

```go
	// 75s app budget < 90s hook timeout < 100s edge window. If the budget is
	// raised past the hook timeout, tusd cuts the request before we can relay
	// a 503 and the browser sees an opaque failure instead of backpressure.
	if cfg.UploadDurabilityWait >= 90*time.Second {
		return nil, fmt.Errorf("UPLOAD_DURABILITY_WAIT_SECONDS must be below the 90s tusd hook timeout, got %v", cfg.UploadDurabilityWait)
	}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/config/
git commit -m "feat: add durable ingest configuration"
```

---

### Task 3: Upload job store

This is the heart of the design. Every transition is conditional; a worker that lost its lease must affect zero rows.

**Files:**
- Create: `backend/internal/store/upload_jobs.go`
- Test: `backend/internal/store/upload_jobs_test.go`

**Interfaces:**
- Consumes: Task 1's `upload_jobs` table; the existing `newTestStore(t)` helper in `store_test.go`.
- Produces:
  - `type JobStatus string` with constants `JobUploading`, `JobPending`, `JobProcessing`, `JobCleanup`, `JobComplete`, `JobDiscarding`, `JobDiscarded`.
  - `type UploadJob struct` (fields below).
  - `var ErrNotClaimed error`
  - `func NowMicros() int64`
  - `func NewLeaseToken() (string, error)`
  - `func (s *Store) CreateUploadingJob(ctx context.Context, j *UploadJob) error`
  - `func (s *Store) GetUploadJob(ctx context.Context, uploadID string) (*UploadJob, error)` — returns `(nil, nil)` when absent.
  - `func (s *Store) PromoteToPending(ctx context.Context, uploadID string, now int64) error`
  - `func (s *Store) ClaimNextJob(ctx context.Context, from JobStatus, to JobStatus, now int64, leaseFor time.Duration) (*UploadJob, error)` — returns `(nil, nil)` when nothing is due.
  - `func (s *Store) ReleaseForRetry(ctx context.Context, uploadID, leaseToken string, status JobStatus, nextAttemptAt int64, counter string, lastError string) error`
  - `func (s *Store) RequestCancellation(ctx context.Context, uploadID string, now int64) error`
  - `func (s *Store) FinishJob(ctx context.Context, uploadID, leaseToken string, status JobStatus, reason string, now int64) error`
  - `func (s *Store) RequeueStartup(ctx context.Context, now int64) (int64, error)`
  - `func (s *Store) DeleteTerminalJobsBefore(ctx context.Context, cutoff int64, limit int) (int64, error)`

- [ ] **Step 1: Write the failing tests**

Create `backend/internal/store/upload_jobs_test.go`:

```go
package store

import (
	"context"
	"testing"
	"time"
)

func seedUploading(t *testing.T, s *Store, uploadID, mediaID string) *UploadJob {
	t.Helper()
	job := &UploadJob{
		UploadID:         uploadID,
		MediaID:          mediaID,
		OriginalFilename: "photo.jpg",
		ExpectedSize:     1024,
		DeclaredSHA256:   "abc",
		GuestName:        "Ana",
		UploaderIP:       "10.0.0.1",
	}
	if err := s.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	return job
}

func TestCreateAndGetUploadJob(t *testing.T) {
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")

	got, err := s.GetUploadJob(context.Background(), "u1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != JobUploading {
		t.Errorf("status = %q, want uploading", got.Status)
	}
	if got.ExpectedSize != 1024 || got.MediaID != "m1" {
		t.Errorf("unexpected job: %+v", got)
	}

	missing, err := s.GetUploadJob(context.Background(), "nope")
	if err != nil || missing != nil {
		t.Errorf("missing job: got %+v err %v, want nil nil", missing, err)
	}
}

func TestPromoteToPendingIsBlockedByCancellation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")

	if err := s.RequestCancellation(ctx, "u1", NowMicros()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := s.PromoteToPending(ctx, "u1", NowMicros()); err != ErrNotClaimed {
		t.Fatalf("promote after cancel = %v, want ErrNotClaimed", err)
	}

	got, _ := s.GetUploadJob(ctx, "u1")
	if got.Status != JobUploading {
		t.Errorf("status = %q, want uploading (promotion must fail closed)", got.Status)
	}
}

func TestClaimIsExclusiveAndFenced(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")
	if err := s.PromoteToPending(ctx, "u1", NowMicros()); err != nil {
		t.Fatalf("promote: %v", err)
	}

	now := NowMicros()
	first, err := s.ClaimNextJob(ctx, JobPending, JobProcessing, now, time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim: %+v %v", first, err)
	}
	if first.LeaseToken == "" {
		t.Fatal("claim must issue a lease token")
	}

	second, err := s.ClaimNextJob(ctx, JobPending, JobProcessing, now, time.Minute)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second != nil {
		t.Fatal("a job must be claimable by exactly one worker")
	}

	// A stale token must not be able to publish, clean, or discard.
	if err := s.FinishJob(ctx, "u1", "stale-token", JobComplete, "", NowMicros()); err != ErrNotClaimed {
		t.Fatalf("stale finish = %v, want ErrNotClaimed", err)
	}
	if err := s.FinishJob(ctx, "u1", first.LeaseToken, JobComplete, "", NowMicros()); err != nil {
		t.Fatalf("owner finish: %v", err)
	}
}

func TestExpiredLeaseBecomesClaimableAgain(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")
	_ = s.PromoteToPending(ctx, "u1", NowMicros())

	now := NowMicros()
	if _, err := s.ClaimNextJob(ctx, JobPending, JobProcessing, now, time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}

	later := now + int64(2*time.Minute/time.Microsecond)
	reclaimed, err := s.ClaimNextJob(ctx, JobProcessing, JobProcessing, later, time.Minute)
	if err != nil || reclaimed == nil {
		t.Fatalf("expired lease must be reclaimable: %+v %v", reclaimed, err)
	}
}

func TestReleaseForRetryIncrementsNamedCounter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")
	_ = s.PromoteToPending(ctx, "u1", NowMicros())
	claimed, _ := s.ClaimNextJob(ctx, JobPending, JobProcessing, NowMicros(), time.Minute)

	next := NowMicros() + 1_000_000
	if err := s.ReleaseForRetry(ctx, "u1", claimed.LeaseToken, JobPending, next, "processing_failures", "disk hiccup"); err != nil {
		t.Fatalf("release: %v", err)
	}

	got, _ := s.GetUploadJob(ctx, "u1")
	if got.Status != JobPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.ProcessingFailures != 1 || got.ConversionFailures != 0 {
		t.Errorf("counters = proc %d conv %d, want 1 and 0", got.ProcessingFailures, got.ConversionFailures)
	}
	if got.LeaseToken != "" {
		t.Error("retry must clear the lease")
	}
}

func TestReleaseForRetryCanKeepTheCurrentStage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")
	_ = s.PromoteToPending(ctx, "u1", NowMicros())
	claimed, _ := s.ClaimNextJob(ctx, JobPending, JobCleanup, NowMicros(), time.Minute)

	// A cleanup failure must not demote an already-published job back to
	// pending, which would re-run processing against a deleted source.
	if err := s.ReleaseForRetry(ctx, "u1", claimed.LeaseToken, JobCleanup, NowMicros(), "cleanup_failures", "fsync failed"); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, _ := s.GetUploadJob(ctx, "u1")
	if got.Status != JobCleanup {
		t.Errorf("status = %q, want cleanup", got.Status)
	}
	if got.CleanupFailures != 1 {
		t.Errorf("cleanup_failures = %d, want 1", got.CleanupFailures)
	}
}

func TestRequeueStartupResetsProcessing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")
	_ = s.PromoteToPending(ctx, "u1", NowMicros())
	_, _ = s.ClaimNextJob(ctx, JobPending, JobProcessing, NowMicros(), time.Hour)

	if _, err := s.RequeueStartup(ctx, NowMicros()); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	got, _ := s.GetUploadJob(ctx, "u1")
	if got.Status != JobPending {
		t.Errorf("status = %q, want pending after restart", got.Status)
	}
	// A redeploy must not wait out a wall-clock lease.
	if got.LeaseToken != "" {
		t.Error("startup must clear stale leases")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/store/ -run Upload -v`
Expected: FAIL — `undefined: UploadJob`.

- [ ] **Step 3: Implement the store**

Create `backend/internal/store/upload_jobs.go`:

```go
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// JobStatus mirrors the CHECK constraint on upload_jobs.status.
type JobStatus string

const (
	JobUploading  JobStatus = "uploading"
	JobPending    JobStatus = "pending"
	JobProcessing JobStatus = "processing"
	JobCleanup    JobStatus = "cleanup"
	JobComplete   JobStatus = "complete"
	JobDiscarding JobStatus = "discarding"
	JobDiscarded  JobStatus = "discarded"
)

// ErrNotClaimed means a conditional update matched zero rows: another worker
// owns the job, the caller's lease token is stale, or the row has moved on.
// Callers must treat it as "you do not own this" and never as "retry harder".
var ErrNotClaimed = errors.New("upload job not claimed")

// UploadJob is one row of the durable ingest queue. All timestamps are signed
// UTC Unix microseconds.
type UploadJob struct {
	UploadID                string
	MediaID                 string
	Status                  JobStatus
	OriginalFilename        string
	StoredFilename          string
	MimeType                string
	ExpectedSize            int64
	DeclaredSHA256          string
	AuthoritativeSHA256     string
	GuestName               string
	UploaderIP              string
	SourceCompletedAt       *int64
	PreparedAt              *int64
	CancellationRequestedAt *int64
	ResultMediaID           string
	TerminalReason          string
	LeaseToken              string
	LeaseUntil              *int64
	NextAttemptAt           int64
	ProcessingFailures      int
	ConversionFailures      int
	CleanupFailures         int
	LastError               string
	CreatedAt               int64
	UpdatedAt               int64
	TerminalAt              *int64
}

// NowMicros is the single clock reading a transaction should take.
func NowMicros() int64 { return time.Now().UTC().UnixMicro() }

// NewLeaseToken returns a fresh random ownership token.
func NewLeaseToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate lease token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

const uploadJobColumns = `upload_id, media_id, status, original_filename, stored_filename, mime_type,
	expected_size, declared_sha256, authoritative_sha256, guest_name, uploader_ip,
	source_completed_at, prepared_at, cancellation_requested_at, result_media_id, terminal_reason,
	lease_token, lease_until, next_attempt_at,
	processing_failures, conversion_failures, cleanup_failures, last_error,
	created_at, updated_at, terminal_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUploadJob(row rowScanner) (*UploadJob, error) {
	var (
		job          UploadJob
		stored       sql.NullString
		mime         sql.NullString
		authoritative sql.NullString
		resultMedia  sql.NullString
		leaseToken   sql.NullString
		sourceDone   sql.NullInt64
		prepared     sql.NullInt64
		cancelled    sql.NullInt64
		leaseUntil   sql.NullInt64
		terminalAt   sql.NullInt64
	)
	err := row.Scan(
		&job.UploadID, &job.MediaID, &job.Status, &job.OriginalFilename, &stored, &mime,
		&job.ExpectedSize, &job.DeclaredSHA256, &authoritative, &job.GuestName, &job.UploaderIP,
		&sourceDone, &prepared, &cancelled, &resultMedia, &job.TerminalReason,
		&leaseToken, &leaseUntil, &job.NextAttemptAt,
		&job.ProcessingFailures, &job.ConversionFailures, &job.CleanupFailures, &job.LastError,
		&job.CreatedAt, &job.UpdatedAt, &terminalAt,
	)
	if err != nil {
		return nil, err
	}
	job.StoredFilename = stored.String
	job.MimeType = mime.String
	job.AuthoritativeSHA256 = authoritative.String
	job.ResultMediaID = resultMedia.String
	job.LeaseToken = leaseToken.String
	job.SourceCompletedAt = nullableMicros(sourceDone)
	job.PreparedAt = nullableMicros(prepared)
	job.CancellationRequestedAt = nullableMicros(cancelled)
	job.LeaseUntil = nullableMicros(leaseUntil)
	job.TerminalAt = nullableMicros(terminalAt)
	return &job, nil
}

func nullableMicros(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}

// CreateUploadingJob records an admitted upload before any byte is written.
func (s *Store) CreateUploadingJob(ctx context.Context, j *UploadJob) error {
	now := NowMicros()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO upload_jobs (
			upload_id, media_id, status, original_filename, expected_size,
			declared_sha256, guest_name, uploader_ip,
			next_attempt_at, created_at, updated_at
		) VALUES (?, ?, 'uploading', ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.UploadID, j.MediaID, j.OriginalFilename, j.ExpectedSize,
		j.DeclaredSHA256, j.GuestName, j.UploaderIP, now, now, now,
	)
	if err != nil {
		return fmt.Errorf("create upload job: %w", err)
	}
	j.Status = JobUploading
	j.CreatedAt, j.UpdatedAt, j.NextAttemptAt = now, now, now
	return nil
}

// GetUploadJob returns nil, nil when no such job exists.
func (s *Store) GetUploadJob(ctx context.Context, uploadID string) (*UploadJob, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+uploadJobColumns+` FROM upload_jobs WHERE upload_id = ?`, uploadID)
	job, err := scanUploadJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get upload job: %w", err)
	}
	return job, nil
}

// PromoteToPending is the durability commit: after it returns nil, the upload
// is the application's responsibility and tus may report success. It fails
// closed if cancellation was requested first.
func (s *Store) PromoteToPending(ctx context.Context, uploadID string, now int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = 'pending',
		       source_completed_at = COALESCE(source_completed_at, ?),
		       next_attempt_at = ?,
		       updated_at = ?
		 WHERE upload_id = ?
		   AND status = 'uploading'
		   AND cancellation_requested_at IS NULL`,
		now, now, now, uploadID,
	)
	if err != nil {
		return fmt.Errorf("promote upload job: %w", err)
	}
	return requireOneRow(res)
}

// ClaimNextJob atomically takes ownership of one due job. Receiving the row is
// the definition of ownership: every later write by this worker must present
// the returned lease token.
func (s *Store) ClaimNextJob(ctx context.Context, from, to JobStatus, now int64, leaseFor time.Duration) (*UploadJob, error) {
	token, err := NewLeaseToken()
	if err != nil {
		return nil, err
	}
	until := now + leaseFor.Microseconds()

	row := s.db.QueryRowContext(ctx, `
		UPDATE upload_jobs
		   SET status = ?, lease_token = ?, lease_until = ?, updated_at = ?
		 WHERE upload_id = (
		       SELECT upload_id FROM upload_jobs
		        WHERE status = ?
		          AND next_attempt_at <= ?
		          AND (lease_until IS NULL OR lease_until <= ?)
		        ORDER BY next_attempt_at
		        LIMIT 1
		 )
		RETURNING `+uploadJobColumns,
		to, token, until, now, from, now, now,
	)
	job, err := scanUploadJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim upload job: %w", err)
	}
	return job, nil
}

// ReleaseForRetry hands a job back for another attempt with capped backoff.
// status is the stage to return to: processing failures go back to pending,
// but a cleanup or discard failure must stay in its own stage. Demoting a
// published job to pending would re-run processing against a source that
// cleanup already deleted, and it would never terminalize.
//
// counter must be one of the three failure column names so budgets and logs
// stay unambiguous.
func (s *Store) ReleaseForRetry(ctx context.Context, uploadID, leaseToken string, status JobStatus, nextAttemptAt int64, counter, lastError string) error {
	switch counter {
	case "processing_failures", "conversion_failures", "cleanup_failures":
	default:
		return fmt.Errorf("unknown failure counter %q", counter)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = ?,
		       lease_token = NULL,
		       lease_until = NULL,
		       next_attempt_at = ?,
		       `+counter+` = `+counter+` + 1,
		       last_error = ?,
		       updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		status, nextAttemptAt, lastError, NowMicros(), uploadID, leaseToken,
	)
	if err != nil {
		return fmt.Errorf("release upload job: %w", err)
	}
	return requireOneRow(res)
}

// RequestCancellation records durable intent. It never reverses a completion:
// callers must check for pending-or-later first and return 409 instead.
func (s *Store) RequestCancellation(ctx context.Context, uploadID string, now int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET cancellation_requested_at = COALESCE(cancellation_requested_at, ?),
		       updated_at = ?
		 WHERE upload_id = ? AND status = 'uploading'`,
		now, now, uploadID,
	)
	if err != nil {
		return fmt.Errorf("request cancellation: %w", err)
	}
	return requireOneRow(res)
}

// FinishJob commits a terminal or intermediate transition under the caller's lease.
func (s *Store) FinishJob(ctx context.Context, uploadID, leaseToken string, status JobStatus, reason string, now int64) error {
	terminal := status == JobComplete || status == JobDiscarded
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = ?,
		       terminal_reason = CASE WHEN ? = '' THEN terminal_reason ELSE ? END,
		       terminal_at = CASE WHEN ? THEN ? ELSE terminal_at END,
		       lease_token = NULL,
		       lease_until = NULL,
		       updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		status, reason, reason, terminal, now, now, uploadID, leaseToken,
	)
	if err != nil {
		return fmt.Errorf("finish upload job: %w", err)
	}
	return requireOneRow(res)
}

// RequeueStartup makes interrupted work claimable immediately after a restart
// rather than waiting out wall-clock leases.
func (s *Store) RequeueStartup(ctx context.Context, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = CASE WHEN status = 'processing' THEN 'pending' ELSE status END,
		       lease_token = NULL,
		       lease_until = NULL,
		       next_attempt_at = ?,
		       updated_at = ?
		 WHERE status IN ('processing', 'cleanup', 'discarding')`,
		now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("requeue on startup: %w", err)
	}
	return res.RowsAffected()
}

// DeleteTerminalJobsBefore expires status rows in bounded batches.
func (s *Store) DeleteTerminalJobsBefore(ctx context.Context, cutoff int64, limit int) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM upload_jobs
		 WHERE upload_id IN (
		       SELECT upload_id FROM upload_jobs
		        WHERE status IN ('complete', 'discarded')
		          AND terminal_at IS NOT NULL
		          AND terminal_at < ?
		        LIMIT ?
		 )`, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("expire terminal jobs: %w", err)
	}
	return res.RowsAffected()
}

func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotClaimed
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 go test ./internal/store/ -race -v`
Expected: PASS, including the pre-existing store tests.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/store/upload_jobs.go backend/internal/store/upload_jobs_test.go
git commit -m "feat: add leased upload job store"
```

---

### Task 4: Shared tus sidecar parser

**Files:**
- Create: `backend/internal/tussidecar/sidecar.go`
- Create: `backend/internal/tussidecar/sidecar_test.go`
- Modify: `backend/internal/httpapi/storage_cleanup.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Info struct { ID string; Size int64; SizeIsDeferred bool; MetaData map[string]string; StoragePath string; StorageInfoPath string }` and `func Parse(infoPath string) (*Info, error)`, plus `var ErrMalformed error`. `SizeIsDeferred` and `StorageInfoPath` exist because the janitor already validates both and must not lose that on the delete path.

- [ ] **Step 1: Write the failing test**

Create `backend/internal/tussidecar/sidecar_test.go`:

```go
package tussidecar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInfo(t *testing.T, dir, id, body string) string {
	t.Helper()
	path := filepath.Join(dir, id+".info")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestParseValidSidecar(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "abc")
	if err := os.WriteFile(dataPath, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	path := writeInfo(t, dir, "abc", `{"ID":"abc","Size":5,"Offset":5,"MetaData":{"filename":"a.jpg"},"Storage":{"Type":"filestore","Path":"`+strings.ReplaceAll(dataPath, `\`, `\\`)+`"}}`)

	info, err := Parse(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.ID != "abc" || info.Size != 5 || info.MetaData["filename"] != "a.jpg" {
		t.Errorf("unexpected info: %+v", info)
	}
}

func TestParseRejectsOversizedSidecar(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "big", `{"ID":"big",`+strings.Repeat(" ", maxSidecarBytes)+`"Size":1}`)
	if _, err := Parse(path); err == nil {
		t.Fatal("expected oversized sidecar to be rejected")
	}
}

func TestParseRejectsWrongStorageType(t *testing.T) {
	dir := t.TempDir()
	path := writeInfo(t, dir, "s3", `{"ID":"s3","Size":1,"Storage":{"Type":"s3store","Path":"/x"}}`)
	if _, err := Parse(path); err == nil {
		t.Fatal("expected non-filestore sidecar to be rejected")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/tussidecar/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement the parser**

Create `backend/internal/tussidecar/sidecar.go`:

```go
// Package tussidecar parses tusd's `.info` sidecar files. Both the ingest
// reconciler and the incomplete-upload janitor read them, and both must apply
// the same bounds and validation, so the logic lives here once.
package tussidecar

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// maxSidecarBytes bounds how much untrusted JSON we will read from the shared
// upload volume.
const maxSidecarBytes = 64 * 1024

// ErrMalformed marks a sidecar that exists but cannot be trusted. Callers must
// treat it as "unknown", never as "safe to delete".
var ErrMalformed = errors.New("malformed tus sidecar")

type Info struct {
	ID              string
	Size            int64
	SizeIsDeferred  bool
	MetaData        map[string]string
	StoragePath     string
	StorageInfoPath string
}

type rawInfo struct {
	ID             string            `json:"ID"`
	Size           int64             `json:"Size"`
	SizeIsDeferred bool              `json:"SizeIsDeferred"`
	MetaData       map[string]string `json:"MetaData"`
	Storage        struct {
		Type     string `json:"Type"`
		Path     string `json:"Path"`
		InfoPath string `json:"InfoPath"`
	} `json:"Storage"`
}

// Parse reads and validates one sidecar. It never returns a partially trusted
// value: either every checked field is sane, or an error.
func Parse(infoPath string) (*Info, error) {
	f, err := os.Open(infoPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxSidecarBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read sidecar: %w", err)
	}
	if len(data) > maxSidecarBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrMalformed, maxSidecarBytes)
	}

	var raw rawInfo
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if raw.ID == "" {
		return nil, fmt.Errorf("%w: empty upload id", ErrMalformed)
	}
	if raw.Size <= 0 {
		return nil, fmt.Errorf("%w: non-positive size", ErrMalformed)
	}
	if raw.SizeIsDeferred {
		return nil, fmt.Errorf("%w: deferred size", ErrMalformed)
	}
	if raw.Storage.Type != "filestore" {
		return nil, fmt.Errorf("%w: unsupported storage type %q", ErrMalformed, raw.Storage.Type)
	}
	if raw.Storage.Path == "" {
		return nil, fmt.Errorf("%w: empty storage path", ErrMalformed)
	}

	return &Info{
		ID:              raw.ID,
		Size:            raw.Size,
		SizeIsDeferred:  raw.SizeIsDeferred,
		MetaData:        raw.MetaData,
		StoragePath:     raw.Storage.Path,
		StorageInfoPath: raw.Storage.InfoPath,
	}, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/tussidecar/ -v`
Expected: PASS.

- [ ] **Step 5: Switch the janitor to the shared parser**

In `backend/internal/httpapi/storage_cleanup.go`, replace the inline sidecar decoding inside `inspectTusCandidate` with a `tussidecar.Parse` call. The existing guard is one long condition at line 273:

```go
if info.ID != id || info.Size <= 0 || info.SizeIsDeferred || info.Storage.Type != "filestore" || filepath.Clean(info.Storage.Path) != filepath.Clean(dataPath) || filepath.Clean(info.Storage.InfoPath) != filepath.Clean(infoPath) {
```

`Parse` now covers the size, deferred-size, and storage-type checks. Keep the three identity comparisons the parser cannot make, because it does not know which paths the caller expected:

```go
if info.ID != id ||
	filepath.Clean(info.StoragePath) != filepath.Clean(dataPath) ||
	filepath.Clean(info.StorageInfoPath) != filepath.Clean(infoPath) {
```

Do not change the janitor's retention policy, and keep its existing behavior that a complete data file is left alone for ingest recovery. Its deletion call is changed in Task 8, once the guarded terminator exists.

- [ ] **Step 6: Run the affected tests**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/httpapi/ -run Cleanup -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add backend/internal/tussidecar/ backend/internal/httpapi/storage_cleanup.go
git commit -m "refactor: share bounded tus sidecar parsing"
```

---

### Task 5: Durable file primitives

**Files:**
- Create: `backend/internal/media/durable.go`
- Create: `backend/internal/media/durable_test.go`

**Interfaces:**
- Consumes: `Processor.OriginalsDir()`, `Processor.OriginalPath(stored)`, `SHA256File(path)` — all existing.
- Produces:
  - `func FsyncFile(path string) error`
  - `func FsyncDir(path string) error`
  - `type PrepareRequest struct { SourcePath, MediaID, LeaseToken, StoredFilename string; Size int64; SHA256 string }`
  - `func (p *Processor) PrepareOriginal(ctx context.Context, req PrepareRequest) error` — verified reuse, else copy, fsync, rename, fsync dir. Never removes the source.
  - `func (p *Processor) VerifyOriginal(storedFilename string, wantSize int64, wantSHA256 string) error`

- [ ] **Step 1: Write the failing test**

Create `backend/internal/media/durable_test.go`:

```go
package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newTestProcessor(t *testing.T) *Processor {
	t.Helper()
	p := NewProcessor(t.TempDir(), 320, []string{"image/jpeg"}, nil)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	return p
}

func TestPrepareOriginalKeepsSource(t *testing.T) {
	p := newTestProcessor(t)
	source := filepath.Join(t.TempDir(), "incoming")
	payload := []byte("some bytes")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sum, err := SHA256File(source)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	req := PrepareRequest{
		SourcePath:     source,
		MediaID:        "media-1",
		LeaseToken:     "lease-a",
		StoredFilename: "media-1.jpg",
		Size:           int64(len(payload)),
		SHA256:         sum,
	}

	if err := p.PrepareOriginal(context.Background(), req); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// The whole point: preparation must never consume the only complete copy.
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source must survive preparation: %v", err)
	}
	got, err := os.ReadFile(p.OriginalPath("media-1.jpg"))
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("original = %q, want %q", got, payload)
	}
	// No temporary may be left behind.
	entries, _ := os.ReadDir(p.OriginalsDir())
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temporary %s", e.Name())
		}
	}

	// Re-running under a different lease must reuse the verified original
	// rather than copying again.
	req.LeaseToken = "lease-b"
	if err := p.PrepareOriginal(context.Background(), req); err != nil {
		t.Fatalf("second prepare: %v", err)
	}
}

func TestPrepareOriginalRefusesToOverwriteDifferentContent(t *testing.T) {
	p := newTestProcessor(t)
	source := filepath.Join(t.TempDir(), "incoming")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(p.OriginalPath("media-2.jpg"), []byte("pre-existing"), 0o600); err != nil {
		t.Fatalf("write squatter: %v", err)
	}

	err := p.PrepareOriginal(context.Background(), PrepareRequest{
		SourcePath:     source,
		MediaID:        "media-2",
		LeaseToken:     "lease-a",
		StoredFilename: "media-2.jpg",
		Size:           3,
		SHA256:         "deadbeef",
	})
	if err == nil {
		t.Fatal("an existing original with different content must be an error, never silently overwritten")
	}
	got, _ := os.ReadFile(p.OriginalPath("media-2.jpg"))
	if string(got) != "pre-existing" {
		t.Errorf("existing original was modified: %q", got)
	}
}

func TestVerifyOriginalDetectsMismatch(t *testing.T) {
	p := newTestProcessor(t)
	path := p.OriginalPath("media-3.jpg")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sum, err := SHA256File(path)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := p.VerifyOriginal("media-3.jpg", 3, sum); err != nil {
		t.Fatalf("verify good: %v", err)
	}
	if err := p.VerifyOriginal("media-3.jpg", 3, "deadbeef"); err == nil {
		t.Error("expected hash mismatch to be reported")
	}
	if err := p.VerifyOriginal("media-3.jpg", 99, sum); err == nil {
		t.Error("expected size mismatch to be reported")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/media/ -run 'PrepareOriginal|VerifyOriginal' -v`
Expected: FAIL — `p.PrepareOriginal undefined`.

- [ ] **Step 3: Implement the primitives**

Create `backend/internal/media/durable.go`:

```go
package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FsyncFile flushes one file's contents to stable storage.
func FsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", path, err)
	}
	return nil
}

// FsyncDir flushes a directory entry so a rename or unlink survives a crash.
func FsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync dir %s: %w", path, err)
	}
	return nil
}

// PrepareRequest describes one copy into permanent storage. Size and SHA256
// are the already-computed identity of the source, which makes preparation
// idempotent across retries.
type PrepareRequest struct {
	SourcePath     string
	MediaID        string
	LeaseToken     string
	StoredFilename string
	Size           int64
	SHA256         string
}

// PrepareOriginal copies the source into permanent storage. It never removes
// the source: until the media row is committed, that source may be the only
// complete copy in existence.
//
// If a deterministic original is already present it is reused when its size
// and hash match, and is an error otherwise — a mismatch is never silently
// overwritten.
//
// Ordering is load-bearing — write, fsync file, fsync dir, rename, fsync dir —
// so that a crash can leave a stale temporary but never a truncated original.
func (p *Processor) PrepareOriginal(ctx context.Context, req PrepareRequest) error {
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	finalPath := p.OriginalPath(req.StoredFilename)

	if _, err := os.Stat(finalPath); err == nil {
		if err := p.VerifyOriginal(req.StoredFilename, req.Size, req.SHA256); err != nil {
			return fmt.Errorf("existing original %s does not match this upload: %w", req.StoredFilename, err)
		}
		// Re-sync the directory: the previous attempt may have failed on
		// exactly this step, and skipping it would publish a rename that a
		// crash could still roll back.
		return FsyncDir(p.OriginalsDir())
	}

	src, err := os.Open(req.SourcePath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()

	// The lease token scopes the temporary, so a worker whose lease expired
	// cannot write into its successor's temporary.
	tmpPath := filepath.Join(p.OriginalsDir(), ".ingest-"+req.MediaID+"-"+req.LeaseToken+"-original.tmp")
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("create temporary: %w", err)
	}

	if err := copyWithContext(ctx, tmp, src); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("fsync temporary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temporary: %w", err)
	}
	// Only now is the temporary a recoverable alternate copy.
	if err := FsyncDir(p.OriginalsDir()); err != nil {
		return err
	}
	// A worker whose attempt was cancelled must not publish an artifact after
	// its successor may already have cleaned up.
	if err := ctx.Err(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("rename original into place: %w", err)
	}
	return FsyncDir(p.OriginalsDir())
}

// SHA256FileContext hashes a file, honoring cancellation. The whole-file hash
// of a multi-gigabyte upload is long enough that a shutdown must be able to
// interrupt it.
func SHA256FileContext(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sum := sha256.New()
	if err := copyWithContext(ctx, sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// VerifyOriginal proves a stored original is the exact content we expect.
// Absence, wrong size, and wrong hash are all errors: a missing file is never
// treated as permission to move on.
func (p *Processor) VerifyOriginal(storedFilename string, wantSize int64, wantSHA256 string) error {
	path := p.OriginalPath(storedFilename)
	stat, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat original: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return errors.New("original is not a regular file")
	}
	if stat.Size() != wantSize {
		return fmt.Errorf("original size %d, want %d", stat.Size(), wantSize)
	}
	sum, err := SHA256File(path)
	if err != nil {
		return fmt.Errorf("hash original: %w", err)
	}
	if sum != wantSHA256 {
		return fmt.Errorf("original sha256 %s, want %s", sum, wantSHA256)
	}
	return nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write copy: %w", writeErr)
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read source: %w", readErr)
		}
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 go test ./internal/media/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/media/durable.go backend/internal/media/durable_test.go
git commit -m "feat: add fsync-ordered original preparation that preserves the source"
```

---

### Task 6: Storage health gate

**Files:**
- Create: `backend/internal/ingest/health.go`
- Create: `backend/internal/ingest/health_test.go`

**Interfaces:**
- Consumes: `store.Store`, `media.Processor`.
- Produces: `type HealthGate struct`, `func NewHealthGate(st *store.Store, proc *media.Processor, sampleSize int) *HealthGate`, `func (g *HealthGate) Check(ctx context.Context) error`, `func (g *HealthGate) Healthy() bool`. A `nil` error means deletions are permitted.

- [ ] **Step 1: Write the failing test**

Create `backend/internal/ingest/health_test.go`:

```go
package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthGateEmptyDatabaseIsHealthy(t *testing.T) {
	st, proc := newIngestFixture(t)
	gate := NewHealthGate(st, proc, 5)
	if err := gate.Check(context.Background()); err != nil {
		t.Fatalf("empty database must be trivially healthy: %v", err)
	}
}

func TestHealthGateOpensCircuitWhenAllSampledOriginalsMissing(t *testing.T) {
	st, proc := newIngestFixture(t)
	insertMediaRow(t, st, "m1", "m1.jpg", "hash-1")

	gate := NewHealthGate(st, proc, 5)
	if err := gate.Check(context.Background()); err == nil {
		t.Fatal("a non-empty database with no originals on disk must open the circuit")
	}
	if gate.Healthy() {
		t.Error("Healthy() must report false while the circuit is open")
	}

	// The volume returns: the circuit must close by itself.
	if err := os.WriteFile(filepath.Join(proc.OriginalsDir(), "m1.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	if err := gate.Check(context.Background()); err != nil {
		t.Fatalf("circuit must close once the volume returns: %v", err)
	}
	if !gate.Healthy() {
		t.Error("Healthy() must report true after recovery")
	}
}

func TestHealthGateOneSurvivingOriginalIsEnough(t *testing.T) {
	st, proc := newIngestFixture(t)
	insertMediaRow(t, st, "m1", "m1.jpg", "hash-1")
	insertMediaRow(t, st, "m2", "m2.jpg", "hash-2")
	if err := os.WriteFile(filepath.Join(proc.OriginalsDir(), "m2.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}

	gate := NewHealthGate(st, proc, 5)
	if err := gate.Check(context.Background()); err != nil {
		t.Fatalf("a mounted volume with one deleted file is healthy, not faulted: %v", err)
	}
}
```

Create the shared fixture `backend/internal/ingest/fixture_test.go`:

```go
package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"event-gallery/backend/internal/db"
	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/models"
	"event-gallery/backend/internal/store"
)

func newIngestFixture(t *testing.T) (*store.Store, *media.Processor) {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	proc := media.NewProcessor(t.TempDir(), 320, []string{"image/jpeg"}, []string{"video/mp4"})
	if err := proc.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	return store.New(sqlDB), proc
}

func insertMediaRow(t *testing.T, st *store.Store, id, storedFilename, sha string) {
	t.Helper()
	err := st.InsertMedia(context.Background(), &models.MediaItem{
		ID:               id,
		OriginalFilename: "photo.jpg",
		StoredFilename:   storedFilename,
		Kind:             models.KindImage,
		MimeType:         "image/jpeg",
		SizeBytes:        1,
		SHA256:           sha,
		UploadedAt:       time.Now(),
	})
	if err != nil {
		t.Fatalf("insert media: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/ingest/ -v`
Expected: FAIL — package `ingest` does not exist.

- [ ] **Step 3: Add the store query the gate needs**

Append to `backend/internal/store/upload_jobs.go`:

```go
// SampleStoredFilenames returns up to limit stored filenames, used to prove a
// media volume is actually mounted before anything is deleted.
func (s *Store) SampleStoredFilenames(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stored_filename FROM media_items LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sample stored filenames: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan stored filename: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
```

- [ ] **Step 4: Implement the gate**

Create `backend/internal/ingest/health.go`:

```go
// Package ingest owns the durable upload queue: admission, the pre-finish
// durability barrier, leased workers, publication, and recovery. Everything in
// here runs on the manager's lifetime context, never on a request context.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/store"
)

// ErrStorageUnhealthy means we cannot prove the media volume is mounted. It
// must block every deletion: a failed bind mount presents an empty directory,
// and treating that as "the files are gone" would delete the database rows
// describing files that are merely temporarily invisible.
var ErrStorageUnhealthy = errors.New("media storage is not verifiably mounted")

type HealthGate struct {
	store      *store.Store
	processor  *media.Processor
	sampleSize int
	healthy    atomic.Bool
}

func NewHealthGate(st *store.Store, proc *media.Processor, sampleSize int) *HealthGate {
	if sampleSize <= 0 {
		sampleSize = 8
	}
	g := &HealthGate{store: st, processor: proc, sampleSize: sampleSize}
	// Starts closed. Nothing may be deleted until a check has actually proven
	// the media volume is mounted.
	g.healthy.Store(false)
	return g
}

// Healthy reports the last observed state without touching the filesystem.
func (g *HealthGate) Healthy() bool { return g.healthy.Load() }

// Check re-evaluates the circuit. It demands positive evidence: at least one
// expected original must be present. An empty gallery is trivially healthy
// because there is nothing that could be missing.
func (g *HealthGate) Check(ctx context.Context) error {
	names, err := g.store.SampleStoredFilenames(ctx, g.sampleSize)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		g.setHealthy(true)
		return nil
	}

	for _, name := range names {
		if _, err := os.Stat(g.processor.OriginalPath(name)); err == nil {
			g.setHealthy(true)
			return nil
		}
	}

	g.setHealthy(false)
	return fmt.Errorf("%w: none of %d sampled originals exist under %s",
		ErrStorageUnhealthy, len(names), g.processor.OriginalsDir())
}

func (g *HealthGate) setHealthy(now bool) {
	if was := g.healthy.Swap(now); was != now {
		if now {
			slog.Warn("storage health circuit closed", "operation", "storage_health", "healthy", true)
		} else {
			slog.Error("storage health circuit opened; refusing uploads and all deletions",
				"operation", "storage_health", "healthy", false,
				"remediation", "verify the media bind mount is attached at "+g.processor.MediaDir)
		}
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 go test ./internal/ingest/ -race -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/ingest/ backend/internal/store/upload_jobs.go
git commit -m "feat: add positive-evidence storage health gate"
```

---

### Task 7: Manager lifecycle and worker pool

**Files:**
- Create: `backend/internal/ingest/manager.go`
- Create: `backend/internal/ingest/manager_test.go`

**Interfaces:**
- Consumes: `store.Store`, `media.Processor`, `HealthGate`, `config.Config` values.
- Produces:
  - `type Options struct { Workers int; DurabilityWorkers int; ProcessingTimeout, MaxBackoff, ReconcileInterval, JobRetention time.Duration; UploadDir string; MinFreeBytes int64; Terminator SourceTerminator }` — the pre-finish response budget is not here: it belongs to the HTTP layer and is read from `cfg.UploadDurabilityWait`.
  - `type SourceTerminator interface { Terminate(ctx context.Context, uploadID string) error }`
  - `type Manager struct`
  - `func New(st *store.Store, proc *media.Processor, opts Options) *Manager` — creates the lifetime context.
  - `func (m *Manager) Start()` — non-blocking; takes no context by construction, so no caller can bind ingest to a request.
  - `func (m *Manager) Stop()` — cancels and waits for workers and in-flight durability operations.
  - `func (m *Manager) Wake()` — non-blocking nudge.
  - `func (m *Manager) Ready() bool`
  - `func (m *Manager) backoffFor(failures int) time.Duration`
  - `func (m *Manager) leaseDuration() time.Duration`

- [ ] **Step 1: Write the failing test**

Create `backend/internal/ingest/manager_test.go`:

```go
package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	uploadDir := t.TempDir()
	return Options{
		Workers:           2,
		DurabilityWorkers: 2,
		ProcessingTimeout: time.Minute,
		MaxBackoff:        time.Minute,
		ReconcileInterval: 50 * time.Millisecond,
		JobRetention:      time.Hour,
		UploadDir:         uploadDir,
		MinFreeBytes:      0,
		Terminator:        &unlinkTerminator{dir: uploadDir},
	}
}

// unlinkTerminator stands in for tusd in unit tests: it removes exactly the
// two files tusd's filestore would remove.
type unlinkTerminator struct{ dir string }

func (u *unlinkTerminator) Terminate(_ context.Context, uploadID string) error {
	for _, p := range []string{filepath.Join(u.dir, uploadID), filepath.Join(u.dir, uploadID+".info")} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func TestBackoffIsExponentialAndCapped(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	first := m.backoffFor(0)
	second := m.backoffFor(1)
	if second <= first {
		t.Errorf("backoff must grow: %v then %v", first, second)
	}
	if capped := m.backoffFor(40); capped != m.opts.MaxBackoff {
		t.Errorf("backoff must cap at %v, got %v", m.opts.MaxBackoff, capped)
	}
}

func TestStartStopIsClean(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	m.Start()
	m.Wake()
	m.Wake() // must not block even when nothing is draining the channel
	m.Stop()
}

func TestLeaseOutlivesTheAttemptTimeout(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	// If the lease expired exactly when the attempt timed out, a second worker
	// could claim a job whose first worker is still shutting down.
	if m.leaseDuration() <= m.opts.ProcessingTimeout {
		t.Errorf("leaseDuration = %v, must exceed ProcessingTimeout %v", m.leaseDuration(), m.opts.ProcessingTimeout)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/ingest/ -run 'Backoff|StartStop|Lifetime' -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement the manager**

Create `backend/internal/ingest/manager.go`:

```go
package ingest

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/store"
)

// SourceTerminator removes one upload from tusd's own storage. The app never
// unlinks tusd's files itself, so tusd's sidecars and locks stay consistent,
// and every deletion in the system goes through one implementation.
type SourceTerminator interface {
	Terminate(ctx context.Context, uploadID string) error
}

type Options struct {
	Workers           int
	DurabilityWorkers int
	ProcessingTimeout time.Duration
	MaxBackoff        time.Duration
	ReconcileInterval time.Duration
	JobRetention      time.Duration
	UploadDir         string
	MinFreeBytes      int64
	Terminator        SourceTerminator
}

// Manager owns the durable ingest queue. Its workers run on lifetime, a
// context created here and cancelled only by Stop. It deliberately accepts no
// context from callers: the original data loss happened because ingest ran on
// a request-derived context that tusd cancelled ten seconds after the
// upload's final PATCH, and a signature that cannot express that mistake is
// worth more than a comment warning against it.
type Manager struct {
	store     *store.Store
	processor *media.Processor
	health    *HealthGate
	opts      Options

	lifetime context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	wg       sync.WaitGroup
	ready    atomic.Bool

	durability *durabilityRegistry
}

func New(st *store.Store, proc *media.Processor, opts Options) *Manager {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.DurabilityWorkers <= 0 {
		opts.DurabilityWorkers = 2
	}
	if opts.ProcessingTimeout <= 0 {
		opts.ProcessingTimeout = time.Hour
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 15 * time.Minute
	}
	if opts.ReconcileInterval <= 0 {
		opts.ReconcileInterval = 15 * time.Second
	}
	m := &Manager{
		store:     st,
		processor: proc,
		health:    NewHealthGate(st, proc, 8),
		opts:      opts,
		wake:      make(chan struct{}, 1),
	}
	m.lifetime, m.cancel = context.WithCancel(context.Background())
	m.durability = newDurabilityRegistry(m)
	return m
}

// Health exposes the gate so HTTP handlers can refuse uploads while the
// circuit is open.
func (m *Manager) Health() *HealthGate { return m.health }

// Ready reports whether the startup inventory has finished. Upload routes must
// return a retryable 503 until it has.
func (m *Manager) Ready() bool { return m.ready.Load() }

// leaseDuration must exceed the attempt timeout, so a worker that runs its
// full budget still owns the job while it unwinds.
func (m *Manager) leaseDuration() time.Duration {
	return m.opts.ProcessingTimeout + m.opts.ReconcileInterval
}

// Start runs the startup inventory and then launches the pool. It is
// deliberately synchronous: readiness is true the moment it returns, which is
// what the rollout requires — every valid pre-upgrade sidecar is adopted
// before the app reports ready. Callers must already be serving HTTP, since
// the inventory fsyncs every recovered source (see main.go, which runs this in
// a goroutine after the listener starts).
func (m *Manager) Start() {
	// Held for the whole of Start so Stop cannot observe an empty WaitGroup
	// while recovery is still using the database.
	m.wg.Add(1)
	defer m.wg.Done()

	m.startupRecovery()
	if m.lifetime.Err() != nil {
		return // Stop() arrived during the inventory; do not launch workers
	}

	for i := 0; i < m.opts.Workers; i++ {
		m.wg.Add(1)
		go func(worker int) {
			defer m.wg.Done()
			m.runWorker(worker)
		}(i)
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.runReconciler()
	}()
}

// Stop cancels the lifetime context and waits for in-flight work to yield,
// including detached durability operations. Without that wait, a promotion
// could still be writing when main closes the database.
func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
	m.durability.wait()
}

// Wake nudges a worker without blocking. A full channel already means "there
// is work to look at", so dropping the signal is correct.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) runWorker(worker int) {
	ticker := time.NewTicker(m.opts.ReconcileInterval)
	defer ticker.Stop()

	for {
		worked, err := m.claimAndRunOnce()
		if err != nil {
			slog.Error("ingest worker iteration failed", "operation", "worker_loop", "worker", worker, "error", err)
		}
		if worked {
			continue // drain the queue before sleeping
		}
		select {
		case <-m.lifetime.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
	}
}

func (m *Manager) runReconciler() {
	ticker := time.NewTicker(m.opts.ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.lifetime.Done():
			return
		case <-ticker.C:
			if err := m.health.Check(m.lifetime); err != nil {
				slog.Warn("storage health check failed", "operation", "reconcile", "error", err)
			}
			if m.Ready() {
				if err := m.reconcileOnce(); err != nil {
					slog.Warn("reconcile pass failed", "operation", "reconcile", "error", err)
				}
			} else {
				// Startup did not complete. Retry the whole prerequisite,
				// including the lease reset: opening readiness without it would
				// leave interrupted jobs holding pre-crash leases for a full
				// lease duration.
				m.startupRecovery()
			}
			m.expireTerminalJobs()
		}
	}
}

// backoffFor grows exponentially and then flattens. Retries are indefinite:
// no failure count may ever escalate into deleting a source.
func (m *Manager) backoffFor(failures int) time.Duration {
	if failures < 0 {
		failures = 0
	}
	if failures > 30 {
		return m.opts.MaxBackoff
	}
	delay := time.Duration(math.Pow(2, float64(failures))) * time.Second
	if delay <= 0 || delay > m.opts.MaxBackoff {
		return m.opts.MaxBackoff
	}
	return delay
}

func (m *Manager) expireTerminalJobs() {
	cutoff := store.NowMicros() - m.opts.JobRetention.Microseconds()
	if _, err := m.store.DeleteTerminalJobsBefore(m.lifetime, cutoff, 200); err != nil {
		slog.Warn("failed to expire terminal upload jobs", "operation", "expire_jobs", "error", err)
	}
}
```

Add temporary no-op stubs so the package compiles; Tasks 9, 11 and 12 replace them.

```go
func (m *Manager) claimAndRunOnce() (bool, error) { return false, nil }
func (m *Manager) reconcileOnce() error           { return nil }

// The health gate starts closed, so something must prove the media volume is
// mounted before uploads are admitted. Task 12's real implementation does the
// same check as its first step.
func (m *Manager) startupRecovery() {
	_ = m.health.Check(m.lifetime)
	m.ready.Store(true)
}
```

Also add the registry placeholder in `durability.go` (fully implemented in Task 9):

```go
package ingest

import "sync"

type durabilityRegistry struct {
	manager *Manager
	wg      sync.WaitGroup
}

func newDurabilityRegistry(m *Manager) *durabilityRegistry { return &durabilityRegistry{manager: m} }

func (r *durabilityRegistry) wait() { r.wg.Wait() }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 go test ./internal/ingest/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/ingest/
git commit -m "feat: add ingest manager lifecycle and worker pool"
```

---

### Task 8: Pre-create admission

**Files:**
- Modify: `backend/internal/httpapi/tus_hooks.go`
- Modify: `backend/internal/httpapi/tus_proxy.go`
- Modify: `backend/internal/httpapi/storage_cleanup.go`
- Modify: `backend/internal/httpapi/server.go`
- Modify: `backend/internal/httpapi/testharness_test.go`
- Modify: `backend/internal/ingest/manager.go`
- Modify: `backend/internal/store/upload_jobs.go`
- Create: `backend/internal/ingest/freespace_unix.go`, `backend/internal/ingest/freespace_other.go`
- Test: `backend/internal/httpapi/tus_hooks_test.go`

**Interfaces:**
- Consumes: `store.CreateUploadingJob`, `ingest.Manager.Ready()`, `ingest.HealthGate.Healthy()`.
- Produces: `pre-create` returns `ChangeFileInfo.ID`; adds `retryHook(w, retryAfterSeconds, message)`; adds `Server.ingest *ingest.Manager` and `func (s *Server) SetIngest(m *ingest.Manager)`.

- [ ] **Step 1: Give the test harness an ingest manager**

Every hook path now depends on the manager, so the shared harness must build one or all of this task's tests fail on a nil dependency. In `backend/internal/httpapi/testharness_test.go`, add the new config fields and start a manager before returning:

```go
		AllowedVideoMIMEs:               []string{"video/mp4"},
		MediaProcessingWorkers:          1,
		MediaProcessingTimeout:          time.Minute,
		UploadDurabilityWait:            5 * time.Second,
		UploadDurabilityWorkers:         1,
		UploadRetryMaxBackoff:           time.Minute,
		IngestReconcileInterval:         time.Hour, // tests drive reconciliation explicitly
		UploadJobRetention:              time.Hour,
		UploadStatusRateLimitPerMinute:  6000,
	}

	srv, err := NewServer(cfg, st, proc, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	manager := ingest.New(st, proc, ingest.Options{
		Workers:           cfg.MediaProcessingWorkers,
		DurabilityWorkers: cfg.UploadDurabilityWorkers,
		ProcessingTimeout: cfg.MediaProcessingTimeout,
		MaxBackoff:        cfg.UploadRetryMaxBackoff,
		ReconcileInterval: cfg.IngestReconcileInterval,
		JobRetention:      cfg.UploadJobRetention,
		UploadDir:         tusDir,
		Terminator:        srv,
	})
	manager.Start()
	t.Cleanup(manager.Stop)
	srv.SetIngest(manager)

	return &testHarness{server: srv, router: srv.Router(), store: st, cfg: cfg, proc: proc, ingest: manager}
```

Add `ingest *ingest.Manager` to `testHarness`, and import `event-gallery/backend/internal/ingest`.

`manager.Start()` runs the startup inventory, so `Ready()` becomes true and `pre-create` admits uploads instead of returning backpressure.

- [ ] **Step 2: Retire the tests this task invalidates**

Deleting `handlePostFinishHook` removes the behavior four tests assert. Delete them from `backend/internal/httpapi/tus_hooks_test.go`; Task 11's ingest tests replace their coverage at the layer that now owns it:

- `TestTusHook_PostFinish_ProcessesAndInsertsMedia`
- `TestTusHook_PostFinish_RejectsChecksumMismatch`
- `TestTusHook_PostFinish_RejectsUnsupportedType`
- `TestTusHook_PostFinish_DuplicateIgnored`

Keep `TestTusHook_PreCreate_RejectsOversized` and `TestTusHook_PreCreate_RejectsMissingFilename` — both still pass. Update `TestTusHook_PreCreate_AllowsValid` to also assert the generated id:

```go
	if resp.ChangeFileInfo == nil || resp.ChangeFileInfo.ID == "" {
		t.Fatal("pre-create must generate the upload id")
	}
```

- [ ] **Step 3: Write the failing test**

Append to `backend/internal/httpapi/tus_hooks_test.go`, following the file's existing `hookRequestBody` / `newRequestWithHeader` / `serveRequest` pattern:

```go
func postHook(t *testing.T, h *testHarness, req tusHookRequest) tusHookResponse {
	t.Helper()
	httpReq := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks",
		hookRequestBody(t, req), internalProxySecretHeader, h.cfg.TusHookSecret)
	rec := serveRequest(h, httpReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("hook endpoint must always answer 200, got %d", rec.Code)
	}
	var resp tusHookResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode hook response: %v", err)
	}
	return resp
}

func TestPreCreateGeneratesIDAndJob(t *testing.T) {
	h := newTestHarness(t)

	resp := postHook(t, h, tusHookRequest{
		Type: "pre-create",
		Event: tusHookEvent{Upload: tusHookUpload{
			Size:     10,
			MetaData: map[string]string{"filename": "a.jpg", "sha256": "abc"},
		}},
	})

	if resp.RejectUpload {
		t.Fatal("valid upload must be admitted")
	}
	if resp.ChangeFileInfo == nil || resp.ChangeFileInfo.ID == "" {
		t.Fatal("pre-create must generate the upload id")
	}

	job, err := h.store.GetUploadJob(context.Background(), resp.ChangeFileInfo.ID)
	if err != nil || job == nil {
		t.Fatalf("uploading row must exist: %+v %v", job, err)
	}
	if job.Status != store.JobUploading || job.ExpectedSize != 10 || job.DeclaredSHA256 != "abc" {
		t.Errorf("unexpected job %+v", job)
	}
}

func TestPreCreateRejectsDeferredSize(t *testing.T) {
	h := newTestHarness(t)

	resp := postHook(t, h, tusHookRequest{
		Type:  "pre-create",
		Event: tusHookEvent{Upload: tusHookUpload{Size: 0, MetaData: map[string]string{"filename": "a.jpg"}}},
	})

	if !resp.RejectUpload || resp.HTTPResponse == nil || resp.HTTPResponse.StatusCode != http.StatusBadRequest {
		t.Errorf("deferred size must be a deterministic 400, got %+v", resp)
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/httpapi/ -run PreCreate -v`
Expected: FAIL — no `ChangeFileInfo` in the response.

- [ ] **Step 5: Extend the hook response types**

In `tus_hooks.go`, add to the response types and add a backpressure helper:

```go
type tusHookChangeFileInfo struct {
	ID string `json:"ID,omitempty"`
}

type tusHookResponse struct {
	HTTPResponse   *tusHookHTTPResponse   `json:"HTTPResponse,omitempty"`
	RejectUpload   bool                   `json:"RejectUpload,omitempty"`
	ChangeFileInfo *tusHookChangeFileInfo `json:"ChangeFileInfo,omitempty"`
}

// retryHook relays backpressure at admission time. tusd honors RejectUpload
// only for pre-create, and only when the hook itself answers 2xx and embeds
// the real response — a non-2xx here would become an opaque 500 at the browser
// instead of a retryable 503.
func retryHook(w http.ResponseWriter, retryAfterSeconds int, message string) {
	resp := tusHookResponse{
		RejectUpload: true,
		HTTPResponse: &tusHookHTTPResponse{
			StatusCode: http.StatusServiceUnavailable,
			Body:       `{"error":"` + message + `"}`,
			Header: map[string]string{
				"Content-Type": "application/json",
				"Retry-After":  strconv.Itoa(retryAfterSeconds),
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
```

Add `"strconv"` to the imports.

- [ ] **Step 6: Rewrite the pre-create handler**

Replace `handlePreCreateHook` with:

```go
func (s *Server) handlePreCreateHook(w http.ResponseWriter, r *http.Request, req tusHookRequest) {
	upload := req.Event.Upload

	// Deterministic client errors keep their existing 4xx semantics.
	if upload.Size <= 0 {
		rejectHook(w, http.StatusBadRequest, "upload size must be known and positive")
		return
	}
	if upload.Size > s.cfg.MaxUploadBytes {
		rejectHook(w, http.StatusRequestEntityTooLarge, "file exceeds the maximum allowed size")
		return
	}
	filename := strings.TrimSpace(upload.MetaData["filename"])
	if filename == "" {
		rejectHook(w, http.StatusBadRequest, "filename metadata is required")
		return
	}

	// Capacity and readiness are backpressure, not client errors.
	if s.ingest == nil || !s.ingest.Ready() {
		retryHook(w, 5, "server is still recovering queued uploads")
		return
	}
	if !s.ingest.Health().Healthy() {
		retryHook(w, 30, "media storage is unavailable")
		return
	}
	if err := s.ingest.AdmitCapacity(r.Context(), upload.Size); err != nil {
		retryHook(w, 30, "insufficient free space")
		return
	}

	uploadID, err := newUploadIdentifier()
	if err != nil {
		retryHook(w, 5, "could not allocate an upload id")
		return
	}
	if s.ingest.UploadPathsExist(uploadID) {
		retryHook(w, 1, "upload id collision, please retry")
		return
	}

	job := &store.UploadJob{
		UploadID:         uploadID,
		MediaID:          uuid.NewString(),
		OriginalFilename: sanitizeFilename(filename),
		ExpectedSize:     upload.Size,
		DeclaredSHA256:   strings.ToLower(strings.TrimSpace(upload.MetaData["sha256"])),
		GuestName:        sanitizeGuestName(upload.MetaData["guestName"], s.cfg.GuestNameMaxLength),
		UploaderIP:       hookUploaderIP(req),
	}
	if err := s.store.CreateUploadingJob(r.Context(), job); err != nil {
		slog.Error("failed to record upload job", "operation", "pre_create", "error", err)
		retryHook(w, 5, "could not record the upload")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(tusHookResponse{
		ChangeFileInfo: &tusHookChangeFileInfo{ID: uploadID},
	})
}

// newUploadIdentifier returns a URL-safe random id. It is always freshly
// generated so no pre-create outcome can ever ask tusd to open an existing
// data or sidecar path.
func newUploadIdentifier() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func hookUploaderIP(req tusHookRequest) string {
	if ip := firstHeaderValue(req.Event.HTTPRequest.Header, clientIPHeader); ip != "" {
		return ip
	}
	return req.Event.HTTPRequest.RemoteAddr
}
```

Update the dispatcher to establish the deadline before anything else, pass `r`, and accept the new hook types. The deadline has to be taken here rather than inside `pre-finish`, because authentication and body decode happen first and would otherwise sit outside the budget:

```go
func (s *Server) handleTusHook(w http.ResponseWriter, r *http.Request) {
	// One absolute deadline for the whole hook, recorded before authentication
	// and body decode so no later phase can reset it.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UploadDurabilityWait)
	defer cancel()
	r = r.WithContext(ctx)
```

```go
	switch req.Type {
	case "pre-create":
		s.handlePreCreateHook(w, r, req)
	case "pre-finish":
		s.handlePreFinishHook(w, r, req)
	case "post-finish":
		// Non-blocking and unordered: treat it only as an idempotent nudge.
		if s.ingest != nil {
			s.ingest.Wake()
		}
		allowHook(w)
	default:
		allowHook(w)
	}
```

Delete `handlePostFinishHook` and `cleanupTusInfoFile` entirely — that pair is the incident. Task 9 adds `handlePreFinishHook`; add a temporary stub that calls `allowHook(w)` so the package compiles until then.

Add to `Server` in `server.go`:

```go
	ingest *ingest.Manager
```

and

```go
// SetIngest wires the durable ingest manager after construction, because the
// manager needs the processor and store that NewServer already holds.
func (s *Server) SetIngest(m *ingest.Manager) { s.ingest = m }
```

- [ ] **Step 7: Add the manager helpers this handler calls**

Append to `backend/internal/ingest/manager.go`, adding `"fmt"`, `"os"`, and `"path/filepath"` to its imports — this is the first code in that file to need them:

```go
// DataPath and InfoPath are the only two paths tusd's filestore derives from
// an upload id. Deriving them here keeps every absence check consistent.
func (m *Manager) DataPath(uploadID string) string {
	return filepath.Join(m.opts.UploadDir, uploadID)
}

func (m *Manager) InfoPath(uploadID string) string {
	return filepath.Join(m.opts.UploadDir, uploadID+".info")
}

// UploadPathsExist reports whether either derived path is already taken.
func (m *Manager) UploadPathsExist(uploadID string) bool {
	if _, err := os.Stat(m.DataPath(uploadID)); err == nil {
		return true
	}
	if _, err := os.Stat(m.InfoPath(uploadID)); err == nil {
		return true
	}
	return false
}

// AdmitCapacity refuses a create that would push either volume under the free
// space floor. It is deliberately coarse: running out of disk is a transient
// failure that retries, so approximate accounting is safe.
func (m *Manager) AdmitCapacity(ctx context.Context, size int64) error {
	if m.opts.MinFreeBytes <= 0 {
		return nil
	}
	for _, dir := range []string{m.opts.UploadDir, m.processor.MediaDir} {
		free, err := freeBytes(dir)
		if err != nil {
			slog.Warn("could not stat filesystem for admission", "operation", "admit_capacity", "dir", dir, "error", err)
			continue
		}
		if free-size < m.opts.MinFreeBytes {
			return fmt.Errorf("free space on %s would fall below the floor", dir)
		}
	}
	return nil
}
```

Create `backend/internal/ingest/freespace_unix.go`:

```go
//go:build unix

package ingest

import "golang.org/x/sys/unix"

func freeBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
```

Create `backend/internal/ingest/freespace_other.go`:

```go
//go:build !unix

package ingest

import "math"

// Non-unix builds are development only; never gate admission there.
func freeBytes(string) (int64, error) { return math.MaxInt64, nil }
```

Run `go get golang.org/x/sys@latest` inside the toolchain container if that module is not already required.

- [ ] **Step 8: Provide the termination seam the manager depends on**

`Options.Terminator` is typed `ingest.SourceTerminator`, and the harness in Step 1 passes `srv`, so `*Server` must satisfy that interface now — not in Task 10 — or this package will not build. The app already has the implementation: `terminateTusUpload` in `storage_cleanup.go` issues the internal DELETE and treats 204, 404, and 410 as success. Expose it in `tus_proxy.go`:

```go
// Terminate implements ingest.SourceTerminator. Both the incomplete-upload
// janitor and the ingest workers remove tus files through this one path, so
// tusd always cleans up its own sidecar and lock state.
//
// It refuses to remove a source that a pending or processing job still needs.
func (s *Server) Terminate(ctx context.Context, uploadID string) error {
	job, err := s.store.GetUploadJob(ctx, uploadID)
	if err != nil {
		return err
	}
	if job != nil && (job.Status == store.JobPending || job.Status == store.JobProcessing) {
		return fmt.Errorf("upload %s is queued for publication and must not be terminated", uploadID)
	}
	return s.terminateTusUpload(ctx, uploadID)
}
```

Now route the janitor through it. In `backend/internal/httpapi/storage_cleanup.go`, replace `s.terminateTusUpload(ctx, candidate.id)` with:

```go
		// Claim the row before deleting. A status read followed by a DELETE is a
		// race: the janitor decides an upload is an abandoned partial, and
		// between that decision and tusd acting on it the final PATCH can
		// complete and commit `pending`. The conditional claim cannot lose that
		// race — whichever transition commits first, the other sees zero rows.
		if err := s.store.ClaimUploadingForDiscard(ctx, candidate.id, store.NowMicros()); err != nil &&
			!errors.Is(err, store.ErrNotClaimed) {
			slog.Warn("could not claim incomplete upload for removal", "upload_id", candidate.id, "error", err)
			continue
		} else if errors.Is(err, store.ErrNotClaimed) && s.uploadJobExists(ctx, candidate.id) {
			continue // it completed while we were deciding; leave it to ingest
		}
		if err := s.Terminate(ctx, candidate.id); err != nil {
```

with a small helper in the same file:

```go
// uploadJobExists reports whether the queue is tracking this upload at all.
// A rowless partial is legacy residue the janitor may still remove.
func (s *Server) uploadJobExists(ctx context.Context, uploadID string) bool {
	job, err := s.store.GetUploadJob(ctx, uploadID)
	return err != nil || job != nil
}
```

and the matching store method appended to `upload_jobs.go`:

```go
// ClaimUploadingForDiscard atomically moves a still-uploading row to discard,
// so a caller about to delete its files cannot be overtaken by a completion.
// It returns ErrNotClaimed when the row has already moved on.
func (s *Store) ClaimUploadingForDiscard(ctx context.Context, uploadID string, now int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = 'discarding',
		       terminal_reason = CASE WHEN terminal_reason = '' THEN 'cancelled' ELSE terminal_reason END,
		       next_attempt_at = ?,
		       updated_at = ?
		 WHERE upload_id = ? AND status = 'uploading'`, now, now, uploadID)
	if err != nil {
		return fmt.Errorf("claim uploading for discard: %w", err)
	}
	return requireOneRow(res)
}
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 go test ./internal/httpapi/ ./internal/ingest/ -race -v`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add backend/internal/httpapi/ backend/internal/ingest/ backend/internal/store/upload_jobs.go
git commit -m "feat: admit uploads through pre-create with a durable job row"
```

---

### Task 9: Pre-finish durability barrier

The fix for the lost uploads. After this task, a tus 204 means the application durably owns the bytes.

**Files:**
- Modify: `backend/internal/ingest/durability.go`
- Modify: `backend/internal/httpapi/tus_hooks.go`
- Test: `backend/internal/ingest/durability_test.go`
- Test: `backend/internal/httpapi/tus_hooks_test.go`

**Interfaces:**
- Consumes: `store.PromoteToPending`, `media.FsyncFile`, `media.FsyncDir`.
- Produces: `func (m *Manager) EnsureDurable(ctx context.Context, uploadID string) error` and `var ErrDurabilityBusy error`. `EnsureDurable` is idempotent: concurrent callers join one operation, and the operation keeps running on the lifetime context even if every caller has given up.

- [ ] **Step 1: Write the failing test**

Create `backend/internal/ingest/durability_test.go`:

```go
package ingest

import (
	"context"
	"os"
	"testing"
	"time"

	"event-gallery/backend/internal/store"
)

func seedCompleteUpload(t *testing.T, m *Manager, st *store.Store, uploadID string, payload []byte) {
	t.Helper()
	job := &store.UploadJob{
		UploadID:         uploadID,
		MediaID:          "media-" + uploadID,
		OriginalFilename: "a.jpg",
		ExpectedSize:     int64(len(payload)),
	}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := os.WriteFile(m.DataPath(uploadID), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := os.WriteFile(m.InfoPath(uploadID), []byte(`{"ID":"`+uploadID+`"}`), 0o600); err != nil {
		t.Fatalf("write info: %v", err)
	}
}

func TestEnsureDurableCommitsPending(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))

	if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
		t.Fatalf("ensure durable: %v", err)
	}

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobPending {
		t.Fatalf("status = %q, want pending", job.Status)
	}
	if job.SourceCompletedAt == nil {
		t.Error("source_completed_at must be committed")
	}
	// The barrier must not move, hash, or delete anything.
	if _, err := os.Stat(m.DataPath("u1")); err != nil {
		t.Errorf("data file must survive: %v", err)
	}
	if _, err := os.Stat(m.InfoPath("u1")); err != nil {
		t.Errorf("sidecar must survive: %v", err)
	}
}

func TestEnsureDurableIsIdempotent(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))
	for i := 0; i < 3; i++ {
		if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestEnsureDurableCommitsEvenAfterCallerGivesUp(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))

	// The caller's budget expires immediately. The work must not be wasted:
	// it continues on the manager's lifetime context and still commits.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = m.EnsureDurable(ctx, "u1")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := st.GetUploadJob(context.Background(), "u1")
		if job.Status == store.JobPending {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("detached durability operation never committed")
}

func TestEnsureDurableRefusesCancelledUpload(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))
	if err := st.RequestCancellation(context.Background(), "u1", store.NowMicros()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := m.EnsureDurable(context.Background(), "u1"); err == nil {
		t.Fatal("promotion must fail closed after cancellation")
	}
}
```

Every test in this package that drives the queue by hand — `EnsureDurable`, `claimAndRunOnce`, `reconcileOnce` — deliberately does **not** call `m.Start()`. `New` creates the lifetime context, so all three work on an unstarted manager, and starting the pool would let a worker claim the job first and race the assertions. `defer m.Stop()` is still needed to wait for detached durability goroutines.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/ingest/ -run Durable -v`
Expected: FAIL — `m.EnsureDurable undefined`.

- [ ] **Step 3: Implement the durability operation**

Replace `backend/internal/ingest/durability.go`:

```go
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/store"
)

// ErrDurabilityBusy means the fixed executor is saturated. Callers must relay
// backpressure to the browser, never report success.
var ErrDurabilityBusy = errors.New("durability executor is saturated")

type durabilityOp struct {
	done chan struct{}
	err  error
}

// durabilityRegistry keys one in-flight operation per upload id so that a
// retried PATCH, a repeated hook, and the proxy fence all join the same work
// instead of racing.
type durabilityRegistry struct {
	manager  *Manager
	mu       sync.Mutex
	inFlight map[string]*durabilityOp
	slots    chan struct{}
	wg       sync.WaitGroup
}

func newDurabilityRegistry(m *Manager) *durabilityRegistry {
	workers := m.opts.DurabilityWorkers
	if workers <= 0 {
		workers = 2
	}
	return &durabilityRegistry{
		manager:  m,
		inFlight: make(map[string]*durabilityOp),
		slots:    make(chan struct{}, workers),
	}
}

// wait blocks until every detached operation has finished, so shutdown cannot
// close the database while a promotion is still committing.
func (r *durabilityRegistry) wait() { r.wg.Wait() }

// EnsureDurable fsyncs a completed upload's source and commits its pending
// row. Returning nil is the only thing that entitles tus to report success.
//
// The operation itself runs on the manager's lifetime context, so a caller
// whose HTTP budget expires abandons only its own wait — the bytes still
// become durable and the browser's retry finds the work already done.
func (m *Manager) EnsureDurable(ctx context.Context, uploadID string) error {
	return m.durability.ensure(ctx, uploadID)
}

func (r *durabilityRegistry) ensure(ctx context.Context, uploadID string) error {
	r.mu.Lock()
	op, joined := r.inFlight[uploadID]
	if !joined {
		select {
		case r.slots <- struct{}{}:
		default:
			r.mu.Unlock()
			return ErrDurabilityBusy
		}
		op = &durabilityOp{done: make(chan struct{})}
		r.inFlight[uploadID] = op
		r.wg.Add(1)
		go r.run(uploadID, op)
	}
	r.mu.Unlock()

	select {
	case <-op.done:
		return op.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *durabilityRegistry) run(uploadID string, op *durabilityOp) {
	defer func() {
		r.mu.Lock()
		delete(r.inFlight, uploadID)
		r.mu.Unlock()
		<-r.slots
		close(op.done)
		r.wg.Done()
	}()

	// Bound the detached work so a stalled fsync cannot hold a slot forever.
	ctx, cancel := context.WithTimeout(r.manager.lifetime, r.manager.opts.ProcessingTimeout)
	defer cancel()

	op.err = r.manager.makeDurable(ctx, uploadID)
	if op.err != nil {
		slog.Error("durability barrier failed", "operation", "durability", "upload_id", uploadID, "error", op.err)
		return
	}
	slog.Info("upload became durable", "operation", "durability", "upload_id", uploadID)
	r.manager.Wake()
}

// makeDurable performs the fsync barrier and the promotion. It never moves,
// hashes, decodes, or deletes anything: that work belongs to the workers,
// where it cannot delay or fail the client's request.
func (m *Manager) makeDurable(ctx context.Context, uploadID string) error {
	job, err := m.store.GetUploadJob(ctx, uploadID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("no upload job for %s", uploadID)
	}
	switch job.Status {
	case store.JobUploading:
		// Needs the barrier; continue below.
	case store.JobDiscarding, store.JobDiscarded:
		// Reporting success here would tell the browser its upload is safe
		// while the discard worker is removing it.
		return fmt.Errorf("upload %s is being discarded", uploadID)
	default:
		return nil // already durable; nothing to do
	}

	dataPath := m.DataPath(uploadID)
	stat, err := os.Stat(dataPath)
	if err != nil {
		return fmt.Errorf("stat completed source: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("source %s is not a regular file", dataPath)
	}
	if stat.Size() != job.ExpectedSize {
		return fmt.Errorf("source size %d, expected %d", stat.Size(), job.ExpectedSize)
	}

	if err := media.FsyncFile(dataPath); err != nil {
		return err
	}
	if err := media.FsyncFile(m.InfoPath(uploadID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := media.FsyncDir(m.opts.UploadDir); err != nil {
		return err
	}

	return m.store.PromoteToPending(ctx, uploadID, store.NowMicros())
}
```

- [ ] **Step 4: Add the blocking hook handler**

Replace the `handlePreFinishHook` stub in `tus_hooks.go`:

```go
// handlePreFinishHook runs inside the upload's final PATCH, and its response
// becomes that PATCH's response. Completing it successfully is precisely what
// makes the browser's success message truthful.
//
// The budget was already established by handleTusHook, so every phase here
// shares one absolute deadline.
func (s *Server) handlePreFinishHook(w http.ResponseWriter, r *http.Request, req tusHookRequest) {
	ctx := r.Context()

	upload := req.Event.Upload
	if s.ingest == nil {
		preFinishRetry(w, "ingest is not ready")
		return
	}
	if upload.ID == "" || !safeUploadID(upload.ID) {
		preFinishRetry(w, "invalid upload id")
		return
	}
	if req.Event.Upload.Storage.Type != "filestore" {
		preFinishRetry(w, "unsupported storage backend")
		return
	}
	// The hook must be talking about the path we derive, not one it chose.
	if filepath.Clean(upload.Storage.Path) != filepath.Clean(s.ingest.DataPath(upload.ID)) {
		preFinishRetry(w, "unexpected storage path")
		return
	}
	if upload.Size <= 0 || upload.Offset != upload.Size {
		preFinishRetry(w, "upload is not complete")
		return
	}

	switch err := s.ingest.EnsureDurable(ctx, upload.ID); {
	case err == nil:
		allowHook(w)
	default:
		// Saturation or an expired budget is backpressure. The detached
		// operation continues, so the retry will usually find it already done.
		slog.Warn("durability barrier did not complete within the request budget",
			"operation", "pre_finish", "upload_id", upload.ID, "error", err)
		preFinishRetry(w, "upload is still being persisted, please retry")
	}
}

// preFinishRetry is the only failure response pre-finish has. tusd ignores
// RejectUpload for this hook — the upload is already written and cannot be
// rejected here — and merges HTTPResponse into the final PATCH response. So
// every failure is reported as retryable backpressure, and it is the
// completion fence, not this status, that stops a false success. Nothing is
// lost either way: an upload we refuse to promote is picked up later by
// reconciliation.
func preFinishRetry(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(tusHookResponse{
		HTTPResponse: &tusHookHTTPResponse{
			StatusCode: http.StatusServiceUnavailable,
			Body:       `{"error":"` + message + `"}`,
			Header: map[string]string{
				"Content-Type": "application/json",
				"Retry-After":  "5",
			},
		},
	})
}

// safeUploadID rejects anything that could escape the upload directory.
func safeUploadID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
```

Add `"path/filepath"` and `"context"` to the imports.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 go test ./internal/ingest/ ./internal/httpapi/ -race -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/ingest/durability.go backend/internal/ingest/durability_test.go backend/internal/httpapi/tus_hooks.go
git commit -m "feat: make uploads durable in a blocking pre-finish hook"
```

---

### Task 10: Completion fence and cancellation

**Files:**
- Modify: `backend/internal/httpapi/tus_proxy.go`
- Modify: `backend/internal/httpapi/tus_hooks.go`
- Test: `backend/internal/httpapi/tus_proxy_test.go`

**Interfaces:**
- Consumes: `Manager.EnsureDurable`, `store.RequestCancellation`, `store.GetUploadJob`.
- Produces: `func (s *Server) fenceCompletedUpload(w http.ResponseWriter, r *http.Request, uploadID string) bool` — returns true when it has written a response and the request must not be forwarded — and `func (s *Server) Terminate(ctx context.Context, uploadID string) error`, the single implementation of `ingest.SourceTerminator`.

- [ ] **Step 1: Write the failing test**

Append to `backend/internal/httpapi/tus_proxy_test.go`:

```go
func seedUploadingJob(t *testing.T, h *testHarness, uploadID string, size int64) {
	t.Helper()
	err := h.store.CreateUploadingJob(context.Background(), &store.UploadJob{
		UploadID:         uploadID,
		MediaID:          "media-" + uploadID,
		OriginalFilename: "a.jpg",
		ExpectedSize:     size,
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
}

// Upload ids must be hex: safeUploadID rejects anything else, and
// tusUploadIDFromPath would return "" so the handler would never see the job.
const testUploadID = "a1b2c3"

func TestFenceBlocksSuccessUntilDurable(t *testing.T) {
	h := newTestHarness(t)
	// A complete source whose row is still 'uploading' must never be reported
	// as successful, because tusd would otherwise short-circuit an
	// already-complete upload with a plain 204.
	payload := []byte("complete bytes")
	seedUploadingJob(t, h, testUploadID, int64(len(payload)))
	if err := os.WriteFile(filepath.Join(h.cfg.TusUploadDir, testUploadID), payload, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	req := httptest.NewRequest(http.MethodHead, "/api/tus/"+testUploadID, nil)
	rec := httptest.NewRecorder()
	intercepted := h.server.fenceCompletedUpload(rec, req, testUploadID)

	// Either the fence promoted the upload itself, or it answered 503. What it
	// must never do is let the request through while the row says 'uploading'.
	job, _ := h.store.GetUploadJob(context.Background(), testUploadID)
	if !intercepted && job.Status == store.JobUploading {
		t.Fatal("fence let an undurable complete upload through")
	}
	if intercepted {
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Error("fence must tell the client when to retry")
		}
	}
}

func TestDeleteAfterDurabilityReturns409(t *testing.T) {
	h := newTestHarness(t)
	seedUploadingJob(t, h, testUploadID, 10)
	if err := h.store.PromoteToPending(context.Background(), testUploadID, store.NowMicros()); err != nil {
		t.Fatalf("promote: %v", err)
	}

	rec := doRequest(h, http.MethodDelete, "/api/tus/"+testUploadID, nil)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: a durable completion is never silently reversed", rec.Code)
	}
}

func TestDeleteBeforeDurabilityRecordsCancellation(t *testing.T) {
	h := newTestHarness(t)
	seedUploadingJob(t, h, testUploadID, 10)

	rec := doRequest(h, http.MethodDelete, "/api/tus/"+testUploadID, nil)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	job, _ := h.store.GetUploadJob(context.Background(), testUploadID)
	if job.CancellationRequestedAt == nil {
		t.Error("cancellation intent must be durable")
	}
}

func TestDeleteOfACompleteUploadIsRefused(t *testing.T) {
	h := newTestHarness(t)
	payload := []byte("complete bytes")
	seedUploadingJob(t, h, testUploadID, int64(len(payload)))
	if err := os.WriteFile(filepath.Join(h.cfg.TusUploadDir, testUploadID), payload, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	// The client gave up after backpressure and cancelled. The bytes are
	// complete, so the fence must promote them rather than let the cancellation
	// destroy the guest's only copy.
	rec := doRequest(h, http.MethodDelete, "/api/tus/"+testUploadID, nil)

	if rec.Code == http.StatusNoContent {
		t.Fatal("a complete upload must not be cancellable")
	}
	job, _ := h.store.GetUploadJob(context.Background(), testUploadID)
	if job.CancellationRequestedAt != nil {
		t.Error("a complete upload must not record cancellation intent")
	}
}

func TestClientDeleteIsNeverForwardedToTusd(t *testing.T) {
	h := newTestHarness(t)
	forwarded := false
	tusd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer tusd.Close()
	h.withTusTarget(t, tusd.URL)

	// An id this proxy cannot parse must still never reach tusd's terminate
	// path; that is what makes client termination structurally impossible.
	doRequest(h, http.MethodDelete, "/api/tus/../../etc/passwd", nil)
	doRequest(h, http.MethodDelete, "/api/tus/unknown-id", nil)

	if forwarded {
		t.Fatal("a client DELETE reached tusd")
	}
}
```

Add the three seed helpers to the test file, following `seedCompleteUpload` in the ingest package.
- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/httpapi/ -run 'Fence|Delete' -v`
Expected: FAIL — `s.fenceCompletedUpload undefined`.

- [ ] **Step 3: Implement the fence and cancellation**

Add to `tus_proxy.go`:

```go
// fenceCompletedUpload stops the proxy from reporting success that the
// database has not recorded. tusd answers an already-complete upload with a
// plain 204 without re-running pre-finish, so without this a client that was
// told 503 could retry and be told success while the row is still 'uploading'.
//
// It fails closed: if we cannot read the row, we do not know whether success
// would be truthful, and an unnecessary retry is always cheaper than a false
// success.
func (s *Server) fenceCompletedUpload(w http.ResponseWriter, r *http.Request, uploadID string) bool {
	if s.ingest == nil {
		return false
	}
	// Before the startup inventory finishes we cannot tell a rowless orphan
	// from an upload we have not inventoried yet, so nothing is forwarded.
	if !s.ingest.Ready() {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "server is still recovering queued uploads")
		return true
	}
	if uploadID == "" {
		return false
	}
	job, err := s.store.GetUploadJob(r.Context(), uploadID)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "upload state is temporarily unavailable, please retry")
		return true
	}
	if job == nil || job.Status != store.JobUploading {
		return false
	}

	stat, err := os.Stat(s.ingest.DataPath(uploadID))
	if err != nil || stat.Size() != job.ExpectedSize {
		return false // still uploading; let the PATCH through
	}

	// Bound the wait with the same budget the hook uses. A proxied HEAD or
	// PATCH has no deadline of its own, and the operation it joins is bounded
	// by the processing timeout, so without this the client could hang.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UploadDurabilityWait)
	defer cancel()
	if err := s.ingest.EnsureDurable(ctx, uploadID); err == nil {
		return false // now durable; forwarding is safe
	}
	w.Header().Set("Retry-After", "5")
	writeError(w, http.StatusServiceUnavailable, "upload is still being persisted, please retry")
	return true
}

// handleTusDelete consumes a public DELETE as cancellation intent. No client
// DELETE is ever forwarded to tusd, which is what makes it structurally
// impossible for a browser to terminate a durable upload.
func (s *Server) handleTusDelete(w http.ResponseWriter, r *http.Request, uploadID string) {
	if uploadID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	job, err := s.store.GetUploadJob(r.Context(), uploadID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read upload state")
		return
	}
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if job.Status != store.JobUploading {
		writeError(w, http.StatusConflict, "upload already completed and cannot be cancelled")
		return
	}
	if err := s.store.RequestCancellation(r.Context(), uploadID, store.NowMicros()); err != nil {
		if !errors.Is(err, store.ErrNotClaimed) {
			writeError(w, http.StatusInternalServerError, "could not record cancellation")
			return
		}
		// Promotion won the race between our read and this write. The upload is
		// durable now, so the answer is the same as if we had seen it above.
		writeError(w, http.StatusConflict, "upload already completed and cannot be cancelled")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

In `handleTusProxy`, before forwarding, route by method. DELETE runs the completion fence **first**: if the bytes are already complete, the fence promotes them and `handleTusDelete`'s re-read then answers 409, so a cancellation can never destroy a complete upload. This matters because a saturated durability executor answers 503 without scheduling anything, and the current client exhausts its five fixed retries and cancels — which would otherwise delete the guest's only complete copy. The DELETE branch is also unconditional, so an id this code cannot parse still never reaches tusd:

```go
	if r.Method == http.MethodDelete {
		uploadID := tusUploadIDFromPath(r.URL.Path)
		if s.fenceCompletedUpload(w, r, uploadID) {
			return
		}
		s.handleTusDelete(w, r, uploadID)
		return
	}
	uploadID := tusUploadIDFromPath(r.URL.Path)
	if (r.Method == http.MethodHead || r.Method == http.MethodPatch) && s.fenceCompletedUpload(w, r, uploadID) {
		return
	}
```

Add the helper:

```go
// tusUploadIDFromPath returns the final path segment of /api/tus/<id>.
func tusUploadIDFromPath(p string) string {
	id := path.Base(strings.TrimSuffix(p, "/"))
	if id == "tus" || id == "." || id == "/" {
		return ""
	}
	if !safeUploadID(id) {
		return ""
	}
	return id
}
```

- [ ] **Step 4: Note on termination**

The `Terminate` seam was added in Task 8 because the test harness needs it. Do **not** enable tusd's `pre-terminate` hook. It would exist to stop a client from terminating an upload, but `handleTusProxy` now refuses every client DELETE before parsing, so no client request can reach tusd's terminate path at all. Enabling it would instead break the existing janitor, whose DELETEs carry no queue state, and would add a hook, a forwarded header, and an authorization scheme with no threat left to defend against.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 go test ./internal/httpapi/ -race -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/httpapi/
git commit -m "feat: fence completions and refuse client terminations"
```

---

### Task 11: Processing, publication, and cleanup

**Files:**
- Create: `backend/internal/ingest/process.go`
- Create: `backend/internal/ingest/process_test.go`
- Modify: `backend/internal/ingest/manager.go` (replace the `claimAndRunOnce` stub)
- Modify: `backend/internal/store/upload_jobs.go` (add `PublishMedia`)

**Interfaces:**
- Consumes: `media.Processor.PrepareOriginal`, `media.Processor.VerifyOriginal`, `store.ClaimNextJob`, `store.FinishJob`, `Options.Terminator`.
- Produces: `func (s *Store) PublishMedia(ctx context.Context, uploadID, leaseToken string, item *models.MediaItem, now int64) (resultMediaID string, isDuplicate bool, err error)`, `func (s *Store) RecordArtifactIdentity(...) error`, `func (s *Store) RecordPrepared(...) error`, and `func (m *Manager) claimAndRunOnce() (bool, error)`.

- [ ] **Step 1: Write the failing test**

Create `backend/internal/ingest/process_test.go`:

```go
package ingest

import (
	"context"
	"os"
	"strings"
	"testing"

	"event-gallery/backend/internal/store"
)

func drainQueue(t *testing.T, m *Manager) {
	t.Helper()
	for i := 0; i < 50; i++ {
		worked, err := m.claimAndRunOnce()
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("queue did not drain")
}

func TestPublishDeletesSourceOnlyAfterCommit(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	seedCompleteUpload(t, m, st, "u1", payload)
	if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
		t.Fatalf("durable: %v", err)
	}

	drainQueue(t, m)

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete (last error: %s)", job.Status, job.LastError)
	}
	if job.ResultMediaID == "" {
		t.Error("published job must record its media id")
	}
	if _, err := os.Stat(m.DataPath("u1")); !os.IsNotExist(err) {
		t.Error("source must be removed after publication commits")
	}
	if err := proc.VerifyOriginal(job.StoredFilename, int64(len(payload)), job.AuthoritativeSHA256); err != nil {
		t.Errorf("published original must be intact: %v", err)
	}
}

func TestTransientFailureNeverDeletesTheSource(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	m := New(st, proc, opts)
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", jpegFixture(t))
	_ = m.EnsureDurable(context.Background(), "u1")

	// Make preparation fail transiently. A regular file where the originals
	// directory must be makes every create and rename fail with ENOTDIR, which
	// works regardless of uid — chmod would not, because the test container
	// runs as root and root bypasses directory permission bits.
	originals := proc.OriginalsDir()
	if err := os.RemoveAll(originals); err != nil {
		t.Fatalf("clear originals: %v", err)
	}
	if err := os.WriteFile(originals, []byte("blocker"), 0o600); err != nil {
		t.Fatalf("block originals dir: %v", err)
	}

	_, _ = m.claimAndRunOnce()

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobPending {
		t.Errorf("status = %q, want pending: transient failures retry forever", job.Status)
	}
	if job.ProcessingFailures != 1 {
		t.Errorf("processing_failures = %d, want 1", job.ProcessingFailures)
	}
	if _, err := os.Stat(m.DataPath("u1")); err != nil {
		t.Fatalf("THE INCIDENT: source deleted on a transient error: %v", err)
	}
}

func TestChecksumMismatchDiscardsBeforeAnyArtifactExists(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	job := &store.UploadJob{
		UploadID:         "u1",
		MediaID:          "media-u1",
		OriginalFilename: "a.jpg",
		ExpectedSize:     int64(len(payload)),
		DeclaredSHA256:   "0000000000000000000000000000000000000000000000000000000000000000",
	}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(m.DataPath("u1"), payload, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = m.EnsureDurable(context.Background(), "u1")

	drainQueue(t, m)

	got, _ := st.GetUploadJob(context.Background(), "u1")
	if got.Status != store.JobDiscarded {
		t.Errorf("status = %q, want discarded", got.Status)
	}
	if got.TerminalReason != "checksum_mismatch" {
		t.Errorf("terminal_reason = %q, want checksum_mismatch", got.TerminalReason)
	}
}

func TestDuplicateResolutionValidatesTheSurvivingOriginal(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)

	// A second, intact row so the storage health sample stays positive.
	// Without it the health gate trips first and this test would pass while
	// never reaching the duplicate-validation code at all.
	insertMediaRow(t, st, "keeper", "keeper.jpg", "keeper-hash")
	if err := os.WriteFile(proc.OriginalPath("keeper.jpg"), []byte("keeper"), 0o600); err != nil {
		t.Fatalf("write keeper: %v", err)
	}

	// Publish once.
	seedCompleteUpload(t, m, st, "u1", payload)
	_ = m.EnsureDurable(context.Background(), "u1")
	drainQueue(t, m)

	// Corrupt the surviving original, then upload the same bytes again.
	// Corrupting rather than deleting keeps the health sample positive, so the
	// failure is attributable to hash validation and nothing else.
	first, _ := st.GetUploadJob(context.Background(), "u1")
	if err := os.WriteFile(proc.OriginalPath(first.StoredFilename), []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt original: %v", err)
	}

	seedCompleteUpload(t, m, st, "u2", payload)
	_ = m.EnsureDurable(context.Background(), "u2")
	_, _ = m.claimAndRunOnce()

	second, _ := st.GetUploadJob(context.Background(), "u2")
	if second.Status != store.JobPending {
		t.Errorf("status = %q, want pending: a corrupt authoritative original is an integrity fault, not a duplicate", second.Status)
	}
	if !strings.Contains(second.LastError, "integrity fault") {
		t.Errorf("last_error = %q, want it to name the integrity fault (proves which branch ran)", second.LastError)
	}
	if _, err := os.Stat(m.DataPath("u2")); err != nil {
		t.Error("the new copy's source must be retained while the old one is broken")
	}
}
```

Add a `jpegFixture(t)` helper to `fixture_test.go` that returns the bytes of a tiny valid JPEG (encode a 2×2 `image.NewRGBA` with `jpeg.Encode` into a `bytes.Buffer`).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/ingest/ -run 'Publish|Transient|Checksum|Duplicate' -v`
Expected: FAIL — publication is not implemented.

- [ ] **Step 3: Add the publication transaction**

Append to `backend/internal/store/upload_jobs.go`, and add `"event-gallery/backend/internal/models"` to its imports — this is the first code in that file to need it:

```go
// PublishMedia inserts the media row and moves the job to cleanup in one
// transaction, so a crash can never leave a published file with no job or a
// finished job with no row. It returns the authoritative media id for this
// content, which may belong to an earlier upload of the same bytes.
func (s *Store) PublishMedia(ctx context.Context, uploadID, leaseToken string, item *models.MediaItem, now int64) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("begin publish: %w", err)
	}
	defer tx.Rollback()

	// Fence and take the write lock in one statement. Starting with a read
	// would open a deferred transaction that has to upgrade later, and SQLite
	// answers that upgrade with SQLITE_BUSY_SNAPSHOT, which busy_timeout does
	// not retry.
	fence, err := tx.ExecContext(ctx,
		`UPDATE upload_jobs SET updated_at = ? WHERE upload_id = ? AND lease_token = ? AND status = 'processing'`,
		now, uploadID, leaseToken)
	if err != nil {
		return "", false, fmt.Errorf("fence publish: %w", err)
	}
	if owned, err := fence.RowsAffected(); err != nil {
		return "", false, fmt.Errorf("rows affected: %w", err)
	} else if owned == 0 {
		return "", false, ErrNotClaimed
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO media_items (
			id, original_filename, stored_filename, kind, mime_type, size_bytes, sha256,
			width, height, duration_seconds, has_thumbnail, captured_at, uploaded_at, approved_at,
			uploader_name, uploader_ip, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			CASE WHEN COALESCE((SELECT value FROM app_config WHERE key = ?), 'false') = 'false' THEN ? ELSE NULL END,
			?, ?, ?)
		ON CONFLICT(sha256) DO NOTHING`,
		item.ID, item.OriginalFilename, item.StoredFilename, string(item.Kind), item.MimeType,
		item.SizeBytes, item.SHA256, nullableInt(item.Width), nullableInt(item.Height),
		nullableFloat(item.DurationSeconds), boolToInt(item.HasThumbnail),
		formatTimePtr(item.CapturedAt), formatTime(item.UploadedAt), ConfigKeyApprovalRequired,
		formatTime(item.UploadedAt), item.UploaderName, item.UploaderIP, string(models.StatusActive),
	)
	if err != nil {
		return "", false, fmt.Errorf("insert media: %w", err)
	}

	// Whoever won the unique hash constraint owns this content.
	var authoritativeID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM media_items WHERE sha256 = ?`, item.SHA256).Scan(&authoritativeID); err != nil {
		return "", false, fmt.Errorf("resolve authoritative media: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = 'cleanup', result_media_id = ?, lease_token = NULL, lease_until = NULL, updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		authoritativeID, now, uploadID, leaseToken)
	if err != nil {
		return "", false, fmt.Errorf("transition to cleanup: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("commit publish: %w", err)
	}
	return authoritativeID, authoritativeID != item.ID, nil
}
```

- [ ] **Step 4: Implement the worker body**

Create `backend/internal/ingest/process.go`:

```go
package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/models"
	"event-gallery/backend/internal/store"
)

// claimAndRunOnce takes one due job and advances it by one stage. It reports
// whether it did any work so the worker can drain a burst before sleeping.
func (m *Manager) claimAndRunOnce() (bool, error) {
	now := store.NowMicros()

	stages := []struct {
		from, to store.JobStatus
		run      func(*store.UploadJob)
	}{
		// Cleanup and discard come first. They are one stat plus one HTTP
		// DELETE, and they are what returns disk to the pool; processing is a
		// whole-file copy. Running them last would mean that under sustained
		// load every tus source is retained for the entire burst on top of its
		// permanent copy, and INGEST_MIN_FREE_BYTES would start refusing new
		// uploads because of work already finished.
		{store.JobCleanup, store.JobCleanup, m.runCleanup},
		{store.JobDiscarding, store.JobDiscarding, m.runDiscard},
		{store.JobPending, store.JobProcessing, m.runProcessing},
		// Reclaims work abandoned by an expired lease without waiting for a
		// restart. The claim's own lease_until filter makes this a no-op while
		// another worker still owns the job.
		{store.JobProcessing, store.JobProcessing, m.runProcessing},
	}

	for _, stage := range stages {
		job, err := m.store.ClaimNextJob(m.lifetime, stage.from, stage.to, now, m.leaseDuration())
		if err != nil {
			return false, err
		}
		if job != nil {
			stage.run(job)
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) runProcessing(job *store.UploadJob) {
	// The deadline starts at claim time, before the health check, so the
	// attempt context always expires before the lease does.
	ctx, cancel := context.WithTimeout(m.lifetime, m.opts.ProcessingTimeout)
	defer cancel()

	// Publishing into a media volume we cannot prove is mounted would write the
	// original somewhere the real volume cannot see, and that new file would
	// then satisfy the health gate's "one original exists" test and authorize
	// deleting the tus source.
	if err := m.health.Check(ctx); err != nil {
		next := store.NowMicros() + m.backoffFor(job.ProcessingFailures).Microseconds()
		if err := m.store.ReleaseForRetry(m.lifetime, job.UploadID, job.LeaseToken, store.JobPending, next, "processing_failures", truncateError(err)); err != nil {
			slog.Error("failed to reschedule after health failure", "operation", "processing", "upload_id", job.UploadID, "error", err)
		}
		return
	}

	if err := m.prepareAndPublish(ctx, job); err != nil {
		var rejection *clientRejection
		if errors.As(err, &rejection) {
			// The only class permitted to discard a complete source, and only
			// while no final artifact exists yet.
			m.recordUploadAudit(job, "", rejection.auditDetail)
			if err := m.store.FinishJob(m.lifetime, job.UploadID, job.LeaseToken, store.JobDiscarding, rejection.reason, store.NowMicros()); err != nil {
				slog.Error("failed to record rejection", "operation", "processing", "upload_id", job.UploadID, "error", err)
			}
			slog.Warn("rejected upload", "operation", "processing", "upload_id", job.UploadID, "reason", rejection.reason)
			m.Wake()
			return
		}

		// Everything else is transient and keeps both the source and any
		// prepared artifacts. There is no failure count that deletes data.
		next := store.NowMicros() + m.backoffFor(job.ProcessingFailures).Microseconds()
		if err := m.store.ReleaseForRetry(m.lifetime, job.UploadID, job.LeaseToken, store.JobPending, next, "processing_failures", truncateError(err)); err != nil {
			slog.Error("failed to schedule retry", "operation", "processing", "upload_id", job.UploadID, "error", err)
		}
		slog.Warn("ingest attempt failed, retrying",
			"operation", "processing", "upload_id", job.UploadID,
			"processing_failures", job.ProcessingFailures+1, "error", err)
		return
	}
	m.Wake()
}

// clientRejection marks a deterministic client fault. Repeating the work
// cannot change the outcome, so the source may be discarded.
type clientRejection struct {
	reason      string
	auditDetail string
}

func (e *clientRejection) Error() string { return "client rejection: " + e.reason }

// recordUploadAudit preserves the admin audit-log coverage the deleted
// post-finish handler used to provide. Best effort by design: an audit write
// must never fail an upload.
func (m *Manager) recordUploadAudit(job *store.UploadJob, mediaID, detail string) {
	actor := job.GuestName
	if actor == "" {
		actor = "anonymous guest"
	}
	_ = m.store.RecordAudit(m.lifetime, models.ActionUpload, actor, mediaID, job.OriginalFilename, detail)
}

func (m *Manager) prepareAndPublish(ctx context.Context, job *store.UploadJob) error {
	sourcePath := m.DataPath(job.UploadID)

	// Sniff reports unrecognised content as an error, not as an empty MIME
	// type, so the deterministic rejection has to be recognised here or
	// garbage would retry forever.
	mimeType, kind, err := media.Sniff(sourcePath)
	if err != nil {
		var unsupported *media.ErrUnsupportedType
		if errors.As(err, &unsupported) {
			return &clientRejection{reason: "unsupported_type", auditDetail: "rejected: unsupported or unrecognized file type"}
		}
		return fmt.Errorf("sniff source: %w", err)
	}
	if !media.IsAllowed(mimeType, kind, m.processor.AllowedImageMIMEs, m.processor.AllowedVideoMIMEs) {
		return &clientRejection{reason: "unsupported_type", auditDetail: "rejected: unsupported or unrecognized file type"}
	}

	sum, err := media.SHA256FileContext(ctx, sourcePath)
	if err != nil {
		return fmt.Errorf("hash source: %w", err)
	}
	if job.DeclaredSHA256 != "" && job.DeclaredSHA256 != sum {
		return &clientRejection{reason: "checksum_mismatch", auditDetail: "rejected: checksum mismatch after upload"}
	}

	stat, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	// Persist the artifact identity before the copy, so a crash can be
	// recovered by name and hash instead of by re-copying or deleting.
	storedFilename := job.MediaID + media.ExtensionForMIME(mimeType, job.OriginalFilename)
	if err := m.store.RecordArtifactIdentity(ctx, job.UploadID, job.LeaseToken, storedFilename, mimeType, sum); err != nil {
		return err
	}

	if err := m.processor.PrepareOriginal(ctx, media.PrepareRequest{
		SourcePath:     sourcePath,
		MediaID:        job.MediaID,
		LeaseToken:     job.LeaseToken,
		StoredFilename: storedFilename,
		Size:           stat.Size(),
		SHA256:         sum,
	}); err != nil {
		return fmt.Errorf("prepare original: %w", err)
	}
	if err := m.processor.VerifyOriginal(storedFilename, stat.Size(), sum); err != nil {
		return fmt.Errorf("verify prepared original: %w", err)
	}
	// prepared_at means "a verified final original exists", so it is only
	// committed once that is true.
	if err := m.store.RecordPrepared(ctx, job.UploadID, job.LeaseToken); err != nil {
		return err
	}

	// Before we may resolve a duplicate, the copy we would keep must be proven
	// intact. Otherwise "duplicate detected" could delete a good new file in
	// favor of a corrupt old one.
	existing, err := m.store.GetBySHA256(ctx, sum)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("look up existing content: %w", err)
	}
	if existing != nil {
		if err := m.processor.VerifyOriginal(existing.StoredFilename, existing.SizeBytes, existing.SHA256); err != nil {
			slog.Error("authoritative original failed validation; refusing to treat this upload as a duplicate",
				"operation", "duplicate_check", "upload_id", job.UploadID, "existing_id", existing.ID, "error", err)
			return fmt.Errorf("integrity fault on existing original %s: %w", existing.ID, err)
		}
	}

	result := m.processor.Derive(ctx, m.processor.OriginalPath(storedFilename), job.MediaID, kind)

	item := &models.MediaItem{
		ID:               job.MediaID,
		OriginalFilename: job.OriginalFilename,
		StoredFilename:   storedFilename,
		Kind:             kind,
		MimeType:         mimeType,
		SizeBytes:        stat.Size(),
		SHA256:           sum,
		Width:            result.Width,
		Height:           result.Height,
		DurationSeconds:  result.DurationSeconds,
		HasThumbnail:     result.HasThumbnail,
		CapturedAt:       result.CapturedAt,
		UploadedAt:       time.Now(),
		UploaderName:     job.GuestName,
		UploaderIP:       job.UploaderIP,
	}

	resultID, duplicate, err := m.store.PublishMedia(ctx, job.UploadID, job.LeaseToken, item, store.NowMicros())
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	if duplicate {
		m.recordUploadAudit(job, resultID, "duplicate content ignored (already in gallery)")
	} else {
		m.recordUploadAudit(job, resultID, "")
	}
	slog.Info("upload published", "operation", "publication",
		"upload_id", job.UploadID, "media_id", resultID, "duplicate", duplicate)
	return nil
}

// runCleanup removes the source, and a duplicate's own artifacts, now that a
// media row is committed. This is the only path that may delete a source
// after a successful publication.
func (m *Manager) runCleanup(job *store.UploadJob) {
	if !m.deletionAllowed(job) {
		return
	}
	if job.ResultMediaID != "" && job.ResultMediaID != job.MediaID {
		if !m.removeArtifactsIfUnowned(job) {
			return
		}
	}
	// The media row is committed, so any temporary from an earlier attempt is
	// provably redundant.
	m.sweepTemporaries(job)
	m.finishBySourceRemoval(job, store.JobComplete, "")
}

// runDiscard terminates a rejected or cancelled upload. Both reasons are
// decisions we honor, so no further guard is needed here: an upload whose bytes
// were simply never observable never reaches this state — it is closed out as
// 'unobservable' instead, which deletes nothing and stays reversible.
func (m *Manager) runDiscard(job *store.UploadJob) {
	if !m.deletionAllowed(job) {
		return
	}
	if !m.removeArtifactsIfUnowned(job) {
		return
	}
	m.sweepTemporaries(job)
	m.finishBySourceRemoval(job, store.JobDiscarded, job.TerminalReason)
}

// deletionAllowed re-checks storage health against the filesystem rather than
// reading the cached flag, because a volume that disappeared since the last
// reconcile tick must not produce a cascade of deletions. On refusal the lease
// is released so the job is retried in seconds instead of after the full lease
// duration.
func (m *Manager) deletionAllowed(job *store.UploadJob) bool {
	if err := m.health.Check(m.lifetime); err != nil {
		slog.Warn("refusing to delete while storage health is unproven",
			"operation", "cleanup", "upload_id", job.UploadID, "error", err)
		m.retryCleanup(job, err)
		return false
	}
	return true
}

// removeArtifactsIfUnowned deletes this job's own artifacts only after proving
// no committed media row claims its media id. GetByID reports absence as
// sql.ErrNoRows, and any other error means ownership is unknown — in which
// case nothing may be deleted. Returns false when the caller should stop.
func (m *Manager) removeArtifactsIfUnowned(job *store.UploadJob) bool {
	owner, err := m.store.GetByID(m.lifetime, job.MediaID, "")
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && owner == nil):
		m.removeArtifacts(job)
		return true
	case err != nil:
		m.retryCleanup(job, fmt.Errorf("ownership check failed: %w", err))
		return false
	default:
		// A committed row owns these artifacts; leave them alone.
		return true
	}
}

func (m *Manager) removeArtifacts(job *store.UploadJob) {
	if job.StoredFilename != "" {
		_ = os.Remove(m.processor.OriginalPath(job.StoredFilename))
	}
	_ = os.Remove(m.processor.ThumbnailPath(job.MediaID))
	_ = media.FsyncDir(m.processor.OriginalsDir())
}

// sweepTemporaries removes this job's leftover copies. A crash between the
// copy and the rename leaves a full-size file in originals/, and because the
// name is lease-scoped a retry writes a new one rather than reusing it. Left
// alone they accumulate at upload size and eventually push the free-space
// floor far enough to refuse new uploads.
func (m *Manager) sweepTemporaries(job *store.UploadJob) {
	matches, err := filepath.Glob(filepath.Join(m.processor.OriginalsDir(), ".ingest-"+job.MediaID+"-*-original.tmp"))
	if err != nil {
		return
	}
	for _, path := range matches {
		_ = os.Remove(path)
	}
	if len(matches) > 0 {
		_ = media.FsyncDir(m.processor.OriginalsDir())
	}
}

// finishBySourceRemoval deletes the tus source and commits the terminal state.
// Removal goes through tusd's own DELETE rather than unlinking its files, so
// tusd's sidecar and lock state stay consistent, and it is issued only for a
// path just observed to exist: an absent file may be a faulted mount rather
// than a deleted one.
func (m *Manager) finishBySourceRemoval(job *store.UploadJob, status store.JobStatus, reason string) {
	dataPath := m.DataPath(job.UploadID)
	switch _, err := os.Stat(dataPath); {
	case err == nil:
		if err := m.opts.Terminator.Terminate(m.lifetime, job.UploadID); err != nil {
			m.retryCleanup(job, fmt.Errorf("terminate source: %w", err))
			return
		}
	case !errors.Is(err, os.ErrNotExist):
		m.retryCleanup(job, fmt.Errorf("stat source: %w", err))
		return
	}

	if err := media.FsyncDir(m.opts.UploadDir); err != nil {
		m.retryCleanup(job, err)
		return
	}

	// Only ENOENT proves absence. Treating EIO or EACCES as "gone" would be the
	// same mistake that caused the incident.
	dataGone, err := pathIsAbsent(dataPath)
	if err != nil {
		m.retryCleanup(job, err)
		return
	}
	infoPath := m.InfoPath(job.UploadID)
	sidecarGone, err := pathIsAbsent(infoPath)
	if err != nil {
		m.retryCleanup(job, err)
		return
	}

	// tusd resolves an upload through its sidecar, so if the sidecar is gone it
	// answers 404 and never touches the data file. That is exactly the shape the
	// old ingest path left behind, and it is the shape this feature adopts, so
	// without this branch every recovered upload would spin in cleanup forever
	// and hold disk it no longer needs. Unlinking is safe here for the same
	// reason the orphan sidecar is: the media row is already committed, so this
	// is not the only copy — and tusd does not know the file exists.
	if !dataGone && sidecarGone {
		slog.Warn("removing tus data file that tusd can no longer address",
			"operation", "cleanup", "upload_id", job.UploadID)
		if err := os.Remove(dataPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.retryCleanup(job, fmt.Errorf("remove orphan data file: %w", err))
			return
		}
		dataGone = true
	}

	// A sidecar can also outlive its data file if tusd crashed between the two
	// unlinks. tusd will not remove it — its own GetUpload fails without the
	// data file — and neither will the janitor, so the app does it here.
	if dataGone && !sidecarGone {
		slog.Warn("removing tus sidecar that outlived its data file",
			"operation", "cleanup", "upload_id", job.UploadID)
		if err := os.Remove(infoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.retryCleanup(job, fmt.Errorf("remove orphan sidecar: %w", err))
			return
		}
	}

	if !dataGone {
		m.retryCleanup(job, errors.New("source still present after termination"))
		return
	}

	// One final verification of both paths across a directory fsync.
	if err := media.FsyncDir(m.opts.UploadDir); err != nil {
		m.retryCleanup(job, err)
		return
	}
	for _, path := range []string{dataPath, infoPath} {
		absent, err := pathIsAbsent(path)
		if err != nil || !absent {
			m.retryCleanup(job, fmt.Errorf("%s is not verifiably gone", path))
			return
		}
	}

	if err := m.store.FinishJob(m.lifetime, job.UploadID, job.LeaseToken, status, reason, store.NowMicros()); err != nil {
		slog.Error("failed to commit terminal state", "operation", "cleanup", "upload_id", job.UploadID, "error", err)
	}
}

// pathIsAbsent distinguishes "proven gone" from "could not tell". Only ENOENT
// counts as absence; every other error is ambiguity and must be retried.
func pathIsAbsent(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, os.ErrNotExist):
		return true, nil
	default:
		return false, err
	}
}

// retryCleanup keeps the job in its current stage. Demoting it to pending
// would re-run processing against a source cleanup may already have deleted.
func (m *Manager) retryCleanup(job *store.UploadJob, cause error) {
	next := store.NowMicros() + m.backoffFor(job.CleanupFailures).Microseconds()
	if err := m.store.ReleaseForRetry(m.lifetime, job.UploadID, job.LeaseToken, job.Status, next, "cleanup_failures", truncateError(cause)); err != nil {
		slog.Error("failed to schedule cleanup retry", "operation", "cleanup", "upload_id", job.UploadID, "error", err)
	}
}

func truncateError(err error) string {
	const maxLen = 500
	s := err.Error()
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}
```

- [ ] **Step 5: Add the two supporting helpers**

Append to `backend/internal/store/upload_jobs.go`:

```go
// RecordArtifactIdentity persists the deterministic artifact identity before
// the final original exists, so a crash can be recovered by name and hash
// instead of by re-copying or deleting.
func (s *Store) RecordArtifactIdentity(ctx context.Context, uploadID, leaseToken, storedFilename, mimeType, sha256Hex string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET stored_filename = ?, mime_type = ?, authoritative_sha256 = ?, updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		storedFilename, mimeType, sha256Hex, NowMicros(), uploadID, leaseToken)
	if err != nil {
		return fmt.Errorf("record artifact identity: %w", err)
	}
	return requireOneRow(res)
}

// RecordPrepared marks that a verified final original now exists. It is
// deliberately separate from RecordArtifactIdentity: prepared_at is a crash
// marker, and it would be worthless if it were set before the copy succeeded.
func (s *Store) RecordPrepared(ctx context.Context, uploadID, leaseToken string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs SET prepared_at = ?, updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		NowMicros(), NowMicros(), uploadID, leaseToken)
	if err != nil {
		return fmt.Errorf("record prepared: %w", err)
	}
	return requireOneRow(res)
}

// MediaIsReferencedByActiveJob is deliberately not added: the purge guard lives
// in PurgeTrashed's DELETE predicate, where it is evaluated in the same
// transaction, and a second out-of-transaction copy of the same rule would only
// invite someone to rely on it.

// ReviveForPublication is intentionally absent: nothing needs it. A job only
// reaches 'discarding' through a deterministic client rejection or an explicit
// user cancellation, and both of those are decisions we honor.

// MarkUnobservable closes out an admitted upload whose bytes were never
// observable — a create whose client vanished, or a partial the retention
// janitor removed. It goes straight to a terminal state because there is
// nothing to delete, and it carries a reason distinct from cancellation
// precisely so it stays reversible: if the paths were merely hidden by a
// faulted mount, reconciliation re-adopts them when they return.
func (s *Store) MarkUnobservable(ctx context.Context, uploadID string, now int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = 'discarded',
		       terminal_reason = 'unobservable',
		       terminal_at = ?,
		       lease_token = NULL,
		       lease_until = NULL,
		       updated_at = ?
		 WHERE upload_id = ?
		   AND status = 'uploading'
		   AND source_completed_at IS NULL`, now, now, uploadID)
	if err != nil {
		return fmt.Errorf("mark upload unobservable: %w", err)
	}
	return requireOneRow(res)
}

// DeleteUploadJob removes a terminal row so its upload id can be adopted
// afresh. Only used when bytes reappear for an 'unobservable' closure.
func (s *Store) DeleteUploadJob(ctx context.Context, uploadID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM upload_jobs WHERE upload_id = ? AND status IN ('complete', 'discarded')`, uploadID)
	if err != nil {
		return fmt.Errorf("delete terminal upload job: %w", err)
	}
	return nil
}

// ReopenTerminal returns a finished job to a working stage because files it
// had verified as gone have reappeared. The ordinary worker path then completes
// the removal it could not perform while the volume was unavailable.
func (s *Store) ReopenTerminal(ctx context.Context, uploadID string, status JobStatus, now int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = ?,
		       terminal_at = NULL,
		       next_attempt_at = ?,
		       lease_token = NULL,
		       lease_until = NULL,
		       updated_at = ?
		 WHERE upload_id = ? AND status IN ('complete', 'discarded')`,
		status, now, now, uploadID)
	if err != nil {
		return fmt.Errorf("reopen terminal upload job: %w", err)
	}
	return requireOneRow(res)
}
```

In `backend/internal/media/processor.go`, expose the two pieces the manager now needs, without changing `Process`:

```go
// ExtensionForMIME picks the stored file's extension from sniffed content,
// falling back to the client's filename only for unmapped types.
func ExtensionForMIME(mimeType, originalFilename string) string {
	if ext := mimeToExt[mimeType]; ext != "" {
		return ext
	}
	return filepath.Ext(originalFilename)
}

// Derive generates thumbnails and metadata for an already-stored original.
// Every failure here is best effort: the item publishes regardless.
func (p *Processor) Derive(ctx context.Context, finalPath, id string, kind models.MediaKind) *Result {
	result := &Result{ID: id}
	switch kind {
	case models.KindImage:
		p.processImage(ctx, finalPath, id, result)
	case models.KindVideo:
		p.processVideo(ctx, finalPath, id, result)
	}
	return result
}
```

- [ ] **Step 6: Stop purge from deleting a row an in-flight job depends on**

A duplicate upload validates the authoritative original, records it as its `result_media_id`, and only then deletes its own source and prepared copy. If that authoritative row were purged in between, both copies would be gone. Admin bulk-purge and the retention sweep can both do exactly that today.

The guard belongs in the delete predicate rather than in a precheck, so it is evaluated in the same transaction that removes the row. In `backend/internal/store/purge.go`, extend the `DELETE` inside `PurgeTrashed`:

```go
		result, err := tx.ExecContext(ctx, `DELETE FROM media_items
			WHERE id = ? AND stored_filename = ? AND status = 'trashed'
			  AND NOT EXISTS (
			      SELECT 1 FROM upload_jobs
			       WHERE result_media_id = media_items.id
			         AND status NOT IN ('complete', 'discarded')
			  )`, item.ID, item.StoredFilename)
```

No other change is needed: `PurgeTrashed` already returns only the IDs it actually deleted, and `purgeMedia` already calls `stage.Restore()` for every staged item missing from that set, so a deferred row keeps its files.

Also add the storage health check `purgeMedia` currently lacks, immediately before the staging loop, so a purge cannot commit a row deletion while the media volume is unproven — `StageForPurge`'s `moveIfExists` treats a missing original as success, which would otherwise orphan the real file when the mount returns:

```go
	if err := s.ingest.Health().Check(ctx); err != nil {
		return nil, fmt.Errorf("refusing to purge while storage health is unproven: %w", err)
	}
```

Add a test in `backend/internal/httpapi/purge_test.go` proving a purge is skipped while a non-terminal job references the id, that the item's files are restored rather than lost, and that the purge succeeds once that job reaches `complete`.

- [ ] **Step 7: Delete the source-consuming ingest path**

`Processor.Process` is now unreachable in production — its only caller was `handlePostFinishHook`, deleted in Task 8 — and it is the function that caused the incident: it calls `moveFile(tempPath, finalPath)`, which consumes the guest's only copy. Leaving a tested, source-destroying entry point in `media` invites someone to reuse it. Delete, in `backend/internal/media/processor.go`:

- `func (p *Processor) Process(...)`
- `func (p *Processor) RemoveMedia(...)`
- `func moveFile(...)`

and in `backend/internal/httpapi/tus_hooks.go`, `func actorLabel(...)` — its callers went with the same handler; `recordUploadAudit` in the ingest package replaces it.

In `backend/internal/media/processor_test.go`, delete every test that calls a removed symbol — all six, not just the obvious three:

- `TestProcessor_ProcessImage`
- `TestProcessor_ProcessAVIF`
- `TestProcessor_ProcessAVIF_RealFixture`
- `TestProcessor_RejectsDisallowedType`
- `TestProcessor_RejectsUnknownContent`
- `TestMoveFile_CrossDeviceFallback`

Keep `TestMimeToExt_AVIF`. Clean up any imports those deletions orphan.

The rejection coverage from the fourth and fifth is genuinely lost, so add the equivalent case to `process_test.go`, where it now belongs:

```go
func TestUnsupportedTypeIsDeterministicallyRejected(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	// Bytes no signature matches: media.Sniff returns *ErrUnsupportedType,
	// which must be a terminal client rejection rather than an endless retry.
	seedCompleteUpload(t, m, st, "u1", []byte("not a media file at all"))
	_ = m.EnsureDurable(context.Background(), "u1")

	drainQueue(t, m)

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.TerminalReason != "unsupported_type" {
		t.Errorf("terminal_reason = %q, want unsupported_type", job.TerminalReason)
	}
}
```

Verify with `go vet ./...` rather than `go build ./...` — `go build` does not type-check `_test.go` files, so it cannot catch a missed test deletion.

- [ ] **Step 8: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 go test ./internal/ingest/ ./internal/store/ ./internal/httpapi/ -race -v`
Expected: PASS. `TestTransientFailureNeverDeletesTheSource` is the regression test for the production incident — if it fails, stop and fix before continuing.

- [ ] **Step 9: Commit**

```bash
git add backend/internal/ingest/ backend/internal/store/ backend/internal/media/ backend/internal/httpapi/
git commit -m "feat: publish uploads transactionally and delete sources only after commit"
```

Stage `backend/internal/media/` as a directory, not just `processor.go`: this task also deletes six tests from `processor_test.go`, and committing the source deletion without the test deletion produces a branch that fails `go test ./...` even though the working tree passes.

---

### Task 12: Recovery reconciler and readiness

**Files:**
- Create: `backend/internal/ingest/recover.go`
- Create: `backend/internal/ingest/recover_test.go`
- Modify: `backend/internal/ingest/manager.go` (replace the `startupRecovery` and `reconcileOnce` stubs)
- Modify: `backend/internal/httpapi/server.go` (add `/readyz`)

**Interfaces:**
- Consumes: `tussidecar.Parse`, `store.RequeueStartup`, `store.CreateUploadingJob`, `Manager.EnsureDurable`.
- Produces: `func (m *Manager) startupRecovery()`, `func (m *Manager) reconcileOnce()`, `func (m *Manager) sweepCancelled(idleFor time.Duration)`, `func (s *Store) ClaimCancelledForDiscard(...) (int64, error)`, `func (s *Store) ListStaleUploading(ctx context.Context, idleBefore int64, limit int) ([]string, error)`, `func (s *Store) MarkUnobservable(ctx context.Context, uploadID string, now int64) error`, `func (s *Store) DeleteUploadJob(ctx context.Context, uploadID string) error`, and `func (s *Server) handleReady(w http.ResponseWriter, r *http.Request)`.

- [ ] **Step 1: Write the failing test**

Create `backend/internal/ingest/recover_test.go`:

```go
package ingest

import (
	"context"
	"os"
	"testing"

	"event-gallery/backend/internal/store"
)

func TestAdoptsCompletedUploadWithNoRow(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	// An upload that completed under the old code, or whose hook was lost.
	payload := jpegFixture(t)
	if err := os.WriteFile(m.DataPath("legacy1"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	writeSidecar(t, m, "legacy1", int64(len(payload)))

	m.reconcileOnce()

	job, err := st.GetUploadJob(context.Background(), "legacy1")
	if err != nil || job == nil {
		t.Fatalf("completed upload must be adopted: %+v %v", job, err)
	}
	if job.Status != store.JobPending {
		t.Errorf("status = %q, want pending", job.Status)
	}
}

func TestPartialUploadIsLeftAlone(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	if err := os.WriteFile(m.DataPath("partial"), []byte("half"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeSidecar(t, m, "partial", 999)

	m.reconcileOnce()

	if job, _ := st.GetUploadJob(context.Background(), "partial"); job != nil {
		t.Errorf("a partial upload must not be adopted as complete: %+v", job)
	}
	if _, err := os.Stat(m.DataPath("partial")); err != nil {
		t.Error("a partial upload must not be deleted by reconciliation")
	}
}

func TestPromotesCompleteUploadStuckInUploading(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	// A crash between pre-create and the durability commit, or a client that
	// gave up after a 503. The bytes are complete and the row exists, so
	// nothing else in the system would ever look at it again.
	payload := jpegFixture(t)
	job := &store.UploadJob{
		UploadID:         "stuck",
		MediaID:          "media-stuck",
		OriginalFilename: "a.jpg",
		ExpectedSize:     int64(len(payload)),
	}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(m.DataPath("stuck"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	writeSidecar(t, m, "stuck", int64(len(payload)))

	m.reconcileOnce()

	got, _ := st.GetUploadJob(context.Background(), "stuck")
	if got.Status != store.JobPending {
		t.Errorf("status = %q, want pending: a complete source must never be stranded", got.Status)
	}
}

func TestCancelledUploadIsSweptIntoDiscard(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	seedCompleteUpload(t, m, st, "u1", []byte("partial"))
	if err := st.RequestCancellation(context.Background(), "u1", store.NowMicros()); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Sweep with a zero idle window; the production reconciler waits one
	// interval so it cannot race an upload that is still being written.
	m.sweepCancelled(0)

	got, _ := st.GetUploadJob(context.Background(), "u1")
	if got.Status != store.JobDiscarding {
		t.Errorf("status = %q, want discarding", got.Status)
	}
}

func TestAdoptsCompletedUploadWithNoSidecar(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	// Exactly the residue the old ingest path left behind: it deleted the
	// sidecar before its failure branch returned, so the complete data file
	// survived alone.
	payload := jpegFixture(t)
	if err := os.WriteFile(m.DataPath("orphan"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}

	m.reconcileOnce()

	job, err := st.GetUploadJob(context.Background(), "orphan")
	if err != nil || job == nil {
		t.Fatalf("a sidecar-less complete upload must still be adopted: %+v %v", job, err)
	}
	if job.Status != store.JobPending {
		t.Errorf("status = %q, want pending", job.Status)
	}
}

func TestUntrustworthySidecarIsLeftAlone(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	payload := jpegFixture(t)
	if err := os.WriteFile(m.DataPath("mixed"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	// A sidecar naming a different upload must never supply this file's
	// metadata: a checksum from the wrong upload would turn a good file into a
	// deterministic rejection and get it discarded.
	writeSidecarFor(t, m, "mixed", "someone-else", int64(len(payload)))

	m.reconcileOnce()

	if job, _ := st.GetUploadJob(context.Background(), "mixed"); job != nil {
		t.Errorf("must not adopt from an untrusted sidecar: %+v", job)
	}
	if _, err := os.Stat(m.DataPath("mixed")); err != nil {
		t.Error("the data file must be left untouched")
	}
}

func TestAbsentSourceNeverTerminalizesADurableJob(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	seedCompleteUpload(t, m, st, "u1", jpegFixture(t))
	_ = m.EnsureDurable(context.Background(), "u1")

	// Simulate a faulted upload mount: everything vanishes.
	_ = os.Remove(m.DataPath("u1"))
	_ = os.Remove(m.InfoPath("u1"))

	m.reconcileOnce()
	m.reconcileOnce()

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobPending {
		t.Errorf("status = %q, want pending: absence must never terminalize durable work", job.Status)
	}
}
```

Add `writeSidecar(t, m, id, size)` to `fixture_test.go`. It must write the full shape `tussidecar.Parse` requires, matching the existing janitor fixture in `storage_cleanup_test.go`:

```json
{"ID":"<id>","Size":<size>,"SizeIsDeferred":false,"MetaData":{"filename":"a.jpg"},
 "Storage":{"Type":"filestore","Path":"<m.DataPath(id)>","InfoPath":"<m.InfoPath(id)>"}}
```

`Storage.Type` is load-bearing: without it `Parse` fails, `trustedSidecar` reports "absent", and `reconcileOne` falls into the adopt-at-observed-size branch — which would invert `TestPartialUploadIsLeftAlone` and `TestUntrustworthySidecarIsLeftAlone` into passing for the wrong reason. Add `writeSidecarFor(t, m, fileID, declaredID, size)` alongside it, identical except that the `ID` field names `declaredID` while the file is written at `m.InfoPath(fileID)`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/ingest/ -run 'Adopt|Partial|Absent' -v`
Expected: FAIL — reconciliation is a no-op stub.

- [ ] **Step 3: Implement recovery**

First append the sweep query to `backend/internal/store/upload_jobs.go`:

```go
// ClaimCancelledForDiscard moves cancelled uploads that have been idle since
// idleBefore into discard, under a lease, so the ordinary worker path reclaims
// their bytes. Uploads that reached pending are untouched: a durable
// completion is never reversed.
func (s *Store) ClaimCancelledForDiscard(ctx context.Context, idleBefore, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = 'discarding',
		       terminal_reason = CASE WHEN terminal_reason = '' THEN 'cancelled' ELSE terminal_reason END,
		       next_attempt_at = ?,
		       lease_token = NULL,
		       lease_until = NULL,
		       updated_at = ?
		 WHERE status = 'uploading'
		   AND cancellation_requested_at IS NOT NULL
		   AND updated_at <= ?`, now, now, idleBefore)
	if err != nil {
		return 0, fmt.Errorf("sweep cancelled uploads: %w", err)
	}
	return res.RowsAffected()
}

// ListStaleUploading returns admitted uploads that never reached the
// durability barrier and have been quiet since idleBefore. Rows with
// source_completed_at are excluded on purpose: those are durable, and their
// files being unreadable is a mount problem, not a reason to give up on them.
func (s *Store) ListStaleUploading(ctx context.Context, idleBefore int64, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT upload_id FROM upload_jobs
		 WHERE status = 'uploading'
		   AND source_completed_at IS NULL
		   AND cancellation_requested_at IS NULL
		   AND updated_at <= ?
		 LIMIT ?`, idleBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("list stale uploads: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stale upload: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
```

Then create `backend/internal/ingest/recover.go`:

```go
package ingest

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"event-gallery/backend/internal/store"
	"event-gallery/backend/internal/tussidecar"
)

// startupRecovery makes interrupted work claimable and adopts any completed
// upload the previous process never queued, then opens the readiness gate.
// Readiness is withheld if the inventory could not be read at all: admitting
// uploads while pre-upgrade completions are still unknown would be worse than
// a few extra seconds of backpressure. The reconciler retries and opens the
// gate on the first pass that succeeds.
func (m *Manager) startupRecovery() {
	if _, err := m.store.RequeueStartup(m.lifetime, store.NowMicros()); err != nil {
		// A database failure here means nothing about the queue can be trusted
		// yet, so readiness stays closed and the reconciler retries.
		slog.Error("failed to requeue interrupted jobs; ingest stays not ready", "operation", "startup_recovery", "error", err)
		return
	}
	if err := m.health.Check(m.lifetime); err != nil {
		slog.Error("storage health check failed at startup", "operation", "startup_recovery", "error", err)
	}
	if err := m.reconcileOnce(); err != nil {
		slog.Error("startup inventory failed; ingest stays not ready", "operation", "startup_recovery", "error", err)
		return
	}
	if m.ready.CompareAndSwap(false, true) {
		slog.Info("ingest ready", "operation", "startup_recovery")
	}
	m.Wake()
}

// reconcileOnce repairs what it can prove. It never deletes a file directly:
// the only outcomes are adoption, promotion, reopening a removal that could not
// finish, and leaving things exactly as they are. It reports whether the
// inventory pass actually completed, which is what gates readiness.
func (m *Manager) reconcileOnce() error {
	m.sweepCancelled(m.opts.ReconcileInterval)
	m.resolveRowsWithoutFiles()

	entries, err := os.ReadDir(m.opts.UploadDir)
	if err != nil {
		slog.Warn("cannot read upload directory", "operation", "reconcile", "error", err)
		return err
	}

	// Both sidecars and bare data files are inventoried. The old code ran
	// `defer cleanupTusInfoFile(...)` before its failure branch, so every
	// upload it failed to ingest left a complete data file with no sidecar
	// at all. Scanning only `.info` entries would strand exactly the files
	// this feature exists to rescue.
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		uploadID := strings.TrimSuffix(entry.Name(), ".info")
		if !isSafeUploadID(uploadID) {
			continue // skips tusd lock files and our own dot-prefixed temporaries
		}
		if _, done := seen[uploadID]; done {
			continue
		}
		seen[uploadID] = struct{}{}

		if err := m.reconcileOne(uploadID); err != nil {
			slog.Warn("reconcile failed", "operation", "reconcile", "upload_id", uploadID, "error", err)
		}
	}
	return nil
}

// sweepCancelled moves cancelled uploads that have gone quiet into discard,
// where the ordinary worker path reclaims their bytes. idleFor is a parameter
// so tests can sweep immediately.
func (m *Manager) sweepCancelled(idleFor time.Duration) {
	idleBefore := store.NowMicros() - idleFor.Microseconds()
	swept, err := m.store.ClaimCancelledForDiscard(m.lifetime, idleBefore, store.NowMicros())
	if err != nil {
		slog.Warn("cancellation sweep failed", "operation", "reconcile", "error", err)
		return
	}
	if swept > 0 {
		m.Wake()
	}
}

// resolveRowsWithoutFiles closes out admitted uploads that never produced any
// bytes — a create whose client vanished, or a partial the retention janitor
// removed. Rows that reached the durability barrier are deliberately excluded:
// their absence may be a faulted mount, and they retry indefinitely instead.
//
// The closure is marked 'unobservable' rather than cancelled. That distinction
// is what makes it safe: it deletes nothing, and if the paths were merely
// hidden, reconcileOne re-adopts them when they come back.
func (m *Manager) resolveRowsWithoutFiles() {
	idleBefore := store.NowMicros() - m.opts.ReconcileInterval.Microseconds()
	ids, err := m.store.ListStaleUploading(m.lifetime, idleBefore, 200)
	if err != nil {
		slog.Warn("could not list stale uploads", "operation", "reconcile", "error", err)
		return
	}
	for _, id := range ids {
		if m.UploadPathsExist(id) {
			continue
		}
		if err := m.store.MarkUnobservable(m.lifetime, id, store.NowMicros()); err != nil && !errors.Is(err, store.ErrNotClaimed) {
			slog.Warn("could not resolve upload with no bytes", "operation", "reconcile", "upload_id", id, "error", err)
		}
	}
}

func (m *Manager) reconcileOne(uploadID string) error {
	job, err := m.store.GetUploadJob(m.lifetime, uploadID)
	if err != nil {
		return err
	}
	if job != nil {
		switch {
		case job.Status == store.JobUploading:
			// May still need the durability barrier; handled below.
		case job.Status == store.JobDiscarded && job.TerminalReason == unobservableReason:
			// The bytes were hidden, not gone — a faulted mount that has now
			// returned. Closing the row out deleted nothing, so drop it and
			// adopt the upload afresh rather than stranding a complete file.
			slog.Info("bytes returned for an unobservable upload; re-adopting",
				"operation", "reconcile", "upload_id", uploadID)
			if err := m.store.DeleteUploadJob(m.lifetime, uploadID); err != nil {
				return err
			}
			job = nil
		case job.Status == store.JobComplete || job.Status == store.JobDiscarded:
			// Files this job verified as gone have reappeared, so the volume
			// came back after it was closed out. Finish the removal rather
			// than leaving them: bytes the guest cancelled must not survive,
			// and once the terminal row expires a rowless complete source
			// would be adopted and republished.
			target := store.JobCleanup
			if job.Status == store.JobDiscarded {
				target = store.JobDiscarding
			}
			slog.Warn("tus files reappeared for a finished upload; reopening to remove them",
				"operation", "reconcile", "upload_id", uploadID, "status", job.Status, "reason", job.TerminalReason)
			if err := m.store.ReopenTerminal(m.lifetime, uploadID, target, store.NowMicros()); err != nil {
				return err
			}
			m.Wake()
			return nil
		default:
			return nil // pending or later: the workers own it
		}
	}

	stat, err := os.Stat(m.DataPath(uploadID))
	if err != nil || !stat.Mode().IsRegular() {
		return nil // nothing observable; absence is never actionable here
	}

	if job != nil {
		// Every upload currently being transferred has an 'uploading' row and
		// a partial file, so this must not fire for them: it would burn a
		// durability slot every tick and inject 503s into live uploads that
		// join the doomed operation.
		if stat.Size() != job.ExpectedSize {
			return nil
		}
		// Complete but never past the barrier: a crash inside the pre-finish
		// window, or a client that stopped retrying after a 503.
		return m.EnsureDurable(m.lifetime, uploadID)
	}

	info, identityMismatch := m.trustedSidecar(uploadID)
	switch {
	case info != nil:
		if stat.Size() != info.Size {
			return nil // still uploading; the incomplete-retention policy owns it
		}
		return m.adopt(uploadID, info.Size, info.MetaData)
	case identityMismatch:
		// A sidecar naming a different upload must not supply this file's
		// metadata: a checksum from the wrong upload would turn a good file
		// into a deterministic rejection and get it discarded.
		slog.Warn("tus sidecar does not describe its own upload; leaving both files untouched",
			"operation", "reconcile", "upload_id", uploadID)
		return nil
	default:
		// No sidecar, or one that cannot be parsed at all. tusd cannot resume
		// such an upload, so the file is residue, most often from the old
		// ingest path, which deleted the sidecar before its failure branch
		// returned. Adopt at the observed size; with no metadata there is no
		// declared checksum to mismatch.
		return m.adopt(uploadID, stat.Size(), nil)
	}
}

// unobservableReason marks a row closed out because its bytes were never
// visible. Unlike a cancellation it deletes nothing, so it stays reversible.
const unobservableReason = "unobservable"

// trustedSidecar returns the sidecar only when it demonstrably describes this
// upload. The second result flags the one case where the data file must be
// left alone: a parseable sidecar naming a different upload. A missing or
// unreadable sidecar is reported as absent, because tusd cannot resume such an
// upload and stranding the bytes would be worse than adopting them.
func (m *Manager) trustedSidecar(uploadID string) (info *tussidecar.Info, identityMismatch bool) {
	infoPath := m.InfoPath(uploadID)
	parsed, err := tussidecar.Parse(infoPath)
	if err != nil {
		return nil, false
	}
	if parsed.ID != uploadID ||
		filepath.Clean(parsed.StoragePath) != filepath.Clean(m.DataPath(uploadID)) ||
		(parsed.StorageInfoPath != "" && filepath.Clean(parsed.StorageInfoPath) != filepath.Clean(infoPath)) {
		return nil, true
	}
	return parsed, false
}

func (m *Manager) adopt(uploadID string, size int64, metadata map[string]string) error {
	job := &store.UploadJob{
		UploadID:         uploadID,
		MediaID:          uuid.NewString(),
		OriginalFilename: sanitizeAdoptedFilename(metadata["filename"]),
		ExpectedSize:     size,
		DeclaredSHA256:   strings.ToLower(strings.TrimSpace(metadata["sha256"])),
		GuestName:        strings.TrimSpace(metadata["guestName"]),
	}
	if err := m.store.CreateUploadingJob(m.lifetime, job); err != nil {
		return err
	}
	// Promote through the same barrier the live path uses, so an adopted
	// upload is fsynced and size-checked exactly like any other.
	if err := m.EnsureDurable(m.lifetime, uploadID); err != nil {
		return err
	}
	slog.Info("adopted completed upload", "operation", "reconcile", "upload_id", uploadID, "media_id", job.MediaID)
	return nil
}

func sanitizeAdoptedFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "recovered-upload"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func isSafeUploadID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		isAlnum := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !isAlnum && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
```

Delete the three stubs from `manager.go`, keeping `claimAndRunOnce` from Task 11.

- [ ] **Step 4: Add the readiness endpoint**

In `server.go`, register the route next to `/healthz`:

```go
	r.Get("/healthz", s.handleHealth)
	r.Get("/readyz", s.handleReady)
```

and add the handler:

```go
// handleReady reports ingest readiness. /healthz stays a shallow liveness
// check so the gallery and the tunnel start promptly; only upload routes wait
// for the startup inventory.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.ingest == nil || !s.ingest.Ready() {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "ingest is still recovering queued uploads")
		return
	}
	if !s.ingest.Health().Healthy() {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "media storage is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
```

Use whatever JSON helper `respond.go` already provides instead of `writeJSON` if the name differs.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 go test ./internal/ingest/ ./internal/httpapi/ -race -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/ingest/ backend/internal/store/upload_jobs.go backend/internal/httpapi/server.go
git commit -m "feat: adopt orphaned completed uploads and gate upload routes on readiness"
```

---

### Task 13: Upload status API

**Files:**
- Create: `backend/internal/httpapi/uploads_status.go`
- Create: `backend/internal/httpapi/uploads_status_test.go`
- Modify: `backend/internal/httpapi/server.go`
- Modify: `backend/internal/httpapi/public_test.go`
- Modify: `backend/internal/httpapi/moderation_test.go`

**Interfaces:**
- Consumes: `store.GetUploadJob`.
- Produces: `POST /api/uploads/status` accepting `{"uploadIds":["..."]}` (max 100) and returning `{"results":{"<id>":{"state":"...","mediaId":"..."}}}`.

- [ ] **Step 1: Write the failing test**

Create `backend/internal/httpapi/uploads_status_test.go`:

```go
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"event-gallery/backend/internal/store"
)

// postStatus posts a batch and returns the decoded results map.
func postStatus(t *testing.T, h *testHarness, ids []string) map[string]uploadStatusEntry {
	t.Helper()
	rec := postStatusRaw(t, h, ids)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp uploadStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Results
}

func postStatusRaw(t *testing.T, h *testHarness, ids []string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(uploadStatusRequest{UploadIDs: ids})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return doRequest(h, http.MethodPost, "/api/uploads/status", body)
}

func TestUploadStatusMapsQueueStatesToPublicStates(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	cases := []struct {
		name  string
		setup func(t *testing.T, uploadID string)
		want  string
	}{
		{"uploading", func(t *testing.T, id string) {
			seedUploadingJob(t, h, id, 10)
		}, "uploading"},
		{"pending", func(t *testing.T, id string) {
			seedUploadingJob(t, h, id, 10)
			if err := h.store.PromoteToPending(ctx, id, store.NowMicros()); err != nil {
				t.Fatalf("promote: %v", err)
			}
		}, "processing"},
		{"cancelled", func(t *testing.T, id string) {
			seedUploadingJob(t, h, id, 10)
			if err := h.store.RequestCancellation(ctx, id, store.NowMicros()); err != nil {
				t.Fatalf("cancel: %v", err)
			}
		}, "cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "u-" + tc.name
			tc.setup(t, id)
			got := postStatus(t, h, []string{id})
			if got[id].State != tc.want {
				t.Errorf("state = %q, want %q", got[id].State, tc.want)
			}
		})
	}
}

func TestUploadStatusRejectsOversizedBatch(t *testing.T) {
	h := newTestHarness(t)
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = "u"
	}
	rec := postStatusRaw(t, h, ids)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestUploadStatusReportsUnknownPerID(t *testing.T) {
	h := newTestHarness(t)
	got := postStatus(t, h, []string{"never-existed"})
	if got["never-existed"].State != "unknown" {
		t.Errorf("state = %q, want unknown", got["never-existed"].State)
	}
}
```

`seedUploadingJob` is the helper added in Task 10. The published and duplicate states are covered end to end by Task 11's ingest tests, which exercise the real publication transaction rather than a hand-built row.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/httpapi/ -run UploadStatus -v`
Expected: FAIL — route not registered.

- [ ] **Step 3: Implement the endpoint**

Create `backend/internal/httpapi/uploads_status.go`:

```go
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"event-gallery/backend/internal/store"
)

const maxStatusBatch = 100

type uploadStatusRequest struct {
	UploadIDs []string `json:"uploadIds"`
}

type uploadStatusEntry struct {
	State   string `json:"state"`
	MediaID string `json:"mediaId,omitempty"`
}

type uploadStatusResponse struct {
	Results map[string]uploadStatusEntry `json:"results"`
}

func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	var req uploadStatusRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.UploadIDs) == 0 || len(req.UploadIDs) > maxStatusBatch {
		writeError(w, http.StatusBadRequest, "uploadIds must contain between 1 and 100 entries")
		return
	}

	results := make(map[string]uploadStatusEntry, len(req.UploadIDs))
	recovering := s.ingest != nil && !s.ingest.Ready()
	for _, id := range req.UploadIDs {
		if _, seen := results[id]; seen {
			continue
		}
		job, err := s.store.GetUploadJob(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read upload state")
			return
		}
		// During the startup inventory an unknown id may simply not have been
		// adopted yet, so say so rather than declaring it lost.
		if job == nil && recovering {
			results[id] = uploadStatusEntry{State: "recovering"}
			continue
		}
		results[id] = s.publicUploadState(r.Context(), job)
	}
	writeJSON(w, http.StatusOK, uploadStatusResponse{Results: results})
}

// publicUploadState maps internal queue states onto the small vocabulary the
// browser understands, without leaking queue mechanics. Core failures report
// "processing" because they retry indefinitely and will eventually publish.
func (s *Server) publicUploadState(ctx context.Context, job *store.UploadJob) uploadStatusEntry {
	if job == nil {
		return uploadStatusEntry{State: "unknown"}
	}
	switch job.Status {
	case store.JobUploading:
		if job.CancellationRequestedAt != nil {
			return uploadStatusEntry{State: "cancelled"}
		}
		return uploadStatusEntry{State: "uploading"}
	case store.JobPending, store.JobProcessing:
		return uploadStatusEntry{State: "processing"}
	case store.JobCleanup, store.JobComplete:
		// Publication already committed; source cleanup must not delay it.
		state := "published"
		if job.ResultMediaID != "" && job.ResultMediaID != job.MediaID {
			state = "duplicate"
		}
		return uploadStatusEntry{State: state, MediaID: s.visibleMediaID(ctx, job.ResultMediaID)}
	case store.JobDiscarding, store.JobDiscarded:
		switch job.TerminalReason {
		case "unsupported_type", "checksum_mismatch":
			return uploadStatusEntry{State: "failed"}
		default:
			return uploadStatusEntry{State: "cancelled"}
		}
	default:
		return uploadStatusEntry{State: "processing"}
	}
}

// visibleMediaID returns the id only when the item is publicly viewable.
// Awaiting approval or trashed media must not be addressable through an
// upload receipt.
func (s *Server) visibleMediaID(ctx context.Context, mediaID string) string {
	if mediaID == "" {
		return ""
	}
	if _, err := s.store.GetVisibleByID(ctx, mediaID, ""); err != nil {
		return ""
	}
	return mediaID
}
```

Register it as a sibling of the public group, not inside it. chi's `Mux.With` on an inline group copies the parent's middleware chain before appending, so registering inside `pub` would stack `publicRateLimit` *and* the new limiter — status polling would still drain the gallery bucket, which is the exact outcome this is meant to prevent:

```go
		api.With(s.uploadStatusRateLimit).Post("/uploads/status", s.handleUploadStatus)
```

Add `uploadStatusLimiter` to `Server`, built from `cfg.UploadStatusRateLimitPerMinute` exactly like `publicLimiter`, and a `uploadStatusRateLimit` middleware mirroring `publicRateLimit`. Add `s.uploadStatusLimiter.StartCleanup(10*time.Minute, stop)` alongside the other limiters in `StartCleanupLoops`, or its per-IP state is never evicted.

Finally, make `/api/uploads/check` a shim so pre-upgrade browser tabs stop deleting their own files:

```go
// handleUploadCheck is retained only for tabs loaded before this deploy. The
// old client removed the local file on a duplicate verdict, so this must
// always answer false and let server-side SHA-256 resolution converge instead.
func (s *Server) handleUploadCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"duplicate": false})
}
```

- [ ] **Step 4: Retire the duplicate-preflight tests**

Making `/api/uploads/check` a constant shim invalidates the tests that assert it performs a real lookup. In `backend/internal/httpapi/public_test.go`, delete `TestHandleUploadCheck_Duplicate`, and change `TestHandleUploadCheck_InvalidSHA` and `TestHandleUploadCheck_SizeTooLarge` to expect `duplicate:false` with HTTP 200 rather than a validation error — the shim no longer validates anything. Keep `TestHandleUploadCheck_NotDuplicate`, which still describes the new behavior. In `backend/internal/httpapi/moderation_test.go`, the assertion that `duplicate.Duplicate` is true must be removed for the same reason.

This is intentional: the endpoint's old behavior is exactly what made a pre-upgrade tab delete the guest's local file, so its contract is deliberately narrowed rather than preserved.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 go test ./internal/httpapi/ -race -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/httpapi/
git commit -m "feat: expose batched upload status and neutralize the legacy check endpoint"
```

---

### Task 14: Wiring, deployment, and documentation

**Files:**
- Modify: `backend/cmd/server/main.go`
- Modify: `deploy/tusd-entrypoint.sh`
- Modify: `docker-compose.yml`
- Modify: `ARCHITECTURE.md`

**Interfaces:**
- Consumes: everything above.
- Produces: a running system where tusd calls four hooks and the app owns the queue.

- [ ] **Step 1: Wire the manager into main**

In `backend/cmd/server/main.go`, after the store, processor, and server are built. The server is constructed first because it is also the `SourceTerminator`, and the manager is attached and the listener started **before** `Start()`, because `Start()` runs the whole startup inventory synchronously:

```go
	ingestManager := ingest.New(st, processor, ingest.Options{
		Workers:           cfg.MediaProcessingWorkers,
		DurabilityWorkers: cfg.UploadDurabilityWorkers,
		ProcessingTimeout: cfg.MediaProcessingTimeout,
		MaxBackoff:        cfg.UploadRetryMaxBackoff,
		ReconcileInterval: cfg.IngestReconcileInterval,
		JobRetention:      cfg.UploadJobRetention,
		UploadDir:         cfg.TusUploadDir,
		MinFreeBytes:      cfg.IngestMinFreeBytes,
		Terminator:        srv,
	})
	srv.SetIngest(ingestManager)
```

Then, **after** the existing `go httpServer.ListenAndServe()` line:

```go
	// Started only once the listener is up. The inventory fsyncs every
	// recovered source, and the first boot after this upgrade has the largest
	// backlog of them; blocking the listener would fail the container
	// healthcheck and keep the tunnel down. Upload routes answer 503 until
	// Ready() flips, which is the correct backpressure for that window.
	//
	// Called synchronously, not with `go`: Start takes its WaitGroup slot on
	// the calling goroutine, so a shutdown racing startup cannot see an empty
	// group, return, and close the database while recovery is still running.
	ingestManager.Start()
```

`ingestManager.Stop()` must run during graceful shutdown, after the HTTP server stops accepting requests and **before** the database is closed — `Stop` waits for in-flight durability commits, and closing the database first would fail them. With `defer sqlDB.Close()` registered earlier, a later `defer ingestManager.Stop()` gives exactly that order.

- [ ] **Step 2: Update the tusd entrypoint**

In `deploy/tusd-entrypoint.sh`, extend the hook list and set the derived timeouts. The existing `-hooks-http-forward-headers` line is unchanged:

```sh
  -hooks-enabled-events "pre-create,pre-finish,post-finish" \
  -hooks-http-retry 0 \
  -hooks-http-timeout 90s \
  -network-timeout 90s \
  -disable-concatenation \
```

`-hooks-http-retry 0` means one attempt with no retries. It is required: the default of 3 would re-invoke `pre-create` and mint duplicate job rows for a single upload.

- [ ] **Step 3: Publish the new settings**

In `docker-compose.yml`, add to the `app` service environment:

```yaml
      MEDIA_PROCESSING_WORKERS: "${MEDIA_PROCESSING_WORKERS:-2}"
      MEDIA_PROCESSING_TIMEOUT_MINUTES: "${MEDIA_PROCESSING_TIMEOUT_MINUTES:-60}"
      UPLOAD_DURABILITY_WAIT_SECONDS: "${UPLOAD_DURABILITY_WAIT_SECONDS:-75}"
      UPLOAD_DURABILITY_WORKERS: "${UPLOAD_DURABILITY_WORKERS:-2}"
      UPLOAD_RETRY_MAX_BACKOFF_MINUTES: "${UPLOAD_RETRY_MAX_BACKOFF_MINUTES:-15}"
      INGEST_RECONCILE_INTERVAL_SECONDS: "${INGEST_RECONCILE_INTERVAL_SECONDS:-15}"
      INGEST_MIN_FREE_BYTES: "${INGEST_MIN_FREE_BYTES:-}"
      UPLOAD_JOB_RETENTION_DAYS: "${UPLOAD_JOB_RETENTION_DAYS:-30}"
      UPLOAD_STATUS_RATE_LIMIT_PER_MINUTE: "${UPLOAD_STATUS_RATE_LIMIT_PER_MINUTE:-6000}"
```

- [ ] **Step 4: Correct the backup documentation**

In `ARCHITECTURE.md` at the repository root (there is no `docs/ARCHITECTURE.md`), replace the line stating that the tus incoming volume is transient resumability state whose exclusion from backup loses nothing:

```markdown
The tus upload volume is **not** transient. Once an upload completes, its
source file is the application's only copy until the media row is committed, so
a backup that omits this volume abandons queued work. Back up app data, media,
and tus uploads together from a stopped stack.

Rollback is not image-only. After migration 0004, a pre-0004 app must not run
against these volumes: its `post-finish` path does not understand durable jobs
and can delete their only source. Roll back by stopping the new containers,
restoring all three volumes from one pre-upgrade backup, and starting the
recorded old image pair.
```

- [ ] **Step 5: Verify the whole build**

Run:
```bash
docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25 sh -c "go build ./cmd/server && go vet ./... && go test ./... -race"
docker compose config >/dev/null
```
Expected: build succeeds, vet is silent, all tests pass, compose config parses.

- [ ] **Step 6: Commit**

```bash
git add backend/cmd/server/main.go deploy/tusd-entrypoint.sh docker-compose.yml ARCHITECTURE.md
git commit -m "feat: wire durable ingest into the app, tusd, and compose"
```

---

## Self-Review Notes

Checked against the spec before handing off:

- **Covered:** SQLite queue and migration (Task 1), configuration (Task 2), leased claims and retry scheduling (Tasks 3, 7), shared sidecar parsing (Task 4), fsync-ordered preparation that preserves the source (Task 5), storage health gate (Task 6), pre-create admission with backpressure envelopes (Task 8), blocking pre-finish durability barrier (Task 9), completion fence and cancellation (Task 10), deterministic preparation, transactional publication, duplicate validation, cleanup, and the purge guard (Task 11), recovery and readiness (Task 12), status API and legacy shim (Task 13), wiring and rollout (Task 14).
- **Deferred by design to plans 2 and 3:** the `heif-preview` helper, `has_preview` population, `GET /api/media/{id}/preview`, `PREVIEW_MAX_DIMENSION`, `HEIC_MAX_SOURCE_PIXELS`, `HEIC_CONVERSION_MAX_FAILURES`, the `conversion_failures` budget's consumer, `MEDIA_TOOL_MEMORY_BYTES` enforcement, the Uppy retry wrapper, status-driven gallery refresh, and the `tus_battle.py` completion oracle. The schema column and config values land here so plan 2 is purely additive.
- **Known follow-up inside this plan:** Task 11 leaves `conversion_failures` unused; that is intentional and plan 2 consumes it.
- **Naming consistency verified:** `EnsureDurable`, `PromoteToPending`, `ClaimNextJob`, `ReleaseForRetry`, `FinishJob`, `PublishMedia`, `RecordArtifactIdentity`, `RecordPrepared`, `MarkUnobservable`, `DeleteUploadJob`, `ClaimCancelledForDiscard`, `ListStaleUploading`, `PrepareOriginal`, `VerifyOriginal`, `Terminate`, and `DataPath`/`InfoPath` are used identically everywhere they appear.
- **Existing-store contracts honored:** `GetBySHA256`, `GetByID`, and `GetVisibleByID` all report absence as `sql.ErrNoRows`, and `media.Sniff` reports unrecognized content as `*media.ErrUnsupportedType`. Every call site in this plan handles those explicitly.

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-11-durable-ingest-queue.md`. Two execution options:

1. **Subagent-Driven (recommended)** — a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
