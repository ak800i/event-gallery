# Wedding Gallery

A self-hosted wedding photo and video gallery with resumable uploads, an
administration UI, and SQLite storage. The production image contains the Go
API and React UI; `tusd` handles resumable upload transport behind the API.

## Portainer deployment

The supplied `compose.yaml` is intended for a Portainer Git stack on a NAS. It
pulls prebuilt `linux/amd64` or `linux/arm64` images from GHCR and runs:

- `cloudflared` on the outbound-capable `edge` network;
- `app` on `edge` and the isolated `uploads` network;
- `tusd` only on `uploads`.

No service publishes a host port. The application is reachable only through
the Cloudflare Tunnel, and `tusd` is reachable only through the application.

### 1. Prepare NAS directories

Create three persistent directories. The paths below are examples:

```text
/volume1/docker/wedding-gallery/app
/volume1/photos/wedding-gallery
/volume1/docker/wedding-gallery/uploads
```

The numeric `PUID` and `PGID` configured in Portainer must own all three
directories and have read/write/execute access. The app directory contains
SQLite state and thumbnails, the media directory contains original files, and
the uploads directory is temporary shared storage used while `tusd` finishes
an upload.

### 2. Create the Cloudflare Tunnel

In Cloudflare Zero Trust:

1. Create a remotely managed Cloudflare Tunnel.
2. Add a public hostname for the gallery.
3. Set its service type to HTTP and URL to `http://app:8080`.
4. Copy the tunnel token for the Portainer environment.

Cloudflare terminates public TLS. `COOKIE_SECURE` remains enabled in the stack,
so use the HTTPS public hostname for administration. Do not add a host port for
the app or `tusd`.

For large uploads, ensure Cloudflare account limits and any policies permit the
configured file size and long-running resumable requests.

### 3. Create the Portainer Git stack

Create a stack from this repository and select `compose.yaml`. Add the
variables from `.env.portainer.example` in Portainer's **Environment
variables** section. At minimum, replace:

- `APP_DATA_PATH`, `MEDIA_PATH`, and `TUS_UPLOAD_PATH` with existing absolute
  NAS paths;
- `ADMIN_PASSWORD` with a strong password of at least 8 characters;
- `TUS_HOOK_SECRET` with an independent random value of at least 16 characters
  (32 bytes or more is recommended);
- `CLOUDFLARE_TUNNEL_TOKEN` with the remotely managed tunnel token;
- `PUID` and `PGID` with the owner of the NAS directories.

Generate secrets locally, for example with `openssl rand -hex 32`. Never put
real values in Git.

`EDGE_SUBNET` defaults to `172.30.0.0/24`. Change it if that range overlaps
another NAS network. The backend trusts forwarded client IP headers only from
this subnet; requests from any other peer cannot spoof those headers.

The GHCR packages must be public, or Portainer must be configured with a GHCR
registry credential that can pull them. Deploy the stack, then confirm all
three containers are running and `app` and `tusd` are healthy.

### Image tags and releases

`.github/workflows/containers.yml` tests the backend and frontend, then builds
both container images for `linux/amd64` and `linux/arm64`. Pushes to `main`
publish `latest` and `sha-<full-commit>` tags. Git tags beginning with `v`
also publish the matching release tag.

For repeatable deployments, set `APP_IMAGE_TAG` to a
`sha-<full-commit>` or release tag instead of `latest`. The same tag is
published for both application and `tusd` images.

## Operations

### Health and logs

The app health check calls `/healthz`, which verifies SQLite connectivity.
The `tusd` health check reads its internal metrics endpoint. Both endpoints are
container-internal and are not published on the NAS.

Use Portainer container logs for startup validation. Common startup failures
are missing secrets, unwritable bind mounts, an overlapping `EDGE_SUBNET`, or
a Cloudflare hostname that does not target `http://app:8080`.

### Backups and restoration

For a consistent backup:

1. Stop the stack so SQLite and media writes are quiescent.
2. Back up `APP_DATA_PATH` and `MEDIA_PATH` together.
3. Start the stack again.

`TUS_UPLOAD_PATH` contains incomplete uploads and normally does not need to be
backed up. To restore, stop the stack, restore the two persistent directories
to their original paths with the configured ownership, and redeploy the same
image tag. Database migrations run automatically at startup.

### Upgrade and rollback

To upgrade, choose a tested release or commit tag in `APP_IMAGE_TAG`, pull and
redeploy the stack, and verify health and an upload. Back up first because a
new version may migrate SQLite.

To roll back application code, restore the matching pre-upgrade backup and set
`APP_IMAGE_TAG` to the previous immutable tag. Redeploying an older image
against a database already migrated by a newer version is not guaranteed to
work.

## Local validation

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

Render the deployment configuration without starting it:

```sh
docker compose --env-file .env.portainer.example config
```
