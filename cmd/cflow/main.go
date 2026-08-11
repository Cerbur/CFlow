// Command cflow is the CFlow CLI entry point. It holds no domain, Store,
// Git, Provider, or filesystem logic: it assembles the command tree and
// maps the returned error to one central exit class. On an interactive
// terminal a bare `cflow` launches the full-screen TUI (design §1);
// every subcommand behaves exactly as before.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mattn/go-isatty"

	"cflow.local/cflow/internal/cli"
	"cflow.local/cflow/internal/observe"
	"cflow.local/cflow/internal/tui"
)

func main() {
	os.Exit(run())
}

func run() int {
	root := cli.NewRoot(cli.Dependencies{
		Build: observe.Current(),
		// The full-screen TUI is the default entry point on an
		// interactive terminal; it never mutates a Workflow by itself.
		RunTUI: func(ctx context.Context) error {
			home, err := cli.ResolveHome()
			if err != nil {
				return err
			}
			operationLog, err := tui.OpenOperationLog(home)
			if err != nil {
				return err
			}
			defer operationLog.Close()
			return tui.Run(ctx, tui.Dependencies{
				CLI:          cli.Dependencies{Build: observe.Current()},
				OperationLog: operationLog,
			})
		},
		IsTerminal: func() bool {
			return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
		},
	})
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cflow:", err)
		return cli.ExitCode(err)
	}
	return 0
}
