# Event Gallery Architecture

## 1. System summary

Event Gallery is a single-event, self-hosted photo/video gallery. Guests use one public page to upload and browse media without accounts. A password-protected admin page manages optional upload approval, trash, upload expiry, audit history, text, and colors.

The design optimizes for:

- reliable mobile uploads over an ordinary internet connection;
- simple single-host Docker operation and backup;
- no public inbound host ports;
- minimal infrastructure: one Go app, tusd, SQLite, and filesystem storage.

The runtime is event-agnostic. Its shipped branding preset uses wedding wording,
but admin-managed plain text and colors can represent any event.

It deliberately does **not** provide multi-tenant accounts, horizontal scaling, high availability, or distributed media processing.

## 2. High-level design

```mermaid
flowchart LR
    Guest[Guest browser]
    Admin[Admin browser]
    CF[Cloudflare edge]

    subgraph Host[Single Docker / Portainer host]
        Tunnel[cloudflared]

        subgraph EdgeNet[edge network]
            App[Go app :8080<br/>API + SPA + tus proxy]
        end

        subgraph UploadNet[internal uploads network]
            Tusd[tusd :1080<br/>resumable transport]
        end

        DB[(SQLite<br/>gallery.db)]
        Media[(Media mount<br/>originals + thumbnails)]
        Incoming[(Tus incoming mount<br/>partials + sidecars)]
    end

    Guest -->|HTTPS| CF
    Admin -->|HTTPS| CF
    CF --> Tunnel --> App
    App --> DB
    App --> Media
    App --> Incoming
    App -->|proxy /api/tus| Tusd
    Tusd --> Incoming
    Tusd -->|pre-create / pre-finish / post-finish hooks| App

    CI[GitHub Actions] -->|multi-arch images| Registry[GHCR]
    Registry -->|pull/redeploy| Host
```

No host ports are published. `cloudflared` reaches the app on the `edge` network. Only the app bridges `edge` and the internal-only `uploads` network, so browsers cannot reach tusd directly.

## 3. Components

### React SPA (`frontend/src`)

- `App.tsx` selects guest or admin UI from the URL path; there is no client router.
- `UploadPanel.tsx` uses Uppy + tus-js-client with 8 MiB chunks, retries, resumability, file restrictions, and whole-file SHA-256 duplicate preflight.
- `Gallery.tsx` / `useGallery.ts` own cursor pagination, sorting, infinite scroll, post-upload polling, and background item merging.
- React Photo Album lays out thumbnails; YARL provides the image/video lightbox, swipe, keyboard navigation, and native video controls.
- Guest name and a casual per-device like ID live in browser `localStorage`; neither is authentication.
- `AdminApp`, `AdminDashboard`, and `AdminBrandingEditor` implement login, pending-upload approval, moderation, audit/config views, and plain-text/color customization.
- `api/client.ts` is the same-origin JSON API boundary and attaches the device ID plus admin CSRF header when required.

The production Vite build is embedded into the Go binary. Unknown non-API paths fall back to `index.html`, enabling direct `/admin` loads.

### Go application (`backend/cmd/server`, `backend/internal`)

The app owns:

- public gallery, media, like, config, and duplicate-check APIs;
- admin authentication, sessions, CSRF, moderation, audit, expiry, and branding APIs;
- the reverse proxy from public `/api/tus` to internal tusd `/files`;
- tus hook validation and completed-upload ingestion;
- MIME sniffing, SHA-256, EXIF/ffprobe metadata, video rotation, thumbnails, and filesystem placement;
- SQLite migrations and all application state.

Global middleware adds panic recovery, JSON request logging, security headers, and trusted-proxy-aware client IP resolution.

### tusd

`tusd` is the resumable transport layer. It stores upload data/offset sidecars in the incoming mount, enforces maximum size, and calls the app for:

- `pre-create`: reject invalid size or missing filename before creation, and record the durable job row;
- `pre-finish`: block until the completed source and its sidecar are fsynced and the job is promoted to the queue;
- `post-finish`: nudge the ingest workers.

Processing itself is never done in a hook. `pre-finish` only makes the source
durable; a worker pool then derives artifacts and publishes the row, so the
response a guest waits on never depends on ffmpeg.

The app injects a shared hook secret and resolved client IP into proxied requests; tusd forwards them to hooks. The proxy also rewrites tusd's internal `Location` response to the public same-origin `/api/tus/{id}` route.

### SQLite and filesystem

SQLite uses WAL, foreign keys, a 5-second busy timeout, and embedded ordered migrations.

| Storage | Contents |
|---|---|
| `/data/app` | `gallery.db`, WAL, SHM |
| `/data/media/originals` | canonical uploaded originals |
| `/data/media/thumbnails` | generated JPEG thumbnails |
| `/data/media/.purging` | recoverable staging for permanent trash deletion |
| `/data/tusd-incoming` | partial/completed tus files and `.info` sidecars before ingestion |

Core tables:

