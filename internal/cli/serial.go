package cli

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
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
	s := consoleSession{d: d, cfg: serial.DefaultConfig()}
	var casePath string
	cmd := &cobra.Command{
		Use:   "console",
		Short: "Open a raw console on a serial port (receive-only unless --allow-device-write)",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := s.cfg.Validate(); err != nil {
				return usageErrorf("%v", err)
			}
			if s.allowWrite && casePath == "" {
				return usageErrorf("--allow-device-write requires --case so that every write is audited")
			}
			if (s.cfg.DTR || s.cfg.RTS) && !s.allowWrite {
				return usageErrorf("--dtr and --rts drive the device's modem control lines and require --allow-device-write")
			}
			if casePath == "" {
				return s.runUnrecorded(cmd.Context())
			}
			return s.runRecorded(cmd.Context(), casePath)
		},
	}
	f := cmd.Flags()
	f.StringVar(&s.port, "port", "", "serial port (e.g. COM3, /dev/ttyUSB0)")
	f.IntVar(&s.cfg.Baud, "baud", s.cfg.Baud, "baud rate")
	f.IntVar(&s.cfg.DataBits, "data-bits", s.cfg.DataBits, "data bits (5-8)")
	f.StringVar(&s.cfg.Parity, "parity", s.cfg.Parity, "none, odd, even, mark or space")
	f.StringVar(&s.cfg.StopBits, "stop-bits", s.cfg.StopBits, "1, 1.5 or 2")
	f.StringVar(&casePath, "case", "", "case directory to record the session into")
	f.BoolVar(&s.allowWrite, "allow-device-write", false, "send stdin to the device (every chunk is audited)")
	f.BoolVar(&s.cfg.DTR, "dtr", false, "assert DTR after open (audited device modification; needs --allow-device-write)")
	f.BoolVar(&s.cfg.RTS, "rts", false, "assert RTS after open (audited device modification; needs --allow-device-write)")
	_ = cmd.MarkFlagRequired("port")
	return cmd
}

// consoleSession is one `serial console` invocation.
type consoleSession struct {
	d          Deps
	port       string
	cfg        serial.Config
	allowWrite bool
}

// open opens the port and announces the session only once it is open.
func (s consoleSession) open() (serial.Port, error) {
	p, err := s.d.Serial.Open(s.port, s.cfg)
	if err != nil {
		return nil, err
	}
	mode := "receive-only"
	if s.allowWrite {
		mode = "WRITE ENABLED (audited)"
	}
	fmt.Fprintf(s.d.Err, "connected to %s at %d baud, %s; Ctrl-C to exit\n", s.port, s.cfg.Baud, mode)
	return p, nil
}

func (s consoleSession) io(rec protocol.Recorder) protocol.IO {
	return protocol.IO{In: s.d.In, Out: s.d.Out, Rec: rec, AllowWrite: s.allowWrite}
}

// runUnrecorded is the console without --case: receive-only, modem lines off.
func (s consoleSession) runUnrecorded(ctx context.Context) error {
	p, err := s.open()
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()
	return protocol.Raw{}.Run(ctx, p, s.io(protocol.NopRecorder{}))
}

// runRecorded opens the case before the port, so a bad case never touches
// the device, and runs the whole port lifetime inside the acquisition: an
// open failure is recorded as acquire.error and the port is closed before
// acquire.end.
func (s consoleSession) runRecorded(ctx context.Context, casePath string) error {
	c, err := openCase(casePath)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return device.RunAcquisition(c, s.port, "serial.session", s.details(), func(acq string) (err error) {
		p, err := s.openAudited(c, acq)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, p.Close()) }()
		rec, err := protocol.NewCaseRecorder(c, s.port, acq)
		if err != nil {
			return err
		}
		runErr := protocol.Raw{}.Run(ctx, p, s.io(rec))
		return errors.Join(runErr, rec.Finish(runErr))
	})
}

// openAudited opens the port. Asserting DTR or RTS can reset a board or
// change a phone's boot mode, so it is audited as a device modification
// before the open and its outcome after.
func (s consoleSession) openAudited(c *evidence.Case, acq string) (serial.Port, error) {
	if !s.cfg.DTR && !s.cfg.RTS {
		return s.open()
	}
	d := map[string]any{"operation": "serial.line_state", "acquisition_id": acq, "dtr": s.cfg.DTR, "rts": s.cfg.RTS}
	if _, err := c.Audit.Append("device.modify", s.port, d); err != nil {
		return nil, err
	}
	p, err := s.open()
	if err != nil {
		d["error"] = err.Error()
		_, aerr := c.Audit.Append("device.modify.error", s.port, d)
		return nil, errors.Join(err, aerr)
	}
	if _, err := c.Audit.Append("device.modify.done", s.port, d); err != nil {
		return nil, errors.Join(err, p.Close())
	}
	return p, nil
}

// details are the acquire.start details: line settings, how the host drives
// the port, and the USB identity of the adapter (empty when not found).
func (s consoleSession) details() map[string]any {
	d := map[string]any{
		"port": s.port, "baud": s.cfg.Baud, "data_bits": s.cfg.DataBits, "parity": s.cfg.Parity,
		"stop_bits": s.cfg.StopBits, "allow_write": s.allowWrite, "protocol": protocol.Raw{}.Name(),
		"dtr": s.cfg.DTR, "rts": s.cfg.RTS, "read_timeout_ms": s.cfg.ReadTimeout.Milliseconds(), "host_os": runtime.GOOS,
		"vid": "", "pid": "", "usb_serial": "", "product": "",
	}
	ports, err := s.d.Serial.List()
	if err != nil {
		d["port_lookup_error"] = err.Error()
	}
	for _, p := range ports {
		if p.Name == s.port || (runtime.GOOS == "windows" && strings.EqualFold(p.Name, s.port)) {
			d["vid"], d["pid"], d["usb_serial"], d["product"] = p.VID, p.PID, p.SerialNumber, p.Product
			break
		}
	}
	return d
}
