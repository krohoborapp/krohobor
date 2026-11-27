package main

// Иконки: кэш на диске (найденная однажды иконка остаётся навсегда) и извлечение иконки из .ipa.
// Иконки внутри .ipa хранятся в «сжатом» формате Apple (CgBI) — переводим в обычный PNG.

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var iconDir string

func iconPath(key string) string {
	return filepath.Join(iconDir, regexp.MustCompile(`[^0-9A-Za-z._-]`).ReplaceAllString(key, "_")+".png")
}

// fetchIcon скачивает картинку из App Store в кэш.
func fetchIcon(ctx context.Context, id, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New(resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil || len(data) < 100 {
		return nil, errors.New("пустая картинка")
	}
	_ = os.MkdirAll(iconDir, 0o755)
	_ = os.WriteFile(iconPath(id), data, 0o644)
	return data, nil
}

// ipaIcon достаёт самую крупную иконку приложения из .ipa.
func ipaIcon(path string) ([]byte, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	type cand struct {
		f    *zip.File
		size uint64
	}
	var cands []cand
	reIcon := regexp.MustCompile(`(?i)^Payload/[^/]+\.app/(AppIcon|Icon)[^/]*\.png$`)
	for _, f := range zr.File {
		if f.Name == "iTunesArtwork" || reIcon.MatchString(f.Name) {
			cands = append(cands, cand{f, f.UncompressedSize64})
		}
	}
	if len(cands) == 0 {
		return nil, errors.New("в файле нет иконки")
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].size > cands[j].size })
	for _, c := range cands {
		rc, err := c.f.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(rc, 8<<20))
		rc.Close()
		if err != nil {
			continue
		}
		if fixed, err := normalizePNG(data); err == nil {
			return fixed, nil
		}
	}
	return nil, errors.New("иконку не удалось прочитать")
}

