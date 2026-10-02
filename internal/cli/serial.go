package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/protocol"
	"github.com/rbenzing/minutiae/internal/transport/serial"
)

func newSerialCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := newGroupCmd("serial", "USB serial ports and console capture")
	cmd.AddCommand(newSerialListCmd(d, opts), newSerialConsoleCmd(d))
	return cmd
}

func newSerialListCmd(d Deps, opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List serial ports",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			ports, err := d.Serial.List()
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(d.Out, ports)
			}
			if len(ports) == 0 {
				fmt.Fprintln(d.Out, "no serial ports found")
			}
			for _, p := range ports {
				id := ""
				if p.IsUSB {
					id = p.VID + ":" + p.PID
				}
				fmt.Fprintf(d.Out, "%-16s %-10s %s\n", p.Name, id, p.Product)
			}
			return nil
		},
	}
}

func newSerialConsoleCmd(d Deps) *cobra.Command {
	cfg := serial.DefaultConfig()
	var port, casePath string
	var allowWrite bool
	cmd := &cobra.Command{
		Use:   "console",
		Short: "Open a raw console on a serial port (receive-only unless --allow-device-write)",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cfg.Validate(); err != nil {
				return usageErrorf("%v", err)
			}
			if allowWrite && casePath == "" {
				return usageErrorf("--allow-device-write requires --case so that every write is audited")
			}
			p, err := d.Serial.Open(port, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = p.Close() }()
			mode := "receive-only"
			if allowWrite {
				mode = "WRITE ENABLED (audited)"
			}
			fmt.Fprintf(d.Err, "connected to %s at %d baud, %s; Ctrl-C to exit\n", port, cfg.Baud, mode)
			session := protocol.IO{In: d.In, Out: d.Out, AllowWrite: allowWrite}
			if casePath == "" {
				session.Rec = protocol.NopRecorder{}
				return protocol.Raw{}.Run(cmd.Context(), p, session)
			}
			c, err := openCase(casePath)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			details := map[string]any{
				"port": port, "baud": cfg.Baud, "data_bits": cfg.DataBits, "parity": cfg.Parity,
				"stop_bits": cfg.StopBits, "allow_write": allowWrite, "protocol": protocol.Raw{}.Name(),
			}
			return device.RunAcquisition(c, port, "serial.session", details, func(acq string) error {
				rec, err := protocol.NewCaseRecorder(c, port, acq)
				if err != nil {
					return err
				}
				session.Rec = rec
				runErr := protocol.Raw{}.Run(cmd.Context(), p, session)
				return errors.Join(runErr, rec.Finish(runErr))
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&port, "port", "", "serial port (e.g. COM3, /dev/ttyUSB0)")
	f.IntVar(&cfg.Baud, "baud", cfg.Baud, "baud rate")
	f.IntVar(&cfg.DataBits, "data-bits", cfg.DataBits, "data bits (5-8)")
	f.StringVar(&cfg.Parity, "parity", cfg.Parity, "none, odd, even, mark or space")
	f.StringVar(&cfg.StopBits, "stop-bits", cfg.StopBits, "1, 1.5 or 2")
	f.StringVar(&casePath, "case", "", "case directory to record the session into")
	f.BoolVar(&allowWrite, "allow-device-write", false, "send stdin to the device (every chunk is audited)")
	_ = cmd.MarkFlagRequired("port")
	return cmd
}
