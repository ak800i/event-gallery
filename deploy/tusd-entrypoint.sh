#!/bin/sh
# Entrypoint for the internal tusd container. tusd is never published to the
# host or the internet -- it is only reachable from the "app" service over
# the internal-only docker network, and app in turn is the only public
# entry point (see docker-compose.yml). This script just applies UMASK and
# builds the tusd command line from environment variables so the compose
# file can stay simple.
#
# The four hook/timeout flags below are load-bearing, not tuning:
#   -hooks-http-retry 0      one attempt, no retries. tusd's default of 3 would
#                            re-invoke pre-create and mint duplicate job rows
#                            for a single upload.
#   -hooks-http-timeout 90s  the default is 15s, far shorter than the app's 75s
#                            pre-finish durability barrier. 90s is the ceiling
#                            UPLOAD_DURABILITY_WAIT_SECONDS is validated below.
#   -network-timeout 90s     the barrier holds the PATCH response open, so the
#                            default 60s would sever it from the other side.
#   -disable-concatenation   a concatenated final upload has no single source
#                            file, which the one-row-per-upload job model has
#                            no way to represent.
#
# The 90s above is one number in three places that cannot share a constant, and
# all three must move together: these two flags, `tusdHookTimeout` in
# backend/internal/config/config.go (which rejects a larger app budget at
# startup), and the comment above UPLOAD_DURABILITY_WAIT_SECONDS in
# docker-compose.yml. Raising these flags alone leaves the app rejecting
# budgets tusd would now tolerate; lowering them alone lets the app accept a
# budget tusd will cut mid-hook.
#
# -disable-termination is deliberately absent: the storage janitor removes
# discarded sources through tusd's own DELETE endpoint, so disabling that
# endpoint would strand them.
set -e
umask "${UMASK:-022}"

exec tusd \
  -host 0.0.0.0 \
  -port 1080 \
  -base-path /files/ \
  -upload-dir "${TUS_UPLOAD_DIR:-/data/tusd-incoming}" \
  -max-size "${MAX_UPLOAD_BYTES:-5368709120}" \
  -hooks-http "${TUS_HOOKS_URL:-http://app:8080/api/internal/tus-hooks}" \
  -hooks-http-forward-headers "X-Internal-Proxy-Secret,X-Event-Gallery-Client-Ip" \
  -hooks-enabled-events "pre-create,pre-finish,post-finish" \
  -hooks-http-retry 0 \
  -hooks-http-timeout 90s \
  -network-timeout 90s \
  -disable-concatenation \
  -behind-proxy \
  -disable-cors \
  -disable-download \
  -show-greeting=false
