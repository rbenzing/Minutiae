package protocol

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// CaseRecorder writes serial/rx.bin (raw received bytes) and
// serial/transcript.jsonl (timestamped rx/tx chunks) as case artifacts, and
// audits every transmitted chunk as a device modification.
type CaseRecorder struct {
	mu       sync.Mutex
	c        *evidence.Case
	deviceID string
	rx, tr   *evidence.ArtifactWriter
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
	return &CaseRecorder{c: c, deviceID: deviceID, rx: rx, tr: tr}, nil
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

// TX audits and records bytes about to be sent.
func (r *CaseRecorder) TX(p []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.c.Audit.Append("device.modify", r.deviceID, map[string]any{
		"operation": "serial.write", "bytes": len(p), "hex": hex.EncodeToString(p),
	}); err != nil {
		return err
	}
	return r.line("tx", p)
}

// Finish closes both artifacts (aborting them as incomplete if cause != nil).
func (r *CaseRecorder) Finish(cause error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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
