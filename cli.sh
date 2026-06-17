#!/usr/bin/env bash
# cli.sh — build and test tide inside a pinned Docker environment.
#
#   ./cli.sh build            build bin/tide for the host OS/arch
#   ./cli.sh test [args]      go test -race (default ./...; args pass through)
#   ./cli.sh check            gofmt + go vet
#   ./cli.sh ci               check + build + test
#   ./cli.sh shell            interactive shell in the build environment
#
# Containers run with --network=none: the spec requires the repo to build
# offline, from itself, forever (all dependencies are vendored), and every
# run re-proves it. Override the toolchain image with TIDE_BUILD_IMAGE.
set -euo pipefail
cd "$(dirname "$0")"

IMAGE="${TIDE_BUILD_IMAGE:-golang:1.26-bookworm}"

# build targets the host platform so bin/tide runs where cli.sh ran; pure Go
# cross-compiles inside the pinned (linux) container. test/check still run as
# linux — cross-compiled tests can't execute there.
case "$(uname -s)" in
Darwin) HOST_GOOS=darwin ;;
*) HOST_GOOS=linux ;;
esac
case "$(uname -m)" in
arm64 | aarch64) HOST_GOARCH=arm64 ;;
*) HOST_GOARCH=amd64 ;;
esac

run() {
  mkdir -p .cache/go-build bin
  docker run --rm \
    --network=none \
    -u "$(id -u):$(id -g)" \
    -v "$PWD:/src" \
    -w /src \
    -e HOME=/tmp \
    -e GOCACHE=/src/.cache/go-build \
    -e GOFLAGS=-mod=vendor \
    "$IMAGE" "$@"
}

need_docker() {
  command -v docker >/dev/null 2>&1 || {
    echo "cli.sh: docker is required but not found" >&2
    exit 1
  }
}

cmd="${1:-help}"
shift || true

case "$cmd" in
build)
  need_docker
  run env GOOS="$HOST_GOOS" GOARCH="$HOST_GOARCH" go build -o bin/ ./cmd/tide ./cmd/teddy
  echo "built bin/tide and bin/teddy ($HOST_GOOS/$HOST_GOARCH)"
  ;;
test)
  need_docker
  if [ "$#" -eq 0 ]; then
    run go test -race ./...
  else
    run go test -race "$@"
  fi
  ;;
check)
  need_docker
  run sh -c '
    bad=$(gofmt -l cmd internal)
    if [ -n "$bad" ]; then
      echo "gofmt: needs formatting:" >&2
      echo "$bad" >&2
      exit 1
    fi
    go vet ./...
  '
  ;;
ci)
  "$0" check
  "$0" build
  "$0" test
  ;;
shell)
  need_docker
  mkdir -p .cache/go-build bin
  docker run --rm -it \
    -u "$(id -u):$(id -g)" \
    -v "$PWD:/src" \
    -w /src \
    -e HOME=/tmp \
    -e GOCACHE=/src/.cache/go-build \
    -e GOFLAGS=-mod=vendor \
    "$IMAGE" bash
  ;;
help | *)
  sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
  ;;
esac
