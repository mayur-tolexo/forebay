//go:build !linux

package main

import "fmt"

// evict is Linux only, since fadvise and mincore are. A run elsewhere refuses
// rather than reporting a warm number as a cold one.
func evict(path string) (before, after int, err error) {
	return 0, 0, fmt.Errorf("evicting the page cache needs Linux, so %s cannot be measured cold here", path)
}
