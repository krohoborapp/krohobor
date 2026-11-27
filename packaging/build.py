#!/usr/bin/env python3
"""Сборка релиза «Дима Крохобор».

Использование:  python3 packaging/build.py <версия> <папка IPA_Downloader> <куда положить>
Собирает: «Дима Крохобор.app» (универсальная программа для Apple Silicon и Intel),
папки и zip-архивы для Windows и Linux, скрипты «Собрать DMG.command» и «Проверить Intel-версию.command».
"""
import os, shutil, stat, struct, subprocess, sys, zipfile

VER, IPAD, OUT = sys.argv[1], sys.argv[2], sys.argv[3]
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PKG = os.path.join(ROOT, "packaging")
NAME = "Дима Крохобор"
SLUG = "DimaKrohobor"


def go_build(goos, goarch, out):
    env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
    subprocess.check_call(["go", "build", "-trimpath", "-ldflags", f"-s -w -X main.AppVersion={VER}", "-o", out, "."], cwd=ROOT, env=env)


def fat_macho(slices, out):
    """Универсальный Mach-O (как lipo): arm64 + x86_64 в одном файле."""
    CPU = {"arm64": (0x0100000C, 0), "amd64": (0x01000007, 3)}
    align = 14
    datas = [(CPU[a], open(p, "rb").read()) for a, p in slices]
    header = struct.pack(">II", 0xCAFEBABE, len(datas))
    offset = (8 + 20 * len(datas) + (1 << align) - 1) & ~((1 << align) - 1)
    entries, body = b"", b""
    cur = offset
    for (cpu, sub), data in datas:
        entries += struct.pack(">iiIII", cpu, sub, cur, len(data), align)
        pad = ((cur + len(data) + (1 << align) - 1) & ~((1 << align) - 1)) - (cur + len(data))
        body += data + b"\0" * pad
        cur += len(data) + pad
    with open(out, "wb") as f:
        f.write(header + entries + b"\0" * (offset - 8 - 20 * len(datas)) + body)
    os.chmod(out, 0o755)


def copy_files(dst):
    os.makedirs(dst, exist_ok=True)
    shutil.copy(os.path.join(IPAD, "Files", "AppsList.txt"), dst)
    shutil.copy(os.path.join(ROOT, "catalog.txt"), dst)


