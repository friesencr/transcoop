package farm

import "runtime"

// testRoot makes fixed fixture paths such as testRoot+"/a" absolute on every
// platform ("C:/a" on Windows).
var testRoot = func() string {
	if runtime.GOOS == "windows" {
		return "C:"
	}
	return ""
}()
