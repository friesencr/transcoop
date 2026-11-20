package fsproto

import "runtime"

// MountPointCreatedByDriver reports that the FUSE driver creates the mount
// directory itself and refuses one that already exists (WinFsp). Elsewhere the
// mount directory must exist and be empty.
const MountPointCreatedByDriver = runtime.GOOS == "windows"
