package main

import (
	"bytes"
	"context"
	"github.com/friesencr/transcoop/internal/config"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestServiceLogTeeAppendAndRestore(t *testing.T) {
	previous, prefix, flags := log.Writer(), log.Prefix(), log.Flags()
	defer func() { log.SetOutput(previous); log.SetPrefix(prefix); log.SetFlags(flags) }()
	var terminal bytes.Buffer
	log.SetOutput(&terminal)
	log.SetPrefix("original: ")
	path := filepath.Join(t.TempDir(), "logs", "worker.log")
	for _, message := range []string{"first launch", "second launch"} {
		closeLog, err := setupLogging(path, "worker")
		if err != nil {
			t.Fatal(err)
		}
		log.Print(message)
		closeLog()
		if log.Writer() != &terminal || log.Prefix() != "original: " || log.Flags() != flags {
			t.Fatal("logger settings not restored")
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"first launch", "second launch", "worker["} {
		if !strings.Contains(string(b), message) || !strings.Contains(terminal.String(), message) {
			t.Fatalf("missing %q in file or terminal", message)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no permission bits; the per-user profile's ACL protects it.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatal("service log is readable by other users")
	}
}

func TestServiceLogStderrOnly(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := log.Writer()
	closeLog, err := setupLogging("-", "serve")
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	if log.Writer() != previous {
		t.Fatal("stderr-only changed output")
	}
	entries, err := os.ReadDir(".")
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected files: %v (%v)", entries, err)
	}
}

func TestCommandFailurePersisted(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveCredentials(path, config.Credentials{Admin: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "serve.log")
	err := run(context.Background(), []string{"serve", "-config", path, "-listen", "invalid address", "-log-file", logPath})
	if err == nil {
		t.Fatal("expected invalid configuration to fail")
	}
	b, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(b), "stopped with error:") || !strings.Contains(string(b), "invalid address") {
		t.Fatalf("missing failure in log: %s", b)
	}
	if strings.Contains(string(b), "private-test-token") {
		t.Fatal("token in service log")
	}
}
