package main

import (
	"os"
	"strings"
	"testing"
)

func TestSearch(t *testing.T) {
	data, _ := os.ReadFile("catalog.txt")
	cat, _ := parseEntries(strings.NewReader(string(data)), "catalog")
	c := &Catalog{catalog: cat}
	for _, q := range []string{"т-банк", "тбанк", "tbank", "T Bank", "тинькофф", "nbymrjaa", "тинькоф", "сбербнак", "sber", "ыиук", "макс", "max", "ечин", "yaqin", "альфабанк", "госуслуги", "gosuslugi", "вб", "озон", "ozon", "втб", "кинопоиск"} {
		h := c.Search(q, 3)
		var names []string
		for _, x := range h {
			names = append(names, x.Name)
		}
		t.Logf("%-12s → %s", q, strings.Join(names, " | "))
	}
	for _, l := range []string{"https://apps.apple.com/ru/app/max/id6739530834", "https://apps.apple.com/ru/app/id6812428354?l=en", "просто текст"} {
		t.Logf("%s → %q", l, idFromLink(l))
	}
}
