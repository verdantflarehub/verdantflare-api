#!/usr/bin/env sh
set -eu

SCRIPT_DIR="$(dirname "$0")"
ROOT_DIR="$(CDPATH= cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT_DIR"

VERSION="${APP_VERSION:-}"
if [ -z "$VERSION" ] && [ -f VERSION ]; then
  VERSION="$(cat VERSION)"
fi
if [ -z "$VERSION" ]; then
  VERSION="v0.0.0"
fi

export GO111MODULE=on
export CGO_ENABLED=0
export GOOS=linux
export GOEXPERIMENT=greenteagc

mkdir -p build
rm -f build/new-api build/new-api-linux-amd64 build/new-api-linux-arm64

for dist in web/default/dist/index.html web/classic/dist/index.html; do
  if [ ! -f "$dist" ]; then
    echo "missing frontend artifact: $dist" >&2
    echo "build the frontend before running backend build" >&2
    exit 1
  fi
done

go mod download

for arch in amd64 arm64; do
  echo "Building new-api ${GOOS}/${arch} (${VERSION})"
  GOARCH="${arch}" go build \
    -trimpath \
    -ldflags "-s -w -X github.com/QuantumNous/new-api/common.Version=${VERSION}" \
    -o "build/new-api-linux-${arch}" \
    .
done

cp build/new-api-linux-amd64 build/new-api
ls -lh build/new-api*
