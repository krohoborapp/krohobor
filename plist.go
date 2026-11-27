package main

// Универсальный разбор plist (XML и binary) в map/[]interface{}/string/int64/bool/[]byte.
// Нужен для списка приложений с iPhone (ideviceinstaller --xml) и iTunesMetadata внутри него.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

func parsePlist(data []byte) (interface{}, error) {
	if bytes.HasPrefix(data, []byte("bplist00")) {
		return parseBplistAny(data)
	}
	return parseXMLPlistAny(data)
}

// ---------- XML ----------

func parseXMLPlistAny(data []byte) (interface{}, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local != "plist" {
			return xmlValue(dec, se)
		}
	}
}

func xmlText(dec *xml.Decoder) (string, error) {
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.EndElement:
			return sb.String(), nil
		}
	}
}

func xmlValue(dec *xml.Decoder, se xml.StartElement) (interface{}, error) {
	switch se.Name.Local {
	case "dict":
		m := map[string]interface{}{}
		key := ""
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					k, err := xmlText(dec)
					if err != nil {
						return nil, err
					}
					key = k
					continue
				}
				v, err := xmlValue(dec, t)
				if err != nil {
					return nil, err
				}
				m[key] = v
			case xml.EndElement:
				return m, nil
			}
		}
	case "array":
		var arr []interface{}
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := xmlValue(dec, t)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			case xml.EndElement:
				return arr, nil
			}
		}
	case "true", "false":
		_ = dec.Skip()
		return se.Name.Local == "true", nil
	}
	txt, err := xmlText(dec)
	if err != nil {
		return nil, err
	}
	switch se.Name.Local {
	case "integer":
		n, _ := strconv.ParseInt(strings.TrimSpace(txt), 10, 64)
		return n, nil
	case "real":
		f, _ := strconv.ParseFloat(strings.TrimSpace(txt), 64)
		return f, nil
	case "data":
		clean := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, txt)
		b, _ := base64.StdEncoding.DecodeString(clean)
		return b, nil
	}
	return txt, nil
}

// ---------- binary ----------

func parseBplistAny(b []byte) (interface{}, error) {
	if len(b) < 40 {
		return nil, errors.New("короткий bplist")
	}
	tr := b[len(b)-32:]
	offSize, refSize := int(tr[6]), int(tr[7])
	numObj := int(binary.BigEndian.Uint64(tr[8:16]))
	top := int(binary.BigEndian.Uint64(tr[16:24]))
	tableOff := int(binary.BigEndian.Uint64(tr[24:32]))
	if offSize < 1 || offSize > 8 || refSize < 1 || refSize > 8 || numObj <= 0 || numObj > 1<<20 || tableOff+numObj*offSize > len(b) {
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
	var obj func(ref, depth int) interface{}
	obj = func(ref, depth int) interface{} {
		if ref < 0 || ref >= numObj || depth > 32 {
			return nil
		}
		pos, ok := readUint(tableOff+ref*offSize, offSize)
		if !ok || pos >= len(b) {
			return nil
		}
		m := b[pos]
		typ, info := m>>4, int(m&0x0F)
		length := func() (int, int) {
			if info != 0x0F {
				return info, pos + 1
			}
			if pos+1 >= len(b) {
				return 0, len(b)
			}
			size := 1 << (b[pos+1] & 0x0F)
			n, _ := readUint(pos+2, size)
			return n, pos + 2 + size
		}
		switch typ {
		case 0x0:
			return info == 0x09
		case 0x1:
			n, _ := readUint(pos+1, 1<<info)
			return int64(n)
		case 0x2:
			if info == 3 && pos+9 <= len(b) {
				return math.Float64frombits(binary.BigEndian.Uint64(b[pos+1:]))
			}
			return 0.0
		case 0x4:
			n, st := length()
			if st+n > len(b) {
				return nil
			}
			return append([]byte(nil), b[st:st+n]...)
		case 0x5, 0x8:
			n, st := length()
			if st+n > len(b) {
				return nil
			}
			return string(b[st : st+n])
		case 0x6:
			n, st := length()
			if st+2*n > len(b) {
				return nil
			}
			u := make([]uint16, n)
			for i := range u {
				u[i] = binary.BigEndian.Uint16(b[st+2*i:])
			}
			return string(utf16.Decode(u))
		case 0xA:
			n, st := length()
			arr := make([]interface{}, 0, n)
			for i := 0; i < n; i++ {
				r, ok := readUint(st+i*refSize, refSize)
				if !ok {
					break
				}
				arr = append(arr, obj(r, depth+1))
			}
			return arr
		case 0xD:
			n, st := length()
			mm := map[string]interface{}{}
			for i := 0; i < n; i++ {
				kr, ok1 := readUint(st+i*refSize, refSize)
				vr, ok2 := readUint(st+(n+i)*refSize, refSize)
				if !ok1 || !ok2 {
					break
				}
				if k, ok := obj(kr, depth+1).(string); ok {
					mm[k] = obj(vr, depth+1)
				}
			}
			return mm
		}
		return nil
	}
	return obj(top, 0), nil
}

func plistString(m map[string]interface{}, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	}
	return ""
}

// extractXML вырезает plist из вывода утилиты, где до него могут быть посторонние строки.
func extractXML(out string) []byte {
	start := strings.Index(out, "<?xml")
	if start < 0 {
		start = strings.Index(out, "<plist")
	}
	end := strings.LastIndex(out, "</plist>")
	if start < 0 || end < 0 {
		return nil
	}
	return []byte(out[start : end+len("</plist>")])
}

var _ = io.EOF
