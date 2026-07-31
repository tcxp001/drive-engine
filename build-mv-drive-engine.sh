#!/bin/sh
set -eu

output="${1:-mv-drive-engine}"

mkdir -p public/dist
printf '%s\n' '<!doctype html><html><body>embedded drive engine</body></html>' \
  > public/dist/index.html

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -tags=jsoniter -ldflags="-s -w" -o "$output" .
