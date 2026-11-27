package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var AppVersion = "1.0.0" // задаётся при сборке релиза: -ldflags "-X main.AppVersion=1.0.1"

//go:embed web
var webFS embed.FS

type Server struct {
	base    string
	files   string
	apps    string
	tools   *Tools
	toolErr string
	token   string
	authMu  sync.RWMutex // вход/выход — эксклюзивно, остальные команды ipatool — параллельно
	jobs    *Jobs

	purchMu    sync.Mutex
	purchCache map[string][]StoreApp

	lib      *libIndex
	dlSem    chan struct{} // не больше двух загрузок одновременно
	dataDir  string        // Документы/Дима Крохобор — видно пользователю
	cfgDir   string        // настройки и кэш
	catalog  *Catalog
	settings *settingsStore
}

// ---------- фоновые задачи (загрузка, установка) ----------

type Job struct {
	ID       int      `json:"id"`
	Title    string   `json:"title"`
	Status   string   `json:"status"` // running | done | error
	Log      []string `json:"log"`
	Result   string   `json:"result,omitempty"`
	Started  int64    `json:"started"`
	Progress float64  `json:"progress"` // 0..100, -1 — неизвестно
	Detail   string   `json:"detail"`
	Hint     string   `json:"hint,omitempty"` // подсказка на русском, что делать при ошибке
	AppID    string   `json:"appId,omitempty"`
	Batch    string   `json:"batch,omitempty"`
	Finished int64    `json:"finished,omitempty"`
	Kind     string   `json:"kind"`
	File     string   `json:"file,omitempty"`   // файл в библиотеке, с которым связана задача
	Name     string   `json:"name,omitempty"`   // понятное имя приложения
	Target   string   `json:"target,omitempty"` // на какой iPhone ставим (пусто — только скачать)
}

type Jobs struct {
	mu   sync.Mutex
	next int
	list []*Job
}

func (j *Jobs) Start(title string, fn func(c *JobCtl) (string, error)) *Job {
	j.mu.Lock()
	j.next++
	job := &Job{ID: j.next, Title: title, Status: "running", Started: time.Now().Unix(), Progress: -1}
	switch {
	case strings.HasPrefix(title, "Загрузка"):
		job.Kind = "download"
	case strings.HasPrefix(title, "Установка"):
		job.Kind = "install"
	default:
		job.Kind = "other"
	}
	logf("начало «%s»", title)
	j.list = append(j.list, job)
	if len(j.list) > 80 {
		j.list = j.list[len(j.list)-80:]
	}
	j.mu.Unlock()
	go func() {
		c := &JobCtl{jobs: j, job: job}
		res, err := fn(c)
		j.mu.Lock()
		job.Finished = time.Now().Unix()
		if err != nil {
			job.Status = "error"
			job.Result = err.Error()
			job.Hint = friendly(job.Result + "\n" + strings.Join(job.Log, "\n"))
			logf("ошибка задачи «%s»: %s", job.Title, job.Result)
		} else {
			logf("готово «%s»: %s", job.Title, res)
			job.Status = "done"
			job.Result = res
			job.Progress = 100
		}
		j.mu.Unlock()
	}()
	return job
}

// Remove убирает законченную задачу из истории; ClearFinished — все законченные.
func (j *Jobs) Remove(id int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i, job := range j.list {
		if job.ID == id && job.Status != "running" && job.Status != "queued" {
			j.list = append(j.list[:i], j.list[i+1:]...)
			return
		}
	}
}

func (j *Jobs) ClearFinished() {
	j.mu.Lock()
	defer j.mu.Unlock()
	keep := j.list[:0]
	for _, job := range j.list {
		if job.Status == "running" || job.Status == "queued" {
			keep = append(keep, job)
		}
	}
	j.list = keep
}

func (j *Jobs) Snapshot() []Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Job, 0, len(j.list))
	for i := len(j.list) - 1; i >= 0; i-- {
		c := *j.list[i]
		c.Log = append([]string(nil), c.Log...)
		out = append(out, c)
	}
	return out
}

// ---------- HTTP ----------

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readBody(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Только локальная страница с правильным токеном: другие сайты в браузере
		// не смогут дёрнуть API (защита от CSRF и DNS rebinding).
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "localhost" {
			writeErr(w, http.StatusForbidden, "forbidden host")
			return
		}
		tok := r.Header.Get("X-Token")
		if tok == "" && r.URL.Path == "/api/icon" { // картинки грузятся тегом <img>, без заголовков
			tok = r.URL.Query().Get("t")
		}
		if tok != s.token {
			writeErr(w, http.StatusUnauthorized, "bad token")
			return
		}
		next(w, r)
	}
}

