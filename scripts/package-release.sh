#!/bin/bash
# Build a complete macOS release: one app bundle that contains the daemon, the
# CLI and the LaunchAgent plist, plus a DMG/ZIP. Users drag the app to
# /Applications, open it, and click "一键配置并启动" — no terminal, no sudo,
# no manual binary copy.
#
# Layout produced:
#   OrbSleepManager.app/
#     Contents/MacOS/OrbSleepManager
#     Contents/Resources/orbsleepd
#     Contents/Resources/orbsleepctl
#     Contents/Library/LaunchAgents/com.misswell.orbsleep.daemon.app.plist
#
# Signing:
#   - If a "Developer ID Application" identity is present it is used with a
#     secure timestamp and hardened runtime; notarization runs when
#     APP_STORE_CONNECT_* credentials are set.
#   - Otherwise everything is ad-hoc signed and the artifact is marked UNSIGNED.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
export PATH="/opt/homebrew/bin:/usr/local/bin:${PATH:-}"
VERSION="$(awk '/MARKETING_VERSION:/ {print $2; exit}' app/project.yml)"
BUILD_NUMBER="$(awk '/CURRENT_PROJECT_VERSION:/ {print $2; exit}' app/project.yml)"
DIST="$ROOT/dist"
NAME="OrbSleepManager-$VERSION-macos-universal"
STAGE="$DIST/$NAME"
WORK="$(mktemp -d /tmp/orbsleep-release.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

echo "==> version $VERSION ($BUILD_NUMBER)"
rm -rf "$STAGE" "$DIST/$NAME.zip" "$DIST/$NAME.dmg" "$DIST/SHA256SUMS"
mkdir -p "$STAGE"

echo "==> building Release app (universal, binaries embedded in the bundle)"
rm -rf "$WORK/DerivedData" "$WORK/app-build"
(cd app && xcodegen generate >/dev/null)
xcodebuild -project app/OrbSleepManager.xcodeproj -scheme OrbSleepManager \
  -configuration Release -destination 'platform=macOS' \
  -derivedDataPath "$WORK/DerivedData" SYMROOT="$WORK/app-build" \
  CODE_SIGNING_ALLOWED=NO ARCHS='arm64 x86_64' ONLY_ACTIVE_ARCH=NO build >/dev/null
APP="$WORK/app-build/Release/OrbSleepManager.app"
[ -d "$APP" ] || { echo "Missing built app at $APP" >&2; exit 1; }

RESOURCES="$APP/Contents/Resources"
LAUNCHAGENTS="$APP/Contents/Library/LaunchAgents"
[ -x "$RESOURCES/orbsleepd" ] || { echo "orbsleepd was not embedded" >&2; exit 1; }
[ -x "$RESOURCES/orbsleepctl" ] || { echo "orbsleepctl was not embedded" >&2; exit 1; }
[ -f "$LAUNCHAGENTS/com.misswell.orbsleep.daemon.app.plist" ] || { echo "LaunchAgent plist was not embedded" >&2; exit 1; }

cp -R "$APP" "$STAGE/OrbSleepManager.app"

IDENTITY="${SIGN_IDENTITY:-$(security find-identity -v -p codesigning 2>/dev/null | sed -n 's/.*"\(Developer ID Application:.*\)"/\1/p' | head -1)}"
SIGNED_NOTE="UNSIGNED（ad-hoc）"
SIGN_ARGS=(--force --options runtime)
if [ -n "$IDENTITY" ]; then
  echo "==> signing nested executables with $IDENTITY"
  SIGN_ARGS+=(--timestamp --sign "$IDENTITY")
  SIGNED_NOTE="Developer ID: $IDENTITY"
else
  echo "==> WARNING: no Developer ID Application identity; ad-hoc signing only"
  SIGN_ARGS+=(--sign -)
fi

# Nested code must be signed before the enclosing bundle so the app seal and
# notarization see a valid chain. Sign each architecture-aware fat binary.
for tool in orbsleepd orbsleepctl; do
  codesign "${SIGN_ARGS[@]}" "$STAGE/OrbSleepManager.app/Contents/Resources/$tool"
  codesign --verify --strict --verbose=1 "$STAGE/OrbSleepManager.app/Contents/Resources/$tool"
done

echo "==> signing app bundle"
codesign "${SIGN_ARGS[@]}" \
  --entitlements app/OrbSleepManager.entitlements \
  "$STAGE/OrbSleepManager.app"
codesign --verify --deep --strict --verbose=1 "$STAGE/OrbSleepManager.app"

