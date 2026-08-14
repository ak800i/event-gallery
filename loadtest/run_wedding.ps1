# Generates base assets on the host, runs a stage inside a sidecar container on
# the app's own network (the app publishes no host port), then merges the app's
# logs in and decides pass/fail.
param(
    [Parameter(Mandatory = $true)][string]$Stage,
    [string]$Network   = 'wedding-gallery_edge',
    [string]$BaseUrl   = 'http://app:8080',
    # Host paths are deployment-specific, so they come from the environment
    # rather than being baked in: this repo is public.
    [string]$UploadDir = $env:EG_UPLOAD_DIR,
    [string]$MediaDir  = $env:EG_MEDIA_DIR,
    [string]$Container = 'wedding-gallery-app-1'
)
$ErrorActionPreference = 'Stop'
if (-not $UploadDir -or -not $MediaDir) {
    throw "Set EG_UPLOAD_DIR and EG_MEDIA_DIR to the host directories bind-mounted as the tus upload dir and the media dir, or pass -UploadDir and -MediaDir."
}
$repo    = Split-Path -Parent $PSScriptRoot
$results = Join-Path $repo 'loadtest\results'
New-Item -ItemType Directory -Force -Path $results | Out-Null

# Belt and braces. A `throw` below never reaches `exit $code` -- the finally
# runs and the script terminates with 1 on its own -- but a future edit that
# returns early would otherwise exit on an unset variable, which is 0.
$code = 1

# A stale results\<Stage>.json from an earlier campaign is worse than none: if
# this run dies before overwriting it, finalize reads the old one and can print
# `passed: true` for a stage that never ran.
Remove-Item -Path (Join-Path $results "$Stage.json"), (Join-Path $results "$Stage-uploads.json") `
    -ErrorAction SilentlyContinue

Push-Location $repo   # `python -m loadtest.wedding.*` resolves from the repo root
try {

# ffmpeg lives on the host, not in the sidecar image, so the corpus is built
# here first. build_assets is idempotent, so the container only ever reads them.
Write-Host '==> generating base assets on the host (idempotent)'
python -c "from pathlib import Path; from loadtest.wedding.corpus import build_assets; build_assets(Path('loadtest/assets'))"
if ($LASTEXITCODE -ne 0) { throw "asset generation failed ($LASTEXITCODE)" }

$since = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

Write-Host "==> running stage $Stage"
# pwsh 7 leaves $PSNativeCommandUseErrorActionPreference off by default, so a
# native command that exits non-zero does not throw even under
# ErrorActionPreference = 'Stop'. Without this check a crashed stage falls
# straight through to finalize, which then judges whatever JSON is on disk.
docker run --rm `
    --network $Network `
    -e PYTHONUNBUFFERED=1 `
    -v "${repo}:/work:ro" `
    -v "${results}:/results" `
    -v "${UploadDir}:/uploads:ro" `
    -v "${MediaDir}:/media:ro" `
    -w /work `
    python:3.13-slim `
    python -m loadtest.wedding.stage --stage $Stage --base-url $BaseUrl `
        --upload-dir /uploads --media-dir /media --out /results
if ($LASTEXITCODE -ne 0) { throw "stage $Stage did not complete ($LASTEXITCODE)" }

Write-Host '==> collecting app logs and finalizing'
$logFile = Join-Path $results "$Stage.log"

# `docker logs`, never `docker compose logs`: compose prefixes every line with
# `app-1  | `, so none of it parses as JSON, the ERROR count comes back zero and
# the stage passes vacuously against a log full of errors. Compose also
# multiplexes tusd and cloudflared, whose errors are not the app's.
$prev = $ErrorActionPreference
$ErrorActionPreference = 'Continue'
try {
    docker logs --since $since $Container 2>&1 |
        ForEach-Object { if ($_ -is [System.Management.Automation.ErrorRecord]) { $_.ToString() } else { $_ } } |
        Set-Content -Path $logFile -Encoding utf8
} finally { $ErrorActionPreference = $prev }

python -m loadtest.wedding.finalize --report (Join-Path $results "$Stage.json") --logs $logFile
$code = $LASTEXITCODE

} finally { Pop-Location }
exit $code
