#!/bin/sh
# Entrypoint for the internal tusd container. tusd is never published to the
# host or the internet -- it is only reachable from the "app" service over
# the internal-only docker network, and app in turn is the only public
# entry point (see docker-compose.yml). This script just applies UMASK and
# builds the tusd command line from environment variables so the compose
# file can stay simple.
set -e
umask "${UMASK:-022}"

exec tusd \
  -host 0.0.0.0 \
  -port 1080 \
  -base-path /files/ \
  -upload-dir "${TUS_UPLOAD_DIR:-/data/tusd-incoming}" \
  -max-size "${MAX_UPLOAD_BYTES:-5368709120}" \
  -hooks-http "${TUS_HOOKS_URL:-http://app:8080/api/internal/tus-hooks}" \
  -hooks-http-forward-headers "X-Internal-Proxy-Secret,X-Wg-Client-Ip" \
  -hooks-enabled-events "pre-create,post-finish" \
  -behind-proxy \
  -disable-cors \
  -disable-download \
  -show-greeting=false