- `media_items`: metadata, unique SHA-256, active/trashed status, and nullable approval timestamp;
- `upload_jobs`: the durable ingest queue -- one row per upload, from `uploading` through `pending`/`processing` to a terminal state, with lease, attempt, and retry columns;
- `likes`: unique `(media_id, device_id)` likes;
- `audit_log`: best-effort administrative/upload history;
- `admin_sessions`: server-side session and CSRF tokens;
- `app_config`: upload expiry and atomic branding JSON.

## 4. Main request flows

### Browse and view

1. SPA requests `/api/config/public` and `/api/gallery`.
2. Go reads active media from SQLite using an opaque cursor and upload-time or capture-time sort.
3. Grid loads generated thumbnails.
4. Lightbox streams originals; Go uses `http.ServeContent`, so video byte-range seeking works.
5. Downloads use the original attachment endpoint.

### Upload

1. Browser streams SHA-256 computation in 8 MiB slices.
2. `/api/uploads/check` always answers "not a duplicate"; the server settles duplicates after the upload instead.
3. Uppy creates a tus upload through `/api/tus/` and sends 8 MiB PATCH requests.
4. The app applies per-IP request, concurrent-PATCH, and bandwidth policies, then proxies to tusd. `pre-create` records an `uploading` job row, refusing the upload when the media volume or free space is unproven.
5. tusd persists chunks and offsets, allowing HEAD-based resume after interruption.
6. On the final PATCH, `pre-finish` fsyncs the completed source and its sidecar and promotes the job to `pending`. Only after that does tusd acknowledge the upload, so an acknowledged upload is one the server has committed to finishing.
7. A worker claims the job under a lease, copies the source into media storage, and fsyncs it. Preparation only reads the source: nothing moves or deletes it while the job is being prepared.
8. Images receive EXIF orientation/capture-time handling and JPEG thumbnails. Videos receive ffprobe metadata (including display rotation) and ffmpeg thumbnails.
9. Publication is one transaction that inserts the media row and finishes the job; the SHA-256 uniqueness constraint settles duplicates. Only once it commits is the tus source removed.
10. Checksum mismatch and unsupported content are terminal; everything else retries with capped exponential backoff, indefinitely. `POST /api/uploads/status` reports where each upload stands.
11. With moderation off, the SPA polls and merges processed items without remounting. With moderation on, guest polling/backoff is a no-op and the uploader shows an awaiting-approval confirmation.

Client-side hashing is optional optimization; server sniffing, hashing, and SQLite's unique SHA constraint are authoritative.

### Admin

1. Password-only login is rate-limited and compared in constant time.
2. A random server-side session is stored in SQLite.
3. The browser receives a Secure, HttpOnly session cookie and a CSRF token; every mutating admin request must echo the token in `X-CSRF-Token`.
4. Admin actions bulk-approve pending media, change media status, update upload expiry/branding/moderation, or read audit data.
5. Disabling moderation and approving every pending row is one SQLite transaction, so uploads cannot remain pending after the toggle completes.

Trash starts as a **soft database status change**. Pending and trashed media use authenticated admin thumbnail routes and return 404 from public media/like routes. Admin purge or the retention janitor atomically stages files under `.purging`, deletes the trashed row plus audit entry, then removes staged files; startup reconciliation restores or finalizes interrupted purges.

## 5. Reliability and protection

- tus provides resumable, retryable chunk transport; 8 MiB requests stay below common reverse-proxy body limits.
- Maximum file size is enforced by the browser config, app pre-create hook, and tusd.
- Content type is determined from magic bytes, not filename or client MIME.
- SHA-256 is recomputed by the server; SQLite uniqueness closes concurrent duplicate races.
- Upload expiry blocks only new upload creation; existing uploads, browsing, and downloads continue.
- Approval is off by default. When enabled, new completed uploads are admin-only until approved; disabling it atomically publishes all pending media.
- Per-IP token buckets, PATCH concurrency, and bandwidth controls are process-local and intentionally generous for guests sharing venue NAT.
- Limiter/session cleanup and one bounded storage janitor run in background goroutines; media processing runs in a durable SQLite-backed queue with leases, capped retries, and a startup reconciler, not inline in tus hooks.
- A guest's source file is never moved or deleted until its artifacts are fsynced and its media row is committed, so no failure between the two loses the upload.
- `/readyz` reports whether this instance can accept uploads: startup recovery finished and the media volume proven. `/healthz` stays a shallow liveness check, so a storage fault refuses uploads without taking read-only gallery serving offline.
- The storage janitor purges expired trash and terminates stale incomplete uploads through tusd's internal DELETE endpoint. Retention can be disabled with zero-valued settings.
- App/tusd containers use read-only roots, dropped capabilities, `no-new-privileges`, non-root Compose identities, and writable bind mounts only where required.

## 6. Deployment and operations

GitHub Actions runs Go tests/vet/build and frontend lint/typecheck/tests/build, then publishes `amd64` and `arm64` app/tusd images to GHCR. Portainer deploys the Git-backed Compose file by pulling those images; immutable commit/release tags are preferable to `latest`.