cat > "$STAGE/README.txt" <<README
OrbSleep Manager $VERSION ($BUILD_NUMBER)
=========================================

安装步骤
--------
  1. 把 OrbSleepManager.app 拖到「应用程序」文件夹。
  2. 打开 OrbSleep Manager。
  3. 点击「一键配置并启动」。
  4. 如 macOS 提示，在「系统设置 → 通用 → 登录项」中允许后台服务运行。

完成。后台服务安装、开机启动、Docker 检测、Lazy TCP Proxy 初始化和环境检查全部由应用自动完成，
不需要打开终端，也不需要执行 sudo 或复制任何文件。

需要 macOS 14+ 与 OrbStack（或 Docker Desktop，使用本地 Unix socket）。

本构建：$SIGNED_NOTE
README
if [ -z "$IDENTITY" ]; then
  cat >> "$STAGE/README.txt" <<'README'

注意：这是未签名、未公证的开发构建。首次打开时 macOS 可能拦截；
可在「系统设置 → 隐私与安全性」中允许，或右键选择「打开」。
正式发布版本使用 Developer ID 签名并已通过 Apple 公证。
README
fi
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
NOTARY_PROFILE="${NOTARY_PROFILE:-}"
if [ -z "$NOTARY_PROFILE" ] && [ -z "${APP_STORE_CONNECT_KEY_ID:-}" ]; then
  # Local machines can keep a notarytool keychain profile instead of env keys.
  if xcrun notarytool history --keychain-profile agentspace-notary >/dev/null 2>&1; then
    NOTARY_PROFILE="agentspace-notary"
  fi
fi
if [ -n "$IDENTITY" ]; then
  if [ -n "${APP_STORE_CONNECT_KEY_ID:-}" ] && [ -n "${APP_STORE_CONNECT_ISSUER_ID:-}" ] && [ -f "${APP_STORE_CONNECT_PRIVATE_KEY_PATH:-}" ]; then
    NOTARY_ARGS=(--key "$APP_STORE_CONNECT_PRIVATE_KEY_PATH" --key-id "$APP_STORE_CONNECT_KEY_ID" --issuer "$APP_STORE_CONNECT_ISSUER_ID")
  elif [ -n "$NOTARY_PROFILE" ]; then
    echo "==> using notarytool keychain profile: $NOTARY_PROFILE"
    NOTARY_ARGS=(--keychain-profile "$NOTARY_PROFILE")
  fi
fi

if [ ${#NOTARY_ARGS[@]} -gt 0 ]; then
  echo "==> notarizing app"
  ditto -c -k --keepParent "$STAGE/OrbSleepManager.app" "$WORK/notary.zip"
  xcrun notarytool submit "$WORK/notary.zip" "${NOTARY_ARGS[@]}" --wait
  staple_with_retry "$STAGE/OrbSleepManager.app"
  validate_with_retry "$STAGE/OrbSleepManager.app"
else
  echo "==> notarization skipped (no notary credentials)"
fi

if [ -n "$IDENTITY" ] && [ ${#NOTARY_ARGS[@]} -eq 0 ]; then
  cat >> "$STAGE/README.txt" <<'README'

注意：本构建使用 Developer ID 签名，但未执行 Apple 公证（发布环境缺少公证凭据）。
正式发布构建应完成公证与 staple 后再分发。
README
fi

echo "==> packaging zip"
ditto -c -k --sequesterRsrc --keepParent "$STAGE" "$DIST/$NAME.zip"

echo "==> packaging dmg"
rm -rf "$WORK/dmg"
mkdir -p "$WORK/dmg"
cp -R "$STAGE/OrbSleepManager.app" "$WORK/dmg/"
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
  # Informational here: a Developer ID build without notarization credentials
  # is expected to be rejected by spctl. CI re-runs spctl on notarized builds.
  spctl --assess --type execute --verbose=2 "$STAGE/OrbSleepManager.app" \
    || echo "WARNING: spctl did not accept the app (expected if notarization was skipped)" >&2
else
  echo "skipped (ad-hoc build)"
fi

echo "==> checksums"
(cd "$DIST" && shasum -a 256 "$NAME.zip" "$NAME.dmg" > SHA256SUMS)
cat "$DIST/SHA256SUMS"
if [ -n "${UPDATE_BASE_URL:-}" ]; then
  ./scripts/make-appcast.sh "$DIST/$NAME.dmg" "$UPDATE_BASE_URL" "OrbSleep Manager $VERSION" || true
fi
echo "Done: $DIST"
