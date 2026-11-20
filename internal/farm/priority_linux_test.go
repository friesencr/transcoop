package farm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func niceOf(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	// Fields after the parenthesised command name; nice is field 19.
	fields := strings.Fields(string(data[strings.LastIndexByte(string(data), ')')+2:]))
	n, err := strconv.Atoi(fields[16])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStartWithPriorityLowersOnlyTheChild(t *testing.T) {
	// A renamed sleep, so priority daemons such as ananicy leave it alone.
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "tcprio")
	if err := os.WriteFile(bin, data, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(priority string, plain bool) int {
		cmd := exec.Command(bin, "5")
		if plain {
			err = cmd.Start()
		} else {
			err = startWithPriority(cmd, priority)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		return niceOf(t, cmd.Process.Pid)
	}
	base := run("", true)
	for _, c := range []struct {
		priority string
		nice     int
	}{{PriorityNormal, base}, {PriorityLow, max(base, 10)}, {PriorityIdle, max(base, 19)}} {
		if got := run(c.priority, false); got != c.nice {
			t.Errorf("%s: child nice %d, want %d", c.priority, got, c.nice)
		}
	}
	// Later plain starts must not land on a lowered thread.
	for i := 0; i < 20; i++ {
		if n := run("", true); n != base {
			t.Fatalf("plain start inherited nice %d, want %d", n, base)
		}
	}
}
