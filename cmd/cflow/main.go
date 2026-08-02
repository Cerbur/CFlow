// Command cflow is the CFlow CLI entry point. It holds no domain, Store,
// Git, Provider, or filesystem logic: it assembles the command tree and
// maps the returned error to one central exit class.
package main

import (
	"fmt"
	"os"

	"cflow.local/cflow/internal/cli"
	"cflow.local/cflow/internal/observe"
)

func main() {
	os.Exit(run())
}

func run() int {
	root := cli.NewRoot(cli.Dependencies{Build: observe.Current()})
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cflow:", err)
		return cli.ExitCode(err)
	}
	return 0
}
