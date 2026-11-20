package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/friesencr/transcoop/internal/config"
	"github.com/friesencr/transcoop/internal/farm"
)

func configCommand(path string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: transcoop config init|check [-config FILE]")
	}
	switch args[0] {
	case "check":
		c, err := config.Load(path, true)
		if err != nil {
			return err
		}
		if err = c.Validate(); err != nil {
			return err
		}
		fmt.Println("Configuration valid:", path)
		return nil
	case "init":
		if _, err := os.Lstat(path); err == nil {
			return errors.New("config already exists; edit it or select another -config file")
		} else if !os.IsNotExist(err) {
			return err
		}
		example := `# Run transcoop setup to configure and pair this installation.
server = "` + defaultServer + `"

[coordinator]
listen = "` + defaultListen + `"
# db = "data/farm.db"  # default: the platform data directory
ui = true

[worker]
mode = "transfer"
encoders = ["x265_10bit"]
# work_dir = "worker-data"  # default: the platform cache directory
handbrake = "HandBrakeCLI"
# HandBrake process priority: normal, low (keeps this machine responsive) or idle.
priority = "low"

[worker.scheduling]
# auto is experimental; an omitted max_jobs defaults to 4 for auto, 1 for fixed.
mode = "fixed"
cpu_target = 85
# max_jobs = 4

# Finished attempt folders in work_dir; these are the defaults.
[worker.retention]
days = 14
attempts = 100
failed_output_days = 2
`
		if err := config.WriteNewPrivate(path, []byte(example)); err != nil {
			return err
		}
		fmt.Println("Created", path)
		return nil
	default:
		return errors.New("usage: config init|check")
	}
}

type prompter struct {
	ctx context.Context
	in  *bufio.Reader
}

func (p prompter) ask(label, def string) (string, error) {
	if err := p.ctx.Err(); err != nil {
		return "", err
	}
	fmt.Printf("%s", label)
	if def != "" {
		fmt.Printf(" [%s]", def)
	}
	fmt.Print(": ")
	type input struct {
		line string
		err  error
	}
	ready := make(chan input, 1)
	// Stdin may be a terminal or a pipe with no pending input. Let cancellation
	// return without waiting for a newline. The buffered result lets the reader
	// finish if stdin later closes; the CLI exits after cancellation.
	go func() { line, err := p.in.ReadString('\n'); ready <- input{line, err} }()
	var result input
	select {
	case <-p.ctx.Done():
		return "", p.ctx.Err()
	case result = <-ready:
	}
	if err := p.ctx.Err(); err != nil {
		return "", err
	}
	if result.err != nil {
		return "", fmt.Errorf("setup needs interactive input: %w", result.err)
	}
	line := result.line
	line = strings.TrimSpace(line)
	if line == "" {
		line = def
	}
	if line == "" {
		return "", errors.New(label + " is required")
	}
	return line, nil
}
func newSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// offerService reports whether setup can offer a systemd service; tests disable it.
var offerService = func() bool { return requireSystemd() == nil }

