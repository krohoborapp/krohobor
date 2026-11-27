package main

// Драйвер для работы с iPhone.
// Windows: служба Apple Mobile Device Service (ставится с iTunes / «Устройства Apple»).
// Если её нет — скачиваем официальный установщик iTunes с сайта Apple, проверяем подпись Apple,
// достаём из него только драйвер (AppleMobileDeviceSupport64.msi) и запускаем установку.
// macOS: служба встроена в систему. Linux: нужен usbmuxd.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const appleITunesURL = "https://www.apple.com/itunes/download/win64"

func (s *Server) driverCheck(ctx context.Context) Check {
	g := "Драйвер iPhone"
	switch runtime.GOOS {
	case "windows":
		state := winServiceState(ctx, "Apple Mobile Device Service")
		switch state {
		case "RUNNING":
			return Check{Group: g, Name: "Драйвер Apple Mobile Device", OK: true, Info: "Служба Apple Mobile Device Service работает"}
		case "":
			return Check{Group: g, Name: "Драйвер Apple Mobile Device", Info: "Не установлен",
				Hint:   "Без него компьютер не видит iPhone. Нажми кнопку — программа скачает официальный драйвер с сайта Apple и установит только его, без iTunes.",
				Action: "install-driver"}
		default:
			return Check{Group: g, Name: "Драйвер Apple Mobile Device", Info: "Служба установлена, но не запущена (" + strings.ToLower(state) + ")",
				Hint:   "Перезагрузи компьютер. Если не поможет — переустанови драйвер кнопкой.",
				Action: "install-driver"}
		}
	case "darwin":
		return Check{Group: g, Name: "Служба usbmuxd", OK: true, Info: "Встроена в macOS"}
	default:
		if _, err := os.Stat("/var/run/usbmuxd"); err == nil {
			return Check{Group: g, Name: "Служба usbmuxd", OK: true, Info: "Работает"}
		}
		return Check{Group: g, Name: "Служба usbmuxd", Info: "Не запущена", Hint: "Установи и запусти: sudo apt install usbmuxd (или аналог для твоего дистрибутива)."}
	}
}

// driverState для мастера и плашки: ok / missing / stopped.
func driverState(ctx context.Context) string {
	// Служба называется по-разному: iTunes ставит «Apple Mobile Device Service»,
	// приложение «Устройства Apple» из Microsoft Store — «AppleMobileDeviceService».
	best := ""
	for _, name := range []string{"Apple Mobile Device Service", "AppleMobileDeviceService"} {
		switch winServiceState(ctx, name) {
		case "RUNNING":
			return "ok"
		case "":
		default:
			best = "stopped"
		}
	}
	if best == "" {
		// запасной способ: служба usbmuxd от Apple слушает порт 27015
		if c, err := net.DialTimeout("tcp", "127.0.0.1:27015", 700*time.Millisecond); err == nil {
			c.Close()
			return "ok"
		}
		return "missing"
	}
	return best
}

// winServiceState: RUNNING / STOPPED / … или "" если службы нет.
// Вывод sc на русской Windows переведён («СОСТОЯНИЕ»), поэтому ищем сами коды состояний.
func winServiceState(ctx context.Context, name string) string {
	cmd := exec.CommandContext(ctx, "sc", "query", name)
	hideWindow(cmd)
	out, _ := cmd.Output()
	text := strings.ToUpper(string(out))
	for _, st := range []string{"RUNNING", "START_PENDING", "STOP_PENDING", "STOPPED", "PAUSED"} {
		if strings.Contains(text, " "+st) {
			if st == "START_PENDING" {
				return "RUNNING"
			}
			return st
		}
	}
	return ""
}

func installDriver(c *JobCtl) (string, error) {
	if runtime.GOOS != "windows" {
		return "", errors.New("установка драйвера нужна только на Windows")
	}
	tmp, err := os.MkdirTemp("", "dimakrohobor-driver-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	setup := filepath.Join(tmp, "iTunes64Setup.exe")

	// 1. Скачиваем официальный установщик только с серверов Apple
	c.Progress(-1, "Скачиваю драйвер с сайта Apple…")
	client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !strings.HasSuffix(req.URL.Hostname(), "apple.com") {
			return fmt.Errorf("перенаправление на чужой сайт %s — отменено", req.URL.Hostname())
		}
		return nil
	}}
	resp, err := client.Get(appleITunesURL)
	if err != nil {
		return "", fmt.Errorf("не получилось скачать с сайта Apple: %v", err)
	}
	if resp.StatusCode != 200 || !strings.HasSuffix(resp.Request.URL.Hostname(), "apple.com") {
		resp.Body.Close()
		return "", fmt.Errorf("сайт Apple ответил %d", resp.StatusCode)
	}
	if err := saveBody(resp, setup, c); err != nil {
		return "", err
	}
	c.Log("Скачано с " + resp.Request.URL.Hostname())

	// 2. Проверяем цифровую подпись Apple
	c.Progress(-1, "Проверяю подпись Apple…")
	ps := exec.Command("powershell", "-NoProfile", "-Command",
		"$s=Get-AuthenticodeSignature -LiteralPath '"+setup+"'; \"$($s.Status)|$($s.SignerCertificate.Subject)\"")
	hideWindow(ps)
	sig, _ := ps.Output()
	sigText := strings.TrimSpace(string(sig))
	c.Log("Подпись: " + sigText)
	if !strings.HasPrefix(sigText, "Valid|") || !strings.Contains(sigText, "Apple Inc.") {
		return "", errors.New("у скачанного файла нет действительной подписи Apple — установка отменена")
	}

	// 3. Достаём только драйвер
	c.Progress(-1, "Распаковываю драйвер…")
	ext := exec.Command(setup, "/extract")
	ext.Dir = tmp
	hideWindow(ext)
	_ = ext.Run()
	msi := ""
	_ = filepath.Walk(tmp, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.EqualFold(info.Name(), "AppleMobileDeviceSupport64.msi") {
			msi = p
		}
		return nil
	})

	// 4. Устанавливаем (Windows сама спросит разрешение администратора)
	c.Progress(-1, "Устанавливаю драйвер… Подтверди запрос Windows, если он появится")
	var inst *exec.Cmd
	if msi != "" {
		inst = exec.Command("msiexec", "/i", msi, "/passive", "/norestart")
	} else {
		c.Log("Отдельный драйвер достать не вышло — запускаю полный установщик iTunes")
		inst = exec.Command(setup)
	}
	if err := inst.Run(); err != nil {
		var ee *exec.ExitError
		if !(errors.As(err, &ee) && ee.ExitCode() == 3010) { // 3010 — нужна перезагрузка, это успех
			return "", fmt.Errorf("установка не завершилась (%v). Если отказался от запроса администратора — нажми кнопку ещё раз и подтверди", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if winServiceState(ctx, "Apple Mobile Device Service") == "RUNNING" {
		return "Драйвер установлен и работает. Переподключи iPhone.", nil
	}
	return "Драйвер установлен. Перезагрузи компьютер, если iPhone всё ещё не виден.", nil
}

func saveBody(resp *http.Response, dst string, c *JobCtl) error {
	defer resp.Body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 256*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			done += int64(n)
			if total > 0 {
				c.Progress(float64(done)/float64(total)*100, fmt.Sprintf("Скачиваю драйвер с сайта Apple: %s из %s", mb(done), mb(total)))
			} else {
				c.Progress(-1, "Скачиваю драйвер с сайта Apple: "+mb(done))
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}
