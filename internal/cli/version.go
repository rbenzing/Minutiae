package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/version"
)

func newVersionCmd(d Deps, opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the Minutiae version",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			if opts.json {
				return writeJSON(d.Out, map[string]string{"version": version.Version, "commit": version.Commit})
			}
			fmt.Fprintf(d.Out, "minutiae %s\n", version.String())
			return nil
		},
	}
}
