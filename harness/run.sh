#!/usr/bin/env bash
# Replays the xdr-kit corpus through stellar/wallet-backend at fixed commits.
# Usage: bash run.sh            (all commits in COMMITS)
#        bash run.sh <sha> ...  (only those)
#        SDK=v0.8.0 bash run.sh  (same, but bump ONLY github.com/stellar/go-stellar-sdk
#                                 to that version in each checkout's go.mod first)
# Needs: git, the portable Go toolchain in tools-local, network for `go mod download`.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KIT="$(cd "$HERE/.." && pwd)"
ROOT="$(cd "$KIT/.." && pwd)"
TOOLS="${TOOLS:-$ROOT/tools-local}"
SRC="${SRC:-$TOOLS/src/wallet-backend}"

if [ -x "$TOOLS/go/bin/go" ] || [ -x "$TOOLS/go/bin/go.exe" ]; then export GOROOT="$TOOLS/go" GOPATH="$TOOLS/gopath" GOCACHE="$TOOLS/gocache"; fi
export PATH="$TOOLS/go/bin:$PATH"
export GOTOOLCHAIN=local GOFLAGS=-mod=mod

# label=sha pairs, in the order they appear in the report
COMMITS=(
  "pre-716=79ea2acd"   # #715 merge, the tree #716 was filed against (AddressBytea truncates)
  "pre-729=2a0a84ea"   # parent of #729 merge (after #693, P keys rejected)
  "fix-729=2c873353"   # #729 merge
  "pre-731=764a4f5b"   # parent of #734 merge (fix for #731)
  "fix-731=c152783c"   # #734 merge
  "close-716=71625b21" # main tip when #716 was closed (2026-09-18T20:09Z); #716 has no linked PR
  "main=3e22515a"      # main tip on 2026-11-12, for reference
)
if [ "$#" -gt 0 ]; then
  sel=()
  for want in "$@"; do for c in "${COMMITS[@]}"; do [[ "$c" == *"=$want"* || "$c" == "$want="* ]] && sel+=("$c"); done; done
  COMMITS=("${sel[@]}")
fi

mkdir -p "$HERE/out"
[ -d "$SRC/.git" ] || git clone -q https://github.com/stellar/wallet-backend.git "$SRC"

for pair in "${COMMITS[@]}"; do
  label="${pair%%=*}"; sha="${pair#*=}"
  wt="$TOOLS/src/wb-$sha"
  [ -d "$wt" ] || git -C "$SRC" worktree add -q --detach "$wt" "$sha"
  mkdir -p "$wt/internal/xdrkitreplay"
  cp "$HERE/go/xdrkitreplay/main.go" "$wt/internal/xdrkitreplay/main.go"
  git -C "$wt" checkout -q -- go.mod go.sum   # start from the pinned dependency set
  tag="pinned"
  if [ -n "${SDK:-}" ]; then
    (cd "$wt" && go mod edit -require=github.com/stellar/go-stellar-sdk@"$SDK")
    tag="sdk-$SDK"
  fi
  echo "== $label ($sha) [$tag]"
  (cd "$wt" && go run ./internal/xdrkitreplay -corpus "$KIT/corpus" -label "$label" -commit "$(git -C "$wt" rev-parse HEAD)" -out "$HERE/out/$label-$sha-$tag.json")
  git -C "$wt" diff --stat -- go.mod go.sum
done