def copy_main(dst, arch_dirs):
    for a in arch_dirs:
        shutil.copytree(os.path.join(IPAD, "MainApp", a), os.path.join(dst, "MainApp", a), dirs_exist_ok=True)
        for f in os.listdir(os.path.join(dst, "MainApp", a)):
            p = os.path.join(dst, "MainApp", a, f)
            os.chmod(p, os.stat(p).st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


def licenses(dst):
    shutil.copy(os.path.join(ROOT, "LICENSE"), dst)
    shutil.copy(os.path.join(IPAD, "LICENSE"), os.path.join(dst, "LICENSE-IPA_Downloader"))


def zipdir(src, zpath, base):
    with zipfile.ZipFile(zpath, "w", zipfile.ZIP_DEFLATED) as z:
        for dp, dn, fn in os.walk(src):
            for name in dn + fn:
                p = os.path.join(dp, name)
                arc = os.path.join(base, os.path.relpath(p, src))
                info = zipfile.ZipInfo.from_file(p, arc)
                info.external_attr = (os.stat(p).st_mode & 0xFFFF) << 16
                if os.path.isdir(p):
                    z.writestr(info, b"")
                else:
                    info.compress_type = zipfile.ZIP_DEFLATED
                    with open(p, "rb") as fh:
                        z.writestr(info, fh.read())


shutil.rmtree(OUT, ignore_errors=True)
os.makedirs(OUT)
tmp = os.path.join(OUT, ".bin")
os.makedirs(tmp)

# ---------- macOS ----------
go_build("darwin", "arm64", os.path.join(tmp, "mac-arm64"))
go_build("darwin", "amd64", os.path.join(tmp, "mac-amd64"))
app = os.path.join(OUT, NAME + ".app")
macos, res = os.path.join(app, "Contents", "MacOS"), os.path.join(app, "Contents", "Resources")
os.makedirs(macos)
os.makedirs(res)
fat_macho([("arm64", os.path.join(tmp, "mac-arm64")), ("amd64", os.path.join(tmp, "mac-amd64"))], os.path.join(macos, "dimakrohobor"))
shutil.copy(os.path.join(PKG, "AppIcon.icns"), res)
copy_files(os.path.join(res, "Files"))
copy_main(res, ["macOS_arm64", "macOS_amd64"])
licenses(res)
with open(os.path.join(app, "Contents", "Info.plist"), "w", encoding="utf-8") as f:
    f.write(f"""<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>{NAME}</string>
  <key>CFBundleDisplayName</key><string>{NAME}</string>
  <key>CFBundleIdentifier</key><string>tv.krohobor.app</string>
  <key>CFBundleExecutable</key><string>dimakrohobor</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>{VER}</string>
  <key>CFBundleVersion</key><string>{VER}</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>LSUIElement</key><true/>
  <key>NSHighResolutionCapable</key><true/>
</dict></plist>
""")

dmg_script = f"""#!/bin/zsh
# Собирает {SLUG}-{VER}.dmg и {SLUG}-{VER}-mac.zip (для обновлений) из «{NAME}.app». Запускать на маке.
cd "$(dirname "$0")" || exit 1
APP="{NAME}.app"
echo "Подписываю приложение (бесплатная подпись без аккаунта разработчика)…"
chmod +x "$APP/Contents/MacOS/"* "$APP/Contents/Resources/MainApp/"*/*
xattr -cr "$APP"
codesign --force --deep --sign - "$APP" || exit 1
echo "Собираю архив для обновлений…"
rm -f "{SLUG}-{VER}-mac.zip"
ditto -c -k --keepParent "$APP" "{SLUG}-{VER}-mac.zip"
echo "Собираю DMG…"
STAGE=$(mktemp -d)
cp -R "$APP" "$STAGE/"
ln -s /Applications "$STAGE/Программы"
rm -f "{SLUG}-{VER}.dmg"
hdiutil create -volname "{NAME}" -srcfolder "$STAGE" -ov -format UDZO "{SLUG}-{VER}.dmg" || exit 1
rm -rf "$STAGE"
echo ""
echo "Готово: {SLUG}-{VER}.dmg и {SLUG}-{VER}-mac.zip лежат рядом с этим файлом."
open -R "{SLUG}-{VER}.dmg"
"""
intel_script = f"""#!/bin/zsh
# Запускает Intel-версию программы через Rosetta, чтобы проверить её на маке с M-чипом.
cd "$(dirname "$0")" || exit 1
if ! /usr/bin/pgrep -q oahd; then
  echo "Нужна Rosetta. Сейчас macOS предложит её установить."
  softwareupdate --install-rosetta --agree-to-license
fi
echo "Важно: перед проверкой закрой обычную версию программы (ссылка «Закрыть программу» внизу страницы)."
echo "Запускаю Intel-версию… В разделе «iPhone и проверка» должно быть написано «macOS amd64»."
arch -x86_64 "./{NAME}.app/Contents/MacOS/dimakrohobor"
"""
for fname, body in [("Собрать DMG.command", dmg_script), ("Проверить Intel-версию.command", intel_script)]:
    p = os.path.join(OUT, fname)
    open(p, "w", encoding="utf-8").write(body)
    os.chmod(p, 0o755)

# ---------- иконка и сведения для .exe ----------
def make_syso():
    import shutil as sh
    if not (sh.which("llvm-rc") and sh.which("llvm-cvtres")):
        print("llvm-rc не найден — .exe будет без иконки"); return None
    tmpd = os.path.join(OUT, ".rc"); os.makedirs(tmpd, exist_ok=True)
    shutil.copy(os.path.join(PKG, "icon.ico"), tmpd)
    v = (VER.split(".") + ["0", "0", "0"])[:3]
    rc = f"""#pragma code_page(65001)
1 ICON "icon.ico"
1 VERSIONINFO
FILEVERSION {v[0]},{v[1]},{v[2]},0
PRODUCTVERSION {v[0]},{v[1]},{v[2]},0
BEGIN
  BLOCK "StringFileInfo"
  BEGIN
    BLOCK "041904B0"
    BEGIN
      VALUE "CompanyName", "Дима Крохобор"
      VALUE "FileDescription", "Дима Крохобор — возврат приложений на iPhone"
      VALUE "ProductName", "Дима Крохобор"
      VALUE "ProductVersion", "{VER}"
      VALUE "FileVersion", "{VER}"
      VALUE "LegalCopyright", "MIT"
    END
  END
  BLOCK "VarFileInfo"
  BEGIN
    VALUE "Translation", 0x419, 1200
  END
END
"""
    open(os.path.join(tmpd, "app.rc"), "w", encoding="utf-8").write(rc)
    subprocess.check_call(["llvm-rc", "/C", "65001", "/FO", "app.res", "app.rc"], cwd=tmpd)
    dst = os.path.join(ROOT, "rsrc_windows_amd64.syso")
    subprocess.check_call(["llvm-cvtres", "/MACHINE:X64", "/OUT:" + dst, os.path.join(tmpd, "app.res")])
    shutil.rmtree(tmpd)
    return dst

syso = make_syso()

# ---------- Windows и Linux ----------
for goos, arch_dir, exe in [("windows", "windows_amd64", NAME + ".exe"), ("linux", "linux_amd64", "dimakrohobor")]:
    d = os.path.join(OUT, f"{SLUG}-{VER}-{goos}")
    os.makedirs(d)
    go_build(goos, "amd64", os.path.join(d, exe))
    copy_main(d, [arch_dir])
    copy_files(os.path.join(d, "Files"))
    licenses(d)
    zipdir(d, os.path.join(OUT, f"{SLUG}-{VER}-{goos}.zip"), f"{SLUG}-{VER}-{goos}")

shutil.rmtree(tmp)
if syso and os.path.exists(syso):
    os.remove(syso)
print("готово:", OUT)
