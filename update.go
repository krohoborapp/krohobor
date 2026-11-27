package main

// Проверка обновлений через релизы GitHub и обновление в один клик.
// Ожидаемые файлы релиза: DimaKrohobor-<версия>.dmg (внутри «Дима Крохобор.app»),
// DimaKrohobor-<версия>-windows.zip и DimaKrohobor-<версия>-linux.zip (внутри папка с программой).

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Репозиторий для обновлений и каталога. Временный адрес — заменить на настоящий.
const UpdateRepo = "krohoborapp/krohobor"

type Release struct {
	Version string `json:"version"`
	Notes   string `json:"notes"`
	URL     string `json:"url"`
	Asset   string `json:"-"`
}

var updMu sync.Mutex
var lastRelease *Release

func releaseAPI() string {
	if u := os.Getenv("KROHOBOR_UPDATE_URL"); u != "" { // для проверки на тестовом сервере
		return u
	}
	return "https://api.github.com/repos/" + UpdateRepo + "/releases/latest"
}

func parseVer(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(strings.TrimFunc(p, func(r rune) bool { return r < '0' || r > '9' }))
		out = append(out, n)
	}
	return out
}

func newer(a, b string) bool { // a > b
	pa, pb := parseVer(a), parseVer(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func assetSuffix() string {
	switch runtime.GOOS {
	case "darwin":
		return ".dmg" // на Mac обновляемся прямо из DMG — того же файла, что качают люди
	case "windows":
		return "-windows.zip"
	}
	return "-linux.zip"
}

func checkUpdate(ctx context.Context) *Release {
	req, err := http.NewRequestWithContext(ctx, "GET", releaseAPI(), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		// у сервиса GitHub лимит 60 запросов в час с одного адреса: много людей за одним провайдером
		// могут в него упереться. Тогда узнаём версию по адресу «последний релиз» — у него лимита нет.
		return checkUpdateByRedirect(ctx)
	}
	var gh struct {
		Tag    string `json:"tag_name"`
		Body   string `json:"body"`
		URL    string `json:"html_url"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if json.NewDecoder(resp.Body).Decode(&gh) != nil || !newer(gh.Tag, AppVersion) {
		return nil
	}
	r := &Release{Version: strings.TrimPrefix(gh.Tag, "v"), Notes: gh.Body, URL: gh.URL}
	for _, a := range gh.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), assetSuffix()) {
			r.Asset = a.URL
		}
	}
	updMu.Lock()
	lastRelease = r
	updMu.Unlock()
	logf("доступно обновление %s", r.Version)
	return r
}

// installRoot — что заменяем: .app на маке или папку с программой на Windows/Linux.
func installRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if i := strings.Index(exe, ".app/Contents/MacOS"); i >= 0 {
		return exe[:i+4], nil
	}
	return filepath.Dir(exe), nil
}

func applyUpdate(c *JobCtl) (string, error) {
	updMu.Lock()
	r := lastRelease
	updMu.Unlock()
	if r == nil || r.Asset == "" {
		return "", errors.New("файл обновления для твоей системы не найден — скачай новую версию вручную со страницы релиза")
	}
	root, err := installRoot()
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "dimakrohobor-update-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	zipPath := filepath.Join(tmp, "update.zip")
	c.Progress(-1, "Скачиваю обновление…")
	if err := downloadTo(r.Asset, zipPath, c); err != nil {
		return "", fmt.Errorf("не скачалось обновление: %v", err)
	}
	c.Progress(-1, "Распаковываю…")
	ex := filepath.Join(tmp, "x")
	if runtime.GOOS == "darwin" {
		if err := extractDMG(zipPath, ex); err != nil {
			return "", err
		}
	} else if err := unzip(zipPath, ex); err != nil {
		return "", err
	}
	var newExe string
	if runtime.GOOS == "darwin" {
		apps, _ := filepath.Glob(filepath.Join(ex, "*.app"))
		if len(apps) == 0 {
			return "", errors.New("в архиве обновления нет приложения")
		}
		old := root + ".old"
		_ = os.RemoveAll(old)
		if err := os.Rename(root, old); err != nil {
			return "", fmt.Errorf("нет прав заменить программу: %v", err)
		}
		if err := os.Rename(apps[0], root); err != nil {
			_ = os.Rename(old, root)
			return "", err
		}
		_ = os.RemoveAll(old)
		_ = exec.Command("xattr", "-cr", root).Run()
		newExe = root
	} else {
		src := ex
		if entries, _ := os.ReadDir(ex); len(entries) == 1 && entries[0].IsDir() {
			src = filepath.Join(ex, entries[0].Name())
		}
		self, _ := os.Executable()
		// работающий файл нельзя перезаписать на Windows, но можно переименовать
		_ = os.Remove(self + ".old")
		if err := os.Rename(self, self+".old"); err != nil {
			return "", err
		}
		if err := copyTree(src, root); err != nil {
			_ = os.Rename(self+".old", self)
			return "", err
		}
		newExe = self
		_ = os.Chmod(newExe, 0o755)
	}
	logf("обновлено до %s", r.Version)
	go func() {
		time.Sleep(800 * time.Millisecond)
		releaseInstance()
		if runtime.GOOS == "darwin" {
			_ = exec.Command("open", "-n", newExe).Start()
		} else {
			cmd := exec.Command(newExe)
			cmd.Dir = filepath.Dir(newExe)
			_ = cmd.Start()
		}
		os.Exit(0)
	}()
	return "Обновлено до версии " + r.Version + ". Программа перезапускается, страница откроется заново.", nil
}

func downloadTo(u, dst string, c *JobCtl) error {
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
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
				c.Progress(float64(done)/float64(total)*100, fmt.Sprintf("Скачиваю обновление: %s из %s", mb(done), mb(total)))
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

func unzip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		p := filepath.Join(dst, f.Name)
		if !strings.HasPrefix(p, filepath.Clean(dst)+string(os.PathSeparator)) {
			return errors.New("подозрительный архив обновления")
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(p, 0o755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if f.Mode()&os.ModeSymlink != 0 {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			target, _ := io.ReadAll(rc)
			rc.Close()
			_ = os.Symlink(string(target), p)
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o200)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(t, 0o755)
		}
		_ = os.Remove(t)
		if err := copyFile(p, t); err != nil {
			return err
		}
		return os.Chmod(t, info.Mode().Perm()|0o200)
	})
}

func checkUpdateByRedirect(ctx context.Context) *Release {
	if os.Getenv("KROHOBOR_UPDATE_URL") != "" {
		return nil
	}
	page := "https://github.com/" + UpdateRepo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, "HEAD", page, nil)
	if err != nil {
		return nil
	}
	c := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return nil
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location") // …/releases/tag/v1.4.2
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return nil
	}
	tag := loc[i+5:]
	if !newer(tag, AppVersion) {
		return nil
	}
	ver := strings.TrimPrefix(tag, "v")
	name := "DimaKrohobor-" + ver + assetSuffix()
	r := &Release{Version: ver, URL: "https://github.com/" + UpdateRepo + "/releases/tag/" + tag,
		Asset: "https://github.com/" + UpdateRepo + "/releases/download/" + tag + "/" + name}
	updMu.Lock()
	lastRelease = r
	updMu.Unlock()
	logf("доступно обновление %s", r.Version)
	return r
}

// extractDMG подключает DMG без показа в Finder, копирует из него программу в dst и отключает.
func extractDMG(dmg, dst string) error {
	mnt := dst + "-mnt"
	_ = os.MkdirAll(mnt, 0o755)
	_ = os.MkdirAll(dst, 0o755)
	if out, err := exec.Command("hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", mnt, dmg).CombinedOutput(); err != nil {
		return fmt.Errorf("не открылся файл обновления: %s", strings.TrimSpace(string(out)))
	}
	defer exec.Command("hdiutil", "detach", "-force", mnt).Run()
	apps, _ := filepath.Glob(filepath.Join(mnt, "*.app"))
	if len(apps) == 0 {
		return errors.New("в файле обновления нет программы")
	}
	if out, err := exec.Command("ditto", apps[0], filepath.Join(dst, filepath.Base(apps[0]))).CombinedOutput(); err != nil {
		return fmt.Errorf("не скопировалась новая версия: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
