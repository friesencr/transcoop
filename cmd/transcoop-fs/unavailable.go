//go:build !fuse

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "transcoop-fs requires a native build with -tags fuse and the platform FUSE driver")
	os.Exit(1)
}
