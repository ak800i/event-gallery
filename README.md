# Event Gallery

A private, self-hosted photo and video gallery for a single event. Guests open
one mobile-friendly page, enter a display name, upload media, and browse the
gallery without creating an account or installing an app. A password-protected
admin area provides moderation, branding, and gallery settings.

The default branding is wedding-oriented so a fresh deployment works
immediately for that common use case. Administrators can replace all listed
main-page text and colors for any event without rebuilding the application.

## Features

### Guest gallery

- Remembers each guest's display name on their device and attributes uploads to
  that name.
- Displays photos and playable videos in an infinite-scroll gallery and
  lightbox.
- Sorts by upload time or EXIF capture time.
- Supports per-device likes and individual original-file downloads.
- Accepts only configured image and video MIME types and enforces a configurable
  maximum file size.

### Reliable large uploads

- Uses the tus resumable upload protocol with 8 MiB requests, safely below
  Cloudflare's 100 MB request-body limit.
- Retries failed chunks automatically and resumes interrupted uploads instead
  of restarting the file.
- Calculates a whole-file SHA-256 hash in the browser to skip known duplicates,
  then verifies the completed file again on the server before storing it.
- Limits public requests, concurrent uploads, and upload bandwidth per client
  IP.

### Administration

Open `/admin` and sign in with the password supplied through
`ADMIN_PASSWORD`. There is no username. An administrator can:

- optionally require approval for new uploads before they appear publicly;
- review and bulk-approve pending uploads;
- select one or more files and move them to trash;
- browse, restore, or permanently delete trashed files;
- review uploads, deletes, restores, logins, and configuration changes in the
  audit log;
- set an upload expiry date. After expiry, browsing and downloads remain
  available, but new uploads are rejected;
- customize all listed main-page text and choose the full page color palette
  with a live preview and one-click reset to defaults. Custom text is
  always rendered as plain text; arbitrary HTML and CSS are not accepted.

Trash is initially reversible: items are excluded from public routes and can be
restored. Trashed media is permanently purged after the configured retention
period (30 days by default), or immediately through the admin Trash tab.

## Architecture

The production stack contains three containers:

- **app**: the Go API, React frontend, SQLite database access, media processing,
  authorization, and upload proxy;
- **tusd**: resumable upload transport, available only to `app` on an isolated
  Docker network;
- **cloudflared**: outbound Cloudflare Tunnel connection to the public
  hostname.

No host ports are published. Cloudflare sends traffic to `http://app:8080`, and
guests cannot reach `tusd` directly. SQLite lives on the app-data mount;
originals and generated thumbnails live together on the media mount.

## Deploy on a Docker host

The supplied `docker-compose.yml` runs on a standard Linux Docker host. It is
ready for a Portainer Git stack and can also be used with ordinary Docker
Compose. It pulls multi-architecture images from GHCR and restarts containers
automatically after a host reboot.

Host paths and container identity are Compose environment variables, not
hardcoded deployment values:

- `APP_DATA_PATH`, `MEDIA_PATH`, and `TUS_UPLOAD_PATH` are required host paths.
  The `/data/app`, `/data/media`, and `/data/tusd-incoming` paths inside the
  containers are fixed.
- `PUID` and `PGID` select the numeric user and group used by both `app` and
  `tusd`. They default to `1000:1000` if omitted and should be set to an account
  that can access all three host paths.

All paths and IDs below are examples; replace them with values appropriate for
your Docker host.

### 1. Prepare persistent directories

For example, create these directories on the host:

```text
/srv/event-gallery/app
/srv/event-gallery/media
/srv/event-gallery/uploads
```

The first directory stores original media and generated thumbnails. The `app`
directory stores SQLite data. The `uploads` directory holds
incomplete tus uploads and does not normally need to be backed up.

Ensure the configured numeric identity can read, write, and traverse all three
directories. For the default `PUID=1000` and `PGID=1000`:

```sh
sudo chown -R 1000:1000 /srv/event-gallery
```

### 2. Create the Cloudflare Tunnel

In Cloudflare Zero Trust:

1. Create a remotely managed Cloudflare Tunnel.
2. Add the public hostname guests will use.
3. Set its service type to **HTTP** and its URL to `http://app:8080`.
4. Copy the tunnel token.

Cloudflare terminates TLS, so use the public HTTPS hostname. Do not publish a
host port for `app` or `tusd`.

### 3. Configure the Portainer stack

In Portainer, create a stack from this Git repository and select
`docker-compose.yml`. Add the following environment variables in the stack
configuration:

