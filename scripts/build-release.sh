#!/usr/bin/env bash
set -Eeuo pipefail
VERSION="${1:?usage: build-release.sh VERSION}"
mkdir -p dist
for arch in amd64 arm64; do
  out="dist/ai-server-agent_${VERSION#v}_linux_${arch}"
  rm -rf "$out"; mkdir -p "$out"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "$out/ai-server-agent" ./cmd/ai-server-agent
  cp README.md LICENSE "$out/"
  tar -C dist -czf "dist/ai-server-agent_${VERSION#v}_linux_${arch}.tar.gz" "$(basename "$out")"
  rm -rf "$out"
done
(cd dist && sha256sum *.tar.gz > SHA256SUMS)
