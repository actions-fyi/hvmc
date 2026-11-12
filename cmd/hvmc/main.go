// Command hvmc is the Hyper-V Memory Coordinator: the coordinator that runs in
// Kubernetes and the agents that run as native services on Hyper-V hosts and
// guests. See docs/DESIGN.md.
package main

import (
	"io"
	"os"

	"github.com/matfax/hvmc/internal/cli"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command tree and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	return cli.Execute(args, stdout, stderr)
}
