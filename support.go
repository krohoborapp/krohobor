package main

// Журнал, настройки, проверка системы, отчёт об ошибке, понятные ошибки, поиск в App Store и статус «есть/удалено».

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ссылки на канал и чат. Их можно поменять без новой версии программы:
// при запуске программа читает links.json из репозитория (см. LinksURL).
var (
	ChannelURL = "https://t.me/krohobortv"
	ChatURL    = "https://t.me/+DpS_lfSl1Mo3NjU6"
)

// applyLinks принимает только ссылки на t.me, чтобы файл в репозитории не мог увести людей куда-то ещё.
func applyLinks(data []byte) {
	var l struct{ Channel, Chat string }
	if json.Unmarshal(data, &l) != nil {
		return
	}
	if strings.HasPrefix(l.Channel, "https://t.me/") {
		ChannelURL = l.Channel
	}
	if strings.HasPrefix(l.Chat, "https://t.me/") {
		ChatURL = l.Chat
	}
}

// ---------- журнал ----------

var (
	logMu   sync.Mutex
	logFile *os.File
	logDir  string
)

func initLog(dir string) {
	logDir = dir
	_ = os.MkdirAll(dir, 0o755)
	// храним последние 7 файлов журнала
	if files, _ := filepath.Glob(filepath.Join(dir, "dimakrohobor-*.log")); len(files) > 6 {
		sort.Strings(files)
		for _, f := range files[:len(files)-6] {
			_ = os.Remove(f)
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "dimakrohobor-"+time.Now().Format("2006-01-02")+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		logFile = f
	}
}

var (
	reEmailAny = regexp.MustCompile(`([A-Za-z0-9._%+-])[A-Za-z0-9._%+-]*@([A-Za-z0-9.-]+\.[A-Za-z]{2,})`)
	reSecret   = regexp.MustCompile(`(?i)((?:-p|--password|-a|--auth-code|--keychain-passphrase)\s+)\S+`)
	reHome     = regexp.MustCompile(`(?i)(/Users/|/home/|\\Users\\)[^/\\\s]+`)
)

// mask убирает из текста личное: пароли, коды, почту, имя пользователя в путях.
func mask(s string) string {
	s = reSecret.ReplaceAllString(s, "${1}***")
	s = reEmailAny.ReplaceAllString(s, "${1}***@${2}")
	s = reHome.ReplaceAllString(s, "${1}…")
	return s
}

func logf(format string, args ...interface{}) {
	line := time.Now().Format("15:04:05 ") + mask(fmt.Sprintf(format, args...))
	logMu.Lock()
	defer logMu.Unlock()
	if logFile != nil {
		fmt.Fprintln(logFile, line)
	}
}

func logTail(n int) []string {
	logMu.Lock()
	path := ""
	if logFile != nil {
		path = logFile.Name()
	}
	logMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// ---------- настройки ----------

type Settings struct {
	Theme       string   `json:"theme"` // auto | light | dark
	GuideSeen   bool     `json:"guideSeen"`
	ThanksShown bool     `json:"thanksShown"`
	WizardDone  bool     `json:"wizardDone"`
	WizardVer   int      `json:"wizardVer"` // версия мастера, которую человек прошёл (2 — «стол»)
	SeenInit    bool     `json:"seenInit"`
	SeenIDs     []string `json:"seenIds,omitempty"` // приложения каталога, которые человек уже видел
}

type settingsStore struct {
	mu   sync.Mutex
	path string
	s    Settings
}

func loadSettings(path string) *settingsStore {
	st := &settingsStore{path: path, s: Settings{Theme: "auto"}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &st.s)
	}
	return st
}

func (st *settingsStore) Get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s
}

func (st *settingsStore) Update(fn func(*Settings)) Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	fn(&st.s)
	data, _ := json.MarshalIndent(st.s, "", "  ")
	_ = os.MkdirAll(filepath.Dir(st.path), 0o755)
	_ = os.WriteFile(st.path, data, 0o644)
	return st.s
}

// ---------- понятные ошибки ----------

type hint struct {
	re   *regexp.Regexp
	text string
}

