#!/usr/bin/env bash
set -Eeuo pipefail

VERSION="${1:?usage: build-release.sh VERSION}"
VERSION="${VERSION#v}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "VERSION must be a stable semantic version such as v0.1.2" >&2; exit 2; }
TAG="v$VERSION"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEV_VERSION="$(sed -n 's/.*const version = "\([0-9][0-9.]*-dev\)".*/\1/p' "$ROOT/internal/mcp/server.go" | head -n1)"
DIST="$ROOT/dist"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
SRC="$TMP/src"
mkdir -p "$SRC"

tar -C "$ROOT" --exclude='./.git' --exclude='./dist' -cf - . | tar -C "$SRC" -xf -
[ -n "$DEV_VERSION" ] || { echo "release development version marker not found" >&2; exit 1; }
grep -qF "const version = \"$DEV_VERSION\"" "$SRC/internal/mcp/server.go" || { echo "release development version marker '$DEV_VERSION' not found" >&2; exit 1; }
sed -i "s/const version = \"$DEV_VERSION\"/const version = \"$VERSION\"/" "$SRC/internal/mcp/server.go"
grep -qF "const version = \"$VERSION\"" "$SRC/internal/mcp/server.go"

rm -rf "$DIST"
mkdir -p "$DIST"
ARCH=amd64
OUT="$DIST/ai-server-agent_${VERSION}_linux_${ARCH}"
mkdir -p "$OUT"
(
  cd "$SRC"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags='-s -w' -o "$OUT/ai-server-agent" ./cmd/ai-server-agent
)
cp "$ROOT/README.md" "$ROOT/LICENSE" "$ROOT/manage.sh" "$ROOT/update.sh" "$ROOT/uninstall.sh" "$OUT/"
tar -C "$DIST" -czf "$DIST/ai-server-agent_${VERSION}_linux_${ARCH}.tar.gz" "$(basename "$OUT")"
rm -rf "$OUT"

# The public install.sh release asset is pinned to this exact stable tag. The
# embedded installer still verifies the release archive against SHA256SUMS.
{
  printf '#!/usr/bin/env bash\n'
  printf 'export AI_SERVER_AGENT_VERSION=%q\n' "$TAG"
  printf 'export AI_SERVER_AGENT_REF=%q\n' "$TAG"
  tail -n +2 "$ROOT/install.sh"
} > "$DIST/install.sh"
chmod 0755 "$DIST/install.sh"

(
  cd "$DIST"
  sha256sum "ai-server-agent_${VERSION}_linux_${ARCH}.tar.gz" > SHA256SUMS
)
