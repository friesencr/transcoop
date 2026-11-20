//go:build !linux

package farm

// clearStaleMount is Linux-only; elsewhere a stale mount is reported and must
// be unmounted by hand.
func clearStaleMount(string) (bool, error) { return false, nil }
