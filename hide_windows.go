//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// На Windows не показываем чёрные окна консоли для каждой утилиты.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

func openURL(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
