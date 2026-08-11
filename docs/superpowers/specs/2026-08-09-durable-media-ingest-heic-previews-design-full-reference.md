# Durable Media Ingest and HEIC Preview Design

## Background

The current tusd `post-finish` hook performs the entire ingest synchronously:
it validates and hashes the upload, moves it to permanent storage, generates
derivatives, inserts the SQLite row, and removes the tus sidecar. tusd starts
the non-blocking hook with a context derived from the completed PATCH request.
That context is cancelled after tusd's 10-second request-completion grace
period. Under concurrent uploads, processing can outlive that context. A late
SQLite insert then fails with `context canceled`, after which the app deletes
the moved media and the deferred hook cleanup deletes the sidecar.

HEIC/HEIF uploads have a separate compatibility problem. The pure-Go decoder
does not support them, and the pinned ffmpeg build cannot demux every phone's
HEIC container. Those uploads are currently published without thumbnails and
the lightbox sends `image/heic` directly to the browser. Chromium and Firefox
cannot display that response.

## Goals

- A completed tus upload is durably queued in blocking `pre-finish` before
  tusd acknowledges the final PATCH.
- Each accepted upload reserves conservative incoming and media capacity before
  tusd creates its source file.
- The completed tus data file, sidecar, and directory entry reach a filesystem
  durability barrier before the queue transaction commits.
- Processing is independent of the hook request context.
- A bounded worker pool limits concurrent hashing, copying, and decoder work.
- Processing and cleanup are idempotent and recover after process crashes.
- Transient processing or database failures never delete the only completed
  upload copy.
- The tus data file and `.info` sidecar are removed only after publication or
  duplicate resolution is durably committed.
- Public tus termination cannot remove a completed upload while it is queued or
  processing.
- Core server-side failures retry indefinitely with persisted backoff and
  retain the completed source until publication succeeds.
- HEIC and HEIF remain accepted. libheif produces JPEG thumbnails and bounded
  browser preview derivatives while the original remains available for
  download.
- If HEIC conversion exhausts its separate retry budget, publish the original
  without derivatives and let capable browsers try the original.
- ffprobe and ffmpeg failures are visible in structured logs.
- The production load test distinguishes tus transport completion from
  database completion and public gallery visibility.
- The browser can follow each successful tus upload to a durable processing or
  terminal state without relying on a fixed polling window.

## Non-Goals

- No Redis, external broker, or separate worker service.
- No Portainer deployment or production data migration in this change.
- No admin UI for queue inspection or manual retry.
- No rejection of HEIC/HEIF uploads.
- No blanket tusd timeout increase.
- No conversion of downloadable originals.
- No automatic deletion of a completed source because a server-side retry
  budget elapsed.

## Invariants

1. The incoming tus data file remains authoritative until SQLite commits a
  media row or duplicate resolution, or until deterministic client rejection
  or cancellation commits discard intent.
2. Before enqueue commits, the regular data file and `.info` sidecar are
  synced and their parent directory is synced. Filesystem guarantees assume
  the host volume honors `fsync`; catastrophic volume loss is outside scope.
3. A request-context cancellation can prevent enqueueing, but cannot trigger
  source deletion. The complete-sidecar reconciler supplies eventual enqueue
  if hook delivery fails.
4. Each physical tus upload ID is one queue-job identity. A stable browser
  lifecycle may elect multiple numbered physical attempts, but at most one is
  current and only that row may promote. Each job has one preallocated media ID
  reused across its processing retries.
5. An `uploading` row reserves capacity before tusd creates the source. A
   completed upload becomes processable only after its durability barrier and
   fenced transition to `pending` commit.
6. Permanent artifacts are written through lease-specific, same-directory
  temporary files. Each file is synced and closed before atomic rename, and
  the parent directory is synced after rename before publication may commit.
  An original temp is additionally parent-synced before rename before it may
  count as a recoverable alternate copy.
7. Every processing, retry, publication, cleanup, and discard transition is
  fenced by the claim's random lease token. A stale worker cannot mutate job
  state or authorize source deletion.
8. Media insertion or duplicate resolution and transition to `cleanup` occur
  in one SQLite write transaction.
9. SQLite runs WAL with `synchronous=FULL`. A durable publication/cleanup or
   discard-intent commit is a precondition for irreversible source removal.
10. Only a currently leased job in `cleanup` or `discarding` may authorize
  termination of a complete tus upload. tusd owns normal deletion of its
  data, sidecar, and lock state.
11. Cleanup is idempotent, but an HTTP response alone is not completion. Both
  tus paths must be absent and the upload directory synced.
12. A job is marked `complete` only after cleanup succeeds. Late tus artifacts
  may re-enter `cleanup` solely to restore verified source absence; the durable
  media result remains complete and publicly terminal throughout.
13. A published HEIC/HEIF item either has JPEG derivatives, recorded a typed
  deterministic conversion outcome, or exhausted the separate transient-
  conversion retry budget. The latter two publish the retained original with
  `has_preview=0` and `has_thumbnail=0` without conflating their counters.

## Architecture

### SQLite queue

Add migration `0004_durable_upload_jobs_and_previews.sql`.

Database opening has explicit modes. Normal server mode acquires the app-
instance lock, opens SQLite without applying migrations, reads
`schema_migrations`/identity state, and auto-applies migrations only when the
database is already identity-bound and the required mounts match. If a fresh or
pre-0004 database is unbound, normal startup binds no listener and applies
nothing; it exits/logs the exact stopped-stack initializer command.

`init-storage-identities` is the sole first-upgrade/fresh migration owner. Under
the initializer flock it opens SQLite with auto-migration disabled, validates
the supported pre-migration schema (or truly empty fresh database) and mounted
evidence, then publishes/fsyncs all sentinels. Its final immediate transaction
applies all pending migration SQL through 0004, records each
`schema_migrations` row, inserts/verifies `storage_volumes`, and writes the
selected adoption/replacement audit atomically. SQLite DDL is transactional;
any statement or final predicate failure rolls back schema version and identity
rows together. A rerun adopts the durable sentinels and retries the same
transaction. Already-current databases skip DDL but use the same identity
transaction and predicates.

The migration adds `media_items.has_preview INTEGER NOT NULL DEFAULT 0` and an
`upload_jobs` table:

```sql
CREATE TABLE upload_jobs (
    upload_id TEXT PRIMARY KEY,
    client_upload_id TEXT NOT NULL,
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    is_current INTEGER NOT NULL DEFAULT 1 CHECK (is_current IN (0, 1)),
    media_id TEXT NOT NULL UNIQUE,
    original_filename TEXT NOT NULL,
    stored_filename TEXT,
    mime_type TEXT,
    authoritative_sha256 TEXT,
    prepared_size INTEGER,
    prepared_at INTEGER,
    expected_size INTEGER NOT NULL,
    observed_offset INTEGER NOT NULL DEFAULT 0,
    client_bound_at INTEGER,
    write_started_at INTEGER,
    source_completed_at INTEGER,
    cancellation_requested_at INTEGER,
    declared_sha256 TEXT NOT NULL DEFAULT '',
    guest_name TEXT NOT NULL DEFAULT '',
    uploader_ip TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (
        status IN (
            'uploading', 'cancelling', 'pending', 'processing', 'cleanup', 'complete',
            'discarding', 'discarded'
        )
    ),
    create_lease_token TEXT,
    create_lease_until INTEGER,
    create_tusd_boot_id TEXT,
    create_finished_at INTEGER,
    create_concluded_at INTEGER,
    write_lease_token TEXT,
    write_lease_until INTEGER,
    transport_lease_token TEXT,
    transport_lease_until INTEGER,
    transport_tusd_boot_id TEXT,
    transport_operation TEXT CHECK (
      transport_operation IS NULL OR
      transport_operation IN ('head', 'patch', 'terminate')
    ),
    sidecar_repair_token TEXT,
    sidecar_repair_until INTEGER,
    sidecar_repair_artifact_token TEXT,
    processing_failures INTEGER NOT NULL DEFAULT 0,
    conversion_failures INTEGER NOT NULL DEFAULT 0,
    cleanup_failures INTEGER NOT NULL DEFAULT 0,
    discard_failures INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    creation_grace_until INTEGER NOT NULL,
    reservation_expires_at INTEGER,
    incoming_reservation_bytes INTEGER NOT NULL,
    media_reservation_bytes INTEGER NOT NULL,
    physical_slot_active INTEGER NOT NULL DEFAULT 1 CHECK (physical_slot_active IN (0, 1)),
    incoming_reservation_active INTEGER NOT NULL DEFAULT 1,
    media_reservation_active INTEGER NOT NULL DEFAULT 1,
    last_activity_at INTEGER NOT NULL,
    lease_token TEXT,
    lease_until INTEGER,
    last_error TEXT NOT NULL DEFAULT '',
    result_media_id TEXT,
    artifacts_cleaned_at INTEGER,
    terminal_reason TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    completed_at INTEGER,
    discarded_at INTEGER,
    CHECK (
      (
        sidecar_repair_token IS NULL AND
        sidecar_repair_until IS NULL
      ) OR
      (
        sidecar_repair_token IS NOT NULL AND
        sidecar_repair_until IS NOT NULL AND
        sidecar_repair_artifact_token IS NOT NULL AND
        sidecar_repair_artifact_token = sidecar_repair_token AND
        status = 'uploading'
      )
    ),
    CHECK (
      artifacts_cleaned_at IS NULL OR sidecar_repair_artifact_token IS NULL
    ),
    CHECK (
      (
        transport_lease_token IS NULL AND
        transport_lease_until IS NULL AND
        transport_tusd_boot_id IS NULL AND
        transport_operation IS NULL
      ) OR (
        transport_lease_token IS NOT NULL AND
        transport_lease_until IS NOT NULL AND
        transport_tusd_boot_id IS NOT NULL AND
        transport_operation IS NOT NULL
      )
    ),
    CHECK (
      (
        write_lease_token IS NULL AND
        write_lease_until IS NULL AND
        (transport_operation IS NULL OR transport_operation IN ('head', 'terminate'))
      ) OR (
        write_lease_token IS NOT NULL AND
        write_lease_until IS NOT NULL AND
        transport_operation = 'patch' AND
        write_lease_token = transport_lease_token
      )
    )
);

CREATE UNIQUE INDEX idx_upload_jobs_client_attempt
  ON upload_jobs(client_upload_id, attempt_number);

CREATE UNIQUE INDEX idx_upload_jobs_current_client
  ON upload_jobs(client_upload_id)
  WHERE is_current = 1;

CREATE INDEX idx_upload_jobs_sidecar_repair_due
  ON upload_jobs(sidecar_repair_until)
  WHERE sidecar_repair_token IS NOT NULL;

CREATE INDEX idx_upload_jobs_due
  ON upload_jobs(status, next_attempt_at, created_at);

CREATE INDEX idx_upload_jobs_lease
  ON upload_jobs(status, lease_until);

CREATE INDEX idx_upload_jobs_result_media_nonterminal
  ON upload_jobs(result_media_id, status, upload_id)
  WHERE result_media_id IS NOT NULL
    AND status NOT IN ('complete', 'discarded');

CREATE INDEX idx_upload_jobs_result_media_any
  ON upload_jobs(result_media_id, status, upload_id)
  WHERE result_media_id IS NOT NULL;

CREATE INDEX idx_upload_jobs_complete_gc
  ON upload_jobs(completed_at, upload_id)
  WHERE status = 'complete'
    AND physical_slot_active = 0
    AND artifacts_cleaned_at IS NOT NULL;

CREATE INDEX idx_upload_jobs_discarded_gc
  ON upload_jobs(discarded_at, upload_id)
  WHERE status = 'discarded'
    AND physical_slot_active = 0
    AND artifacts_cleaned_at IS NOT NULL;

CREATE TABLE upload_attempt_controls (
  client_upload_id TEXT NOT NULL,
  attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
  seen_at INTEGER NOT NULL,
  cancellation_requested_at INTEGER,
  expires_at INTEGER NOT NULL,
  PRIMARY KEY (client_upload_id, attempt_number)
);

CREATE INDEX idx_upload_attempt_controls_expires
  ON upload_attempt_controls(expires_at);

CREATE TABLE upload_lifecycle_controls (
    client_upload_id TEXT PRIMARY KEY,
    max_seen_attempt INTEGER NOT NULL CHECK (max_seen_attempt > 0),
    max_admitted_attempt INTEGER NOT NULL DEFAULT 0,
    client_admissible_until INTEGER NOT NULL,
    cancelled_at INTEGER,
    max_seen_attempt_at_cancel INTEGER,
    closed_at INTEGER,
    terminal_outcome TEXT CHECK (
      terminal_outcome IS NULL OR
      terminal_outcome IN ('published', 'duplicate', 'cancelled', 'failed')
    ),
    expires_at INTEGER NOT NULL,
    CHECK (
        (cancelled_at IS NULL AND max_seen_attempt_at_cancel IS NULL) OR
        (cancelled_at IS NOT NULL AND max_seen_attempt_at_cancel IS NOT NULL)
    ),
    CHECK (
      max_admitted_attempt >= 0 AND
      max_admitted_attempt <= max_seen_attempt
    ),
    CHECK (
      (closed_at IS NULL AND terminal_outcome IS NULL) OR
      (closed_at IS NOT NULL AND terminal_outcome IS NOT NULL)
    ),
    CHECK (
      cancelled_at IS NULL OR
      (closed_at IS NOT NULL AND terminal_outcome = 'cancelled')
    ),
    CHECK (
      client_admissible_until < expires_at
    )
);

CREATE INDEX idx_upload_lifecycle_controls_expires
  ON upload_lifecycle_controls(expires_at);

CREATE INDEX idx_audit_log_action_media_created
  ON audit_log(action, media_id, created_at, id)
  WHERE media_id IS NOT NULL;

CREATE TABLE media_purge_tombstones (
    media_id TEXT PRIMARY KEY,
    purge_token TEXT NOT NULL UNIQUE,
    stored_filename TEXT NOT NULL,
    expected_original_size INTEGER NOT NULL CHECK (expected_original_size > 0),
    purged_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    CHECK (purged_at < expires_at)
);

CREATE INDEX idx_media_purge_tombstones_expires
  ON media_purge_tombstones(expires_at, media_id);

CREATE TABLE storage_volumes (
    kind TEXT PRIMARY KEY CHECK (kind IN ('app', 'media', 'uploads')),
    volume_id TEXT NOT NULL UNIQUE,
    active_tusd_boot_id TEXT,
    recorded_at INTEGER NOT NULL,
    CHECK (kind = 'uploads' OR active_tusd_boot_id IS NULL)
);

```

Indexes on `(status, next_attempt_at, created_at)` and `(status, lease_until)`
support due claims and expired-lease recovery; the attempt-control expiry index
supports bounded retention batches. Result-media indexes serve the nonterminal
purge-pin predicate and tombstone-GC reference check without scanning job
history; retained jobs are never bulk-updated by purge. Separate
`completed_at`/`discarded_at` partial indexes
serve bounded artifact-clean terminal GC. The action/media/time audit index
narrows versioned purge-oracle lookups before exact token/filename detail
validation, while tombstone primary-key/expiry indexes serve late cleanup and
bounded proof GC. Purge holds a media stripe and SQLite writer only across these
indexed bounded operations, never a full-table scan.

Every `INTEGER` time in migration 0004 is signed UTC Unix microseconds. The
store obtains one `nowMicros` per transaction (`time.Now().UTC().UnixMicro()`),
adds durations with checked 64-bit arithmetic, and binds integers; SQL compares
them numerically. Null means unset. No queue/control/storage-volume deadline or
timestamp uses RFC3339 text, local time, floating Julian days, or mixed units.
Existing `media_items` timestamp encoding remains unchanged because it is
outside these lease/due indexes.
All lease/grace/deadline ownership is valid while `nowMicros < until`; due or
expired predicates become eligible at `nowMicros >= until`.

`client_upload_id` is a stable browser lifecycle; each outer no-URL create gets
a monotonically increasing attempt number and a fresh physical tus `upload_id`.
The creation transaction atomically sets the prior current attempt to
`is_current=0`/`cancelling` and inserts the new current row. Older attempts
remain durable but cannot promote or publish. This prevents a delayed
`NewUpload` from truncating a newer attempt because physical IDs/paths are
never reused.

Terminal rows are retained for the configured 30-day status/idempotency window,
then removed in bounded batches only when `physical_slot_active=0` and
`artifacts_cleaned_at IS NOT NULL`. This provides a compact processing record
without deleting the ownership proof for a retained source or media temp.
Lifecycle closure does not depend on that row: the publication transaction
atomically sets `closed_at`/`terminal_outcome` to `published` or `duplicate`,
and a winning cancellation or current-attempt terminal-failure transaction sets
`cancelled` or `failed`. Discard of a superseded noncurrent physical attempt
never closes the lifecycle containing its newer current attempt. Registration
and pre-create return 410 whenever the lifecycle is closed or its immutable
`client_admissible_until` has arrived, even after the terminal job is removed.

### Storage identity

Each persistent mount root contains a private, synced
`.event-gallery-volume-id` file with a random UUID and expected kind. The
matching IDs are recorded in `storage_volumes`.

`.event-gallery-volume-id` is a reserved control name, never gallery or tus
evidence. Every initializer inventory, fresh-root check, periodic upload-root
inventory, sidecar reconciler, physical-upload counter, and incomplete janitor
skips that exact basename before classification/counting. Similar names are not
special. The initializer additionally owns the closed pattern
`.event-gallery-volume-id.init-<safe-random-token>`; scanners ignore it, but
only the stopped-stack initializer may remove or recover regular, non-symlink
files in that namespace.

The upload root has a second closed control namespace for tusd's PID locker:
`<safeUploadID>.lock`, `<safeUploadID>.stop`, and
`<safeUploadID>.lock.<bounded-safe-temp-suffix>`. Initializer/fresh-root scans,
periodic inventory, reconciliation, janitors, and physical-upload counting
recognize only regular, non-symlink entries matching those exact forms and
treat them as neutral: never mount evidence, gallery/tus data, or a physical
upload slot. `statfs` still reflects their real bytes/inodes. Malformed IDs,
unsafe suffixes, wrong file types, and lookalikes remain ambiguity that refuses
binding/cleanup. The app and initializer never unlink this namespace; only the
identity-authorized tusd startup scrub below may do so.

The upload root also reserves one app-owned sidecar-repair temp form:
`.event-gallery-ingest-<safeUploadID>-<safeRepairToken>.info.tmp`. Repair first
requires no prior artifact token, then atomically stores the same fresh random
value as active `sidecar_repair_token` and durable
`sidecar_repair_artifact_token` plus a numeric lease. Under the upload stripe it
rechecks all three fields and opens the contained regular non-symlink temp with
`O_CREATE|O_EXCL`. It writes bounded canonical metadata, syncs/closes the file,
atomically renames it to `<uploadID>.info`, and syncs the upload directory.
Initializer/runtime inventories and physical counting recognize the form only
when the upload ID and basename token match the candidate row's durable artifact
token; active lease presence is not required. It is neutral
for logical slot/evidence accounting but its bytes/inode remain in `statfs`.
An unowned token, malformed name, symlink, wrong type, oversized file, or
lookalike remains ambiguity and refuses binding/cleanup.

After upload identity matches, the runtime reconciler owns this namespace. It
promotes a complete validated matching temp when the target sidecar is absent,
or removes and directory-syncs an invalid/redundant temp only after the durable
artifact-token recheck plus stripe acquisition and active-lease invalidation
prove no repair can publish it. It clears `sidecar_repair_artifact_token` only
after temp absence or successful rename is directory-synced and target state is
revalidated; a crash before that database clear remains recoverably owned.

Every transition out of `uploading` waits for the stripe and clears only the
matching active repair token/lease in its state transaction; SQL forbids carrying
that active lease into another state, while durable artifact ownership survives
until cleanup above. Before any new repair token is allocated, the prior artifact
token must be resolved and cleared. The stopped-stack initializer
never interprets it as tus data or mount evidence; it leaves an owned temp for
post-binding recovery and may remove only a target-redundant owned temp when the
existing upload identity already matches. Thus a crash in sidecar temp-write
cannot strand an unclassifiable root, while a planted basename cannot bless or
clean an unrelated mount.

Sentinels are never auto-created during normal startup. Deployment includes an
explicit one-shot `server init-storage-identities` command that an operator runs
with the stack stopped and all three configured bind mounts attached. It refuses
symlinks/non-directories and cross-checks existing state before writing:

Before opening SQLite, inventorying any root, or writing a sentinel, every
initializer/adoption/replacement mode validates or no-replace-publishes and
parent-syncs the exact app-data `.event-gallery-app-instance.lock`, acquires its
exclusive nonblocking `flock`, and holds the descriptor until command exit. A
held lock returns a stable `app instance still running on this app root` error
without reading/mutating identities. This is the executable stopped-stack gate
for new binaries; first-upgrade orchestration additionally verifies the legacy
containers are stopped because they predate this lock.

- it first performs read-only, bounded inventories of validated purge stages
  and legacy tus sidecars. It does not restore, finalize, adopt, or delete
  anything before identities are bound;
- if `media_items` is non-empty, a streaming full-table scan verifies every
  contained `stored_filename` as a regular live original, or exactly one valid
  purge manifest with matching media ID/stored filename and a contained regular
  staged original of the row's size. A valid stage whose row is already absent
  is accepted only when the candidate app database contains the authoritative
  purge-commit proof described below; otherwise it is ambiguous, remains
  untouched, and refuses binding. Malformed, duplicate, mismatched, or
  ambiguous stages refuse binding;
- job/path cross-checking is state-aware. Existing data/sidecar paths for any
  retained row must be contained regular files and match its physical ID,
  metadata, size, and allowed one-/two-sided shape. `uploading`, `pending`, and
  `processing` normally own a source; an absent uploading source is expected
  before expired creation grace/lease when no write began, while pending/
  processing may instead have a validated prepared final/original temp. Any
  other absence in those states is recorded only as a post-binding reconcile
  input, never terminalized by the initializer;
- `cancelling`, `cleanup`, `discarding`, `complete`, and `discarded` are
  source-optional during binding because creation may never have materialized,
  termination may have removed zero/one/both paths, or late artifacts may await
  cleanup. Present paths are still validated; absence neither satisfies nor
  defeats upload-mount identity;
- source-optional/absent rows cannot authenticate a first-time upload mount. If
  no matching `storage_volumes` upload UUID/sentinel pair already exists, the
  candidate root needs at least one affirmative validated job or legacy tus
  artifact. Otherwise initialization refuses with `insufficient upload-mount
  evidence`, rather than binding an arbitrary empty root. Once independently
  authenticated, all optional/absent states are left unchanged for normal
  reconciliation;
- safe rowless pre-upgrade data/sidecar pairs and one-sided valid sidecars are
  accepted as upload-mount evidence after bounded sidecar parsing, path/ID
  containment checks, regular-file checks, and offset/size validation. A
  rowless data-only or otherwise unclassifiable artifact refuses binding;
- an existing app database must reside beside the app sentinel being created;
- an apparently fresh deployment is allowed only when the database has no
  media/jobs and both media/upload roots contain no gallery artifacts. Valid
  purge or legacy-tus evidence instead selects the existing-install path.

A drained pre-upgrade installation can legitimately have media rows but no tus
artifacts. The stopped-stack command therefore supports explicit
`--adopt-empty-upload-root`. It is accepted only for a first upload-volume bind
when `upload_jobs` has zero rows, all app/media evidence and every media original
or purge-stage proof validate independently, and the complete upload-root
inventory contains no data, `.info`, or unclassifiable entry after excluding
the reserved initializer/sentinel and safe locker controls. Any tus artifact,
job row, malformed control, or app/media validation failure rejects the flag.
The command logs a stable high-visibility operator-adoption event and README/
rollout instructions name this as the normal upgrade command for a drained
legacy upload root. The flag supplies explicit operator provenance, not inferred
filesystem evidence; sentinel publication then uses the same no-replace/fsync
protocol.

Replacing a lost/recreated upload volume is a separate explicit operation:
`--replace-upload-root --expected-old-upload-uuid <uuid>`. It is accepted only
when the supplied UUID exactly matches the existing `storage_volumes('uploads')`
row, app/media identities and all media evidence validate, and the candidate
upload root is logically empty under the same strict inventory used by
`--adopt-empty-upload-root`. Without an additional loss acknowledgement, no
upload job may be in `uploading`, `cancelling`, `pending`, `processing`,
`cleanup`, or `discarding`; retained `complete`/`discarded` history rows are
allowed. A candidate with any sentinel (matching or foreign)
uses the normal match path or refuses, except the crash-resume case below;
initial replacement only publishes into an absent final sentinel path. The
replacement sentinel's bounded versioned record includes
`replaces_uuid=<expected-old-uuid>` in addition to kind/new UUID.

If the old upload volume is irrecoverably lost while nonterminal jobs remain,
the operator must add `--acknowledge-lost-upload-data`. The command prints every
affected upload ID/status plus whether a validated prepared final/original-temp
candidate exists, requires an explicit confirmation, and records the loss mode
and counts in the replacement audit. It still requires the empty candidate and
fully valid app/media evidence but does not mutate job states. After binding,
startup invalidates old leases: cleanup/discard/cancelling rows finish their
state-specific absence verification; jobs with a validated prepared candidate
resume publication; never-written uploading rows become creation-orphan
cancelled; and written uploading/pending/processing rows without another copy
use the durable two-scan source-loss protocol and become public failed only
after its barriers. No row is silently discarded by the initializer.

