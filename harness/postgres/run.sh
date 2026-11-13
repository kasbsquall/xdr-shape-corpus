#!/usr/bin/env bash
# Runs wallet-backend's own `migrate up` and `ingest` commands against a real TimescaleDB
# (the image wallet-backend pins), at the pre-#729 and fixed commits, for the corpus entry
# cap40-signed-payload-signer-max-payload served by harness/mockrpc over getLedgers.
#
# Usage: bash harness/postgres/run.sh
#        PG_CONTAINER=<id> PG_PASSWORD=<pw> bash harness/postgres/run.sh   (use an existing
#          Postgres container, e.g. the CI service; its postgres role must have PG_PASSWORD)
# Writes harness/postgres/out/<label>-<sha>-sdk-v0.8.0.json and harness/postgres/results.json.
#
# Every process (mockrpc, migrate, ingest, the xdrkitpg observer) runs as a static linux
# binary in its own container that shares the Postgres container's network namespace, so
# Postgres, the mock RPC and wallet-backend's health server are reachable only on that
# namespace's loopback. Nothing is published on the host. The Postgres password is generated
# here (or passed in), kept in a 0600 env file in a temp dir, never printed, and removed on exit.
# Needs: docker, git, bash, python3, the portable Go toolchain in tools-local, network the
# first time for the image pull and `go mod download`.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS="$(cd "$HERE/.." && pwd)"
KIT="$(cd "$HARNESS/.." && pwd)"
ROOT="$(cd "$KIT/.." && pwd)"
TOOLS="${TOOLS:-$ROOT/tools-local}"
SRC="${SRC:-$TOOLS/src/wallet-backend}"
SDK=v0.8.0   # both commits pin v0.7.3, which cannot decode protocol 30 ledger headers
ENTRY=cap40-signed-payload-signer-max-payload
# wallet-backend's docker-compose.yaml and .github/workflows/go.yaml pin 2.28.2-pg17 at both commits.
IMAGE="${PG_IMAGE:-timescale/timescaledb:2.28.2-pg17@sha256:909e2c7577c074517c8936b6d8799294b33f25fb5bad978a2199250665f1ce1a}"
COMMITS=(
  "pre-729=2a0a84ea"   # parent of the #729 merge
  "fix-729=2c873353"   # #729 merge (SignerKeyBytea)
)
MAX_RUNS=2           # a second start shows where a restarted ingest resumes
RUN_TIMEOUT=240      # seconds per ingest run

if [ -x "$TOOLS/go/bin/go" ] || [ -x "$TOOLS/go/bin/go.exe" ]; then export GOROOT="$TOOLS/go" GOPATH="$TOOLS/gopath" GOCACHE="$TOOLS/gocache"; fi
export PATH="$TOOLS/go/bin:$PATH"
export GOTOOLCHAIN=local GOFLAGS=-mod=mod
# Git Bash on Windows: do not rewrite container paths in docker arguments (only for docker,
# since Go needs its GOROOT converted).
docker() { MSYS_NO_PATHCONV=1 command docker "$@"; }

hostpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi; }
randhex() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }

