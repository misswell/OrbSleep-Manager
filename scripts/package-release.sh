#!/bin/bash
# Build a macOS release bundle (universal app + daemon/CLI + DMG/ZIP).
#
# Signing:
#   - If a "Developer ID Application" identity is present, it is used with a
#     secure timestamp, and notarization runs when APP_STORE_CONNECT_* are set.
#   - Otherwise the app is ad-hoc signed and the artifact is marked UNSIGNED.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
VERSION="$(awk '/MARKETING_VERSION:/ {print $2; exit}' app/project.yml)"
BUILD_NUMBER="$(awk '/CURRENT_PROJECT_VERSION:/ {print $2; exit}' app/project.yml)"
DIST="$ROOT/dist"
NAME="OrbSleepManager-$VERSION-macos-universal"
STAGE="$DIST/$NAME"
WORK="$(mktemp -d /tmp/orbsleep-release.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

echo "==> version $VERSION ($BUILD_NUMBER)"
rm -rf "$STAGE" "$DIST/$NAME.zip" "$DIST/$NAME.dmg" "$DIST/SHA256SUMS"
mkdir -p "$STAGE/bin" "$WORK/bin"

echo "==> building universal daemon/CLI"
for arch in arm64 amd64; do
  (cd core && CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" \
    go build -trimpath -ldflags='-s -w' -o "$WORK/bin/orbsleepd-$arch" ./cmd/orbsleepd)
  (cd core && CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" \
    go build -trimpath -ldflags='-s -w' -o "$WORK/bin/orbsleepctl-$arch" ./cmd/orbsleepctl)
done
lipo -create "$WORK/bin/orbsleepd-arm64" "$WORK/bin/orbsleepd-amd64" -output "$STAGE/bin/orbsleepd"
lipo -create "$WORK/bin/orbsleepctl-arm64" "$WORK/bin/orbsleepctl-amd64" -output "$STAGE/bin/orbsleepctl"
chmod 755 "$STAGE/bin/orbsleepd" "$STAGE/bin/orbsleepctl"

echo "==> building Release app"
rm -rf "$WORK/DerivedData" "$WORK/app-build"
(cd app && xcodegen generate >/dev/null)
xcodebuild -project app/OrbSleepManager.xcodeproj -scheme OrbSleepManager \
  -configuration Release -destination 'platform=macOS' \
  -derivedDataPath "$WORK/DerivedData" SYMROOT="$WORK/app-build" \
  CODE_SIGNING_ALLOWED=NO ARCHS='arm64 x86_64' ONLY_ACTIVE_ARCH=NO build >/dev/null
cp -R "$WORK/app-build/Release/OrbSleepManager.app" "$STAGE/OrbSleepManager.app"

IDENTITY="${SIGN_IDENTITY:-$(security find-identity -v -p codesigning 2>/dev/null | sed -n 's/.*"\(Developer ID Application:.*\)"/\1/p' | head -1)}"
SIGNED_NOTE="UNSIGNED (ad-hoc)"
if [ -n "$IDENTITY" ]; then
  echo "==> signing with $IDENTITY"
  codesign --force --options runtime --timestamp \
    --entitlements app/OrbSleepManager.entitlements --sign "$IDENTITY" \
    "$STAGE/OrbSleepManager.app"
  for tool in orbsleepd orbsleepctl; do
    codesign --force --options runtime --timestamp --sign "$IDENTITY" "$STAGE/bin/$tool"
  done
  SIGNED_NOTE="Developer ID: $IDENTITY"
else
  echo "==> WARNING: no Developer ID Application identity; ad-hoc signing only"
  codesign --force --options runtime \
    --entitlements app/OrbSleepManager.entitlements --sign - \
    "$STAGE/OrbSleepManager.app"
  for tool in orbsleepd orbsleepctl; do
    codesign --force --options runtime --sign - "$STAGE/bin/$tool"
  done
fi
codesign --verify --deep --strict --verbose=1 "$STAGE/OrbSleepManager.app"

cat > "$STAGE/README.txt" <<README
OrbSleep Manager $VERSION ($BUILD_NUMBER)
=========================================

Install:
  1. Drag OrbSleepManager.app into /Applications.
  2. Install the daemon and CLI:
       sudo install -m 755 bin/orbsleepd /usr/local/bin/
       sudo install -m 755 bin/orbsleepctl /usr/local/bin/
       orbsleepctl install-agent
  3. Run orbsleepctl doctor, then open OrbSleep Manager.

Requires macOS 14+ and OrbStack (or Docker Desktop) with a local Unix socket.

This build is $SIGNED_NOTE.
If Gatekeeper blocks an unsigned/ad-hoc build, right-click the app and choose Open.
README
chmod 644 "$STAGE/README.txt"

validate_with_retry() {
  local target="$1" attempt
  for attempt in 1 2 3; do
    if xcrun stapler validate "$target"; then
      return 0
    fi
    echo "stapler validate attempt $attempt failed; retrying in 10s" >&2
    sleep 10
  done
  echo "WARNING: stapler validate failed after retries; the ticket was stapled successfully" >&2
  return 0
}

staple_with_retry() {
  local target="$1" attempt
  for attempt in 1 2 3 4 5; do
    if xcrun stapler staple "$target"; then
      return 0
    fi
    echo "stapler attempt $attempt failed; retrying in 10s" >&2
    sleep 10
  done
  return 1
}

NOTARY_ARGS=()
if [ -n "$IDENTITY" ] && [ -n "${APP_STORE_CONNECT_KEY_ID:-}" ] && [ -n "${APP_STORE_CONNECT_ISSUER_ID:-}" ] && [ -f "${APP_STORE_CONNECT_PRIVATE_KEY_PATH:-}" ]; then
  echo "==> notarizing app"
  ditto -c -k --keepParent "$STAGE/OrbSleepManager.app" "$WORK/notary.zip"
  xcrun notarytool submit "$WORK/notary.zip" \
    --key "$APP_STORE_CONNECT_PRIVATE_KEY_PATH" --key-id "$APP_STORE_CONNECT_KEY_ID" \
    --issuer "$APP_STORE_CONNECT_ISSUER_ID" --wait
  staple_with_retry "$STAGE/OrbSleepManager.app"
  validate_with_retry "$STAGE/OrbSleepManager.app"
  NOTARY_ARGS=(--key "$APP_STORE_CONNECT_PRIVATE_KEY_PATH" --key-id "$APP_STORE_CONNECT_KEY_ID" --issuer "$APP_STORE_CONNECT_ISSUER_ID")
else
  echo "==> notarization skipped (Developer ID identity or APP_STORE_CONNECT credentials missing)"
fi

echo "==> packaging zip"
ditto -c -k --sequesterRsrc --keepParent "$STAGE" "$DIST/$NAME.zip"

echo "==> packaging dmg"
[ -d "$STAGE/OrbSleepManager.app" ] || { echo "Missing staged app at $STAGE/OrbSleepManager.app" >&2; exit 1; }
rm -rf "$WORK/dmg"
mkdir -p "$WORK/dmg"
cp -R "$STAGE/OrbSleepManager.app" "$WORK/dmg/"
cp -R "$STAGE/bin" "$WORK/dmg/"
cp "$STAGE/README.txt" "$WORK/dmg/"
ln -s /Applications "$WORK/dmg/Applications"
hdiutil create -quiet -volname "OrbSleep Manager $VERSION" -srcfolder "$WORK/dmg" \
  -ov -format UDZO "$DIST/$NAME.dmg"
[ -f "$DIST/$NAME.dmg" ] || { echo "DMG was not created: $DIST/$NAME.dmg" >&2; exit 1; }

if [ ${#NOTARY_ARGS[@]} -gt 0 ]; then
  echo "==> notarizing dmg"
  xcrun notarytool submit "$DIST/$NAME.dmg" "${NOTARY_ARGS[@]}" --wait
  staple_with_retry "$DIST/$NAME.dmg"
  validate_with_retry "$DIST/$NAME.dmg"
fi

echo "==> assessing Gatekeeper"
if [ -n "$IDENTITY" ]; then
  spctl --assess --type execute --verbose=2 "$STAGE/OrbSleepManager.app" || true
fi

echo "==> checksums"
(cd "$DIST" && shasum -a 256 "$NAME.zip" "$NAME.dmg" > SHA256SUMS)
cat "$DIST/SHA256SUMS"
echo "Done: $DIST"