Health checks are intentionally shallow:

- app: SQLite ping via `/healthz`;
- app upload readiness: `/readyz`, which is deliberately **not** the container healthcheck -- it fails while uploads are refused, and pulling the instance for that would also stop the gallery it can still serve;
- tusd: `/metrics` response;
- cloudflared: no repository-defined health check.

Operational visibility is JSON stdout logs, Portainer container state/logs, SQLite audit rows, and the guarded production tus load-test harness under `loadtest/`.

For a consistent backup:

1. stop the stack;
2. back up app-data, media, and tus uploads together;
3. preserve deployment secrets/configuration and image revision separately;
4. restart the stack.

Restarts are not instant on a large backlog. The app runs its startup inventory
before it begins waiting for signals, and that inventory fsyncs every recovered
source, so a `SIGTERM` arriving while it runs is not acted on until it
finishes -- and if that outlasts the 30s `stop_grace_period`, Docker kills the
container instead. It costs nothing but time: recovery commits nothing partway
through and simply repeats on the next boot. It is most likely on the first
boot after upgrading, which has the largest backlog to adopt. Uploads are
refused with a retryable 503 for that window (`/readyz`), while the gallery
serves normally throughout.

The tus upload volume is **not** transient. Once an upload completes, its
source file is the application's only copy until the media row is committed, so
a backup that omits this volume abandons queued work. Back up app data, media,
and tus uploads together from a stopped stack.

Rollback is not image-only. After migration 0004, a pre-0004 app must not run
against these volumes: its `post-finish` path does not understand durable jobs
and can delete their only source. Roll back by stopping the new containers,
restoring all three volumes from one pre-upgrade backup, and starting the
recorded old image pair.

## 7. Scaling and tradeoffs

This is a KISS single-node design.

**Strengths**

- few moving parts and low idle resource use;
- no exposed host ports;
- strong resumable upload path;
- simple, consistent stop-the-stack backup;
- browser, API, transport, and processing responsibilities are clearly separated.

**Limits**

- SQLite, local mounts, and in-memory limiters assume one app replica;
- no HA, object store, external queue, distributed locks, or worker autoscaling;
- concurrent completions can amplify hashing, copying, image decode, and ffmpeg work;
- retention cleanup is periodic/bounded, so disk capacity still requires monitoring between passes;
- whole-file browser hashing saves duplicate bandwidth but costs phone CPU/battery;
- original images/videos are served directly, so lightbox bandwidth can be high.

A larger multi-event service would separate API, object storage, metadata database, and asynchronous media workers. That complexity is intentionally deferred here.

## 8. Known gaps

- Media filesystem changes and SQLite inserts are not one transaction, but the ordering makes that survivable: artifacts are written and fsynced before the row is inserted, and a startup reconciler plus the queue's retries resolve anything interrupted. An unreferenced artifact is removed by the same pass rather than left as a silent orphan.
- A one-sided tus artifact belonging to a condemned job -- a data file whose sidecar is gone, or a sidecar whose data file is gone -- is unlinked by the ingest cleanup stage, which runs only after publication has committed or a terminal decision has been recorded. Residue with no job row, and sidecars that cannot be parsed, are still retained for manual inspection rather than unlinked on a guess.
- Purge recovery depends on valid manifests in the media `.purging` directory; corrupt stages are logged and left untouched.
- `/healthz` does not verify media/upload mount writability, free space, ffmpeg, tusd reachability, or tunnel connectivity; `/readyz` covers only the first two.
- Audit writes are best effort and are not an authoritative transaction log.
- Like/device identity is client-asserted and intended only for casual deduplication.
- When approval is off, post-upload appearance is polling-based; `POST /api/uploads/status` reports per-upload state, but the SPA does not consume it yet.
- Rolling back to a pre-approval binary while pending rows exist would expose them because old queries do not understand `approved_at`; rollback requires the matching pre-migration backup.
- Branding defaults are duplicated in Go and TypeScript and must remain synchronized.

## 9. Source map

| Concern | Primary files |
|---|---|
| Startup/config | `backend/cmd/server/main.go`, `backend/internal/config/config.go` |
| Routes/middleware/auth | `backend/internal/httpapi/server.go`, `middleware.go`, `auth.go` |
| Public/admin API | `public.go`, `admin.go`, `branding.go` |
| Tus proxy/hooks | `tus_proxy.go`, `tus_hooks.go`, `deploy/tusd-entrypoint.sh` |
| Durable ingest queue | `backend/internal/ingest/*`, `backend/internal/store/upload_jobs.go` |
| Media processing | `backend/internal/media/*` |
| Database/store | `backend/internal/db/*`, `backend/internal/store/*` |
| Guest/admin SPA | `frontend/src/App.tsx`, `components/*`, `hooks/*` |
| Deployment/CI | `Dockerfile*`, `docker-compose.yml`, `.github/workflows/containers.yml` |
| Production load test | `loadtest/README.md`, `loadtest/tus_battle.py` |
