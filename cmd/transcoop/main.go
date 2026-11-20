package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/friesencr/transcoop/internal/config"

	"github.com/friesencr/transcoop/internal/farm"
	"github.com/friesencr/transcoop/internal/review"
	"github.com/friesencr/transcoop/internal/update"
	"github.com/friesencr/transcoop/internal/watch"
	"github.com/friesencr/transcoop/internal/webui"
)

const defaultPort = "8765"
const defaultListen = "127.0.0.1:" + defaultPort
const defaultServer = "http://" + defaultListen

// version is set at release build time with -ldflags "-X main.version=v0.1.0-alpha.1".
var version = ""

func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`transcoop ` + buildVersion() + `: a self-hosted HandBrake encoding farm

  setup                    Configure a coordinator or worker
  config init|check        Create or validate TOML settings
  serve                    Start the coordinator and browser console
  worker                   Start an encoding worker
  service install|uninstall|status
                           Run serve or worker at boot (Linux, systemd)
  pair -role ROLE          Issue a browser, worker, or operator pairing code
  join -server URL         Pair this CLI with a coordinator
  devices list|revoke ID   Manage paired devices
  import queue.json        Import a HandBrake queue
  jobs                     List jobs
  retry JOB_ID             Retry a failed or interrupted job
  tui [options] INPUT      Review a queue or folder
  update [--check]         Check for or install a signed release
  version                  Print the version

All commands accept -config FILE. Explicit flags override TOML settings.
Worker scheduling: -scheduler fixed|auto -max-jobs N -cpu-target 85.
Run a command with -h for its flags. Flags precede positional arguments.`)
}

type mappings []farm.Mapping

