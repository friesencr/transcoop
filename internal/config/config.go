// Package config owns the persistent, non-secret TOML settings and private credentials.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/friesencr/transcoop/internal/farm"
	"github.com/friesencr/transcoop/internal/fsproto"
	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Server      *string     `toml:"server,omitempty"`
	Coordinator Coordinator `toml:"coordinator,omitempty"`
	Worker      Worker      `toml:"worker,omitempty"`
	TUI         TUI         `toml:"tui,omitempty"`
	File        string      `toml:"-"`
}
type Coordinator struct {
	HandBrake     *string `toml:"handbrake,omitempty"`
	Listen        *string `toml:"listen,omitempty"`
	DB            *string `toml:"db,omitempty"`
	LogFile       *string `toml:"log_file,omitempty"`
	UI            *bool   `toml:"ui,omitempty"`
	SecureCookies *bool   `toml:"secure_cookies,omitempty"`
}
type Scheduling struct {
	Mode      *string  `toml:"mode,omitempty"`
	CPUTarget *float64 `toml:"cpu_target,omitempty"`
	MaxJobs   *int     `toml:"max_jobs,omitempty"`
}
type Worker struct {
	Fuse       fsproto.Settings `toml:"fuse,omitempty"`
	Name       *string          `toml:"name,omitempty"`
	Mode       *string          `toml:"mode,omitempty"`
	Encoders   []string         `toml:"encoders,omitempty"`
	WorkDir    *string          `toml:"work_dir,omitempty"`
	HandBrake  *string          `toml:"handbrake,omitempty"`
	LogFile    *string          `toml:"log_file,omitempty"`
	Mappings   []string         `toml:"mappings,omitempty"`
	Scheduling Scheduling       `toml:"scheduling,omitempty"`
	Retention  farm.Retention   `toml:"retention,omitempty"`
	Priority   *string          `toml:"priority,omitempty"` // HandBrake process priority: normal, low, idle
}
type TUI struct {
	HandBrake     *string  `toml:"handbrake,omitempty"`
	Preset        *string  `toml:"preset,omitempty"`
	PresetFile    *string  `toml:"preset_file,omitempty"`
	OutputDir     *string  `toml:"output_dir,omitempty"`
	KeepLanguages *string  `toml:"keep_languages,omitempty"`
	Recursive     *bool    `toml:"recursive,omitempty"`
	FirstAudio    *bool    `toml:"first_audio,omitempty"`
	Mappings      []string `toml:"mappings,omitempty"`
}

