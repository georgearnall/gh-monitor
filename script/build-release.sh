#!/usr/bin/env bash
set -euo pipefail

platforms=(
  darwin-arm64
)

rm -rf dist
mkdir -p dist

for p in "${platforms[@]}"; do
  goos="${p%-*}"
  goarch="${p#*-}"
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED="${CGO_ENABLED:-0}" go build -trimpath -ldflags="-s -w" -o "dist/${p}"
done
