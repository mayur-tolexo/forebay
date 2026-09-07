package checkpoint

import "os"

// SyncFile lets a test replace the flush, so the call that makes a staged
// acknowledgement mean what it says can be seen to happen.
func SyncFile(with func(*os.File) error) func() {
	was := syncFile
	syncFile = with
	return func() { syncFile = was }
}
