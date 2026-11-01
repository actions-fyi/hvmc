// Command hvmc is the Hyper-V Memory Coordinator: the coordinator that runs in
// Kubernetes and the agents that run as native services on Hyper-V hosts and
// guests. See docs/DESIGN.md.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/matfax/hvmc/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the entry point; it is replaced by the full command tree in WP 1.6.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		_, _ = fmt.Fprintf(stdout, "hvmc %s\n", version.String())
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "usage: hvmc version")
	return 2
}
