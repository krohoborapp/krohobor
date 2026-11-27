package main

// Чтение метаданных из .ipa (Info.plist: XML или binary plist) и списков приложений из папки Files.

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
)

type IPAMeta struct {
	File     string `json:"file"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	MinOS    string `json:"minOS"`
	Platform string `json:"platform"`
	Size     int64  `json:"size"`
}

var plistKeys = []string{"CFBundleName", "CFBundleDisplayName", "CFBundleShortVersionString", "CFBundleVersion", "MinimumOSVersion", "DTPlatformName", "CFBundleSupportedPlatforms"}

var reInfoPlist = regexp.MustCompile(`^Payload/[^/]+\.app/Info\.plist$`)

func ReadIPAMeta(path string) (IPAMeta, error) {
	meta := IPAMeta{File: filepath.Base(path)}
	if st, err := os.Stat(path); err == nil {
		meta.Size = st.Size()
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return meta, err
	}
	defer zr.Close()
	var data []byte
	for _, f := range zr.File {
		if reInfoPlist.MatchString(f.Name) {
			rc, err := f.Open()
			if err != nil {
				return meta, err
			}
			data, err = io.ReadAll(io.LimitReader(rc, 8<<20))
			rc.Close()
			if err != nil {
				return meta, err
			}
			break
		}
	}
	if data == nil {
		return meta, errors.New("в архиве нет Info.plist")
	}
	var v map[string]string
	if bytes.HasPrefix(data, []byte("bplist00")) {
		v, err = parseBplist(data)
		if err != nil {
			return meta, err
		}
	} else {
		v = parseXMLPlist(string(data))
	}
	meta.Name = firstNonEmpty(v["CFBundleDisplayName"], v["CFBundleName"])
	meta.Version = firstNonEmpty(v["CFBundleShortVersionString"], v["CFBundleVersion"])
	meta.MinOS = v["MinimumOSVersion"]
	meta.Platform = platformName(firstNonEmpty(v["DTPlatformName"], v["CFBundleSupportedPlatforms"]))
	return meta, nil
}

func platformName(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "iphoneos":
		return "iOS"
	case "appletvos":
		return "tvOS"
	case "macosx":
		return "macOS"
	case "watchos":
		return "watchOS"
	case "xros":
		return "visionOS"
	}
	return "iOS"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func parseXMLPlist(content string) map[string]string {
	res := map[string]string{}
	for _, k := range plistKeys {
		pat := `<key>` + k + `</key>\s*<string>([^<]+)</string>`
		if k == "CFBundleSupportedPlatforms" {
			pat = `<key>` + k + `</key>\s*<array>\s*<string>([^<]+)</string>`
		}
		if m := regexp.MustCompile(pat).FindStringSubmatch(content); m != nil {
			res[k] = html.UnescapeString(m[1])
		}
	}
	return res
}

// parseBplist — минимальный разбор binary plist: верхний словарь, строковые значения
// и первая строка из массивов (для CFBundleSupportedPlatforms).
func parseBplist(b []byte) (map[string]string, error) {
	if len(b) < 40 {
		return nil, errors.New("короткий bplist")
	}
	tr := b[len(b)-32:]
	offSize := int(tr[6])
	refSize := int(tr[7])
	numObj := int(binary.BigEndian.Uint64(tr[8:16]))
	top := int(binary.BigEndian.Uint64(tr[16:24]))
	tableOff := int(binary.BigEndian.Uint64(tr[24:32]))
	if offSize < 1 || offSize > 8 || refSize < 1 || refSize > 8 || numObj <= 0 || tableOff+numObj*offSize > len(b) {
		return nil, errors.New("битый bplist")
	}
	readUint := func(pos, size int) (int, bool) {
		if pos < 0 || pos+size > len(b) {
			return 0, false
		}
		v := 0
		for i := 0; i < size; i++ {
			v = v<<8 | int(b[pos+i])
		}
		return v, true
	}
	offsetOf := func(ref int) (int, bool) {
		if ref < 0 || ref >= numObj {
			return 0, false
		}
		return readUint(tableOff+ref*offSize, offSize)
	}
	// длина объекта и позиция начала его данных
	lenAt := func(pos int) (int, int, bool) {
		info := int(b[pos] & 0x0F)
		if info != 0x0F {
			return info, pos + 1, true
		}
		if pos+1 >= len(b) || b[pos+1]>>4 != 0x1 {
			return 0, 0, false
		}
		size := 1 << (b[pos+1] & 0x0F)
		n, ok := readUint(pos+2, size)
		return n, pos + 2 + size, ok
	}
	var str func(ref int, depth int) (string, bool)
	str = func(ref int, depth int) (string, bool) {
		pos, ok := offsetOf(ref)
		if !ok || pos >= len(b) || depth > 2 {
			return "", false
		}
		typ := b[pos] >> 4
		n, start, ok := lenAt(pos)
		if !ok {
			return "", false
		}
		switch typ {
		case 0x5: // ASCII
			if start+n > len(b) {
				return "", false
			}
			return string(b[start : start+n]), true
		case 0x6: // UTF-16BE
			if start+2*n > len(b) {
				return "", false
			}
			u := make([]uint16, n)
			for i := 0; i < n; i++ {
				u[i] = binary.BigEndian.Uint16(b[start+2*i:])
			}
			return string(utf16.Decode(u)), true
		case 0x8: // UTF-8 (редко)
			if start+n > len(b) {
				return "", false
			}
			return string(b[start : start+n]), true
		case 0xA: // массив — берём первую строку
			if n == 0 {
				return "", false
			}
			r, ok := readUint(start, refSize)
			if !ok {
				return "", false
			}
			return str(r, depth+1)
		}
		return "", false
	}
	pos, ok := offsetOf(top)
	if !ok || pos >= len(b) || b[pos]>>4 != 0xD {
		return nil, errors.New("в bplist нет словаря")
	}
	n, start, ok := lenAt(pos)
	if !ok {
		return nil, errors.New("битый bplist")
	}
	want := map[string]bool{}
	for _, k := range plistKeys {
		want[k] = true
	}
	res := map[string]string{}
	for i := 0; i < n; i++ {
		kr, ok1 := readUint(start+i*refSize, refSize)
		vr, ok2 := readUint(start+(n+i)*refSize, refSize)
		if !ok1 || !ok2 {
			break
		}
		k, ok := str(kr, 0)
		if !ok || !want[k] {
			continue
		}
		if v, ok := str(vr, 0); ok {
			res[k] = v
		}
	}
	return res, nil
}

// ---------- списки приложений (формат IPA_Downloader: «Название: ID») ----------

var reListLine = regexp.MustCompile(`^(.*?):\s*(\d+)`)

func ReadAppsList(path, source string) []StoreApp {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var res []StoreApp
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "\ufeff")
		if m := reListLine.FindStringSubmatch(line); m != nil {
			res = append(res, StoreApp{Name: strings.TrimSpace(m[1]), ID: m[2], Source: source})
		}
	}
	return res
}

var reBadFileChars = regexp.MustCompile(`[\\/:*?"<>|]`)

// FinalName — имя файла как в IPA_Downloader: Название_Версия_Платформа_МинОС+_аккаунт.ipa
func FinalName(appName string, m IPAMeta, account string) string {
	name := firstNonEmpty(appName, m.Name, "App")
	name = reBadFileChars.ReplaceAllString(name, "")
	name = regexp.MustCompile(`\s*-\s*`).ReplaceAllString(name, "-")
	parts := []string{name, firstNonEmpty(m.Version, "0"), firstNonEmpty(m.Platform, "iOS"), firstNonEmpty(m.MinOS, "0") + "+"}
	if account != "" {
		parts = append(parts, account)
	}
	return strings.Join(strings.Fields(strings.Join(parts, "_")), "_") + ".ipa"
}
