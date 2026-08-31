// Command forebay-controller runs the control plane, which grants capacity
// leases and decides placement, and never sits in the IO path.
//
// The controller has no runtime yet.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mayur-tolexo/forebay/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the build identity and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("forebay-controller", version.String())
		return
	}

	fmt.Fprintln(os.Stderr, "forebay-controller", version.String())
	fmt.Fprintln(os.Stderr, "no runtime yet: see docs/rfcs/0002-architecture-overview.md")
}
