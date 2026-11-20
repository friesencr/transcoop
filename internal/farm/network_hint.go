package farm

import (
	"errors"
	"log"
	"runtime"
	"syscall"
)

// networkHint explains, once, the usual cause of "no route to host" on macOS:
// Local Network privacy. A worker started outside Terminal or an SSH session
// (for example after logging out, or by launchd) is denied LAN access until it
// is allowed in System Settings.
func (w *Worker) networkHint(err error) {
	if runtime.GOOS != "darwin" || !errors.Is(err, syscall.EHOSTUNREACH) {
		return
	}
	w.hintOnce.Do(func() {
		log.Print("macOS may be blocking this worker's local network access: allow transcoop in System Settings → Privacy & Security → Local Network, or start the worker from Terminal and accept the prompt")
	})
}
