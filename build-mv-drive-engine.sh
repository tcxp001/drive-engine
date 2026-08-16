#!/bin/sh
set -eu

output="${1:-mv-drive-engine}"
target_arch="${2:-}"

unset GOAMD64 GOARM64
case "$target_arch" in
  amd64)
    GOAMD64=v1
    export GOAMD64
    ;;
  arm64)
    GOARM64=v8.0
    export GOARM64
    ;;
  *)
    printf 'unsupported target architecture: %s\n' "${target_arch:-<empty>}" >&2
    exit 2
    ;;
esac

mkdir -p public/dist
printf '%s\n' '<!doctype html><html><body>embedded drive engine</body></html>' \
  > public/dist/index.html

CGO_ENABLED=0 GOOS=linux GOARCH="$target_arch" \
  go build -trimpath -tags=jsoniter -ldflags="-s -w" -o "$output" .