func (s *Server) ctx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func (s *Server) needTools(w http.ResponseWriter) bool {
	if s.toolErr != "" {
		writeErr(w, http.StatusInternalServerError, s.toolErr)
		return false
	}
	return true
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	res := map[string]interface{}{
		"version": AppVersion, "os": runtime.GOOS, "appsDir": s.apps, "toolError": s.toolErr,
		"settings": s.settings.Get(), "channel": ChannelURL, "chat": ChatURL,
	}
	// macOS запускает программу из «Загрузок» во временной папке (App Translocation) — тогда не работает обновление
	if exe, err := os.Executable(); err == nil && runtime.GOOS == "darwin" && strings.Contains(exe, "/AppTranslocation/") {
		res["translocated"] = true
	}
	updMu.Lock()
	if lastRelease != nil {
		res["update"] = lastRelease
	}
	updMu.Unlock()
	if runtime.GOOS == "windows" {
		ctx, cancel := s.ctx(r, 10*time.Second)
		res["driver"] = driverState(ctx)
		cancel()
	} else {
		res["driver"] = "ok"
	}
	if s.toolErr == "" {
		s.authMu.RLock()
		ctx, cancel := s.ctx(r, 20*time.Second)
		res["account"] = s.tools.AuthInfo(ctx)
		cancel()
		s.authMu.RUnlock()
	}
	writeJSON(w, res)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.needTools(w) {
		return
	}
	var req struct{ Email, Password, Code string }
	if err := readBody(r, &req); err != nil || strings.TrimSpace(req.Email) == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "введи Apple ID и пароль")
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	ctx, cancel := s.ctx(r, 2*time.Minute)
	defer cancel()
	res := s.tools.Login(ctx, strings.TrimSpace(req.Email), req.Password, strings.TrimSpace(req.Code))
	if res.Error != "" {
		res.Hint = friendly(res.Error)
		logf("вход не удался: %s", res.Error)
	} else if res.OK {
		logf("вход выполнен")
	}
	s.purchMu.Lock()
	s.purchCache = map[string][]StoreApp{}
	s.purchMu.Unlock()
	writeJSON(w, res)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.needTools(w) {
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	ctx, cancel := s.ctx(r, 30*time.Second)
	defer cancel()
	s.tools.Logout(ctx)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) nameFromLists(id string) string {
	return s.catalog.NameByID(id)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, map[string]interface{}{"apps": []Hit{}})
		return
	}
	logf("поиск: %s", q)
	var found []Hit
	seen := map[string]bool{}
	add := func(h Hit) {
		if h.ID != "" && !seen[h.ID] {
			seen[h.ID] = true
			found = append(found, h)
		}
	}
	idHit := func(id string) Hit {
		name := s.nameFromLists(id)
		h := Hit{StoreApp: StoreApp{ID: id, Name: firstNonEmpty(name, "Приложение "+id), Source: "id"}}
		if p := reParen.FindStringSubmatch(h.Name); p != nil {
			h.Brand, h.StoreAs = p[1], p[2]
		}
		return h
	}
	// ссылка из App Store или «Поделиться»
	if id := idFromLink(q); id != "" {
		add(idHit(id))
		writeJSON(w, map[string]interface{}{"apps": found})
		return
	}
	// один или несколько ID через запятую
	var ids []string
	allIDs := true
	for _, p := range strings.FieldsFunc(q, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if !validID(p) {
			allIDs = false
			break
		}
		ids = append(ids, p)
	}
	if allIDs && len(ids) > 0 {
		for _, id := range ids {
			add(idHit(id))
		}
		writeJSON(w, map[string]interface{}{"apps": found})
		return
	}
	for _, h := range s.catalog.Search(q, 40) {
		add(h)
	}
	// App Store: открытый поиск по российскому магазину + поиск в регионе аккаунта (ipatool)
	ctx, cancel := s.ctx(r, 25*time.Second)
	defer cancel()
	terms := []string{q}
	if alt := swapLayout(q); skel(alt) != skel(q) && len(s.catalog.Search(alt, 1)) > 0 {
		terms = append(terms, alt)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var store []StoreApp
	for _, t := range terms {
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			res := storeSearch(ctx, t)
			mu.Lock()
			store = append(store, res...)
			mu.Unlock()
		}(t)
	}
	if s.toolErr == "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.authMu.RLock()
			res, _ := s.tools.Search(ctx, q)
			s.authMu.RUnlock()
			mu.Lock()
			store = append(store, res...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, a := range store {
		add(Hit{StoreApp: a, Brand: a.Name})
	}
	if found == nil {
		found = []Hit{}
	}
	writeJSON(w, map[string]interface{}{"apps": found})
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	cats, brands := s.catalog.Brands()
	// новое с прошлого раза: при самом первом запуске всё считаем просмотренным
	st := s.settings.Get()
	seen := map[string]bool{}
	for _, id := range st.SeenIDs {
		seen[id] = true
	}
	var fresh []Entry
	if st.SeenInit {
		for _, e := range s.catalog.Entries() {
			if !seen[e.ID] {
				fresh = append(fresh, e)
			}
		}
	} else if len(s.catalog.IDs()) > 0 {
		ids := s.catalog.IDs()
		s.settings.Update(func(x *Settings) { x.SeenInit, x.SeenIDs = true, ids })
	}
	if fresh == nil {
		fresh = []Entry{}
	}
	writeJSON(w, map[string]interface{}{"categories": cats, "brands": brands, "fresh": fresh, "channel": ChannelURL})
}

