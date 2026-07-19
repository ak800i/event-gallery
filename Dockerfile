# syntax=docker/dockerfile:1
#
# Multi-stage build for the wedding-gallery application server. Produces a
# single image that serves both the API and the built React frontend.
# tusd runs as a separate container (see docker-compose.yml) and is never
# reachable directly from outside this compose stack.

# --- Stage 1: build the frontend -------------------------------------------
FROM node@sha256:16e22a550f3863206a3f701448c45f7912c6896a62de43add43bb9c86130c3e2 AS frontend-builder
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# --- Stage 2: build the Go backend, embedding the frontend build ----------
FROM golang@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS backend-builder
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
# Overwrite the placeholder embedded UI with the real production build.
COPY --from=frontend-builder /src/frontend/dist/ ./internal/staticui/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# --- Stage 3: minimal runtime image -----------------------------------------
FROM alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce AS runtime
# ffmpeg: video thumbnail extraction + probing (see internal/media/video.go)
# tzdata: makes the TZ env var (e.g. Europe/Belgrade) meaningful for the app
#         and for ffmpeg-derived timestamps.
# ca-certificates: not strictly required today (no outbound TLS calls), but
#         keeps the image ready for that without a rebuild.
RUN apk add --no-cache ffmpeg=6.1.2-r2 tzdata=2026c-r0 ca-certificates=20260611-r0

WORKDIR /app
COPY --from=backend-builder /out/server /app/server
COPY deploy/app-entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh /app/server

ENV LISTEN_ADDR=:8080 \
    DATA_DIR=/data/app \
    MEDIA_DIR=/data/media \
    TZ=UTC \
    UMASK=022

EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
