package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/friesencr/transcoop/internal/config"
)

const serviceUsage = "usage: transcoop service install|uninstall|status [-role serve|worker] [-config FILE]"

// serviceCommand manages a systemd user unit that runs `serve` or `worker`
// at boot. User units run as the current user, so they see the same
// HandBrake, GPU access, configuration and XDG paths as a manual start.
func serviceCommand(c config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New(serviceUsage)
	}
	action := args[0]
	fs := flag.NewFlagSet("service "+action, flag.ContinueOnError)
	role := fs.String("role", "", "serve or worker (default: inferred from the config)")
	linger := fs.Bool("linger", true, "install: start at boot without logging in (loginctl enable-linger)")
	printOnly := fs.Bool("print", false, "install: print the unit instead of installing it")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New(serviceUsage)
	}
	r, err := serviceRole(c, *role)
	if err != nil {
		return err
	}
	switch action {
	case "install":
		return installService(c, r, *linger, *printOnly)
	case "uninstall":
		return uninstallService(r)
	case "status":
		if err := requireSystemd(); err != nil {
			return err
		}
		cmd := exec.Command("systemctl", "--user", "status", "--no-pager", unitName(r))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		_ = cmd.Run() // status exits non-zero for stopped units; its output says why
		return nil
	}
	return errors.New(serviceUsage)
}

// serviceRole picks serve or worker from -role or from the config's sections.
func serviceRole(c config.Config, role string) (string, error) {
	switch role {
	case "serve", "coordinator":
		return "serve", nil
	case "worker":
		return "worker", nil
	case "":
	default:
		return "", fmt.Errorf("-role must be serve or worker, not %q", role)
	}
	coordinator := c.Coordinator.Listen != nil || c.Coordinator.DB != nil
	worker := c.Worker.Name != nil || c.Worker.Mode != nil || len(c.Worker.Encoders) > 0
	switch {
	case coordinator && !worker:
		return "serve", nil
	case worker && !coordinator:
		return "worker", nil
	case coordinator && worker:
		return "", errors.New("this config sets up both a coordinator and a worker; pass -role serve or -role worker")
	}
	return "", fmt.Errorf("%s has no coordinator or worker settings; run transcoop setup first or pass -role", c.File)
}

func unitName(role string) string { return "transcoop-" + role + ".service" }

func unitDir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user"), err
}

// systemdQuote quotes one ExecStart argument (systemd.service(5) "COMMAND LINES").
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if s != "" && !strings.ContainsAny(s, " \t\"'\\;$") {
		return s
	}
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", "$$").Replace(s)
	return `"` + s + `"`
}

// unitFile renders the unit. KillMode=mixed sends SIGTERM to Transcoop only,
// so a worker can hand running jobs back to the queue before HandBrake is
// stopped; TimeoutStopSec leaves time for that report.
func unitFile(role, executable, configFile string) string {
	description := "Transcoop coordinator"
	if role == "worker" {
		description = "Transcoop encoding worker"
	}
	exec := strings.Join([]string{systemdQuote(executable), role, "-config", systemdQuote(configFile)}, " ")
	return fmt.Sprintf(`# Installed by transcoop service install. Reinstall to update; edits are overwritten.
[Unit]
Description=%s
Documentation=https://github.com/friesencr/transcoop
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=5
KillMode=mixed
TimeoutStopSec=30

[Install]
WantedBy=default.target
`, description, exec)
}

func requireSystemd() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("transcoop service supports Linux with systemd; on %s, start transcoop from a login item or scheduled task", runtime.GOOS)
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl not found; transcoop service needs systemd")
	}
	if out, err := exec.Command("systemctl", "--user", "show-environment").CombinedOutput(); err != nil {
		return fmt.Errorf("the systemd user manager is not available (%s); log in to a desktop or SSH session, or use -print and install the unit yourself", strings.TrimSpace(string(out)))
	}
	return nil
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func installService(c config.Config, role string, linger, printOnly bool) error {
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		return fmt.Errorf("cannot locate the transcoop binary: %w", err)
	}
	configFile, err := filepath.Abs(c.File)
	if err != nil {
		return err
	}
	if _, err := os.Stat(configFile); err != nil {
		return fmt.Errorf("config %s not found; run transcoop setup first", configFile)
	}
	unit := unitFile(role, executable, configFile)
	if printOnly {
		fmt.Print(unit)
		return nil
	}
	if strings.HasPrefix(executable, os.TempDir()) || strings.Contains(executable, "go-build") {
		return fmt.Errorf("%s looks temporary (go run?); install transcoop to a permanent location such as ~/.local/bin first", executable)
	}
	if err = requireSystemd(); err != nil {
		return err
	}
	if pids := manualProcesses(role); len(pids) > 0 {
		return fmt.Errorf("transcoop %s is already running outside systemd (pid %s); stop it first so the service can start", role, strings.Join(pids, ", "))
	}
	dir, err := unitDir()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, unitName(role))
	if err = os.WriteFile(path, []byte(unit), 0644); err != nil {
		return err
	}
	for _, step := range [][]string{{"daemon-reload"}, {"enable", unitName(role)}, {"restart", unitName(role)}} {
		if err = systemctl(step...); err != nil {
			return err
		}
	}
	fmt.Printf("Installed and started %s (%s)\n", unitName(role), path)
	if linger {
		enableLinger()
	}
	fmt.Printf("Status: transcoop service status -role %s · Logs: journalctl --user -u %s -f\n", role, unitName(role))
	return nil
}

// enableLinger lets user services start at boot and survive logout.
func enableLinger() {
	u, err := user.Current()
	if err != nil {
		return
	}
	out, _ := exec.Command("loginctl", "show-user", u.Username, "-p", "Linger", "--value").Output()
	if strings.TrimSpace(string(out)) == "yes" {
		fmt.Println("Starts at boot (lingering is enabled for " + u.Username + ").")
		return
	}
	if err := exec.Command("loginctl", "enable-linger", u.Username).Run(); err == nil {
		fmt.Println("Enabled lingering, so the service starts at boot without logging in.")
		return
	}
	fmt.Printf("The service starts when you log in. To start it at boot instead, run:\n  sudo loginctl enable-linger %s\n", u.Username)
}

// manualProcesses finds `transcoop <role>` processes not started by this unit.
func manualProcesses(role string) []string {
	mainPID := ""
	if out, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", unitName(role)).Output(); err == nil {
		mainPID = strings.TrimSpace(string(out))
	}
	entries, _ := os.ReadDir("/proc")
	self := strconv.Itoa(os.Getpid())
	var pids []string
	for _, e := range entries {
		pid := e.Name()
		if _, err := strconv.Atoi(pid); err != nil || pid == self || pid == mainPID {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		if len(args) >= 2 && filepath.Base(args[0]) == "transcoop" && args[1] == role {
			pids = append(pids, pid)
		}
	}
	return pids
}

func uninstallService(role string) error {
	if err := requireSystemd(); err != nil {
		return err
	}
	dir, err := unitDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, unitName(role))
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s is not installed", unitName(role))
	}
	_ = systemctl("disable", "--now", unitName(role))
	if err = os.Remove(path); err != nil {
		return err
	}
	if err = systemctl("daemon-reload"); err != nil {
		return err
	}
	fmt.Printf("Stopped and removed %s. Configuration, database and logs are kept.\n", unitName(role))
	return nil
}
