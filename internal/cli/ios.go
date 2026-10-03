package cli

import (
	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/device"
)

func newIOSCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := newGroupCmd("ios", "iPhone/iPad over usbmuxd (Apple Mobile Device Service on Windows)")
	const k, f = device.KindIOS, "udid"
	cmd.AddCommand(
		newInfoCmd(d, opts, k, f),
		newLsCmd(d, opts, k, f),
		newPullCmd(d, opts, k, f),
		newLogicalCmd(d, opts, k, f, "backup", "Logical acquisition via a full mobilebackup2 (iTunes-style) backup", false),
	)
	return cmd
}
