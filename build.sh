#!/bin/sh
# Builds cpa-carpool.so in Docker with golang:1.26-bookworm, the same toolchain and
# glibc as the official CLIProxyAPI image (debian:bookworm).
#
# Usage: ./build.sh [test]        Output: out/cpa-carpool.so
#
#   test               also run go vet and the unit tests before building
#   PLATFORM=...       target platform, e.g. linux/arm64 (default: the Docker host's;
#                      a foreign platform needs QEMU emulation in Docker)
#   GO_IMAGE=...       override the build image
set -e

REPO=$(cd "$(dirname "$0")" && pwd)
IMAGE=${GO_IMAGE:-golang:1.26-bookworm}

CMD='go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o out/cpa-carpool.so . && rm -f out/cpa-carpool.h'
[ "$1" = "test" ] && CMD="go vet ./... && go test -count=1 ./... && $CMD"

mkdir -p "$REPO/out"
# shellcheck disable=SC2086
docker run --rm ${PLATFORM:+--platform "$PLATFORM"} \
  -v "$REPO:/src" \
  -v cpa-carpool-gomod:/go/pkg/mod -v cpa-carpool-gocache:/root/.cache/go-build \
  -w /src "$IMAGE" sh -c "$CMD"

ls -lh "$REPO/out/cpa-carpool.so"
