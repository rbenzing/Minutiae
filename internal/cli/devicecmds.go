package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
)

type selector struct {
	kind device.Kind
	flag string
	id   string
}

func newSelector(cmd *cobra.Command, kind device.Kind, flag string) *selector {
	s := &selector{kind: kind, flag: flag}
	cmd.Flags().StringVar(&s.id, flag, "", fmt.Sprintf("%s device id (optional when exactly one is connected)", kind))
	return s
}

func (s *selector) find(ctx context.Context, d Deps) (device.Device, error) {
	if s.id != "" {
		return d.Registry.Find(ctx, s.kind, s.id)
	}
	devs, errs := d.Registry.List(ctx, s.kind)
	switch len(devs) {
	case 1:
		return devs[0], nil
	case 0:
		if len(errs) > 0 {
			return nil, fmt.Errorf("%w: no %s device (%v)", device.ErrNotFound, s.kind, errors.Join(errs...))
		}
		return nil, fmt.Errorf("%w: no %s device connected", device.ErrNotFound, s.kind)
	default:
		return nil, usageErrorf("%d %s devices connected; choose one with --%s", len(devs), s.kind, s.flag)
	}
}

func capability[T any](dv device.Device) (T, error) {
	t, ok := dv.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("%w on %s", device.ErrUnsupported, dv.Kind())
	}
	return t, nil
}

func newInfoCmd(d Deps, opts *rootOptions, kind device.Kind, flag string) *cobra.Command {
	var casePath string
	cmd := &cobra.Command{Use: "info", Short: "Show device information", Args: exactArgs(0)}
	sel := newSelector(cmd, kind, flag)
	cmd.Flags().StringVar(&casePath, "case", "", "also record the information into this case")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		dv, err := sel.find(cmd.Context(), d)
		if err != nil {
			return err
		}
		info, err := dv.Info(cmd.Context())
		if err != nil {
			return err
		}
		if casePath != "" {
			c, err := openCase(casePath)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			err = device.RunAcquisition(c, dv.ID(), "info", nil, func(acq string) error {
				src := evidence.Source{Kind: "info", DeviceID: dv.ID()}
				_, err := c.Capture(dv.ID(), acq, "device/info.json", src, func(w io.Writer) error { return writeJSON(w, info) })
				return err
			})
			if err != nil {
				return err
			}
		}
		if opts.json {
			return writeJSON(d.Out, info)
		}
		fmt.Fprintf(d.Out, "ID:           %s\nKind:         %s\nModel:        %s\nManufacturer: %s\nOS:           %s\nSerial:       %s\n",
			info.ID, info.Kind, info.Model, info.Manufacturer, info.OSVersion, info.SerialNumber)
		if opts.verbose {
			keys := make([]string, 0, len(info.Extra))
			for k := range info.Extra {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(d.Out, "  %s = %s\n", k, info.Extra[k])
			}
		}
		return nil
	}
	return cmd
}

func newLsCmd(d Deps, opts *rootOptions, kind device.Kind, flag string) *cobra.Command {
	cmd := &cobra.Command{Use: "ls <remote-dir>", Short: "List a directory on the device", Args: exactArgs(1)}
	sel := newSelector(cmd, kind, flag)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		dv, err := sel.find(cmd.Context(), d)
		if err != nil {
			return err
		}
		t, err := capability[device.FileTransferer](dv)
		if err != nil {
			return err
		}
		es, err := t.List(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if opts.json {
			return writeJSON(d.Out, es)
		}
		for _, e := range es {
			fmt.Fprintf(d.Out, "%s %12d %s %s\n", e.Mode, e.Size, e.ModTime.Format("2006-01-02 15:04:05"), displayName(e.Name))
		}
		return nil
	}
	return cmd
}

func newPullCmd(d Deps, opts *rootOptions, kind device.Kind, flag string) *cobra.Command {
	var casePath string
	cmd := &cobra.Command{Use: "pull <remote-path>...", Short: "Copy files from the device into the case", Args: minArgs(1)}
	sel := newSelector(cmd, kind, flag)
	cmd.Flags().StringVar(&casePath, "case", "", "case directory")
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
		var recs []evidence.ManifestRecord
		err = device.RunAcquisition(c, dv.ID(), "pull", map[string]any{"paths": args}, func(acq string) error {
			for _, p := range args {
				rec, err := device.PullToCase(cmd.Context(), c, t, dv.ID(), acq, p)
				recs = append(recs, rec)
				if err != nil {
					return err
				}
			}
			return nil
		})
		printRecords(d, opts, recs)
		return err
	}
	return cmd
}

// displayName returns a remote file name safe to print on a terminal: a name
// with control or non-printable characters, invalid UTF-8 or a leading quote
// is printed Go-quoted, so a hostile name cannot inject escape sequences or
// fake extra lines.
func displayName(s string) string {
	if !utf8.ValidString(s) || strings.HasPrefix(s, `"`) || strings.IndexFunc(s, func(r rune) bool { return !strconv.IsPrint(r) }) >= 0 {
		return strconv.Quote(s)
	}
	return s
}

// printRecords prints the artifacts that were created; a zero-value record
// (the artifact could not even be created) is skipped — its error is reported
// separately.
func printRecords(d Deps, opts *rootOptions, recs []evidence.ManifestRecord) {
	created := make([]evidence.ManifestRecord, 0, len(recs))
	for _, r := range recs {
		if r.ID != "" {
			created = append(created, r)
		}
	}
	recs = created
	if opts.json {
		_ = writeJSON(d.Out, recs)
		return
	}
	for _, r := range recs {
		status := ""
		if r.Incomplete {
			status = "  INCOMPLETE: " + r.Error
		}
		fmt.Fprintf(d.Out, "%s  %s  %s%s\n", r.SHA256, humanBytes(r.Size), r.Path, status)
	}
}

func newLogicalCmd(d Deps, _ *rootOptions, kind device.Kind, flag, use, short string, withRoots bool) *cobra.Command {
	var casePath string
	var roots []string
	cmd := &cobra.Command{Use: use, Short: short, Args: exactArgs(0)}
	sel := newSelector(cmd, kind, flag)
	cmd.Flags().StringVar(&casePath, "case", "", "case directory")
	_ = cmd.MarkFlagRequired("case")
	if withRoots {
		cmd.Flags().StringSliceVar(&roots, "root", nil, "remote directory to acquire (repeatable; default /sdcard)")
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		dv, err := sel.find(cmd.Context(), d)
		if err != nil {
			return err
		}
		la, err := capability[device.LogicalAcquirer](dv)
		if err != nil {
			return err
		}
		c, err := openCase(casePath)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		err = la.AcquireLogical(cmd.Context(), c, device.LogicalOptions{Roots: roots}, newProgress(d.Err, use))
		fmt.Fprintln(d.Err)
		if err != nil {
			return err
		}
		fmt.Fprintf(d.Out, "%s acquisition of %s complete; run `minutiae case verify --case %s`\n", use, dv.ID(), casePath)
		return nil
	}
	return cmd
}
