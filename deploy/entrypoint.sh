#!/bin/sh
# Restore state from the replica if the volume is empty, then run the daemon
# under Litestream so every write is replicated. Litestream is the real
# backup (volume snapshots cost money), so the daemon refuses to start
# without a replica configured.
set -eu

: "${UNCHARLES_DB_PATH:=/data/uncharles.db}"
if [ -z "${LITESTREAM_REPLICA_URL:-}" ]; then
	if [ -z "${BUCKET_NAME:-}" ]; then
		echo "entrypoint: set LITESTREAM_REPLICA_URL or BUCKET_NAME; refusing to run without a replica" >&2
		exit 1
	fi
	LITESTREAM_REPLICA_URL="s3://${BUCKET_NAME}/uncharles.db?endpoint=fly.storage.tigris.dev&region=auto"
fi
export UNCHARLES_DB_PATH LITESTREAM_REPLICA_URL

mkdir -p "$(dirname "$UNCHARLES_DB_PATH")"

# Fresh volume + existing replica: restore. Existing DB or no replica yet:
# no-op, so first boot and normal restarts both work.
litestream restore -if-db-not-exists -if-replica-exists "$UNCHARLES_DB_PATH"

exec litestream replicate -exec "uncharles run"
