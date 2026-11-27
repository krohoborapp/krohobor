package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Отдельное окно программы: интерфейс открывается в Chrome, Edge, Яндекс Браузере,
// Brave или Chromium в режиме приложения — без вкладок, адресной строки и закладок.
// Если ни одного такого браузера нет, открываем обычную вкладку в браузере по умолчанию.
func openWindow(url, cfgDir string) {
	if os.Getenv("KROHOBOR_BROWSER_TAB") == "" {
		for _, bin := range appBrowsers() {
			args := []string{
				"--app=" + url,
				"--user-data-dir=" + filepath.Join(cfgDir, "window"),
				"--window-size=1400,900",
				"--no-first-run", "--no-default-browser-check",
			}
			if err := exec.Command(bin, args...).Start(); err == nil {
				return
			}
		}
	}
	_ = openURL(url)
}

func appBrowsers() []string {
	var list []string
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		for _, app := range []string{
			"Google Chrome.app/Contents/MacOS/Google Chrome",
			"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"Yandex.app/Contents/MacOS/Yandex",
			"Brave Browser.app/Contents/MacOS/Brave Browser",
			"Chromium.app/Contents/MacOS/Chromium",
		} {
			list = append(list, filepath.Join("/Applications", app), filepath.Join(home, "Applications", app))
		}
	case "windows":
		pf, pf86, local := os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")
		for _, base := range []string{pf, pf86, local} {
			if base == "" {
				continue
			}
			list = append(list,
				filepath.Join(base, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(base, `Yandex\YandexBrowser\Application\browser.exe`),
				filepath.Join(base, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			)
		}
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "yandex-browser", "brave-browser"} {
			if p, err := exec.LookPath(name); err == nil {
				list = append(list, p)
			}
		}
		return list
	}
	var found []string
	for _, p := range list {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			found = append(found, p)
		}
	}
	return found
}

// hostMode: программу запустило своё окно на Mac — адрес отдаём ему, браузер не открываем.
func hostMode() bool { return os.Getenv("KROHOBOR_HOST") != "" }
