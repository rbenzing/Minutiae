// Package protocol defines how Minutiae talks over a byte stream (a serial
// port). Spec 1 ships the raw console; AT, EDL, BROM and UART drivers are
// roadmap sub-project 8.
package protocol

import (
	"context"
	"io"
)

// Recorder captures a session. TX is called before bytes reach the device and
// must refuse (return an error) once Finish has been called; TXResult is
// called after every write that TX allowed, with the bytes the port accepted
// and the write error (io.ErrShortWrite when it accepted fewer than offered).
type Recorder interface {
	RX(p []byte) error
	TX(p []byte) error
	TXResult(n int, err error) error
	Finish(cause error) error
}

// NopRecorder records nothing (console without --case). It cannot back a
// session that writes to the device: Raw refuses AllowWrite with it.
type NopRecorder struct{}

func (NopRecorder) RX([]byte) error           { return nil }
func (NopRecorder) TX([]byte) error           { return nil }
func (NopRecorder) TXResult(int, error) error { return nil }
func (NopRecorder) Finish(error) error        { return nil }

// IO is the operator side of a session.
//
// AllowWrite sends In to the device. It requires a real Recorder (one that
// audits every chunk): with a nil Rec or NopRecorder, Run refuses with
// device.ErrDeviceWriteNotAllowed before touching the port.
type IO struct {
	In         io.Reader
	Out        io.Writer
	Rec        Recorder
	AllowWrite bool
}

// Protocol runs a session over port until the port closes or ctx ends.
type Protocol interface {
	Name() string
	Run(ctx context.Context, port io.ReadWriter, s IO) error
}
