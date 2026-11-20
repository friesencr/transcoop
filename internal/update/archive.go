package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxArchive = 128 << 20
const maxBinary = 128 << 20

func download(ctx context.Context, base, name, work, key string) (string, error) {
	manifest, err := fetch(ctx, base+"SHA256SUMS", 1<<20)
	if err != nil {
		return "", err
	}
	sig, err := fetch(ctx, base+"SHA256SUMS.sig", 64)
	if err != nil {
		return "", err
	}
	digest, err := verifyManifest(key, manifest, sig, name)
	if err != nil {
		return "", err
	}
	r, err := get(ctx, base+name)
	if err != nil {
		return "", err
	}
	defer r.Close()
	f, err := os.Create(filepath.Join(work, "archive"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(r, maxArchive+1))
	if err != nil {
		return "", err
	}
	if n > maxArchive || !bytes.Equal(hash.Sum(nil), digest) {
		return "", errors.New("archive size or SHA-256 checksum verification failed")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	binary := binaryName("transcoop")
	stage := filepath.Join(work, binary)
	member := strings.TrimSuffix(strings.TrimSuffix(name, ".tar.gz"), ".zip") + "/" + binary
	return stage, extract(f, n, member, stage)
}

// Only the expected executable is written. Archive-supplied paths and file
// permissions are never used as local paths or permissions.
func extract(f *os.File, n int64, member, dest string) error {
	if strings.HasSuffix(member, ".exe") {
		z, err := zip.NewReader(f, n)
		if err != nil {
			return err
		}
		for _, entry := range z.File {
			if entry.Name == member && entry.Mode().IsRegular() {
				r, err := entry.Open()
				if err != nil {
					return err
				}
				defer r.Close()
				return writeBinary(dest, r)
			}
		}
	} else {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		t := tar.NewReader(io.LimitReader(gz, 2*maxBinary))
		for {
			h, err := t.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if h.Name == member && h.Typeflag == tar.TypeReg {
				return writeBinary(dest, t)
			}
		}
	}
	return fmt.Errorf("release archive is missing %s", member)
}

func writeBinary(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxBinary+1))
	if err == nil && (n == 0 || n > maxBinary) {
		err = errors.New("invalid executable size")
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
