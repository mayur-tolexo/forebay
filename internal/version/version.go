// Package version carries build identity stamped in at link time.
package version

// Values are overridden with -ldflags at build time. The defaults are what a
// plain "go build" produces, so an unstamped binary is identifiable as such.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String renders the build identity for logs and version output.
func String() string {
	return Version + " (" + Commit + ", built " + Date + ")"
}
