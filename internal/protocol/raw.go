package protocol

import (
	"context"
	"errors"
	"io"
)

// Raw is a transparent console: port → Out always; In → port only when
// AllowWrite is set, with every chunk recorded (and audited) before sending.
type Raw struct{}

// Name is "raw".
func (Raw) Name() string { return "raw" }

// Run returns nil when the port reaches EOF or ctx is cancelled.
func (Raw) Run(ctx context.Context, port io.ReadWriter, s IO) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	txErr := make(chan error, 1)
	if s.AllowWrite && s.In != nil {
		// Reading stdin cannot be interrupted; this goroutine may outlive Run
		// until the process exits, which is acceptable for a console.
		go func() {
			if err := pumpTX(ctx, s.In, port, s.Rec); err != nil {
				txErr <- err
				cancel()
			}
		}()
	}
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
			break
		}
		if err != nil {
			return err
		}
	}
	select {
	case err := <-txErr:
		return err
	default:
		return nil
	}
}

func pumpTX(ctx context.Context, in io.Reader, port io.Writer, rec Recorder) error {
	buf := make([]byte, 1024)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if ctx.Err() != nil {
				return nil
			}
			if err := rec.TX(buf[:n]); err != nil {
				return err
			}
			if _, err := port.Write(buf[:n]); err != nil {
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
}