func Ptr[T any](v T) *T { return &v }
func DefaultFile() (string, error) {
	d, err := os.UserConfigDir()
	return filepath.Join(d, "transcoop", "config.toml"), err
}
func Extract(args []string) (string, bool, []string, error) {
	var path string
	var err error
	explicit := false
	out := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-config" || a == "--config" {
			i++
			if i == len(args) {
				return "", false, nil, errors.New("-config requires a file")
			}
			path = args[i]
			explicit = true
		} else if strings.HasPrefix(a, "-config=") || strings.HasPrefix(a, "--config=") {
			_, path, _ = strings.Cut(a, "=")
			explicit = true
		} else {
			out = append(out, a)
		}
	}
	if !explicit {
		path, err = DefaultFile()
		if err != nil {
			return "", false, nil, err
		}
	}
	if path == "" {
		return "", false, nil, errors.New("config path cannot be empty")
	}
	path, err = filepath.Abs(path)
	return path, explicit, out, err
}
func Load(path string, required bool) (Config, error) {
	c := Config{File: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	d := toml.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid config %s: %w", path, err)
	}
	c.File = path
	return c, c.Validate()
}
func (c Config) Validate() error {
	if err := c.Worker.Fuse.Validate(); err != nil {
		return err
	}
	if err := c.Worker.Retention.Validate(); err != nil {
		return err
	}
	if c.Worker.Priority != nil {
		if err := farm.ValidatePriority(*c.Worker.Priority); err != nil {
			return fmt.Errorf("worker.%w", err)
		}
	}
	if c.Coordinator.Listen != nil {
		_, port, err := net.SplitHostPort(*c.Coordinator.Listen)
		if err != nil {
			return fmt.Errorf("coordinator.listen: %w", err)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("coordinator.listen requires a port from 1 to 65535")
		}
	}
	for key, value := range map[string]*string{"coordinator.handbrake": c.Coordinator.HandBrake, "coordinator.db": c.Coordinator.DB, "coordinator.log_file": c.Coordinator.LogFile, "worker.name": c.Worker.Name, "worker.work_dir": c.Worker.WorkDir, "worker.handbrake": c.Worker.HandBrake, "worker.log_file": c.Worker.LogFile, "tui.handbrake": c.TUI.HandBrake} {
		if value != nil && strings.TrimSpace(*value) == "" {
			return fmt.Errorf("%s cannot be empty", key)
		}
	}

	if c.Server != nil {
		if _, err := URL(*c.Server); err != nil {
			return err
		}
	}
	if v := c.Worker.Mode; v != nil && *v != "shared" && *v != "transfer" && *v != "fuse" {
		return errors.New("worker.mode must be shared, transfer, or fuse")
	}
	if v := c.Worker.Scheduling.Mode; v != nil && *v != "fixed" && *v != "auto" {
		return errors.New("worker.scheduling.mode must be fixed or auto")
	}
	if v := c.Worker.Scheduling.MaxJobs; v != nil && *v < 1 {
		return errors.New("worker.scheduling.max_jobs must be positive")
	}
	if v := c.Worker.Scheduling.CPUTarget; v != nil && (*v <= 10 || *v > 100 || math.IsNaN(*v) || math.IsInf(*v, 0)) {
		return errors.New("worker.scheduling.cpu_target must be greater than 10 and at most 100")
	}
	if c.Worker.Name != nil && len(*c.Worker.Name) > 200 {
		return errors.New("worker.name must be at most 200 bytes")
	}
	if len(c.Worker.Encoders) > 100 {
		return errors.New("worker.encoders must contain at most 100 entries")
	}
	if c.Worker.Encoders != nil {
		if len(c.Worker.Encoders) == 0 {
			return errors.New("worker.encoders cannot be empty")
		}
		for _, v := range c.Worker.Encoders {
			if strings.TrimSpace(v) == "" {
				return errors.New("empty encoder")
			}
		}
	}
	for _, m := range append(append([]string{}, c.Worker.Mappings...), c.TUI.Mappings...) {
		from, to, ok := strings.Cut(m, "=")
		if !ok || from == "" || to == "" {
			return errors.New("mappings must be source-prefix=local-path")
		}
	}
	return nil
}
func URL(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return "", errors.New("server must be an HTTP(S) host and optional port, without credentials, query, or path")
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return "", errors.New("server port must be from 1 to 65535")
		}
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = ""
	return u.String(), nil
}
func (c Config) Path(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(c.File), p)
}

// Default location kinds for files Transcoop creates.
const (
	Data  = "data"  // databases
	Cache = "cache" // worker scratch space; safe to delete when idle
	State = "state" // logs
)

// DefaultPath returns where a file of the given kind lives when the TOML does
// not say. With the default config file it follows platform conventions
// (XDG directories on Linux), keeping large or changing files out of the
// config directory. A config chosen with -config keeps everything beside that
// file so separate installations stay self-contained.
func (c Config) DefaultPath(kind, name string) string {
	if def, err := DefaultFile(); err != nil || filepath.Clean(def) != filepath.Clean(c.File) {
		if kind == Data {
			return c.Path(filepath.Join("data", name))
		}
		return c.Path(name)
	}
	if base := platformDir(kind); base != "" {
		return filepath.Join(base, "transcoop", name)
	}
	return c.Path(name)
}

