#!/bin/sh
# Builds cpa-carpool.so for linux/amd64 in Docker (golang:1.26-bookworm)
# matching the official CLIProxyAPI toolchain and libc.
#
# Usage: ./build.sh [test]        Output: out/cpa-carpool.so
set -e

REPO=$(cd "$(dirname "$0")" && pwd)
PARENT=$(dirname "$REPO")
NAME=$(basename "$REPO")
CPA_VERSION=${CPA_VERSION:-v8.0.30}

CMD='go mod tidy && go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o out/cpa-carpool.so . && rm -f out/cpa-carpool.h'
[ "$1" = "test" ] && CMD="go mod tidy && go vet ./... && go test -count=1 ./... && $CMD"

if [ ! -d "$PARENT/CLIProxyAPI" ]; then
  CMD="git clone -q --depth 1 --branch $CPA_VERSION https://github.com/router-for-me/CLIProxyAPI /work/CLIProxyAPI && $CMD"
fi

mkdir -p "$REPO/out"
docker run --rm \
  -v "$PARENT:/work" \
  -v cpa-carpool-gomod:/go/pkg/mod -v cpa-carpool-gocache:/root/.cache/go-build \
  -w "/work/$NAME" golang:1.26-bookworm sh -c "$CMD"

ls -lh "$REPO/out/cpa-carpool.so"
