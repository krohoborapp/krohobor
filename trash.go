package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// moveToTrash убирает файл в Корзину системы, а не стирает насовсем:
// если человек удалил не то, файл можно вернуть.
func moveToTrash(path string) error {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		dir := filepath.Join(home, ".Trash")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return renameOrCopy(path, uniquePath(filepath.Join(dir, filepath.Base(path))))
	case "windows":
		// путь передаём через переменную окружения, чтобы не возиться с кавычками
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"Add-Type -AssemblyName Microsoft.VisualBasic; [Microsoft.VisualBasic.FileIO.FileSystem]::DeleteFile($env:DK_TRASH_FILE, 'OnlyErrorDialogs', 'SendToRecycleBin')")
		cmd.Env = append(os.Environ(), "DK_TRASH_FILE="+path)
		hideWindow(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("файл остался на месте")
		}
		return nil
	default: // freedesktop: ~/.local/share/Trash
		base := filepath.Join(home, ".local", "share", "Trash")
		files, info := filepath.Join(base, "files"), filepath.Join(base, "info")
		if err := os.MkdirAll(files, 0o700); err != nil {
			return err
		}
		_ = os.MkdirAll(info, 0o700)
		dst := uniquePath(filepath.Join(files, filepath.Base(path)))
		if err := renameOrCopy(path, dst); err != nil {
			return err
		}
		meta := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n", (&url.URL{Path: path}).EscapedPath(), time.Now().Format("2006-01-02T15:04:05"))
		_ = os.WriteFile(filepath.Join(info, filepath.Base(dst)+".trashinfo"), []byte(meta), 0o600)
		return nil
	}
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); err != nil {
		return p
	}
	ext := filepath.Ext(p)
	return strings.TrimSuffix(p, ext) + " " + time.Now().Format("150405") + ext
}

// renameOrCopy: Корзина может быть на другом диске — тогда копируем и удаляем оригинал.
func renameOrCopy(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// revealPath открывает папку с файлом и выделяет его.
func revealPath(p string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", "/select,", p)
	case "darwin":
		cmd = exec.Command("open", "-R", p)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(p))
	}
	_ = cmd.Start()
}
