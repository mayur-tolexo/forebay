//go:build !linux

package main

import "fmt"

// available needs statfs with the fields Linux reports, so this measures
// nothing elsewhere rather than reporting a number from another filesystem.
func available(dir string) (int64, error) {
	return 0, fmt.Errorf("reading free space on %s needs Linux", dir)
}
