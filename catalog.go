package main

// Каталог российских приложений, подборка IPA_Downloader и умный поиск по ним:
// без учёта регистра/дефисов/пробелов, русский↔английский, неправильная раскладка, опечатки, синонимы.

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// Каталог Димы — подставить настоящий репозиторий, когда он появится.
	CatalogURL      = "https://raw.githubusercontent.com/" + UpdateRepo + "/main/catalog.txt"
	AvailabilityURL = "https://raw.githubusercontent.com/" + UpdateRepo + "/data/availability.json"
	LinksURL        = "https://raw.githubusercontent.com/" + UpdateRepo + "/main/links.json"
	AppsListURL     = "https://raw.githubusercontent.com/kda2495/IPA_Downloader/refs/heads/main/Files/AppsList.txt"
)

type Entry struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`     // полное имя из файла: «Альфа-Банк (Деньги пришли)»
	Brand    string   `json:"brand"`    // «Альфа-Банк»
	Store    string   `json:"store"`    // как называется в App Store: «Деньги пришли» (пусто, если оригинал)
	Category string   `json:"category"` // для каталога
	Syn      []string `json:"-"`
	Source   string   `json:"source"`          // catalog | list | custom
	Added    string   `json:"added,omitempty"` // дата добавления в каталог (ГГГГ-ММ-ДД)
	Icon     string   `json:"-"`               // ссылка на иконку, вписанная вручную
	keys     []string
}

type Catalog struct {
	mu      sync.RWMutex
	catalog []*Entry // российские, с категориями
	list    []*Entry // подборка IPA_Downloader + пользовательский список
	cats    []string
}

var reEntry = regexp.MustCompile(`^(.*?):\s*(\d{6,})\s*(?:\|\s*(.*))?$`)
var reDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
var reParen = regexp.MustCompile(`^(.*?)\s*\((.*)\)\s*$`)

func parseEntries(r io.Reader, source string) ([]*Entry, []string) {
	var res []*Entry
	var cats []string
	cat := ""
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if strings.HasPrefix(line, "##") {
			cat = strings.TrimSpace(strings.TrimLeft(line, "#"))
			cats = append(cats, cat)
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := reEntry.FindStringSubmatch(line)
		if m == nil {
			// формат IPA_Downloader может содержать хвост после ID
			if mm := reListLine.FindStringSubmatch(line); mm != nil {
				m = []string{line, mm[1], mm[2], ""}
			} else {
				continue
			}
		}
		e := &Entry{ID: m[2], Name: strings.TrimSpace(m[1]), Category: cat, Source: source}
		e.Brand, e.Store = e.Name, ""
		if p := reParen.FindStringSubmatch(e.Name); p != nil {
			e.Brand = strings.TrimSpace(p[1])
			if !strings.EqualFold(strings.TrimSpace(p[2]), "Оригинал") {
				e.Store = strings.TrimSpace(p[2])
			}
		}
		// после «|»: слова для поиска, дата добавления (ГГГГ-ММ-ДД), ссылка на иконку — в любом порядке
		for _, part := range strings.Split(m[3], "|") {
			part = strings.TrimSpace(part)
			switch {
			case part == "":
			case reDate.MatchString(part):
				e.Added = part
			case strings.HasPrefix(part, "http://") || strings.HasPrefix(part, "https://"):
				e.Icon = part
			default:
				for _, s := range strings.Split(part, ",") {
					if s = strings.TrimSpace(s); s != "" {
						e.Syn = append(e.Syn, s)
					}
				}
			}
		}
		e.buildKeys()
		res = append(res, e)
	}
	return res, cats
}

func (e *Entry) buildKeys() {
	add := func(s string) {
		if k := skel(s); k != "" {
			e.keys = append(e.keys, k)
		}
	}
	add(e.Name)
	add(e.Brand)
	add(e.Store)
	for _, s := range e.Syn {
		add(s)
	}
}

// ---------- загрузка с GitHub с кэшем ----------

// fetchCached: пробует скачать свежий файл, сохраняет в кэш; при неудаче отдаёт кэш или встроенную копию.
func fetchCached(ctx context.Context, url, cachePath, fallback string) []byte {
	client := &http.Client{Timeout: 6 * time.Second}
	if req, err := http.NewRequestWithContext(ctx, "GET", url, nil); err == nil {
		if resp, err := client.Do(req); err == nil {
			data, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if rerr == nil && resp.StatusCode == 200 && len(data) > 100 {
				_ = os.MkdirAll(filepath.Dir(cachePath), 0o755)
				_ = os.WriteFile(cachePath, data, 0o644)
				return data
			}
		}
	}
	if data, err := os.ReadFile(cachePath); err == nil && len(data) > 0 {
		return data
	}
	data, _ := os.ReadFile(fallback)
	return data
}

func (c *Catalog) Load(ctx context.Context, s *Server) {
	catData := fetchCached(ctx, CatalogURL, filepath.Join(s.cfgDir, "catalog.txt"), filepath.Join(s.files, "catalog.txt"))
	listData := fetchCached(ctx, AppsListURL, filepath.Join(s.cfgDir, "AppsList.txt"), filepath.Join(s.files, "AppsList.txt"))
	cat, cats := parseEntries(strings.NewReader(string(catData)), "catalog")
	list, _ := parseEntries(strings.NewReader(string(listData)), "list")
	if f, err := os.Open(filepath.Join(s.dataDir, "Мой список.txt")); err == nil {
		custom, _ := parseEntries(f, "custom")
		f.Close()
		list = append(custom, list...)
	}
	// синонимы из каталога помогают и подборке
	syn := map[string][]string{}
	for _, e := range cat {
		syn[e.ID] = e.Syn
	}
	for _, e := range list {
		if len(e.Syn) == 0 && syn[e.ID] != nil {
			e.Syn = syn[e.ID]
			e.keys = nil
			e.buildKeys()
		}
	}
	c.mu.Lock()
	c.catalog, c.list, c.cats = cat, list, cats
	c.mu.Unlock()
	logf("каталог: %d приложений в %d категориях, подборка: %d", len(cat), len(cats), len(list))
}

func (c *Catalog) IconOverride(id string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.catalog {
		if e.ID == id && e.Icon != "" {
			return e.Icon
		}
	}
	return ""
}

// IDs — все ID приложений каталога.
func (c *Catalog) IDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := make([]string, 0, len(c.catalog))
	for _, e := range c.catalog {
		ids = append(ids, e.ID)
	}
	return ids
}

func (c *Catalog) Entries() []Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Entry, 0, len(c.catalog))
	for _, e := range c.catalog {
		out = append(out, *e)
	}
	return out
}

func (c *Catalog) NameByID(id string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, arr := range [][]*Entry{c.catalog, c.list} {
		for _, e := range arr {
			if e.ID == id {
				return e.Name
			}
		}
	}
	return ""
}

// ---------- нормализация ----------

var lat2cyr = []struct{ from, to string }{
	{"shch", "ш"}, {"sch", "ш"}, {"sh", "ш"}, {"ch", "ч"}, {"zh", "ж"}, {"kh", "х"}, {"ts", "ц"},
	{"yu", "ю"}, {"ya", "я"}, {"yo", "е"}, {"ye", "е"}, {"ph", "ф"}, {"ck", "к"}, {"ee", "и"}, {"oo", "у"},
	{"a", "а"}, {"b", "б"}, {"c", "к"}, {"d", "д"}, {"e", "е"}, {"f", "ф"}, {"g", "г"}, {"h", "х"}, {"i", "и"},
	{"j", "ж"}, {"k", "к"}, {"l", "л"}, {"m", "м"}, {"n", "н"}, {"o", "о"}, {"p", "п"}, {"q", "к"}, {"r", "р"},
	{"s", "с"}, {"t", "т"}, {"u", "у"}, {"v", "в"}, {"w", "в"}, {"x", "кс"}, {"y", "и"}, {"z", "з"},
}

var cyrNorm = map[rune]string{'ё': "е", 'й': "и", 'ы': "и", 'ь': "", 'ъ': "", 'э': "е", 'щ': "ш"}

// skel превращает любое написание в «скелет» на кириллице: «T-Bank», «т банк», «Т-Банк» → «тбанк».
func skel(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for i := 0; i < len(s); {
		matched := false
		if s[i] < 128 && unicode.IsLetter(rune(s[i])) {
			for _, p := range lat2cyr {
				if strings.HasPrefix(s[i:], p.from) {
					b.WriteString(p.to)
					i += len(p.from)
					matched = true
					break
				}
			}
		}
		if matched {
			continue
		}
		r, size := decodeRune(s[i:])
		i += size
		if v, ok := cyrNorm[r]; ok {
			b.WriteString(v)
		} else if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	// двойные буквы → одна: «тинькофф» = «тинькоф»
	out := []rune(b.String())
	var res []rune
	for i, r := range out {
		if i > 0 && r == out[i-1] {
			continue
		}
		res = append(res, r)
	}
	return string(res)
}

func decodeRune(s string) (rune, int) {
	return utf8.DecodeRuneInString(s)
}

const layoutEN = "qwertyuiop[]asdfghjkl;'zxcvbnm,.`"
const layoutRU = "йцукенгшщзхъфывапролджэячсмитьбюё"

