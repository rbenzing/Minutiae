// Package cli is the thin command-line layer over Minutiae's internal packages.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/android"
	"github.com/rbenzing/minutiae/internal/android/adb"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/ios"
	"github.com/rbenzing/minutiae/internal/transport/serial"
)

// Deps are the external dependencies of the CLI, injectable for tests.
type Deps struct {
	In       io.Reader
	Out      io.Writer
	Err      io.Writer
	Serial   serial.Provider
	Registry *device.Registry
	// FSDrivers overrides the filesystem drivers used by the image commands
	// (tests inject fakes). nil = detect.Drivers.
	FSDrivers []detect.Driver
}

// DefaultDeps wires the real process streams and device backends.
func DefaultDeps() Deps {
	sp := serial.System{}
	return Deps{
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr,
		Serial:   sp,
		Registry: device.NewRegistry(serial.Enumerator{P: sp}, android.Enumerator{C: adb.New("")}, ios.Enumerator{B: ios.GoIOS{}}),
	}
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
	root.AddCommand(newCaseCmd(d, opts))
	root.AddCommand(newDevicesCmd(d, opts))
	root.AddCommand(newSerialCmd(d, opts))
	root.AddCommand(newAndroidCmd(d, opts))
	root.AddCommand(newIOSCmd(d, opts))
	root.AddCommand(newImageCmd(d, opts))
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
		fmt.Fprintln(d.Err, "error:", escapeMultiline(err.Error()))
	}
	return ExitCode(err)
}

// newGroupCmd builds a command that only holds subcommands. Unknown
// subcommands are usage errors (exit 2) instead of silently printing help.
func newGroupCmd(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageErrorf("unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return cmd.Help()
		},
	}
}
