#!/bin/sh
# Entrypoint for the wedding-gallery app container. Applies the configured
# UMASK (Synology deployments typically want 022) before starting the
# server, so any files/directories the app creates on the bind-mounted
# volumes inherit sane, predictable permissions.
set -e
umask "${UMASK:-022}"
exec /app/server