RUN="xdrkit-pg-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
tmp="$(mktemp -d)"
chmod 700 "$tmp"
OWN_PG=0
cleanup() {
  ids="$(docker ps -aq --filter "name=^${RUN}" 2>/dev/null || true)"
  [ -z "$ids" ] || docker rm -f $ids >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

if [ -n "${PG_CONTAINER:-}" ]; then
  PG="$PG_CONTAINER"
  PW="${PG_PASSWORD:?PG_PASSWORD is required with PG_CONTAINER}"
else
  OWN_PG=1
  PG="$RUN"
  PW="$(randhex)"
  umask 077
  printf 'POSTGRES_USER=postgres\nPOSTGRES_PASSWORD=%s\nTIMESCALEDB_TELEMETRY=off\n' "$PW" > "$tmp/pg.env"
  umask 022
  echo "== starting $PG ($IMAGE), no published ports, 1 GiB / 1 CPU"
  docker run -d --name "$PG" --memory 1g --memory-swap 1g --cpus 1 --pids-limit 512 \
    --env-file "$(hostpath "$tmp/pg.env")" "$IMAGE" >/dev/null
fi
for _ in $(seq 120); do docker exec "$PG" pg_isready -q -U postgres -h 127.0.0.1 && break; sleep 1; done
docker exec "$PG" pg_isready -q -U postgres -h 127.0.0.1 || { echo "postgres did not become ready" >&2; exit 1; }

psql() { docker exec -i "$PG" psql -v ON_ERROR_STOP=1 -X -q -U postgres "$@"; }

mkdir -p "$HERE/out" "$tmp/bin"
[ -d "$SRC/.git" ] || git clone -q https://github.com/stellar/wallet-backend.git "$SRC"

echo "== building mockrpc (linux)"
(cd "$HARNESS/mockrpc" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$tmp/bin/mockrpc" .)
BIN="$(hostpath "$tmp/bin")"
CORPUS="$(hostpath "$KIT/corpus")"
# Helper containers: same image (static binaries need no libc), Postgres' network namespace.
helper() { docker run --network "container:$PG" --memory 1g --memory-swap 1g --cpus 1 --pids-limit 512 \
  -v "$BIN:/x:ro" -v "$CORPUS:/corpus:ro" "$@"; }

helper -d --name "$RUN-rpc" --entrypoint /x/mockrpc "$IMAGE" -corpus /corpus -addr 127.0.0.1:18000 >/dev/null
for _ in $(seq 50); do docker logs "$RUN-rpc" 2>/dev/null | grep -q '^http://' && break; sleep 0.2; done
docker logs "$RUN-rpc" 2>/dev/null | grep -q '^http://127.0.0.1:18000' || { echo "mockrpc did not start" >&2; docker logs "$RUN-rpc" >&2; exit 1; }
echo "== mockrpc serving the corpus at http://127.0.0.1:18000 inside $PG's network namespace"

LEDGER="$(python3 -c 'import json,sys; m=json.load(open(sys.argv[1])); print([e for e in m["entries"] if e["id"]==sys.argv[2]][0]["source"]["ledger"])' "$KIT/corpus/manifest.json" "$ENTRY" 2>/dev/null \
  || python -c 'import json,sys; m=json.load(open(sys.argv[1])); print([e for e in m["entries"] if e["id"]==sys.argv[2]][0]["source"]["ledger"])' "$KIT/corpus/manifest.json" "$ENTRY")"
LEDGER="$(printf '%s' "$LEDGER" | tr -d '\r')"

for pair in "${COMMITS[@]}"; do
  label="${pair%%=*}"; sha="${pair#*=}"
  wt="$TOOLS/src/wb-$sha"
  [ -d "$wt" ] || git -C "$SRC" worktree add -q --detach "$wt" "$sha"
  full="$(git -C "$wt" rev-parse HEAD)"
  git -C "$wt" checkout -q -- go.mod go.sum
  (cd "$wt" && go mod edit -require=github.com/stellar/go-stellar-sdk@"$SDK")
  mkdir -p "$wt/internal/xdrkitpg"
  cp "$HERE/go/xdrkitpg/main.go" "$wt/internal/xdrkitpg/main.go"
  echo "== $label ($full) [sdk-$SDK]: building wallet-backend and xdrkitpg (linux)"
  (cd "$wt" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$tmp/bin/wb-$label" . \
            && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$tmp/bin/obs-$label" ./internal/xdrkitpg)

  dbname="wb_${label//-/_}"
  psql -c "DROP DATABASE IF EXISTS $dbname" -c "CREATE DATABASE $dbname"
  # wallet-backend's docker-compose.yaml creates the extension in its init script and starts
  # Postgres with these two settings; they are set per database here so an existing container
  # (a CI service cannot take a command line) gets the same configuration.
  psql -d "$dbname" -c "CREATE EXTENSION IF NOT EXISTS timescaledb" \
    -c "ALTER DATABASE $dbname SET timescaledb.enable_chunk_skipping = on" \
    -c "ALTER DATABASE $dbname SET timescaledb.enable_sparse_index_bloom = on"

  work="$tmp/work/$label"; mkdir -p "$work"; chmod 777 "$work"
  WORK="$(hostpath "$work")"
  umask 077
  # wallet-backend reads its configuration from these environment variables (cmd/utils).
  cat > "$tmp/wb.env" <<EOF
DATABASE_URL=postgres://postgres:$PW@127.0.0.1:5432/$dbname?sslmode=disable
RPC_URL=http://127.0.0.1:18000
LEDGER_BACKEND_TYPE=rpc
NETWORK=testnet
NETWORK_PASSPHRASE=Test SDF Network ; September 2015
STELLAR_ENVIRONMENT=xdrkit-harness
ARCHIVE_URL=http://127.0.0.1:9/
LOG_LEVEL=INFO
EOF
  umask 022
  ENVF="$(hostpath "$tmp/wb.env")"
  obs() { helper --rm --env-file "$ENVF" -v "$WORK:/work" --entrypoint "/x/obs-$label" "$IMAGE" -corpus /corpus -entry "$ENTRY" "$@"; }

  echo "== $label: wallet-backend migrate up"
  helper --rm --name "$RUN-migrate" --env-file "$ENVF" --entrypoint "/x/wb-$label" "$IMAGE" migrate up > "$work/migrate.log" 2>&1 \
    || { cat "$work/migrate.log" >&2; exit 1; }
  echo "== $label: seed latest/oldest cursors to $((LEDGER - 1)) and snapshot"
  obs -mode seed -out /work/before.json

  : > "$work/runs.tsv"
  for run in $(seq "$MAX_RUNS"); do
    name="$RUN-ingest-$label-$run"
    echo "== $label: wallet-backend ingest, run $run"
    helper -d --name "$name" --env-file "$ENVF" --entrypoint "/x/wb-$label" "$IMAGE" ingest >/dev/null
    stopped="self"
    deadline=$((SECONDS + RUN_TIMEOUT))
    while :; do
      [ "$(docker inspect -f '{{.State.Running}}' "$name")" = "true" ] || break
      cur="$(psql -d "$dbname" -At -c "SELECT value FROM ingest_store WHERE key = 'latest_ingest_ledger'" | tr -d '\r')"
      if [ -n "$cur" ] && [ "$cur" -ge "$LEDGER" ]; then
        docker stop -t 30 "$name" >/dev/null; stopped="harness-after-cursor-reached-ledger"; break
      fi
      if [ "$SECONDS" -ge "$deadline" ]; then
        docker stop -t 30 "$name" >/dev/null; stopped="harness-timeout"; break
      fi
      sleep 1
    done
    code="$(docker wait "$name")"
    docker logs "$name" > "$work/ingest-$run.log" 2>&1
    docker rm "$name" >/dev/null
    if grep -qF "$PW" "$work/ingest-$run.log" "$work/migrate.log"; then echo "password found in a log; aborting" >&2; exit 1; fi
    printf '%s\t%s\t%s\t%s\n' "$run" "$code" "$stopped" "/work/ingest-$run.log" >> "$work/runs.tsv"
    echo "   exit $code, stopped by $stopped"
    cur="$(psql -d "$dbname" -At -c "SELECT value FROM ingest_store WHERE key = 'latest_ingest_ledger'" | tr -d '\r')"
    [ "$cur" -ge "$LEDGER" ] && break
  done

  out="$label-$sha-sdk-$SDK"
  obs -mode report -before /work/before.json -runs /work/runs.tsv -label "$label" -commit "$full" -image "$IMAGE" -out /work/result.json
  cp "$work/result.json" "$HERE/out/$out.json"
  for f in "$work"/ingest-*.log; do cp "$f" "$HERE/out/$out-$(basename "$f")"; done
  psql -c "DROP DATABASE $dbname"
done

if [ "$OWN_PG" = 1 ]; then echo "== removing $PG"; fi
python3 "$HERE/aggregate.py" 2>/dev/null || python "$HERE/aggregate.py"