func platformDir(kind string) string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		local, err := os.UserCacheDir() // %LocalAppData%
		if err != nil {
			return ""
		}
		if kind == Cache {
			return filepath.Join(local, "transcoop", "cache")
		}
		return local
	case "darwin":
		if home == "" {
			return ""
		}
		switch kind {
		case Cache:
			return filepath.Join(home, "Library", "Caches")
		case State:
			return filepath.Join(home, "Library", "Logs")
		}
		return filepath.Join(home, "Library", "Application Support")
	}
	env, fallback := "XDG_DATA_HOME", ".local/share"
	switch kind {
	case Cache:
		env, fallback = "XDG_CACHE_HOME", ".cache"
	case State:
		env, fallback = "XDG_STATE_HOME", ".local/state"
	}
	if v := os.Getenv(env); filepath.IsAbs(v) {
		return v
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, fallback)
}
func (c Config) Executable(p string) string {
	if strings.ContainsAny(p, "/\\") {
		return c.Path(p)
	}
	return p
}

// Apply runs before flag parsing, so explicitly supplied flags override these values.
func (c Config) Apply(command string, fs *flag.FlagSet) error {
	values := map[string]string{}
	str := func(key string, p *string) {
		if p != nil {
			values[key] = *p
		}
	}
	path := func(key string, p *string) {
		if p != nil {
			if key == "log-file" && *p == "-" {
				values[key] = "-"
			} else {
				values[key] = c.Path(*p)
			}
		}
	}
	boolean := func(key string, p *bool) {
		if p != nil {
			values[key] = fmt.Sprint(*p)
		}
	}
	exe := func(p *string) {
		if p != nil {
			values["handbrake"] = c.Executable(*p)
		}
	}
	str("server", c.Server)
	switch command {
	case "serve":
		exe(c.Coordinator.HandBrake)
		str("listen", c.Coordinator.Listen)
		path("db", c.Coordinator.DB)
		path("log-file", c.Coordinator.LogFile)
		boolean("ui", c.Coordinator.UI)
		boolean("ui-secure-cookies", c.Coordinator.SecureCookies)
	case "worker":
		w := c.Worker
		str("name", w.Name)
		str("mode", w.Mode)
		path("work-dir", w.WorkDir)
		path("log-file", w.LogFile)
		exe(w.HandBrake)
		if w.Encoders != nil {
			values["encoders"] = strings.Join(w.Encoders, ",")
		}
		str("priority", w.Priority)
		str("scheduler", w.Scheduling.Mode)
		if w.Scheduling.MaxJobs != nil {
			values["max-jobs"] = fmt.Sprint(*w.Scheduling.MaxJobs)
		}
		if w.Scheduling.CPUTarget != nil {
			values["cpu-target"] = fmt.Sprint(*w.Scheduling.CPUTarget)
		}
	case "tui":
		t := c.TUI
		exe(t.HandBrake)
		str("preset", t.Preset)
		path("preset-file", t.PresetFile)
		path("output-dir", t.OutputDir)
		str("keep-languages", t.KeepLanguages)
		boolean("recursive", t.Recursive)
		boolean("first-audio", t.FirstAudio)
	}
	for k, v := range values {
		if fs.Lookup(k) != nil {
			if err := fs.Set(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}
func (c Config) Maps(command string) []string {
	m := c.Worker.Mappings
	if command == "tui" {
		m = c.TUI.Mappings
	}
	out := []string{}
	for _, v := range m {
		a, b, _ := strings.Cut(v, "=")
		out = append(out, a+"="+c.Path(b))
	}
	return out
}
func (c Config) Save() error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	return WritePrivate(c.File, b)
}
func WritePrivate(path string, data []byte) error    { return writePrivate(path, data, true) }
func WriteNewPrivate(path string, data []byte) error { return writePrivate(path, data, false) }
func writePrivate(path string, data []byte, replace bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".transcoop-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if !replace {
		return os.Link(f.Name(), path)
	}
	return os.Rename(f.Name(), path)
}

type Credentials struct {
	Admin   string                     `json:"admin,omitempty"`
	Clients map[string]farm.Enrollment `json:"clients"`
}

func CredentialsFile(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".credentials.json"
}
func ReadCredentials(path string) (Credentials, error) {
	v := Credentials{Clients: map[string]farm.Enrollment{}}
	b, err := os.ReadFile(CredentialsFile(path))
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(b, &v)
	if v.Clients == nil {
		v.Clients = map[string]farm.Enrollment{}
	}
	return v, err
}
func SaveCredentials(path string, v Credentials) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivate(CredentialsFile(path), b)
}
