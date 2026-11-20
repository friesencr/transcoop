package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/friesencr/transcoop/internal/filelock"
)

// PublicKey is the base64-encoded Ed25519 SubjectPublicKeyInfo embedded by
// the release workflow. A source build has no authority to install updates.
var PublicKey string

func Run(ctx context.Context, installed string, check, fuse bool) error {
	rel, err := newest(ctx, installed)
	if err != nil {
		return err
	}
	fmt.Println("Installed:", installed)
	if rel.Tag == "" {
		fmt.Println("Already up to date.")
		return nil
	}
	fmt.Println("Available:", rel.Tag)
	name := archiveName(rel.Tag)
	if err := rel.checkAssets(name); err != nil {
		return err
	}
	if check {
		return nil
	}
	if PublicKey == "" {
		return errors.New("this build has no release verification key; update it using its original install method")
	}
	target, err := os.Executable()
	if err != nil {
		return err
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	helper := binaryName("transcoop-fs")
	_, adjacent := os.Stat(filepath.Join(filepath.Dir(target), helper))
	_, onPath := exec.LookPath(helper)
	if fuse || adjacent == nil || onPath == nil {
		return errors.New("update this FUSE installation manually with a matching transcoop-fs helper")
	}
	return install(ctx, target, installed, rel, name, PublicKey)
}

func install(ctx context.Context, target, installed string, rel release, name, key string) error {
	lock := target + ".update.lock"
	unlock, err := filelock.Lock(lock)
	if err != nil {
		return fmt.Errorf("cannot lock the installation; check directory permissions and other updaters: %w", err)
	}
	defer unlock()
	if err := probe(ctx, target, installed); err != nil {
		return fmt.Errorf("installed executable changed: %w", err)
	}
	work, err := os.MkdirTemp(filepath.Dir(target), ".transcoop-update-")
	if err != nil {
		return fmt.Errorf("cannot stage update in install directory: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(work)
		}
	}()
	stage, err := download(ctx, downloadURL+"/"+rel.Tag+"/", name, work, key)
	if err != nil {
		return err
	}
	old, err := os.Stat(target)
	if err != nil {
		return err
	}
	if err := os.Chmod(stage, old.Mode().Perm()); err != nil {
		return err
	}
	if err := probe(ctx, stage, rel.Tag); err != nil {
		return fmt.Errorf("staged binary failed its version check: %w", err)
	}
	keep, err = apply(ctx, work, target, stage, installed, rel.Tag)
	return err
}

func probe(ctx context.Context, path, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "transcoop "+want {
		return fmt.Errorf("expected transcoop %s, got %q", want, strings.TrimSpace(string(out)))
	}
	return nil
}
