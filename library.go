package main

// Индекс библиотеки: какой файл .ipa относится к какому приложению (ID из App Store).
// Нужен, чтобы в списках показывать «В библиотеке» и сразу предлагать установку.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type libIndex struct {
	mu   sync.Mutex
	path string
	dir  string
	m    map[string][]string // id -> имена файлов
}

func loadLibIndex(dir, cfg string) *libIndex {
	li := &libIndex{path: filepath.Join(cfg, "library.json"), dir: dir, m: map[string][]string{}}
	if data, err := os.ReadFile(li.path); err == nil {
		_ = json.Unmarshal(data, &li.m)
	}
	return li
}

func (li *libIndex) save() {
	data, _ := json.MarshalIndent(li.m, "", " ")
	_ = os.WriteFile(li.path, data, 0o644)
}

func (li *libIndex) Add(id, file string) {
	li.mu.Lock()
	defer li.mu.Unlock()
	for _, f := range li.m[id] {
		if f == file {
			return
		}
	}
	li.m[id] = append(li.m[id], file)
	li.save()
}

// Snapshot — только реально существующие файлы.
func (li *libIndex) Snapshot() (byID map[string][]string, byFile map[string]string) {
	li.mu.Lock()
	defer li.mu.Unlock()
	byID, byFile = map[string][]string{}, map[string]string{}
	changed := false
	for id, files := range li.m {
		var keep []string
		for _, f := range files {
			if _, err := os.Stat(filepath.Join(li.dir, f)); err == nil {
				keep = append(keep, f)
				byFile[f] = id
			} else {
				changed = true
			}
		}
		if len(keep) > 0 {
			byID[id] = keep
		}
	}
	if changed {
		li.m = byID
		li.save()
	}
	return
}
