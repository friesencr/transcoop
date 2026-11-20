package config

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfigPrecedenceAndPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	text := `server = "host:8080"
[worker]
encoders = ["x264", "x265_10bit"]
work_dir = "scratch"
mappings = ["/media=mount"]
[worker.scheduling]
mode = "auto"
max_jobs = 3
cpu_target = 75
[coordinator]
ui = false
`
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	enc := fs.String("encoders", "default", "")
	dir := fs.String("work-dir", "", "")
	cap := fs.Int("max-jobs", 0, "")
	fs.String("scheduler", "fixed", "")
	fs.Float64("cpu-target", 85, "")
	if err = c.Apply("worker", fs); err != nil {
		t.Fatal(err)
	}
	if err = fs.Parse([]string{"-encoders", "x264", "-max-jobs", "2"}); err != nil {
		t.Fatal(err)
	}
	if *enc != "x264" || *cap != 2 || *dir != filepath.Join(filepath.Dir(path), "scratch") {
		t.Fatal(*enc, *cap, *dir)
	}
	if c.Maps("worker")[0] != "/media="+filepath.Join(filepath.Dir(path), "mount") {
		t.Fatal(c.Maps("worker"))
	}
	serve := flag.NewFlagSet("serve", flag.ContinueOnError)
	ui := serve.Bool("ui", true, "")
	if err = c.Apply("serve", serve); err != nil {
		t.Fatal(err)
	}
	if *ui {
		t.Fatal("explicit false lost")
	}
}
func TestInvalidSettings(t *testing.T) {
	for _, body := range []string{"unknown = 1", "[worker.scheduling]\nmax_jobs=0", "[worker.scheduling]\nmode='weighted'", "[worker.scheduling]\ncpu_target=nan", "[worker]\nmode='bad'", "server='http://a/path'", "[worker]\nmappings=['bad']", "[worker]\nencoders=[]", "[worker]\npriority='realtime'"} {
		path := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(path, []byte(body), 0600)
		if _, err := Load(path, true); err == nil {
			t.Fatal("accepted", body)
		}
	}
	path := filepath.Join(t.TempDir(), "absent")
	if _, err := Load(path, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, true); err == nil {
		t.Fatal("explicit missing file accepted")
	}
}
func TestPrivateAtomicRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	c := Config{File: path, Server: Ptr("host:8080")}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c.Server = Ptr("other:8080")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path, true)
	if err != nil || *got.Server != "other:8080" {
		t.Fatal(got, err)
	}
	if runtime.GOOS != "windows" {
		stat, _ := os.Stat(path)
		if stat.Mode().Perm() != 0600 {
			t.Fatal(stat.Mode())
		}
	}
	if err := SaveCredentials(path, Credentials{Admin: "private"}); err != nil {
		t.Fatal(err)
	}
	v, err := ReadCredentials(path)
	if err != nil || v.Admin != "private" {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 2 {
		t.Fatal("temporary files leaked", entries)
	}
}

func TestFuseConfigurationDefaultsAndLimits(t *testing.T) {
	c := Config{Worker: Worker{Mode: Ptr("fuse")}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	defaults := c.Worker.Fuse.Defaults("work")
	if defaults.RAMBytes != 256<<20 || defaults.DiskBytes != 8<<30 || defaults.DirtyBytes != 512<<20 || defaults.Reads != 4 || defaults.Writes != 2 {
		t.Fatal(defaults)
	}
	c.Worker.Fuse.DiskBytes = 1
	if err := c.Validate(); err == nil {
		t.Fatal("invalid disk limit accepted")
	}
	c.Worker.Fuse.DiskBytes = 0
	c.Worker.Fuse.Reads = -1
	if err := c.Validate(); err == nil {
		t.Fatal("invalid concurrency accepted")
	}
}

func TestDefaultPathFollowsPlatformForDefaultConfig(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG layout is Linux-specific")
	}
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	def, err := DefaultFile()
	if err != nil {
		t.Fatal(err)
	}
	c := Config{File: def}
	for kind, want := range map[string]string{
		Data:  filepath.Join(root, "data", "transcoop", "farm.db"),
		Cache: filepath.Join(root, "cache", "transcoop", "farm.db"),
		State: filepath.Join(root, "state", "transcoop", "farm.db"),
	} {
		if got := c.DefaultPath(kind, "farm.db"); got != want {
			t.Errorf("%s: got %s want %s", kind, got, want)
		}
	}
	explicit := Config{File: filepath.Join(root, "elsewhere", "config.toml")}
	if got := explicit.DefaultPath(Data, "farm.db"); got != filepath.Join(root, "elsewhere", "data", "farm.db") {
		t.Errorf("explicit data: %s", got)
	}
	if got := explicit.DefaultPath(Cache, "worker-data"); got != filepath.Join(root, "elsewhere", "worker-data") {
		t.Errorf("explicit cache: %s", got)
	}
}

func TestSaveOmitsEmptySections(t *testing.T) {
	c := Config{File: filepath.Join(t.TempDir(), "config.toml"), Server: Ptr("http://127.0.0.1:8765")}
	c.Coordinator.Listen = Ptr("127.0.0.1:8765")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(c.File)
	for _, section := range []string{"[worker", "[tui]"} {
		if strings.Contains(string(b), section) {
			t.Fatalf("empty %s section written:\n%s", section, b)
		}
	}
	if _, err := Load(c.File, true); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRetentionSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[worker.retention]\ndays = 30\nattempts = 20\nfailed_output_days = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Worker.Retention; r.Days != 30 || r.Attempts != 20 || r.FailedOutputDays != 1 {
		t.Fatalf("retention: %+v", r)
	}
	if err := os.WriteFile(path, []byte("[worker.retention]\ndays = 1\nfailed_output_days = 3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, true); err == nil {
		t.Fatal("failed_output_days beyond days accepted")
	}
}
