#!/usr/bin/env bash
# Replays the xdr-kit corpus through stellar/stellar-etl's export code at a pinned commit.
# Usage: bash harness/etl/run.sh               (corpus served by mockrpc over getLedgers)
#        INPUT=file bash harness/etl/run.sh    (adapter reads the .b64 files instead)
#        ETL_GO=go1.27.4 bash harness/etl/run.sh  (try another Go toolchain; see README)
#        ENTRY=<entry-id> bash harness/etl/run.sh (one entry, written to a temp dir)
# Writes harness/etl/out/<entry>.json and harness/etl/results.json.
# Needs: git, the portable Go toolchain in tools-local, a C compiler for cgo
# (stellar-etl links go-stellar-xdr-json's prebuilt static library), network the
# first time for the clone, `go mod download` and the go1.27.3 toolchain.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS="$(cd "$HERE/.." && pwd)"
KIT="$(cd "$HARNESS/.." && pwd)"
ROOT="$(cd "$KIT/.." && pwd)"
TOOLS="${TOOLS:-$ROOT/tools-local}"
SRC="${SRC:-$TOOLS/src/stellar-etl}"
ETL_COMMIT="2b24ec89ddce7fc0beb7795de982c93e6895eaf3"   # stellar-etl master, 2026-11-11
# stellar-etl's go.mod says `go 1.27.3` and its Dockerfile builds with golang:1.27.3.
# go1.27.4's cmd/go rejects the .llvmbc sections in go-stellar-xdr-json's static
# libraries, so the default is the toolchain stellar-etl itself declares.
ETL_GO="${ETL_GO:-go1.27.3}"

if [ -x "$TOOLS/go/bin/go" ] || [ -x "$TOOLS/go/bin/go.exe" ]; then export GOROOT="$TOOLS/go" GOPATH="$TOOLS/gopath" GOCACHE="$TOOLS/gocache"; fi
export PATH="$TOOLS/go/bin:$PATH"
export CGO_ENABLED=1

if [ ! -d "$SRC/.git" ]; then
  git init -q "$SRC"
  git -C "$SRC" remote add origin https://github.com/stellar/stellar-etl.git
fi
if [ "$(git -C "$SRC" rev-parse HEAD 2>/dev/null || true)" != "$ETL_COMMIT" ]; then
  git -C "$SRC" fetch -q --depth 1 --filter=blob:limit=300k origin "$ETL_COMMIT"
  git -C "$SRC" checkout -q --detach "$ETL_COMMIT"
fi
cp "$HERE/go/xdrkit_replay.go" "$SRC/cmd/xdrkit_replay.go"

RPC_FLAG=()
if [ "${INPUT:-rpc}" != "file" ]; then
  tmp="$(mktemp -d)"
  mock="$tmp/mockrpc$(GOTOOLCHAIN=local go env GOEXE)"
  (cd "$HARNESS/mockrpc" && GOTOOLCHAIN=local go build -o "$mock" .)
  "$mock" -corpus "$KIT/corpus" -addr 127.0.0.1:0 > "$tmp/url" &
  mock_pid=$!
  trap 'kill "$mock_pid" 2>/dev/null; rm -rf "$tmp"' EXIT
  for _ in $(seq 100); do [ -s "$tmp/url" ] && break; sleep 0.1; done
  [ -s "$tmp/url" ] || { echo "mockrpc did not start" >&2; exit 1; }
  RPC_FLAG=(--rpc "$(head -n1 "$tmp/url")")
  echo "== mockrpc serving the corpus at ${RPC_FLAG[1]}"
fi

OUT="$HERE"
ENTRY_FLAG=()
if [ -n "${ENTRY:-}" ]; then
  # One entry only: write to a scratch directory so results.json is not replaced.
  OUT="${OUT_DIR:-$(mktemp -d)}"
  ENTRY_FLAG=(--entry "$ENTRY")
fi
mkdir -p "$OUT/out"
echo "== stellar-etl $ETL_COMMIT with $ETL_GO -> $OUT"
(cd "$SRC" && GOTOOLCHAIN="$ETL_GO" go run . xdrkit_replay --corpus "$KIT/corpus" "${RPC_FLAG[@]}" "${ENTRY_FLAG[@]}" --out "$OUT" --commit "$ETL_COMMIT")