```dotenv
# Use an immutable sha-<40-character-commit> or release tag from GHCR.
APP_IMAGE_TAG=sha-REPLACE_WITH_FULL_COMMIT_SHA

APP_DATA_PATH=/srv/event-gallery/app
MEDIA_PATH=/srv/event-gallery/media
TUS_UPLOAD_PATH=/srv/event-gallery/uploads

PUID=1000
PGID=1000
TZ=UTC
UMASK=022

# Set independent secret values. Do not commit them to this repository.
ADMIN_PASSWORD=REPLACE_WITH_A_STRONG_PASSWORD
TUS_HOOK_SECRET=REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS
CLOUDFLARE_TUNNEL_TOKEN=REPLACE_WITH_THE_TUNNEL_TOKEN

# 5 GiB; accommodates long high-resolution phone videos.
MAX_UPLOAD_BYTES=5368709120

# Automatic storage lifecycle. Set either retention to 0 to disable it.
TRASH_RETENTION_DAYS=30
TUS_INCOMPLETE_RETENTION_HOURS=48
STORAGE_CLEANUP_INTERVAL_MINUTES=60

# Change this if it overlaps another Docker or LAN subnet.
EDGE_SUBNET=172.30.0.0/24
```

For Docker Compose, save these values in a protected `.env` file beside the
Compose file and run `docker compose up -d`. In Portainer, enter the same values
in the stack environment.

Generate independent secrets locally, for example with
`openssl rand -hex 32`. `ADMIN_PASSWORD` must contain at least 8 characters and
`TUS_HOOK_SECRET` at least 16. Never commit actual passwords or tokens.

The GHCR packages must be public, or Portainer must have a registry credential
that can pull them. Prefer an immutable `sha-...` or release image tag rather
than `latest` so redeployments remain repeatable.

### 4. Deploy and verify

Deploy the stack and confirm that `app`, `tusd`, and `cloudflared` are running
and that the first two report healthy. Then test from a phone on mobile data:

1. Open the HTTPS public hostname and save a guest display name.
2. Upload a photo and a video larger than 500 MB.
3. Interrupt the video upload, reload, and confirm that it resumes.
4. Confirm both items appear, the video plays, and the original photo
   downloads.
5. Upload the same photo again and confirm it is skipped.
6. Open `/admin`, test trash and restore, and inspect the audit log.

Use Docker/Compose or Portainer container logs to diagnose startup failures.
Common causes are
missing secrets, incorrect directory ownership, an overlapping `EDGE_SUBNET`,
or a Tunnel hostname that does not target `http://app:8080`.

## Configuration

The most useful optional application settings are:

| Variable | Default | Purpose |
| --- | ---: | --- |
| `MAX_UPLOAD_BYTES` | `5368709120` | Maximum whole-file size in bytes (5 GiB) |
| `PUBLIC_RATE_LIMIT_PER_MINUTE` | `12000` | Sustained public requests per IP |
| `PUBLIC_RATE_LIMIT_BURST` | `3000` | Public request burst allowance |
| `UPLOAD_CONCURRENCY_PER_IP` | `50` | Concurrent uploads per IP; also configures the browser uploader |
| `UPLOAD_BANDWIDTH_PER_IP_BYTES_PER_SEC` | `1073741824` | Upload bandwidth per IP (effectively unthrottled at 1 GiB/s) |
| `ADMIN_SESSION_TTL_MINUTES` | `720` | Admin session lifetime |
| `THUMBNAIL_MAX_DIMENSION` | `800` | Longest thumbnail edge in pixels |
| `ALLOWED_IMAGE_MIME_TYPES` | common image formats | Comma-separated image MIME types |
| `ALLOWED_VIDEO_MIME_TYPES` | MP4, QuickTime, WebM | Comma-separated video MIME types |
| `TRASH_RETENTION_DAYS` | `30` | Permanently purge trash older than this; `0` disables automatic purge |
| `TUS_INCOMPLETE_RETENTION_HOURS` | `48` | Expire idle incomplete uploads through tusd; `0` disables cleanup |
| `STORAGE_CLEANUP_INTERVAL_MINUTES` | `60` | Trash/tus cleanup interval |

The stack enables secure cookies and configures trusted proxy addresses
automatically. Only change those internal settings if you also change the
network design. An upload that remains idle beyond the tus retention period is
terminated and must restart rather than resume.

## Backups, upgrades, and restoration

For a consistent backup:

1. Stop the stack so SQLite and media writes are idle.
2. Back up `/srv/event-gallery/app` and `/srv/event-gallery/media`
   together.
3. Start the stack.

To restore, stop the stack, restore the configured app-data and media
directories with ownership matching `PUID:PGID`, select the same immutable
image tag, and redeploy. Database migrations run automatically at startup.

Before upgrading, take a backup and change `APP_IMAGE_TAG` to the tested release
or commit tag. To roll back after a database migration, restore the matching
pre-upgrade data and media backup as well as the earlier image tag.

## Development and validation

Backend:

```sh
cd backend
go test ./...
go vet ./...
go build ./cmd/server
```

Frontend:

```sh
cd frontend
npm ci
npm run lint
npm run typecheck
npm test
npm run build
```

Render the production deployment configuration without starting it:

```sh
docker compose --env-file .env.portainer.example config
```
