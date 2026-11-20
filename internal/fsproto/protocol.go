// Package fsproto defines the versioned, attempt-scoped filesystem wire protocol.
package fsproto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"unicode/utf8"
)

const Capability = "filesystem"
const BlockSize = 1 << 20
const MaxWrite = 4 << 20

type Node struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Modified  int64  `json:"modified"`
	Directory bool   `json:"directory"`
}
type Manifest struct {
	AvailableBytes uint64            `json:"available_bytes"`
	Version        int               `json:"version"`
	Nodes          []Node            `json:"nodes"`
	Source         string            `json:"source"`
	Sidecars       map[string]string `json:"sidecars"`
	Output         string            `json:"output"`
}
type Operation struct {
	Sequence int64  `json:"sequence"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Target   string `json:"target,omitempty"`
	Offset   int64  `json:"offset,omitempty"`
	Data     []byte `json:"data,omitempty"`
	Digest   string `json:"digest"`
}

func (o Operation) Hash() string {
	o.Digest = ""
	b, _ := json.Marshal(o)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// PortableName deliberately uses the common denominator of supported platforms.
func PortableName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 255 || !utf8.ValidString(s) || strings.ContainsAny(s, "/\\<>:\"|?*\x00") || strings.TrimRight(s, " .") != s {
		return false
	}
	for _, c := range s {
		if c < 32 {
			return false
		}
	}
	base := strings.ToUpper(strings.SplitN(s, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
		return false
	}
	return true
}
func (o Operation) Validate() error {
	if o.Sequence < 1 || o.Digest != o.Hash() || o.Offset < 0 || len(o.Data) > MaxWrite || o.Offset > 1<<50 {
		return errors.New("invalid filesystem operation or digest")
	}
	if !PortableName(o.Name) || path.Base(o.Name) != o.Name {
		return errors.New("invalid output name")
	}
	switch o.Kind {
	case "create", "write", "truncate", "unlink", "flush", "seal":
	case "rename":
		if !PortableName(o.Target) {
			return errors.New("invalid rename target")
		}
	default:
		return errors.New("unsupported filesystem operation")
	}
	return nil
}