After no-replace/fsync publication of the candidate sentinel, one immediate
transaction conditionally updates the uploads UUID from the expected old value
and inserts an `ActionConfig` audit containing old/new UUID and the explicit
replacement reason. A race or changed old UUID rolls back. A crash before the
transaction leaves a complete new sentinel; the same flagged command adopts it
only when its kind and `replaces_uuid` exactly match the candidate database's
still-current expected old UUID and the root remains logically empty. Any
sentinel without that provenance or with a different predecessor is foreign and
refuses. A crash after commit is already converged. Unflagged runs
never rewrite an identity. README remediation prints the recorded old UUID and
the exact stopped-stack command; no SQLite hand-editing is required.

The fixed media-directory basenames are structural entries, not gallery
artifacts by themselves. During fresh/partial-run classification they must be
contained real directories; empty `originals`, `thumbnails`, `previews`,
`.purging`, and `.ingest-locks` are accepted. Their recognized contents are
classified normally, and any symlink, wrong file type, unexpected entry, live
artifact without database evidence, or malformed stage still refuses binding.
This lets a rerun recover after directory creation but before identity commit
without making a populated wrong media mount look fresh.

Media-root classification is also closed and state-aware:

- `.ingest-locks` may contain only regular non-symlink empty files named
  `stripe-<index>.lock`, where the decimal index is canonical and below
  `INGEST_LOCK_STRIPES`, plus exactly one `capacity.lock`. These fixed controls
  are structural neutral entries:
  never media evidence or ambiguity, never removed by the initializer, and
  never counted beyond the configured fixed inode charge. Missing stripe files
  are created/synced by normal fixed-layout setup; out-of-range, duplicate-form,
  nonempty, wrong-type, or lookalike entries refuse binding;
- recognized `.ingest-<mediaID>-<leaseToken>-<kind>.tmp` regular files inside
  originals/thumbnails/previews are matched to the candidate database job.
  Matching job-owned temps are validated and left for post-binding recovery or
  cleanup; a valid original-kind temp/final can be affirmative media evidence
  for that job. An unowned original-kind temp is potentially authoritative and
  refuses binding for manual inspection;
- unowned derivative temps and unowned final thumbnails/previews are
  non-authoritative post-binding cleanup input only when the media mount is
  independently authenticated by all database originals/purge proofs. They do
  not authenticate a mount or qualify it as fresh, and the initializer never
  removes them. By default an unowned regular file in `originals` refuses
  binding;
- any unsafe ID/token/kind, unrecognized basename, symlink, directory in an
  artifact leaf, or conflicting path shape refuses binding.

For the verified pre-upgrade failure window, the stopped-stack initializer
supports explicit `--accept-legacy-orphan-originals`. It is accepted only for a
first media-volume bind when `upload_jobs` has zero rows, every database media
row has a valid live/staged original proof, and no other media-root ambiguity
exists. Each orphan must be a contained non-symlink regular positive-size file
with the legacy UUID-plus-allowed-extension shape and allowlisted magic; the
initializer hashes it for a stable validation report but does not log the hash.
It logs every exact path/size/type and a high-visibility operator-adoption event,
then leaves the files untouched. Generic runtime reconciliation never unlinks
an unowned regular original; these accepted legacy orphans remain protected for
manual inspection/recovery. The flag does not accept original-kind ingest temps
or unsafe/unrecognized originals. Without the flag, the same files continue to
refuse binding.

The command publishes and syncs every sentinel and mount root first, then
records/verifies all matching IDs in one immediate SQLite transaction. That
transaction repeats every dynamic database predicate used by the selected mode:
fresh/adopt-empty/legacy-orphan modes recheck their required zero row/media/job
conditions; planned replacement rechecks expected old UUID and zero
nonterminal rows; loss-acknowledged replacement requires the exact confirmed
set of nonterminal upload IDs, statuses, prepared markers, and result IDs to be
unchanged. Any mismatch rolls back the UUID/audit commit and leaves the newly
published sentinel resumable under its provenance rules; no unconfirmed row is
orphaned. Filesystem inventories remain protected from app mutation by the held
lifetime lock.

Before identity commit, the stopped-stack initializer also establishes a
power-loss barrier for every existing live or purge-staged original accepted as
proof for a `media_items` row. Through the already validated no-follow regular-
file descriptor it syncs the file, then syncs its containing directory and each
fixed directory ancestor through `MEDIA_DIR`; it finally rechecks the row/path
and same-file stat. Any sync/recheck error refuses initialization and leaves all
copies/stages untouched. This one-time barrier makes legacy originals durable
before the new runtime can use them as duplicate authorities; originals created
by the new publication flow already receive equivalent barriers.

The same pre-commit phase establishes durability for every present validated
job-owned or rowless legacy tus artifact accepted as upload evidence or
post-binding recovery input. It opens each contained regular data file and
`.info` sidecar with no-follow semantics, revalidates the sidecar's bounded
upload ID/size/storage paths and current data size, syncs each present file,
then syncs `TUS_UPLOAD_DIR` once after the batch. Under the held app-instance
lock it finally re-stats every path against its open descriptor and repeats the
sidecar/data/row association check. One-sided valid sidecars receive the same
sidecar/root barrier; absent optional paths are not fabricated. Any file/root
sync, same-file, parse, size, path, or association failure aborts before
sentinel/identity/migration commit and leaves every artifact unchanged. This is
a stopped-stack durability operation, not adoption or namespace mutation;
normal startup may still defer large-file fsync because the initializer has
already anchored every accepted legacy source.
`deploy/app-entrypoint.sh` forwards arguments with
`exec /app/server "$@"`, so operators can run the initializer through the
shipped image/Compose service without an entrypoint override. README/rollout
instructions make this one-time stopped-stack step mandatory for existing
installations.

The command is idempotent and resumable. For each root, an absent sentinel gets
a new UUID/kind; an existing contained, non-symlink regular sentinel with valid
syntax and the expected kind is adopted unchanged. In one immediate database
transaction, `storage_volumes` inserts missing kind/UUID pairs and verifies
existing rows match exactly. A conflicting sentinel kind, duplicate UUID across
roots, or existing database row with a different UUID refuses binding without
rewriting either side. Because sentinels are synced before that transaction, a
crash after any subset of file writes or before/after commit converges on the
next identical command invocation.

Sentinel creation never writes the final path directly. The initializer creates
an exclusive temp in its reserved namespace, writes the complete bounded
kind/UUID record, fsyncs and closes it, atomically publishes it to the absent
final basename with no-replace semantics, and fsyncs that mount root. A crash
before publication leaves only a removable temp; a crash after publication but
before root sync leaves either no final or a complete file-synced final, both
safe on rerun. Existing malformed/conflicting final sentinels are never
overwritten. On rerun, after validating the root/final state, the initializer
removes only its recognized temp files and syncs the root. Every final sentinel
and all three mount roots must be successfully fsynced before the immediate
`storage_volumes` transaction begins; therefore a committed UUID can never
depend on an unflushed directory entry.

The initialized persistent media layout contains `originals`, `thumbnails`,
`previews`, `.purging`, and `.ingest-locks`. The initializer creates any missing
fixed directory, syncs it, and syncs `MEDIA_DIR` so each new child entry is
durably anchored before it records readiness-capable identities. Normal startup
with valid identities verifies/repairs and syncs this fixed layout before
readiness. Purge never lazily creates `.purging`; per-item stage creation syncs
the new stage and `.purging` before moving any live artifact.

The app-data layout contains one empty regular
`.event-gallery-app-instance.lock`, created/no-replace-published and parent-
synced like other fixed controls. It is never application evidence and is never
removed during normal operation. As the first server action, before opening
SQLite, running migrations, binding the listener, checking identities, or
enabling background work/routes, the process validates/creates and acquires an
exclusive nonblocking `flock` on that file and holds its open descriptor for
its entire lifetime, including missing-identity degraded mode. Failure means
another app instance owns the same app root: the contender stays nonserving and
exits/retries; it never reclaims leases. A suspended process retains the kernel
lock, while process death releases it. This enforces the documented singleton
across rollout overlap instead of relying only on operator sequencing.

Normal startup never creates or rebinds identities. Its matrix is explicit:

- unbound fresh/pre-0004 schema: after singleton acquisition, open read-only/
  no-migrate, diagnose, log the initializer command, and exit nonzero without
  binding any listener or starting background work;
- current schema with `storage_volumes` present but a missing/mismatched/
  unreadable sentinel: bind the HTTP listener and shallow SQLite `/healthz`,
  keep `/readyz` false, upload/status/media-file/admin-mutation routes at
  retryable 503, and disable all background/destructive/absence work. Read-only
  gallery/admin metadata may remain available for diagnosis;
- current schema plus matching identities/fixed layout: acquire all dependency
  gates, authorize current tusd, run inventory, then become ready.

A stable remediation log names the exact initializer/replacement command for
each non-ready state. Once matching sentinels are present, the dependency circuit
closes without changing lifecycle state.

Before readiness, admission/statfs, preparation, publication, cleanup,
discard, source-loss classification, or any path-absence conclusion, the app
requires safe regular sentinels whose IDs/kinds match SQLite. Missing,
mismatched, symlinked, or unreadable identity opens the relevant dependency
circuit and changes no lifecycle state. Stopped-stack backups include all
sentinels. This prevents an empty fallback mountpoint from certifying that a
source or artifact is absent.

### Ingest manager

Create `backend/internal/ingest`. Its manager owns:

- validation and idempotent enqueueing;
- durable upload reservations and capacity admission;
- a non-blocking wake channel plus periodic polling;
- a configurable fixed-size worker pool;
- transactional claims and lease expiry;
- processing retry scheduling;
- cleanup retry scheduling;
- cancellation drain from `cancelling` to leased `discarding` after create/write
  leases expire, while retaining incoming capacity until transport fencing and
  directory-synced source absence;
- a bounded durability executor and queue for pre-finish fsync/promotion;
- complete-sidecar reconciliation at startup and periodically;
- stale ingest-temporary and unowned-artifact cleanup;
- orphan reservation maintenance and terminal-row retention;
- graceful shutdown and worker waiting.

The HTTP server receives the manager as a dependency. Public media routes keep
using the media processor directly for artifact paths.

Default configuration:

| Variable | Default | Purpose |
| --- | ---: | --- |
| `MEDIA_PROCESSING_WORKERS` | `2` | Maximum concurrent ingest jobs |
| `HEIC_CONVERSION_MAX_FAILURES` | `6` | Conversion failures before fallback publication |
| `MEDIA_PROCESSING_TIMEOUT_MINUTES` | `60` | Deadline for one processing attempt |
| `PREVIEW_MAX_DIMENSION` | `2560` | Longest JPEG preview edge |
| `UPLOAD_RESERVATION_IDLE_MINUTES` | `30` | Release idle capacity reservations |
| `UPLOAD_CREATION_GRACE_SECONDS` | `120` | Suppress orphan repair while tusd creates paths |
| `TUS_UNCERTAIN_CREATE_ROLLOVER_COUNT` | `100` | Drain/rotate tusd before ambiguous creates exhaust physical slots |
| `UPLOAD_RESERVATION_MAX_COUNT_PER_IP` | `max(2 * UPLOAD_CONCURRENCY_PER_IP, 100)` | Bound cheap create-only reservations |
| `UPLOAD_RESERVATION_MAX_BYTES_PER_IP` | `UPLOAD_CONCURRENCY_PER_IP * MAX_UPLOAD_BYTES` | Match expected bytes to venue concurrency |
| `UPLOAD_CREATE_RATE_LIMIT_PER_MINUTE` | `max(2 * UPLOAD_CONCURRENCY_PER_IP, 120)` | Separate tus create rate |
| `UPLOAD_ADMISSION_TIMEOUT_SECONDS` | `5` | Bound pre-create admission below hook timeout |
| `INGEST_MIN_FREE_BYTES` | `2 * MAX_UPLOAD_BYTES` | Free-space floor on involved filesystems |
| `INGEST_MIN_FREE_INODES` | `10000` | Filesystem inode safety floor |
| `TUS_PHYSICAL_MAX_UPLOADS` | `50000` | Bound retained tus upload artifacts |
| `APP_DATA_MIN_FREE_BYTES` | `1073741824` | SQLite/WAL free-space floor |
| `UPLOAD_JOB_MAX_ROWS` | `250000` | Hard upload-job/history admission cap |
| `UPLOAD_CONTROL_MAX_ROWS` | `1500000` | Separate lifecycle plus attempt-control cap |
| `UPLOAD_JOB_RETENTION_DAYS` | `30` | Retain complete/discarded status rows |
| `UPLOAD_CLIENT_LIFECYCLE_DAYS` | `30` | Maximum browser ledger lifecycle age |
| `UPLOAD_CONTROL_RETENTION_DAYS` | `45` | Retain server attempt/lifecycle cancellation controls |
| `MEDIA_PURGE_TOMBSTONE_RETENTION_DAYS` | `45` | Minimum purge proof retention after media deletion |
| `UPLOAD_STATUS_RATE_LIMIT_PER_MINUTE` | `6000` | Separate batched-status limit per IP |
| `INGEST_RECONCILE_INTERVAL_SECONDS` | `15` | Sidecar, lease, and artifact recovery cadence |
| `INGEST_RECONCILE_PAGE_ENTRIES` | `2000` | Maximum sidecars per periodic page |
| `UPLOAD_DURABILITY_WAIT_SECONDS` | `75` | Total pre-finish/fence response budget below outer timeouts |
| `UPLOAD_DURABILITY_WORK_TIMEOUT_MINUTES` | `10` | Detached idempotent fsync/promotion deadline |
| `UPLOAD_DURABILITY_WORKERS` | `2` | Maximum concurrent source fsync operations |
| `UPLOAD_DURABILITY_QUEUE_SIZE` | `100` | Bounded completion backlog |
| `TUS_NETWORK_TIMEOUT_SECONDS` | `90` | Keep tusd's PATCH context alive through the hook timeout |
| `TUS_TRANSPORT_LEASE_SECONDS` | `45` | Persist exact lock-taking request ownership across app death |
| `TUS_TRANSPORT_HEARTBEAT_SECONDS` | `10` | Renew active HEAD/PATCH/internal-DELETE ownership |
| `IMAGE_MAX_SOURCE_PIXELS` | `50000000` | Pure-Go decode admission limit |
| `IMAGE_DECODE_MEMORY_BUDGET_BYTES` | `536870912` | Shared weighted decode semaphore |
| `HEIC_MAX_SOURCE_PIXELS` | `100000000` | Primary-image decode safety limit |
| `HEIC_CONVERTER_MEMORY_BYTES` | `1073741824` | Per-converter address-space limit |
| `MEDIA_TOOL_MAX_SOURCE_PIXELS` | `100000000` | ffmpeg/ffprobe decoder pixel limit |
| `MEDIA_TOOL_MEMORY_BYTES` | `1073741824` | Per ffmpeg/ffprobe address-space limit |
| `MEDIA_TOOL_MAX_THREADS` | `2` | Bound each external media process |
| `MEDIA_TOOL_LOG_BYTES` | `65536` | Cap captured stdout/stderr per process |
| `INGEST_LOCK_STRIPES` | `1024` | Bound per-upload/media cross-process lock stripes |

Retry delays are persisted, exponential, and capped at 15 minutes. Workers do
not sleep for a job; they store `next_attempt_at` and return to the queue.
HEIC environmental conversion retries use a separate short persisted schedule
and stop after `HEIC_CONVERSION_MAX_FAILURES`; deterministic input/codec
failures do not wait through that schedule. Queue residency, unrelated core
retries, lock reschedules, and restarts never consume conversion attempts.

Terminal `complete` and `discarded` rows older than
`UPLOAD_JOB_RETENTION_DAYS` are deleted in bounded batches after the maximum
hook-retry and browser-status windows, with predicates
`physical_slot_active=0 AND artifacts_cleaned_at IS NOT NULL`. SQLite reuses
their free pages. Before
new reservations, maintenance runs due terminal/control deletion and eligible
attempt-control compaction. It checks job and control counts separately and
rejects admission only for the exhausted cap or app-data free-space floor,
preventing queue history and WAL growth from exhausting `/data/app` without
letting controls consume the job budget.

Startup uses checked arithmetic and requires
`UPLOAD_JOB_MAX_ROWS > TUS_PHYSICAL_MAX_UPLOADS`,
`UPLOAD_CONTROL_MAX_ROWS >= 6 * UPLOAD_JOB_MAX_ROWS`, and
`TUS_UNCERTAIN_CREATE_ROLLOVER_COUNT < TUS_PHYSICAL_MAX_UPLOADS`. The factor-six
default budgets one lifecycle plus an average of three registered attempts over
the longer 45-day control horizon relative to 30-day jobs; it is sizing margin,
not permission to drop live controls. README capacity planning requires both
caps to exceed, with venue margin, the measured peak job attempts in the job
retention window and the measured lifecycle-plus-attempt registrations in the
control window. Metrics and stable logs alert at 70/85/95 percent and name the
exact environment variable; reaching a cap is deliberate retryable 503
backpressure, not an unexplained permanent outage. Operators may raise a cap
and restart after app-data sizing, but no row is hand-deleted.

Attempt-control retention is state-aware. While a lifecycle is open, every
registered attempt row remains through its fixed control horizon. Once the
lifecycle closure transaction commits, bounded maintenance may delete its
attempt-control rows immediately because the retained lifecycle tombstone alone
rejects every replay/higher attempt and serves cancellation idempotency; delayed
hooks then fail closed before allocation. It never deletes the lifecycle row
before fixed `expires_at`, never compacts controls for an open lifecycle, and
does not require terminal job GC. This keeps one long-lived control row per
closed lifecycle while preserving full attempt evidence for resumable work.

`MEDIA_PURGE_TOMBSTONE_RETENTION_DAYS` must exceed
`UPLOAD_JOB_RETENTION_DAYS` plus the maximum hook/reconcile/cleanup margin.
Tombstones are deleted in bounded expiry-index batches only when their fixed
expiry has arrived, no `upload_jobs.result_media_id` references the media ID,
and no indexed/final purge stage carries it. If cleanup ambiguity retains a job,
its tombstone remains regardless of age. Purge tombstones count toward app-data
free-space pressure and expose count/oldest-retained metrics; their cardinality
is at most one per purged media ID, never one per duplicate job.

### Capacity admission

The pre-create hook allocates the tus upload ID and media ID, then inserts an
`uploading` row in an immediate transaction. It captures involved-filesystem
`statfs` snapshots while holding the dedicated fixed cross-process
`MEDIA_DIR/.ingest-locks/capacity.lock`,
then enters the immediate transaction and compares bounded reservation
aggregates to those snapshots. No filesystem I/O or maintenance batch runs
while the SQLite write lock is held.

Initial create reserves full expected incoming size and full projected media
size. Pending, processing, and cleanup jobs retain their media reservation
through terminal source cleanup. Physical progress never decreases an active
initial reservation, so bytes written after a snapshot remain covered rather
than creating a snapshot/ledger race. This is intentionally conservative
because `statfs` already reflects written bytes.
All reservation activation, deactivation, and terminal release use the same
capacity lock; a stale snapshot can therefore only under-admit. If upload and
media paths share a backing filesystem, both full obligations are charged to
that filesystem.

Before MIME is known, each active reservation uses a worst-case media-byte
obligation of `expected_size + derivative_reserve`. `derivative_reserve` is an
overflow-checked upper bound of four bytes per pixel for both configured
preview and thumbnail bounding squares plus 1 MiB of encoder overhead. The
pipeline creates derivative temporaries sequentially. Each accepted upload also
reserves one physical-upload slot, five upload-mount inodes (data, sidecar,
lock, stop, one app sidecar-repair temp), and four media-mount inodes (original, preview, thumbnail, one
temporary). Fixed directories, `INGEST_LOCK_STRIPES` stripe files, and the one
capacity-lock file are charged at startup.

The media bound depends on an enforced phase invariant, not an average: before
final-original publication there is at most one original temp and no derivative
work; rename turns that inode into the final rather than adding a copy. The
single-temp resolution gate clears/promotes every prior candidate before another
copy, and derivative generation starts only after the final exists with no
original temp. Derivative temps are sequential. Thus `expected_size` covers
exactly one original temp-or-final and `derivative_reserve` covers all final/
temporary derivative bytes, while the four media inodes cover original,
preview, thumbnail, and the one current phase temp. Any observed second original
candidate is recovery ambiguity: `statfs` already reflects it, admission closes
conservatively, and no worker creates another until the set converges.

The five upload inodes are a hard bound under the proxy operation gate. Tusd's
canonical `.lock` is a hard link to the one creator temp, so both names share
one inode; with no second acquirer, no concurrent extra temp exists. Data and
`.info` consume two more, and one optional stale/holder-release `.stop` consumes
the fourth; the closed app repair namespace contributes at most one fifth inode
because exact row/token/stripe ownership serializes repairs. Each single lock attempt cleans its temp, and identity-authorized
startup removes crash leftovers before serving. Directory entries/bytes still
appear in `statfs`; safe locker controls are neutral only for logical slot
counting, not free-space accounting.

The creation transaction sets `physical_slot_active=1`, charging one physical
upload slot and five upload inodes independently of byte reservations and job
status. Successful proxy completion records `create_finished_at` after
validating the resource; any positive `write_started_at` also proves creation
finished. Reconciliation may also set it when, after create lease/grace expiry,
the safe data and `.info` paths form a validated matching tus resource even at
offset zero; this recovers commit-before-lost-201 without inferring completion
from path absence. A terminal cleanup may clear the slot under the capacity lock only
after both tus paths are directory-synced/absent and creation is proven
finished. A never-written terminal row with unknown creation completion keeps
the slot charged even while paths are absent.

The proxy owns the outer POST lifetime. It records
`create_concluded_at=nowMicros` only after receiving a complete upstream tusd
HTTP response, of any status, proving that handler can no longer enter or resume
`NewUpload`. A cancelled client request, proxy timeout, incomplete response, or
upstream network error is ambiguous and never records conclusion: tusd may have
returned from pre-create but be paused before context-insensitive filestore
creation. The enabled authenticated nonblocking `post-create` hook is separate
positive proof that `NewUpload` and `GetInfo` returned; after path validation it
sets `create_finished_at` and wakes cleanup even if the 201 was lost.

After create lease/grace expiry, a safely concluded same-boot attempt whose
data/sidecar are absent across upload-directory sync and post-sync revalidation
cannot later materialize from that completed handler. Cleanup may clear its
physical slot under the capacity lock. Ambiguous outcomes remain charged until
a different authorized tusd boot plus complete inventory/absence fencing; late
artifact discovery still atomically reactivates the row/slot.

Tusd's authorized startup sends a random boot ID, which the app commits as the
current boot before scrub/start; every new job records it as
`create_tusd_boot_id`. Once a different authorized boot is current, no blocked
`NewUpload` from the prior boot can resume. After a complete inventory and
verified cleanup/absence, maintenance may clear old-boot uncertain liabilities
under the capacity lock. Terminal retention never deletes a row while
`physical_slot_active=1`.

The maintenance loop owns a paginated upload-root inventory initialized before
readiness and refreshed every reconcile interval. Its conservative cache counts
each distinct physical upload ID with recognized data/sidecar artifacts that is
not already charged by a row's `physical_slot_active`, including rowless IDs and
artifacts attached to terminal rows. Admission performs no directory
enumeration: it sums active row slot obligations and that exception cache. A
late terminal artifact discovery atomically reactivates the row's slot before
the cache can drop its exception charge and wakes/transitions cleanup; the
charge decreases only after verified termination, directory barrier, and
conditional slot clear under the capacity lock. Expected delayed creation is
already covered by the retained row liability, so cache staleness can only
under-admit.

The inode floor applies only when a filesystem reports inode accounting. When
both `f_files` and `f_ffree` are zero (for example btrfs), the app logs that the
inode gate is unavailable and relies on byte and physical-count limits.
`INGEST_MIN_FREE_INODES=0` explicitly disables the inode check.

The public proxy derives the effective tus operation before routing. It rejects
creation-with-upload and strips/rejects `X-HTTP-Method-Override`, because the
shipped client uses ordinary POST creation followed by PATCH and these alternate
write paths would bypass method-specific controls. Every accepted data write
therefore enters one PATCH gate: atomically validate/adopt and fund the
reservation, set `write_started_at` if null, and claim a random
PATCH operation token stored identically in `write_lease_token` and
`transport_lease_token`, with `transport_operation='patch'` and the current
tusd boot, then apply the existing
PATCH concurrency and bandwidth controls before forwarding. A lightweight
heartbeat extends `write_lease_until` while request-body transfer is active.
Every PATCH renewal is one fenced update requiring both token columns to equal
that operation token, `transport_operation='patch'`, `is_current=1`,
`status='uploading'`, and no cancellation request; it extends both deadlines.
Zero affected rows or a
busy/I/O/timeout renewal error cancels the upstream request immediately,
closes/drains the client body within a bound, and returns retryable 503; the
proxy never relays a later tusd success after ownership is uncertain. The
initial lease covers at least two heartbeat periods, and renewal completes
before half of the remaining lease elapses.
Idle release of either byte reservation requires no unexpired write lease and
an exactly absent `transport_lease_token`; an expired same-boot transport token
is uncertainty, not absence, and remains charged until its owner clears it or
the fresh-boot/two-lock-absence reclaim protocol fences it. Every deactivation
UPDATE includes those predicates. Cancellation is the sole exception for media
accounting: under the capacity lock it clears media accounting but
unconditionally keeps or reactivates the full incoming liability, even if that
temporarily puts admission below its floor. Cancellation itself is never
rejected for lack of capacity. Request completion clears only matching tokens;
crash recovery uses the type-specific fencing rules. A second write for the
same upload receives retryable 423 rather than replacing the lease.