func (s *Server) handleDeviceApps(w http.ResponseWriter, r *http.Request) {
	if !s.needTools(w) {
		return
	}
	ctx, cancel := s.ctx(r, 60*time.Second)
	defer cancel()
	devs, _ := s.tools.Devices(ctx)
	var d *Device
	for i := range devs {
		if devs[i].UDID == r.URL.Query().Get("udid") || r.URL.Query().Get("udid") == "" {
			d = &devs[i]
			break
		}
	}
	if d == nil || !d.Trusted {
		writeJSON(w, map[string]interface{}{"apps": []DevApp{}, "connected": false})
		return
	}
	apps, err := s.tools.DeviceApps(ctx, *d)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// сопоставляем с App Store ID: по itemId из iTunesMetadata, иначе по bundle id из кэша App Store
	byBundle := map[string]string{}
	infoCache.Lock()
	for id, v := range infoCache.m {
		if v.BundleID != "" {
			byBundle[v.BundleID] = id
		}
	}
	infoCache.Unlock()
	type out struct {
		DevApp
		ID string `json:"id,omitempty"`
	}
	res := make([]out, 0, len(apps))
	for _, a := range apps {
		id := a.ItemID
		if id == "" {
			id = byBundle[a.BundleID]
		}
		res = append(res, out{a, id})
	}
	writeJSON(w, map[string]interface{}{"apps": res, "connected": true, "device": d})
}

// handleImport — IPA, перетащенный в окно программы, кладём в библиотеку.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Query().Get("name"))
	if !strings.HasSuffix(strings.ToLower(name), ".ipa") {
		writeErr(w, http.StatusBadRequest, "это не файл .ipa")
		return
	}
	tmp, err := os.CreateTemp(s.apps, ".import-*.ipa")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, err = io.Copy(tmp, http.MaxBytesReader(w, r.Body, 4<<30))
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		writeErr(w, http.StatusBadRequest, "файл не загрузился: "+err.Error())
		return
	}
	if err := checkIPA(tmp.Name()); err != nil {
		os.Remove(tmp.Name())
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	dst := filepath.Join(s.apps, reBadFileChars.ReplaceAllString(name, "_"))
	if _, err := os.Stat(dst); err == nil {
		dst = strings.TrimSuffix(dst, ".ipa") + "_" + time.Now().Format("150405") + ".ipa"
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	meta, _ := ReadIPAMeta(dst)
	logf("добавлен файл в библиотеку: %s", filepath.Base(dst))
	writeJSON(w, map[string]interface{}{"file": filepath.Base(dst), "name": meta.Name, "minOS": meta.MinOS})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	list := s.catalog.List()
	apps := make([]Hit, 0, len(list))
	for _, e := range list {
		apps = append(apps, Hit{StoreApp: StoreApp{ID: e.ID, Name: e.Name, Source: e.Source}, Brand: e.Brand, StoreAs: e.Store})
	}
	sort.SliceStable(apps, func(i, j int) bool { return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name) })
	writeJSON(w, map[string]interface{}{"apps": apps})
}