func (m *mappings) String() string { return fmt.Sprint([]farm.Mapping(*m)) }
func (m *mappings) Set(value string) error {
	from, to, ok := strings.Cut(value, "=")
	if !ok || from == "" || to == "" {
		return errors.New("mapping must be coordinator-prefix=worker-prefix")
	}
	if !filepath.IsAbs(to) {
		return errors.New("worker mapping target must be an absolute path")
	}
	*m = append(*m, farm.Mapping{From: from, To: to})
	return nil
}
func run(ctx context.Context, args []string) (runErr error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage()
		return nil
	}
	if args[0] == "version" || args[0] == "-version" || args[0] == "--version" {
		fmt.Println("transcoop", buildVersion())
		return nil
	}
	path, explicit, args, err := config.Extract(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		usage()
		return nil
	}
	if args[0] == "update" {
		fs := flag.NewFlagSet("update", flag.ContinueOnError)
		check := fs.Bool("check", false, "show the available release without installing it")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: transcoop update [--check]")
		}
		fuse := false
		if !*check {
			cfg, err := config.Load(path, explicit)
			if err != nil {
				return err
			}
			fuse = cfg.Worker.Mode != nil && *cfg.Worker.Mode == "fuse"
		}
		return update.Run(ctx, buildVersion(), *check, fuse)
	}
	if args[0] == "config" {
		return configCommand(path, args[1:])
	}
	cfg, err := config.Load(path, explicit && args[0] != "setup")
	if err != nil {
		return err
	}
	if args[0] == "setup" {
		return setup(ctx, cfg, args[1:])
	}
	if args[0] == "service" {
		return serviceCommand(cfg, args[1:])
	}
	if args[0] == "pair" || args[0] == "join" || args[0] == "devices" {
		return deviceCommand(ctx, cfg, args)
	}
	command := args[0]
	if command != "serve" && command != "worker" && command != "import" && command != "jobs" && command != "retry" && command != "tui" {
		return fmt.Errorf("unknown command %q", command)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	server := fs.String("server", defaultServer, "coordinator URL")
	var logFile *string
	if command == "serve" || command == "worker" {
		logFile = fs.String("log-file", cfg.DefaultPath(config.State, filepath.Join("logs", command+".log")), "append service logs to this file and stderr (- for stderr only)")
	}
	var listen, dbPath, name, mode, encoders, workDir, handbrake, priority *string
	var maxJobs *int
	var scheduler *string
	var ui, uiSecureCookies *bool
	var cpuTarget *float64
	var preset, presetFile, outputDir, keepLanguages *string
	var recursive, firstAudio *bool
	var maps mappings
	if command == "serve" {
		handbrake = fs.String("handbrake", "HandBrakeCLI", "HandBrakeCLI executable for browser media scans")
		ui = fs.Bool("ui", true, "enable browser operator UI")
		uiSecureCookies = fs.Bool("ui-secure-cookies", false, "require HTTPS browser cookies behind a trusted TLS reverse proxy")
		listen = fs.String("listen", defaultListen, "HTTP listen address")
		dbPath = fs.String("db", cfg.DefaultPath(config.Data, "farm.db"), "local SQLite database path")
	}
	if command == "worker" {
		host, _ := os.Hostname()
		name = fs.String("name", host, "worker name")
		mode = fs.String("mode", "transfer", "shared, transfer, or fuse")
		encoders = fs.String("encoders", "x265_10bit", "comma-separated encoders supported by this worker")
		workDir = fs.String("work-dir", cfg.DefaultPath(config.Cache, "worker-data"), "local scratch and log directory")
		handbrake = fs.String("handbrake", "HandBrakeCLI", "HandBrakeCLI executable")
		maxJobs = fs.Int("max-jobs", 0, "maximum concurrent jobs (default: fixed 1, auto 4)")
		scheduler = fs.String("scheduler", "fixed", "fixed or auto (experimental CPU-driven admission)")
		cpuTarget = fs.Float64("cpu-target", 85, "automatic admission CPU target (>10..100); not a hard ceiling")
		priority = fs.String("priority", farm.DefaultPriority, "HandBrake process priority: normal, low or idle")
		fs.Var(&maps, "map", "coordinator-prefix=worker-prefix (repeatable; shared mode)")
	}
	if command == "tui" {
		preset = fs.String("preset", "", "HandBrake preset for folder input (default: imported GUI/CLI default)")
		presetFile = fs.String("preset-file", "", "import presets from this file instead of GUI configuration")
		outputDir = fs.String("output-dir", "", "destination folder for folder input")
		recursive = fs.Bool("recursive", false, "include video files in subfolders")
		keepLanguages = fs.String("keep-languages", "", "initial audio language filter, e.g. eng,jpn")
		firstAudio = fs.Bool("first-audio", false, "initially keep the first source track per language")
		handbrake = fs.String("handbrake", "HandBrakeCLI", "HandBrakeCLI executable for folder scans and queue rescans")
		fs.Var(&maps, "map", "coordinator-prefix=local-prefix for folder scans/rescans (repeatable)")
	}
	if err := cfg.Apply(command, fs); err != nil {
		return err
	}
	explicitMaps := false
	for _, arg := range args[1:] {
		if arg == "-map" || arg == "--map" || strings.HasPrefix(arg, "-map=") || strings.HasPrefix(arg, "--map=") {
			explicitMaps = true
		}
	}
	if (command == "worker" || command == "tui") && !explicitMaps {
		for _, m := range cfg.Maps(command) {
			if err := maps.Set(m); err != nil {
				return err
			}
		}
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	expected := 0
	if command == "import" || command == "retry" || command == "tui" {
		expected = 1
	}
	if fs.NArg() != expected {
		return fmt.Errorf("%s expects %d positional argument(s); put flags before arguments", command, expected)
	}
	if command == "worker" {
		if *cpuTarget <= 10 || *cpuTarget > 100 || math.IsNaN(*cpuTarget) || math.IsInf(*cpuTarget, 0) {
			return errors.New("cpu-target must be greater than 10 and at most 100")
		}
		bad := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "max-jobs" && *maxJobs <= 0 {
				bad = true
			}
		})
		if bad {
			return errors.New("max-jobs must be positive")
		}
	}
	creds, err := config.ReadCredentials(path)
	if err != nil {
		return err
	}
	serverURL, err := config.URL(*server)
	if err != nil {
		return err
	}
	token := ""
	if e, ok := creds.Clients[serverURL]; ok {
		token = e.Token
	}
	if command == "serve" {
		token = creds.Admin
	}
	if token == "" && command != "tui" {
		return errors.New("this device is not configured; run transcoop setup or transcoop join")
	}
	if logFile != nil {
		closeLog, err := setupLogging(*logFile, command)
		if err != nil {
			return err
		}
		defer func() {
			if runErr != nil {
				log.Printf("stopped with error: %v", runErr)
			} else {
				log.Print("stopped")
			}
			closeLog()
		}()
	}
	if command == "serve" {
		if _, _, err := net.SplitHostPort(*listen); err != nil {
			return fmt.Errorf("invalid listen address %q: %w", *listen, err)
		}
		if err := os.MkdirAll(filepath.Dir(*dbPath), 0700); err != nil {
			return err
		}
		store, err := farm.OpenStore(*dbPath)
		if err != nil {
			return err
		}
		defer store.Close()
		if err := store.InstallAdmin(ctx, token); err != nil {
			return err
		}
		handler := (&farm.Server{Store: store, Version: buildVersion()}).Handler()
		presetDir := filepath.Join(filepath.Dir(*dbPath), "presets")
		watcher := &watch.Watcher{Store: store, HandBrake: *handbrake, PresetDir: presetDir}
		watchCtx, stopWatch := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go func() { defer close(watchDone); watcher.Run(watchCtx) }()
		defer func() { stopWatch(); <-watchDone }()
		if *ui {
			mux := http.NewServeMux()
			mux.Handle("/api/", handler)
			browserUI := webui.New(store)
			browserUI.SecureCookies = *uiSecureCookies
			browserUI.HandBrake = *handbrake
			browserUI.Version = buildVersion()
			browserUI.PresetDir = presetDir
			browserUI.Watcher = watcher
			mux.Handle("/", browserUI.Handler())
			handler = mux
		}
		srv := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
		}()
		log.Printf("transcoop %s coordinator listening on %s; database %s", buildVersion(), *listen, *dbPath)
		if host, port, _ := net.SplitHostPort(*listen); host == "0.0.0.0" || host == "::" || host == "" {
			for _, ip := range lanAddresses() {
				log.Printf("console available at http://%s", net.JoinHostPort(ip, port))
			}
		}
		err = srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	client := farm.NewClient(serverURL, token)
	switch command {
	case "tui":
		return review.Run(ctx, review.Options{Queue: fs.Arg(0), HandBrake: *handbrake, Client: client, Mappings: maps, Preset: *preset, PresetFile: *presetFile, OutputDir: *outputDir, Recursive: *recursive, KeepLanguages: *keepLanguages, FirstAudio: *firstAudio})
	case "worker":
		dir, err := filepath.Abs(*workDir)
		if err != nil {
			return err
		}
		list := []string{}
		for _, e := range strings.Split(*encoders, ",") {
			if e = strings.TrimSpace(e); e != "" {
				list = append(list, e)
			}
		}
		fuseSettings := cfg.Worker.Fuse
		fuseSettings.MountDir = cfg.Path(fuseSettings.MountDir)
		fuseSettings.CacheDir = cfg.Path(fuseSettings.CacheDir)
		worker := farm.Worker{Fuse: fuseSettings, Client: client, Name: *name, Mode: *mode, Encoders: list, MaxJobs: *maxJobs, WorkDir: dir, HandBrake: *handbrake, Mappings: maps, Scheduler: *scheduler, CPUTarget: *cpuTarget, Version: buildVersion(), Retention: cfg.Worker.Retention, Priority: *priority}
		return worker.Run(ctx)
	case "import":
		data, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		var queue []json.RawMessage
		if err = json.Unmarshal(data, &queue); err != nil {
			return err
		}
		var result map[string]int
		_, err = client.JSON(ctx, "POST", "/api/jobs", "", queue, &result)
		if err != nil {
			return err
		}
		fmt.Printf("Imported %d new jobs\n", result["imported"])
	case "jobs":
		var jobs []farm.Job
		_, err = client.JSON(ctx, "GET", "/api/jobs", "", nil, &jobs)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(jobs)
	case "retry":
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil || id < 1 {
			return errors.New("job ID must be a positive integer")
		}
		_, err = client.JSON(ctx, "POST", fmt.Sprintf("/api/jobs/%d/retry", id), "", nil, nil)
		if err != nil {
			return err
		}
		fmt.Printf("Job %d queued for retry\n", id)
	}
	return nil
}
