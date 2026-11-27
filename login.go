package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Вход в Apple ID одним заходом (Mac и Linux).
//
// Раньше программа входила дважды: «почта + пароль» → Apple присылает код → вход заново уже с кодом.
// Второй вход Apple считает новой попыткой и присылает новый код, а введённый человеком перестаёт
// действовать. Поэтому ipatool запускается в псевдотерминале как в Терминале: он спрашивает пароль,
// затем в том же процессе спрашивает код и ждёт — программа держит этот вход открытым,
// пока человек вводит код, и передаёт код туда же.

type loginSession struct {
	email string
	stdin io.WriteCloser
	mu    sync.Mutex
	out   strings.Builder
	done  chan struct{}
	kill  func()
}

func (l *loginSession) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cleanLine(l.out.String())
}

func (l *loginSession) exited() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

// waitFor ждёт, пока в выводе после позиции from появится одна из строк, или пока процесс не завершится.
func (l *loginSession) waitFor(d time.Duration, from int, subs ...string) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		t := strings.ToLower(l.text())
		if from < len(t) {
			for _, s := range subs {
				if strings.Contains(t[from:], s) {
					return true
				}
			}
		}
		if l.exited() {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

func interactiveLoginSupported() bool {
	if runtime.GOOS == "windows" || os.Getenv("KROHOBOR_NO_TTY") != "" {
		return false
	}
	w, _ := ttyWrap("x", nil)
	return w != "x"
}

// LoginStart начинает вход. Если Apple просит код — возвращает NeedCode и открытую сессию.
func (t *Tools) LoginStart(email, password string) (*loginSession, LoginResult) {
	addSecret(password) // до конца входа: терминал может повторить пароль в выводе
	go func() { time.Sleep(7 * time.Minute); dropSecret(password) }()
	wbin, wargs := ttyWrap(t.Ipatool, []string{"auth", "login", "-e", email})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute) // на ввод кода — с запасом
	cmd := t.command(ctx, "", wbin, wargs...)
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, LoginResult{Error: err.Error()}
	}
	l := &loginSession{email: email, stdin: in, done: make(chan struct{}), kill: cancel}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, LoginResult{Error: err.Error()}
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				l.mu.Lock()
				l.out.Write(buf[:n])
				l.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { _ = cmd.Wait(); pw.Close(); close(l.done); cancel() }()

	hide := func(res LoginResult) LoginResult { // на случай, если терминал отобразил введённый пароль
		res.Error = strings.ReplaceAll(res.Error, password, "•••")
		return res
	}
	if !l.waitFor(30*time.Second, 0, "password") {
		l.kill()
		<-l.done
		return nil, hide(t.loginOutcome(email, l.text(), false))
	}
	_, _ = io.WriteString(in, password+"\n")
	mark := len(l.text())
	if l.waitFor(90*time.Second, mark, "2fa code", "auth code", "two-factor") && !l.exited() {
		return l, LoginResult{NeedCode: true}
	}
	<-l.done
	return nil, hide(t.loginOutcome(email, l.text(), false))
}

// LoginCode передаёт код в открытый вход и ждёт результата.
func (t *Tools) LoginCode(l *loginSession, code string) LoginResult {
	if l.exited() {
		return LoginResult{Error: "Вход прервался — Apple ждёт код только несколько минут. Нажми «Войти заново»."}
	}
	mark := len(l.text())
	_, _ = io.WriteString(l.stdin, code+"\n")
	select {
	case <-l.done:
	case <-time.After(2 * time.Minute):
		l.kill()
		<-l.done
	}
	out := l.text()
	if mark < len(out) {
		out = out[mark:]
	}
	out = strings.ReplaceAll(out, code, "") // терминал повторяет введённый код
	return t.loginOutcome(l.email, out, true)
}

// loginOutcome проверяет вход по факту: ipatool видит вошедший аккаунт.
func (t *Tools) loginOutcome(email, out string, withCode bool) LoginResult {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if acc := t.AuthInfo(ctx); acc.LoggedIn && strings.EqualFold(acc.Email, email) {
		_ = os.MkdirAll(ipatoolHome(), 0o700)
		_ = os.WriteFile(filepath.Join(ipatoolHome(), "login"), nil, 0o600)
		_, _, _ = t.Run(ctx, "", t.Ipatool, "kbsync", "--refresh")
		return LoginResult{OK: true}
	}
	for _, p := range []string{"Enter password:", "Enter 2FA code:"} {
		out = strings.ReplaceAll(out, p, "")
	}
	msg := lastLines(strings.TrimSpace(out), 4)
	if msg == "" {
		msg = "Bad login"
	}
	return LoginResult{Error: msg}
}