func (s *Server) handlePurchases(w http.ResponseWriter, r *http.Request) {
	if !s.needTools(w) {
		return
	}
	mode := ""
	if r.URL.Query().Get("all") == "1" {
		mode = "all"
	}
	if y := r.URL.Query().Get("year"); len(y) == 4 {
		if _, err := strconv.Atoi(y); err == nil {
			mode = y
		}
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	ctx, cancel := s.ctx(r, 10*time.Minute)
	defer cancel()
	acc := s.tools.AuthInfo(ctx)
	key := acc.Email + "|" + mode
	s.purchMu.Lock()
	cached, ok := s.purchCache[key]
	s.purchMu.Unlock()
	if ok && !refresh {
		writeJSON(w, map[string]interface{}{"apps": cached})
		return
	}
	apps, err := s.tools.Purchases(ctx, mode)
	if err != nil {
		writeErr(w, http.StatusBadGateway, withHint(err.Error()))
		return
	}
	if apps == nil {
		apps = []StoreApp{}
	}
	s.purchMu.Lock()
	s.purchCache[key] = apps
	s.purchMu.Unlock()
	writeJSON(w, map[string]interface{}{"apps": apps})
}

func withHint(e string) string {
	if h := friendly(e); h != "" {
		return h + "\n\nТехнически: " + e
	}
	return e
}

func validID(id string) bool {
	if len(id) < 6 || len(id) > 20 {
		return false
	}
	_, err := strconv.ParseUint(id, 10, 64)
	return err == nil
}

func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	if !s.needTools(w) {
		return
	}
	id := r.URL.Query().Get("id")
	if !validID(id) {
		writeErr(w, http.StatusBadRequest, "неверный ID")
		return
	}
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	ctx, cancel := s.ctx(r, 2*time.Minute)
	defer cancel()
	ids, err := s.tools.Versions(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, withHint(err.Error()))
		return
	}
	writeJSON(w, map[string]interface{}{"versions": ids})
}

func (s *Server) handleVersionMeta(w http.ResponseWriter, r *http.Request) {
	if !s.needTools(w) {
		return
	}
	id, v := r.URL.Query().Get("id"), r.URL.Query().Get("v")
	if !validID(id) || !validID(v) && v != "" {
		writeErr(w, http.StatusBadRequest, "неверный ID")
		return
	}
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	ctx, cancel := s.ctx(r, time.Minute)
	defer cancel()
	m, err := s.tools.VersionMeta(ctx, id, v)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, m)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !s.needTools(w) {
		return
	}
	var req DLReq
	if err := readBody(r, &req); err != nil || !validID(req.ID) || (req.VersionID != "" && !validID(req.VersionID)) {
		writeErr(w, http.StatusBadRequest, "неверный ID")
		return
	}
	job := s.enqueue(req)
	writeJSON(w, map[string]int{"jobId": job.ID})
}

// DLReq — заявка на загрузку; незавершённые хранятся на диске и продолжаются после перезапуска.
type DLReq struct {
	ID, Name, VersionID, VersionLabel string
	Batch                             string  `json:",omitempty"` // метка пакетной загрузки
	Install                           *Device `json:",omitempty"` // сразу поставить на iPhone после загрузки
	Key                               string  `json:",omitempty"`
}

var queueMu sync.Mutex

func (s *Server) queuePath() string { return filepath.Join(s.cfgDir, "queue.json") }

func (s *Server) queueEdit(fn func(m map[string]DLReq)) {
	queueMu.Lock()
	defer queueMu.Unlock()
	m := map[string]DLReq{}
	if data, err := os.ReadFile(s.queuePath()); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	fn(m)
	data, _ := json.Marshal(m)
	_ = os.WriteFile(s.queuePath(), data, 0o644)
}

// resumeQueue — продолжить загрузки, прерванные закрытием программы.
func (s *Server) resumeQueue() {
	var pending []DLReq
	s.queueEdit(func(m map[string]DLReq) {
		for k, r := range m {
			r.Key, r.Install = k, nil
			pending = append(pending, r)
			delete(m, k)
		}
	})
	sort.Slice(pending, func(i, j int) bool { return pending[i].Key < pending[j].Key })
	for _, r := range pending {
		r.Key = ""
		s.enqueue(r)
	}
	if len(pending) > 0 {
		logf("продолжаю %d загрузок после перезапуска", len(pending))
	}
}

