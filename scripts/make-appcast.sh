#!/bin/bash
# Generate an update feed JSON for the in-app update center.
# Usage: scripts/make-appcast.sh <dmg-path> <base-url> [notes]
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DMG="${1:?usage: make-appcast.sh <dmg-path> <base-url> [notes]}"
BASE="${2:?usage: make-appcast.sh <dmg-path> <base-url> [notes]}"
NOTES="${3:-OrbSleep Manager update}"
[ -f "$DMG" ] || { echo "missing dmg: $DMG" >&2; exit 1; }
NAME="$(basename "$DMG")"
SHA="$(shasum -a 256 "$DMG" | awk '{print $1}')"
SIZE="$(stat -f%z "$DMG")"
VERSION="$(awk '/MARKETING_VERSION:/ {print $2; exit}' "$ROOT/app/project.yml")"
PUBLISHED="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
OUT="$ROOT/dist/appcast.json"
cat > "$OUT" <<JSON
{
  "version": "$VERSION",
  "downloadURL": "${BASE%/}/$NAME",
  "sha256": "$SHA",
  "sizeBytes": $SIZE,
  "publishedAt": "$PUBLISHED",
  "notes": "$NOTES"
}
JSON
echo "wrote $OUT"
cat "$OUT"