var hints = []hint{
	{regexp.MustCompile(`(?i)forget your password|incorrect.*password|invalid credentials|bad login`), "Неверный пароль Apple ID. Проверь его, войдя на icloud.com. Если код 2FA не приходит, причина обычно тоже в пароле."},
	{regexp.MustCompile(`(?i)account is disabled|locked`), "Apple временно заблокировала аккаунт, обычно после нескольких неверных паролей или кодов. Разблокируй его на iPhone или на iforgot.apple.com."},
	{regexp.MustCompile(`(?i)license is required|item is temporarily unavailable`), "Apple не отдаёт это приложение на твой Apple ID: его не скачивали с этого аккаунта раньше, а в App Store его уже нет. Если оно было на другом твоём Apple ID, войди в него. Если приложение ещё есть в App Store твоего региона, сначала скачай его на iPhone из магазина."},
	{regexp.MustCompile(`(?i)app not found`), "Приложения больше нет в App Store, и на этом Apple ID его не скачивали. Проверь вкладку «Мои покупки» или войди в другой аккаунт, где оно было."},
	{regexp.MustCompile(`(?i)could not be found\. try again later|unable to be used in the itunes store`), "Apple не видит аккаунт в магазине. Скачай на iPhone любое бесплатное приложение из App Store с этим Apple ID и попробуй снова."},
	{regexp.MustCompile(`(?i)not logged in|empty songlist|token.*expired|session`), "Сессия Apple ID устарела. Выйди из аккаунта (нажми на имя в шапке) и войди заново."},
	{regexp.MustCompile(`(?i)timeout|resolve host|could not connect to server|failure when receiving|connection (reset|refused)|network`), "Не получилось связаться с серверами Apple. Проверь интернет, отключи VPN или прокси и попробуй ещё раз."},
	{regexp.MustCompile(`(?i)usbmuxd|AppleMobileDevice|lockdownd`), "Компьютер не видит iPhone. На Windows поставь iTunes или «Устройства Apple» из Microsoft Store, переподключи кабель, разблокируй телефон и нажми «Доверять»."},
	{regexp.MustCompile(`(?i)PackageExtractionFailed|not enough space|disk.?full`), "На iPhone не хватает памяти. Освободи место и попробуй ещё раз."},
	{regexp.MustCompile(`(?i)APIInternalError|IXErrorDomain`), "Удали это приложение с iPhone, перезагрузи телефон и установи заново."},
	{regexp.MustCompile(`(?i)MinimumOSVersion|DeviceOSVersionTooLow|requires a newer version of iOS`), "Эта версия приложения не подходит для твоей iOS. Скачай более старую версию через кнопку «Версии»."},
	{regexp.MustCompile(`(?i)sinf`), "В файле нет подписи Apple. Скачай приложение заново через программу."},
}

// friendly добавляет к технической ошибке подсказку на русском.
func friendly(raw string) string {
	for _, h := range hints {
		if h.re.MatchString(raw) {
			return h.text
		}
	}
	return ""
}

// ---------- App Store: поиск и статус ----------

type appInfo struct {
	MinOS     string `json:"m,omitempty"`
	Size      int64  `json:"s,omitempty"`
	Version   string `json:"v,omitempty"`
	Icon      string `json:"i,omitempty"`
	InStore   bool   `json:"st,omitempty"`
	Checked   bool   `json:"c,omitempty"`
	CheckedAt int64  `json:"t,omitempty"`
	Name      string `json:"n,omitempty"`
	BundleID  string `json:"b,omitempty"`
}

// infoCache — сведения из App Store; хранится на диске, чтобы иконки и данные не пропадали,
// даже когда Apple удалит приложение.
var infoCache = struct {
	sync.Mutex
	m     map[string]appInfo
	path  string
	dirty bool
}{m: map[string]appInfo{}}

func loadInfoCache(path string) {
	infoCache.Lock()
	defer infoCache.Unlock()
	infoCache.path = path
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &infoCache.m)
	}
	go func() {
		for range time.Tick(20 * time.Second) {
			saveInfoCache()
		}
	}()
}