func (s *Server) enqueue(req DLReq) *Job {
	req.Key = fmt.Sprintf("%d-%s-%s", time.Now().UnixNano(), req.ID, req.VersionID)
	saved := req
	saved.Install = nil
	s.queueEdit(func(m map[string]DLReq) { m[req.Key] = saved })
	title := firstNonEmpty(req.Name, s.nameFromLists(req.ID), req.ID)
	if req.VersionLabel != "" {
		title += " " + req.VersionLabel
	}
	if req.Install != nil && !reUDID.MatchString(req.Install.UDID) {
		req.Install = nil
	}
	job := s.jobs.Start("Загрузка: "+title, func(c *JobCtl) (string, error) {
		defer s.queueEdit(func(m map[string]DLReq) { delete(m, req.Key) })
		logf := c.Log
		c.Queue("В очереди…")
		s.dlSem <- struct{}{}
		defer func() { <-s.dlSem }()
		c.Run()
		s.authMu.RLock()
		defer s.authMu.RUnlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		acc := s.tools.AuthInfo(ctx)
		if !acc.LoggedIn {
			return "", fmt.Errorf("сначала войди в Apple ID")
		}
		tmp, err := os.MkdirTemp(s.apps, ".download-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmp)
		c.Progress(-1, "Получаю лицензию и ссылку на файл…")
		var total int64 // размер узнаём параллельно, чтобы не задерживать старт загрузки
		if req.VersionID == "" {
			go func() { atomic.StoreInt64(&total, lookupSize(ctx, req.ID)) }()
		}
		watchCtx, stopWatch := context.WithCancel(ctx)
		go c.watchDownload(watchCtx, tmp, &total)
		defer stopWatch()
		args := []string{"download", "-i", req.ID, "-o", filepath.Join(tmp, "app.ipa")}
		if req.VersionID != "" {
			args = append(args, "--external-version-id", req.VersionID)
		} else {
			args = append(args, "--purchase")
		}
		onLine := func(l string) {
			if !c.downloadLine(l) {
				logf(l)
			}
		}
		_, err = s.tools.Stream(ctx, tmp, s.tools.Ipatool, onLine, args...)
		matches, _ := filepath.Glob(filepath.Join(tmp, "*.ipa"))
		// повторяем без псевдотерминала, только если Apple не ответила понятной ошибкой
		if len(matches) == 0 && ctx.Err() == nil && !strings.Contains(strings.ToLower(c.lastLog()), "error") {
			// запасной путь: без псевдотерминала, как в IPA_Downloader
			logf("Пробую ещё раз в обычном режиме…")
			_, err = s.tools.StreamOpt(ctx, tmp, s.tools.Ipatool, false, onLine, args...)
			matches, _ = filepath.Glob(filepath.Join(tmp, "*.ipa"))
		}
		stopWatch()
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			return "", fmt.Errorf("%s", c.lastLog())
		}
		c.Progress(100, "Сохраняю файл…")
		var saved []string
		for _, f := range matches {
			meta, _ := ReadIPAMeta(f)
			name := firstNonEmpty(req.Name, s.nameFromLists(req.ID), meta.Name)
			dst := filepath.Join(s.apps, FinalName(name, meta, acc.Email))
			_ = os.Remove(dst)
			if err := os.Rename(f, dst); err != nil {
				return "", err
			}
			saved = append(saved, filepath.Base(dst))
			s.lib.Add(req.ID, filepath.Base(dst))
			s.jobs.mu.Lock()
			c.job.File = filepath.Base(dst)
			s.jobs.mu.Unlock()
			if meta.MinOS != "" {
				logf("Минимальная версия " + firstNonEmpty(meta.Platform, "iOS") + ": " + meta.MinOS)
			}
			if req.Install != nil {
				d := *req.Install
				if meta.MinOS != "" && d.IOS != "?" && versionLess(d.IOS, meta.MinOS) {
					return "", fmt.Errorf("скачано в библиотеку, но эта версия требует iOS %s, а на %s — iOS %s. Выбери более старую версию", meta.MinOS, d.Name, d.IOS)
				}
				c.Log("Ставлю на " + d.Name)
				ictx, icancel := context.WithTimeout(context.Background(), 30*time.Minute)
				err := s.tools.Install(ictx, d, dst, c)
				icancel()
				if err != nil {
					return "", fmt.Errorf("скачано в библиотеку, но установка не удалась: %v", err)
				}
				return "Готово: скачано и установлено на " + d.Name, nil
			}
		}
		return "Сохранено в библиотеку: " + strings.Join(saved, ", "), nil
	})
	s.jobs.mu.Lock()
	job.AppID, job.Batch, job.Name = req.ID, req.Batch, req.Name
	if req.Install != nil {
		job.Target = req.Install.Name
	}
	s.jobs.mu.Unlock()
	return job
}

