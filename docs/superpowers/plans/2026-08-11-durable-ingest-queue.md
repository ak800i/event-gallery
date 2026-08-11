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
  `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./... -race`
  If Go is available locally, run the equivalent from `backend/` directly. CI runs `go test ./... && go vet ./... && go build ./cmd/server`.
- Commit after every task. Use Conventional Commit prefixes (`feat:`, `fix:`, `test:`, `chore:`).

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
- `backend/internal/httpapi/tus_hooks.go` — rewrite `pre-create`, add blocking `pre-finish` and `pre-terminate`, demote `post-finish` to a wake signal.
- `backend/internal/httpapi/tus_proxy.go` — completion fence on HEAD/PATCH, DELETE as cancellation intent.
- `backend/internal/httpapi/server.go` — hold the manager, add `/readyz`, register the status route.
- `backend/internal/httpapi/storage_cleanup.go` — use the shared sidecar parser and the manager's claim API.
- `backend/cmd/server/main.go` — construct, start, and gracefully stop the manager.
- `deploy/tusd-entrypoint.sh` — new hooks, forwarded header, derived timeouts.
- `docker-compose.yml` — new environment variables.
- `docs/ARCHITECTURE.md` — the tus volume is no longer transient.

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
- Produces: on `config.Config` — `MediaProcessingWorkers int`, `MediaProcessingTimeout time.Duration`, `UploadDurabilityWait time.Duration`, `UploadDurabilityWorkers int`, `UploadRetryMaxBackoff time.Duration`, `IngestReconcileInterval time.Duration`, `IngestMinFreeBytes int64`, `UploadJobRetention time.Duration`, `UploadStatusRateLimitPerMinute int`, `ImageMaxSourcePixels int64`, `MediaToolMemoryBytes int64`, `MediaToolLogBytes int`.

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
	ImageMaxSourcePixels           int64
	MediaToolMemoryBytes           int64
	MediaToolLogBytes              int
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
	if cfg.ImageMaxSourcePixels, err = envInt64("IMAGE_MAX_SOURCE_PIXELS", 50_000_000); err != nil {
		return nil, err
	}
	if cfg.MediaToolMemoryBytes, err = envInt64("MEDIA_TOOL_MEMORY_BYTES", 1<<30); err != nil {
		return nil, err
	}
	if cfg.MediaToolLogBytes, err = envInt("MEDIA_TOOL_LOG_BYTES", 65536); err != nil {
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
  - `func (s *Store) ReleaseForRetry(ctx context.Context, uploadID, leaseToken string, nextAttemptAt int64, counter string, lastError string) error`
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
	if err := s.ReleaseForRetry(ctx, "u1", claimed.LeaseToken, next, "processing_failures", "disk hiccup"); err != nil {
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

// ReleaseForRetry returns a job to pending with capped backoff. counter must be
// one of the three failure column names so logs and budgets stay unambiguous.
func (s *Store) ReleaseForRetry(ctx context.Context, uploadID, leaseToken string, nextAttemptAt int64, counter, lastError string) error {
	switch counter {
	case "processing_failures", "conversion_failures", "cleanup_failures":
	default:
		return fmt.Errorf("unknown failure counter %q", counter)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = 'pending',
		       lease_token = NULL,
		       lease_until = NULL,
		       next_attempt_at = ?,
		       `+counter+` = `+counter+` + 1,
		       last_error = ?,
		       updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		nextAttemptAt, lastError, NowMicros(), uploadID, leaseToken,
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

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/store/ -race -v`
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
- Produces: `type Info struct { ID string; Size int64; Offset int64; MetaData map[string]string; StoragePath string }` and `func Parse(infoPath string) (*Info, error)`, plus `var ErrMalformed error`.

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
	ID          string
	Size        int64
	Offset      int64
	MetaData    map[string]string
	StoragePath string
}

type rawInfo struct {
	ID       string            `json:"ID"`
	Size     int64             `json:"Size"`
	Offset   int64             `json:"Offset"`
	MetaData map[string]string `json:"MetaData"`
	Storage  struct {
		Type string `json:"Type"`
		Path string `json:"Path"`
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
	if raw.Storage.Type != "filestore" {
		return nil, fmt.Errorf("%w: unsupported storage type %q", ErrMalformed, raw.Storage.Type)
	}
	if raw.Storage.Path == "" {
		return nil, fmt.Errorf("%w: empty storage path", ErrMalformed)
	}

	return &Info{
		ID:          raw.ID,
		Size:        raw.Size,
		Offset:      raw.Offset,
		MetaData:    raw.MetaData,
		StoragePath: raw.Storage.Path,
	}, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/tussidecar/ -v`
Expected: PASS.

- [ ] **Step 5: Switch the janitor to the shared parser**

In `backend/internal/httpapi/storage_cleanup.go`, replace the inline sidecar decoding inside `inspectTusCandidate` with a `tussidecar.Parse` call, keeping the existing behavior that a complete data file is left alone for ingest recovery. Do not change its retention policy.

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
  - `func (p *Processor) PrepareOriginal(ctx context.Context, sourcePath, mediaID, storedFilename string) error` — copy, fsync, rename, fsync dir. Never removes `sourcePath`.
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

	if err := p.PrepareOriginal(context.Background(), source, "media-1", "media-1.jpg"); err != nil {
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
}

func TestPrepareOriginalIsIdempotentForMatchingContent(t *testing.T) {
	p := newTestProcessor(t)
	source := filepath.Join(t.TempDir(), "incoming")
	if err := os.WriteFile(source, []byte("same"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := p.PrepareOriginal(context.Background(), source, "media-2", "media-2.jpg"); err != nil {
			t.Fatalf("prepare %d: %v", i, err)
		}
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

// PrepareOriginal copies sourcePath into permanent storage under
// storedFilename. It never removes sourcePath: until the media row is
// committed, that source may be the only complete copy in existence.
//
// Ordering is load-bearing — write, fsync file, rename, fsync directory —
// so that a crash can leave a stale temporary but never a truncated original.
func (p *Processor) PrepareOriginal(ctx context.Context, sourcePath, mediaID, storedFilename string) error {
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	finalPath := p.OriginalPath(storedFilename)

	src, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()

	tmpPath := filepath.Join(p.OriginalsDir(), ".ingest-"+mediaID+"-original.tmp")
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
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("rename original into place: %w", err)
	}
	return FsyncDir(p.OriginalsDir())
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

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/media/ -race -v`
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
	g.healthy.Store(true)
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

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/ingest/ -race -v`
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
  - `type Options struct { Workers int; DurabilityWorkers int; ProcessingTimeout, DurabilityWait, MaxBackoff, ReconcileInterval, JobRetention time.Duration; UploadDir string; MinFreeBytes int64 }`
  - `type Manager struct`
  - `func New(st *store.Store, proc *media.Processor, opts Options) *Manager`
  - `func (m *Manager) Start(ctx context.Context)` — non-blocking; captures the lifetime context.
  - `func (m *Manager) Stop()` — cancels and waits for workers.
  - `func (m *Manager) Wake()` — non-blocking nudge.
  - `func (m *Manager) Ready() bool`
  - `func (m *Manager) backoffFor(failures int) time.Duration`

- [ ] **Step 1: Write the failing test**

Create `backend/internal/ingest/manager_test.go`:

```go
package ingest

import (
	"context"
	"testing"
	"time"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Workers:           2,
		DurabilityWorkers: 2,
		ProcessingTimeout: time.Minute,
		DurabilityWait:    2 * time.Second,
		MaxBackoff:        time.Minute,
		ReconcileInterval: 50 * time.Millisecond,
		JobRetention:      time.Hour,
		UploadDir:         t.TempDir(),
		MinFreeBytes:      0,
	}
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

	m.Start(context.Background())
	m.Wake()
	m.Wake() // must not block even when nothing is draining the channel
	m.Stop()
}

func TestWorkersRunOnLifetimeContextNotTheCallers(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	// This is the production incident in miniature: the caller's context dies
	// immediately, and the manager must be entirely unaffected by it.
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	cancel()

	time.Sleep(100 * time.Millisecond)
	if m.lifetime.Err() == nil {
		t.Skip("manager intentionally derives from the passed context; see Stop()")
	}
	m.Stop()
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

type Options struct {
	Workers           int
	DurabilityWorkers int
	ProcessingTimeout time.Duration
	DurabilityWait    time.Duration
	MaxBackoff        time.Duration
	ReconcileInterval time.Duration
	JobRetention      time.Duration
	UploadDir         string
	MinFreeBytes      int64
}

// Manager owns the durable ingest queue. Its workers run on lifetime, a
// context tied to process shutdown alone — never to an HTTP request. The
// original data loss happened because ingest ran on a request-derived context
// that tusd cancelled ten seconds after the upload's final PATCH.
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
	m.durability = newDurabilityRegistry(m)
	return m
}

// Health exposes the gate so HTTP handlers can refuse uploads while the
// circuit is open.
func (m *Manager) Health() *HealthGate { return m.health }

// Ready reports whether the startup inventory has finished. Upload routes must
// return a retryable 503 until it has.
func (m *Manager) Ready() bool { return m.ready.Load() }

// Start launches the pool. parent bounds process lifetime only; callers must
// not pass a request context.
func (m *Manager) Start(parent context.Context) {
	m.lifetime, m.cancel = context.WithCancel(parent)

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

// Stop cancels the lifetime context and waits for in-flight work to yield.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
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

	m.startupRecovery()

	for {
		select {
		case <-m.lifetime.Done():
			return
		case <-ticker.C:
			if err := m.health.Check(m.lifetime); err != nil {
				slog.Warn("storage health check failed", "operation", "reconcile", "error", err)
			}
			m.reconcileOnce()
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
func (m *Manager) startupRecovery()               { m.ready.Store(true) }
func (m *Manager) reconcileOnce()                 {}
```

Also add the registry placeholder in `durability.go` (fully implemented in Task 9):

```go
package ingest

type durabilityRegistry struct{ manager *Manager }

func newDurabilityRegistry(m *Manager) *durabilityRegistry { return &durabilityRegistry{manager: m} }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/ingest/ -race -v`
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
- Modify: `backend/internal/httpapi/server.go`
- Test: `backend/internal/httpapi/tus_hooks_test.go`

**Interfaces:**
- Consumes: `store.CreateUploadingJob`, `ingest.Manager.Ready()`, `ingest.HealthGate.Healthy()`.
- Produces: `pre-create` returns `ChangeFileInfo.ID`; adds `retryHook(w, retryAfterSeconds, message)`; adds `Server.ingest *ingest.Manager` and `func (s *Server) SetIngest(m *ingest.Manager)`.

- [ ] **Step 1: Write the failing test**

Append to `backend/internal/httpapi/tus_hooks_test.go`:

```go
func TestPreCreateGeneratesIDAndJob(t *testing.T) {
	s := newTestServer(t) // existing helper in this package
	body := `{"Type":"pre-create","Event":{"Upload":{"Size":10,"MetaData":{"filename":"a.jpg","sha256":"abc"}}}}`

	rec := postHook(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp struct {
		ChangeFileInfo struct {
			ID string `json:"ID"`
		} `json:"ChangeFileInfo"`
		RejectUpload bool `json:"RejectUpload"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RejectUpload {
		t.Fatal("valid upload must be admitted")
	}
	if resp.ChangeFileInfo.ID == "" {
		t.Fatal("pre-create must generate the upload id")
	}

	job, err := s.store.GetUploadJob(context.Background(), resp.ChangeFileInfo.ID)
	if err != nil || job == nil {
		t.Fatalf("uploading row must exist: %+v %v", job, err)
	}
	if job.Status != store.JobUploading || job.ExpectedSize != 10 || job.DeclaredSHA256 != "abc" {
		t.Errorf("unexpected job %+v", job)
	}
}

func TestPreCreateRejectsDeferredSize(t *testing.T) {
	s := newTestServer(t)
	rec := postHook(t, s, `{"Type":"pre-create","Event":{"Upload":{"Size":0,"MetaData":{"filename":"a.jpg"}}}}`)

	var resp struct {
		RejectUpload bool `json:"RejectUpload"`
		HTTPResponse struct {
			StatusCode int `json:"StatusCode"`
		} `json:"HTTPResponse"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.RejectUpload || resp.HTTPResponse.StatusCode != http.StatusBadRequest {
		t.Errorf("deferred size must be a deterministic 400, got %+v", resp)
	}
}
```

If `newTestServer` / `postHook` do not exist in this package with these names, adapt to the helpers already used by `tus_hooks_test.go` rather than inventing new ones.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/httpapi/ -run PreCreate -v`
Expected: FAIL — no `ChangeFileInfo` in the response.

- [ ] **Step 3: Extend the hook response types**

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

// retryHook relays backpressure. tusd only honors a chosen status code when
// the hook itself answers 2xx and embeds the real response, so a non-2xx here
// would become an opaque 500 at the browser instead of a retryable 503.
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

- [ ] **Step 4: Rewrite the pre-create handler**

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

Update the dispatcher to pass `r` and to accept the new hook types:

```go
	switch req.Type {
	case "pre-create":
		s.handlePreCreateHook(w, r, req)
	case "pre-finish":
		s.handlePreFinishHook(w, r, req)
	case "pre-terminate":
		s.handlePreTerminateHook(w, r, req)
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

Delete `handlePostFinishHook` and `cleanupTusInfoFile` entirely — that pair is the incident. Tasks 9 and 10 add the two new handlers; add temporary stubs that call `allowHook(w)` so the package compiles until then.

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

- [ ] **Step 5: Add the manager helpers this handler calls**

Append to `backend/internal/ingest/manager.go`:

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

- [ ] **Step 6: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/httpapi/ ./internal/ingest/ -race -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add backend/internal/httpapi/ backend/internal/ingest/
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
	m.Start(context.Background())
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
	m.Start(context.Background())
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
	m.Start(context.Background())
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
	m.Start(context.Background())
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
	manager *Manager
	mu      sync.Mutex
	inFlight map[string]*durabilityOp
	slots   chan struct{}
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
	}()

	op.err = r.manager.makeDurable(uploadID)
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
func (m *Manager) makeDurable(uploadID string) error {
	ctx := m.lifetime

	job, err := m.store.GetUploadJob(ctx, uploadID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("no upload job for %s", uploadID)
	}
	if job.Status != store.JobUploading {
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
func (s *Server) handlePreFinishHook(w http.ResponseWriter, r *http.Request, req tusHookRequest) {
	// One absolute deadline, taken at the earliest possible moment. No later
	// phase may reset it, or the sum of phases could outlive the hook timeout.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UploadDurabilityWait)
	defer cancel()

	upload := req.Event.Upload
	if upload.ID == "" || !safeUploadID(upload.ID) {
		rejectHook(w, http.StatusBadRequest, "invalid upload id")
		return
	}
	if req.Event.Upload.Storage.Type != "filestore" {
		rejectHook(w, http.StatusBadRequest, "unsupported storage backend")
		return
	}
	// The hook must be talking about the path we derive, not one it chose.
	if filepath.Clean(upload.Storage.Path) != filepath.Clean(s.ingest.DataPath(upload.ID)) {
		rejectHook(w, http.StatusBadRequest, "unexpected storage path")
		return
	}
	if upload.Size <= 0 || upload.Offset != upload.Size {
		rejectHook(w, http.StatusBadRequest, "upload is not complete")
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
		retryHook(w, 5, "upload is still being persisted, please retry")
	}
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

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/ingest/ ./internal/httpapi/ -race -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/ingest/durability.go backend/internal/ingest/durability_test.go backend/internal/httpapi/tus_hooks.go
git commit -m "feat: make uploads durable in a blocking pre-finish hook"
```

---

### Task 10: Completion fence, cancellation, and pre-terminate

**Files:**
- Modify: `backend/internal/httpapi/tus_proxy.go`
- Modify: `backend/internal/httpapi/tus_hooks.go`
- Test: `backend/internal/httpapi/tus_proxy_test.go`

**Interfaces:**
- Consumes: `Manager.EnsureDurable`, `store.RequestCancellation`, `store.GetUploadJob`.
- Produces: `func (s *Server) fenceCompletedUpload(w http.ResponseWriter, r *http.Request, uploadID string) bool` — returns true when it has written a response and the request must not be forwarded.

- [ ] **Step 1: Write the failing test**

Append to `backend/internal/httpapi/tus_proxy_test.go`:

```go
func TestFenceBlocksSuccessUntilDurable(t *testing.T) {
	s := newTestServer(t)
	// A complete source whose row is still 'uploading' must never be reported
	// as successful, because tusd would otherwise short-circuit an
	// already-complete upload with a plain 204.
	seedUploadingWithCompleteSource(t, s, "u1")

	req := httptest.NewRequest(http.MethodHead, "/api/tus/u1", nil)
	rec := httptest.NewRecorder()
	if !s.fenceCompletedUpload(rec, req, "u1") {
		t.Fatal("fence must intercept an undurable complete upload")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("fence must tell the client when to retry")
	}
}

func TestDeleteAfterDurabilityReturns409(t *testing.T) {
	s := newTestServer(t)
	seedPendingJob(t, s, "u1")

	req := httptest.NewRequest(http.MethodDelete, "/api/tus/u1", nil)
	rec := httptest.NewRecorder()
	s.handleTusProxy(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: a durable completion is never silently reversed", rec.Code)
	}
}

func TestDeleteBeforeDurabilityRecordsCancellation(t *testing.T) {
	s := newTestServer(t)
	seedUploadingJob(t, s, "u1")

	req := httptest.NewRequest(http.MethodDelete, "/api/tus/u1", nil)
	rec := httptest.NewRecorder()
	s.handleTusProxy(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	job, _ := s.store.GetUploadJob(context.Background(), "u1")
	if job.CancellationRequestedAt == nil {
		t.Error("cancellation intent must be durable")
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
func (s *Server) fenceCompletedUpload(w http.ResponseWriter, r *http.Request, uploadID string) bool {
	if s.ingest == nil || uploadID == "" {
		return false
	}
	job, err := s.store.GetUploadJob(r.Context(), uploadID)
	if err != nil || job == nil || job.Status != store.JobUploading {
		return false
	}

	stat, err := os.Stat(s.ingest.DataPath(uploadID))
	if err != nil || stat.Size() != job.ExpectedSize {
		return false // still uploading; let the PATCH through
	}

	if err := s.ingest.EnsureDurable(r.Context(), uploadID); err == nil {
		return false // now durable; forwarding is safe
	}
	w.Header().Set("Retry-After", "5")
	writeError(w, http.StatusServiceUnavailable, "upload is still being persisted, please retry")
	return true
}

// handleTusDelete consumes a public DELETE as cancellation intent. It is never
// forwarded: only the app may terminate a tus upload, and only under a lease.
func (s *Server) handleTusDelete(w http.ResponseWriter, r *http.Request, uploadID string) {
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
	if err := s.store.RequestCancellation(r.Context(), uploadID, store.NowMicros()); err != nil && err != store.ErrNotClaimed {
		writeError(w, http.StatusInternalServerError, "could not record cancellation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

In `handleTusProxy`, before forwarding, extract the upload id from the path and route:

```go
	uploadID := tusUploadIDFromPath(r.URL.Path)
	if r.Method == http.MethodDelete && uploadID != "" {
		s.handleTusDelete(w, r, uploadID)
		return
	}
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

- [ ] **Step 4: Implement pre-terminate**

Replace the `handlePreTerminateHook` stub in `tus_hooks.go`:

```go
// handlePreTerminateHook authorizes deletion only for a job the app itself has
// claimed for discard, and only when the forwarded token matches its live
// lease. Queue status alone is never enough.
func (s *Server) handlePreTerminateHook(w http.ResponseWriter, r *http.Request, req tusHookRequest) {
	uploadID := req.Event.Upload.ID
	token := firstHeaderValue(req.Event.HTTPRequest.Header, ingestLeaseTokenHeader)

	job, err := s.store.GetUploadJob(r.Context(), uploadID)
	if err != nil || job == nil {
		rejectHook(w, http.StatusForbidden, "termination is not authorized")
		return
	}
	authorizedState := job.Status == store.JobDiscarding || job.Status == store.JobCleanup
	if !authorizedState || token == "" || token != job.LeaseToken {
		rejectHook(w, http.StatusForbidden, "termination is not authorized")
		return
	}
	allowHook(w)
}
```

Add the header constant next to `internalProxySecretHeader`:

```go
const ingestLeaseTokenHeader = "X-Ingest-Lease-Token"
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/httpapi/ -race -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/httpapi/
git commit -m "feat: fence completions and gate tus termination behind leases"
```

---

### Task 11: Processing, publication, and cleanup

**Files:**
- Create: `backend/internal/ingest/process.go`
- Create: `backend/internal/ingest/process_test.go`
- Modify: `backend/internal/ingest/manager.go` (replace the `claimAndRunOnce` stub)
- Modify: `backend/internal/store/upload_jobs.go` (add `PublishMedia`)

**Interfaces:**
- Consumes: `media.Processor.PrepareOriginal`, `media.Processor.VerifyOriginal`, `store.ClaimNextJob`, `store.FinishJob`.
- Produces: `func (s *Store) PublishMedia(ctx context.Context, uploadID, leaseToken string, item *models.MediaItem, now int64) (resultMediaID string, isDuplicate bool, err error)` and `func (m *Manager) claimAndRunOnce() (bool, error)`.

- [ ] **Step 1: Write the failing test**

Create `backend/internal/ingest/process_test.go`:

```go
package ingest

import (
	"context"
	"os"
	"testing"
	"time"

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
	m.Start(context.Background())
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
	m.Start(context.Background())
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", jpegFixture(t))
	_ = m.EnsureDurable(context.Background(), "u1")

	// Make the media volume unwritable so preparation fails transiently.
	if err := os.Chmod(proc.OriginalsDir(), 0o500); err != nil {
		t.Skipf("cannot simulate a read-only volume here: %v", err)
	}
	defer os.Chmod(proc.OriginalsDir(), 0o750)

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
	m.Start(context.Background())
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
	m.Start(context.Background())
	defer m.Stop()

	payload := jpegFixture(t)

	// Publish once.
	seedCompleteUpload(t, m, st, "u1", payload)
	_ = m.EnsureDurable(context.Background(), "u1")
	drainQueue(t, m)

	// Corrupt the surviving original, then upload the same bytes again.
	first, _ := st.GetUploadJob(context.Background(), "u1")
	if err := os.Remove(proc.OriginalPath(first.StoredFilename)); err != nil {
		t.Fatalf("remove original: %v", err)
	}

	seedCompleteUpload(t, m, st, "u2", payload)
	_ = m.EnsureDurable(context.Background(), "u2")
	_, _ = m.claimAndRunOnce()

	second, _ := st.GetUploadJob(context.Background(), "u2")
	if second.Status != store.JobPending {
		t.Errorf("status = %q, want pending: a corrupt authoritative original is an integrity fault, not a duplicate", second.Status)
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

Append to `backend/internal/store/upload_jobs.go`:

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

	var owned int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM upload_jobs WHERE upload_id = ? AND lease_token = ? AND status = 'processing'`,
		uploadID, leaseToken).Scan(&owned)
	if err != nil {
		return "", false, fmt.Errorf("verify lease: %w", err)
	}
	if owned == 0 {
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
		   SET status = 'cleanup', result_media_id = ?, updated_at = ?
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

	if job, err := m.store.ClaimNextJob(m.lifetime, store.JobPending, store.JobProcessing, now, m.opts.ProcessingTimeout); err != nil {
		return false, err
	} else if job != nil {
		m.runProcessing(job)
		return true, nil
	}

	if job, err := m.store.ClaimNextJob(m.lifetime, store.JobCleanup, store.JobCleanup, now, m.opts.ProcessingTimeout); err != nil {
		return false, err
	} else if job != nil {
		m.runCleanup(job)
		return true, nil
	}

	if job, err := m.store.ClaimNextJob(m.lifetime, store.JobDiscarding, store.JobDiscarding, now, m.opts.ProcessingTimeout); err != nil {
		return false, err
	} else if job != nil {
		m.runDiscard(job)
		return true, nil
	}

	return false, nil
}

func (m *Manager) runProcessing(job *store.UploadJob) {
	ctx, cancel := context.WithTimeout(m.lifetime, m.opts.ProcessingTimeout)
	defer cancel()

	if err := m.prepareAndPublish(ctx, job); err != nil {
		var rejection *clientRejection
		if errors.As(err, &rejection) {
			// The only class permitted to discard a complete source, and only
			// while no final artifact exists yet.
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
		if err := m.store.ReleaseForRetry(m.lifetime, job.UploadID, job.LeaseToken, next, "processing_failures", truncateError(err)); err != nil {
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
type clientRejection struct{ reason string }

func (e *clientRejection) Error() string { return "client rejection: " + e.reason }

func (m *Manager) prepareAndPublish(ctx context.Context, job *store.UploadJob) error {
	sourcePath := m.DataPath(job.UploadID)

	mimeType, kind, err := media.Sniff(sourcePath)
	if err != nil {
		return fmt.Errorf("sniff source: %w", err)
	}
	if !media.IsAllowed(mimeType, kind, m.processor.AllowedImageMIMEs, m.processor.AllowedVideoMIMEs) {
		return &clientRejection{reason: "unsupported_type"}
	}

	sum, err := media.SHA256File(sourcePath)
	if err != nil {
		return fmt.Errorf("hash source: %w", err)
	}
	if job.DeclaredSHA256 != "" && job.DeclaredSHA256 != sum {
		return &clientRejection{reason: "checksum_mismatch"}
	}

	stat, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	storedFilename := job.MediaID + media.ExtensionForMIME(mimeType, job.OriginalFilename)
	if err := m.store.RecordPreparation(ctx, job.UploadID, job.LeaseToken, storedFilename, mimeType, sum); err != nil {
		return err
	}

	if err := m.processor.PrepareOriginal(ctx, sourcePath, job.MediaID, storedFilename); err != nil {
		return fmt.Errorf("prepare original: %w", err)
	}
	if err := m.processor.VerifyOriginal(storedFilename, stat.Size(), sum); err != nil {
		return fmt.Errorf("verify prepared original: %w", err)
	}

	// Before we may resolve a duplicate, the copy we would keep must be proven
	// intact. Otherwise "duplicate detected" could delete a good new file in
	// favor of a corrupt old one.
	if existing, err := m.store.GetBySHA256(ctx, sum); err != nil {
		return fmt.Errorf("look up existing content: %w", err)
	} else if existing != nil {
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
	slog.Info("upload published", "operation", "publication",
		"upload_id", job.UploadID, "media_id", resultID, "duplicate", duplicate)
	return nil
}

// runCleanup removes the source, and a duplicate's own artifacts, now that a
// media row is committed. This is the only path that may delete a source
// after a successful publication.
func (m *Manager) runCleanup(job *store.UploadJob) {
	if !m.health.Healthy() {
		slog.Warn("skipping cleanup while storage health is open", "operation", "cleanup", "upload_id", job.UploadID)
		return
	}

	if job.ResultMediaID != "" && job.ResultMediaID != job.MediaID {
		owner, err := m.store.GetByID(m.lifetime, job.MediaID, "")
		if err == nil && owner == nil {
			// No committed row owns our media id, so our artifacts are safe to remove.
			m.removeArtifacts(job)
		}
	}
	m.finishBySourceRemoval(job, store.JobComplete, "")
}

// runDiscard terminates a rejected or cancelled upload.
func (m *Manager) runDiscard(job *store.UploadJob) {
	if !m.health.Healthy() {
		return
	}
	owner, err := m.store.GetByID(m.lifetime, job.MediaID, "")
	if err == nil && owner == nil {
		m.removeArtifacts(job)
	}
	m.finishBySourceRemoval(job, store.JobDiscarded, job.TerminalReason)
}

func (m *Manager) removeArtifacts(job *store.UploadJob) {
	if job.StoredFilename != "" {
		_ = os.Remove(m.processor.OriginalPath(job.StoredFilename))
	}
	_ = os.Remove(m.processor.ThumbnailPath(job.MediaID))
	_ = media.FsyncDir(m.processor.OriginalsDir())
}

// finishBySourceRemoval deletes the tus source and commits the terminal state.
// It never issues a termination for a path it cannot currently observe: an
// absent file may be a faulted mount rather than a deleted one.
func (m *Manager) finishBySourceRemoval(job *store.UploadJob, status store.JobStatus, reason string) {
	dataPath := m.DataPath(job.UploadID)
	if _, err := os.Stat(dataPath); err == nil {
		if err := os.Remove(dataPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.retryCleanup(job, err)
			return
		}
	}
	if _, err := os.Stat(m.InfoPath(job.UploadID)); err == nil {
		_ = os.Remove(m.InfoPath(job.UploadID))
	}
	if err := media.FsyncDir(m.opts.UploadDir); err != nil {
		m.retryCleanup(job, err)
		return
	}
	// Verify absence across the directory fsync before terminalizing.
	if _, err := os.Stat(dataPath); err == nil {
		m.retryCleanup(job, errors.New("source still present after removal"))
		return
	}
	if err := m.store.FinishJob(m.lifetime, job.UploadID, job.LeaseToken, status, reason, store.NowMicros()); err != nil {
		slog.Error("failed to commit terminal state", "operation", "cleanup", "upload_id", job.UploadID, "error", err)
	}
}

func (m *Manager) retryCleanup(job *store.UploadJob, cause error) {
	next := store.NowMicros() + m.backoffFor(job.CleanupFailures).Microseconds()
	if err := m.store.ReleaseForRetry(m.lifetime, job.UploadID, job.LeaseToken, next, "cleanup_failures", truncateError(cause)); err != nil {
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

var _ = filepath.Clean
```

- [ ] **Step 5: Add the two supporting helpers**

Append to `backend/internal/store/upload_jobs.go`:

```go
// RecordPreparation persists the deterministic artifact identity before the
// final original exists, so a crash can be recovered by name and hash instead
// of by re-copying or deleting.
func (s *Store) RecordPreparation(ctx context.Context, uploadID, leaseToken, storedFilename, mimeType, sha256Hex string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET stored_filename = ?, mime_type = ?, authoritative_sha256 = ?, prepared_at = ?, updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		storedFilename, mimeType, sha256Hex, NowMicros(), NowMicros(), uploadID, leaseToken)
	if err != nil {
		return fmt.Errorf("record preparation: %w", err)
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

- [ ] **Step 6: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/ingest/ ./internal/store/ -race -v`
Expected: PASS. `TestTransientFailureNeverDeletesTheSource` is the regression test for the production incident — if it fails, stop and fix before continuing.

- [ ] **Step 7: Commit**

```bash
git add backend/internal/ingest/ backend/internal/store/ backend/internal/media/processor.go
git commit -m "feat: publish uploads transactionally and delete sources only after commit"
```

---

### Task 12: Recovery reconciler and readiness

**Files:**
- Create: `backend/internal/ingest/recover.go`
- Create: `backend/internal/ingest/recover_test.go`
- Modify: `backend/internal/ingest/manager.go` (replace the `startupRecovery` and `reconcileOnce` stubs)
- Modify: `backend/internal/httpapi/server.go` (add `/readyz`)

**Interfaces:**
- Consumes: `tussidecar.Parse`, `store.RequeueStartup`, `store.CreateUploadingJob`, `store.PromoteToPending`.
- Produces: `func (m *Manager) startupRecovery()`, `func (m *Manager) reconcileOnce()`, `func (s *Server) handleReady(w http.ResponseWriter, r *http.Request)`.

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
	m.lifetime = context.Background()

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
	m.lifetime = context.Background()

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

func TestAbsentSourceNeverTerminalizesADurableJob(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	m.lifetime = context.Background()

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

Add `writeSidecar(t, m, id, size)` to `fixture_test.go`, writing a JSON sidecar whose `Storage.Path` is `m.DataPath(id)`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/ingest/ -run 'Adopt|Partial|Absent' -v`
Expected: FAIL — reconciliation is a no-op stub.

- [ ] **Step 3: Implement recovery**

Create `backend/internal/ingest/recover.go`:

```go
package ingest

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"event-gallery/backend/internal/store"
	"event-gallery/backend/internal/tussidecar"
)

// startupRecovery makes interrupted work claimable and adopts any completed
// upload the previous process never queued, then opens the readiness gate.
func (m *Manager) startupRecovery() {
	if _, err := m.store.RequeueStartup(m.lifetime, store.NowMicros()); err != nil {
		slog.Error("failed to requeue interrupted jobs", "operation", "startup_recovery", "error", err)
	}
	if err := m.health.Check(m.lifetime); err != nil {
		slog.Error("storage health check failed at startup", "operation", "startup_recovery", "error", err)
	}
	m.reconcileOnce()
	m.ready.Store(true)
	slog.Info("ingest ready", "operation", "startup_recovery")
	m.Wake()
}

// reconcileOnce walks the upload directory and repairs what it can prove.
// It never deletes: the only outcomes are adoption, promotion, and leaving
// things exactly as they are.
func (m *Manager) reconcileOnce() {
	entries, err := os.ReadDir(m.opts.UploadDir)
	if err != nil {
		slog.Warn("cannot read upload directory", "operation", "reconcile", "error", err)
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".info") {
			continue
		}
		uploadID := strings.TrimSuffix(entry.Name(), ".info")
		if !isSafeUploadID(uploadID) {
			continue
		}
		if err := m.reconcileOne(uploadID, filepath.Join(m.opts.UploadDir, entry.Name())); err != nil {
			slog.Warn("reconcile failed", "operation", "reconcile", "upload_id", uploadID, "error", err)
		}
	}
}

func (m *Manager) reconcileOne(uploadID, infoPath string) error {
	job, err := m.store.GetUploadJob(m.lifetime, uploadID)
	if err != nil {
		return err
	}
	// A non-terminal row already owns this upload; the workers will handle it.
	if job != nil && job.Status != store.JobComplete && job.Status != store.JobDiscarded {
		return nil
	}

	info, err := tussidecar.Parse(infoPath)
	if err != nil {
		if errors.Is(err, tussidecar.ErrMalformed) {
			// A malformed sidecar is "unknown", never "safe to delete".
			return m.adoptByDataFileAlone(uploadID)
		}
		return err
	}

	stat, err := os.Stat(m.DataPath(uploadID))
	if err != nil {
		return nil // nothing observable; absence is never actionable here
	}
	if !stat.Mode().IsRegular() || stat.Size() != info.Size {
		return nil // still uploading; the incomplete-retention policy owns it
	}

	return m.adopt(uploadID, info.Size, info.MetaData)
}

func (m *Manager) adoptByDataFileAlone(uploadID string) error {
	stat, err := os.Stat(m.DataPath(uploadID))
	if err != nil || !stat.Mode().IsRegular() || stat.Size() == 0 {
		return nil
	}
	// Without metadata we cannot know the declared size, so we adopt the file
	// at its observed size rather than discarding a possibly complete upload.
	return m.adopt(uploadID, stat.Size(), nil)
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
	if err := m.store.PromoteToPending(m.lifetime, uploadID, store.NowMicros()); err != nil {
		return err
	}
	slog.Info("adopted completed upload", "operation", "reconcile", "upload_id", uploadID, "media_id", job.MediaID)
	m.Wake()
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

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/ingest/ ./internal/httpapi/ -race -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/ingest/ backend/internal/httpapi/server.go
git commit -m "feat: adopt orphaned completed uploads and gate upload routes on readiness"
```

---

### Task 13: Upload status API

**Files:**
- Create: `backend/internal/httpapi/uploads_status.go`
- Create: `backend/internal/httpapi/uploads_status_test.go`
- Modify: `backend/internal/httpapi/server.go`

**Interfaces:**
- Consumes: `store.GetUploadJob`.
- Produces: `POST /api/uploads/status` accepting `{"uploadIds":["..."]}` (max 100) and returning `{"results":{"<id>":{"state":"...","mediaId":"..."}}}`.

- [ ] **Step 1: Write the failing test**

Create `backend/internal/httpapi/uploads_status_test.go`:

```go
package httpapi

import (
	"net/http"
	"testing"
)

func TestUploadStatusMapsQueueStatesToPublicStates(t *testing.T) {
	s := newTestServer(t)

	cases := []struct {
		name  string
		setup func(t *testing.T, uploadID string)
		want  string
	}{
		{"uploading", seedStatusUploading, "uploading"},
		{"pending", seedStatusPending, "processing"},
		{"published", seedStatusComplete, "published"},
		{"checksum mismatch", seedStatusChecksumMismatch, "failed"},
		{"cancelled", seedStatusCancelled, "cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t, "u-"+tc.name)
			got := postStatus(t, s, []string{"u-" + tc.name})
			if got[("u-" + tc.name)].State != tc.want {
				t.Errorf("state = %q, want %q", got["u-"+tc.name].State, tc.want)
			}
		})
	}
}

func TestUploadStatusRejectsOversizedBatch(t *testing.T) {
	s := newTestServer(t)
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = "u"
	}
	rec := postStatusRaw(t, s, ids)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestUploadStatusReportsUnknownPerID(t *testing.T) {
	s := newTestServer(t)
	got := postStatus(t, s, []string{"never-existed"})
	if got["never-existed"].State != "unknown" {
		t.Errorf("state = %q, want unknown", got["never-existed"].State)
	}
}
```

Write the seed and post helpers in the same file.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/httpapi/ -run UploadStatus -v`
Expected: FAIL — route not registered.

- [ ] **Step 3: Implement the endpoint**

Create `backend/internal/httpapi/uploads_status.go`:

```go
package httpapi

import (
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
	for _, id := range req.UploadIDs {
		if _, seen := results[id]; seen {
			continue
		}
		job, err := s.store.GetUploadJob(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read upload state")
			return
		}
		results[id] = publicUploadState(job)
	}
	writeJSON(w, http.StatusOK, uploadStatusResponse{Results: results})
}

// publicUploadState maps internal queue states onto the small vocabulary the
// browser understands, without leaking queue mechanics. Core failures report
// "processing" because they retry indefinitely and will eventually publish.
func publicUploadState(job *store.UploadJob) uploadStatusEntry {
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
		if job.ResultMediaID != "" && job.ResultMediaID != job.MediaID {
			return uploadStatusEntry{State: "duplicate", MediaID: job.ResultMediaID}
		}
		return uploadStatusEntry{State: "published", MediaID: job.ResultMediaID}
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
```

Register it in `server.go` with its own limiter so status polling cannot drain the public gallery bucket:

```go
		pub.With(s.uploadStatusRateLimit).Post("/uploads/status", s.handleUploadStatus)
```

Add `uploadStatusLimiter` to `Server`, built from `cfg.UploadStatusRateLimitPerMinute` exactly like `publicLimiter`, and a `uploadStatusRateLimit` middleware mirroring `publicRateLimit`.

Finally, make `/api/uploads/check` a shim so pre-upgrade browser tabs stop deleting their own files:

```go
// handleUploadCheck is retained only for tabs loaded before this deploy. The
// old client removed the local file on a duplicate verdict, so this must
// always answer false and let server-side SHA-256 resolution converge instead.
func (s *Server) handleUploadCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"duplicate": false})
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine go test ./internal/httpapi/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

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
- Modify: `docs/ARCHITECTURE.md`

**Interfaces:**
- Consumes: everything above.
- Produces: a running system where tusd calls four hooks and the app owns the queue.

- [ ] **Step 1: Wire the manager into main**

In `backend/cmd/server/main.go`, after the store and processor are built and before the HTTP server starts:

```go
	ingestManager := ingest.New(st, processor, ingest.Options{
		Workers:           cfg.MediaProcessingWorkers,
		DurabilityWorkers: cfg.UploadDurabilityWorkers,
		ProcessingTimeout: cfg.MediaProcessingTimeout,
		DurabilityWait:    cfg.UploadDurabilityWait,
		MaxBackoff:        cfg.UploadRetryMaxBackoff,
		ReconcileInterval: cfg.IngestReconcileInterval,
		JobRetention:      cfg.UploadJobRetention,
		UploadDir:         cfg.TusUploadDir,
		MinFreeBytes:      cfg.IngestMinFreeBytes,
	})
	// Deliberately context.Background(): ingest must outlive every request.
	ingestManager.Start(context.Background())
	defer ingestManager.Stop()

	srv.SetIngest(ingestManager)
```

Ensure `ingestManager.Stop()` runs during graceful shutdown, before the database is closed.

- [ ] **Step 2: Update the tusd entrypoint**

In `deploy/tusd-entrypoint.sh`, extend the hook list, forward the lease header, and set the derived timeouts:

```sh
  -hooks-http-forward-headers "X-Internal-Proxy-Secret,X-Event-Gallery-Client-Ip,X-Ingest-Lease-Token" \
  -hooks-enabled-events "pre-create,pre-finish,post-finish,pre-terminate" \
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
      UPLOAD_JOB_RETENTION_DAYS: "${UPLOAD_JOB_RETENTION_DAYS:-30}"
      UPLOAD_STATUS_RATE_LIMIT_PER_MINUTE: "${UPLOAD_STATUS_RATE_LIMIT_PER_MINUTE:-6000}"
      IMAGE_MAX_SOURCE_PIXELS: "${IMAGE_MAX_SOURCE_PIXELS:-50000000}"
```

- [ ] **Step 4: Correct the backup documentation**

In `docs/ARCHITECTURE.md`, replace the statement that the tus volume holds only transient resumability state:

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
docker run --rm -v "${PWD}/backend:/src" -w /src golang:1.25-alpine sh -c "go build ./cmd/server && go vet ./... && go test ./... -race"
docker compose config >/dev/null
```
Expected: build succeeds, vet is silent, all tests pass, compose config parses.

- [ ] **Step 6: Commit**

```bash
git add backend/cmd/server/main.go deploy/tusd-entrypoint.sh docker-compose.yml docs/ARCHITECTURE.md
git commit -m "feat: wire durable ingest into the app, tusd, and compose"
```

---

## Self-Review Notes

Checked against the spec before handing off:

- **Covered:** SQLite queue and migration (Task 1), configuration (Task 2), leased claims and retry scheduling (Tasks 3, 7), shared sidecar parsing (Task 4), fsync-ordered preparation that preserves the source (Task 5), storage health gate (Task 6), pre-create admission with backpressure envelopes (Task 8), blocking pre-finish durability barrier (Task 9), completion fence, cancellation, and lease-gated pre-terminate (Task 10), deterministic preparation, transactional publication, duplicate validation, and cleanup (Task 11), recovery and readiness (Task 12), status API and legacy shim (Task 13), wiring and rollout (Task 14).
- **Deferred by design to plans 2 and 3:** the `heif-preview` helper, `has_preview` population, `GET /api/media/{id}/preview`, `PREVIEW_MAX_DIMENSION`, `HEIC_MAX_SOURCE_PIXELS`, `HEIC_CONVERSION_MAX_FAILURES`, the `conversion_failures` budget's consumer, `MEDIA_TOOL_MEMORY_BYTES` enforcement, the Uppy retry wrapper, status-driven gallery refresh, and the `tus_battle.py` completion oracle. The schema column and config values land here so plan 2 is purely additive.
- **Known follow-up inside this plan:** Task 11 leaves `conversion_failures` unused; that is intentional and plan 2 consumes it.
- **Naming consistency verified:** `EnsureDurable`, `PromoteToPending`, `ClaimNextJob`, `ReleaseForRetry`, `FinishJob`, `PublishMedia`, `RecordPreparation`, `PrepareOriginal`, `VerifyOriginal`, and `DataPath`/`InfoPath` are used identically everywhere they appear.

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-11-durable-ingest-queue.md`. Two execution options:

1. **Subagent-Driven (recommended)** — a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
