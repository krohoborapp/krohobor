#!/bin/zsh
# Добавляет в «Дима Крохобор.app» своё окно (main.swift) — со значком в Dock и без браузера.
# Нужен Swift: на GitHub он есть, на своём маке — если установлены Xcode Command Line Tools.
# Использование: build-window.sh "<путь к .app>"
set -e
HERE="$(cd "$(dirname "$0")" && pwd)"
APP="$1"
PL="$APP/Contents/Info.plist"
OUT="$APP/Contents/MacOS/Дима Крохобор"
T=$(mktemp -d)
echo "Собираю окно программы…"
xcrun swiftc -O -target arm64-apple-macos11 "$HERE/main.swift" -o "$T/arm64"
xcrun swiftc -O -target x86_64-apple-macos11 "$HERE/main.swift" -o "$T/x86_64"
lipo -create "$T/x86_64" "$T/arm64" -output "$OUT"
chmod +x "$OUT"
rm -rf "$T"
plutil -replace CFBundleExecutable -string "Дима Крохобор" "$PL"
plutil -remove LSUIElement "$PL" 2>/dev/null || true
plutil -replace NSAppTransportSecurity -json '{"NSAllowsLocalNetworking":true,"NSExceptionDomains":{"127.0.0.1":{"NSExceptionAllowsInsecureHTTPLoads":true}}}' "$PL"
plutil -lint "$PL"
echo "Окно добавлено."
