#!/bin/sh
# Phase 0 restore check, runnable locally and in CI against a built image:
#   1. boot on an empty volume          -> boot_count 1
#   2. restart on the same volume       -> boot_count 2
#   3. destroy the volume, boot fresh   -> Litestream restores, boot_count 3
# A Docker volume stands in for Tigris (a volume, not a bind mount, so
# root-owned replica files never land on the host).
#
# Usage: deploy/smoke-restore.sh [image]   (default chuck:dev)
set -eu

image=${1:-chuck:dev}
name=chuck-smoke
trap 'docker rm -f $name >/dev/null 2>&1 || true; docker volume rm -f ${name}-1 ${name}-2 ${name}-replica >/dev/null 2>&1 || true' EXIT

boot() { # volume expected_count
	docker run -d --name $name -v "$1:/data" -v ${name}-replica:/replica \
		-e LITESTREAM_REPLICA_URL=file:///replica/chuck \
		-e CHUCK_HEARTBEAT=1s "$image" >/dev/null
	got=""
	for _ in $(seq 1 30); do
		got=$(docker exec $name wget -qO- http://127.0.0.1:8080/healthz 2>/dev/null || true)
		[ -n "$got" ] && break
		sleep 1
	done
	sleep 3 # let Litestream ship the boot row before stopping
	docker stop -t 20 $name >/dev/null
	if ! echo "$got" | grep -q "\"boot_count\":$2[,}]"; then
		echo "FAIL: want boot_count $2, healthz said: ${got:-<no response>}" >&2
		docker logs $name >&2
		exit 1
	fi
	docker rm $name >/dev/null
	echo "ok: boot_count $2"
}

boot ${name}-1 1
boot ${name}-1 2
docker volume rm ${name}-1 >/dev/null
boot ${name}-2 3
echo "restore check passed"
