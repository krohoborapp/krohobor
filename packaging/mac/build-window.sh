#!/bin/zsh
# Добавляет в «Дима Крохобор.app» своё окно (main.swift) — со значком в Dock и без браузера.
# Нужен Swift: на GitHub он есть, на своём маке — если установлены Xcode Command Line Tools.
# Использование: build-window.sh "<путь к .app>"
set -e
HERE="$(cd "$(dirname "$0")" && pwd)"
APP="$1"
PL="$APP/Contents/Info.plist"
OUT="$APP/Contents/MacOS/DimaKrohobor"
T=$(mktemp -d)
echo "Собираю окно программы…"
xcrun swiftc -O -target arm64-apple-macos11 "$HERE/main.swift" -o "$T/arm64"
xcrun swiftc -O -target x86_64-apple-macos11 "$HERE/main.swift" -o "$T/x86_64"
lipo -create "$T/x86_64" "$T/arm64" -output "$OUT"
chmod +x "$OUT"
rm -rf "$T"
PB=/usr/libexec/PlistBuddy
$PB -c "Set :CFBundleExecutable DimaKrohobor" "$PL"
$PB -c "Delete :LSUIElement" "$PL" 2>/dev/null || true
$PB -c "Delete :NSAppTransportSecurity" "$PL" 2>/dev/null || true
$PB -c "Add :NSAppTransportSecurity dict" "$PL"
$PB -c "Add :NSAppTransportSecurity:NSAllowsLocalNetworking bool true" "$PL"
$PB -c "Add :NSAppTransportSecurity:NSExceptionDomains dict" "$PL"
$PB -c "Add :NSAppTransportSecurity:NSExceptionDomains:127.0.0.1 dict" "$PL"
$PB -c "Add :NSAppTransportSecurity:NSExceptionDomains:127.0.0.1:NSExceptionAllowsInsecureHTTPLoads bool true" "$PL"
echo "Окно добавлено."