// mergeAvailability — подхватываем результат ночной проверки с GitHub: иконки и статусы
// появляются сразу, без запросов к Apple по каждому приложению.
func mergeAvailability(data []byte) int {
	var av struct {
		CheckedAt string `json:"checkedAt"`
		Apps      map[string]struct {
			InStore  bool   `json:"st"`
			Icon     string `json:"i"`
			MinOS    string `json:"m"`
			Version  string `json:"v"`
			Size     int64  `json:"s"`
			BundleID string `json:"b"`
		} `json:"apps"`
	}
	if json.Unmarshal(data, &av) != nil || len(av.Apps) == 0 {
		return 0
	}
	t, err := time.Parse(time.RFC3339, av.CheckedAt)
	if err != nil {
		return 0
	}
	infoCache.Lock()
	defer infoCache.Unlock()
	n := 0
	for id, a := range av.Apps {
		old, ok := infoCache.m[id]
		if ok && old.Checked && old.CheckedAt >= t.Unix() {
			continue // свои данные свежее
		}
		v := appInfo{Icon: firstNonEmpty(a.Icon, old.Icon), InStore: a.InStore, Checked: true, CheckedAt: t.Unix(),
			MinOS: firstNonEmpty(a.MinOS, old.MinOS), Version: firstNonEmpty(a.Version, old.Version), BundleID: firstNonEmpty(a.BundleID, old.BundleID), Name: old.Name}
		v.Size = a.Size
		if v.Size == 0 {
			v.Size = old.Size
		}
		infoCache.m[id] = v
		n++
	}
	infoCache.dirty = n > 0
	return n
}

func saveInfoCache() {
	infoCache.Lock()
	defer infoCache.Unlock()
	if !infoCache.dirty || infoCache.path == "" {
		return
	}
	data, err := json.Marshal(infoCache.m)
	if err == nil {
		_ = os.WriteFile(infoCache.path, data, 0o644)
		infoCache.dirty = false
	}
}

var httpc = &http.Client{Timeout: 8 * time.Second}

type itunesItem struct {
	TrackID    int64  `json:"trackId"`
	TrackName  string `json:"trackName"`
	BundleID   string `json:"bundleId"`
	Version    string `json:"version"`
	Artwork512 string `json:"artworkUrl512"`
	Artwork100 string `json:"artworkUrl100"`
	MinOS      string `json:"minimumOsVersion"`
	FileSize   string `json:"fileSizeBytes"`
}

func (it itunesItem) icon() string {
	if it.Artwork512 != "" {
		return strings.Replace(it.Artwork512, "512x512bb", "128x128bb", 1)
	}
	return it.Artwork100
}

func itunesGet(ctx context.Context, u string) []itunesItem {
	items, _ := itunesGetOK(ctx, u)
	return items
}

