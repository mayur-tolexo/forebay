//go:build !linux

package main

import "fmt"

// allocate needs fallocate, so this measures nothing off Linux rather than
// timing the release of a file that never held anything.
func allocate(path string, size int64) error {
	return fmt.Errorf("committing %d bytes to %s needs Linux", size, path)
}