func (s *Server) safeIPA(name string) (string, bool) {
	if name == "" || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(strings.ToLower(name), ".ipa") {
		return "", false
	}
	p := filepath.Join(s.apps, name)
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	entries, _ := os.ReadDir(s.apps)
	type fileOut struct {
		IPAMeta
		ID  string `json:"id,omitempty"`
		Mod int64  `json:"mod,omitempty"` // когда файл появился в библиотеке
	}
	_, byFile := s.lib.Snapshot()
	files := []fileOut{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".ipa") {
			continue
		}
		m, err := ReadIPAMeta(filepath.Join(s.apps, e.Name()))
		if err != nil {
			m.Name = strings.TrimSuffix(e.Name(), ".ipa")
		}
		var mod int64
		if fi, err := e.Info(); err == nil {
			mod = fi.ModTime().Unix()
		}
		files = append(files, fileOut{IPAMeta: m, ID: byFile[e.Name()], Mod: mod})
	}
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].File) < strings.ToLower(files[j].File) })
	writeJSON(w, map[string]interface{}{"files": files, "dir": s.apps})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if !s.needTools(w) {
		return
	}
	ctx, cancel := s.ctx(r, 30*time.Second)
	defer cancel()
	devs, err := s.tools.Devices(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if devs == nil {
		devs = []Device{}
	}
	writeJSON(w, map[string]interface{}{"devices": devs})
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	if !s.needTools(w) {
		return
	}
	var req struct {
		File   string
		Device Device
	}
	if err := readBody(r, &req); err != nil || !reUDID.MatchString(req.Device.UDID) {
		writeErr(w, http.StatusBadRequest, "не выбрано устройство")
		return
	}
	path, ok := s.safeIPA(req.File)
	if !ok {
		writeErr(w, http.StatusBadRequest, "файл не найден")
		return
	}
	meta, _ := ReadIPAMeta(path)
	job := s.jobs.Start("Установка: "+firstNonEmpty(meta.Name, req.File)+" → "+req.Device.Name, func(c *JobCtl) (string, error) {
		if meta.MinOS != "" && req.Device.IOS != "?" && versionLess(req.Device.IOS, meta.MinOS) {
			return "", fmt.Errorf("нужна iOS %s или новее, а на устройстве %s", meta.MinOS, req.Device.IOS)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := s.tools.Install(ctx, req.Device, path, c); err != nil {
			return "", err
		}
		return "Установлено на " + req.Device.Name, nil
	})
	_, byFile := s.lib.Snapshot()
	s.jobs.mu.Lock()
	job.File, job.Name, job.AppID, job.Target = filepath.Base(path), firstNonEmpty(meta.Name, req.File), byFile[filepath.Base(path)], req.Device.Name
	s.jobs.mu.Unlock()
	writeJSON(w, map[string]int{"jobId": job.ID})
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func (s *Server) handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	dir := s.apps
	switch r.URL.Query().Get("which") {
	case "logs":
		dir = logDir
	case "data":
		dir = s.dataDir
	}
	openPath(dir)
	writeJSON(w, map[string]bool{"ok": true})
}

func openPath(p string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", p)
	case "darwin":
		cmd = exec.Command("open", p)
	default:
		cmd = exec.Command("xdg-open", p)
	}
	_ = cmd.Start()
}

// baseDir — где лежат MainApp и Files: рядом с программой или внутри .app (Contents/Resources).
func baseDir() string {
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	dir := filepath.Dir(exe)
	if strings.HasSuffix(dir, filepath.Join("Contents", "MacOS")) {
		return filepath.Join(filepath.Dir(dir), "Resources")
	}
	return dir
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func userDirs() (data, cfg string) {
	home, _ := os.UserHomeDir()
	if d := os.Getenv("KROHOBOR_DATA"); d != "" {
		data = d
	} else {
		data = filepath.Join(home, "Documents", "Дима Крохобор")
	}
	if d, err := os.UserConfigDir(); err == nil {
		cfg = filepath.Join(d, "DimaKrohobor")
	} else {
		cfg = filepath.Join(data, ".settings")
	}
	if d := os.Getenv("KROHOBOR_CONFIG"); d != "" {
		cfg = d
	}
	return
}

// ---------- один экземпляр программы ----------

var instancePath string

type instanceInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// tryExisting: если программа уже запущена — открываем её страницу и выходим.
func tryExisting(cfg string) bool {
	instancePath = filepath.Join(cfg, "instance.json")
	data, err := os.ReadFile(instancePath)
	if err != nil {
		return false
	}
	var in instanceInfo
	if json.Unmarshal(data, &in) != nil || in.Port == 0 {
		return false
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/ping", in.Port), nil)
	req.Header.Set("X-Token", in.Token)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	var ping struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ping)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	if ping.Version != AppVersion {
		// запущена другая версия (например, старая) — закрываем её и стартуем сами
		q, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/quit", in.Port), nil)
		q.Header.Set("X-Token", in.Token)
		if r, err := (&http.Client{Timeout: 2 * time.Second}).Do(q); err == nil {
			r.Body.Close()
		}
		for i := 0; i < 20; i++ { // ждём, пока порт освободится
			time.Sleep(150 * time.Millisecond)
			if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", in.Port), 200*time.Millisecond); err != nil {
				break
			} else {
				c.Close()
			}
		}
		return false
	}
	_ = openURL(fmt.Sprintf("http://127.0.0.1:%d/?t=%s", in.Port, in.Token))
	return true
}

func saveInstance(port int, token string) {
	data, _ := json.Marshal(instanceInfo{Port: port, Token: token})
	_ = os.MkdirAll(filepath.Dir(instancePath), 0o700)
	_ = os.WriteFile(instancePath, data, 0o600)
}

func releaseInstance() { _ = os.Remove(instancePath) }

