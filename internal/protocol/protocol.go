// Package protocol defines how Minutiae talks over a byte stream (a serial
// port). Spec 1 ships the raw console; AT, EDL, BROM and UART drivers are
// roadmap sub-project 8.
package protocol

import (
	"context"
	"io"
)

// Recorder captures a session. TX is called before bytes reach the device.
type Recorder interface {
	RX(p []byte) error
	TX(p []byte) error
	Finish(cause error) error
}

// NopRecorder records nothing (console without --case).
type NopRecorder struct{}

func (NopRecorder) RX([]byte) error    { return nil }
func (NopRecorder) TX([]byte) error    { return nil }
func (NopRecorder) Finish(error) error { return nil }

// IO is the operator side of a session.
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