// itunesGetOK: ok=false, если Apple вообще не ответила (нет интернета) — тогда статус «неизвестно».
func itunesGetOK(ctx context.Context, u string) ([]itunesItem, bool) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, false
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, false
	}
	var out struct {
		Results []itunesItem `json:"results"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil, false
	}
	return out.Results, true
}

// storeSearch — открытый поиск Apple по российскому магазину (работает без входа и не зависит от региона аккаунта).
func storeSearch(ctx context.Context, term string) []StoreApp {
	var apps []StoreApp
	for _, it := range itunesGet(ctx, "https://itunes.apple.com/search?entity=software&limit=15&country=ru&term="+url.QueryEscape(term)) {
		id := strconv.FormatInt(it.TrackID, 10)
		infoCache.Lock()
		sz, _ := strconv.ParseInt(it.FileSize, 10, 64)
		infoCache.m[id] = appInfo{Icon: it.icon(), InStore: true, Checked: true, CheckedAt: time.Now().Unix(), Name: it.TrackName, BundleID: it.BundleID, MinOS: it.MinOS, Size: sz, Version: it.Version}
		infoCache.dirty = true
		infoCache.Unlock()
		apps = append(apps, StoreApp{ID: id, Name: it.TrackName, BundleID: it.BundleID, Version: it.Version, Source: "appstore"})
	}
	return apps
}

type IconInfo struct {
	MinOS   string `json:"minOS,omitempty"`   // минимальная iOS текущей версии в App Store
	Size    int64  `json:"size,omitempty"`    // размер текущей версии
	Version string `json:"version,omitempty"` // номер текущей версии
	Icon    string `json:"icon"`
	InStore bool   `json:"inStore"`
	Known   bool   `json:"known"` // false — не удалось спросить Apple
}

// lookupInfo — иконка и статус «есть в российском/белорусском App Store» для списка ID.
// Магазины, где ищем: первые два определяют статус «Есть в App Store» (Россия и Беларусь),
// остальные — только чтобы найти иконку и данные приложений, удалённых у нас.
var statusCountries = []string{"ru", "by"}
var extraCountries = []string{"kz", "am", "ge", "uz", "kg", "az", "tr", "ae", "us"}

func infoOut(v appInfo) IconInfo {
	return IconInfo{Icon: v.Icon, InStore: v.InStore, Known: v.Checked, MinOS: v.MinOS, Size: v.Size, Version: v.Version}
}

// lookupInfo — иконка, версия, минимальная iOS и статус «есть в App Store» для списка ID.
func lookupInfo(ctx context.Context, ids []string) map[string]IconInfo {
	res := map[string]IconInfo{}
	var need []string
	now := time.Now().Unix()
	infoCache.Lock()
	for _, id := range ids {
		v, ok := infoCache.m[id]
		if ok && v.Checked && now-v.CheckedAt < 24*3600 {
			res[id] = infoOut(v)
		} else {
			need = append(need, id)
		}
	}
	infoCache.Unlock()
	if len(need) == 0 {
		return res
	}
	found := map[string]appInfo{}
	answered := false
	ask := func(cc string, ids []string, status bool) {
		for start := 0; start < len(ids); start += 100 {
			end := start + 100
			if end > len(ids) {
				end = len(ids)
			}
			items, ok := itunesGetOK(ctx, "https://itunes.apple.com/lookup?country="+cc+"&id="+strings.Join(ids[start:end], ","))
			if ok && status {
				answered = true
			}
			for _, it := range items {
				id := strconv.FormatInt(it.TrackID, 10)
				prev, seen := found[id]
				sz, _ := strconv.ParseInt(it.FileSize, 10, 64)
				v := appInfo{Icon: it.icon(), InStore: prev.InStore || status, Name: it.TrackName, BundleID: it.BundleID, MinOS: it.MinOS, Size: sz, Version: it.Version}
				if seen && prev.MinOS != "" { // данные из первого найденного магазина важнее
					v.MinOS, v.Size, v.Version, v.Icon = prev.MinOS, prev.Size, prev.Version, firstNonEmpty(prev.Icon, v.Icon)
				}
				found[id] = v
			}
		}
	}
	rest := func() []string {
		var r []string
		for _, id := range need {
			if _, ok := found[id]; !ok {
				r = append(r, id)
			}
		}
		return r
	}
	for _, cc := range statusCountries {
		ask(cc, rest(), true)
	}
	for _, cc := range extraCountries {
		if r := rest(); len(r) > 0 && ctx.Err() == nil {
			ask(cc, r, false)
		}
	}
	infoCache.Lock()
	defer infoCache.Unlock()
	for _, id := range need {
		v, ok := found[id]
		old := infoCache.m[id]
		if !ok { // нигде не нашлось — сохраняем то, что знали раньше (иконку, версию)
			v = old
			v.InStore = false
		}
		if v.Icon == "" {
			v.Icon = old.Icon
		}
		if ctx.Err() != nil || !answered {
			res[id] = IconInfo{Icon: firstNonEmpty(v.Icon, old.Icon)} // статус неизвестен, не сохраняем
			continue
		}
		v.Checked, v.CheckedAt = true, now
		infoCache.m[id] = v
		infoCache.dirty = true
		res[id] = infoOut(v)
	}
	return res
}

// ---------- проверка системы ----------

type Check struct {
	Group  string `json:"group"`
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"` // не ошибка, но стоит обратить внимание
	Info   string `json:"info"`
	Hint   string `json:"hint,omitempty"`
	Action string `json:"action,omitempty"` // кнопка-исправление: install-driver
}

func fileInfo(p string) (string, bool) {
	st, err := os.Stat(p)
	if err != nil || p == "" {
		return "не найден", false
	}
	return fmt.Sprintf("%s (%.1f МБ)", filepath.Base(p), float64(st.Size())/(1<<20)), true
}

