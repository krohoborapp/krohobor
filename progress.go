package main

// Прогресс загрузки и установки.
// ipatool-cpp печатает проценты только в настоящий терминал, а ideviceinstaller
// буферизует вывод в трубу, поэтому на macOS/Linux запускаем их через `script`
// (псевдотерминал). Плюс сами считаем скачанные байты — это работает везде, и на Windows.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type JobCtl struct {
	jobs *Jobs
	job  *Job
	mu   sync.Mutex
	last time.Time // когда последний раз пришёл процент от самой утилиты
}

// Queue/Run — задача ждёт своей очереди (одновременно качаются не больше двух приложений).
func (c *JobCtl) Queue(detail string) {
	c.jobs.mu.Lock()
	c.job.Status, c.job.Detail, c.job.Progress = "queued", detail, -1
	c.jobs.mu.Unlock()
}

func (c *JobCtl) Run() {
	c.jobs.mu.Lock()
	c.job.Status = "running"
	c.jobs.mu.Unlock()
}

func (c *JobCtl) Log(s string) {
	c.jobs.mu.Lock()
	defer c.jobs.mu.Unlock()
	c.job.Log = append(c.job.Log, s)
	if len(c.job.Log) > 300 {
		c.job.Log = c.job.Log[len(c.job.Log)-300:]
	}
}

func (c *JobCtl) lastLog() string {
	c.jobs.mu.Lock()
	defer c.jobs.mu.Unlock()
	for i := len(c.job.Log) - 1; i >= 0; i-- {
		l := c.job.Log[i]
		if strings.Contains(l, "Пробую ещё раз") {
			continue
		}
		return l
	}
	return "приложение не покупалось на этом Apple ID, нет лицензии или проблемы с сетью"
}

// Progress: p от 0 до 100, или -1 — «идёт, но сколько осталось неизвестно».
func (c *JobCtl) Progress(p float64, detail string) {
	c.jobs.mu.Lock()
	defer c.jobs.mu.Unlock()
	if p > 100 {
		p = 100
	}
	c.job.Progress = p
	c.job.Detail = detail
}

func (c *JobCtl) fromTool() {
	c.mu.Lock()
	c.last = time.Now()
	c.mu.Unlock()
}

func (c *JobCtl) toolActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.last) < 3*time.Second
}

// ---------- псевдотерминал ----------

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ttyWrap оборачивает команду в `script`, чтобы утилита думала, что пишет в терминал.
func ttyWrap(bin string, args []string) (string, []string) {
	if os.Getenv("KROHOBOR_NO_TTY") != "" {
		return bin, args
	}
	script, err := exec.LookPath("script")
	if err != nil {
		return bin, args
	}
	switch runtime.GOOS {
	case "darwin":
		return script, append([]string{"-q", "/dev/null", bin}, args...)
	case "linux":
		parts := []string{shellQuote(bin)}
		for _, a := range args {
			parts = append(parts, shellQuote(a))
		}
		return script, []string{"-qfec", strings.Join(parts, " "), "/dev/null"}
	}
	return bin, args
}

// ---------- разбор строк ----------

var (
	rePercent   = regexp.MustCompile(`(\d{1,3})\s*%`)
	reSpeedInfo = regexp.MustCompile(`\(([^()]*MB/s)\)`)
	reInstStat  = regexp.MustCompile(`^(?:Install|Upgrade|Installing|Upgrading)?:?\s*([A-Za-z]+)\s*\((\d{1,3})%\)`)
)

var installStages = map[string]string{
	"CreatingStagingDirectory":    "Готовлю iPhone",
	"ExtractingPackage":           "Распаковываю",
	"InspectingPackage":           "Проверяю приложение",
	"TakingInstallLock":           "Жду iPhone",
	"PreflightingApplication":     "Проверяю совместимость",
	"InstallingEmbeddedProfile":   "Ставлю профиль",
	"VerifyingApplication":        "Проверяю подпись",
	"CreatingContainer":           "Создаю папку приложения",
	"InstallingApplication":       "Устанавливаю",
	"PostflightingApplication":    "Завершаю",
	"SandboxingApplication":       "Завершаю",
	"GeneratingApplicationMap":    "Обновляю список приложений",
	"UpdatingSpotlight":           "Обновляю поиск",
	"RecoveringPreviousContainer": "Переношу данные",
	"Complete":                    "Готово",
}

// downloadLine разбирает строку прогресса ipatool: « 42% ... (120 MB/290 MB, 8.4 MB/s)».
func (c *JobCtl) downloadLine(line string) bool {
	m := rePercent.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	p, _ := strconv.Atoi(m[1])
	detail := fmt.Sprintf("Скачиваю: %d%%", p)
	if s := reSpeedInfo.FindStringSubmatch(line); s != nil {
		detail += " · " + strings.ReplaceAll(strings.ReplaceAll(s[1], "MB", "МБ"), "/s", "/с")
	}
	c.fromTool()
	c.Progress(float64(p), detail)
	return true
}

// installLine разбирает статусы ideviceinstaller: «Install: VerifyingApplication (40%)».
func (c *JobCtl) installLine(line string) bool {
	m := reInstStat.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return false
	}
	p, _ := strconv.Atoi(m[2])
	stage := installStages[m[1]]
	if stage == "" {
		stage = "Устанавливаю"
	}
	c.fromTool()
	c.Progress(float64(p), fmt.Sprintf("%s: %d%%", stage, p))
	return true
}

// ---------- подсчёт байт ----------

func dirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func mb(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.2f ГБ", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.0f МБ", float64(n)/(1<<20))
}

// watchDownload раз в секунду считает размер папки загрузки. Если сама утилита
// процент не печатает (Windows), прогресс берётся отсюда.
func (c *JobCtl) watchDownload(ctx context.Context, dir string, totalPtr *int64) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var prev int64
	prevT := time.Now()
	speed := 0.0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			size := dirSize(dir)
			dt := now.Sub(prevT).Seconds()
			if dt > 0 && size >= prev {
				cur := float64(size-prev) / dt
				if speed == 0 {
					speed = cur
				} else {
					speed = speed*0.6 + cur*0.4
				}
			}
			prev, prevT = size, now
			if c.toolActive() {
				continue
			}
			if size == 0 {
				continue
			}
			total := atomic.LoadInt64(totalPtr)
			sp := fmt.Sprintf("%.1f МБ/с", speed/(1<<20))
			if total > 0 {
				p := float64(size) / float64(total) * 100
				if p > 99 {
					p = 99
				}
				c.Progress(p, fmt.Sprintf("Скачано %s из %s · %s", mb(size), mb(total), sp))
			} else {
				c.Progress(-1, fmt.Sprintf("Скачано %s · %s", mb(size), sp))
			}
		}
	}
}

// lookupSize — примерный размер последней версии из публичного iTunes Lookup.
// Для удалённых из магазина приложений вернёт 0 — тогда показываем только мегабайты.
func lookupSize(ctx context.Context, id string) int64 {
	client := &http.Client{Timeout: 6 * time.Second}
	for _, cc := range []string{"ru", "by", "us"} {
		req, err := http.NewRequestWithContext(ctx, "GET", "https://itunes.apple.com/lookup?id="+id+"&country="+cc, nil)
		if err != nil {
			return 0
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var res struct {
			Results []struct {
				FileSizeBytes string `json:"fileSizeBytes"`
			} `json:"results"`
		}
		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err == nil && len(res.Results) > 0 {
			if n, err := strconv.ParseInt(res.Results[0].FileSizeBytes, 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

func elapsed(start time.Time) string {
	d := time.Since(start).Round(time.Second)
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}
