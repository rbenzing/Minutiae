// Package cli is the thin command-line layer over Minutiae's internal packages.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
)

// Deps are the external dependencies of the CLI, injectable for tests.
type Deps struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// DefaultDeps wires the real process streams.
func DefaultDeps() Deps {
	return Deps{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
}

type rootOptions struct {
	json    bool
	verbose bool
}

func newRootCmd(d Deps) *cobra.Command {
	opts := &rootOptions{}
	root := &cobra.Command{
		Use:           "minutiae",
		Short:         "Forensic acquisition for mobile devices",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(d.In)
	root.SetOut(d.Out)
	root.SetErr(d.Err)
	root.PersistentFlags().BoolVar(&opts.json, "json", false, "machine-readable JSON output")
	root.PersistentFlags().BoolVarP(&opts.verbose, "verbose", "v", false, "verbose output")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddCommand(newVersionCmd(d, opts))
	return root
}

// Run executes the CLI with args and returns the process exit code.
// Ctrl-C cancels the command context so acquisitions can record partial evidence.
func Run(args []string, d Deps) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	root := newRootCmd(d)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err != nil {
		if isCobraUsageError(err) {
			err = usageError{err}
		}
		fmt.Fprintln(d.Err, "error:", err)
	}
	return ExitCode(err)
}