Before any public HEAD or PATCH can reach tusd's file locker, the proxy claims
the row's exact `transport_lease_token`/operation and current
`transport_tusd_boot_id` in an immediate transaction.
That first successful claim also sets `client_bound_at` if null, proving a
client possessed the accepted physical URL; retries preserve it.
An unexpired lease for that physical upload returns 423 with `Retry-After` and
does not forward; unrelated upload IDs never contend. The proxy heartbeats the
lease through the entire upstream request/response, including tusd lock wait
and pre-finish. PATCH claims/renews its write lease atomically with transport
ownership; failure of either heartbeat cancels upstream and never relays later
success. Before request-body EOF, every body read and upstream write remains
conditional on the exact write/transport tokens, active incoming reservation,
`is_current=1`, `status='uploading'`, and no cancellation. Loss or ambiguous
renewal of any predicate closes/drains the client body within the bound,
cancels upstream, and never forwards more bytes.

Final PATCH completion uses an explicit one-way handoff. Once body EOF has been
forwarded, the exact owner sends no more bytes. The pre-finish durability update
requires its still-matching write/transport tokens when they are present, commits
`source_completed_at` plus `pending`, and preserves both tokens. Thereafter the
same proxy request heartbeats/relays only when those exact tokens and current
physical row still match, `source_completed_at IS NOT NULL`, and status is
one of `pending`, `processing`, `cleanup`, `complete`, `discarding`, or
`discarded`; `uploading` is no longer required and cancellation cannot have won.
This completion-derived predicate is mandatory before relaying any upstream 2xx.
Processing or deterministic failure may advance while the response is in flight
without invalidating the handoff.

An upstream non-2xx chosen by pre-finish/hook failure may instead be relayed
under the same exact current tokens while the row is still `uploading`; if a
detached operation commits completion before relay, the completion-derived set
also permits that already-chosen non-2xx. Neither branch turns non-2xx into
success.

Token release coordinates with the durability registry. If no same-token
operation remains after a final response copy/flush attempt, the proxy clears
matching write token/until and transport token/until/boot/operation in one
fenced transaction. If a retryable 503 is relayed while that exact operation is
still active, the registry atomically marks `responseDone` and takes over
heartbeating both deadlines before the proxy stops; the proxy does not clear or
reuse the tuple. The operation may continue only while both columns/operation/
boot still match. On durable promotion, lifecycle-ended, work timeout, or other
terminal operation result, whichever side observes both operation-done and
response-done clears the entire matching tuple exactly once. Client retries see
423 while this bounded owner remains, then status/completion fencing takes over.
A process crash leaves the persisted tuple to the fresh-boot/two-lock-absence
reclaim protocol. A clear failure retains the whole tuple and retries
conservatively rather than changing completion state.

HEAD has no write token. Before durable completion it keeps the ordinary
`uploading` transport predicate and completion fence. Once
`source_completed_at` is committed, a current exact HEAD transport owner may
relay tusd's complete response in any of the six completion-derived states
above, then clears only its matching transport ownership tuple. No completion-
handoff predicate authorizes a body write, noncurrent row,
cancelled-before-promotion row, or stale token.

Only the process holding `.event-gallery-app-instance.lock` may claim,
heartbeat, reclaim, or forward transport operations. Immediately before opening
the upstream request/stream, it rechecks the exact transport token. A suspended
old process cannot resume past this boundary after a replacement exists because
it still holds the singleton lock and prevents that replacement from serving.
If the old process dies, its kernel lock is gone and its persisted transport
lease plus the tusd lock-absence protocol below fence any handler it left behind.

The lease duration is validated at startup to exceed tusd's configured lock-
acquire timeout plus request-completion grace and a safety margin; the default
is 45 seconds for 20 + 10 seconds. After app death/restart, no transport lease
from the same tusd boot is reclaimable, even after expiry or clean lock-path
observations: a handler may be paused before entering `lockUpload`. Reclaim
requires `storage_volumes.active_tusd_boot_id` to be a different freshly
authorized boot than `transport_tusd_boot_id`, then waits through the old bound
and checks the contained canonical `<uploadID>.lock` path twice, separated by
at least the locker holder-poll interval. Only the boot change plus two clean
`ENOENT` observations with healthy authenticated storage allow a conditional
stale-token replacement; any same boot, lock, `.stop`, stat error, or old-token
heartbeat returns 423/503 and retries.

Public DELETE is always consumed by the app as cancellation intent and never
forwarded to tusd, so it does not claim a transport lease. It is a compatibility
alias, not physical-attempt-only cancellation: the app resolves the addressed
retained row's `client_upload_id`, treats the random physical ID as capability,
and runs the same lifecycle/current-row cancellation command as
`POST /api/uploads/cancel`. A stale noncurrent URL therefore cancels the newer
`is_current=1` uploading attempt or returns the same pending/closed result; it
never merely terminates the old row while a newer attempt can publish. The
addressed noncurrent row remains/wakes in its ordinary discard path. Unknown or
expired physical IDs return 404 without revealing lifecycle metadata.

The cleanup/discard
worker claims operation `terminate` under its existing job lease before its
internal tusd DELETE; it does not acquire a long-held hashed stripe around
network I/O. Hook callbacks and the pre-finish durability operation do not
claim transport ownership themselves; they execute inside or join the owning
PATCH and use fenced database transitions. Thus at most one tusd lock-
acquisition loop per exact physical ID exists across app overlap, and proxy
contention cannot create `.stop` or additional lock temps.

