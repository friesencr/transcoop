// Package update installs signed Transcoop release archives on the local machine.
package update

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// These URLs are fixed in distributed builds. Integration tests link a pair of
// fixture binaries to a local HTTP server instead.
var apiURL = "https://api.github.com/repos/friesencr/transcoop/releases"
var downloadURL = "https://github.com/friesencr/transcoop/releases/download"

var client = &http.Client{Timeout: 5 * time.Minute}

type release struct {
	Tag        string                  `json:"tag_name"`
	Draft      bool                    `json:"draft"`
	Prerelease bool                    `json:"prerelease"`
	Assets     []struct{ Name string } `json:"assets"`
}

func validTag(tag string) bool {
	core, _, _ := strings.Cut(tag, "-")
	core, _, _ = strings.Cut(core, "+")
	return semver.IsValid(tag) && strings.Count(core, ".") == 2
}

func newest(ctx context.Context, installed string) (release, error) {
	if !validTag(installed) {
		return release{}, fmt.Errorf("this build (%s) has no release version; install a release archive to use the updater", installed)
	}
	stable := semver.Prerelease(installed) == ""
	var best release
	for page := 1; ; page++ {
		data, err := fetch(ctx, fmt.Sprintf("%s?per_page=100&page=%d", apiURL, page), 8<<20)
		if err != nil {
			return release{}, err
		}
		var releases []release
		if err = json.Unmarshal(data, &releases); err != nil {
			return release{}, fmt.Errorf("invalid GitHub release list: %w", err)
		}
		for _, r := range releases {
			if r.Draft || !validTag(r.Tag) || (stable && (r.Prerelease || semver.Prerelease(r.Tag) != "")) {
				continue
			}
			if semver.Compare(r.Tag, installed) > 0 && semver.Compare(r.Tag, best.Tag) > 0 {
				best = r
			}
		}
		if len(releases) < 100 {
			return best, nil
		}
	}
}

func archiveName(tag string) string {
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("transcoop_%s_%s_%s%s", tag, runtime.GOOS, runtime.GOARCH, ext)
}

func binaryName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func (r release) checkAssets(archive string) error {
	if !r.has(archive) {
		return fmt.Errorf("%s has no archive for this platform (%s)", r.Tag, archive)
	}
	for _, asset := range []string{"SHA256SUMS", "SHA256SUMS.sig"} {
		if !r.has(asset) {
			return fmt.Errorf("%s is missing required release asset %s", r.Tag, asset)
		}
	}
	return nil
}

func (r release) has(name string) bool {
	for _, asset := range r.Assets {
		if asset.Name == name {
			return true
		}
	}
	return false
}

func get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "transcoop-updater")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	return res.Body, nil
}

func fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	r, err := get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds size limit: %s", url)
	}
	return b, err
}

func verifyManifest(publicKey string, manifest, signature []byte, name string) ([]byte, error) {
	der, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil {
		return nil, errors.New("invalid embedded release key")
	}
	key, err := x509.ParsePKIXPublicKey(der)
	pub, ok := key.(ed25519.PublicKey)
	if err != nil || !ok || !ed25519.Verify(pub, manifest, signature) {
		return nil, errors.New("release signature verification failed")
	}
	var digest []byte
	for _, line := range strings.Split(string(manifest), "\n") {
		hash, file, ok := strings.Cut(line, "  ")
		if !ok || file != name {
			continue
		}
		if digest != nil {
			return nil, fmt.Errorf("duplicate checksum for %s", name)
		}
		digest, err = hex.DecodeString(hash)
		if err != nil || len(digest) != 32 {
			return nil, fmt.Errorf("invalid checksum for %s", name)
		}
	}
	if digest == nil {
		return nil, fmt.Errorf("no signed checksum for %s", name)
	}
	return digest, nil
}
