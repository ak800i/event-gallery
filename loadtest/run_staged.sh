#!/bin/sh
set -eu

if [ "${I_UNDERSTAND_PRODUCTION_LOAD:-}" != "YES" ]; then
  echo "Refusing production load. Set I_UNDERSTAND_PRODUCTION_LOAD=YES." >&2
  exit 64
fi

: "${BASE_URL:?Set BASE_URL to the production gallery origin}"
RESULTS="${RESULTS:-loadtest/results}"
mkdir -p "$RESULTS"

run() {
  echo "\n=== $1 ==="
  shift
  BASE_URL="$BASE_URL" python loadtest/tus_battle.py "$@"
}

run "Smoke + forced HEAD/resume" \
  --stage smoke-resume --count 1 --size-mb 16 --chunk-mb 8 --resume \
  --min-success-rate 1 --json-out "$RESULTS/01-smoke-resume.json"

run "10 concurrent uploads" \
  --stage concurrent-10 --count 10 --size-mb 5 --chunk-mb 8 \
  --min-success-rate 1 --json-out "$RESULTS/02-concurrent-10.json"

run "40 concurrent uploads" \
  --stage concurrent-40 --count 40 --size-mb 5 --chunk-mb 8 \
  --min-success-rate 0.98 --json-out "$RESULTS/03-concurrent-40.json"

run "60 concurrent uploads (crosses configured boundary)" \
  --stage boundary-60 --count 60 --size-mb 5 --chunk-mb 8 \
  --min-success-rate 0.90 --max-5xx-rate 0.02 \
  --json-out "$RESULTS/04-boundary-60.json"

run "Two sustained 250 MiB uploads" \
  --stage large-2x250 --count 2 --size-mb 250 --chunk-mb 8 \
  --min-success-rate 1 --max-5xx-rate 0.02 \
  --json-out "$RESULTS/05-large-2x250.json"

cat <<EOF

All stages passed. Results: $RESULTS
To remove test items from the public gallery (soft trash):
  BASE_URL='$BASE_URL' ADMIN_PASSWORD='...' python loadtest/cleanup_battle.py
EOF
