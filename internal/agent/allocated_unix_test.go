//go:build unix

package agent

import (
	"os"
	"syscall"
	"testing"
)

// allocatedBytes reports what a file really occupies, which is the only way to
// tell a reserved extent from a sparse one of the same length.
func allocatedBytes(t *testing.T, info os.FileInfo) int64 {
	t.Helper()
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no block count available on this platform")
	}
	return int64(st.Blocks) * 512
}