func main() {
	data, cfg := userDirs()
	if os.Getenv("KROHOBOR_NO_BROWSER") == "" && tryExisting(cfg) {
		return
	}
	instancePath = filepath.Join(cfg, "instance.json")
	tb := make([]byte, 16)
	_, _ = rand.Read(tb)
	s := &Server{base: baseDir(), token: hex.EncodeToString(tb), jobs: &Jobs{}, purchCache: map[string][]StoreApp{}}
	s.dataDir, s.cfgDir = data, cfg
	s.files = filepath.Join(s.base, "Files")
	s.apps = filepath.Join(s.dataDir, "Моя библиотека IPA")
	// перенос из старой папки первых версий
	if old := filepath.Join(s.dataDir, "Скачанные"); dirExists(old) && !dirExists(s.apps) {
		_ = os.Rename(old, s.apps)
	}
	_ = os.MkdirAll(s.apps, 0o755)
	_ = os.MkdirAll(s.cfgDir, 0o755)
	initLog(filepath.Join(s.dataDir, "Журнал"))
	logf("запуск %s, %s, папка программы %s", AppVersion, osName(), s.base)
	s.settings = loadSettings(filepath.Join(s.cfgDir, "settings.json"))
	s.lib = loadLibIndex(s.apps, s.cfgDir)
	iconDir = filepath.Join(s.cfgDir, "icons")
	loadInfoCache(filepath.Join(s.cfgDir, "appinfo.json"))
	s.dlSem = make(chan struct{}, 2)
	if root, err := installRoot(); err == nil { // хвосты после обновления
		_ = os.RemoveAll(root + ".old")
		if self, err := os.Executable(); err == nil {
			_ = os.Remove(self + ".old")
		}
	}
	tools, err := LoadTools(s.base)
	s.tools = tools
	if err != nil {
		s.toolErr = err.Error()
		logf("утилиты: %v", err)
	}
	s.catalog = &Catalog{}
	// сначала мгновенно из кэша/встроенной копии, потом свежая версия из интернета
	offline, cancelOffline := context.WithCancel(context.Background())
	cancelOffline()
	s.catalog.Load(offline, s)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s.catalog.Load(ctx, s)
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		checkUpdate(ctx)
	}()
	if s.toolErr == "" {
		go s.resumeQueue()
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		data := fetchCached(ctx, AvailabilityURL, filepath.Join(s.cfgDir, "availability.json"), filepath.Join(s.files, "availability.json"))
		if n := mergeAvailability(data); n > 0 {
			logf("ночная проверка каталога: %d приложений", n)
		}
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		applyLinks(fetchCached(ctx, LinksURL, filepath.Join(s.cfgDir, "links.json"), ""))
	}()

	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(static)))
	api := map[string]http.HandlerFunc{
		"/api/ping": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]interface{}{"ok": true, "version": AppVersion})
		},
		"/api/status":      s.handleStatus,
		"/api/login":       s.handleLogin,
		"/api/logout":      s.handleLogout,
		"/api/search":      s.handleSearch,
		"/api/catalog":     s.handleCatalog,
		"/api/list":        s.handleList,
		"/api/purchases":   s.handlePurchases,
		"/api/versions":    s.handleVersions,
		"/api/version":     s.handleVersionMeta,
		"/api/download":    s.handleDownload,
		"/api/files":       s.handleFiles,
		"/api/devices":     s.handleDevices,
		"/api/install":     s.handleInstall,
		"/api/open-folder": s.handleOpenFolder,
		"/api/icons": func(w http.ResponseWriter, r *http.Request) {
			var ids []string
			for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
				if validID(id) && len(ids) < 300 {
					ids = append(ids, id)
				}
			}
			ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
			defer cancel()
			writeJSON(w, map[string]interface{}{"icons": lookupInfo(ctx, ids)})
		},
		"/api/settings": func(w http.ResponseWriter, r *http.Request) {
			var req map[string]interface{}
			if readBody(r, &req) != nil {
				writeErr(w, http.StatusBadRequest, "bad")
				return
			}
			st := s.settings.Update(func(st *Settings) {
				if v, ok := req["theme"].(string); ok && (v == "auto" || v == "light" || v == "dark") {
					st.Theme = v
				}
				if v, ok := req["guideSeen"].(bool); ok {
					st.GuideSeen = v
				}
				if v, ok := req["thanksShown"].(bool); ok {
					st.ThanksShown = v
				}
				if v, ok := req["wizardDone"].(bool); ok {
					st.WizardDone = v
				}
				if v, ok := req["wizardVer"].(float64); ok {
					st.WizardVer = int(v)
				}
			})
			writeJSON(w, st)
		},
		"/api/checks": func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
			defer cancel()
			writeJSON(w, map[string]interface{}{"checks": s.runChecks(ctx), "system": s.systemInfo()})
		},
		"/api/icon":        s.handleIcon,
		"/api/device-apps": s.handleDeviceApps,
		"/api/import":      s.handleImport,
		"/api/catalog-seen": func(w http.ResponseWriter, r *http.Request) {
			ids := s.catalog.IDs()
			s.settings.Update(func(x *Settings) { x.SeenInit, x.SeenIDs = true, ids })
			writeJSON(w, map[string]bool{"ok": true})
		},
		"/api/library-ids": func(w http.ResponseWriter, r *http.Request) {
			byID, _ := s.lib.Snapshot()
			writeJSON(w, map[string]interface{}{"ids": byID})
		},
		"/api/install-driver": func(w http.ResponseWriter, r *http.Request) {
			job := s.jobs.Start("Установка драйвера iPhone", installDriver)
			writeJSON(w, map[string]int{"jobId": job.ID})
		},
		"/api/reload-lists": func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
			defer cancel()
			s.catalog.Load(ctx, s)
			s.catalog.mu.RLock()
			n1, n2 := len(s.catalog.catalog), len(s.catalog.list)
			s.catalog.mu.RUnlock()
			writeJSON(w, map[string]int{"catalog": n1, "list": n2})
		},
		"/api/report": func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
			defer cancel()
			summary, path := s.buildReport(ctx)
			writeJSON(w, map[string]string{"summary": summary, "file": path, "chat": ChatURL})
		},
		"/api/show-file": func(w http.ResponseWriter, r *http.Request) {
			openPath(logDir)
			writeJSON(w, map[string]bool{"ok": true})
		},
		"/api/check-update": func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			rel := checkUpdate(ctx)
			go func() { // заодно обновим каталог
				c2, cl := context.WithTimeout(context.Background(), 20*time.Second)
				defer cl()
				s.catalog.Load(c2, s)
			}()
			if rel == nil {
				writeJSON(w, map[string]interface{}{"latest": true, "version": AppVersion})
				return
			}
			writeJSON(w, map[string]interface{}{"latest": false, "update": rel})
		},
		"/api/update": func(w http.ResponseWriter, r *http.Request) {
			job := s.jobs.Start("Обновление программы", applyUpdate)
			writeJSON(w, map[string]int{"jobId": job.ID})
		},
		"/api/jobs": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]interface{}{"jobs": s.jobs.Snapshot()})
		},
		"/api/jobs-remove": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("all") == "1" {
				s.jobs.ClearFinished()
			} else {
				id, _ := strconv.Atoi(r.URL.Query().Get("id"))
				s.jobs.Remove(id)
			}
			writeJSON(w, map[string]bool{"ok": true})
		},
		"/api/reveal": func(w http.ResponseWriter, r *http.Request) {
			path, ok := s.safeIPA(r.URL.Query().Get("file"))
			if !ok {
				writeErr(w, http.StatusBadRequest, "файл не найден")
				return
			}
			revealPath(path)
			writeJSON(w, map[string]bool{"ok": true})
		},
		"/api/trash": func(w http.ResponseWriter, r *http.Request) {
			var req struct{ Files []string }
			if readBody(r, &req) != nil {
				writeErr(w, http.StatusBadRequest, "bad")
				return
			}
			moved, lastErr := 0, ""
			for _, f := range req.Files {
				path, ok := s.safeIPA(f)
				if !ok {
					continue
				}
				if err := moveToTrash(path); err != nil {
					lastErr = err.Error()
					logf("не удалось убрать в корзину %s: %v", filepath.Base(path), err)
					continue
				}
				logf("убрано в корзину: %s", filepath.Base(path))
				moved++
			}
			s.lib.Snapshot()
			writeJSON(w, map[string]interface{}{"moved": moved, "error": lastErr})
		},
		"/api/quit": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]bool{"ok": true})
			go func() { time.Sleep(300 * time.Millisecond); releaseInstance(); os.Exit(0) }()
		},
	}
	for path, h := range api {
		mux.HandleFunc(path, s.guard(h))
	}

	ln, err := net.Listen("tcp", "127.0.0.1:47823")
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			logf("не удалось открыть порт: %v", err)
			log.Fatal(err)
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	saveInstance(port, s.token)
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, s.token)
	fmt.Println("Дима Крохобор", AppVersion)
	fmt.Println("Страница открыта в браузере. Если не открылась — скопируй ссылку:")
	fmt.Println(url)
	fmt.Println("Не закрывай это окно, пока пользуешься программой.")
	if os.Getenv("KROHOBOR_NO_BROWSER") == "" {
		_ = openURL(url)
	}
	log.Fatal(http.Serve(ln, mux))
}
