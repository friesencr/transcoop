package main

import "testing"

func TestWithDefaultPort(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1":             "127.0.0.1:" + defaultPort,
		"127.0.0.1:9000":        "127.0.0.1:9000",
		"farm.lan":              "farm.lan:" + defaultPort,
		"::1":                   "[::1]:" + defaultPort,
		"[::1]:9000":            "[::1]:9000",
		"http://farm.lan":       "http://farm.lan",
		"https://farm.lan:8443": "https://farm.lan:8443",
		"":                      "",
	} {
		if got := withDefaultPort(in); got != want {
			t.Errorf("withDefaultPort(%q) = %q, want %q", in, got, want)
		}
	}
}
