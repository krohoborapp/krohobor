// Ночная проверка каталога: в каких магазинах App Store есть каждое приложение,
// его иконка, версия, минимальная iOS и размер. Результат — availability.json,
// который программа скачивает при запуске. Запускается GitHub Actions каждую ночь.
//
//	go run ./tools/availability -catalog catalog.txt -list AppsList.txt -out availability.json
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type item struct {
	TrackID    int64  `json:"trackId"`
	BundleID   string `json:"bundleId"`
	Version    string `json:"version"`
	MinOS      string `json:"minimumOsVersion"`
	FileSize   string `json:"fileSizeBytes"`
	Artwork512 string `json:"artworkUrl512"`
	Artwork100 string `json:"artworkUrl100"`
}

type app struct {
	InStore   bool     `json:"st"`
	Countries []string `json:"c,omitempty"`
	Icon      string   `json:"i,omitempty"`
	MinOS     string   `json:"m,omitempty"`
	Version   string   `json:"v,omitempty"`
	Size      int64    `json:"s,omitempty"`
	BundleID  string   `json:"b,omitempty"`
}

var reID = regexp.MustCompile(`:\s*(\d{6,})`)

func readIDs(path string, set map[string]bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := reID.FindStringSubmatch(sc.Text()); m != nil && !strings.HasPrefix(strings.TrimSpace(sc.Text()), "#") {
			set[m[1]] = true
		}
	}
}

func main() {
	cat := flag.String("catalog", "catalog.txt", "")
	list := flag.String("list", "", "")
	out := flag.String("out", "availability.json", "")
	flag.Parse()
	set := map[string]bool{}
	readIDs(*cat, set)
	if *list != "" {
		readIDs(*list, set)
	}
	var ids []string
	for id := range set {
		ids = append(ids, id)
	}
	if len(ids) < 10 {
		fmt.Fprintln(os.Stderr, "слишком мало ID")
		os.Exit(1)
	}
	status := map[string]bool{"ru": true, "by": true}
	countries := []string{"ru", "by", "kz", "am", "ge", "uz", "kg", "az", "tr", "ae", "us"}
	res := map[string]*app{}
	client := &http.Client{Timeout: 20 * time.Second}
	failed := 0
	for _, cc := range countries {
		for i := 0; i < len(ids); i += 100 {
			j := i + 100
			if j > len(ids) {
				j = len(ids)
			}
			var data struct {
				Results []item `json:"results"`
			}
			ok := false
			for try := 0; try < 3 && !ok; try++ {
				resp, err := client.Get("https://itunes.apple.com/lookup?country=" + cc + "&id=" + strings.Join(ids[i:j], ","))
				if err == nil {
					ok = resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&data) == nil
					resp.Body.Close()
				}
				if !ok {
					time.Sleep(3 * time.Second)
				}
			}
			if !ok {
				failed++
				continue
			}
			for _, it := range data.Results {
				id := strconv.FormatInt(it.TrackID, 10)
				a := res[id]
				if a == nil {
					a = &app{}
					res[id] = a
				}
				a.Countries = append(a.Countries, cc)
				if status[cc] {
					a.InStore = true
				}
				if a.Version == "" {
					sz, _ := strconv.ParseInt(it.FileSize, 10, 64)
					icon := it.Artwork100
					if it.Artwork512 != "" {
						icon = strings.Replace(it.Artwork512, "512x512bb", "128x128bb", 1)
					}
					a.Icon, a.MinOS, a.Version, a.Size, a.BundleID = icon, it.MinOS, it.Version, sz, it.BundleID
				}
			}
			time.Sleep(300 * time.Millisecond) // не нагружаем Apple
		}
	}
	if failed > len(ids)/100+3 {
		fmt.Fprintln(os.Stderr, "слишком много неответов Apple:", failed)
		os.Exit(1)
	}
	for _, id := range ids { // нигде нет — тоже записываем, это и есть «удалено»
		if res[id] == nil {
			res[id] = &app{}
		}
	}
	data, _ := json.Marshal(map[string]interface{}{"checkedAt": time.Now().UTC().Format(time.RFC3339), "apps": res})
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("проверено %d приложений, сохранено в %s\n", len(ids), *out)
}
