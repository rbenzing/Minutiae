package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/rbenzing/minutiae/internal/device"
)

// Raw is a transparent console: port → Out always; In → port only when
// AllowWrite is set, with every chunk recorded (and audited) before sending
// and its outcome recorded after.
type Raw struct{}

// Name is "raw".
func (Raw) Name() string { return "raw" }

// txGate serialises writes against the end of the session. pumpTX holds it
// from the closed check through rec.TX, port.Write and rec.TXResult; Run
// closes it on every return path, so once Run returns no write can start and
// none is still in flight. err keeps the first TX-side failure for Run.
type txGate struct {
	mu     sync.Mutex
	closed bool
	err    error
}

// Run returns nil when the port reaches EOF or ctx is cancelled.
func (Raw) Run(ctx context.Context, port io.ReadWriter, s IO) (err error) {
	if s.AllowWrite {
		switch s.Rec.(type) {
		case nil, NopRecorder, *NopRecorder:
			return fmt.Errorf("%w: raw console writes need an auditing recorder", device.ErrDeviceWriteNotAllowed)
		}
	}
	if s.Rec == nil {
		s.Rec = NopRecorder{}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	gate := &txGate{}
	defer func() {
		gate.mu.Lock()
		gate.closed = true
		txErr := gate.err
		gate.mu.Unlock()
		err = errors.Join(err, txErr)
	}()
	if s.AllowWrite && s.In != nil {
		// Reading stdin cannot be interrupted; this goroutine may outlive Run
		// until the process exits. After Run returns it can never write: the
		// gate is closed.
		go func() {
			if pumpTX(ctx, s.In, port, s.Rec, gate) {
				cancel()
			}
		}()
	}
	return rxLoop(ctx, port, s)
}

func rxLoop(ctx context.Context, port io.Reader, s IO) error {
	buf := make([]byte, 4096)
	for ctx.Err() == nil {
		n, err := port.Read(buf) // returns (0, nil) on read timeout
		if n > 0 {
			if err := s.Rec.RX(buf[:n]); err != nil {
				return err
			}
			if _, err := s.Out.Write(buf[:n]); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// pumpTX copies in to port until EOF, an error, ctx ends or the gate closes.
// It reports whether it stopped on an error (recorded in gate.err).
func pumpTX(ctx context.Context, in io.Reader, port io.Writer, rec Recorder, gate *txGate) bool {
	buf := make([]byte, 1024)
	for {
		n, rerr := in.Read(buf)
		gate.mu.Lock()
		if gate.closed || ctx.Err() != nil {
			gate.mu.Unlock()
			return false
		}
		var err error
		if n > 0 {
			err = send(port, rec, buf[:n])
		}
		if err == nil && !errors.Is(rerr, io.EOF) {
			err = rerr
		}
		if err != nil {
			gate.err = err
		}
		gate.mu.Unlock()
		if err != nil {
			return true
		}
		if errors.Is(rerr, io.EOF) {
			return false
		}
	}
}

// send records p, writes it and records the outcome. Caller holds the gate.
func send(port io.Writer, rec Recorder, p []byte) error {
	if err := rec.TX(p); err != nil {
		return err
	}
	n, werr := port.Write(p)
	if werr == nil && n < len(p) {
		werr = io.ErrShortWrite
	}
	return errors.Join(werr, rec.TXResult(n, werr))
}