func setup(ctx context.Context, c config.Config, args []string) error {
	if len(args) > 0 {
		return errors.New("usage: transcoop setup [-config FILE]")
	}
	p := prompter{ctx: ctx, in: bufio.NewReader(os.Stdin)}
	if _, err := os.Stat(c.File); err == nil {
		v, err := p.ask("Update the existing configuration? (yes/no)", "no")
		if err != nil {
			return err
		}
		if v != "yes" {
			return errors.New("setup canceled; configuration unchanged")
		}
	}
	role, err := p.ask("Set up coordinator or worker", "coordinator")
	if err != nil {
		return err
	}
	creds, err := config.ReadCredentials(c.File)
	if err != nil {
		return err
	}
	switch role {
	case "coordinator":
		lan := lanAddresses()
		choices := append([]string{"0.0.0.0", "127.0.0.1"}, lan...)
		fmt.Println("Available addresses:", strings.Join(choices, ", "))
		fmt.Println("0.0.0.0 listens on every interface; 127.0.0.1 allows only this machine.")
		bindDefault := ""
		if len(lan) > 0 {
			bindDefault = lan[0]
		}
		bind, err := p.ask("Address to listen on (select a LAN address for other machines)", bindDefault)
		if err != nil {
			return err
		}
		valid := false
		for _, v := range choices {
			if bind == v {
				valid = true
			}
		}
		if !valid {
			return errors.New("choose an address from the displayed list")
		}
		portDefault := defaultPort
		if c.Coordinator.Listen != nil {
			if _, port, err := net.SplitHostPort(*c.Coordinator.Listen); err == nil {
				portDefault = port
			}
		}
		var port string
		for {
			port, err = p.ask("Port to listen on", portDefault)
			if err != nil {
				return err
			}
			n, parseErr := strconv.Atoi(port)
			if parseErr == nil && n >= 1 && n <= 65535 {
				port = strconv.Itoa(n)
				break
			}
			fmt.Println("Choose a port from 1 to 65535.")
		}
		listen := net.JoinHostPort(bind, port)
		// The local CLI reaches an all-interfaces listener through loopback.
		server := "http://" + listen
		if bind == "0.0.0.0" {
			server = "http://" + net.JoinHostPort("127.0.0.1", port)
		}
		dbDefault := c.DefaultPath(config.Data, "farm.db")
		if c.Coordinator.DB != nil {
			dbDefault = c.Path(*c.Coordinator.DB)
		}
		db, err := p.ask("Database path (existing job history will be migrated)", dbDefault)
		if err != nil {
			return err
		}
		c.Server = config.Ptr(server)
		c.Coordinator.Listen = config.Ptr(listen)
		c.Coordinator.DB = config.Ptr(c.Path(db))
		c.Coordinator.UI = config.Ptr(true)
		c.Coordinator.SecureCookies = config.Ptr(false)
		if creds.Admin == "" {
			creds.Admin = newSecret()
		}
		creds.Clients[server] = farm.Enrollment{Device: farm.Device{Role: "admin", Name: "Local administrator"}, Token: creds.Admin}
		fmt.Println("LAN HTTP authenticates devices but does not encrypt traffic.")
		if bind != "127.0.0.1" {
			if hint := firewallHint(port); hint != "" {
				fmt.Println(hint)
			}
		}
	case "worker":
		def := ""
		if c.Server != nil {
			def = *c.Server
		}
		var server string
		for {
			raw, err := p.ask("Coordinator address", def)
			if err != nil {
				return err
			}
			server, err = config.URL(withDefaultPort(raw))
			if err == nil {
				var version string
				if version, err = checkCoordinator(ctx, server); err == nil {
					fmt.Println("Coordinator reachable:", version)
					break
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Printf("Cannot use %s: %v\n", raw, err)
			if errors.Is(err, errRefused) {
				if alt := localCoordinator(ctx, server); alt != "" {
					fmt.Printf("A coordinator on this machine answers at %s; it is not listening on loopback.\n", alt)
					def = alt
					continue
				}
			}
			fmt.Println("Check the address and port, that the coordinator is running, and that its firewall allows this port.")
			def = raw
		}
		c.Server = config.Ptr(server)
		executable := "HandBrakeCLI"
		if c.Worker.HandBrake != nil {
			executable = c.Executable(*c.Worker.HandBrake)
		}
		executable, err = p.ask("HandBrakeCLI executable", executable)
		if err != nil {
			return err
		}
		executable = c.Executable(executable)
		version, available, err := detectHandBrake(ctx, executable)
		if err != nil {
			return err
		}
		fmt.Println("HandBrake:", version)
		enc, err := selectWorkerEncoders(ctx, executable, available, probeHardwareEncoder)
		if err != nil {
			return err
		}
		fmt.Println("Worker will accept:", strings.Join(enc, ", "))
		fmt.Println("To restrict encoders later, edit worker.encoders in the config file.")
		host, _ := os.Hostname()
		name, err := p.ask("Worker name", host)
		if err != nil {
			return err
		}
		work, err := p.ask("Scratch directory", c.DefaultPath(config.Cache, "worker-data"))
		if err != nil {
			return err
		}
		work = c.Path(work)
		if err = os.MkdirAll(work, 0700); err != nil {
			return err
		}
		probe, err := os.CreateTemp(work, ".write-check-*")
		if err != nil {
			return err
		}
		probe.Close()
		os.Remove(probe.Name())
		fmt.Println("transfer downloads each source; shared uses storage mounted on both machines; fuse (experimental) streams through a mount.")
		recommended := "transfer"
		probeErr := farm.ProbeFuse(ctx, work)
		if probeErr == nil {
			recommended = "fuse"
		}
		var mode string
		for {
			mode, err = p.ask("Storage mode: transfer, shared, or fuse", recommended)
			if err != nil {
				return err
			}
			if mode == "fuse" && probeErr != nil {
				fmt.Printf("FUSE is unavailable on this machine: %v\n", probeErr)
				continue
			}
			break
		}
		c.Worker.Mappings = nil
		if mode == "shared" {
			m, err := p.ask("Path mappings, comma-separated FROM=TO (or none)", "none")
			if err != nil {
				return err
			}
			if m != "none" {
				c.Worker.Mappings = strings.Split(m, ",")
			}
		}
		schedule, err := p.ask("Scheduling: fixed or auto (experimental)", "fixed")
		if err != nil {
			return err
		}
		capDefault := "1"
		if schedule == "auto" {
			capDefault = "4"
		}
		capText, err := p.ask("Maximum concurrent jobs", capDefault)
		if err != nil {
			return err
		}
		var cap int
		if _, err = fmt.Sscan(capText, &cap); err != nil || cap < 1 {
			return errors.New("maximum jobs must be positive")
		}
		target := 85.0
		if schedule == "auto" {
			v, err := p.ask("CPU target (admission threshold, not a hard ceiling)", "85")
			if err != nil {
				return err
			}
			if _, err = fmt.Sscan(v, &target); err != nil {
				return err
			}
		}
		c.Worker.Name = config.Ptr(name)
		c.Worker.HandBrake = config.Ptr(executable)
		c.Worker.Encoders = enc
		c.Worker.WorkDir = config.Ptr(work)
		c.Worker.Mode = config.Ptr(mode)
		c.Worker.Scheduling = config.Scheduling{Mode: config.Ptr(schedule), MaxJobs: config.Ptr(cap), CPUTarget: config.Ptr(target)}
		if err = c.Validate(); err != nil {
			return err
		}
		var e farm.Enrollment
		for attempt := 1; ; attempt++ {
			code, err := p.ask("Worker pairing code from the coordinator console", "")
			if err != nil {
				return err
			}
			if e, err = redeem(ctx, server, "worker", strings.TrimSpace(code), name); err == nil {
				break
			}
			if ctx.Err() != nil || attempt == 5 {
				return err
			}
			fmt.Println("That code was not accepted. Codes expire after ten minutes; create a new one on the Devices page if needed.")
		}
		creds.Clients[server] = e
	default:
		return errors.New("choose coordinator or worker")
	}
	if err = c.Validate(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = config.SaveCredentials(c.File, creds); err != nil {
		return err
	}
	if err = c.Save(); err != nil {
		return err
	}
	cmd := "worker"
	if role == "coordinator" {
		cmd = "serve"
	}
	fmt.Printf("Saved %s\n", c.File)
	installed := false
	if offerService() {
		v, err := p.ask("Start transcoop "+cmd+" now and at every boot with systemd? (yes/no)", "yes")
		if err != nil {
			return err
		}
		if v == "yes" {
			if err := installService(c, cmd, true, false); err != nil {
				fmt.Println("Could not install the service:", err)
			} else {
				installed = true
			}
		}
	}
	if !installed {
		fmt.Printf("Start: transcoop %s -config %q\n", cmd, c.File)
	}
	if role == "coordinator" {
		open := *c.Server
		if host, port, _ := net.SplitHostPort(*c.Coordinator.Listen); host == "0.0.0.0" {
			if lan := lanAddresses(); len(lan) > 0 {
				open = "http://" + net.JoinHostPort(lan[0], port)
			}
		}
		fmt.Printf("Open %s, then issue a code with: transcoop pair -role browser -config %q\n", open, c.File)
	} else {
		fmt.Println("Worker ready: HandBrake, scratch storage, and pairing verified.")
	}
	return nil
}
func detectHandBrake(ctx context.Context, executable string) (string, []string, error) {
	path, err := exec.LookPath(executable)
	if err != nil {
		return "", nil, fmt.Errorf("install HandBrakeCLI or supply its executable path: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	version, err := farm.HandBrakeVersion(ctx, path)
	if err != nil {
		return "", nil, err
	}
	help, err := exec.CommandContext(ctx, path, "--help").CombinedOutput()
	if err != nil {
		return "", nil, err
	}
	encoders := parseEncoders(string(help))
	if len(encoders) == 0 {
		return "", nil, errors.New("could not read HandBrake video encoders from --help")
	}
	return version, encoders, nil
}
func parseEncoders(help string) []string {
	result := []string{}
	in := false
	for _, line := range strings.Split(help, "\n") {
		if strings.Contains(line, "--encoder <") {
			in = true
			continue
		}
		if !in {
			continue
		}
		v := strings.TrimSpace(line)
		if strings.HasPrefix(v, "-") {
			break
		}
		if v != "" && !strings.ContainsAny(v, " \t:") {
			result = append(result, v)
		}
	}
	return result
}
func redeem(ctx context.Context, server, role, code, name string) (farm.Enrollment, error) {
	var e farm.Enrollment
	_, err := farm.NewClient(server, "").JSON(ctx, "POST", "/api/pairing/redeem", "", map[string]string{"role": role, "code": code, "name": name}, &e)
	return e, err
}
func deviceCommand(ctx context.Context, c config.Config, args []string) error {
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	server := fs.String("server", defaultServer, "coordinator URL")
	role := fs.String("role", "operator", "browser, worker, or operator")
	name := fs.String("name", "", "device label")
	jsonOut := fs.Bool("json", false, "pair: print the code as JSON for scripts")
	if err := c.Apply(command, fs); err != nil {
		return err
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	u, err := config.URL(*server)
	if err != nil {
		return err
	}
	creds, err := config.ReadCredentials(c.File)
	if err != nil {
		return err
	}
	if command == "join" {
		if *role == "browser" {
			return errors.New("pair browsers through the console")
		}
		p := prompter{ctx: ctx, in: bufio.NewReader(os.Stdin)}
		if *name == "" {
			host, _ := os.Hostname()
			*name, err = p.ask("Device name", host)
			if err != nil {
				return err
			}
		}
		code, err := p.ask("Pairing code", "")
		if err != nil {
			return err
		}
		e, err := redeem(ctx, u, *role, code, *name)
		if err != nil {
			return err
		}
		creds.Clients[u] = e
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = config.SaveCredentials(c.File, creds); err != nil {
			return err
		}
		c.Server = config.Ptr(u)
		if err = c.Save(); err != nil {
			return err
		}
		fmt.Println("Paired", e.Device.Name, "with", u)
		return nil
	}
	token := creds.Clients[u].Token
	if token == "" {
		return errors.New("run setup or join to pair an operator device")
	}
	client := farm.NewClient(u, token)
	if command == "pair" {
		var p farm.Pairing
		if _, err = client.JSON(ctx, "POST", "/api/pairing", "", map[string]string{"role": *role}, &p); err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(os.Stdout).Encode(map[string]string{"role": p.Role, "code": p.Code, "expires": time.Unix(p.Expires, 0).Format(time.RFC3339)})
		}
		fmt.Printf("%s pairing code: %s\nExpires: %s\n", p.Role, p.Code, time.Unix(p.Expires, 0).Format(time.RFC3339))
		return nil
	}
	if fs.NArg() == 1 && fs.Arg(0) == "list" {
		var devices []farm.Device
		if _, err = client.JSON(ctx, "GET", "/api/devices", "", nil, &devices); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(devices)
	}
	if fs.NArg() == 2 && fs.Arg(0) == "revoke" {
		_, err = client.JSON(ctx, "DELETE", "/api/devices/"+fs.Arg(1), "", nil, nil)
		return err
	}
	return errors.New("usage: devices [-server URL] list|revoke ID")
}

// lanAddresses lists this machine's non-loopback IPv4 addresses.
func lanAddresses() []string {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := []string{}
	for _, a := range addresses {
		ip, _, e := net.ParseCIDR(a.String())
		if e == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			out = append(out, ip.String())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		// Private LAN ranges first, so the default is the usual choice.
		pi, pj := net.ParseIP(out[i]).IsPrivate(), net.ParseIP(out[j]).IsPrivate()
		if pi != pj {
			return pi
		}
		return out[i] < out[j]
	})
	return out
}

var errRefused = errors.New("connection refused: nothing is listening on that address and port")

// withDefaultPort adds the coordinator's default port to a bare host, so
// "192.168.1.5" means port 8765 rather than HTTP's port 80. Addresses with a
// scheme or port are left as typed.
func withDefaultPort(raw string) string {
	if raw == "" || strings.Contains(raw, "://") {
		return raw
	}
	if _, _, err := net.SplitHostPort(raw); err == nil {
		return raw
	}
	if ip := net.ParseIP(strings.Trim(raw, "[]")); ip != nil && ip.To4() == nil {
		return net.JoinHostPort(ip.String(), defaultPort)
	}
	if strings.Contains(raw, ":") {
		return raw // malformed; let config.URL report it
	}
	return net.JoinHostPort(raw, defaultPort)
}

// localCoordinator looks for a coordinator on this machine's LAN addresses
// when a loopback address was refused, because a coordinator bound to one LAN
// address does not accept loopback connections. It returns the host:port that
// answered, or "".
func localCoordinator(ctx context.Context, server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	for _, addr := range lanAddresses() {
		hostPort := net.JoinHostPort(addr, port)
		if _, err := checkCoordinator(ctx, u.Scheme+"://"+hostPort); err == nil {
			return hostPort
		}
	}
	return ""
}

// checkCoordinator confirms the unauthenticated health endpoint answers.
func checkCoordinator(ctx context.Context, server string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if _, err := farm.NewClient(server, "").JSON(ctx, "GET", "/api/health", "", nil, &health); err != nil {
		var netErr net.Error
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			return "", errRefused
		case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
			return "", errors.New("no response within 10 seconds: a firewall may be dropping the connection, or the address is wrong")
		}
		return "", err
	}
	if health.Status != "ok" {
		return "", errors.New("unexpected health response; is this a Transcoop coordinator?")
	}
	if health.Version == "" {
		return "Transcoop", nil
	}
	return "Transcoop " + health.Version, nil
}

// firewallHint returns a command to open the coordinator port when a common
// Linux host firewall is active, or "" when none is detected.
func firewallHint(port string) string {
	if runtime.GOOS != "linux" {
		return "If other machines cannot connect, allow TCP port " + port + " through this machine's firewall."
	}
	active := func(unit string) bool {
		return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
	}
	switch {
	case active("ufw"):
		return "ufw is active. Allow workers to connect with:\n  sudo ufw allow " + port + "/tcp"
	case active("firewalld"):
		return "firewalld is active. Allow workers to connect with:\n  sudo firewall-cmd --permanent --add-port=" + port + "/tcp && sudo firewall-cmd --reload"
	}
	return ""
}
