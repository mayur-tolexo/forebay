// Command forebay-agent runs on each node, owning the split between capacity
// the workload keeps and capacity lent to the storage fabric.
//
// The agent has no runtime yet. Its capacity accounting and lease handling
// exist and are tested, but nothing wires them to a device or a control plane.
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
		fmt.Println("forebay-agent", version.String())
		return
	}

	fmt.Fprintln(os.Stderr, "forebay-agent", version.String())
	fmt.Fprintln(os.Stderr, "no runtime yet: see docs/rfcs/0004-node-agent.md")
}
