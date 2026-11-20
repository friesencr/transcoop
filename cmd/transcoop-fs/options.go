//go:build fuse

package main

import "runtime"

var mountOptions = func() string {
	o := "fsname=transcoop,default_permissions,attr_timeout=0,entry_timeout=0,negative_timeout=0"
	if runtime.GOOS == "windows" {
		// Windows has no Unix uid (os.Getuid returns -1); WinFsp maps -1 to
		// the mounting user, so files are owned by the worker's account.
		o += ",uid=-1,gid=-1"
	}
	return o
}()
