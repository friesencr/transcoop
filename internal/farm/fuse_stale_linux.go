package farm

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// clearStaleMount unmounts a Transcoop FUSE mount at dir. The caller holds the
// directory's lock, so the mount's helper is gone.
func clearStaleMount(dir string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, nil
	}
	defer f.Close()
	found := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if mountPoint, source, ok := parseMountInfo(scanner.Text()); ok && source == "transcoop" && filepath.Clean(mountPoint) == filepath.Clean(dir) {
			found = true
			break
		}
	}
	if !found {
		return false, nil
	}
	var last error
	for _, tool := range []string{"fusermount", "fusermount3"} {
		if path, err := exec.LookPath(tool); err == nil {
			out, err := exec.Command(path, "-u", "-z", dir).CombinedOutput()
			if err == nil {
				return true, nil
			}
			last = fmt.Errorf("%s: %w: %s", tool, err, strings.TrimSpace(string(out)))
		}
	}
	if last == nil {
		last = exec.ErrNotFound
	}
	return false, last
}

// parseMountInfo returns the mount point and source of a mountinfo line.
func parseMountInfo(line string) (mountPoint, source string, ok bool) {
	fields := strings.Fields(line)
	sep := -1
	for i, f := range fields {
		if f == "-" {
			sep = i
			break
		}
	}
	if len(fields) < 5 || sep < 0 || sep+2 >= len(fields) {
		return "", "", false
	}
	return unescapeMountInfo(fields[4]), unescapeMountInfo(fields[sep+2]), true
}

// unescapeMountInfo decodes the octal escapes (\040 for space) in mountinfo.
func unescapeMountInfo(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
