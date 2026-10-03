package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

func newAndroidCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := newGroupCmd("android", "Android devices over ADB")
	const k, f = device.KindAndroid, "serial"
	cmd.AddCommand(
		newInfoCmd(d, opts, k, f),
		newLsCmd(d, opts, k, f),
		newPullCmd(d, opts, k, f),
		newLogicalCmd(d, opts, k, f, "logical", "Logical acquisition: device info, package list and files", true),
		newAndroidPushCmd(d),
		newAndroidPartitionsCmd(d, opts),
		newAndroidImageCmd(d, opts),
	)
	return cmd
}

func newAndroidPushCmd(d Deps) *cobra.Command {
	var casePath string
	var allow bool
	cmd := &cobra.Command{
		Use:   "push <local-file> <remote-path>",
		Short: "Write a file to the device (modifies evidence; audited; needs --allow-device-write)",
		Args:  exactArgs(2),
	}
	sel := newSelector(cmd, device.KindAndroid, "serial")
	cmd.Flags().StringVar(&casePath, "case", "", "case directory (the write is audited here)")
	cmd.Flags().BoolVar(&allow, "allow-device-write", false, "confirm that writing to the device is intended")
	_ = cmd.MarkFlagRequired("case")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		dv, err := sel.find(cmd.Context(), d)
		if err != nil {
			return err
		}
		t, err := capability[device.FileTransferer](dv)
		if err != nil {
			return err
		}
		c, err := openCase(casePath)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		if err := device.PushAudited(cmd.Context(), c, t, dv.ID(), args[0], args[1], allow); err != nil {
			return err
		}
		fmt.Fprintf(d.Out, "pushed %s to %s:%s (audited)\n", args[0], dv.ID(), args[1])
		return nil
	}
	return cmd
}

func newAndroidPartitionsCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "partitions", Short: "List partitions (requires root)", Args: exactArgs(0)}
	sel := newSelector(cmd, device.KindAndroid, "serial")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		dv, err := sel.find(cmd.Context(), d)
		if err != nil {
			return err
		}
		im, err := capability[device.Imager](dv)
		if err != nil {
			return err
		}
		ps, err := im.Partitions(cmd.Context())
		if err != nil {
			return err
		}
		if opts.json {
			return writeJSON(d.Out, ps)
		}
		for _, p := range ps {
			fmt.Fprintf(d.Out, "%-24s %-28s %s\n", p.Name, p.Path, humanBytes(p.Size))
		}
		return nil
	}
	return cmd
}

// partitionResolver is implemented by imagers that can report, before
// imaging, the block device and exact size a partition name resolves to.
type partitionResolver interface {
	ResolvePartition(ctx context.Context, partition string) (device.Partition, error)
}

func newAndroidImageCmd(d Deps, opts *rootOptions) *cobra.Command {
	var casePath, partition string
	cmd := &cobra.Command{Use: "image", Short: "Image a partition into the case (requires root; read-only)", Args: exactArgs(0)}
	sel := newSelector(cmd, device.KindAndroid, "serial")
	cmd.Flags().StringVar(&casePath, "case", "", "case directory")
	cmd.Flags().StringVar(&partition, "partition", "", "partition name (see `android partitions`)")
	_ = cmd.MarkFlagRequired("case")
	_ = cmd.MarkFlagRequired("partition")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		dv, err := sel.find(cmd.Context(), d)
		if err != nil {
			return err
		}
		im, err := capability[device.Imager](dv)
		if err != nil {
			return err
		}
		c, err := openCase(casePath)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		details := map[string]any{"partition": partition}
		if r, ok := im.(partitionResolver); ok {
			p, err := r.ResolvePartition(cmd.Context(), partition)
			if err != nil {
				return err
			}
			details["block_path"], details["expected_size"] = p.Path, p.Size // -1: unknown
		}
		var rec evidence.ManifestRecord
		err = device.RunAcquisition(c, dv.ID(), "physical", details, func(acq string) error {
			var err error
			rec, err = device.ImageToCase(cmd.Context(), c, im, dv.ID(), acq, partition, newProgress(d.Err, partition))
			return err
		})
		fmt.Fprintln(d.Err)
		if rec.ID != "" {
			printRecords(d, opts, []evidence.ManifestRecord{rec})
		}
		return err
	}
	return cmd
}