The proxy handles public OPTIONS itself (or rewrites tusd's response) and emits
`Tus-Extension: creation,termination` exactly. It does not advertise
`creation-with-upload`, `creation-defer-length`, `concatenation`, or any other
upstream capability the proxy does not implement. `Tus-Version`,
`Tus-Resumable`, and the configured `Tus-Max-Size` remain truthful.

The proxy records `observed_offset` and `last_activity_at` after each successful
PATCH response. This progress write is short and best effort/batched off the
response-critical path; it is diagnostic/recovery state and never reduces
capacity accounting. Reconciliation refreshes offsets from regular files.
Pre-create admission remains a bounded immediate transaction and maps SQLite
busy/I/O contention to retryable 503 with `Retry-After`, not a hook 500. Its
configured timeout fits within tusd's hook timeout; no `statfs` or batch
maintenance runs while holding the write transaction.

User cancellation does not wait for an active writer. The shared lifecycle
cancellation command first applies the same completion fence used by HEAD/PATCH
to the current physical row: durable marker,
in-process durability registry, and validated data-file size. An active
durability operation returns retryable 503 only for its bounded decision window;
the client retries. A committed `pending` or later state returns 409/
cancellation-lost. If the current row is still `uploading`, including a physically
complete but unfunded/recovering source with no active durability owner, one
capacity-locked immediate transaction atomically changes it to `cancelling`,
closes the lifecycle, sets `cancellation_requested_at`, clears only the media
reservation, and keeps or reactivates its full incoming reservation until a
worker has fenced transport and durably removed the source. That
single immediate transaction races the pre-finish `uploading -> pending`
transition: whichever commits first wins. `cancelling` rejects every later
PATCH/HEAD completion attempt; the active PATCH heartbeat observes lost
ownership/cancellation and aborts upstream fail-closed. Actual tus termination
waits for create/write leases to expire and for exact transport fencing. If
`pending` committed first, cancellation
returns 409 and the upload proceeds to publication; it cannot silently reverse
a durable completion.

The hook rejects creation before tusd allocates a file when projected free
space, free inodes, physical tus-upload count, create rate, per-IP count/bytes,
app-data floor, job cap, or control cap would be crossed. A paginated upload-root inventory
cache counts safe data/sidecar pairs and one-sided artifacts even when no queue
row exists. Admission rejection is retryable: rate/count pressure returns 429 with
`Retry-After`, and storage/inode/maintenance pressure returns 503 with
`Retry-After`. The public proxy preserves those statuses so tus-js/Uppy backs
off instead of hard-failing the file.

Reservation maintenance is mandatory and independent of optional
`TUS_INCOMPLETE_RETENTION_HOURS`, but it does not delete a resumable source.
After `UPLOAD_RESERVATION_IDLE_MINUTES` without progress it atomically sets
both reservation-active flags false, releasing accounting while leaving the
row and tus files intact. Before
forwarding any later write, the
proxy validates/adopts the current sidecar if needed and atomically reacquires
incoming bytes as `expected_size - verified regular data-file size` and the
full projected media obligation under current admission checks;
failure returns retryable 429/503 without deleting or advancing the upload.
Successful progress renews the idle deadline.

One narrow class is not considered resumable: a creation-finished row with
`client_bound_at IS NULL`, `write_started_at IS NULL`, no active create/
transport lease, and no lifecycle cancellation. It represents a 201
that was never durably installed/used by a client. After the same
`UPLOAD_RESERVATION_IDLE_MINUTES` window, maintenance commits creation-orphan
discard intent and cleans it even when incomplete retention is disabled. A
bound zero-offset upload retains the ordinary resumable behavior above.

Inactive `uploading` rows remain as durable identities while their tus source
exists, but do not consume byte reservations. Startup requires the physical cap
to fit below the separate job cap and sizes the independent control cap for
retained open attempts plus closed lifecycle tombstones. HEAD/resume remains
valid and a later PATCH reactivates the same row.
Legacy rowless sources are adopted by inventory/reconciliation when row
and control capacity permits and otherwise report `recovering` while admission
remains closed at the limiting physical, job, or control cap.
Termination of incomplete sources remains governed solely by the documented
`TUS_INCOMPLETE_RETENTION_HOURS` policy, including zero disabling it.

Setting that policy to zero deliberately permits incomplete tus files to remain
until client deletion or operator cleanup. Admission still prevents ENOSPC but
may remain unavailable if retained physical uploads reach the byte, inode, or
count floor; this is an explicit operational consequence of disabling source
retention cleanup. Independent job/control caps and alerts can also backpressure
admission if actual retained traffic exceeds configured capacity; this is
observable/remediable, not assumed impossible. Deterministic client failures
still move directly to retryable `discarding` and release their artifacts.

Every completion path requires funded media capacity. Pre-finish promotion,
periodic complete-sidecar promotion, and rowless complete adoption first create
or reactivate an `uploading` row, then require an active media reservation and
transition to `pending` in the same immediate transaction. A normally admitted
upload reuses the full media obligation charged at pre-create; completion never
double-reserves it. Only legacy/rowless adoption or an idle reservation that was
released before a recovered out-of-band completion must reacquire funding. If
that funding is unavailable, the complete source remains inactive `uploading`
(or rowless when the job/control cap is the blocker), status reports `recovering`, and
reconciliation retries after capacity changes. No unfunded complete upload can
be claimed by a worker, but explicit cancellation may win its still-uploading
state when no durability operation is active.

Claims use a single conditional `UPDATE ... RETURNING`; receiving the row is
the sole definition of ownership. Each claim writes a fresh random
`lease_token`. Every later update includes the expected status and token in
its `WHERE` clause and treats zero affected rows as a lost lease.

Explicit multi-statement writer transactions use `BEGIN IMMEDIATE` semantics
so SQLite acquires the write lock before any lookup. The implementation may
apply modernc's `_txlock=immediate` DSN option because every explicit
transaction in this application is a writer. The existing busy timeout stays
enabled. Publication never depends on upgrading a stale deferred read
snapshot.

The database DSN explicitly pins `journal_mode=WAL` and `synchronous=FULL`.
Performance tuning must not lower commit durability because a committed
publication/cleanup or discard transition authorizes deletion of the only tus
source copy.

Database fencing is paired with a fixed namespace of
`INGEST_LOCK_STRIPES` kernel-backed `flock` files under
`MEDIA_DIR/.ingest-locks/`. Domain-prefixed upload or media IDs hash to a
stripe; unrelated collisions serialize only bounded media-namespace critical
sections and the inode count is constant. Whole-file hashing/copying, writing
lease-specific temps, decode/derivative generation, request-body transfer, tusd
lock wait, hooks, and all other network I/O occur outside hashed stripes. Before
a final-path rename/reuse/remove or publication ownership commit, the worker
non-blockingly acquires the required upload/media stripe(s), revalidates exact
status/lease token and open-file/path identity, performs the bounded namespace
operation plus directory sync or fenced publication transaction, and releases.
A busy stripe reschedules without incrementing a failure counter.

Long file validation uses an already-open regular descriptor outside the
stripe, then under the stripe verifies the row/token and that the path still
names the same file/size/metadata before accepting the result. Cleanup/discard
uses a stripe only around final-artifact ownership checks/removal, releases it,
then uses the exact transport lease for tus termination/path verification, and
finally takes capacity alone for the terminal transition/reservation release.
No hashed stripe spans source termination or a terminal capacity transaction.
Kernel ownership vanishes on process death; database tokens remain the state
fence, while the stripe closes short filesystem check-to-rename/unlink races.

The same fixed layout contains one empty regular `capacity.lock`, used only for
the global capacity `flock`; it is never hashed as a job/media stripe. Hashed
stripes and capacity are never nested. Among hashed stripes, upload precedes
media and equal indexes are deduplicated; no path acquires upload after media
or re-enters a stripe. Capacity-only admission/release cannot cycle with media
namespace work.

Duplicate publication may hold the job's upload stripe and then the
authoritative row's media stripe. Purge holds at most one media stripe, and no
path acquires an upload stripe while holding a media stripe. A bulk purge never
pre-acquires a stripe set; it completes and releases one media ID before the
next. Equal upload/media stripe indexes in duplicate processing are
deduplicated. This one-way order avoids cross-process lock cycles.

### Job state machine

```text
pre-create -> uploading
uploading -- superseded never-written create --> cancelling
uploading -- durable pre-finish/recovery --> pending
uploading -- user cancellation intent --> cancelling
cancelling -- write/create lease expiry --> discarding
uploading -- orphan repair --> discarding
pending -- fenced claim --> processing
processing -- retryable failure/expired lease --> pending
pending|processing -- definitive source loss --> discarding
processing -- publication/duplicate transaction --> cleanup
processing -- exhausted HEIC conversion + fallback publication --> cleanup
processing -- deterministic client validation --> discarding
cleanup -- retryable failure/expired lease --> cleanup
cleanup -- verified tus termination --> complete
complete -- late creation artifacts + slot reactivate --> cleanup
discarding -- retryable failure/expired lease --> discarding
discarding -- verified tus termination --> discarded
discarded -- late creation artifacts --> discarding
```

Claims record a lease longer than the per-attempt timeout. Counters increment
when a specific failure is persisted, not when a job is claimed. Core
processing, cleanup, and discard failures retry indefinitely with capped
backoff; only HEIC derivative conversion has a finite budget because exhaustion
publishes the preserved original. Queue residency and database failures cannot
consume that conversion budget.

On startup, the singleton app invalidates only worker `lease_token` claims,
returns all `processing` jobs to `pending` immediately even when their
wall-clock worker lease has not expired, and makes `cleanup`/`discarding` jobs
immediately claimable. This avoids a one-hour redeploy stall. Create, write,
and exact transport tokens are not blanket-cleared: they retain their stated
expiry, tusd-boot, heartbeat, and lock-absence recovery rules because external
filesystem/network work may outlive the old app. Normal runtime recovery
reclaims each lease through its type-specific protocol. Token fencing remains
authoritative against stale goroutines, delayed callbacks, and pre-death
external work; the process-lifetime singleton prevents a second app instance
from serving the same app root concurrently. Cancellation-aware hashing and
copying merely reduce wasted work.

## Enqueue and Sidecar Recovery

### Pre-create reservation

For every public tus POST, the app proxy injects a random creation-lease token
before forwarding to tusd, and tusd forwards it to the hook endpoint.

For every proxied tus method, the proxy first removes all client-supplied
internal headers (`X-Internal-Proxy-Secret`, `X-Event-Gallery-Client-Ip`,
`X-Event-Gallery-Create-Lease`, and
`X-Ingest-Lease-Token`) and then sets only values owned by that server-side
path. A public client cannot smuggle a lease token to tusd.

The tusd entrypoint adds `X-Event-Gallery-Create-Lease` and
`X-Ingest-Lease-Token` to
`-hooks-http-forward-headers`, alongside the existing secret and client-IP
headers. The singular ingest header is operation-specific: PATCH carries the
shared PATCH operation token stored in both ownership columns; internal DELETE
carries the cleanup/discard job lease consumed by pre-terminate; POST and HEAD
do not set it. Transport ownership for DELETE and HEAD remains database-fenced
and is never inferred from that header.

Before forwarding an outer POST, the proxy parses and validates
`clientUploadId`, positive integer `clientAttempt`, `Upload-Length`, sanitized
filename, normalized declared SHA-256, and sanitized guest name through one
shared canonical parser also used by pre-create. These persisted
client-supplied fields are immutable. The server-resolved uploader IP remains
the original audit value and is neither client-controlled nor rewritten on
replay. Before synthesis or forwarding, the proxy rejects a matching
closed, cancelled, or client-expired lifecycle as durable 410 with its opaque
terminal class; this check occurs before attempt registration, so no later
attempt can reopen it. An exact replay of the current attempt with a cleared/expired creation
lease and a validated
tus resource is completed entirely by the proxy: it synthesizes 201 with
`Location: /api/tus/<existing-upload-id>` and never forwards the POST to tusd.

For a pre-upgrade request with absent `clientUploadId`/`clientAttempt`, the
proxy generates a fresh random lifecycle and attempt 1 for that POST, atomically
inserts lifecycle/attempt controls (`max_seen=1`, admitted=0) with the ordinary
fixed client-admission/control-GC horizons, and injects those values into
server-owned tus metadata before forwarding. The hook therefore
uses the ordinary exact-registered admission path. The old browser does not
learn the lifecycle ID and gets no cross-POST idempotency; a retried POST gets a
new random lifecycle, and content deduplication resolves any completed copies.
The proxy first requires every canonical immutable field to equal the row;
mismatch returns 409 without synthesis or forwarding. An exact replay with a
live creation lease returns retryable 423. For a greater
attempt on an existing lifecycle, it first applies the current attempt's
completion fence outside SQLite. A durability marker/operation, nonzero data
file, `write_started_at`, or state later than `uploading` wins: the proxy
joins/promotes recovery and returns 409 or retryable 423/503 without calling
tusd. Only an `uploading` attempt with `write_started_at IS NULL`, no completion
evidence, and an absent or validated empty regular data path is supersedable.

For a POST not short-circuited by the proxy, the pre-create hook's immediate
creation transaction rechecks the state and is exhaustive:

- a fresh attempt is admissible only when its exact attempt-control row exists,
  lifecycle cancellation/closure is null, `nowMicros < client_admissible_until`,
  `clientAttempt = max_seen_attempt`, and `clientAttempt > max_admitted_attempt`.
  The same transaction capacity-admits,
  inserts fresh physical tus/media IDs as current, and advances
  `max_admitted_attempt`; failure rolls back both row and watermark;
- a repeat of the same `(clientUploadId, clientAttempt)` while its creation
  lease is live returns retryable 423;
- an exact replay that raced past the proxy but now has a validated resource is
  rejected with retryable 423, so the next outer retry is synthesized by the
  proxy; the hook never returns an existing ID;
- an attempt below `max_seen_attempt`, at/below `max_admitted_attempt`, or
  lacking its exact control row is stale and rejected (including any noncurrent
  attempt). A higher registration can therefore supersede a delayed lower hook
  before either reaches `NewUpload`;
- when the admissible fresh attempt is greater than the current job attempt,
  the same transaction also rechecks the never-written supersession predicates
  and sets the prior current row noncurrent/cancelling. If admission/insertion
  fails, the prior row and admitted watermark remain unchanged.

Superseding sets the old row `is_current=0`, records cancellation intent, and
under the capacity lock keeps or reactivates its full incoming reservation.
Expiry of its create/write lease permits discard driving but does not release
that charge; maintenance releases it only after exact transport fencing,
authorized termination, upload-directory sync, and post-sync source absence.
The new attempt never reuses the old physical ID. Terminal lifecycle state
returns 410 until the ledger starts a new lifecycle with a fresh
`clientUploadId`.

The blocking `pre-create` hook remains the size/filename gate and additionally:

1. Requires the internal creation-lease token and rejects unknown/deferred or
  non-positive sizes and missing filenames. A present malformed
  `clientUploadId`/`clientAttempt` is rejected. The proxy guarantees both are
  present by this point; a valid client ID with an absent attempt is normalized
  by the proxy as transitional attempt 1 and registered before forwarding.
2. Reuses the shared canonical parser and executes only the fresh-attempt
  admission/election branch above. Exact or stale attempts are rejected before
  `NewUpload`; immutable metadata mismatch for the same lifecycle is rejected.
3. For a new elected attempt, generates safe random physical `upload_id` and
  `media_id` values and verifies that neither derived tus path already exists.
4. Sanitizes and stores filename, guest name, declared SHA-256, and resolved
  uploader IP plus `client_upload_id`, `attempt_number`, and `is_current`.
5. Runs the serialized capacity admission transaction and inserts the
  `uploading` row with `create_lease_token`, `create_lease_until`,
  `creation_grace_until`, and `reservation_expires_at`.
6. Returns the generated tus ID through `ChangeFileInfo.ID`.

`ChangeFileInfo.ID` is therefore set only to a freshly generated,
never-previously-issued physical ID. No pre-create outcome can ask tusd to
open an existing data or sidecar path with `NewUpload`.

Deterministic validation uses the existing 400/413 rejection semantics.
Capacity/rate pressure and transient SQLite busy/I/O failures return a normal
2xx hook envelope with `RejectUpload` plus embedded 429/503 and `Retry-After`,
so tusd does not translate them into an internal hook 500 and allocates no
source. Admission is canceled at `UPLOAD_ADMISSION_TIMEOUT_SECONDS`, safely
below the configured tusd HTTP-hook timeout. On eventual success, the frontend
receives the generated ID in the normal tus `Location` URL and retains it for
status polling.

While the outer creation POST is in flight, the proxy heartbeats the matching
creation lease after the hook creates the row. It clears only its own token
after tusd finishes `NewUpload` and the proxied response ends. A proxy/app crash
leaves a short expiring lease. Orphan terminalization, one-sided repair, and
row repair require both `creation_grace_until` and
`create_lease_until` to have elapsed. This covers arbitrarily slow filesystem
creation rather than assuming the fixed grace alone is sufficient.

The deployment also passes tusd `-disable-concatenation`. The shipped client
does not use partial/final concatenation, and accepting partial resources would
create completion semantics outside this state machine. Tests assert that tusd
no longer advertises the concatenation extension or accepts `Upload-Concat`.

### Pre-finish and post-finish hooks

Enable blocking `pre-finish`. It performs bounded validation and promotion:

At the earliest hook-handler entry, before authentication/body decode, record
one absolute deadline `now + UPLOAD_DURABILITY_WAIT_SECONDS`. Every parse,
validation, SQLite operation, executor submission, and join uses a child
context bounded by the remaining time. No phase resets or extends the deadline.

1. Require a safe, non-empty tus upload ID.
2. After authenticating the internal hook, require and snapshot one safe
  `X-Ingest-Lease-Token`; it is the PATCH operation token and is immutable for
  this hook/detached operation.
3. Require `Storage.Type == "filestore"`.
4. Derive the expected data path from `TUS_UPLOAD_DIR` and the upload ID.
5. Require the hook storage path to equal that derived path after cleaning.
6. Require a positive known size, a complete offset, and a regular data file
  whose size exactly matches the declared size.
7. Require immutable size and sanitized metadata to match the `uploading` row.
  Require `is_current=1`, both write/transport token columns equal the snapshot,
  `transport_operation='patch'`, and the recorded transport boot remains the
  current authorized tusd boot; a superseded/reowned attempt records/wakes deterministic
  discard and receives the retryable lifecycle-ended response, never
  promotion.
8. Start or join an idempotent ingest-manager durability operation keyed by
  `(upload_id, patch_operation_token)` under its
  root context in the fixed `UPLOAD_DURABILITY_WORKERS` executor, bounded by
  `UPLOAD_DURABILITY_WORK_TIMEOUT_MINUTES`; it syncs the data file, sidecar,
  and upload directory.
9. That operation atomically transitions `uploading` to
  `pending`, clears the reservation deadline, sets
  `observed_offset=expected_size`, `source_completed_at`, and
  `next_attempt_at`, and requires `is_current=1`, both token columns equal the
  snapshotted operation token, `transport_operation='patch'`, the same boot,
  and `cancellation_requested_at IS NULL`, then signals the workers. Pre-finish
  joins only for the remaining absolute budget; it returns the normal hook
  envelope only when the operation has committed.

The durability operation has explicit terminal race outcomes, not only success/
timeout. Before expensive fsync and again when its fenced promotion affects zero
rows, it reads the current row under the exact upload ID and snapshotted PATCH
token. If either ownership column/operation/boot changed, `is_current=0`,
lifecycle/current-row cancellation exists, or status is `cancelling`/
`discarding`, it completes immediately with `lifecycle-ended`, releases its
executor slot, wakes the cancelling/discarding driver, and performs no detached
continuation. Pre-finish maps that outcome to the retryable lifecycle-ended
hook envelope immediately; it does not wait the response budget or report
generic 503 backpressure. If `pending` or later already committed, the operation
joins that durable success. Only an unchanged uploading row with transient I/O/
SQLite work remains detached/retryable.

The hook does not delete, move, hash, decode, or derive media. It does not defer
sidecar cleanup. Queue saturation or wait expiry returns HTTP 200 from the hook
endpoint with a valid hook envelope whose `HTTPResponse` is client-facing 503
plus `Retry-After`. This is the only way tusd v2.10 can relay that status; it
also causes tusd to emit post-finish, which remains an untrusted idempotent wake
signal. The detached durability operation may continue to its own deadline and
the registry heartbeats its shared PATCH token after response-done as described
above; token replacement terminates it before filesystem work/promotion. The
proxy completion fence prevents later false success. Genuine malformed
hook payload/auth/protocol/internal failures use a non-2xx hook response and
accept tusd's bare fail-closed 500.

Configure the single global `-hooks-http-timeout` to 90 seconds and pass
`-network-timeout=90s`; pre-create and pre-terminate retain their own much
shorter app deadlines. The ordinary `-request-completion-timeout=10s` remains
unchanged. The required production inequality from the final request-body read
is `75s app response budget < 90s hook timeout < min(100s edge window, 90s network
timeout + 10s delayed cancellation)`. Thus app response-budget expiry has 15 seconds to
return its 2xx hook envelope before either tusd's hook client or request
context can cut it off. This is a narrow blocking durability wait, not a
media-processing timeout workaround. Incoming files remain unchanged on every
error.

When the bounded durability queue is full, pre-finish and the proxy completion
fence use that retryable 503 contract immediately rather than consuming the
75-second response budget. Queue depth and oldest-wait age are admission-pressure
inputs; sustained saturation causes new pre-create requests to back off before
more completions arrive. Join-by-upload ID does not consume an extra queue slot.

Set `-hooks-http-retry=0`. The entrypoint derives and validates the two timeout
flags from the documented defaults; invalid or reordered budgets fail startup.
The endpoint is on the internal Compose network and
all hooks are idempotent/recoverable; pester's default retries would otherwise
reuse the same hook deadline, hold tusd's upload lock longer, and invalidate the
edge budget. The deployment assumes Cloudflare's 100-second origin response
window: one 75-second app response budget inside one 90-second hook attempt leaves explicit
edge margin. A client disconnect or outer timeout may suppress post-finish and
still fails closed; reconciliation/completion fencing recovers the source.

Non-blocking `post-finish` is retained only as an idempotent wake/recovery
signal after a successful pre-finish. It performs no fsync or processing.
Reconciliation remains the backup for lost hook delivery or process crash.

The public tus proxy fences completion before forwarding a potentially
contending request to tusd. For every HEAD/PATCH, it first checks the durable
row plus the in-process durability registry and, when needed, the validated
data-file size. If a complete source or active durability operation lacks the
committed `source_completed_at`/funded `pending` marker, the proxy triggers or
joins the idempotent operation. From proxy-handler entry, all fence checks and
the join share one absolute `UPLOAD_DURABILITY_WAIT_SECONDS` deadline; the join
receives only the remaining budget. Until commit it returns retryable 503 with
`Retry-After` without forwarding to tusd. This prevents HEAD/retry PATCH from
acquiring tusd's busy upload lock, creating `.stop`, or cancelling the
in-flight pre-finish. Once the marker exists, requests may reach tusd; its
already-complete fast path can no longer bypass durability.
A pre-finish failure followed immediately by HEAD/PATCH can therefore never be
misreported as transport success before durable queue promotion.

If pre-finish receives a complete, path-validated legacy upload with no row,
it performs the same durable funded recovery transaction as the reconciler and
returns success. Without media funding it remains `recovering` rather than
becoming claimable. This is a normal cutover path, not a hook error.

### Termination gate

Enable tusd's blocking `pre-terminate` hook and non-blocking `post-terminate`
hook through the existing authenticated endpoint. No tusd termination is
authorized from queue status alone or without durable discard/cleanup intent.

For public cancellation or stale-incomplete retention, the app first validates
or adopts the upload row, verifies that create/write leases are absent or
expired, and atomically claims `cancelling -> discarding` (user intent) or
`uploading -> discarding` (retention) with a random termination lease. User
intent may already be durable while a writer drains, but DELETE is not issued
until those leases expire. This claim does not release incoming capacity.
Before claiming exact transport operation `terminate`, the worker also requires
`transport_lease_token IS NULL`; expiry alone is insufficient. A matching live
handler must clear it normally, or a different freshly authorized tusd boot
plus the two clean lock-absence observations must conditionally fence and clear
the stale token. Only then does the worker claim transport ownership and issue/
forward DELETE with `X-Ingest-Lease-Token`. A subsequent PATCH sees
`cancelling`/`discarding` and is rejected, while any paused old handler cannot
pass its token/reservation heartbeat; DELETE therefore cannot race a resumed
write. If DELETE fails or its response is lost, the discard worker owns retry
and the incoming reservation remains active.

The reconcile/maintenance loop is the explicit driver for deferred
`cancelling -> discarding`: on every pass it claims cancelling rows whose
create/write leases are absent or expired, assigns a termination lease, and
wakes the discard worker. This scan is database-indexed and independent of
reservation activity, but transport acquisition still obeys the exact absence/
fresh-boot fence above. A cancelled upload therefore waits safely while a
handler is uncertain and proceeds as soon as that writer is truly drained.

For published cleanup, the current `cleanup` lease token serves the same role.
The blocking pre-terminate hook allows deletion only when the row is
`discarding` or `cleanup` and the forwarded token exactly matches its live
lease. Every other request returns `RejectTermination`, regardless of current
offset. The public proxy strips client tokens before setting its own, and the
incomplete janitor uses the same claim API rather than calling tusd directly.

`pre-terminate` and `post-terminate` callbacks are deliberately stripe-free and
filesystem-free: tusd invokes them synchronously/asynchronously back into the
same app while the owning worker holds its job and exact transport leases but
no hashed stripe. They perform only bounded database lookup and constant-time
status/token comparison or wake signaling; they never acquire a stripe, begin
a writer transaction, or inspect paths.

`post-terminate` only wakes the owning cleanup/discard verification; it does not
create discard intent or mark a terminal state. Terminal `discarded`/`complete`
still requires verified path absence and directory sync.

The single tusd container still owns locker crash recovery, but never runs it
before storage identity authorization. Compose passes the hook secret to tusd
and starts the app without waiting for tusd health (`service_started`, not
`service_healthy`), avoiding a cycle. After acquiring its singleton lock, each
app process generates an in-memory random `app_process_boot_id`.

The app exposes secret-authenticated internal startup and heartbeat operations.
Tusd's PID-1 supervisor reads the safe regular upload sentinel, generates a
random tusd boot ID, and sends observed uploads UUID + tusd boot ID in bounded
startup headers. The app returns 204 plus its app-process boot header only after
all sentinels/`storage_volumes` match, the supplied UUID matches mount/database,
the boot ID is valid, and fixed-layout checks pass. The authorization
transaction records `storage_volumes.active_tusd_boot_id` before 204.
Missing/mismatched identity returns 503 and performs no mutation.

Only after 204 does the still-nonserving supervisor scrub exact safe locker
controls, sync the upload directory, and start tusd as a child. It records the
returned app boot ID and every five seconds sends heartbeat mode with that
expected app boot and its tusd boot. The app returns 204 only when the request
reaches that same singleton process and the tusd boot is still current. App
unreachability or boot/identity mismatch for three consecutive heartbeats makes
the supervisor TERM the child, wait a bounded grace, KILL if needed, and exit
nonzero; Docker restart policy starts a new supervisor/boot that must authorize
again before serving. PID 1 forwards TERM/INT and reaps the child.

`TUS_UNCERTAIN_CREATE_ROLLOVER_COUNT` is validated at startup as positive and
strictly below `TUS_PHYSICAL_MAX_UPLOADS`. When expired absent creation rows for
the current boot have neither `create_finished_at` nor a safe
`create_concluded_at` and reach that threshold, the app opens a tusd-generation
drain circuit before slot exhaustion: new upload operations receive retryable
503, exact-token heartbeats cancel active forwarding, and no new create/write/
transport owner is admitted. Once handlers drain or the bounded drain deadline
expires, the next authenticated heartbeat returns an explicit restart-required
response. The supervisor immediately TERM/KILL/reaps the child and restarts with
a fresh boot ID. Killing the old child fences every paused `NewUpload`; proxy
handlers tied to its boot cannot relay bytes/success after the new boot commits.
Startup inventory then clears only directory-synced absent old-boot liabilities
before reopening upload routes. Thus ambiguous failures remain conservatively
charged but cannot permanently consume the physical/job caps in one live boot.

The supervisor never removes data, `.info`, sentinels, or unrecognized entries.
The app holds `/readyz` and upload routes false until a current healthy tusd boot
and startup inventory finish. Overlapping tusd containers remain unsupported;
the supervisor enforces paired app/tusd lifetime across app crashes. Locker
scrub remains necessary because a tusd child PID can recur in a replacement
container and make a stale PID file appear live.

### Complete-sidecar reconciler

Move tus sidecar parsing into a small shared package used by both the ingest
manager and incomplete-upload cleanup. Parsing remains size-bounded and
validates file type, upload identity, storage paths, expected size, and data
file type.

In the fully identity-matched current-schema startup branch, the HTTP listener
starts promptly with ingest readiness false. `/healthz`
remains the existing shallow SQLite liveness endpoint used by Compose, so
cloudflared and gallery browsing can start. A separate `/readyz` reports ingest
readiness, and upload/create/status routes return retryable 503 while a bounded,
metadata-only startup inventory parses sidecars in batches. It performs no
large-file fsync. Every valid legacy partial or complete sidecar is adopted as
an `uploading` row when job/control capacity permits, with generated media ID, recovered
metadata, current offset, and a fresh random server-only client lifecycle ID,
with `attempt_number=1` and `is_current=1`.
Adoption sets
`write_started_at` whenever the validated data size is positive and
`source_completed_at` whenever it equals `expected_size`. Inventory runs the
same serialized capacity allocation: obligations that fit are active/funded;
others are persisted inactive/unfunded, or remain safely rowless if either cap
is reached. Readiness does not wait for unfunded obligations. Before any such
partial advances, PATCH must obtain a funded reservation; complete unfunded
uploads wait in `uploading` until reconciliation can reserve media capacity.
Once inventory reaches the bounded scan limit without unread entries,
readiness becomes true and new admission opens. This includes legacy physical
bytes in `statfs` immediately without pretending every historical remaining
obligation is funded.

After readiness and every `INGEST_RECONCILE_INTERVAL_SECONDS`, the manager
processes at most `INGEST_RECONCILE_PAGE_ENTRIES` sidecars from a persisted,
wrapping lexical cursor. It first handles directly known/woken job IDs by exact
path, so hook recovery does not wait for the full directory sweep; the paged
scan discovers rowless/legacy artifacts. At the 50,000 physical-upload cap and
2,000-entry page, worst-case full-sweep discovery is 25 intervals (375 seconds
at the default cadence), while changed/known uploads are immediate. Stable
entries whose sidecar/data size and mtime match the previous inventory need no
JSON reparse.

A sidecar whose regular data file size equals its declared complete size is
synced with the data file and directory. A complete
legacy/orphan sidecar with no row is first represented as inactive `uploading`
with a generated media ID, metadata recovered from the sidecar, a fresh random
server-only client lifecycle ID, attempt 1, current status,
empty uploader IP,
`observed_offset=expected_size`, and both write/completion timestamps set. The
same immediate transaction funds media capacity and promotes it to `pending`;
without funding it remains recoverable and unclaimable. Existing
`pending`, `processing`, `cleanup`, `complete`, `discarding`, or
`discarded` rows are never reset.

Every rowless/legacy adoption path, including startup inventory, periodic
partial/complete discovery, and pre-finish no-row recovery, uses one shared
immediate transaction. It collision-checks the random lifecycle ID and inserts
the job, `upload_lifecycle_controls` (`max_seen_attempt=1`,
`max_admitted_attempt=1`, fixed horizons from adoption time), and exact attempt-1
control together. Attempt 1 is already admitted because the physical tus
resource exists. Funding/promotion may occur in that transaction or later, but
no adopted job is visible without its controls; capacity/control-cap failure
leaves the source safely rowless/recovering and retries. The server-only
lifecycle is never exposed as metadata, but public physical DELETE, publication,
failure, terminal 410, retention, and control GC use the ordinary closure rules.
For pre-finish no-row recovery, that same transaction additionally stores the
authenticated PATCH token in both write/transport columns with operation
`patch`, current boot, and deadlines before its fenced promotion; startup and
periodic adoption have no request owner and leave those fields null.

No absent-path decision or one-sided repair is allowed until both the immutable
`creation_grace_until` and heartbeated creation lease expire. Afterward:

- If a contained regular data file is exactly `expected_size`, it is a
  recoverable completion even when `.info` is missing or malformed. Sync the
  data/directory, persist `observed_offset=expected_size` plus both
  write/completion timestamps, fund media capacity, and promote to `pending`;
  publication never deletes it merely because tusd metadata was lost.
- If a contained regular data file is smaller than `expected_size`, reconstruct
  a canonical bounded `.info` sidecar from the durable row and current file
  size through temp-write, file sync, atomic rename, and directory sync. A
  failure or ambiguous/non-regular path remains `recovering`; it is retained,
  not discarded.
- If data is absent but a valid sidecar exists, recreate an empty data file only
  when `write_started_at` and `source_completed_at` are both null. The write
  gate sets `write_started_at` before forwarding the first PATCH, so lossy
  `observed_offset` is never used to decide whether bytes once existed. A
  missing source after any write started is evaluated by the same definitive
  source-loss classifier used by pending/processing; it never removes the
  sidecar on a single stat error.
- If both paths are absent, handling is state-aware. An `uploading` row with
  both write/completion timestamps null, no cancellation conflict, and expired
  creation grace/lease records durable creation-orphan discard intent and moves
  to `discarding`; the discard worker's directory barrier then yields public
  `cancelled`. If either timestamp proves bytes existed, `uploading`, `pending`,
  or `processing` uses the full alternate-copy and definitive-source-loss
  classifier, including two scans, mount health, directory sync, and post-sync
  revalidation; unrecoverable loss maps public `failed`. `cancelling`,
  `cleanup`, and `discarding` remain with their state-specific leased workers,
  and terminal rows are not reclassified. No zero-path branch commits a
  terminal state from one observation.

Idle partials merely release active reservation accounting; only ordinary
incomplete-retention policy or explicit client DELETE terminates their source.

A `discarded` row is never resurrected. If late data/sidecar artifacts appear
for a lifecycle already reported cancelled, maintenance transitions it back to
`discarding` only for fenced artifact/source removal, then returns to
`discarded`. A no-URL browser retry receives terminal 410, rotates to a fresh
client upload ID, and starts a new lifecycle.

A `complete` publication result is immutable but its cleanup state is
re-enterable. Late recognized data/sidecar artifacts atomically set
`physical_slot_active=1`, transition `complete -> cleanup`, assign a fresh
cleanup lease schedule, and preserve `result_media_id`/public published or
duplicate status. The cleanup worker performs ordinary authorized termination,
directory barrier, and slot clear, then returns to `complete`; it never
reprocesses media or changes the public result.

The stale-incomplete janitor continues to ignore complete uploads. Complete
uploads are terminated only by the current leased worker for `cleanup` or
`discarding`.

## Processing and Publication

### Deterministic preparation

The media processor gains an API that accepts the preallocated media ID and
does not remove the source. It:

1. Magic-sniffs and allow-list checks the incoming file.
2. Derives `stored_filename` from the preallocated media ID and sniffed MIME,
  then persists both `stored_filename` and `mime_type` in a fenced job update.
3. Reads size and computes the authoritative SHA-256 using a
  cancellation-aware loop.
4. Compares any declared client SHA-256. A mismatch records the rejected-upload
  audit outcome/reason and transitions directly to `discarding` before any
  final artifact is created.
5. Persists `authoritative_sha256` in a fenced update before creating the final
   original.
6. Runs the single-original-temp resolution gate below. It reuses/promotes a
  valid anchored candidate even while the tus source exists and opens a new
  temp only after no candidate remains.
7. Copies the immutable source through a lease-specific same-directory
  temporary to `originals/<mediaID>.<ext>`, syncs and closes the file, then
  syncs the originals directory to anchor the temporary entry. Only after that
  barrier is it a recoverable alternate source. The worker atomically renames
  it and syncs the originals directory again before publication.
8. Reopens and validates the final original's size and SHA-256, then commits
   `prepared_size` and `prepared_at` under the active fence.
9. Generates derivatives into deterministic paths.
10. Returns a complete media result plus typed derivative warnings/errors.

Before any JPEG, PNG, GIF, or WebP full decode, the processor calls
`image.DecodeConfig`, rejects zero/overflowing dimensions, and computes a
conservative decoded-memory weight. Images above `IMAGE_MAX_SOURCE_PIXELS` or
the total shared `IMAGE_DECODE_MEMORY_BUDGET_BYTES` are published without a
thumbnail and logged as deterministic derivative skips; they do not crash or
retry the job. Accepted decodes acquire a weighted semaphore for their full
decode and resize lifetime. The pixel limit and two-worker pool cap worst-case
in-process memory, while ordinary phone images can still decode concurrently.

On retry, an existing deterministic original is reused only if its size and
SHA-256 match the source. A mismatch is an error; it is never overwritten
silently. Temporary files use the recognizable form
`.ingest-<mediaID>-<leaseToken>-<kind>.tmp` in the destination directory, so
concurrent stale workers cannot share them. A worker rechecks ownership before
each final rename and before publication. Deterministic final artifacts are
validated before reuse.

The manager owns a process-local original-temp writer registry keyed by upload
ID and lease token. Registration/unregistration is exception-safe; a paused
same-process writer remains registered, while process death empties the registry
and releases the lifetime singleton before any replacement can serve. Before
creating an original temp, the current worker acquires the upload stripe,
revalidates exact job ownership, and refuses/reschedules while another writer
is registered. It enumerates every contained recognized original temp for the
job/media ID. If any exists, it opens no new path.

With no registered writer, candidates are descriptor-validated outside the
stripe by expected size and authoritative SHA, then rechecked under the stripe
with current job ownership and same-file identity. A valid durably
parent-anchored candidate is synced and renamed to the deterministic final even
when the tus source still exists; a valid final instead authorizes removal and
directory sync of redundant candidates. Conclusively partial/invalid candidates
may be removed and synced only when no writer is registered. Any I/O ambiguity,
changing file, or multiple-candidate uncertainty retains every path and
reschedules. Only after a locked re-enumeration proves no candidate/final remains
does the worker register itself and `O_CREATE|O_EXCL` exactly one original temp;
it then releases the stripe for the cancellation-aware long copy. Before each
write and final rename it checks context and exact lease ownership; token loss
stops writing and leaves the one owned temp for recovery.

Derivative temp creation keeps the simpler rule: acquire the short upload
stripe, revalidate exact status/lease token, and open the contained temp with
`O_CREATE|O_EXCL`, then release the stripe for long work. Thus cleanup holding
the stripe sees every opened temp or prevents creation after token invalidation.

The durable `stored_filename` plus `authoritative_sha256` also form the recovery
identity for a final original whose `prepared_at` commit was lost. Before
source-loss classification or final-artifact removal, the worker opens the
contained deterministic path and validates size/SHA outside the stripe even
when `prepared_at`/`prepared_size` is null. It then acquires the upload stripe,
rechecks row/token and that the path still names the same file, and only then
syncs a valid file and performs the fenced update that backfills
`prepared_size`/`prepared_at` and resumes publication from it. Stat/read/hash
errors are transient and retain both paths. Only conclusive absence or a
size/hash mismatch after successful I/O proves that candidate unusable.

A stale reserved temporary with `kind=original` is also an alternate prepared
source for a retryable `pending`/`processing` job. The recovery path opens and
validates parsed media ID/token, contained regular-file path, `expected_size`,
and `authoritative_sha256` outside the stripe. The single-temp resolution gate
applies in `pending`/`processing` too: a durably parent-anchored match is promoted
regardless of tus-source presence, then the final is directory-synced, validated,
and used to backfill the prepared marker. If fault injection presents multiple
candidates, no new temp is opened; one valid candidate may be promoted and all
others are removed only after the final validates, while any conflicting/
ambiguous candidate retains the set for retry. Transient source, final, or temp
stat/read/hash errors retain every candidate. A temp is deleted only after
another valid copy is established, all checks conclusively mismatch with no
registered writer, or durable cancellation/client-rejection discard intent
authorizes removal under the same writer/stripe fence.

At startup and periodically, an artifact reconciler scans only that reserved
temporary namespace. It removes and directory-syncs expired derivative temps;
an expired original temp first follows the alternate-source rules above and is
retained/woken when copy state is ambiguous. Unrecognized files are never
touched. A stale writer whose path was unlinked cannot publish because both rename and
the subsequent fenced transition fail.

All final artifact creation/removal and stale-temp reconciliation also acquire
the upload's lock stripe. This prevents a superseded process from creating
an unowned final artifact or renaming a stale temporary after a new process has
completed ownership cleanup.

### Transactional publication

Before duplicate resolution, the worker queries for a conflicting SHA row
without a write transaction. If one exists, it opens the contained regular
original and validates size/SHA outside any stripe. It then acquires the fixed
media stripe keyed by that row's ID, re-reads the row, and requires the path to
still name the same open file with unchanged stored filename/size. While still
holding the stripe, it syncs the validated open file descriptor and the
`originals` directory, then repeats the row/path/same-file recheck. Only that
successful barrier permits the publication transaction and later deletion of
the new prepared copy. It holds the stripe through this bounded sync/recheck
and the publication transaction. Missing, non-regular,
unreadable, wrong-size, or wrong-hash originals are an integrity fault: the job
returns to persisted transient backoff, retains its tus source and validated
prepared original, and emits a critical metric/log. It is never classified as
a duplicate and neither copy is deleted automatically. File/directory sync or
post-sync recheck failure has the same retryable retain-both-copies outcome.

One immediate, fenced store transaction then:

1. Verifies `status='processing'` and the exact `lease_token`.
2. Attempts the media insert with SHA-256 conflict ignored, then selects the
  authoritative row by SHA-256 while retaining the write lock. If a different
  row first appears here and was not the locked, validated candidate, the
  transaction rolls back and restarts the validation flow; no duplicate state
  is committed from an unvalidated conflict.
3. If the row ID equals the job's preallocated `media_id`, treats it as this
  job's publication, writes `result_media_id = media_id`, and preserves those
  artifact paths.
4. If the row ID differs, records it as `result_media_id` and treats this job
  as a duplicate. The nonterminal job row is also a database pin on that
  authoritative media ID until duplicate cleanup completes.
5. Transitions the fenced job to `cleanup`, clears its processing lease, sets
  the lifecycle `closed_at` plus `published`/`duplicate` outcome, and commits
  the media result, queue transition, and immutable lifecycle closure together.

A unique-hash race is serialized by the immediate writer transaction and the
database constraint. Database failure rolls everything back. The job and
source remain retryable, and valid deterministic artifacts may be reused.

Permanent media purge is a per-media operation even when invoked by the bulk
admin endpoint or retention loop. It deduplicates IDs, iterates deterministically,
acquires one media stripe before staging that item's original, and holds it
through one immediate row-deletion/audit transaction and stage restore/finalize.
It releases that stripe before the next item. The transaction must find no
nonterminal upload job with
`result_media_id = media_id`. If a pin exists after files were staged for
purge, purge aborts and restores the stage; it retries only after the duplicate
job reaches `complete`. If purge commits before duplicate resolution, the
duplicate transaction no longer finds the old SHA row and publishes the new
job/media ID instead. Therefore purge and duplicate resolution always leave at
least one committed row plus complete original.

After the indexed nonterminal-pin check, that same purge transaction inserts one
`media_purge_tombstones` row containing the exact v2 token, media ID,
authoritative stored filename/size, purge time, and fixed expiry; it then deletes
the media row and writes the matching purge audit. A conflicting pre-existing
tombstone refuses the purge, while an exact same-token replay is idempotent.
The transaction never scans or updates retained upload jobs, regardless of how
many duplicates reference the media ID.

A concurrent late-artifact `complete -> cleanup` transition either wins first
and blocks purge as a nonterminal pin, or runs after purge and observes the
committed tombstone through its indexed `result_media_id`. No retained complete
job can lose its result row without bounded durable intentional-removal proof.
Its published/duplicate status remains the historical ingest outcome; the
tombstone governs later artifact cleanup rather than republishing media.

New purge manifests are version 2 and contain a random `purge_token`, media ID,
stored filename, and expected original size. The same per-item transaction that
deletes the row writes `ActionPurge` audit details containing that exact token,
media ID, and stored filename; the audit is retained as the durable purge-commit
oracle. Reconciliation may finalize an absent-row v2 stage only after matching
all three fields in that committed audit. Existing version-1 manifests remain
readable: because the old `PurgeTrashed` transaction already inserted
`ActionPurge` with the deleted media ID atomically, an absent-row v1 stage
requires that exact action/media-ID audit match. Without the matching proof,
either version is left untouched for manual inspection and cannot help bind a
candidate app database. A surviving matching media row still means restore.

Pre-upgrade v1 partial stages have their own closed compatibility form. Go
1.25's `os.MkdirTemp(PURGING_DIR, "purge-")` emits
`purge-<canonical-uint32-decimal>` (1-10 digits, no leading zero except `0`).
Before the old code moved any media it created that directory and completed
`manifest.json.tmp -> manifest.json`. Therefore a matching contained real
directory is a **legacy pre-move partial** only when it is empty, or contains
exactly one bounded regular non-symlink `manifest.json.tmp`, with no final
manifest and no original/thumbnail/preview/other child. A valid final v1
manifest continues through ordinary v1 recovery. Final-plus-temp, wrong type,
unsafe/noncanonical suffix, oversized temp, staged media without a final
manifest, or any foreign child remains ambiguity because it might contain the
only copy.

Legacy pre-move partials are tolerated structural recovery input: never media,
mount, or purge-commit evidence, but they neither refuse stopped-stack binding
nor keep purge-dependent readiness closed. The initializer is read-only and
leaves them byte-identical. After media identity matches, the singleton runtime
reconciler is their sole owner; no new binary creates v1 stages. It revalidates
the entire empty/temp-only shape, unlinks only the recognized temp if present,
removes the now-empty stage, and syncs `.purging`. Any changed/ambiguous shape
is left untouched with a stable remediation log. The pre-readiness scan tracks
these entries in a separate cleanup list rather than the media-ID stage index,
so verifier/duplicate decisions never mistake them for staged media.

New v2 stage directories use the closed basename
`purge-v2-<safeMediaID>-<safePurgeToken>` and their sole manifest temporary is
`manifest.json.<sameSafePurgeToken>.tmp`; version-1 stage basenames remain
loader-compatible. Stage creation generates both IDs first, creates/syncs the
directory and `.purging`, writes the bounded temp with `O_EXCL`, syncs/closes
it, renames to `manifest.json`, and syncs the stage before moving any live
artifact. Therefore a v2 directory with no final manifest may contain only its
matching manifest temp and no staged media.

Scanners recognize that partial form only when directory/media/token syntax,
regular non-symlink type, bounded size, and token equality all hold. It is
structural recovery input, never media/mount/purge-commit evidence; a foreign
child, staged artifact without a final manifest, token mismatch, or lookalike
remains ambiguity. After media identity matches, runtime recovery validates and
promotes a complete temp to the final manifest, or removes a conclusively
partial/expired temp plus now-empty stage and syncs `.purging`. If the final
manifest exists, an expired matching temp is redundant and removable only under
the media stripe. The stopped-stack initializer leaves valid partial stages for
post-binding recovery unless the existing media identity already matches and
the temp is conclusively redundant/empty. No generic scanner unlinks any other
stage child.

Startup and periodic runtime purge-stage reconciliation are the third member of
the same serialization domain. After bounded manifest/path validation, the
reconciler acquires the media stripe keyed by the manifest media ID before
reading the media row or purge audit. It holds that one stripe through the
locked state recheck, restore or proof-authorized finalize, all affected
directory syncs, and stage removal, then releases it before the next stage. A
busy stripe leaves the stage unchanged for the next pass. The stopped-stack
initializer remains read-only for stages and needs no runtime stripe. Thus no
verifier, duplicate worker, online purge, or stage reconciler can observe or
create a row/file combination from the middle of restore/finalize.

Before readiness, a complete bounded `.purging` scan builds an in-memory index
from media ID to exact validated stage path/version/token/stored filename/size;
malformed or conflicting entries keep purge-dependent readiness closed for
manual inspection. Online purge registers its file-synced manifest in that
index under the media stripe before moving the live original and removes it only
after restore/finalize and directory barriers. Runtime reconciliation validates
and updates/removes entries under the same stripe. After a crash, readiness
therefore cannot expose verifier/duplicate routes before every surviving stage
is indexed, even if reconciliation has not yet acquired its item's stripe.

Whenever verifier or duplicate resolution sees a row but its live original is
absent/mismatched, its short media-stripe recheck consults and revalidates this
index before returning false or an integrity fault. A matching row-present
stage, or ambiguity while checking one, returns retryable 503, wakes/joins stage
reconciliation, and is never negative-cached; the next call observes restored
live state or committed row deletion. Only absence of both a valid live original
and any matching/ambiguous indexed stage permits the ordinary false/integrity
path. The index is a recovery hint, never purge-commit proof; absent-row
finalization still requires the durable versioned audit oracle.

Bulk purge is intentionally non-atomic across IDs. A busy stripe is not a
batch failure: the admin response keeps `changed` and adds `retry` IDs, and the
UI refreshes state; retention leaves that row for the next interval. A fatal
per-item validation/I/O/database error is reported with its ID without rolling
back already committed items. No request holds hundreds of stripes or blocks
ingest while staging an entire batch.

Upload audit recording remains best effort and preserves the existing outcome
coverage: success, duplicate resolution, unsupported content, and checksum
mismatch each produce the corresponding `ActionUpload` entry after their
durable state transition.

### Cleanup

Cleanup jobs are claimed with their own lease token. For a duplicate, cleanup
first removes the losing job's deterministic artifacts only when
`result_media_id` is non-empty, `result_media_id != media_id`, and a fresh
database check confirms that no committed media row owns `media_id`. The
ownership check is mandatory. Original cleanup uses the job's persisted,
validated single-segment `stored_filename`, never the client filename or a
re-sniffed source. Each unlink is followed by syncing the
affected originals/thumbnail/preview directory and verifying absence. A
self-match never removes artifacts.

A `discarding` worker performs the same persisted-path ownership check for
every deterministic original/thumbnail/preview. When no media row owns the
job's `media_id`, it removes, directory-syncs, and verifies those paths before
terminating the tus source. This is mandatory even if checksum validation
normally occurs before materialization, because a crash/retry may have left
artifacts from an earlier attempt. `discarded` cannot commit while an unowned
final artifact remains. If `stored_filename` and `authoritative_sha256` are
present but the prepared marker is absent, the worker first runs the
markerless-final recovery check above; it cannot unlink the candidate while
that check is transient or before a conclusive mismatch is durably recorded.

The worker then calls tusd's internal DELETE endpoint with its lease token.
tusd acquires its upload lock, invokes the pre-terminate gate, and performs the
normal removal of data and `.info`. The app never unlinks tusd lock files.

HTTP 204, 404, or 410 is only a prompt to verify, not proof of completion. The
worker requires both derived data and sidecar paths to be absent, syncs the
upload directory, and verifies absence again before its terminal transition.
If tusd crashed between its two unlinks, the worker may repair the remaining
one-sided regular data or sidecar only after revalidating the job status/token,
the sidecar identity when present, and absence of an active tusd lock. It then
syncs and verifies the directory. Any ambiguity or remaining lock schedules a
retry; the app never removes lock files.

Only after that post-sync absence proof does one capacity-locked fenced
transaction require the exact job and `terminate` transport tokens, clear the
matching transport token, deactivate incoming/media reservations, conditionally
clear the physical slot under its separate creation-fence rules, and transition
`cleanup -> complete` or `discarding -> discarded`. No earlier status/lease-
expiry step releases incoming capacity. A crash after source deletion is
recovered by the same path verification with the charge retained. A cleanup
error leaves the job in `cleanup` with future backoff and does not affect the
committed media row.

Before terminal transition, the cleanup/discard worker also holds the short
upload stripe and scans all three media temp namespaces for the job's `media_id`,
the upload root's exact `sidecar_repair_artifact_token`-owned temp when nonnull,
plus every persisted
deterministic final path. Published self-owned
finals must still be owned/valid unless `media_id = result_media_id` and the
indexed tombstone for `result_media_id` matches the job's authoritative stored
filename/size and its token/media/filename matches a committed `ActionPurge`
audit. That bounded tombstone-plus-audit proof authorizes the former self-owned
final and derivatives to be absent; if any exact former path reappears, cleanup
removes and directory-syncs it without restoring or republishing the purged media.
Duplicate/discard finals and every job-owned temp must be absent. It syncs each
affected media/upload directory, revalidates absence/ownership or exact purge
proof, then clears the durable sidecar artifact token and sets
`artifacts_cleaned_at=nowMicros` in the same fenced terminal transaction.
Ambiguous/open/I/O-error candidates retry and leave the marker null.

Discovery of any later recognized job-owned temp/final or tus artifact under a
terminal row, including a temp matching a still-persisted durable sidecar
artifact token, atomically
clears `artifacts_cleaned_at`, reactivates physical liability when applicable,
and transitions `complete -> cleanup` or `discarded -> discarding` with a fresh
schedule. Retention cannot race this
because discovery/marker clear and terminal deletion serialize as immediate
transactions; the fixed process/stripe/token rules prevent artifact creation
after the final clean scan by an unowned worker.

## Failure Handling

Failures are classified by the manager:

- **Transient core processing:** context deadline not caused by shutdown,
  SQLite errors, source/original filesystem I/O errors, and a conflicting
  authoritative media row whose original fails descriptor size/SHA validation
  or the short locked same-file recheck.
  These increment
  `processing_failures`, retain source and prepared artifacts, and return to
  `pending` with capped persisted backoff indefinitely.
- **Definitive source loss:** an uploading/pending/processing job may enter `discarding`
  with public `failed` only after alternate prepared-source recovery fails.
  If `authoritative_sha256` and `stored_filename` are present, the worker
  opens and validates the deterministic final and every recognized
  original-kind temp by expected/persisted size and hash outside hashed stripes,
  then uses short same-file/token rechecks for backfill/promotion regardless of
  whether `prepared_at` committed. A valid markerless final is synced/backfilled;
  a valid temp is promoted through the ordered final-rename flow. Either resumes
  derivative generation/publication, and the missing tus source is then
  ordinary cleanup. A transient candidate check retries without state change;
  conclusive absence/mismatch of every candidate falls through to the criteria below.
  Source loss requires the upload mount identity/health probe to pass,
  the expected regular data path returns `ENOENT` (not `EIO`, `ESTALE`, or
  permission error) in two scans separated by a reconciliation interval, and
  `write_started_at` or `source_completed_at` proves bytes once existed. Each
  scan takes/releases the upload stripe only for its bounded namespace snapshot;
  no stripe spans the interval. During the second short critical section, the
  worker syncs the upload directory and, when deterministic final/original-temp
  absence contributes, the originals directory. It then re-stats the expected
  data/final paths and re-enumerates the reserved original-temp namespace.
  Only post-sync `ENOENT`/no-candidate observations may commit definitive source
  loss intent in a fenced `status -> discarding` transaction while still under
  the stripe. Any directory-sync error, candidate/path reappearance, or
  post-sync I/O ambiguity is transient and leaves the job retryable. After
  releasing the stripe, a capacity-only conditional transaction releases byte/
  media reservations for that exact discard reason; a crash between commits is
  conservatively charged and maintenance completes it. The physical slot stays
  charged until source cleanup verifies absence. No sidecar cleanup precedes
  durable discard intent.
- **Deterministic client validation:** unsafe identity/path, unsupported magic
  type, and declared checksum mismatch. Unsafe identity/path is rejected
  before allocation. Unsupported magic type and checksum mismatch are
  deterministic client failures: record the durable terminal reason and move
  directly to `discarding`.

One discard-reason table is authoritative for the state transition, lifecycle
tombstone, batch status, and cancellation response. Explicit user intent and a
never-bound/never-written creation orphan set `closed_at` with outcome
`cancelled`; unrecoverable source loss, unsupported/checksum-invalid content,
and stale-incomplete retention expiry set outcome `failed`. The same immediate
transaction writes `terminal_reason` and the matching lifecycle outcome.
The predicate requires the same `is_current=1` physical attempt; cleanup of an
`is_current=0` superseded row never mutates lifecycle closure.

- **Transient HEIC/HEIF conversion:** process start, timeout, resource, and
  temporary/output I/O failures increment only `conversion_failures`. They use
  the short conversion schedule without consuming the core budget.
- **Deterministic HEIC/HEIF conversion:** a typed unsupported codec/container,
  invalid primary image, or source-pixel safety-limit result immediately uses
  fallback publication without derivatives. Repeating the same decode cannot
  change the outcome, so it does not occupy workers through the retry budget.
- **Other derivative failures:** JPEG/PNG/GIF/WebP/AVIF thumbnail and all
  ffprobe/ffmpeg probe or thumbnail failures retain today's best-effort
  behavior: log the error and publish without that derivative. They do not
  consume a processing retry.
- **Shutdown cancellation:** stop work promptly, release or expire the lease,
  and retain every source and prepared artifact for restart recovery.
- **HEIC/HEIF conversion exhaustion:** publish the original with
  `has_preview=false` and `has_thumbnail=false`, then clean the tus source only
  after the publication transaction commits. The preview endpoint falls back
  to the original so browsers with native HEIC support can display it.

A generic "malformed" classification is not inferred from an optional
derivative failure. Magic-recognized JPEG/PNG/GIF/WebP/AVIF and video retain
the existing best-effort publication contract, while HEIC/HEIF follows its
typed conversion fallback below.

There is no core retry exhaustion transition. A dependency circuit breaker
probes only dependencies required for publication itself: SQLite writes,
upload/media mount readability and writability, and free space/inodes. While a
shared core dependency is unhealthy, due jobs are rescheduled without
incrementing per-job failure counters. Recovery wakes pending jobs immediately.
A job-specific core error still increments diagnostics and retries, but never
authorizes source discard. Optional derivative executables are excluded:
`heif-preview` start/runtime failures advance each HEIC job's finite conversion
schedule toward fallback publication, while ffprobe/ffmpeg unavailability is
logged and retains best-effort publication. One optional tool cannot open a
global circuit that blocks unrelated media. If retryable sources consume
capacity, new admission pauses rather than deleting existing completed uploads.

### External media process isolation

Every ffprobe and ffmpeg invocation runs through a small `media-tool-limit`
launcher that applies `RLIMIT_AS=MEDIA_TOOL_MEMORY_BYTES` before `exec`. Commands
also set the supported decoder pixel ceiling, thread/filter-thread limits, and
short operation-specific timeouts. ffprobe requests only the fields consumed by
the parser with bounded probe/analyze settings. Go captures stdout/stderr in
fixed-size truncating buffers capped by `MEDIA_TOOL_LOG_BYTES`; child output can
never grow the Go heap without bound.

Memory/pixel/resource-limit exits, oversized metadata, and unsupported decoder
outcomes remain deterministic derivative skips under the existing best-effort
contract. They never crash the Go server or trigger a core retry. Integration
tests cover oversized AVIF/video dimensions, excessive metadata/log output,
and concurrent bounded children.

## HEIC/HEIF Derivatives

### Runtime dependency

Keep the Go backend `CGO_ENABLED=0`. Build a small, separately compiled
`heif-preview` helper against Alpine v3.22 libheif `1.19.8-r1` and libpng, then
copy that helper into the final image and install the pinned libheif runtime
package plus its APK-resolved codec dependencies and
`libpng=1.6.57-r0`. This contains native decoding outside the Go process
without introducing CGO into the server. Build with pinned
`libheif-dev`/`libpng-dev`; keep `libheif-tools=1.19.8-r1`, which provides
`heif-enc`, in the Docker integration-test stage only. Production does not ship
an encoder. CI verifies `scanelf`/`ldd` dependency closure and executes the real
helper inside both final amd64 and arm64 runtime images.

The helper is intentionally narrow. It accepts input/output paths, maximum
source pixels, maximum output dimension, and memory bytes; selects the primary
image with libheif's primary-image API; returns stable typed exit outcomes; and
emits machine-readable source dimensions. Its first operation applies
`RLIMIT_AS=HEIC_CONVERTER_MEMORY_BYTES`, before libheif initialization or input
read. It configures restrictive context security limits before
`heif_context_read_from_file`, then selects the primary and rejects zero,
overflowing, or above-`HEIC_MAX_SOURCE_PIXELS` dimensions before decode. This
replaces libheif's much larger default one-gigapixel and per-block limits with
process-wide and parser/decode bounds.

### Conversion pipeline

For MIME types `image/heic` and `image/heif`:

1. Create a private empty temporary directory under `/tmp`.
2. Run `heif-preview` with `exec.CommandContext`; never invoke a shell. The
   helper decodes the primary image with transformations, scales only after
   the source-pixel and memory checks, and writes a bounded PNG.
3. Require a successful typed outcome plus a regular, non-empty PNG and valid
   metadata response, then decode the PNG with Go. Success without valid output
   is a conversion failure.
4. Record true original display dimensions by comparing the helper's primary
   source dimensions with the rendered PNG aspect, using the existing
   orientation helper. Never store the preview-bounded dimensions as original
   dimensions.
5. Run the existing ffprobe image probe independently as best-effort metadata:
   retain its capture time when available and log failures without affecting
   libheif derivative success.
6. Generate `previews/<mediaID>.jpg` at JPEG quality 90 and
   `thumbnails/<mediaID>.jpg` at JPEG quality 85 through lease-specific files,
   atomic renames, and parent-directory syncs.
7. Remove the private temporary directory.

The original HEIC/HEIF is never rewritten. AVIF continues through the ffmpeg
fallback because that path is already validated with the pinned image.

The general `heif-convert`/`heif-dec` tool is deliberately not used: libheif
1.19.8 writes every top-level image with numbered filenames, does not guarantee
primary-first order, and can report process success after an encoder write
failure. The upstream thumbnailer is not used directly because it fully
decodes before scaling and exposes only libheif's one-gigapixel default limit.
Tests include a multi-image fixture whose primary item is not first, a rotated
primary, oversized valid dimensions, memory-limit failure, and output-write
failure.

### Artifact lifecycle

The processor adds `PreviewsDir` and `PreviewPath`. Directory setup, media
removal, recoverable purge staging, purge restoration, and purge finalization
include previews. Preview presence is discovered by path and remains optional
in both manifest versions. The version-2 shape change exists only for the purge
commit token/size proof; the loader continues to read version 1 stages.

## API and Frontend

Add:

```text
GET /api/media/{id}/preview
POST /api/uploads/status
POST /api/uploads/cancel
POST /api/admin/uploads/verify
```

The public configuration DTO adds `uploadClientLifecycleDays` only as a
provisional pre-POST UI/scheduling hint. The proxy returns decimal signed Unix-
microsecond headers `Upload-Lifecycle-Admissible-Until` and
`Upload-Server-Time` on every outer POST response once that lifecycle row
exists, including synthesized replay, retryable refusal, and terminal 410; it
adds both names to the exposed-header set. `POST /api/uploads/cancel` returns
the same fields in JSON, and every known `/api/uploads/status` result includes
`clientAdmissibleUntilMicros`; the batch envelope includes one
`serverNowMicros`. All values come from the transaction's one `nowMicros` and
the immutable stored cutoff. Unknown lifecycles expose neither a guessed cutoff
nor existence metadata.

The exact cutoff is an opaque scheduling fact, not authorization: server
registration/pre-create still enforce it. A client computes remaining lifetime
from `cutoff - serverNow` rather than assuming its wall clock agrees, refreshes
the server-time relation on each response, and treats 410 as authoritative even
if its local estimate differs.

`POST /api/uploads/cancel` accepts only a safe random `clientUploadId` and
positive `clientAttempt`, under a dedicated keyed limiter. These values are an
opaque proof that this client previously registered the logical lifecycle;
responses expose no metadata. Cancellation means the whole logical file across
tabs, not only the supplied physical attempt. After the same completion-fence
checks used by public DELETE, one immediate transaction first looks up the
lifecycle. If it exists, the random `clientUploadId` is sufficient capability;
the supplied attempt need not have reached proxy registration and no unseen
attempt-control row is allocated. The transaction locks the lifecycle and
computes `max_seen_attempt_at_cancel = max(max_seen_attempt, suppliedAttempt)`.
If no lifecycle row exists, it returns 404 and allocates nothing. Then:

- if `closed_at` is already set, `cancelled` returns its idempotent durable
  result, while `published`, `duplicate`, or `failed` returns 409/
  cancellation-lost; cancellation never rewrites a terminal outcome;
- if any `is_current=1` row for that lifecycle is `uploading`, regardless of
  its attempt number, set lifecycle `cancelled_at`/`closed_at` and outcome
  `cancelled`, mark the supplied attempt control cancelled only when that exact
  control already exists, atomically
  change that current row to `cancelling`, and return its opaque physical ID
  for status polling;
- if the current row is `pending` or later, insert no cancellation and return
  409/cancellation-lost;
- if no current row exists, set lifecycle `cancelled_at`/`closed_at` with
  `cancelled` outcome and return durable public cancelled;
- if lifecycle cancellation already won, return the same durable result and
  current physical ID when one exists.

Once `cancelled_at` commits, proxy registration, pre-create, exact replay, and
all greater attempts for that `clientUploadId` return 410. The lifecycle-
cancel/current-row/promotion races are serialized by immediate transactions:
cancel-first prevents creation/promotion across every tab, while pending-first
returns 409. A hook that already returned may still be followed by late
`NewUpload`, but its current row is `cancelling` and the charged cleanup path
removes those artifacts. Lifecycle/attempt controls use
`UPLOAD_CONTROL_RETENTION_DAYS`, which must exceed
`UPLOAD_CLIENT_LIFECYCLE_DAYS` plus the maximum edge/create/reconcile margin.
Lifecycle tombstones are deleted in bounded batches only after expiry and after
no retained job references the lifecycle. Open-lifecycle attempt controls retain
the same horizon; closed-lifecycle attempt rows may use the earlier bounded
compaction rule because the tombstone remains. Closed and client-expired
lifecycle controls therefore outlive every supported client that could reuse
the ID and cannot be resurrected within the documented lifecycle horizon.

At first valid outer-POST entry, before forwarding, the proxy creates a missing
lifecycle with immutable `client_admissible_until = created +
UPLOAD_CLIENT_LIFECYCLE_DAYS` and `expires_at = created +
UPLOAD_CONTROL_RETENTION_DAYS`. It inserts/idempotently updates the exact
attempt control's `seen_at` and gives it the lifecycle's fixed `expires_at`;
neither horizon is refreshed. Existing registration advances
`max_seen_attempt = max(old, clientAttempt)` under the create limiter and
app-data/control-cap admission only while cancellation/closure is null and
`nowMicros < client_admissible_until`; otherwise it returns 410 and allocates
nothing. A registration that would insert one or two control rows requires that
exact headroom under `UPLOAD_CONTROL_MAX_ROWS` and also refuses when the separate
job cap has no potential slot; pre-create rechecks actual job headroom when it
inserts. Controls never consume `UPLOAD_JOB_MAX_ROWS`. Due tombstones and
eligible closed-lifecycle attempt rows are deleted in bounded batches.
Therefore cancellation cannot allocate arbitrary unseen rows, and create/cancel
spam remains bounded by the separate control/database-growth envelope.

The route applies the same active/approved visibility check as files and
downloads. If `has_preview` is true, it serves `previews/<id>.jpg` as
`image/jpeg`. Otherwise it streams the original with its stored MIME type.

`GET /api/media/{id}/download` always serves the untouched original with the
original filename and MIME type. Video playback continues to use `/file`.
Image lightbox slides switch from `/file` to `/preview`. Per the chosen
fallback behavior, a published HEIC without a preview is still attempted by
the browser through `/preview`; unsupported browsers may show their normal
decode failure while the download action remains available.

Add `hasPreview` to backend DTOs and the frontend `MediaItem` contract. It is
used for tests and future UI behavior even though the preview route itself
provides fallback semantics.

Keep existing `POST /api/uploads/check` as a backward-compatible public shim.
It always returns HTTP 200 with deprecated `duplicate:false` and performs no
database lookup, stat, or hash; pre-upgrade tabs therefore continue to tus
rather than removing the file or failing on a transient check. The new uploader
only hashes and sets tus metadata and does not call this route before upload.

Add `POST /api/admin/uploads/verify` for the production load-test oracle under
the existing admin session and CSRF middleware. It is reachable through the
same public HTTPS origin but anonymous, stale-session, and missing/wrong-CSRF
requests fail before acquiring verifier resources. It never accepts or exposes
`TUS_HOOK_SECRET`. After upload status already says published/duplicate, an
authenticated harness calls this endpoint once per unique digest.
A SHA row alone is not verified. The handler checks a bounded memoized verdict
keyed by `(media_id, stored_filename, size_bytes, sha256, device, inode, mtime,
ctime)` with a short TTL. A matching regular-file stat returns the cached
hash verdict only after the mandatory media-stripe recheck below; any key
change forces revalidation.

On cache miss, a dedicated very-low-rate limiter plus fixed two-slot semaphore
bounds work. The handler opens and validates the contained regular original
against row/request size and SHA with cancellation-aware I/O outside the stripe.
Before every true or false response, including a cache hit or an initial
missing/mismatched stat, it acquires the short media stripe, re-reads the row,
and re-stats the live contained path. A cached true is returned only if the
row/path/stat key still matches; a cached false is returned only if its negative
key/row absence still matches. On cache miss, the previously opened/hash-
validated descriptor must still be the path's same file/stat key before the
verdict is cached. Stripe contention returns 503, never a momentary false.
Online purge and runtime purge-stage reconciliation hold the same stripe through
stage/commit/restore/finalize, so verification sees either the pre-purge live
row/file or the final restored/deleted state, never a staging or crash-recovery
window. The endpoint response identifies only the requested opaque result and
leaks no paths or internal metadata.

Add a small IndexedDB upload ledger whose object-store primary key is the random
`clientUploadId`. It has non-unique indexes on the Uppy metadata fingerprint and
the exact content key `(size, SHA-256)`. Fingerprint lookup returns bounded
candidate records; after hashing/canonicalization, one read-write transaction
reuses a retained nonterminal lifecycle only when size, SHA, canonical filename,
and canonical guest name all match and the UI action chose resume. Otherwise it
inserts a distinct new lifecycle, even for the same bytes. Thus
metadata collisions can coexist and concurrent tabs cannot overwrite one
another merely because their filename/type/path/size/mtime match. Before the
first POST the selected record durably stores SHA-256, size, canonical sanitized
filename, and canonical sanitized guest name,
`lastAllocatedAttempt=0`, nullable `activeAttempt`/active upload ID/URL, and
lifecycle state, plus nullable `clientAdmissibleUntilMicros`,
`lastServerNowMicros`, and local observation time. `createdAtMicros` is
informational and never reconstructs server expiry. Before every no-URL outer POST, one IndexedDB transaction
increments `lastAllocatedAttempt` and returns that value as `clientAttempt`,
ensuring fresh physical attempts across tabs. A 201 response installs its
attempt/upload ID/URL plus the exact response cutoff/time as active only when no
higher accepted active attempt is already stored; the local uploader awaits that IndexedDB commit as a hard
barrier before issuing any HEAD or PATCH. The tus fork pauses after parsing and
validating Location/upload ID and cannot expose the resource to its request
engine until the barrier resolves. If compare-and-set loses to a higher active
attempt, it aborts the lower uploader without sending bytes. A transient
IndexedDB error retries the same install while leaving the newly created server
resource empty; unrecoverable ledger failure reports a local error and still
sends no bytes, allowing later never-written supersession. 423/503 or a server
refusal does not change the active tuple but durably refreshes the matching
lifecycle's exact cutoff/time when supplied. A changed cutoff for an existing
lifecycle or a 201 missing the required timing headers is a retryable protocol
fault and sends no HEAD/PATCH.

Ledger capability is chosen before the first POST. If `indexedDB` is absent, its
open is blocked/rejected, or the initial record transaction fails before any
server resource exists, the page switches to an explicit in-memory mode. It
generates `clientUploadId`, allocates attempts, retains URL/status, and runs the
same retry/completion logic for that page, but offers no cross-reload resume or
cross-tab election; a stable warning/metric records the degraded mode. This is
the supported compatibility path, not an upload failure. If durable mode was
selected and a 201 resource exists, failure of the active-tuple commit remains
fail-closed with no HEAD/PATCH as above. The live page uses the frozen attempt
locator to request durable cancellation before reporting the local persistence
error. If it crashes first, the attempt remains unbound/empty and is removed by
the bounded creation-orphan rule; a later in-memory lifecycle is independent,
not a supersession claim. Once an active tuple has committed,
later progress/UI ledger-write failures retry while the durable URL remains
authoritative and do not require aborting transmitted bytes. Optional Golden
Retriever blob persistence is never required.

Every attempt/replay/restored tab in a lifecycle sources upload length,
filename, declared SHA, and guest name from that frozen ledger record (or from
the equivalent frozen page record in in-memory mode), never from current Uppy
metadata or the live attribution input. Editing guest attribution affects only
a newly created lifecycle; it cannot mutate or strand an existing one.

Frontend and backend implement the same documented filename/guest
canonicalization limits and normalization; shared fixture vectors assert
byte-for-byte equivalent outputs before lifecycle creation/replay tests.

Status-only entries
remain usable after file bytes are no longer available. Integrate a pinned
`@uppy/golden-retriever` instance with its default IndexedDB storage as
best-effort restoration for small blobs (documented by Uppy as suitable up to
about 5 MiB), metadata, and UI state when browser quota permits. Automatic
large-file restoration is not promised: service-worker reference storage is
outside scope and would not survive browser crashes/restarts. A large or
evicted file restores as status-only/ghost state and asks the user to reselect
it. The Uppy/metadata fingerprint only locates a candidate ledger entry; it
never authorizes resume. Before attaching reselected bytes or PATCHing a
nonzero old URL, the browser recomputes SHA-256 and requires exact equality with
the ledger's stored digest and size. A match reattaches the `File` to the same
ledger lifecycle and guarded tus URL. A mismatch creates a separate ledger
entry with a fresh `clientUploadId` and never sends those bytes to the old URL.
Ledger writes are transactional and terminal
published/duplicate/failed/cancelled entries are removed only after the UI has
consumed the result. If blob persistence is unavailable, the current page can
still upload, and reselecting the same fingerprint reuses the durable ledger
identity rather than allocating a duplicate.

Every asynchronous URL, progress, retry, status, cancellation, and terminal
write carries the ledger primary key/`clientUploadId`, its allocated
`clientAttempt`, and (once known) the physical upload ID. The IndexedDB
transaction first selects that exact record and applies the write only when the
tuple matches its stored active tuple; a delayed lower-attempt callback is
ignored. A terminal result removes the ledger/fingerprint only for the matching
active tuple. Allocation counter updates are separate and never erase a valid
active URL, so a rejected higher attempt leaves the lower accepted attempt
resumable. Cross-tab notifications are hints only; IndexedDB compare-and-set is
authoritative.

The exact server `clientAdmissibleUntilMicros`, not `createdAtMicros` or the
public-config duration, governs lifecycle admission. The client maintains a
relative remaining-time estimate from the latest server-time pair and resyncs
after reload before making a horizon decision. At/after the cutoff it issues no
new no-URL attempt or replacement POST for that lifecycle. A lifecycle with no
active accepted tuple becomes locally expired; reselect/retry requires explicit
user action and a fresh `clientUploadId`. An already accepted active physical
URL is not abandoned merely because the create horizon passed: guarded
HEAD/PATCH resume, lifecycle cancellation, and status polling may continue to a
durable outcome because the server cutoff gates attempt registration, not the
existing resource. A 410 is authoritative and updates the record before the UI
offers a fresh lifecycle. Server controls retain the immutable cutoff, any
terminal outcome, and the longer GC horizon, so late hidden/offline tabs cannot
reopen a cancelled, published, duplicate, failed, or client-expired lifecycle.

The browser upload client explicitly implements server-backpressure retry. The
current `@uppy/tus` default is not sufficient: its 429 delay iterator is shared
across the plugin instance, has a finite five-entry budget here, and does not
honor `Retry-After`; tus-js-client also stops before `onShouldRetry` once its
finite retry-delay array is exhausted. A callback alone cannot meet this
contract.

Implement a thin local Uppy tus uploader/fork with per-file outer retry state.
Before the first request, generate and persist one random `clientUploadId` in
the file's Uppy state/metadata. The outer wrapper models three states:

- **No URL:** initial POST 429/503 or network failure schedules another POST
  with the same `clientUploadId` and next persisted `clientAttempt`; it never
  attempts HEAD. A delayed older physical attempt is superseded and cleaned,
  never reused or allowed to publish.
- **Resumable URL:** the wrapper owns HEAD semantics rather than delegating
  transient responses to tus-js-client. HEAD 423/429/503 retains the URL and
  fingerprint, honors `Retry-After`, and returns to the scheduler. PATCH may
  use ordinary finite inner retries; exhaustion creates a fresh tus object
  bound to the same URL and guarded HEAD.
- **Server completed:** batch status reports processing, published, or
  duplicate. The wrapper never creates another upload even if cleanup makes
  the tus URL return 404. It removes the resume fingerprint only on a durable
  public terminal result, not merely transport completion.

Before replacing a URL after 404/410, query batch status.
Published/duplicate status completes locally; processing/recovering keeps the
existing identity; failed/cancelled terminates. `unknown` waits at least two
reconcile intervals and retries status. Persistent unknown permits a
replacement POST using the same `clientUploadId`; the server completion fence
and transactional duplicate resolver remain authoritative.
That POST uses the next persisted `clientAttempt`; server 423/503 or a
recovering/completed status keeps the prior lifecycle under observation rather
than forcing supersession.

The wrapper continues across any number of finite inner cycles until durable
terminal result or page teardown; one file never consumes another file's retry
budget. User cancellation stops new PATCH scheduling and aborts the active
client request, but is not itself a terminal result. The wrapper transactionally
marks the lifecycle/attempt and matching active tuple when present
`cancellation-requested`. It always sends `POST /api/uploads/cancel` with the
frozen `clientUploadId` and exact allocated attempt, whether or not an active
physical URL is known; it never increments/rotates that locator for
cancellation. A physical ID is only a status/cleanup hint returned by the
server, never the scope of new-client cancellation. The public DELETE alias
remains for tus compatibility and exercises the same lifecycle command.
A no-current-row lifecycle-cancel response is durable public cancelled; a returned physical ID
is stored as status-only and polled. The wrapper retains the ledger/status
poller until the server reports a durable outcome. It retries cancellation on
404 while the dispatched POST is unsettled because cancellation may have raced
ahead of proxy registration; it reports no success. Once the POST has
definitively failed before registration and the edge/create-grace bound has
elapsed with repeated 404, local cancellation may complete only when the ledger
has no active tuple, proving no known lifecycle/resource exists. If any active
tuple exists, 404 never completes locally: the client retries the lifecycle
route and may invoke the physical DELETE compatibility alias, which derives the
same lifecycle/current-row command, plus status polling. It retries 423/429/503 with `Retry-After`; a 2xx
accepted intent polls through `cancelling` to public `cancelled`. HTTP 409 means
pending/publication won, so the UI reports cancellation lost and continues
processing/publication observation. Network ambiguity retries lifecycle cancel
plus status rather than assuming success. Only consumed public `cancelled`,
`failed`, `published`, or `duplicate` may remove the matching ledger/fingerprint.
The custom uploader suppresses Uppy/tus-js-client's fire-and-forget termination
and file removal; Dashboard removal becomes the durable outcome transition.

This requires a pinned local fork/wrapper of the relevant Dashboard/Core
removal path, not only the tus plugin. Every per-file remove button and
cancel-all action invokes the lifecycle cancellation controller before
`Uppy.removeFile`; built-in direct removal/termination is disabled. The entry
remains visible in an explicit cancelling/cancellation-lost/processing state
until the matching durable result is consumed, then the controller calls
`removeFile` exactly once. Cancel-all independently drives every lifecycle and
does not clear Core state eagerly.

The Uppy instance, ledger, retry scheduler, and cancellation controller are
owned by an app-scoped upload service rather than `UploadPanel`. Component
unmount/navigation only detaches Dashboard listeners and pauses visible work; it
does not call `uppy.destroy()`/`cancelAll` and is not interpreted as user
cancellation. Page unload relies on the durable ledger for later continuation.
Explicit application shutdown may destroy the instance only after persisting
all active tuples; it does not issue server cancellation unless the user chose
cancel.

Page teardown with a durable ledger leaves `cancellation-requested` for the
next page and may additionally send a keepalive cancellation request; restart
always resumes it. In page-scoped in-memory mode, cancellation is awaited before
reporting success. An abrupt process/tab crash before the request lands cannot
be recovered client-side, so the unbound-empty creation-orphan rule above
provides the explicit bounded server cleanup; no fresh lifecycle is claimed to
supersede the lost locator.

Integration tests use the pinned libraries, make
one file exceed its full inner retry array, inject prolonged initial POST,
HEAD, and PATCH backpressure, simulate lost create/final-PATCH responses, and
prove no physical attempt is posted twice or publishes after supersession.
They also cover successful small-file IndexedDB restoration and a file above
5 MiB whose blob is unavailable after restart: the latter keeps status/URL
identity, requests reselection, and resumes the guarded lifecycle without
claiming that Golden Retriever persisted its bytes.

The batch status route accepts 1-100 safe opaque upload IDs and always returns
HTTP 200 with one result per ID from a bounded set-based query; request-level
4xx is reserved for malformed batches. It returns no filename, guest, path,
raw error, or uploader data. It has a separate keyed limiter, so status traffic
cannot consume the shared public gallery/media bucket. Queue states map to
`uploading`, `processing`, `published`, `duplicate`, `failed`, `cancelled`,
`recovering`, or per-ID `unknown`. A committed `cleanup`/`complete` job is already `published` or
`duplicate`; cleanup of its tus source does not delay visibility. `mediaId` is
included only when the resulting media row is publicly visible, and the
response separately indicates approval-pending publication.

Internal `discarding`/`discarded` maps by stable `terminal_reason` rather than
leaking queue states. Unsupported type, checksum mismatch, and unrecoverable
source loss plus stale-incomplete retention expiry map to public `failed`;
explicit client cancellation and never-bound creation/orphan repair map to
public `cancelled`. Core
server-side errors remain `processing` because they retry indefinitely. These
terminal responses stop polling. Raw reasons and internal errors remain
private.

Upload IDs are application-generated random 128-bit values returned in the tus
`Location`. Uppy completion handling retains each successful upload ID and
polls in batches of at most 100 IDs, with one in-flight status request per
browser, a minimum two-second interval, jittered backoff capped at 15 seconds,
and no polling while the page is hidden. It continues until publication,
duplicate resolution, terminal failure, or browser teardown. Gallery refresh is
triggered by published IDs instead of the current fixed 45-second schedule;
approval-pending results show the existing awaiting-approval message. Network
errors remain retryable and do not turn a processing upload into failure.

During upgrade cutover, a valid sidecar without a row maps to `recovering` and
wakes reconciliation. Neither row nor valid sidecar maps that element to
`unknown` without hiding other batch results. There is no finite deadline for
known uploading/processing/recovering jobs; browser teardown merely pauses
polling while the IndexedDB ledger and restored Uppy state permit later
continuation.

## Structured Logging

Use stable messages and structured attributes. At minimum, log:

- job enqueued or recovered;
- capacity admission rejection, idle reservation release, and row retention;
- job claimed with lease deadline and current failure counts;
- retry scheduled with next attempt and error;
- dependency circuit open/recovered and retained-source retry;
- media published or duplicate resolved;
- cleanup failure and completion;
- stale temporary/final artifact cleanup and deterministic discard;
- HEIC conversion failure and exhausted fallback;
- HEIC pixel/memory safety-limit outcome;
- ffprobe image/video failure;
- ffmpeg image/video thumbnail failure.

Attributes include `upload_id`, `media_id`, `operation`, and `error` as
applicable. `upload_id` is also the job identity, so new logs do not use a
second ambiguous `job_id`. Attempt attributes name their budget
(`processing_failures`, `conversion_failures`, `cleanup_failures`, or
`discard_failures`) rather than using one ambiguous attempt number. Do not log
media bytes, hook secrets, declared hashes, uploader IPs, or guest names in new
worker logs.

## Load-Test Completion Oracle

The generated PNG payload remains streaming and dependency-free. Add a helper
that computes the exact SHA-256 of the generated byte sequence without
materializing the whole file. Generate one stable random client upload ID per
result and send it with the digest as tus `clientUploadId`/`sha256` metadata on
every create retry for that file. Persist and increment `clientAttempt` before
each new no-URL POST, matching the browser attempt-election contract.

After all tus PATCH requests finish, the harness performs a processing phase:

1. Poll returned upload IDs through bounded `/api/uploads/status` batches,
   surfacing terminal failure without waiting for the global deadline.
2. When integrity verification is enabled, obtain an admin session/CSRF token
  from the same HTTPS `BASE_URL` using `LOADTEST_ADMIN_PASSWORD` supplied only
  through the environment or protected prompt. Keep password, cookie, and CSRF
  token out of command arguments, logs, JSON reports, exceptions, and shell
  tracing. After each ID reaches published/duplicate, call
  `/api/admin/uploads/verify` once for its digest and require
  `verifiedExisting:true`.
  Retry only documented 423/503 backpressure within the processing deadline;
  do not poll successful verification. This proves SQLite publication plus a
  currently readable size/hash-valid original even when moderation approval
  is enabled without making the measured ingest contend with repeated hashes.
3. Fetch public configuration to determine whether approval is required.
4. Paginate `/api/gallery` and match unique generated filenames.
5. When approval is disabled, require every database-complete filename to be
   visible. When approval is enabled, report gallery visibility but do not fail
   the stage solely because rows are pending approval.

The JSON report separates:

- `transport_success` and `transport_failed`;
- `processing_success`, `processing_failed`, and `processing_timeout`;
- `database_success` and `database_missing`;
- `gallery_success` and `gallery_missing`;
- `backpressure_responses` by status and request phase;
- `unexpected_5xx_responses` and rate;
- processing latency median, p95, and maximum;
- per-file transport or processing errors.

429 and 503 responses carrying valid `Retry-After` from the documented
admission/readiness/write gates are tracked as backpressure and excluded from
the unexpected-5xx numerator, but their upload must still complete before the
deadline. Bare 503 and all 500/502/504 remain unexpected failures.

The configured minimum success rate applies to `database_success / count` when
approval is enabled and to `gallery_success / count` when approval is disabled;
transport success alone never passes a stage. Processing timeout and public
terminal failure count as end-to-end failures. The 5xx threshold applies to the
unexpected-5xx rate. Add a processing-timeout option with a production-suitable
default.

Production staged runs require the admin verifier credential and fail before
uploading if it is absent or login fails. All upload/status/gallery/admin calls
retain the harness's existing same-host HTTPS redirect policy; there is no
internal URL override or Compose-only runner requirement. The harness logs out
or discards the cookie jar at completion.

## Testing

### Database and store

- Migration creates all queue fields/indexes and `has_preview`.
- Normal startup against an unbound fresh/pre-0004 database acquires the
  singleton, applies no migration, binds no listener, and reports the initializer
  command. Initializer fault injection after mount validation, sentinel fsync,
  each DDL statement, schema-migration record, and identity insert proves one
  transaction leaves either the exact old schema/no identities or complete
  0004/all identities/audit, never a partial boundary. Rerun converges from
  durable sentinels; current bound databases retain normal migration behavior.
- Startup matrix integration tests assert: pre-0004 unbound has connection
  refused/no health endpoint; current schema with a removed/wrong sentinel has
  `/healthz` 200 but readiness/media/upload/admin mutation 503; current matching
  identities wait for supervised tusd/inventory then become ready. Compose first
  upgrade uses the one-shot initializer rather than expecting a healthcheck from
  the pre-migration app service.
- Migration declares every new time/deadline column as `INTEGER`; store tests
  reject text/mixed-unit helpers and verify numeric ordering at 100/110
  milliseconds, microsecond equality boundaries, negative/pre-epoch values,
  checked duration overflow, and one-now-per-transaction semantics. Lease
  reclaim is false at `now < until` and true only at the documented inclusive/
  exclusive boundary.
- Seed representative 250,000-job/1.5-million-control data and assert
  `EXPLAIN QUERY PLAN` uses `idx_upload_jobs_result_media_nonterminal` for pin
  checks, `idx_upload_jobs_result_media_any` for tombstone-GC reference checks,
  the tombstone primary key/expiry index for late cleanup/proof GC, and
  `idx_upload_jobs_complete_gc`/`discarded_gc` for ordered bounded
  terminal deletion, and `idx_audit_log_action_media_created` before exact
  purge-token/filename verification. Plans containing an `upload_jobs` or
  `audit_log` full scan fail. A 500-ID per-item purge near both caps and a due-GC
  batch remain within configured transaction time, do not hold the writer while
  hashing/networking, and do not make unrelated pre-create or durability commits
  exceed their busy budget. Migration tests verify every named index exists on
  upgrade from 0003 as well as a fresh database.
- Explicit stopped-stack storage initialization cross-checks existing DB/media
  and upload evidence before recording synced kind/UUID sentinels; normal
  startup never auto-bootstraps an empty fallback mount. Later identity mismatch
  blocks all destructive/absence transitions without changing job state.
- Fault injection stops initialization after each fixed-directory creation,
  during sentinel-temp write, after temp fsync, after no-replace publication
  but before root sync, after each root sync, after all files but before the
  database transaction, and after commit. The same command removes only owned
  temps, adopts complete finals, republishes absent finals, and converges;
  malformed final or conflicting kind/UUID cases fail without rewrite. Every
  upload-root scanner ignores exactly
  `.event-gallery-volume-id`, while lookalike names remain ordinary evidence or
  ambiguity and the sentinel never consumes a physical-upload slot/inode count.
- Initializer/runtime inventories treat safe regular `<id>.lock`, `<id>.stop`,
  and `<id>.lock.<safe-temp>` as neutral controls: they neither authenticate nor
  reject the mount and never increment physical-upload count. Unsafe ID/suffix,
  symlink, directory, or lookalike fixtures refuse binding/cleanup. All controls
  remain byte-identical until the authenticated tusd startup gate returns 204.
- Fault sidecar reconstruction after temp create/write/fsync and before/after
  rename. The exact row/durable-artifact-token-owned app temp is accepted but never counted as
  evidence; post-identity recovery promotes or removes it with an upload-root
  sync. Unowned token, malformed/oversized/wrong-type/lookalike fixtures refuse
  and remain byte-identical. A later initializer with matching database/job is
  not stranded, while fresh/empty-root adoption cannot use the temp as proof.
- The shipped entrypoint forwards initializer arguments. Unbound fresh/pre-0004
  startup exits without a listener; current-schema missing/mismatched identity
  keeps shallow `/healthz` live while `/readyz` and all mutation/media/upload
  routes stay closed until remediation.
- Storage initialization verifies every media row, not a sample. A live
  original or unique valid matching purge-stage original satisfies the row;
  after binding, normal reconciliation restores a stage with a surviving row
  or finalizes one whose row is absent.
- Existing-install initialization injects failure at each accepted live/staged
  original file sync, containing-directory sync, fixed-ancestor sync, and
  post-sync same-file/row recheck. Every failure leaves identities uncommitted
  and all copies/stages byte-identical; success commits only after all barriers.
  A crash-image test then resolves a same-SHA upload as a duplicate, removes its
  new prepared copy, reopens the filesystem image, and still finds the legacy
  row's complete original.
- Existing-install initialization also injects failure at every accepted legacy
  tus data/sidecar file sync, upload-root sync, and post-sync descriptor/path/
  metadata/size recheck. Every failure leaves schema/identities uncommitted and
  all artifacts byte-identical. Fixtures cover complete pairs, partial pairs,
  valid one-sided sidecars, and job-owned paths; data-only and ambiguous shapes
  still refuse. A crash-image test commits initialization only after all tus
  barriers, reopens the filesystem image before normal reconciliation, and finds
  every only-copy source/sidecar complete and adoptable. Fault before the final
  barrier proves migration 0004 never commits on an unanchored legacy source.
- An absent-row v2 stage binds/finalizes only with an exact token/media-ID/
  stored-filename purge audit in the candidate database. A v1 stage requires
  its legacy action/media-ID purge audit. The correct database passes; an empty,
  stale-before-commit, or unrelated database leaves the stage untouched and
  refuses binding. Row-present interrupted stages bind and restore without any
  purge audit.
- Fault v2 purge creation after stage sync, partial/full manifest-temp write,
  temp fsync, and final-manifest rename. Before the final manifest no media has
  moved; matching complete temp recovery promotes it, while partial expiry
  removes only the token-matched temp/empty stage and syncs `.purging`. Existing
  v1 stages still load. Token mismatch, a staged media child without final
  manifest, symlink/wrong type, and foreign children refuse binding/recovery and
  remain untouched. Re-running the stopped-stack initializer never treats a
  manifest temp as affirmative media or purge-commit proof.
- Build pre-upgrade Go-1.25 fixtures crashed after v1 `MkdirTemp("purge-")`
  and after `manifest.json.tmp` fsync but before rename. The canonical-decimal
  empty and temp-only directories are tolerated/non-authenticating, remain
  byte-identical through the stopped-stack initializer, permit identity/migration
  commit and purge-dependent readiness, then are removed and `.purging` synced
  only by post-binding runtime recovery. A normal valid v1 final-manifest stage
  still restores/finalizes through its audit rules. Final-plus-temp,
  noncanonical/oversized/wrong-type temp, staged media without final manifest,
  symlink, and foreign-child fixtures remain blocking ambiguity and untouched.
  Tests assert no legacy partial enters the media-ID stage index or yields a
  verifier/duplicate false result.
- A valid rowless legacy tus pair or one-sided sidecar permits binding and is
  adopted only after binding. Malformed/ambiguous purge stages, mismatched
  staged originals, and rowless data-only tus artifacts refuse binding without
  mutation and name the offending path.
- Startup, periodic, and pre-finish no-row adoption each insert one random
  server-only lifecycle, admitted attempt-1 control, and upload job in the same
  immediate transaction. Fault after every insert rolls back all three; a
  concurrent adopter elects one set and leaves no orphan control. Job/control
  cap pressure leaves bytes rowless and retryable. Adopted partial, complete,
  source-loss, deterministic-rejection, retention, and public-DELETE paths then
  publish or close the lifecycle without a zero-row tombstone update; terminal
  reuse is 410 and controls GC only at their fixed horizon.
- Binding accepts zero/one/two tus paths for `cancelling`, `cleanup`, and
  `discarding`, an unmaterialized within-grace `uploading` row, and a
  prepared-only `pending`/`processing` row once the upload root is independently
  proven. Present malformed/mismatched paths always fail. Without an existing
  matching identity or any affirmative job/legacy tus artifact, those absent
  rows produce `insufficient upload-mount evidence`; adding one valid unrelated
  legacy pair permits binding and post-binding reconciliation terminalizes or
  recovers each row.
- A healthy pre-upgrade fixture has media rows with every original valid, zero
  `upload_jobs` rows after migration, and a logically empty tus root. Default
  initialization refuses insufficient evidence; `--adopt-empty-upload-root`
  logs the operator assertion and binds it. Adding any job, data/sidecar,
  malformed/lookalike entry, or media validation failure makes the flagged run
  refuse. Safe neutral locker controls are allowed but remain byte-identical
  until the later identity-authorized tusd scrub.
- Planned upload-volume replacement allows retained complete/discarded history
  but refuses each nonterminal status, a nonempty root, foreign/existing sentinel,
  wrong expected-old UUID, or invalid app/media evidence. Faults after new
  sentinel root-sync and before the conditional transaction converge on rerun;
  the rerun accepts only matching `replaces_uuid` provenance and rejects a
  missing/different predecessor. Faults after commit are already matched. The successful test verifies one
  atomic uploads-UUID change plus an old/new UUID `ActionConfig` audit and that
  unflagged initialization still refuses the mismatch.
- Loss-mode replacement fixtures include cleanup after source unlink,
  cancelling before materialization, pending with a markerless prepared final,
  processing with a valid original temp, and written uploading/pending with no
  surviving copy. Unacknowledged replacement refuses; acknowledged replacement
  audits the exact affected set, leaves all rows/artifacts unchanged during
  init, and after binding reaches complete/cancelled/published/failed through
  the state-specific recovery paths. Prepared files remain byte-identical until
  publication ownership commits; first-scan or transient absence never fails a
  job.
- Fresh/existing initialization creates and syncs every fixed media directory
  and then `MEDIA_DIR`; startup cannot become ready before the same check.
  Fault-order tests prove `.purging` and its first stage are parent-synced
  before the first original rename, so crash recovery can always discover it.
- Initialization creates/parent-syncs the exact empty regular app-instance lock
  and treats it as a neutral fixed control; missing is repairable after app
  identity validation, while symlink/wrong-type/nonempty/lookalike entries fail.
  Two app processes on the same app root prove only one can pass the lifetime
  `flock`; the loser exposes no startup authorization/readiness/upload route and
  cannot reclaim create/write/transport leases until the owner exits.
- Every initializer mode attempts that same flock before database open,
  inventory, or sentinel mutation. A serving app/held-lock fixture makes
  default init, both adoption flags, planned replacement, and loss-acknowledged
  replacement refuse with byte-identical roots/database. First-upgrade tests
  also prove the old containers are stopped before invoking new tooling.
- Pause each mode after precheck/sentinel fsync and mutate the database from a
  fault injector before its immediate commit: new media/job rows invalidate
  fresh/adoption modes; a new or transitioned nonterminal job invalidates
  planned replacement; and any ID/status/prepared/result change invalidates the
  acknowledged set. Each transaction rolls back identity/audit changes, and
  rerun safely adopts or rejects the provenance-bearing sentinel.
- Exact empty in-range stripe files plus one empty `capacity.lock` are neutral
  on rerun; missing fixed controls are created and parent-synced, while
  nonempty/out-of-range/wrong-type/lookalike lock entries fail. Matching job-owned original and
  derivative temps bind without mutation and feed post-binding recovery.
  Unowned derivative temps/thumbnails/previews are tolerated only beside a
  fully authenticated media-row/original set and are later reconciled; they
  disqualify a fresh root. Unowned regular originals refuse by default. A
  pre-upgrade zero-job fixture with safe UUID/extension/magic orphan originals
  binds only with `--accept-legacy-orphan-originals`, logs every path, and
  leaves them byte-identical across startup/reconciliation. Unsafe names, magic,
  symlinks, non-regular files, and all unowned original-kind ingest temps refuse
  even with the flag and remain untouched.
- Pre-create capacity admission serializes concurrent reservations, accounts
  correctly for shared versus separate filesystems, and returns a stable random
  physical ID for each accepted attempt.
- A fault test pauses admission after `statfs`, advances PATCH/publication, and
  proves the global capacity lock/full-obligation ledger cannot over-admit.
- An exact replay of the current `(clientUploadId, clientAttempt)` gets 423
  while its creation lease is live; after the lease clears and the resource is
  validated, the proxy replays the same 201/Location without forwarding to
  tusd. With a nonempty source, data and sidecar remain byte-identical and the
  tusd test double observes no second `NewUpload`.
- Exact replay independently mutates size, canonical filename, declared SHA,
  and guest name; each returns 409 before synthesis/forwarding and leaves row,
  data, sidecar, and original uploader audit IP unchanged.
- A greater attempt atomically capacity-admits different physical IDs and
  supersedes only a never-written current attempt. Commit-before-response and
  delayed old-file creation leave the old attempt noncurrent/cancelling; its
  artifacts are discarded and it never publishes or truncates the new paths.
- Out-of-order/lower or noncurrent attempts are stale. Concurrent higher
  attempts elect only the greatest committed attempt as current, with at most
  one current row under the partial unique index.
- Proxy registration creates lifecycle attempt 1 with admitted watermark zero;
  pre-create admits that exact registered attempt and atomically advances the
  watermark. Concurrent registration of attempt 2 before attempt-1 hook entry
  makes hook 1 stale and admits only hook 2. Faults between registration,
  admission, job insertion, and response prove no failed transaction advances
  `max_admitted_attempt` and no physical ID is issued twice.
- A real pre-upgrade POST with no lifecycle metadata is proxy-assigned a random
  lifecycle, gets registered controls, and is admitted by pre-create; the old
  bundle uploads successfully. A retry receives a different random lifecycle
  by design, and identical completed bytes converge through transactional
  duplicate resolution. Missing/invalid proxy injection cannot reach the hook.
- A nonzero source, `write_started_at`, durability evidence, active/later queue
  state, or failed capacity admission prevents supersession. The completion
  fence promotes/retries the old attempt, and a failed election leaves it
  unchanged.
- Terminal lifecycle reuse returns 410; the ledger rotates to a new client ID.
- Publication, duplicate resolution, explicit cancellation, definitive source
  loss, deterministic rejection, creation orphan, and retention expiry each
  close the current lifecycle atomically with their state transition. Force
  terminal job GC at day 30, then submit a higher attempt before control GC at
  day 45; every closed outcome remains 410 and cannot republish purged content.
  Attempts near day 30 do not extend `client_admissible_until` or `expires_at`,
  and an otherwise-open lifecycle returns 410 exactly at the admission horizon.
  Discarding an older `is_current=0` attempt leaves the newer current lifecycle
  open. SQL constraint tests reject half-populated closure/cancellation states.
- Reconciliation cannot terminalize or repair a row before
  both creation grace and the proxy-heartbeated create lease expire; fault
  injection pauses tusd before data creation and between data and sidecar
  creation beyond the fixed grace.
- PATCH progress telemetry never reduces the full source/media obligation;
  concurrent writes and failed telemetry remain conservatively reserved.
- Post-finish, periodic reconciliation, and rowless complete adoption cannot
  promote to `pending` without funded media capacity in the same transaction.
- Per-IP count/bytes, create rate, and idle reservation release bound cheap
  create-only starvation without reducing the configured venue concurrency or
  incomplete-retention window.
- The default reservation cap admits the existing 60-way load stage, and
  429/503 admission responses carry `Retry-After` and remain retryable in Uppy
  and the battle harness.
- Idle reservation release does not delete the tus source; a resumed PATCH
  either reacquires capacity or receives retryable backpressure.
- Inactive rows remain durable while their tus source exists; HEAD remains
  resumable and a later PATCH reactivates the same identity.
- Pre-finish promotion is idempotent and does not reset later job states.
- Concurrent claims return each job to only one worker.
- Conditional transitions from a stale lease token affect no rows.
- Startup immediately requeues and invalidates unexpired worker claims only;
  create/write/transport tokens survive until their type-specific safe reclaim
  tests pass.
- Runtime expiry makes abandoned claims eligible without allowing stale writes.
- Core, HEIC conversion, cleanup, and discard counters advance independently.
- Queue residency, restart, core retry, and lock reschedule do not consume the
  HEIC conversion failure budget; only executed transient conversion attempts
  do.
- Media insert/duplicate resolution and cleanup transition are atomic.
- Publication does not hit a deferred-transaction busy snapshot under a
  concurrent unique-hash race.
- A self-match preserves committed artifacts; a different-ID duplicate removes
  only paths not owned by any media row.
- Duplicate resolution validates the conflicting row's regular original by
  stored filename, size, and SHA through an open descriptor outside the stripe,
  then holds the purge-shared media stripe for same-file/row recheck, open-file
  sync, originals-directory sync, post-sync recheck, and publication. Injected
  file/directory sync or recheck failure retains both the new prepared copy and
  tus source and commits no duplicate cleanup. Missing,
  corrupt, or unreadable authoritative originals retain the new prepared copy
  and source in retryable state; a conflict discovered only inside publication
  rolls back and enters the same validation path.
- The public legacy check returns 200/`duplicate:false` without lookup and
  ignores attempted verification fields. The admin verifier rejects anonymous,
  expired-session, cross-origin, and missing/wrong-CSRF requests before taking
  a semaphore slot or touching media; `TUS_HOOK_SECRET` is rejected/not used.
  On cache miss it returns
  `verifiedExisting:true` only after outside-stripe file validation plus short
  locked same-file/row recheck; repeated matching-stat requests hit the TTL
  cache without reopening/hashing but still take the short stripe and recheck
  row/path/stat key, while inode/mtime/ctime/size/path-row changes force
  revalidation.
  Missing, wrong-size, and
  wrong-hash originals return false; transient I/O returns retryable 503. A
  concurrently purge-staged original either validates after restore or observes
  the committed row deletion, never momentary absence. Purge immediately after
  a validated response changes the stat key; no stale cache result survives.
  Limiter tests prove anonymous requests cannot occupy verifier slots.
- Pause purge after staging and invoke cached-true, cached-false, and cache-miss
  verification: each waits/returns 503 on the stripe, then observes restore or
  committed deletion. No branch emits an integrity alert from staged absence or
  returns true after the row deletion commits.
- Repeat with a crash-left stage and pause runtime reconciliation before and
  during restore/finalize. The reconciler holds the manifest media stripe before
  its row/audit read; verifier and duplicate resolution wait/503 rather than
  report missing/corrupt media. A concurrent online purge cannot delete the row
  while restore is pending, and after release observes either fully restored
  row/file or committed deletion with no resurrected unowned original. The
  absent-row tokenized finalize path has the same serialization. A pre-held busy
  stripe leaves stage bytes and database unchanged until the next pass.
- Restart with a row-present crash-left stage and pause reconciliation before it
  attempts the stripe. Startup's stage index is complete before readiness; a
  verifier cache miss/hit and duplicate candidate miss each acquire the free
  stripe, find/revalidate the indexed stage, return retryable recovery without a
  false cache or critical integrity log, and wake the paused reconciler. Remove,
  corrupt, or duplicate the indexed stage between scans and prove ambiguity is
  retryable/noncached rather than false. Routes remain closed when initial
  stage indexing itself is incomplete or malformed.
- A 500-ID bulk purge with repeated and colliding stripe indexes holds at most
  one deduplicated media stripe and cannot self-deadlock. A busy stripe or a
  duplicate-pinned item is restored/reported in `retry` while unrelated IDs commit and appear in
  `changed`; fatal per-ID failures are named without undoing committed IDs.
  Retention skips busy items and successfully revisits them next interval.
- Self-publication writes `result_media_id=media_id`; duplicate cleanup requires
  non-empty different IDs plus the hard ownership check.
- Forced transaction failure leaves no media row and no cleanup transition.
- Terminal rows expire in bounded batches, SQLite can reuse their pages, and
  app-data floor plus independent job/control-cap admission prevents unbounded
  queue-history growth.
  Deletion requires both `physical_slot_active=0` and non-null
  `artifacts_cleaned_at`; a transient temp/final scan error keeps the row.
- Startup rejects overflow or configurations where the physical cap does not
  fit under the job cap, the control cap is below six times the job cap, or the
  uncertain-create rollover reaches the physical cap. At default limits,
  50,000 open one-attempt uploads fit one job plus two controls each; 250,000
  closed one-attempt jobs fit transient controls before bounded compaction and
  retain one lifecycle tombstone each. Three-attempt lifecycle fixtures over
  the 45-day horizon stay below the 1.5-million control cap.
- Closing a lifecycle makes only its attempt rows eligible for bounded early
  compaction; replay/cancel remains 410/idempotent from the tombstone through
  fixed expiry, while an open resumable lifecycle loses no control. Fill only
  the job cap and only the control cap in separate tests: each returns named
  retryable 503 without consuming/corrupting the other budget, emits 70/85/95
  percent alerts, and succeeds after a configured cap increase or eligible GC.
  Two 20,000-upload event waves inside both retention windows remain admitted
  under defaults; no manual SQLite deletion is part of recovery.
- Temp creation performs token recheck plus `O_EXCL` open while holding the
  short upload stripe. Crash/stale-worker tests prove a clean terminal scan
  cannot be followed by a newly created job temp. Late temp/final/source
  discovery atomically clears the marker and re-enters cleanup/discarding before
  retention; cleanup restores the marker only after directory-synced proof.
- Leave a sidecar-repair temp across `uploading -> cancelling` and
  `uploading -> pending`. Each transition clears the active token/lease but
  retains the equal durable artifact token, so restart inventory still owns the
  basename. Fault after temp fsync, active-lease clear, rename/unlink,
  upload-directory sync, and artifact-token clear; recovery promotes/removes or
  clears the token idempotently. Terminal cleanup cannot set
  `artifacts_cleaned_at` while the temp/artifact token remains and clears both
  proof fields together only after synced absence. A new repair is rejected
  until prior artifact resolution. SQL rejects an active token unequal to the
  artifact token and a clean marker with nonnull artifact ownership. A post-GC
  lookalike is not recognized or removed and triggers the documented ambiguity
  alert.
- Byte, inode, and physical-upload floors count rowless/two-file tus artifacts;
  retention zero can backpressure admission but cannot exhaust the filesystem.
- Physical upload count is refreshed outside admission, accepted rows reserve
  future inodes/slots, and a stale cache can only under-admit while tusd path
  creation is paused.
- Accepted rows remain physically charged across terminal state while creation
  completion is uncertain. Delayed data/sidecar materialization under
  complete/discarded never raises total admission headroom: either the row slot
  was retained or cache discovery transfers its exception charge atomically to
  the reactivated row. Count drops only after leased termination, upload-dir
  sync/revalidation, and conditional slot clear. Terminal retention skips
  charged rows.
- Late artifacts under `complete` atomically reactivate the slot and enter
  `cleanup`; status remains the original published/duplicate result, media is
  not reprocessed, pre-terminate authorizes the fresh cleanup token, and verified
  absence clears the slot and returns to `complete`. Crash tests cover discovery
  before/after transition, tus termination, and slot clear without leaking the
  row or changing `result_media_id`.
- Purge a self-published retained `complete` result and assert one transaction
  inserts one matching media tombstone, deletes the media row, and records the
  v2 audit without updating retained jobs. Inject late tus/final artifacts:
  cleanup consumes the exact tombstone+audit, removes reappeared paths, clears
  the slot, and returns to historical `complete` without restoring media. Pause `complete -> cleanup`
  against purge in both commit orders; cleanup-first blocks/restores purge,
  purge-first supplies the tombstone. Missing/mismatched tombstone or audit
  remains an integrity retry. Populate 250,000 complete duplicate jobs for one
  media ID: purge still performs one tombstone insert plus media/audit writes,
  with no mass UPDATE and bounded WAL/writer hold. Exact-token replay is
  idempotent; conflicting tombstone refuses. Expiry alone cannot delete proof
  while any result job or purge stage references it; after both disappear,
  expiry-index GC removes it in a bounded batch. SQL rejects invalid size/time
  tombstones.
- A lost 201 after tusd creates a valid zero-offset data/sidecar pair is
  reconciled as `create_finished_at` after lease/grace expiry; later cleanup can
  clear its slot after verified absence. Path absence alone never sets that
  marker.
- A paused old-boot `NewUpload` cannot justify slot release. Tests replace tusd,
  commit a new boot ID, complete the root inventory, and only then clear the
  old-boot absent liability; old-process simulation cannot create artifacts
  afterward. Crash points around cache discovery, row reactivation, cleanup,
  and slot clear prove there is always at least one charge.
- Same-boot create outcomes distinguish a complete upstream response from
  client cancellation, proxy timeout, truncated response, and network error.
  Only the complete response records `create_concluded_at`; ambiguous outcomes
  stay charged despite lease/grace expiry and directory-synced absence. Pause
  tusd after pre-create and prove the handler can still create paths until a
  fresh boot kills it. A delivered authenticated `post-create` instead validates
  the resource, records `create_finished_at`, and permits ordinary cleanup even
  when 201 delivery is lost. At the configured ambiguous-row threshold, tests
  assert admission drain, active-forward cancellation, supervisor TERM/KILL,
  new boot commit/inventory, and only then slot release. Late-artifact injection
  before the boot fence never increases admission headroom.
- Exactly `INGEST_LOCK_STRIPES` stripe files plus one `capacity.lock` cover all
  cross-process locks; terminal-row churn does not create more lock inodes.
  Instrumentation proves capacity and hashed stripes are never nested, hashed
  stripes never span network or whole-file work, upload precedes media, and no
  lock is re-entered. Two distinct upload IDs forced onto one stripe continue
  concurrent PATCH transport; only their bounded final namespace commits may
  reschedule, and admission/terminal reservation release make forward progress.
- The opened database reports WAL plus `synchronous=FULL`; lowering durability
  fails configuration/startup tests.

### Hook and recovery

- Pre-create reserves capacity; blocking pre-finish syncs and promotes before a
  final PATCH can return success, but performs no media processing/removal.
- After a forced pre-finish failure leaves tusd offset complete, subsequent
  HEAD/already-complete PATCH is intercepted before tusd, receives 503 until
  the durable marker commits, and never creates `.stop` or cancels pre-finish.
- tusd runs with `-hooks-http-retry=0`, `-hooks-http-timeout=90s`,
  `-network-timeout=90s`, and the 10-second request-completion grace. A real
  tusd test holds pre-finish beyond the old 70-second effective ceiling and
  proves the 75-second app response budget still relays 503/`Retry-After` before the
  90-second hook and 100-second edge deadlines; startup rejects any reordered
  timeout configuration.
- Fake-clock/unit tests delay authentication/body decode, path validation, and
  SQLite lookup before executor submission; each delay reduces the join's
  remaining budget. The hook returns its 503 envelope at the original
  request-entry deadline and never receives a fresh 75 seconds. Equivalent
  proxy-fence tests include slow preliminary checks in the same absolute budget.
- Saturation/wait-expiry pre-finish tests make the hook endpoint itself return
  2xx with an `HTTPResponse` 503/`Retry-After`, assert tusd relays those values
  on the final PATCH and emits only the harmless post-finish wake, and contrast
  malformed/auth/internal non-2xx hook failures that fail closed as bare 500.
- A canceled hook context cannot cause source deletion.
- Invalid storage paths never enqueue and never delete files.
- A complete unqueued sidecar is recovered at startup.
- Startup binds the listener but remains non-ready during bounded metadata-only
  legacy inventory; shallow `/healthz` stays healthy for gallery ingress while
  `/readyz` and upload routes report not ready.
- Legacy incomplete sidecars are classified funded or unfunded before
  readiness; unfunded/rowless sidecars cannot advance PATCH until admission.
- Incomplete sidecars remain owned by stale-upload retention.
- tusd does not advertise or accept concatenation.
- Creation-with-upload and `X-HTTP-Method-Override` are rejected; every data
  write traverses the funded PATCH gate, limiter, and bandwidth reader.
- Public OPTIONS advertises exactly `creation,termination`; it strips
  creation-with-upload, deferred-length, concatenation, and unsupported
  upstream extensions while preserving truthful tus version/max-size headers.
- The proxy strips client-supplied secret, client-IP, and
  lease-token headers before setting its own values.
- Public DELETE can terminate an incomplete upload but cannot terminate a
  complete upload in any queue state or with no row.
- Client cancellation and stale-incomplete cleanup commit `discarding` with a
  termination lease before tusd unlinks; active create/write leases reject
  termination, and a resumed PATCH cannot race the claim.
- Cancellation and pre-finish promotion race one conditional transition:
  cancellation-first blocks publication and drains to discard; pending-first
  returns cancellation conflict and preserves the published upload.
- Pause cancellation during pre-finish validation before durability
  registration and during the promotion UPDATE. Cancellation-first produces the
  immediate lifecycle-ended hook result, frees the durability slot, wakes
  discard, performs no 75-second wait/no 10-minute continuation, and never
  reports generic backpressure. Pending-first joins success. Two simultaneous
  last-byte cancellations do not exhaust the two durability workers or pressure
  unrelated pre-create admission.
- A complete admitted source promotes with its existing media reservation and
  does not re-reserve. A complete legacy/recovered source blocked on funding
  returns 503 only while a durability operation is active; once idle, explicit
  cancellation atomically wins `uploading -> cancelling`, reaches public
  cancelled, and releases the source. If funding/promotion commits `pending`
  first, cancellation returns 409 and observes publication.
- Maintenance claims `cancelling` immediately after writer/create lease expiry
  and drives discard, but retains incoming capacity while transport ownership
  is uncertain; no cancellation row remains stuck once that owner is fenced.
- Pause a PATCH after its final token check with write/transport heartbeats
  expired, commit cancellation, and submit another create sized to fit only if
  the old incoming reservation were released. The create remains backpressured,
  same-boot transport expiry cannot authorize DELETE, and resuming the PATCH
  loses its status/token/reservation check, stops body/upstream forwarding, and
  cannot append or relay success. Normal owner cleanup, or process death plus a
  fresh authorized tusd boot and two clean lock-absence observations, permits
  the terminate claim. Only DELETE plus upload-directory sync and post-sync
  data/sidecar absence releases the charge, after which the waiting create can
  pass. Repeat from an idle row whose reservations were inactive and prove the
  cancellation transaction first reactivates full incoming liability without
  rejecting the user's cancellation.
- Internal DELETE requires the exact active cleanup/discard lease token.
- A stale token is rejected even while the row remains in an authorized state.
- Pre/post-terminate callbacks complete while the owning worker holds job/
  transport leases but no hashed stripe, proving callbacks do not acquire
  stripes or touch files and network I/O remains outside hashed critical sections.
- An unexpired write lease or any exact transport token prevents idle release;
  a merely expired same-boot token remains charged. A second concurrent write
  gets 423, and only normal token clear or the fresh-boot lock-absence fence
  restores safe progress/release.
- Fault injection makes a write-lease heartbeat affect zero rows and return
  SQLite busy/I/O errors; each case cancels the tusd PATCH, never relays
  success, and a later guarded HEAD/PATCH recovers from the persisted offset.
- On the final PATCH, pause after body EOF before pre-finish, after
  `uploading -> pending`, before/after each write/transport heartbeat, and before
  response copy/flush/token clear. Pre-EOF checks remain uploading-only; after
  promotion the same exact tokens relay one final success despite pending,
  processing, cleanup, complete, discarding, or discarded advancement, then
  clear both tokens. Token
  replacement, noncurrent election, missing completion marker, or
  cancellation-first prevents relay. Concurrent complete HEAD takes its own
  exact transport lease, accepts the durable completion-derived marker, returns
  completion, and performs no body write. A failed token-clear retries without
  turning durable completion into transport failure or permitting another tusd
  lock acquirer under the stale token.
- Repeat with pre-finish selecting 503/`Retry-After` before detached promotion.
  Relay preserves that non-2xx under exact tokens whether the row remains
  uploading or commits pending before response copy; it never relays 2xx without
  `source_completed_at`. Deterministic processing may reach discarding while a
  valid 2xx is in flight and still relays transport completion, after which
  status reports failed. If the operation is still active at 503 flush, assert
  atomic `responseDone` transfer, uninterrupted manager heartbeat of both
  deadlines, and 423 for a retry until operation commit/timeout clears token,
  deadline, boot, and operation fields atomically. Crash during the transfer
  requires the fresh-boot reclaim path, never lease-expiry reuse.
- Pause PATCH A's detached durability work before fsync and before promotion;
  after responseDone, fault-inject loss/replacement of A's ownership and install
  PATCH B's shared operation token. A's next heartbeat/pre-fsync check or exact
  two-column/operation/boot promotion affects zero rows and returns ownership-
  ended; only B may promote/relay. Repeat by changing only one column in a fault fixture and
  assert startup/store invariants reject or fence the split token pair. Hook
  tests prove the proxy strips a client header, injects the shared PATCH token,
  tusd forwards that exact value, and no second implicit token channel exists.
- A real-tusd long PATCH plus concurrent HEAD bursts forwards exactly one
  lock-taking operation for that physical ID; contenders receive proxy 423
  and never create `.stop` or extra lock-temp inodes. The writer is not
  interrupted, reaches the expected offset, and the upload root never exceeds
  data + sidecar + one hard-linked lock inode + optional one stop inode in this
  no-repair fixture. A concurrent sidecar-repair fault fixture adds exactly one
  row/durable-artifact-token-owned temp inode, never two, and remains within the reserved fifth
  inode through create/fsync/rename/crash cleanup.
  Equivalent overlapping app-process contention is serialized by the exact
  transport lease. Killing the forwarding app while tusd still holds/waits for
  its lock leaves the lease; the replacement app returns 423 until lease expiry
  plus two clean lock-absence checks, creates no `.stop`/extra temp, then
  forwards successfully. Injected stale-app heartbeat or stat ambiguity blocks
  reclaim. Lease timing validation rejects values not greater than acquire
  timeout + completion grace + margin.
- Suspend app A after transport-token commit but before dialing tusd. App B
  cannot acquire the process-lifetime singleton lock, cannot serve, and cannot
  reclaim A's token; resuming A performs the final token check and forwards as
  the sole owner. Killing A releases the singleton lock, after which B still
  observes transport expiry and tusd lock-absence barriers before forwarding.
  A mismatched app-data root cannot bypass this because storage identity blocks
  startup authorization and upload routes.
- Two different physical upload IDs forced onto the same hashed stripe transfer
  PATCHes concurrently because transport ownership is exact-ID/DB-leased, not
  stripe-based. A colliding worker's long hash/copy/decode also cannot 423 an
  unrelated upload; hashed collision is observable only at bounded final-path
  commits.
- Public DELETE during a long PATCH commits app-local cancellation intent
  without claiming transport ownership or forwarding to tusd; no `.stop`/second lock temp
  appears, the next write-lease heartbeat aborts the writer, and only the later
  leased discard worker issues internal tusd DELETE after claiming exact
  `transport_operation='terminate'`. Internal cleanup/discard forwarding uses
  that matching transport token and completes without
  self-deadlock or a second tusd acquirer.
- Browser cancellation tests override the pinned Uppy default: a 2xx lifecycle-cancel
  keeps the file/ledger visible as cancelling until status returns consumed
  cancelled; 423/429/503 and network loss retry without local success. If
  pending wins and cancel returns 409, the UI shows cancellation lost and keeps
  polling to published/duplicate. Page restart in cancellation-requested state
  resumes lifecycle-cancel/status, and no fingerprint/ledger removal precedes the matching
  durable terminal result.
- Race no-URL cancellation against creation-orphan maintenance in both commit
  orders. Orphan-first atomically closes `cancelled`; the later cancel route is
  idempotent 2xx and batch status agrees. Cancel-first follows the same discard
  reason. In contrast, stale-incomplete retention expiry closes `failed` and a
  later cancel returns 409/cancellation-lost with matching failed status.
  Unsupported content, checksum mismatch, and source loss exercise the same
  failed mapping. A superseded noncurrent attempt's cleanup changes neither the
  current lifecycle outcome nor its public status.
- Tests use the shipped Dashboard 5.1.1 interaction paths against the local
  wrapper: per-file remove and cancel-all never call Core `removeFile` before a
  consumed durable result, entries remain visibly cancelling through 404/423/
  503, and 409 switches to cancellation-lost/processing. Component unmount and
  remount preserve the app-scoped Uppy/controller and send no DELETE; actual
  page reload restores from the ledger. Exactly one eventual Core removal is
  asserted for each terminal lifecycle.
- No-URL cancellation tests pause before pre-create, after row insertion, after
  tusd resource creation with a lost 201, and after 201 before active-tuple
  commit. The exact seen-attempt capability closes the lifecycle, prevents
  delayed/future creation, and drives any current uploading physical ID through
  cancelling; no test reports success before durable cancelled/cancellation-
  lost. Page teardown
  resumes from the durable request. With IndexedDB unavailable, awaited
  in-memory cancellation succeeds when the page lives; an abrupt crash leaves
  only an unbound zero-offset attempt, which the independent idle orphan janitor
  removes even when incomplete retention is zero.
- Reorder cancellation ahead of the dispatched create POST: initial 404 does
  not complete locally, proxy registration arrives, retry commits lifecycle
  cancellation, and delayed pre-create is rejected. When the POST is proven never registered,
  repeated 404 through the edge/create-grace bound permits local cancellation
  without allocating a control row.
- Cross-tab cancellation supplies a seen higher attempt that has no job while a
  lower current attempt is uploading: one transaction closes the lifecycle and
  moves the lower current row to cancelling. A pending/later current row returns
  409 and remains published/processing. After lifecycle cancellation, attempts
  above `max_seen_attempt_at_cancel` are also rejected 410, and no tab can
  consume cancelled while another attempt later publishes.
- Give tab A a stale attempt-1 URL, let tab B admit attempt 2 as current, then
  cancel from tab A before its IndexedDB view refreshes. The wrapper posts the
  lifecycle capability and atomically moves attempt 2 to cancelling; attempt 2
  never PATCHes/publishes. Repeat by sending public DELETE to stale physical ID
  1: the compatibility alias derives the lifecycle and produces the identical
  current-row result while old ID 1 remains in ordinary discard. If attempt 2
  reached pending first, both routes return 409 and preserve publication.
- Repeat with an allocated higher attempt that never reached the proxy. Because
  the lifecycle row exists, cancellation closes it and cancels the lower current
  upload without inserting an attempt-control row. If the cancel request returns
  404 but the ledger has any active tuple, the client retries lifecycle cancel/
  status and may invoke the lifecycle-aware DELETE alias; it never reports
  local cancelled. Repeated-404 local completion
  is tested only with no lifecycle and no active tuple.
- Fake server/client-clock tests run 7-, 30-, and 60-day configured client
  horizons against the same bundle. Create success, synthesized replay,
  retryable create, terminal 410, lifecycle cancel, and known batch status all
  return the identical immutable cutoff plus current server time; IndexedDB
  stores it before any PATCH. Public config changes only the provisional hint.
  Client clocks skewed by days use `cutoff - serverNow`, resync after reload,
  and never reconstruct from `createdAtMicros`. At the exact boundary, a
  lifecycle without an active tuple stops no-URL attempts and requires explicit
  fresh ID; a previously admitted active URL still resumes/statuses/cancels to
  terminal. Hidden/offline tabs receive authoritative 410 on reconnect.
  Cancelled/closed controls still exist through the 45-day server horizon;
  control cleanup refuses while a retained job references the lifecycle, and no
  expired browser can recreate a closed ID. Unknown responses leak no cutoff.
- Pre-finish fsync/commit completes before final transport success; failure
  keeps the resumable URL and timeout/crash is recovered by reconciliation.
- Durability worker/queue bounds hold under a completion burst; saturation
  returns immediate 503/Retry-After and feeds pre-create admission pressure.
- Complete no-row pre-finish delivery uses funded recovery rather than
  returning a hook error or creating an unfunded pending job.
- A full regular data file with missing/malformed sidecar is synced, funded,
  and published rather than deleted. A partial regular data file reconstructs
  a canonical sidecar atomically; ambiguous paths remain recovering.
- With both tus paths absent, a never-written expired creation records
  `discarding` and becomes public cancelled only after discard's directory
  verification. A row with either write/completion timestamp remains
  recovering after the first ENOENT scan and maps public failed only after the
  complete durable source-loss protocol; EIO/ESTALE/reappearance never
  terminalizes it. Existing cancelling/cleanup/discarding rows continue only
  through their own leased verification paths.
- Every adopted positive-size source records `write_started_at`; every adopted
  complete source records `source_completed_at` in its funded promotion.
- Periodic sidecar work respects the page budget/cursor; known upload wakes are
  immediate and a 50,000-upload full sweep completes within 25 intervals.
- Missing/mismatched storage identity keeps tusd non-listening: validated
  `.lock`, `.stop`, lock-temp, data, sidecar, sentinel, and malformed-lookalike
  fixtures remain byte-identical. The authorization endpoint rejects a wrong
  supplied UUID even when its secret is valid.
- Startup authorization conditionally writes the random boot ID only on the
  matched uploads `storage_volumes` row; app/media rows reject non-null boot
  IDs. Restart changes that value before 204, and physical-liability cleanup
  reads the committed generation rather than process memory.
- With all identities matching, the app starts independently, authorizes the
  observed UUID, and only exact regular locker controls are removed and the
  upload directory synced before tusd listens. Symlink/wrong-type/lookalike
  entries remain untouched, tusd becomes healthy, startup inventory runs, and
  only then does `/readyz` pass; Compose startup has no health dependency cycle.
- Tusd supervisor tests start a child, record the returned app-process boot, and
  then crash/replace or suspend the app. Three failed/mismatched heartbeats
  TERM/KILL/reap the child and exit the container; restart generates and commits
  a different tusd boot before any stale transport reclaim. Pause an old tusd
  handler before `lockUpload`: app replacement cannot reclaim under the same
  boot, supervisor restart kills the handler, and only the fresh boot plus
  lock-absence checks permit forwarding. Signal forwarding and child reaping
  are asserted under Compose stop grace.
- Killing and restarting the real tusd container while PATCH and termination
  locks are held proves authorized startup removes only stale validated
  `.lock`/`.stop`/lock-temp artifacts and both uploads recover.

### Worker and crash windows

- Processing uses no hook context and obeys the worker bound.
- A transient processor or SQLite failure retains source and sidecar.
- Retry reuses deterministic artifacts without duplicate media rows.
- Crash recovery resumes immediately without waiting for an unexpired lease.
- A stale worker cannot publish, schedule retry, clean, or discard after its
  token is invalidated.
- A paused stale process cannot create/rename a final artifact after a new
  process has performed cleanup because the shared lock stripe spans both
  operations.
- Startup/periodic reconciliation removes stale derivative `.ingest-*` temps
  and only conclusively redundant/invalid original temps, syncing directories.
- Publication commit followed by cleanup failure remains in `cleanup`.
- Cleanup removes sidecar only after committed publication.
- A simulated tusd crash between data and sidecar unlink is repaired; terminal
  transition requires both absent across a directory sync.
- Duplicate jobs clean only their own deterministic artifacts and tus source.
- Duplicate resolution pins its authoritative media row; concurrent permanent
  purge restores/stops while pinned, or purge wins first and the new job
  publishes itself. At least one original/row always survives.
- Duplicate/deterministic-discard final-artifact unlink uses persisted
  `stored_filename` and is followed by media-directory sync;
  a crash cannot resurrect an orphan after its cleanup marker commits.
- Core context/SQLite/filesystem failures retry indefinitely and never reach
  source deletion. Shared dependency outages do not consume per-job counters;
  recovery wakes jobs and publishes their preserved sources.
- Optional helper outage is dependency-specific: HEIC advances finite fallback,
  ffprobe/ffmpeg remains best effort, and unrelated jobs keep publishing.
- Two data-path ENOENT scans on a healthy mount are insufficient alone:
  source-loss commits only after successful upload/originals directory barriers
  and post-sync revalidation finds no data, final, or original-temp candidate.
  Injected directory-sync errors remain retryable; a path/temp appearing before
  revalidation is recovered. Transient EIO/ESTALE never terminalizes.
- Crash injection after final rename, after originals-directory fsync, and
  before the prepared-marker commit then removes the tus source. Recovery
  validates the markerless final from persisted filename/SHA, backfills the
  marker, and publishes it; injected stat/read/hash errors retain it for retry.
- Crash injection after an original temp is fully synced/closed and its parent
  directory is synced, but before rename, then removes the tus source and
  invalidates the old lease. Startup retains the temp; the next fenced worker
  validates, promotes, directory-syncs, marks, and publishes it.
  Partial/mismatched temps are removed only after conclusive checks, while
  injected I/O ambiguity retains them. A fault-order test proves the temp is
  never advertised as recoverable before the pre-rename parent sync completes.
- Repeat while the tus source remains present. A fully anchored matching temp is
  promoted and reused rather than copied again. Pause stale worker A during a
  partial/full write after its path opens, expire/reclaim its job lease, and try
  worker B in the same process: the writer registry makes B reschedule and the
  media namespace never contains a second original temp. Kill A's process;
  after singleton takeover the empty registry lets B validate/promote the full
  candidate or remove a conclusively partial one with directory sync before
  opening exactly one replacement. Ambiguous/changing candidates retain the set
  and block another copy. Instrumentation asserts at most one production
  original temp path/inode and at most `expected_size` original-temp/final bytes
  throughout each phase; derivative work cannot start until candidate resolution
  leaves one validated final and zero original temps. Fault-injected multiple
  candidates close admission conservatively and converge without creating a
  third.
- A synced prepared original, with or without its persisted prepared marker,
  survives tus-source loss and becomes the publication source; source-loss or
  discard never deletes the last validated copy.
- Deterministic discard retries artifact/source cleanup until `discarded` is
  impossible while an unowned final artifact remains.

### HEIC and previews

- A synthetic HEIC generated with `heif-enc` produces a JPEG preview and
  thumbnail through the real `heif-preview` helper and libheif.
- Preview and thumbnail dimensions respect their configured maxima.
- Original bytes and SHA-256 remain unchanged.
- Environmental conversion failures retry on the short schedule; typed
  deterministic failures and bounded exhaustion publish without derivatives.
- A multi-image HEIC renders its declared primary image even when it is not the
  first top-level item; a rotated primary renders in display orientation.
- Invalid helper metadata or missing, empty, or undecodable output is treated
  as a conversion failure.
- Source dimensions remain full-resolution while the derivative is bounded;
  best-effort ffprobe capture time is preserved.
- Oversized primary dimensions and per-process memory pressure are bounded and
  never destabilize the Go server.
- Helper address-space and parser security limits apply before libheif reads a
  metadata-heavy hostile file.
- Oversized AVIF/video and excessive ffprobe/ffmpeg output stay within launcher
  memory/pixel/thread/log limits and publish as deterministic derivative skips.
- Oversized valid PNG/JPEG/GIF/WebP headers skip pure-Go derivatives before
  allocation; weighted decode admission prevents a poison-image restart loop.
- `/preview` serves JPEG when present and original HEIC when absent.
- `/download` always serves the original HEIC.
- Purge stage restore/finalize includes preview files and remains compatible
  with stages that have no preview.
- Frontend image slides use `/preview`; videos continue using `/file`.
- Frontend upload completion polls opaque upload IDs through processing,
  recovering, approval-pending, duplicate, failed, and cancelled states through
  bounded batches beyond the old 45-second window.
- Pinned Uppy integration survives more than five 429 responses across
  concurrent files, makes one file exceed its entire inner retry array, honors
  423/429/503 `Retry-After`, and succeeds through a fresh resumable outer cycle
  after prolonged backpressure without sharing retry budgets. Transient HEAD
  423/429/503 retains the same URL/fingerprint and never sends replacement POST.
- IndexedDB ledger is written before first POST, survives component teardown,
  restores status polling and the same client ID/URL, and is removed only after
  a consumed durable terminal result. A lost create response may leave a
  superseded physical attempt, but tests prove paths are never reused, that
  attempt never publishes, and exactly one lifecycle result is returned. A
  lost final response resumes/status-polls the same physical URL.
- Concurrent tabs allocate distinct monotonic attempts. If a lower 201 or its
  cancelled/failed status arrives after a higher 201 became active, every stale
  URL/state/terminal write is ignored and the higher upload keeps polling. If
  the server rejects the higher attempt, its response does not replace or erase
  the lower accepted active URL. Only a matching active tuple can remove the
  ledger or tus fingerprint.
- Lost-create and restored-tab tests change the live guest-name input after the
  first POST. Every retry/new physical attempt in that lifecycle sends the
  frozen canonical guest/filename/size/SHA and does not receive immutable-
  metadata 409. Choosing the edited attribution explicitly starts a new
  `clientUploadId`; in-memory mode applies the same freeze rule for its page.
- Pause/crash after 201 but before active-tuple commit, inject IndexedDB commit
  failure, and make the CAS lose to a higher accepted attempt; each case sends
  no HEAD/PATCH and the created resource remains offset zero/safely
  supersedable. The success case proves the durable tuple commits before the
  first PATCH byte. A page restart after the commit resumes/polls that physical
  ID; a restart before it allocates a new attempt without orphaning bytes.
- Missing IndexedDB API, blocked/rejected open, and initial pre-POST transaction
  failure each select page-scoped in-memory mode; upload/retry/status polling
  succeeds in that page and emits the degraded metric without promising reload
  recovery. Separate degraded tabs use distinct client lifecycles and converge
  through server duplicate resolution. After a durable active-tuple commit,
  injected progress/UI write failure retries without erasing the URL or
  interrupting PATCH. The existing post-201 active-tuple failure case remains
  offset zero and sends no upload request.
- The new browser uploader makes no `/api/uploads/check` request and always
  retains the selected file through durable ingest. A pre-upgrade tab receives
  200/`duplicate:false` and also uploads. If purge deletes a previously valid
  copy while either client proceeds, transactional processing publishes the
  client source instead of losing the only copy.
- Golden Retriever restores a small IndexedDB-backed file when quota permits;
  a blob above 5 MiB becomes a reselectable ghost/status entry after restart
  and resumes through the ledger without an automatic-persistence claim.
- Ghost reselection tests use equal filename/MIME/path/size/mtime metadata with
  different bytes. Digest mismatch sends no HEAD/PATCH to the restored URL and
  creates a fresh client lifecycle/primary-key record under the same non-unique
  fingerprint while preserving the old active URL/status record. Delayed
  callbacks mutate only their own `clientUploadId` record. Byte-identical
  size/SHA plus identical canonical filename/guest reselection may
  transactionally choose and resume the fenced existing lifecycle; changed
  immutable attribution or an explicit new-upload choice creates a separate
  lifecycle.

### Logging and load test

- Captured structured logs contain ffprobe/ffmpeg failure operation and IDs.
- Reservation rejection, stale artifact cleanup, HEIC safety outcomes, and
  cleanup/discard retry logs carry the named counter and stable IDs.
- Payload hash helper matches bytes emitted by the streaming body factory.
- Completion polling distinguishes transport, database, and gallery results.
- Admin-authenticated `/api/admin/uploads/verify`
  `verifiedExisting:true` in the completion oracle proves
  both the committed SHA row and its currently readable size/hash-valid
  original at that snapshot; a row-only or corrupt result cannot pass the
  database phase. One successful verification per digest is asserted, and
  verifier I/O is reported separately from ingest processing latency.
- Harness tests enforce same-origin HTTPS, fail-fast missing/bad admin login,
  CSRF/session handling, and redaction: password/cookies/tokens never appear in
  argv, logs, exceptions, subprocess traces, or JSON output. No hook secret or
  internal-network URL is used.
- Structurally valid mixed batches always return one result per ID, including
  per-ID unknown, without hiding known terminal results behind request 404.
- Hundreds of unresolved IDs use bounded 100-ID batches, one in-flight request,
  the minimum poll interval, and a separate limiter without 429ing gallery or
  media requests behind one venue NAT.
- Status polling reports a terminal processing failure without waiting for the
  overall timeout.
- Approval-required mode does not mistake pending rows for processing loss.
- Missing database/gallery items fail the appropriate load-test threshold.
- Documented 429/503 with `Retry-After` is reported as backpressure rather than
  unexpected 5xx; bare 503 and 500/502/504 still fail the 5xx gate.

### Verification commands

Run backend unit and race tests, frontend tests/build, Python load-test unit
tests, and a Docker build/integration pass with real libheif tools. The final
container must run as the existing non-root UID/GID with a read-only root
filesystem and writable `/tmp` tmpfs.

## Compatibility and Rollout

- Migration is additive and preserves all existing media rows with
  `has_preview=0`.
- Migration/deployment rollback is not image-only. After migration 0004 or any
  new queue activity, no pre-0004 app or tusd image may start against the
  upgraded app/media/uploads volumes: the legacy post-finish path does not
  understand durable jobs and may move/delete their only source. Before first
  upgrade, the operator stops the stack and captures one mutually consistent
  backup of app data (including WAL state), media, uploads, and all sentinels,
  plus records the exact old app/tusd image digests. Rollback means stopping all
  new containers, restoring all three volumes from that same pre-upgrade backup,
  and starting the matched old image pair. Partial-volume restore or tag-only
  rollback is refused by the runbook and unsupported.
- First-upgrade instructions verify the backup, stop/verify both legacy
  containers, and run the new `init-storage-identities` command, which validates
  mounts then atomically applies migrations through 0004 plus identity rows.
  They start the new pair and perform readiness/oracle checks before declaring
  the backup retirement window. Any pre-commit initializer failure leaves the
  legacy schema version unchanged and is retried with new tooling; any failure
  after the migration/identity commit stays on new tooling for remediation or
  restores all three pre-upgrade volumes before old images start. A rollback integration test queues partial,
  complete-retrying, prepared, and cleanup jobs, proves old images are never
  launched on upgraded volumes, then restores the snapshot and verifies the old
  stack sees exactly the pre-upgrade database/media/tus state.
- Startup adopts every valid pre-upgrade partial or complete tus sidecar before
  readiness. Complete no-row pre-finish delivery uses the same path, and
  status reports `recovering` during the brief cutover window.
- Existing JPEG, PNG, GIF, WebP, AVIF, and video routes retain their behavior
  except image lightboxes use the preview endpoint, which falls back to the
  original.
- Existing HEIC rows remain without previews. Backfilling them is intentionally
  outside this change; new HEIC uploads use libheif.
- Existing version-1 purge recovery stages remain readable; previews are
  optional, and absent-row finalization uses the legacy same-transaction purge
  audit. New version-2 stages add the tokenized commit proof.
- Existing installs whose legacy tus root is drained run the documented
  stopped-stack initializer with `--adopt-empty-upload-root`; installs with
  affirmative legacy tus evidence use the default upload-root path. If the
  media inventory reports safe legacy orphan originals from the old move-before-
  insert window, the operator additionally supplies
  `--accept-legacy-orphan-originals`; the command prints the exact protected
  paths before requiring that explicit rerun.
- A later lost/recreated empty upload volume is replaced only with the logged
  `--replace-upload-root --expected-old-upload-uuid <uuid>` command after all
  nonterminal jobs drain; retained terminal status rows do not require manual
  deletion. If the old volume is irrecoverably lost before drain, the same
  command requires `--acknowledge-lost-upload-data` and prints the jobs/copy
  evidence that post-binding reconciliation will recover or fail.
- tusd enables exactly `pre-create`, `post-create`, `pre-finish`, `post-finish`,
  `pre-terminate`, and `post-terminate`, and disables concatenation.
  Incomplete cancellation remains supported, while a lease token gates
  deletion of complete uploads.
- Singleton tusd restarts scrub only identity-authorized, validated stale
  locker controls before serving; rollout must not overlap old and new tusd
  containers.
- Deterministic rejected content is discarded after its durable audit/state
  transition. Core server failures retry indefinitely and retain completed
  sources; terminal complete/discarded rows remain for the configured 30-day
  status/idempotency window.
- The incoming tus volume is no longer transient once a completed source is
  queued. README and architecture backup instructions change to require a
  stopped-stack, mutually consistent backup and restore of app data, media,
  and tus uploads. Omitting uploads explicitly abandons queued/retrying
  jobs and is not a complete backup.
- Capacity documentation states the envelope: incoming storage holds active
  and completed-unpublished/retrying sources; media storage holds
  published media plus at most one prepared original per worker/job. The
  deployed upload and media paths are separate bind mounts, so capacity and
  verification assume a full copy rather than hard-link deduplication. Byte,
  inode, and physical-upload floors gate admission.
- Terminal queue rows are retained for 30 days and deleted in bounded batches;
  separate job/control caps and the app-data free-space floor gate new upload
  creation, with closed-attempt compaction and threshold alerts as above.
- Service count, bind mounts, and public hostname do not change. Compose changes
  app dependency ordering from tusd-healthy to tusd-started, passes the existing
  `TUS_HOOK_SECRET` to tusd, and uses that same secret for the internal startup
  authorization endpoint; no new secret is introduced.

## Decisions

- Queue: SQLite with leased in-process workers.
- Creation: durable `uploading` reservation with conservative filesystem
  admission before tusd allocates bytes.
- Creation retries: one durable client lifecycle elects monotonically numbered
  fresh physical attempts; exact-request replay is idempotent, while delayed
  superseded attempts can only be discarded.
- Queue identity: tus upload ID; no separate job UUID.
- Concurrency: conditional claims, random fencing tokens, and immediate SQLite
  writer transactions, plus a fixed set of kernel-backed lock stripes around
  final filesystem operations and one global capacity lock spanning statfs and
  reservation commits/releases.
- Worker bound: configurable, default two.
- Preview size: configurable, default 2560px longest edge.
- Pure-Go image decode: `DecodeConfig` pixel admission and a shared weighted
  512 MiB memory budget; over-limit images publish without thumbnails.
- HEIC conversion exhaustion: publish original without derivatives after six
  transient conversion failures or a typed deterministic outcome; do not
  reject HEIC.
- HEIC decoder: resource-limited `heif-preview` helper linked to pinned
  libheif, followed by Go JPEG encoding.
- Core failures: retry indefinitely with dependency circuit breaking; only
  deterministic client rejection/cancellation discards a completed source.
- Reservation abuse: conservative full-source/full-media obligations, byte,
  inode, physical-upload, create-rate, and per-source bounds; 30-minute idle
  accounting release with durable inactive identities; tus source deletion
  still follows the configured incomplete-retention policy.
- Source termination: exact lease-token authorization, tusd normal deletion,
  stale-lock startup recovery, path verification, directory sync, and bounded
  post-publication one-sided repair. Pre-publication full data is recovered,
  not deleted for missing metadata.
- Transport scope: concatenation disabled; resumable create/PATCH/HEAD/DELETE
  behavior retained.
- Backups: app, media, and tus upload mounts are one stopped-stack backup set.
- Queue history: complete/discarded jobs retained 30 days under a 250,000-job
  cap; lifecycle/attempt controls use a separate 1.5-million-row cap, fixed
  45-day tombstone horizon, and bounded closed-attempt compaction; app-data
  retains a 1 GiB free-space floor.
- Browser backpressure: a local outer tus cycle resumes from retained URLs and
  honors Retry-After for 423/429/503 beyond each inner finite retry budget.
- Processing status: bounded 100-ID batches, one in-flight poll per browser,
  minimum cadence, and a separate venue-safe limiter.
- No-preview HEIC browser behavior: try the original through the preview route.
- Original downloads: always untouched source bytes.