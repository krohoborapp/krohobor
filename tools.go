package main

// Обёртки над утилитами из папки MainApp: ipatool-cpp и libimobiledevice.
// Логика повторяет IPA_Downloader.ps1 (kda2495/IPA_Downloader), но без консольных меню.

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var reANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// cleanLine убирает ANSI-коды и служебные символы терминала (например ^D от script на macOS).
func cleanLine(s string) string {
	s = reANSI.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "^D", "")
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

type Tools struct {
	BinDir           string
	Ipatool          string
	IdeviceID        string
	IdeviceInfo      string
	IdeviceInstaller string
}

func archSubFolder() string {
	switch runtime.GOOS {
	case "windows":
		return "windows_amd64"
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "macOS_arm64"
		}
		return "macOS_amd64"
	default:
		return "linux_amd64"
	}
}

func findTool(dir, prefix string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
			continue
		}
		return filepath.Join(dir, name)
	}
	return ""
}

func LoadTools(baseDir string) (*Tools, error) {
	dir := filepath.Join(baseDir, "MainApp", archSubFolder())
	t := &Tools{
		BinDir:           dir,
		Ipatool:          findTool(dir, "ipatool"),
		IdeviceID:        findTool(dir, "idevice_id"),
		IdeviceInfo:      findTool(dir, "ideviceinfo"),
		IdeviceInstaller: findTool(dir, "ideviceinstaller"),
	}
	if t.Ipatool == "" {
		return t, fmt.Errorf("не найден ipatool в %s — положи рядом с программой папку MainApp из IPA_Downloader", dir)
	}
	if runtime.GOOS != "windows" {
		if runtime.GOOS == "darwin" {
			// Снимаем карантин macOS, иначе Gatekeeper не даст запустить утилиты.
			_ = exec.Command("xattr", "-cr", dir).Run()
		}
		for _, p := range []string{t.Ipatool, t.IdeviceID, t.IdeviceInfo, t.IdeviceInstaller} {
			if p != "" {
				_ = os.Chmod(p, 0o755)
			}
		}
	}
	return t, nil
}

func (t *Tools) env() []string {
	env := os.Environ()
	sep := string(os.PathListSeparator)
	add := func(key string) {
		for i, kv := range env {
			if strings.HasPrefix(strings.ToUpper(kv), strings.ToUpper(key)+"=") {
				env[i] = kv[:len(key)+1] + t.BinDir + sep + kv[len(key)+1:]
				return
			}
		}
		env = append(env, key+"="+t.BinDir)
	}
	switch runtime.GOOS {
	case "windows":
		add("PATH")
	case "darwin":
		add("DYLD_LIBRARY_PATH")
	default:
		add("LD_LIBRARY_PATH")
	}
	return env
}

func (t *Tools) command(ctx context.Context, dir, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = t.env()
	cmd.Dir = dir
	cmd.Stdin = nil // интерактивные вопросы ipatool сразу получают EOF
	hideWindow(cmd)
	return cmd
}

// Run запускает утилиту и возвращает очищенный от ANSI вывод.
func (t *Tools) Run(ctx context.Context, dir, bin string, args ...string) (string, int, error) {
	if bin == "" {
		return "", -1, errors.New("утилита не найдена в папке MainApp")
	}
	out, err := t.command(ctx, dir, bin, args...).CombinedOutput()
	defer func() {
		o := strings.TrimSpace(reANSI.ReplaceAllString(string(out), ""))
		if o == "" {
			o = "(пусто)"
		} else {
			o = lastLines(o, 6)
		}
		logf("$ %s %s → %s", filepath.Base(bin), strings.Join(args, " "), o)
	}()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			err = nil
		} else {
			code = -1
		}
	}
	return reANSI.ReplaceAllString(string(out), ""), code, err
}

// Stream запускает утилиту и построчно отдаёт вывод (строки режутся и по \r — для прогресса).
func (t *Tools) Stream(ctx context.Context, dir, bin string, onLine func(string), args ...string) (int, error) {
	return t.StreamOpt(ctx, dir, bin, true, onLine, args...)
}

