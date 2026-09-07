package filedriver

import "os"

// SyncFile lets a test replace the flush, so the call that makes a durable
// write durable can be seen to happen.
func SyncFile(with func(*os.File) error) func() {
	was := syncFile
	syncFile = with
	return func() { syncFile = was }
}
