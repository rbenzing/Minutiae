package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/device"
)

// oneLine flattens multi-line error messages (the ADB server's "unauthorized"
// text has several lines) so each device stays on one table row.
var oneLine = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

func newDevicesCmd(d Deps, opts *rootOptions) *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List connected devices across all backends",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch device.Kind(kind) {
			case "", device.KindAndroid, device.KindIOS, device.KindSerial:
			default:
				return usageErrorf("--kind must be android, ios or serial")
			}
			devs, errs := d.Registry.List(cmd.Context(), device.Kind(kind))
			infos := make([]device.Info, 0, len(devs))
			for _, dv := range devs {
				info, err := dv.Info(cmd.Context())
				if err != nil {
					info = device.Info{ID: dv.ID(), Kind: dv.Kind(), Extra: map[string]string{"error": err.Error()}}
				}
				infos = append(infos, info)
			}
			for _, e := range errs {
				fmt.Fprintln(d.Err, "warning:", e)
			}
			if opts.json {
				return writeJSON(d.Out, infos)
			}
			if len(infos) == 0 {
				fmt.Fprintln(d.Out, "no devices found")
			}
			for _, i := range infos {
				detail := i.OSVersion
				if msg := i.Extra["error"]; msg != "" {
					detail = "error: " + oneLine.Replace(msg)
				}
				fmt.Fprintf(d.Out, "%-8s %-28s %-20s %s\n", i.Kind, i.ID, i.Model, detail)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "android, ios or serial")
	return cmd
}