func writable(dir string) bool {
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.CreateTemp(dir, ".check-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}

var reIpatoolVer = regexp.MustCompile(`ipatool-cpp-([\d.]+)`)

func (s *Server) runChecks(ctx context.Context) []Check {
	var out []Check
	add := func(c Check) { out = append(out, c) }
	reinstall := "Скачай программу заново целиком: похоже, часть файлов не распаковалась или её удалил антивирус."

	// --- Программа и утилиты ---
	g := "Программа и утилиты"
	t := s.tools
	if t == nil {
		t = &Tools{}
	}
	add(Check{Group: g, Name: "Папка утилит", OK: dirExists(t.BinDir), Info: mask(t.BinDir), Hint: ifs(!dirExists(t.BinDir), reinstall)})
	for _, it := range []struct{ name, path string }{
		{"ipatool (скачивание из App Store)", t.Ipatool},
		{"ideviceinstaller (установка на iPhone)", t.IdeviceInstaller},
		{"idevice_id (поиск iPhone)", t.IdeviceID},
		{"ideviceinfo (данные iPhone)", t.IdeviceInfo},
	} {
		info, ok := fileInfo(it.path)
		add(Check{Group: g, Name: it.name, OK: ok, Info: info, Hint: ifs(!ok, reinstall)})
	}
	// библиотеки libimobiledevice рядом с утилитами
	var libs []string
	switch runtime.GOOS {
	case "windows":
		libs = []string{"libimobiledevice-1.0.dll", "libplist-2.0.dll"}
	case "darwin":
		libs = []string{"libimobiledevice-1.0.6.dylib", "libplist-2.0.12.dylib"}
	default:
		libs = []string{"libimobiledevice-1.0.so.6", "libplist-2.0.so.12"}
	}
	var missing []string
	for _, l := range libs {
		if _, err := os.Stat(filepath.Join(t.BinDir, l)); err != nil {
			missing = append(missing, l)
		}
	}
	c := Check{Group: g, Name: "Библиотеки libimobiledevice", OK: len(missing) == 0}
	if c.OK {
		c.Info = strings.Join(libs, ", ")
		// реальная проверка: утилита запускается и находит библиотеки
		if _, code, err := t.Run(ctx, "", t.IdeviceID, "-h"); err != nil || code > 1 {
			c.OK, c.Info = false, "файлы на месте, но утилиты не запускаются"
			c.Hint = "Антивирус мог заблокировать файлы. Добавь папку программы в исключения или скачай программу заново."
		}
	} else {
		c.Info, c.Hint = "нет: "+strings.Join(missing, ", "), reinstall
	}
	add(c)

	// --- Драйвер ---
	add(s.driverCheck(ctx))

	// --- Списки и папки ---
	g = "Списки и папки"
	s.catalog.mu.RLock()
	nCat, nList := len(s.catalog.catalog), len(s.catalog.list)
	s.catalog.mu.RUnlock()
	add(Check{Group: g, Name: "Каталог российских приложений", OK: nCat > 0, Info: fmt.Sprintf("%d приложений", nCat), Hint: ifs(nCat == 0, "Нажми «Обновить списки». Если не помогло — скачай программу заново.")})
	add(Check{Group: g, Name: "Подборка IPA_Downloader", OK: nList > 0, Info: fmt.Sprintf("%d приложений", nList), Hint: ifs(nList == 0, "Нажми «Обновить списки».")})
	add(Check{Group: g, Name: "Моя библиотека IPA", OK: writable(s.apps), Info: mask(s.apps), Hint: ifs(!writable(s.apps), "Нет прав на запись в папку. Проверь, что «Документы» не только для чтения и не заблокированы антивирусом.")})
	add(Check{Group: g, Name: "Журнал", OK: writable(logDir), Info: mask(logDir)})

	// --- Сеть и аккаунт ---
	g = "Apple и iPhone"
	c = Check{Group: g, Name: "Связь с серверами Apple"}
	if items := itunesGet(ctx, "https://itunes.apple.com/lookup?id=284882215"); len(items) > 0 {
		c.OK, c.Info = true, "Apple отвечает"
	} else {
		c.Info, c.Hint = "Apple не отвечает", "Проверь интернет. Если включён VPN или прокси, попробуй без него."
	}
	add(c)
	if s.toolErr == "" {
		s.authMu.RLock()
		acc := s.tools.AuthInfo(ctx)
		s.authMu.RUnlock()
		c = Check{Group: g, Name: "Вход в Apple ID", OK: acc.LoggedIn}
		if acc.LoggedIn {
			c.Info = mask(acc.Email)
			if acc.Storefront != "" {
				c.Info += ", регион " + acc.Storefront
			}
		} else {
			c.Info, c.Hint = "Не выполнен", "Нажми «Войти в Apple ID» в шапке."
		}
		add(c)
		devs, _ := s.tools.Devices(ctx)
		c = Check{Group: g, Name: "iPhone подключён", OK: len(devs) > 0, Warn: len(devs) == 0}
		if len(devs) > 0 {
			c.Info = devs[0].Name + ", iOS " + devs[0].IOS
		} else {
			c.Info, c.Hint = "Не найден", "Подключи iPhone кабелем, разблокируй экран и нажми «Доверять». Нужен только для установки."
		}
		add(c)
	}
	return out
}

func ifs(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

// SystemInfo — блок «Система» в диагностике.
func (s *Server) systemInfo() []string {
	ipv := "не найден"
	if s.tools != nil {
		if m := reIpatoolVer.FindStringSubmatch(filepath.Base(s.tools.Ipatool)); m != nil {
			ipv = "ipatool-cpp " + m[1]
		}
	}
	return []string{
		"Дима Крохобор " + AppVersion,
		"ipatool: " + ipv,
		"Система: " + osName() + osVersion(),
		"Программа: " + mask(s.base),
		"Библиотека: " + mask(s.apps),
		"Журнал: " + mask(logDir),
	}
}

func osVersion() string {
	var out []byte
	switch runtime.GOOS {
	case "darwin":
		out, _ = exec.Command("sw_vers", "-productVersion").Output()
	case "windows":
		c := exec.Command("cmd", "/c", "ver")
		hideWindow(c)
		out, _ = c.Output()
	default:
		out, _ = exec.Command("uname", "-r").Output()
	}
	if v := strings.TrimSpace(string(out)); v != "" {
		return " (" + v + ")"
	}
	return ""
}

func osName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS " + runtime.GOARCH
	case "windows":
		return "Windows " + runtime.GOARCH
	}
	return runtime.GOOS + " " + runtime.GOARCH
}

// ---------- отчёт об ошибке ----------

func (s *Server) buildReport(ctx context.Context) (string, string) {
	var b strings.Builder
	fmt.Fprintf(&b, "Отчёт «Дима Крохобор» %s\n", AppVersion)
	fmt.Fprintf(&b, "Система: %s\n", osName())
	fmt.Fprintf(&b, "Время: %s\n\n", time.Now().Format("02.01.2006 15:04"))
	for _, l := range s.systemInfo()[1:3] {
		b.WriteString(l + "\n")
	}
	b.WriteString("\nПроверка:\n")
	for _, c := range s.runChecks(ctx) {
		mark := "✅"
		if c.Warn {
			mark = "⚠️"
		} else if !c.OK {
			mark = "❌"
		}
		fmt.Fprintf(&b, "%s %s: %s\n", mark, c.Name, c.Info)
	}
	shown := 0
	for _, j := range s.jobs.Snapshot() {
		if j.Status != "error" || shown >= 2 {
			continue
		}
		shown++
		fmt.Fprintf(&b, "\nОшибка: %s\n%s\n", j.Title, j.Result)
		lines := j.Log
		if len(lines) > 6 {
			lines = lines[len(lines)-6:]
		}
		for _, l := range lines {
			b.WriteString("  " + l + "\n")
		}
	}
	summary := mask(b.String())
	full := summary + "\n--- Журнал ---\n" + strings.Join(logTail(400), "\n") + "\n"
	path := filepath.Join(logDir, "Отчёт-"+time.Now().Format("2006-01-02_15-04-05")+".txt")
	_ = os.WriteFile(path, []byte(mask(full)), 0o644)
	return summary, path
}
