# Wedding Gallery

A private, self-hosted photo and video gallery for a single wedding. Guests
open one mobile-friendly page, enter a display name, upload media, and browse
the gallery without creating an account or installing an app. A
password-protected admin area provides moderation and gallery settings.

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
- Calculates a SHA-256 checksum for each chunk and verifies it before accepting
  the chunk.
- Calculates a whole-file SHA-256 hash in the browser to skip known duplicates,
  then verifies the completed file again on the server before storing it.
- Limits public requests, concurrent uploads, and upload bandwidth per client
  IP.

### Administration

Open `/admin` and sign in with the password supplied through
`ADMIN_PASSWORD`. There is no username. An administrator can:

- select one or more files and move them to trash;
- browse trashed files and restore them;
- review uploads, deletes, restores, logins, and configuration changes in the
  audit log;
- set an upload expiry date. After expiry, browsing and downloads remain
  available, but new uploads are rejected.

Deleted originals are moved into a trash area beneath the media directory; they
are retained on disk and excluded from the public gallery.

## Architecture

The production stack contains three containers:

- **app**: the Go API, React frontend, SQLite database access, media processing,
  authorization, and upload proxy;
- **tusd**: resumable upload transport, available only to `app` on an isolated
  Docker network;
- **cloudflared**: outbound Cloudflare Tunnel connection to the public
  hostname.

No host ports are published. Cloudflare sends traffic to `http://app:8080`, and
guests cannot reach `tusd` directly. SQLite data and generated thumbnails are
stored separately from original media.

## Deploy on Synology with Portainer

The supplied `compose.yaml` is ready to use as a Portainer Git stack. It pulls
multi-architecture images from GHCR and restarts containers automatically after
a NAS reboot.

The NAS paths and container identity are Compose environment variables, not
hardcoded deployment values:

- `APP_DATA_PATH`, `MEDIA_PATH`, and `TUS_UPLOAD_PATH` are required host paths.
  The `/data/app`, `/data/media`, and `/data/tusd-incoming` paths inside the
  containers are fixed.
- `PUID` and `PGID` select the numeric user and group used by both `app` and
  `tusd`. They default to `1000:1000` if omitted and should be set to an account
  that can access all three host paths.

All Synology paths and IDs below are examples; replace them with values from
your NAS.

### 1. Prepare persistent directories

For example, create these directories on the NAS:

```text
/volume1/data/media/wedding-photos
/volume2/docker-data/wedding-gallery/app
/volume2/docker-data/wedding-gallery/uploads
```

The first directory stores original media and its trash area. The `app`
directory stores SQLite data and thumbnails. The `uploads` directory holds
incomplete tus uploads and does not normally need to be backed up.

If using the example `PUID=1027` and `PGID=65536`, ensure that identity can
read, write, and traverse all three directories:

```sh
sudo chown -R 1027:65536 \
  /volume1/data/media/wedding-photos \
  /volume2/docker-data/wedding-gallery
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
`compose.yaml`. Add the following environment variables in the stack
configuration:

```dotenv
# Use an immutable sha-<40-character-commit> or release tag from GHCR.
APP_IMAGE_TAG=sha-REPLACE_WITH_FULL_COMMIT_SHA

APP_DATA_PATH=/volume2/docker-data/wedding-gallery/app
MEDIA_PATH=/volume1/data/media/wedding-photos
TUS_UPLOAD_PATH=/volume2/docker-data/wedding-gallery/uploads

PUID=1027
PGID=65536
TZ=Europe/Belgrade
UMASK=022

# Set independent secret values. Do not commit them to this repository.
ADMIN_PASSWORD=REPLACE_WITH_A_STRONG_PASSWORD
TUS_HOOK_SECRET=REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS
CLOUDFLARE_TUNNEL_TOKEN=REPLACE_WITH_THE_TUNNEL_TOKEN

# 1 GiB; increase if guests need to upload larger videos.
MAX_UPLOAD_BYTES=1073741824

# Change this if it overlaps another Docker or LAN subnet.
EDGE_SUBNET=172.30.0.0/24
```

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

Use Portainer container logs to diagnose startup failures. Common causes are
missing secrets, incorrect directory ownership, an overlapping `EDGE_SUBNET`,
or a Tunnel hostname that does not target `http://app:8080`.

## Configuration

The most useful optional application settings are:

| Variable | Default | Purpose |
| --- | ---: | --- |
| `MAX_UPLOAD_BYTES` | `314572800` | Maximum whole-file size in bytes |
| `PUBLIC_RATE_LIMIT_PER_MINUTE` | `120` | Sustained public requests per IP |
| `PUBLIC_RATE_LIMIT_BURST` | `40` | Public request burst allowance |
| `UPLOAD_CONCURRENCY_PER_IP` | `3` | Concurrent upload requests per IP |
| `UPLOAD_BANDWIDTH_PER_IP_BYTES_PER_SEC` | `3145728` | Upload bandwidth per IP |
| `ADMIN_SESSION_TTL_MINUTES` | `720` | Admin session lifetime |
| `THUMBNAIL_MAX_DIMENSION` | `800` | Longest thumbnail edge in pixels |
| `ALLOWED_IMAGE_MIME_TYPES` | common image formats | Comma-separated image MIME types |
| `ALLOWED_VIDEO_MIME_TYPES` | MP4, QuickTime, WebM | Comma-separated video MIME types |

The stack enables secure cookies and configures trusted proxy addresses
automatically. Only change those internal settings if you also change the
network design.

## Backups, upgrades, and restoration

For a consistent backup:

1. Stop the stack so SQLite and media writes are idle.
2. Back up `/volume2/docker-data/wedding-gallery/app` and
   `/volume1/data/media/wedding-photos` together.
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
