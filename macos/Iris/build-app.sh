#!/usr/bin/env bash
# Builds dist/Iris.app: SwiftUI menu bar app + universal Go binaries bundled
# in Contents/Resources/bin. Requires macOS with Xcode command line tools and Go.
set -euo pipefail
cd "$(dirname "$0")/../.."
ROOT=$(pwd)
VERSION=${VERSION:-0.2.0}
DIST="$ROOT/dist"
APP="$DIST/Iris.app"
rm -rf "$APP" && mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources/bin"

echo "==> Go binaries (universal)"
for bin in iris iris-helper; do
  for arch in arm64 amd64; do
    CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "-s -w -X github.com/mechanomythia/iris/internal/broker.Version=$VERSION" -o "$DIST/$bin-$arch" "./cmd/$bin"
  done
  lipo -create -output "$APP/Contents/Resources/bin/$bin" "$DIST/$bin-arm64" "$DIST/$bin-amd64"
  rm "$DIST/$bin-arm64" "$DIST/$bin-amd64"
done

echo "==> Swift app (universal)"
( cd macos/Iris && swift build -c release --arch arm64 --arch x86_64 )
BIN=$(cd macos/Iris && swift build -c release --arch arm64 --arch x86_64 --show-bin-path)
cp "$BIN/Iris" "$APP/Contents/MacOS/Iris"
sed "s/__VERSION__/$VERSION/g" macos/Iris/Info.plist > "$APP/Contents/Info.plist"

echo "==> Sparkle.framework"
FW_SRC="$BIN/Sparkle.framework"
if [ ! -d "$FW_SRC" ]; then
  FW_SRC=$(find macos/Iris/.build -type d -name Sparkle.framework -path "*macos*" | head -1)
fi
test -d "$FW_SRC" || { echo "Sparkle.framework not found in the Swift build"; exit 1; }
mkdir -p "$APP/Contents/Frameworks"
ditto "$FW_SRC" "$APP/Contents/Frameworks/Sparkle.framework"
FW="$APP/Contents/Frameworks/Sparkle.framework"
FWV="$FW/Versions/$(readlink "$FW/Versions/Current")"
# XPC services are only needed by sandboxed apps; Iris isn't sandboxed.
rm -rf "$FWV/XPCServices" "$FW/XPCServices"

# Auto-update is enabled only in builds that carry the update-signing key.
if [ -n "${SPARKLE_ED_PRIVATE_KEY:-}" ]; then
  PUB=$(go run ./cmd/iris dev sparkle-pubkey)
  /usr/libexec/PlistBuddy -c "Add :SUPublicEDKey string $PUB" "$APP/Contents/Info.plist"
  echo "auto-update enabled (SUPublicEDKey $PUB)"
else
  echo "auto-update disabled: no SPARKLE_ED_PRIVATE_KEY"
fi
iconutil -c icns macos/Iris/icon/AppIcon.iconset -o "$APP/Contents/Resources/AppIcon.icns"
mkdir -p "$APP/Contents/Resources/Fonts"
cp macos/Iris/Fonts/*.ttf macos/Iris/Fonts/OFL-*.txt "$APP/Contents/Resources/Fonts/"

echo "==> Sign"
IDENTITY=${CODESIGN_IDENTITY:--}   # "-" = ad-hoc; set to "Developer ID Application: ..." for distribution
OPTS=(--force --timestamp=none --sign "$IDENTITY")
if [ "$IDENTITY" != "-" ]; then OPTS=(--force --options runtime --timestamp --sign "$IDENTITY"); fi
# Inside out: Sparkle's helpers, the framework, our binaries, then the app.
codesign "${OPTS[@]}" "$FWV/Autoupdate"
codesign "${OPTS[@]}" "$FWV/Updater.app"
codesign "${OPTS[@]}" "$FW"
codesign "${OPTS[@]}" "$APP/Contents/Resources/bin/iris-helper"
codesign "${OPTS[@]}" "$APP/Contents/Resources/bin/iris"
codesign "${OPTS[@]}" "$APP"
codesign --verify --deep --strict "$APP"

echo "==> Zip"
( cd "$DIST" && ditto -c -k --keepParent Iris.app "Iris-$VERSION.zip" )
echo "built $APP and $DIST/Iris-$VERSION.zip"