// normalizePNG: обычный PNG/JPEG отдаём как есть, PNG в формате Apple CgBI — перекодируем.
func normalizePNG(data []byte) ([]byte, error) {
	if len(data) < 16 || !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		if len(data) > 3 && data[0] == 0xFF && data[1] == 0xD8 { // JPEG (iTunesArtwork)
			return data, nil
		}
		return nil, errors.New("не PNG")
	}
	if string(data[12:16]) != "CgBI" {
		return data, nil
	}
	pos := 8
	var w, h int
	var idat []byte
	for pos+8 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[pos:]))
		typ := string(data[pos+4 : pos+8])
		if pos+12+n > len(data) {
			break
		}
		body := data[pos+8 : pos+8+n]
		switch typ {
		case "IHDR":
			w, h = int(binary.BigEndian.Uint32(body[0:])), int(binary.BigEndian.Uint32(body[4:]))
			if body[8] != 8 || body[9] != 6 {
				return nil, errors.New("неподдерживаемый формат CgBI")
			}
		case "IDAT":
			idat = append(idat, body...)
		}
		pos += 12 + n
	}
	if w <= 0 || h <= 0 || w > 4096 || h > 4096 {
		return nil, errors.New("плохие размеры")
	}
	raw, err := io.ReadAll(flate.NewReader(bytes.NewReader(idat)))
	if err != nil && len(raw) == 0 {
		return nil, err
	}
	stride := w * 4
	if len(raw) < h*(stride+1) {
		return nil, errors.New("неполные данные")
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	prev := make([]byte, stride)
	cur := make([]byte, stride)
	for y := 0; y < h; y++ {
		line := raw[y*(stride+1):]
		filter := line[0]
		copy(cur, line[1:1+stride])
		for x := 0; x < stride; x++ {
			var a, b, c byte
			if x >= 4 {
				a = cur[x-4]
				c = prev[x-4]
			}
			b = prev[x]
			switch filter {
			case 1:
				cur[x] += a
			case 2:
				cur[x] += b
			case 3:
				cur[x] += byte((int(a) + int(b)) / 2)
			case 4:
				cur[x] += paeth(a, b, c)
			}
		}
		for x := 0; x < w; x++ {
			bl, g, r, al := cur[x*4], cur[x*4+1], cur[x*4+2], cur[x*4+3]
			if al > 0 && al < 255 { // убираем премультипликацию
				r = byte(min255(int(r) * 255 / int(al)))
				g = byte(min255(int(g) * 255 / int(al)))
				bl = byte(min255(int(bl) * 255 / int(al)))
			}
			img.SetNRGBA(x, y, color.NRGBA{r, g, bl, al})
		}
		prev, cur = cur, prev
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func min255(v int) int {
	if v > 255 {
		return 255
	}
	return v
}

func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// handleIcon: /api/icon?id=… или ?file=… — отдаёт иконку из кэша, App Store или из .ipa библиотеки.
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	id, file, bundle := r.URL.Query().Get("id"), r.URL.Query().Get("file"), r.URL.Query().Get("bundle")
	key := id
	if key == "" && bundle != "" && reBundle.MatchString(bundle) {
		key = "b_" + bundle
	} else if key == "" {
		key = "file_" + file
	}
	serve := func(data []byte) {
		ct := "image/png"
		if len(data) > 2 && data[0] == 0xFF && data[1] == 0xD8 {
			ct = "image/jpeg"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write(data)
	}
	if data, err := os.ReadFile(iconPath(key)); err == nil {
		serve(data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if id != "" && validID(id) {
		if u := s.catalog.IconOverride(id); u != "" {
			if data, err := fetchIcon(ctx, id, u); err == nil {
				serve(data)
				return
			}
		}
		infoCache.Lock()
		u := infoCache.m[id].Icon
		infoCache.Unlock()
		if u == "" {
			// приложение ещё не встречалось (например, стоит на iPhone, но его нет в каталоге) — спросим Apple во всех магазинах
			if inf, ok := lookupInfo(ctx, []string{id})[id]; ok {
				u = inf.Icon
			}
		}
		if u != "" {
			if data, err := fetchIcon(ctx, id, u); err == nil {
				serve(data)
				return
			}
		}
		byID, _ := s.lib.Snapshot()
		if files := byID[id]; len(files) > 0 {
			file = files[0]
		}
	}
	// приложение без номера App Store (стоит из другой страны или TestFlight) — ищем иконку по bundle ID в разных регионах
	if id == "" && file == "" && strings.HasPrefix(key, "b_") {
		if data, ok := bundleIcon(ctx, bundle, key); ok {
			serve(data)
			return
		}
	}
	if file != "" {
		if p, ok := s.safeIPA(file); ok {
			if data, err := ipaIcon(p); err == nil {
				_ = os.MkdirAll(iconDir, 0o755)
				_ = os.WriteFile(iconPath(key), data, 0o644)
				serve(data)
				return
			}
		}
	}
	http.NotFound(w, r)
}

var _ = strings.TrimSpace

var (
	reBundle    = regexp.MustCompile(`^[A-Za-z0-9._-]{3,200}$`)
	bundleMissM sync.Mutex
	bundleMiss  = map[string]time.Time{} // не нашли — не спрашиваем Apple снова 6 часов
)

func bundleIcon(ctx context.Context, bundle, key string) ([]byte, bool) {
	bundleMissM.Lock()
	t, missed := bundleMiss[bundle]
	bundleMissM.Unlock()
	if missed && time.Since(t) < 6*time.Hour {
		return nil, false
	}
	for _, cc := range []string{"ru", "us", "by", "kz", "tr", "ae", "gb", "de", "cn"} {
		items, ok := itunesGetOK(ctx, "https://itunes.apple.com/lookup?bundleId="+url.QueryEscape(bundle)+"&country="+cc)
		if !ok {
			break // нет связи с Apple — не запоминаем промах
		}
		if len(items) > 0 && items[0].icon() != "" {
			if data, err := fetchIcon(ctx, key, items[0].icon()); err == nil {
				return data, true
			}
		}
		if ctx.Err() != nil {
			return nil, false
		}
	}
	bundleMissM.Lock()
	bundleMiss[bundle] = time.Now()
	bundleMissM.Unlock()
	return nil, false
}
