package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Package the real CLI using the same version, key encoding, archive layout and
// signed manifest as the release workflow. Network traffic stays on loopback.
func TestPackagedUpgrade(t *testing.T) {
	if os.Getenv("TRANSCOOP_UPDATE_INTEGRATION") != "1" {
		t.Skip("set TRANSCOOP_UPDATE_INTEGRATION=1 to build the two fixture releases")
	}
	const from, to = "v0.1.0-alpha.9", "v0.1.0-alpha.10"
	name := archiveName(to)
	base := "/download/" + to + "/"
	fixtures := map[string][]byte{}
	var mu sync.RWMutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		data, ok := fixtures[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	defer server.Close()
	key, private := signingKey(t)
	dir := t.TempDir()
	bin := binaryName("transcoop")
	installed := filepath.Join(dir, "install with spaces", bin)
	next := filepath.Join(dir, "next", bin)
	build := func(path, version string) {
		t.Helper()
		const pkg = "github.com/friesencr/transcoop/internal/update."
		flags := fmt.Sprintf("-X main.version=%s -X %sPublicKey=%s -X %sapiURL=%s/releases -X %sdownloadURL=%s/download", version, pkg, key, pkg, server.URL, pkg, server.URL)
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", flags, "-o", path, "./cmd/transcoop")
		cmd.Dir = filepath.Join("..", "..")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
	}
	build(installed, from)
	build(next, to)
	b, err := os.ReadFile(next)
	if err != nil {
		t.Fatal(err)
	}
	member := strings.TrimSuffix(strings.TrimSuffix(name, ".zip"), ".tar.gz") + "/" + bin
	var archive bytes.Buffer
	if runtime.GOOS == "windows" {
		z := zip.NewWriter(&archive)
		f, err := z.Create(member)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(b); err != nil {
			t.Fatal(err)
		}
		if err = z.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		g := gzip.NewWriter(&archive)
		w := tar.NewWriter(g)
		if err := w.WriteHeader(&tar.Header{Name: member, Size: int64(len(b)), Mode: 0755}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := g.Close(); err != nil {
			t.Fatal(err)
		}
	}
	manifest := fmt.Appendf(nil, "%x  %s\n", sha256.Sum256(archive.Bytes()), name)
	list := func(names ...string) []byte {
		r := release{Tag: to, Prerelease: true}
		for _, s := range names {
			r.Assets = append(r.Assets, struct{ Name string }{s})
		}
		b, err := json.Marshal([]release{r})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	put := func(path string, data []byte) {
		mu.Lock()
		defer mu.Unlock()
		fixtures[path] = data
	}
	put("/releases", list())
	cmd := func(args ...string) (string, error) {
		c := exec.Command(installed, args...)
		// An unrelated transcoop-fs in the developer's PATH must not change this fixture.
		c.Env = append(os.Environ(), "PATH="+filepath.Dir(installed))
		c.WaitDelay = 60 * time.Second
		out, err := c.CombinedOutput()
		return string(out), err
	}
	check := func(args []string, want string, success bool) {
		t.Helper()
		out, err := cmd(args...)
		if (err == nil) != success || !strings.Contains(out, want) {
			t.Fatalf("%v: got %q err=%v; want %q", args, out, err, want)
		}
	}
	config := filepath.Join(dir, "worker.toml")
	if err := os.WriteFile(config, []byte("[worker]\nmode='transfer'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"update", "-config", config}
	check([]string{"update", "--check"}, "no archive", false)
	put("/releases", list(name, "SHA256SUMS"))
	check([]string{"update", "--check"}, "missing required release asset SHA256SUMS.sig", false)
	put("/releases", list(name, "SHA256SUMS", "SHA256SUMS.sig"))
	check([]string{"update", "--check"}, to, true)
	if files, err := os.ReadDir(filepath.Dir(installed)); err != nil || len(files) != 1 {
		t.Fatalf("check wrote install files: %v %v", files, err)
	}
	fuse := filepath.Join(dir, "fuse.toml")
	if err := os.WriteFile(fuse, []byte("[worker]\nmode='fuse'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	check([]string{"update", "-config", fuse}, "matching transcoop-fs", false)
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		t.Run("read-only directory", func(t *testing.T) {
			d := filepath.Dir(installed)
			if err := os.Chmod(d, 0555); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(d, 0755)
			out, err := cmd(args...)
			if err == nil || !strings.Contains(out, "permissions") {
				t.Fatalf("update in read-only directory: %v, %s", err, out)
			}
		})
	}
	put(base+"SHA256SUMS", manifest)
	put(base+"SHA256SUMS.sig", make([]byte, 64))
	check(args, "signature verification failed", false)
	put(base+"SHA256SUMS.sig", ed25519.Sign(private, manifest))
	put(base+name, archive.Bytes()[:archive.Len()/2])
	check(args, "checksum verification failed", false)
	check([]string{"version"}, from, true)
	put(base+name, archive.Bytes())
	if runtime.GOOS == "windows" {
		// A handle without delete sharing reproduces an in-use destination.
		held, err := os.Open(installed)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		check(args, "Stop running Transcoop", true) // The launcher returns after handing off.
		if err := held.Close(); err != nil {
			t.Fatal(err)
		}
		check([]string{"version"}, from, true)
	}
	check(args, "Installed "+to, true)
	check([]string{"version"}, to, true)
	check([]string{"update", "--check"}, "Already up to date", true)
	// An old process waiting for the lock must never replace a newer install.
	if err := install(t.Context(), installed, from, release{Tag: to}, name, key); err == nil || !strings.Contains(err.Error(), "installed executable changed") {
		t.Fatalf("stale updater was not rejected: %v", err)
	}
	after, err := os.ReadFile(config)
	if err != nil || string(after) != "[worker]\nmode='transfer'\n" {
		t.Fatalf("config changed: %s %v", after, err)
	}
	f, err := os.Open(installed)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(b)
	if !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatal("installed bytes differ from signed release")
	}
}
