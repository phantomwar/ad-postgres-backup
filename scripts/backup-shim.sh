#!/bin/sh
# Compatibility shim for legacy `docker exec <container> /backup.sh` invocations.
exec /usr/local/bin/ad-postgres-backup backup "$@"
