#!/bin/bash
# Embed orbsleepd / orbsleepctl and the LaunchAgent plist into the app bundle.
#
# Usage: scripts/embed-service.sh <repo-root> <app-contents-dir>
#
# Called from the Xcode post-build phase (app/project.yml). Everything used at
# runtime ships inside OrbSleepManager.app, so users never copy binaries or run
# commands. Signing happens after this phase, so the nested code is covered by
# the app signature and by notarization.
set -euo pipefail
ROOT="${1:?usage: embed-service.sh <repo-root> <app-contents-dir>}"
CONTENTS="${2:?usage: embed-service.sh <repo-root> <app-contents-dir>}"
CONFIG="${CONFIGURATION:-Debug}"

export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
export GOFLAGS="${GOFLAGS:-}"

RESOURCES="$CONTENTS/Resources"
LAUNCHAGENTS="$CONTENTS/Library/LaunchAgents"
mkdir -p "$RESOURCES" "$LAUNCHAGENTS"

# App icon (CFBundleIconFile = AppIcon).
if [ -f "$ROOT/app/Support/AppIcon.icns" ]; then
  cp "$ROOT/app/Support/AppIcon.icns" "$RESOURCES/AppIcon.icns"
  chmod 644 "$RESOURCES/AppIcon.icns"
fi

# Remove the 0.1.x plist name if an incremental build left it behind.
rm -f "$LAUNCHAGENTS/com.misswell.orbsleep.daemon.plist"

# Copy the signed LaunchAgent plist that SMAppService reads from the bundle.
cp "$ROOT/app/Support/com.misswell.orbsleep.daemon.app.plist" \
   "$LAUNCHAGENTS/com.misswell.orbsleep.daemon.app.plist"
chmod 644 "$LAUNCHAGENTS/com.misswell.orbsleep.daemon.app.plist"

have_go() { command -v go >/dev/null 2>&1; }

build_native() {
  local out="$1" pkg="$2"
  ( cd "$ROOT/core" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$out" "$pkg" )
}

build_universal() {
  local out="$1" pkg="$2" work
  work="$(mktemp -d /tmp/orbsleep-universal.XXXXXX)"
  for arch in arm64 amd64; do
    ( cd "$ROOT/core" && CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" \
        go build -trimpath -ldflags='-s -w' -o "$work/$(basename "$out")-$arch" "$pkg" )
  done
  lipo -create "$work/$(basename "$out")-arm64" "$work/$(basename "$out")-amd64" -output "$out"
  rm -rf "$work"
}

install_tool() {
  local name="$1" pkg="$2"
  local dest="$RESOURCES/$name"
  if have_go; then
    if [ "$CONFIG" = "Release" ]; then
      build_universal "$dest" "$pkg"
    else
      build_native "$dest" "$pkg"
    fi
  elif [ -x "$ROOT/bin/$name" ]; then
    cp "$ROOT/bin/$name" "$dest"
    echo "warning: go not found; embedded prebuilt $ROOT/bin/$name" >&2
  else
    echo "error: go toolchain not found and $ROOT/bin/$name is missing" >&2
    echo "       install Go (brew install go) and rebuild, or run 'make build-core'" >&2
    exit 1
  fi
  chmod 755 "$dest"
  # Remove extended attributes that would break strict signature validation.
  xattr -cr "$dest" 2>/dev/null || true
}

install_tool orbsleepd ./cmd/orbsleepd
install_tool orbsleepctl ./cmd/orbsleepctl

# Debug builds keep the linker's ad-hoc signature on the nested tools; Xcode
# signs the app bundle afterwards. scripts/package-release.sh re-signs every
# nested executable with the Developer ID identity, hardened runtime and a
# timestamp before notarization.
echo "embedded $RESOURCES/orbsleepd $RESOURCES/orbsleepctl"
