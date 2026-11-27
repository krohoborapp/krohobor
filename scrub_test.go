package main

import "testing"

func TestScrubEcho(t *testing.T) {
	in := []string{"09:03:20 поиск", "09:03:22   Secret12.", "09:03:22   Enter password: **********", "09:03:23 ok"}
	out := scrubEcho(in)
	if len(out) != 3 || out[1] != "09:03:22   Enter password: **********" {
		t.Fatal(out)
	}
	addSecret("Pa ss!w0rd")
	if m := mask("echo Pa ss!w0rd here"); m != "echo ••• here" {
		t.Fatal(m)
	}
}