// swapLayout чинит текст, набранный не в той раскладке: «nbymrjaa» → «тинькофф», «ыиук» → «sber».
func swapLayout(s string) string {
	en, ru := []rune(layoutEN), []rune(layoutRU)
	m := map[rune]rune{}
	for i := range en {
		m[en[i]] = ru[i]
		m[ru[i]] = en[i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if v, ok := m[r]; ok {
			b.WriteRune(v)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// approxSubstring — наименьшее число правок, чтобы q встретилось внутри t (алгоритм Селлерса).
func approxSubstring(q, t []rune) int {
	m := len(q)
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for i := 0; i <= m; i++ {
		prev[i] = i
	}
	best := prev[m]
	for j := 1; j <= len(t); j++ {
		cur[0] = 0
		for i := 1; i <= m; i++ {
			cost := 1
			if q[i-1] == t[j-1] {
				cost = 0
			}
			v := prev[i-1] + cost
			if prev[i]+1 < v {
				v = prev[i] + 1
			}
			if cur[i-1]+1 < v {
				v = cur[i-1] + 1
			}
			cur[i] = v
		}
		if cur[m] < best {
			best = cur[m]
		}
		prev, cur = cur, prev
	}
	return best
}

func queryVariants(q string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range []string{q, swapLayout(q)} {
		if k := skel(v); len([]rune(k)) >= 2 && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func score(e *Entry, variants []string) int {
	best := 0
	for vi, v := range variants {
		penalty := vi * 5 // вариант с исправленной раскладкой чуть ниже
		vr := []rune(v)
		allow := 0
		switch {
		case len(vr) >= 7:
			allow = 2
		case len(vr) >= 4:
			allow = 1
		}
		for ki, k := range e.keys {
			sc := 0
			switch {
			case k == v:
				sc = 100
			case strings.HasPrefix(k, v):
				sc = 85
			case strings.Contains(k, v):
				sc = 70
			case allow > 0:
				if d := approxSubstring(vr, []rune(k)); d <= allow {
					sc = 50 - d*10
				}
			}
			if ki == 0 && sc > 0 {
				sc += 3 // совпадение по полному имени
			}
			if sc-penalty > best {
				best = sc - penalty
			}
		}
	}
	return best
}

type Hit struct {
	StoreApp
	Brand    string `json:"brand"`
	StoreAs  string `json:"storeAs,omitempty"`
	Category string `json:"category,omitempty"`
	score    int
}

func (c *Catalog) Search(q string, limit int) []Hit {
	variants := queryVariants(q)
	if len(variants) == 0 {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := map[string]bool{}
	var hits []Hit
	for _, arr := range [][]*Entry{c.catalog, c.list} {
		for _, e := range arr {
			if seen[e.ID] {
				continue
			}
			if sc := score(e, variants); sc > 0 {
				seen[e.ID] = true
				if e.Source == "catalog" {
					sc += 2
				}
				if e.Store == "" {
					sc++ // оригинал выше маскировок
				}
				hits = append(hits, Hit{StoreApp: StoreApp{ID: e.ID, Name: e.Name, Source: e.Source}, Brand: e.Brand, StoreAs: e.Store, Category: e.Category, score: sc})
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].Name < hits[j].Name
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

type Brand struct {
	Brand    string  `json:"brand"`
	Category string  `json:"category"`
	Apps     []Entry `json:"apps"` // первым — оригинал, если есть
}

func (c *Catalog) Brands() ([]string, []Brand) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	idx := map[string]int{}
	var out []Brand
	for _, e := range c.catalog {
		key := e.Category + "|" + e.Brand
		i, ok := idx[key]
		if !ok {
			i = len(out)
			idx[key] = i
			out = append(out, Brand{Brand: e.Brand, Category: e.Category})
		}
		if e.Store == "" {
			out[i].Apps = append([]Entry{*e}, out[i].Apps...)
		} else {
			out[i].Apps = append(out[i].Apps, *e)
		}
	}
	return append([]string(nil), c.cats...), out
}

func (c *Catalog) List() []*Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]*Entry(nil), c.list...)
}

// ---------- ссылки App Store ----------

var reStoreLink = regexp.MustCompile(`(?i)(?:apps\.apple\.com|itunes\.apple\.com)\S*?/id(\d{6,})`)
var reIDParam = regexp.MustCompile(`(?i)[?&]id=(\d{6,})`)

func idFromLink(s string) string {
	if m := reStoreLink.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if strings.Contains(s, "apple.com") {
		if m := reIDParam.FindStringSubmatch(s); m != nil {
			return m[1]
		}
	}
	return ""
}
