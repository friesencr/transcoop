package fsproto

import (
	"errors"
	"path/filepath"
)

type Settings struct {
	MountDir   string `toml:"mount_dir,omitempty"`
	CacheDir   string `toml:"cache_dir,omitempty"`
	RAMBytes   int64  `toml:"ram_bytes,omitempty"`
	DiskBytes  int64  `toml:"disk_bytes,omitempty"`
	DirtyBytes int64  `toml:"dirty_bytes,omitempty"`
	Reads      int    `toml:"read_concurrency,omitempty"`
	Writes     int    `toml:"write_concurrency,omitempty"`
}

func (s Settings) Defaults(work string) Settings {
	if s.MountDir == "" {
		s.MountDir = filepath.Join(work, "mount")
	}
	if s.CacheDir == "" {
		s.CacheDir = filepath.Join(work, "cache")
	}
	if s.RAMBytes == 0 {
		s.RAMBytes = 256 << 20
	}
	if s.DiskBytes == 0 {
		s.DiskBytes = 8 << 30
	}
	if s.DirtyBytes == 0 {
		s.DirtyBytes = 512 << 20
	}
	if s.Reads == 0 {
		s.Reads = 4
	}
	if s.Writes == 0 {
		s.Writes = 2
	}
	return s
}
func (s Settings) Validate() error {
	s = s.Defaults(".")
	if s.RAMBytes < BlockSize || s.DirtyBytes < MaxWrite*2 || s.DiskBytes < s.DirtyBytes || s.Reads < 1 || s.Reads > 64 || s.Writes < 1 || s.Writes > 64 {
		return errors.New("invalid worker.fuse limits: RAM >= 1 MiB, dirty >= 8 MiB, disk >= dirty, concurrency 1..64 required")
	}
	return nil
}

type Control struct {
	RequestID  int64    `json:"request_id"`
	Kind       string   `json:"kind"`
	Settings   Settings `json:"settings,omitempty"`
	URL        string   `json:"url,omitempty"`
	Credential string   `json:"credential,omitempty"`
	Namespace  string   `json:"namespace,omitempty"`
	JobID      int64    `json:"job_id,omitempty"`
	Attempt    string   `json:"attempt,omitempty"`
	Deadline   int64    `json:"deadline,omitempty"`
}
type Stats struct {
	ReadRate   float64 `json:"read_bytes_per_second"`
	WriteRate  float64 `json:"write_bytes_per_second"`
	Mounted    bool    `json:"mounted"`
	ReadBytes  int64   `json:"read_bytes"`
	WriteBytes int64   `json:"write_bytes"`
	CacheBytes int64   `json:"cache_bytes"`
	DirtyBytes int64   `json:"dirty_bytes"`
	Retries    int64   `json:"retries"`
	Waiting    int64   `json:"waiting"`
	Error      string  `json:"error,omitempty"`
}
type Reply struct {
	RequestID int64     `json:"request_id"`
	Error     string    `json:"error,omitempty"`
	Manifest  *Manifest `json:"manifest,omitempty"`
	Stats     Stats     `json:"stats"`
}