func (t *Tools) StreamOpt(ctx context.Context, dir, bin string, tty bool, onLine func(string), args ...string) (int, error) {
	return t.StreamIn(ctx, dir, bin, tty, "", onLine, args...)
}

// StreamIn — то же, но с текстом на вход утилиты (например, пароль через псевдотерминал).
func (t *Tools) StreamIn(ctx context.Context, dir, bin string, tty bool, stdin string, onLine func(string), args ...string) (int, error) {
	if bin == "" {
		return -1, errors.New("утилита не найдена в папке MainApp")
	}
	// через псевдотерминал — чтобы утилиты печатали прогресс сразу, а не в конце
	wbin, wargs := bin, args
	if tty {
		wbin, wargs = ttyWrap(bin, args)
	}
	cmd := t.command(ctx, dir, wbin, wargs...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		sc.Split(scanLinesCR)
		for sc.Scan() {
			line := strings.TrimSpace(cleanLine(sc.Text()))
			if line != "" {
				if !strings.Contains(line, "%") {
					logf("  %s", line)
				}
				onLine(line)
			}
		}
		close(done)
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	logf("$ %s %s → завершено (%v)", filepath.Base(bin), strings.Join(args, " "), err)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

func scanLinesCR(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// ---------- ipatool ----------

type Account struct {
	LoggedIn   bool   `json:"loggedIn"`
	Email      string `json:"email"`
	Name       string `json:"name"`
	Storefront string `json:"storefront"`
}

var (
	reEmail      = regexp.MustCompile(`email=(\S+)`)
	reName       = regexp.MustCompile(`(?m)email=\S+\s+name=(.*?)(?:\s+\w+=|\s*$)`)
	reStorefront = regexp.MustCompile(`(?m)email=.*?\bstorefront=(\S*)`)
)

func (t *Tools) AuthInfo(ctx context.Context) Account {
	out, _, _ := t.Run(ctx, "", t.Ipatool, "auth", "info")
	a := Account{LoggedIn: strings.Contains(out, "success=true")}
	if m := reEmail.FindStringSubmatch(out); m != nil {
		a.Email = m[1]
	}
	if m := reName.FindStringSubmatch(out); m != nil {
		a.Name = strings.Trim(strings.TrimSpace(m[1]), `"`)
	}
	if m := reStorefront.FindStringSubmatch(out); m != nil {
		a.Storefront = m[1]
	}
	return a
}

func ipatoolHome() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".ipatool")
}

type LoginResult struct {
	OK       bool   `json:"ok"`
	NeedCode bool   `json:"needCode"`
	Error    string `json:"error,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

func (t *Tools) Login(ctx context.Context, email, password, code string) LoginResult {
	addSecret(password)
	defer dropSecret(password)
	var out string
	// На macOS и Linux пароль передаём через псевдотерминал — он не виден в списке процессов.
	// На Windows псевдотерминала нет, поэтому как в IPA_Downloader: параметром -p.
	viaTTY := runtime.GOOS != "windows" && os.Getenv("KROHOBOR_NO_TTY") == ""
	if viaTTY {
		if _, err := exec.LookPath("script"); err != nil {
			viaTTY = false
		}
	}
	if viaTTY {
		args := []string{"auth", "login", "-e", email}
		if code != "" {
			args = append(args, "--auth-code", code)
		}
		var sb strings.Builder
		_, err := t.StreamIn(ctx, "", t.Ipatool, true, password+"\n", func(l string) { sb.WriteString(l + "\n") }, args...)
		out = sb.String()
		if err != nil || strings.Contains(out, "not running interactively") {
			viaTTY = false
		}
	}
	if !viaTTY {
		args := []string{"auth", "login", "-e", email, "-p", password}
		if code != "" {
			args = append(args, "--auth-code", code)
		}
		o, _, err := t.Run(ctx, "", t.Ipatool, args...)
		if err != nil {
			return LoginResult{Error: err.Error()}
		}
		out = o
	}
	// Успех проверяем не по коду выхода, а по факту: ipatool видит вошедший аккаунт.
	if acc := t.AuthInfo(ctx); acc.LoggedIn && strings.EqualFold(acc.Email, email) {
		// Маркер успешного входа — как в IPA_Downloader, чтобы обе программы видели один аккаунт.
		_ = os.MkdirAll(ipatoolHome(), 0o700)
		_ = os.WriteFile(filepath.Join(ipatoolHome(), "login"), nil, 0o600)
		_, _, _ = t.Run(ctx, "", t.Ipatool, "kbsync", "--refresh")
		return LoginResult{OK: true}
	}
	low := strings.ToLower(out)
	if code == "" && (strings.Contains(low, "two-factor") || strings.Contains(low, "auth-code") || strings.Contains(low, "2fa") || strings.Contains(low, "verification code")) {
		return LoginResult{NeedCode: true}
	}
	out = strings.ReplaceAll(out, "Enter password:", "")
	return LoginResult{Error: lastLines(out, 4)}
}

func (t *Tools) Logout(ctx context.Context) {
	_, _, _ = t.Run(ctx, "", t.Ipatool, "auth", "revoke")
	_ = os.Remove(filepath.Join(ipatoolHome(), "login"))
}

type StoreApp struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	BundleID string `json:"bundleId,omitempty"`
	Version  string `json:"version,omitempty"`
	Source   string `json:"source"`
	Kind     string `json:"kind,omitempty"`
	Date     string `json:"date,omitempty"`
}

func pick(m map[string]interface{}, keys ...string) string {
	for k, v := range m {
		for _, want := range keys {
			if strings.EqualFold(k, want) && v != nil {
				switch x := v.(type) {
				case string:
					return x
				case float64:
					return fmt.Sprintf("%.0f", x)
				default:
					return fmt.Sprint(x)
				}
			}
		}
	}
	return ""
}

func (t *Tools) Search(ctx context.Context, term string) ([]StoreApp, error) {
	term = strings.Join(strings.Fields(strings.NewReplacer(`"`, " ", `\`, " ").Replace(term)), " ")
	if term == "" {
		return nil, nil
	}
	out, exit, err := t.Run(ctx, "", t.Ipatool, "search", term, "--limit", "15", "--format", "json")
	if err != nil {
		return nil, err
	}
	start := strings.Index(out, "{")
	if exit != 0 || start < 0 {
		return nil, errors.New(lastLines(out, 3))
	}
	var res struct {
		Apps []map[string]interface{} `json:"apps"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &res); err != nil {
		return nil, err
	}
	var apps []StoreApp
	for _, m := range res.Apps {
		id := pick(m, "id", "trackId")
		if id == "" {
			continue
		}
		apps = append(apps, StoreApp{
			ID: id, Name: pick(m, "name", "trackName"), BundleID: pick(m, "bundleID", "bundleId"),
			Version: pick(m, "version"), Source: "appstore",
		})
	}
	return apps, nil
}

var reApps = regexp.MustCompile(`(?m)\bapps=(\[.*\])`)

// Purchases: mode "" — последние 90 дней, "all" — вся история, "2016" — один год.
func (t *Tools) Purchases(ctx context.Context, mode string) ([]StoreApp, error) {
	args := []string{"list-purchases"}
	switch {
	case mode == "all":
		args = append(args, "--all")
	case len(mode) == 4:
		args = append(args, "--range", mode+"-all")
	}
	out, _, err := t.Run(ctx, "", t.Ipatool, args...)
	if err != nil {
		return nil, err
	}
	m := reApps.FindStringSubmatch(out)
	if m == nil {
		return nil, errors.New(lastLines(out, 3))
	}
	var raw []map[string]interface{}
	if err := json.Unmarshal([]byte(m[1]), &raw); err != nil {
		return nil, err
	}
	var apps []StoreApp
	for i := len(raw) - 1; i >= 0; i-- { // свежие сверху, как в IPA_Downloader
		r := raw[i]
		kind := strings.TrimSpace(pick(r, "kind"))
		if kind != "iOS App" && kind != "Mac App" {
			continue
		}
		id := pick(r, "id")
		if id == "" {
			continue
		}
		apps = append(apps, StoreApp{ID: id, Name: pick(r, "name"), Kind: kind, Date: shortDate(pick(r, "purchaseDate")), Source: "purchase"})
	}
	return apps, nil
}

var reQuotedNum = regexp.MustCompile(`"(\d+)"`)

func (t *Tools) Versions(ctx context.Context, id string) ([]string, error) {
	out, _, err := t.Run(ctx, "", t.Ipatool, "list-versions", "-i", id, "--purchase")
	if err != nil {
		return nil, err
	}
	if strings.Contains(out, "Error:") {
		return nil, errors.New(lastLines(out, 3))
	}
	var ids []string
	for _, m := range reQuotedNum.FindAllStringSubmatch(out, -1) {
		ids = append(ids, m[1])
	}
	if len(ids) == 0 {
		return nil, errors.New("версии не найдены")
	}
	return ids, nil
}

type VersionMeta struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	ReleaseDate string `json:"releaseDate"`
	MinOS       string `json:"minOS"`
}

func field(text, key string) string {
	m := regexp.MustCompile(key + `=([^\s,]+)`).FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.Trim(m[1], `"`)
}

func (t *Tools) VersionMeta(ctx context.Context, id, ver string) (VersionMeta, error) {
	out, _, err := t.Run(ctx, "", t.Ipatool, "get-version-metadata", "-i", id, "--external-version-id", ver)
	if err != nil {
		return VersionMeta{}, err
	}
	vm := VersionMeta{ID: ver, Version: field(out, "displayVersion"), ReleaseDate: shortDate(field(out, "releaseDate")), MinOS: field(out, "minimumOSVersion")}
	if vm.Version == "" {
		return vm, errors.New(lastLines(out, 2))
	}
	return vm, nil
}

// ---------- устройства ----------

type Device struct {
	UDID    string `json:"udid"`
	Name    string `json:"name"`
	IOS     string `json:"ios"`
	Network bool   `json:"network"`
	WiFi    bool   `json:"wifi,omitempty"` // подключён кабелем, но виден и по Wi‑Fi (синхронизация включена)
	Trusted bool   `json:"trusted"` // false — iPhone виден, но не нажато «Доверять»
	Free    int64  `json:"free,omitempty"`
	Total   int64  `json:"total,omitempty"`
}

var reTrust = regexp.MustCompile(`(?i)pair|trust|lockdown|password protected|invalid host|user denied`)

var diskCache = struct {
	sync.Mutex
	m map[string][3]int64 // free, total, время
}{m: map[string][3]int64{}}

var reUDID = regexp.MustCompile(`^[0-9A-Fa-f-]{20,}$`)

func (t *Tools) Devices(ctx context.Context) ([]Device, error) {
	if t.IdeviceID == "" {
		return nil, errors.New("нет утилит libimobiledevice в папке MainApp")
	}
	var devs []Device
	seen := map[string]bool{}
	for _, network := range []bool{false, true} {
		flag := "-l"
		if network {
			flag = "-n"
		}
		out, _, _ := t.Run(ctx, "", t.IdeviceID, flag)
		for _, line := range strings.Split(out, "\n") {
			udid := strings.Fields(line + " ")
			if len(udid) == 0 || !reUDID.MatchString(udid[0]) {
				continue
			}
			if seen[udid[0]] {
				if network {
					for i := range devs {
						if devs[i].UDID == udid[0] {
							devs[i].WiFi = true
						}
					}
				}
				continue
			}
			u := udid[0]
			seen[u] = true
			d := Device{UDID: u, Network: network, Name: "iPhone", IOS: "?", Trusted: true}
			args := []string{"-u", u}
			if network {
				args = append([]string{"-n"}, args...)
			}
			name, _, _ := t.Run(ctx, "", t.IdeviceInfo, append(args, "-k", "DeviceName")...)
			if reTrust.MatchString(name) || strings.Contains(name, "ERROR") {
				// телефон виден, но связь не разрешена — эта же попытка показывает на iPhone «Доверять?»
				d.Trusted = false
				devs = append(devs, d)
				continue
			}
			if strings.TrimSpace(name) != "" {
				d.Name = strings.TrimSpace(name)
			}
			d.Free, d.Total = t.diskUsage(ctx, args, u)
			if v, _, _ := t.Run(ctx, "", t.IdeviceInfo, append(args, "-k", "ProductVersion")...); regexp.MustCompile(`^\d+(\.\d+)+$`).MatchString(strings.TrimSpace(v)) {
				d.IOS = strings.TrimSpace(v)
			}
			devs = append(devs, d)
		}
	}
	return devs, nil
}

func (t *Tools) diskUsage(ctx context.Context, args []string, udid string) (int64, int64) {
	diskCache.Lock()
	v, ok := diskCache.m[udid]
	diskCache.Unlock()
	if ok && time.Now().Unix()-v[2] < 60 {
		return v[0], v[1]
	}
	out, _, _ := t.Run(ctx, "", t.IdeviceInfo, append(args, "-q", "com.apple.disk_usage")...)
	get := func(keys ...string) int64 {
		for _, k := range keys {
			if m := regexp.MustCompile(`(?m)^` + k + `:\s*(\d+)`).FindStringSubmatch(out); m != nil {
				n, _ := strconv.ParseInt(m[1], 10, 64)
				return n
			}
		}
		return 0
	}
	free, total := get("AmountDataAvailable", "TotalDataAvailable"), get("TotalDiskCapacity", "TotalDataCapacity")
	diskCache.Lock()
	diskCache.m[udid] = [3]int64{free, total, time.Now().Unix()}
	diskCache.Unlock()
	return free, total
}

// DevApp — приложение, установленное на iPhone.
type DevApp struct {
	BundleID string `json:"bundleId"`
	Version  string `json:"version"`
	Name     string `json:"name"`
	ItemID   string `json:"itemId,omitempty"` // ID из App Store (из iTunesMetadata)
}

// DeviceApps — пользовательские приложения на iPhone (читается локально, никуда не отправляется).
func (t *Tools) DeviceApps(ctx context.Context, d Device) ([]DevApp, error) {
	base := []string{}
	if d.Network {
		base = append(base, "-n")
	}
	base = append(base, "--udid", d.UDID, "list", "--user", "--xml",
		"-a", "CFBundleIdentifier", "-a", "CFBundleShortVersionString", "-a", "CFBundleDisplayName", "-a", "CFBundleName", "-a", "iTunesMetadata")
	out, _, err := t.Run(ctx, "", t.IdeviceInstaller, base...)
	if err != nil {
		return nil, err
	}
	x := extractXML(out)
	if x == nil {
		return nil, errors.New(lastLines(out, 2))
	}
	v, err := parsePlist(x)
	if err != nil {
		return nil, err
	}
	arr, _ := v.([]interface{})
	var apps []DevApp
	for _, it := range arr {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		a := DevApp{BundleID: plistString(m, "CFBundleIdentifier"), Version: plistString(m, "CFBundleShortVersionString"),
			Name: firstNonEmpty(plistString(m, "CFBundleDisplayName"), plistString(m, "CFBundleName"))}
		if meta, ok := m["iTunesMetadata"].([]byte); ok && len(meta) > 0 {
			if mv, err := parsePlist(meta); err == nil {
				if mm, ok := mv.(map[string]interface{}); ok {
					a.ItemID = plistString(mm, "itemId")
				}
			}
		}
		apps = append(apps, a)
	}
	return apps, nil
}

// checkIPA — перед установкой убеждаемся, что файл целый и подписан Apple (есть SC_Info/*.sinf).
func checkIPA(path string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return errors.New("файл повреждён или это не IPA — скачай его заново")
	}
	defer zr.Close()
	hasPlist, hasSinf := false, false
	for _, f := range zr.File {
		if reInfoPlist.MatchString(f.Name) {
			hasPlist = true
		}
		if strings.Contains(f.Name, ".app/SC_Info/") && strings.HasSuffix(f.Name, ".sinf") {
			hasSinf = true
		}
	}
	if !hasPlist {
		return errors.New("в файле нет приложения (Info.plist) — скачай его заново")
	}
	if !hasSinf {
		return errors.New("это IPA не из App Store: в нём нет подписи Apple. Обычный iPhone такой файл не примет — его нужно переподписать своим сертификатом. Программа ставит IPA, скачанные из App Store на любой Apple ID")
	}
	return nil
}

func (t *Tools) Install(ctx context.Context, d Device, ipa string, c *JobCtl) error {
	if err := checkIPA(ipa); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
		state := driverState(dctx)
		dcancel()
		if state != "ok" {
			return errors.New("на компьютере нет драйвера Apple, без него Windows не видит iPhone. Установи его: на Главной кнопка «Установить драйвер Apple»")
		}
	}
	if !d.Trusted {
		return errors.New("iPhone не доверяет компьютеру: разблокируй его и нажми «Доверять»")
	}
	if st, err := os.Stat(ipa); err == nil && d.Free > 0 && d.Free < st.Size()*2 {
		return fmt.Errorf("на %s мало места: свободно %s, а приложению нужно около %s. Освободи место и попробуй снова", d.Name, mb(d.Free), mb(st.Size()*2))
	}
	// на один iPhone ставим по одному приложению: параллельные установки мешают друг другу
	lk := deviceLock(d.UDID)
	if !lk.TryLock() {
		c.Progress(-1, "Жду, пока закончится предыдущая установка на "+d.Name+"…")
		lk.Lock()
	}
	defer lk.Unlock()
	c.SetPhase("install")
	via := ""
	if d.Network {
		via = " по Wi‑Fi"
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("krohobor_%d.ipa", time.Now().UnixNano()))
	if err := copyFile(ipa, tmp); err != nil {
		return err
	}
	defer os.Remove(tmp)
	base := []string{}
	if d.Network {
		base = append(base, "-n")
	}
	base = append(base, "--udid", d.UDID)

	// Пока файл копируется на iPhone, утилита молчит — показываем таймер.
	start := time.Now()
	size := int64(0)
	if st, err := os.Stat(ipa); err == nil {
		size = st.Size()
	}
	tickCtx, stopTick := context.WithCancel(ctx)
	defer stopTick()
	go func() {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-tickCtx.Done():
				return
			case <-tk.C:
				if !c.toolActive() {
					c.Progress(-1, "Отправляю на iPhone"+via+" "+mb(size)+" · "+elapsed(start))
				}
			}
		}
	}()
	c.Progress(-1, "Отправляю на iPhone"+via+" "+mb(size))
	onLine := func(l string) {
		if !c.installLine(l) {
			c.Log(l)
		}
	}
	code, err := t.Stream(ctx, "", t.IdeviceInstaller, onLine, append(base, "install", tmp)...)
	if err == nil && code == 0 {
		return nil
	}
	c.Log("Пробую обновить поверх установленной версии…")
	start = time.Now()
	code, err = t.Stream(ctx, "", t.IdeviceInstaller, onLine, append(base, "upgrade", tmp)...)
	if err != nil {
		return err
	}
	if code != 0 {
		return errors.New("установка не удалась — проверь, что iPhone разблокирован и компьютеру нажато «Доверять»")
	}
	return nil
}

// ---------- helpers ----------

func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 0 {
		return "неизвестная ошибка"
	}
	return strings.Join(lines, "\n")
}

func shortDate(s string) string {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05", "2006-01-02", "2 Jan 2006", "02 Jan 2006", "Jan 2, 2006", "January 2, 2006", "2 January 2006", "02.01.2006"} {
		if tm, err := time.Parse(layout, s); err == nil {
			return tm.Format("02.01.2006")
		}
	}
	return s
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

var devLocks sync.Map

func deviceLock(udid string) *sync.Mutex {
	m, _ := devLocks.LoadOrStore(udid, &sync.Mutex{})
	return m.(*sync.Mutex)
}
