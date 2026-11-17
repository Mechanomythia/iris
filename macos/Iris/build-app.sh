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
iconutil -c icns macos/Iris/icon/AppIcon.iconset -o "$APP/Contents/Resources/AppIcon.icns"
mkdir -p "$APP/Contents/Resources/Fonts"
cp macos/Iris/Fonts/*.ttf macos/Iris/Fonts/OFL-*.txt "$APP/Contents/Resources/Fonts/"

echo "==> Sign"
IDENTITY=${CODESIGN_IDENTITY:--}   # "-" = ad-hoc; set to "Developer ID Application: ..." for distribution
OPTS=(--force --timestamp=none --sign "$IDENTITY")
if [ "$IDENTITY" != "-" ]; then OPTS=(--force --options runtime --timestamp --sign "$IDENTITY"); fi
codesign "${OPTS[@]}" "$APP/Contents/Resources/bin/iris-helper"
codesign "${OPTS[@]}" "$APP/Contents/Resources/bin/iris"
codesign "${OPTS[@]}" "$APP"
codesign --verify --deep --strict "$APP"

echo "==> Zip"
( cd "$DIST" && ditto -c -k --keepParent Iris.app "Iris-$VERSION.zip" )
echo "built $APP and $DIST/Iris-$VERSION.zip"
