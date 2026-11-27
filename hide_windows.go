//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// На Windows не показываем чёрные окна консоли для каждой утилиты.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

func openURL(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// Запуск прямо из zip: Windows распаковывает во временную папку только сам exe,
// без папок MainApp и Files — программа не заработает. Просим сначала распаковать архив.
func startupChecks() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	low := strings.ToLower(exe)
	tmp := strings.ToLower(os.TempDir())
	inZip := (strings.HasPrefix(low, tmp) && strings.Contains(low, ".zip")) || strings.Contains(low, `\temp\7zo`) || strings.Contains(low, `\temp\rar$`)
	if !inZip {
		if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "MainApp")); err == nil {
			return
		}
	}
	msgBox("Дима Крохобор", "Сначала распакуй архив.\n\nНажми на скачанный архив правой кнопкой мыши → «Извлечь всё…» → «Извлечь». Потом открой распакованную папку и запусти «Дима Крохобор.exe» оттуда.\n\nИз архива напрямую программа не работает.")
	os.Exit(1)
}

func msgBox(title, text string) {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(text)
	syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x30)
}
