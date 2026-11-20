package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/friesencr/transcoop/internal/filelock"
)

func signingKey(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der), private
}

func TestSelectRelease(t *testing.T) {
	fullPage := make([]release, 100)
	fullPage[0] = release{Tag: "v2.0.0-alpha.10", Prerelease: true}
	fullPage[1] = release{Tag: "v5.0.0", Draft: true}
	// A stable-looking tag marked as a prerelease on GitHub is also filtered.
	fullPage[2] = release{Tag: "v1.11.0", Prerelease: true}
	fullPage[3] = release{Tag: "v9"}
	fullPage[4] = release{Tag: "v2.0.0-alpha.9", Prerelease: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stable" {
			json.NewEncoder(w).Encode([]release{{Tag: "v2.0.0"}, {Tag: "v2.0.0-alpha.10"}})
			return
		}
		if r.URL.Query().Get("page") == "1" {
			json.NewEncoder(w).Encode(fullPage)
			return
		}
		json.NewEncoder(w).Encode([]release{{Tag: "v1.9.0"}, {Tag: "v1.10.0"}})
	}))
	defer server.Close()
	old := apiURL
	apiURL = server.URL
	defer func() { apiURL = old }()
	for _, tc := range []struct{ from, to string }{
		{"v1.8.0", "v1.10.0"}, {"v1.10.0", ""}, {"v2.0.0-alpha.9", "v2.0.0-alpha.10"},
	} {
		got, err := newest(t.Context(), tc.from)
		if err != nil || got.Tag != tc.to {
			t.Fatalf("%s: got %s, %v", tc.from, got.Tag, err)
		}
	}
	apiURL = server.URL + "/stable"
	got, err := newest(t.Context(), "v2.0.0-alpha.10")
	if err != nil || got.Tag != "v2.0.0" {
		t.Fatalf("stable transition: %s, %v", got.Tag, err)
	}
	if _, err := newest(t.Context(), "dev"); err == nil {
		t.Fatal("accepted dev as a release")
	}
}

func TestSignedChecksum(t *testing.T) {
	key, private := signingKey(t)
	hash := sha256.Sum256([]byte("payload"))
	line := fmt.Sprintf("%x  archive.zip\n", hash)
	for _, tc := range []struct {
		body, signed string
		want         bool
	}{
		{line, line, true}, {line + "x", line, false}, {line + line, line + line, false},
		{"bad  archive.zip", "bad  archive.zip", false}, {line, line + "x", false}, {"", "", false},
	} {
		got, err := verifyManifest(key, []byte(tc.body), ed25519.Sign(private, []byte(tc.signed)), "archive.zip")
		if tc.want != (err == nil) || tc.want && !bytes.Equal(got, hash[:]) {
			t.Fatalf("unexpected digest %x or error %v", got, err)
		}
	}
}

func TestInstallRefusesConcurrentUpdate(t *testing.T) {
	target := filepath.Join(t.TempDir(), "transcoop")
	r := release{Tag: "v1.0.1"}
	unlock, err := filelock.Lock(target + ".update.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err = install(t.Context(), target, "v1.0.0", r, "archive", ""); err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatal(err)
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("unexpected target: %v", err)
	}
}
