package main

import (
	"strings"
	"testing"

	"github.com/friesencr/transcoop/internal/config"
)

func TestServiceRole(t *testing.T) {
	s := "x"
	for _, c := range []struct {
		cfg  config.Config
		flag string
		want string
	}{
		{config.Config{Coordinator: config.Coordinator{Listen: &s}}, "", "serve"},
		{config.Config{Worker: config.Worker{Name: &s}}, "", "worker"},
		{config.Config{Worker: config.Worker{Name: &s}}, "coordinator", "serve"},
		{config.Config{Coordinator: config.Coordinator{Listen: &s}, Worker: config.Worker{Mode: &s}}, "worker", "worker"},
	} {
		if got, err := serviceRole(c.cfg, c.flag); err != nil || got != c.want {
			t.Errorf("got %q %v, want %q", got, err, c.want)
		}
	}
	both := config.Config{Coordinator: config.Coordinator{Listen: &s}, Worker: config.Worker{Mode: &s}}
	for _, c := range []struct {
		cfg  config.Config
		flag string
	}{{both, ""}, {config.Config{}, ""}, {config.Config{}, "tui"}} {
		if _, err := serviceRole(c.cfg, c.flag); err == nil {
			t.Errorf("accepted %+v %q", c.cfg, c.flag)
		}
	}
}

func TestUnitFile(t *testing.T) {
	unit := unitFile("worker", "/home/me/.local/bin/transcoop", "/home/me/My Configs/100% transcoop.toml")
	for _, want := range []string{
		"Description=Transcoop encoding worker",
		`ExecStart=/home/me/.local/bin/transcoop worker -config "/home/me/My Configs/100%% transcoop.toml"`,
		"Restart=on-failure",
		"KillMode=mixed",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
	for in, want := range map[string]string{`/a/b`: `/a/b`, `/a b`: `"/a b"`, `/a"b`: `"/a\"b"`, `/a$b`: `"/a$$b"`, `/a\b`: `"/a\\b"`} {
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
