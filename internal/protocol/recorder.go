package protocol

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// ErrRecorderFinished is returned by TX once the session has been finished:
// no write may be audited, or sent, after the session artifacts are closed.
var ErrRecorderFinished = errors.New("serial session recorder is finished")

// CaseRecorder writes serial/rx.bin (raw received bytes) and
// serial/transcript.jsonl (timestamped rx/tx chunks) as case artifacts, and
// audits every transmitted chunk as a device modification (device.modify
// before the write, device.modify.done or device.modify.error after it).
type CaseRecorder struct {
	mu       sync.Mutex
	c        *evidence.Case
	deviceID string
	acqID    string
	rx, tr   *evidence.ArtifactWriter
	finished bool
	pending  int // length of the chunk TX allowed and TXResult has not settled
}

type transcriptLine struct {
	T   string `json:"t"`
	Dir string `json:"dir"`
	Hex string `json:"hex"`
}

// NewCaseRecorder creates both session artifacts.
func NewCaseRecorder(c *evidence.Case, deviceID, acqID string) (*CaseRecorder, error) {
	src := evidence.Source{Kind: "serial", DeviceID: deviceID}
	rx, err := c.NewArtifact(deviceID, acqID, "serial/rx.bin", src)
	if err != nil {
		return nil, err
	}
	tr, err := c.NewArtifact(deviceID, acqID, "serial/transcript.jsonl", src)
	if err != nil {
		_, aerr := rx.Abort(err)
		return nil, errors.Join(err, aerr)
	}
	return &CaseRecorder{c: c, deviceID: deviceID, acqID: acqID, rx: rx, tr: tr}, nil
}

func (r *CaseRecorder) line(dir string, p []byte) error {
	b, err := json.Marshal(transcriptLine{T: time.Now().UTC().Format(time.RFC3339Nano), Dir: dir, Hex: hex.EncodeToString(p)})
	if err != nil {
		return err
	}
	_, err = r.tr.Write(append(b, '\n'))
	return err
}

// RX records received bytes.
func (r *CaseRecorder) RX(p []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.rx.Write(p); err != nil {
		return err
	}
	return r.line("rx", p)
}

// TX audits and records bytes about to be sent. It refuses once finished.
func (r *CaseRecorder) TX(p []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return ErrRecorderFinished
	}
	if _, err := r.c.Audit.Append("device.modify", r.deviceID, map[string]any{
		"operation": "serial.write", "acquisition_id": r.acqID, "bytes": len(p), "hex": hex.EncodeToString(p),
	}); err != nil {
		return err
	}
	if err := r.line("tx", p); err != nil {
		// Audited but never sent: close the record so it is not left open.
		return errors.Join(err, r.outcome(0, err))
	}
	r.pending = len(p)
	return nil
}

// TXResult records the outcome of the write TX allowed: n bytes accepted by
// the port and its error. Fewer bytes than audited is io.ErrShortWrite.
func (r *CaseRecorder) TXResult(n int, err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil && n < r.pending {
		err = io.ErrShortWrite
	}
	r.pending = 0
	return r.outcome(n, err)
}

func (r *CaseRecorder) outcome(n int, err error) error {
	d := map[string]any{"operation": "serial.write", "acquisition_id": r.acqID, "bytes_sent": n}
	action := "device.modify.done"
	if err != nil {
		action, d["error"] = "device.modify.error", err.Error()
	}
	_, aerr := r.c.Audit.Append(action, r.deviceID, d)
	return aerr
}

// Finish closes both artifacts (aborting them as incomplete if cause != nil).
// After Finish, TX refuses.
func (r *CaseRecorder) Finish(cause error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = true
	end := func(w *evidence.ArtifactWriter) error {
		if cause != nil {
			_, err := w.Abort(cause)
			return err
		}
		_, err := w.Close()
		return err
	}
	return errors.Join(end(r.rx), end(r.tr))
}
