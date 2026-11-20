//go:build !windows

package remotefs

import "os"

func syncJournalDirectory(p string) error {
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
