// Package serial is the USB/serial-port transport. It wraps go.bug.st/serial
// behind Provider so everything above it is testable without hardware.
package serial

import (
	"fmt"
	"io"
	"time"

	bug "go.bug.st/serial"
)

// PortInfo describes one serial port.
type PortInfo struct {
	Name         string `json:"name"`
	IsUSB        bool   `json:"is_usb"`
	VID          string `json:"vid,omitempty"`
	PID          string `json:"pid,omitempty"`
	SerialNumber string `json:"serial_number,omitempty"`
	Product      string `json:"product,omitempty"`
}

// Config holds line settings.
type Config struct {
	Baud        int
	DataBits    int
	Parity      string // none, odd, even, mark, space
	StopBits    string // 1, 1.5, 2
	ReadTimeout time.Duration
	// DTR and RTS are the modem output lines held after open. Both default
	// to false: on many boards and phones DTR/RTS are wired to reset or
	// boot-mode pins, so asserting them is a device modification.
	DTR, RTS bool
}

// DefaultConfig is 115200 8N1 with a 100 ms read timeout.
func DefaultConfig() Config {
	return Config{Baud: 115200, DataBits: 8, Parity: "none", StopBits: "1", ReadTimeout: 100 * time.Millisecond}
}

var parities = map[string]bug.Parity{
	"none": bug.NoParity, "odd": bug.OddParity, "even": bug.EvenParity,
	"mark": bug.MarkParity, "space": bug.SpaceParity,
}

var stopBits = map[string]bug.StopBits{
	"1": bug.OneStopBit, "1.5": bug.OnePointFiveStopBits, "2": bug.TwoStopBits,
}

// Validate reports the first invalid setting.
func (c Config) Validate() error {
	switch {
	case c.Baud <= 0:
		return fmt.Errorf("baud must be positive, got %d", c.Baud)
	case c.DataBits < 5 || c.DataBits > 8:
		return fmt.Errorf("data bits must be 5-8, got %d", c.DataBits)
	}
	if _, ok := parities[c.Parity]; !ok {
		return fmt.Errorf("parity must be none, odd, even, mark or space, got %q", c.Parity)
	}
	if _, ok := stopBits[c.StopBits]; !ok {
		return fmt.Errorf("stop bits must be 1, 1.5 or 2, got %q", c.StopBits)
	}
	if c.ReadTimeout <= 0 {
		return fmt.Errorf("read timeout must be positive, got %s", c.ReadTimeout)
	}
	return nil
}

// Port is an open serial port.
type Port interface {
	io.ReadWriteCloser
	SetDTR(bool) error
	SetRTS(bool) error
}

// Provider lists and opens ports.
type Provider interface {
	List() ([]PortInfo, error)
	Open(name string, cfg Config) (Port, error)
}

// System is the real Provider backed by the operating system.
type System struct{}

// Open opens name with cfg.
func (System) Open(name string, cfg Config) (Port, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	p, err := bug.Open(name, openMode(cfg))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	if err := p.SetReadTimeout(cfg.ReadTimeout); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("set read timeout on %s: %w", name, err)
	}
	return p, nil
}

// openMode builds the driver mode. InitialStatusBits is never nil: nil makes
// go.bug.st/serial assert DTR and RTS on open. On Linux/macOS the kernel may
// still pulse both lines during open(2); they are de-asserted right after.
func openMode(cfg Config) *bug.Mode {
	return &bug.Mode{
		BaudRate: cfg.Baud, DataBits: cfg.DataBits,
		Parity: parities[cfg.Parity], StopBits: stopBits[cfg.StopBits],
		InitialStatusBits: &bug.ModemOutputBits{DTR: cfg.DTR, RTS: cfg.RTS},
	}
}
