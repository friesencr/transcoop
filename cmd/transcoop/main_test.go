package main

import (
	"context"
	"github.com/friesencr/transcoop/internal/config"
	"github.com/friesencr/transcoop/internal/farm"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredClientAndCredentialBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	token := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("wrong credential")
		}
		if r.URL.Path != "/api/jobs" {
			t.Error(r.URL.Path)
		}
		w.Write([]byte("[]"))
	}))
	defer server.Close()
	c := config.Config{File: path, Server: config.Ptr(server.URL)}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveCredentials(path, config.Credentials{Clients: map[string]farm.Enrollment{server.URL: {Token: token}}}); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"jobs", "-config", path}); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"jobs", "-config", path, "-server", "http://other-machine:8080"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("credentials leaked to another server: %v", err)
	}
}
func TestConfigInitDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := configCommand(path, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := configCommand(path, []string{"init"}); err == nil {
		t.Fatal("overwrote config")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("changed file")
	}
	if err := configCommand(path, []string{"check"}); err != nil {
		t.Fatal(err)
	}
}
func TestHandBrakeEncoderParsing(t *testing.T) {
	list := parseEncoders("Video options\n -e, --encoder <string> Select video encoder:\n    x264\n    x265_10bit\n --encoder-preset <string>\n   garbage\n")
	if strings.Join(list, ",") != "x264,x265_10bit" {
		t.Fatal(list)
	}
}

func TestGuidedCoordinatorSetup(t *testing.T) {
	previousOffer := offerService
	offerService = func() bool { return false }
	defer func() { offerService = previousOffer }()
	for _, tt := range []struct {
		name, existingPort, inputPort, wantPort string
	}{
		{"default", "", "", defaultPort},
		{"custom", "", "9123", "9123"},
		{"existing", "9456", "", "9456"},
		{"change existing", "9456", "9123", "9123"},
		{"retry invalid", "", "0\n65536\ninvalid\n-1\n9123", "9123"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			prefix := ""
			if tt.existingPort != "" {
				c := config.Config{File: path}
				c.Coordinator.Listen = config.Ptr("127.0.0.1:" + tt.existingPort)
				if err := c.Save(); err != nil {
					t.Fatal(err)
				}
				prefix = "yes\n"
			}
			input, err := os.CreateTemp(t.TempDir(), "input")
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			if _, err := input.WriteString(prefix + "coordinator\n127.0.0.1\n" + tt.inputPort + "\n\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := input.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			previous := os.Stdin
			os.Stdin = input
			defer func() { os.Stdin = previous }()
			if err := run(context.Background(), []string{"setup", "-config", path}); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path, true)
			wantListen := "127.0.0.1:" + tt.wantPort
			if err != nil || cfg.Server == nil || *cfg.Server != "http://"+wantListen {
				t.Fatal(cfg, err)
			}
			if cfg.Coordinator.Listen == nil || *cfg.Coordinator.Listen != wantListen {
				t.Fatal("wrong listen address", cfg.Coordinator.Listen)
			}
			creds, err := config.ReadCredentials(path)
			if err != nil || len(creds.Admin) != 64 || creds.Clients[*cfg.Server].Token != creds.Admin {
				t.Fatal("missing local credential", err)
			}
			if _, err := os.Stat(*cfg.Coordinator.DB); !os.IsNotExist(err) {
				t.Fatal("setup started or migrated coordinator")
			}
		})
	}
}
