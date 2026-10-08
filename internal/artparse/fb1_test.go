package artparse_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

// editManifestOnOpen changes manifest.jsonl once, just before the first artifact is opened.
func editManifestOnOpen(t *testing.T, f *rx, edit func([]byte) []byte) func(*artparse.Options) {
	var once sync.Once
	return func(o *artparse.Options) {
		artparse.WithOnOpen(o, func(string) {
			once.Do(func() {
				path := filepath.Join(f.c.Dir, "manifest.jsonl")
				b, err := os.ReadFile(path) //nolint:gosec // inside a temporary test case
				if err != nil {
					t.Error(err)
					return
				}
				if err := os.WriteFile(path, edit(b), 0o600); err != nil { //nolint:gosec // inside a temporary test case
					t.Error(err)
				}
			})
		})
	}
}

// A record of the snapshot that is gone or unreadable when the job opens it means the manifest changed
// under the run: integrity (exit 4), never a plain I/O failure.
func TestManifestRecordMissingAtOpenIsIntegrity(t *testing.T) {
	f := newRx(t, "gone")
	p := &rxCount{WellBehaved: wbp("gone")}
	h := f.host(editManifestOnOpen(t, f, func(b []byte) []byte {
		var out [][]byte
		for _, l := range bytes.Split(b, []byte("\n")) {
			if !bytes.Contains(l, []byte(f.recs["gone"].SHA256)) {
				out = append(out, l)
			}
		}
		return bytes.Join(out, []byte("\n"))
	}), p)
	sum := f.run(h, artparse.Selection{})
	j := jobOf(t, sum, "gone")
	if !j.Integrity || sum.Class() != artparse.ClassIntegrity {
		t.Fatalf("job %+v class %v", j, sum.Class())
	}
	if p.parses.Load() != 0 {
		t.Error("Parse ran")
	}
}

func TestManifestUnreadableAtOpenIsIntegrity(t *testing.T) {
	f := newRx(t, "junk")
	h := f.host(editManifestOnOpen(t, f, func(b []byte) []byte {
		return append(append([]byte{}, b...), []byte("{not json\n")...)
	}), wbp("junk"))
	sum := f.run(h, artparse.Selection{})
	if j := jobOf(t, sum, "junk"); !j.Integrity || sum.Class() != artparse.ClassIntegrity {
		t.Fatalf("job %+v class %v", j, sum.Class())
	}
}

// An integrity refusal of Writer.Start that coincides with a cancel is still integrity.
func TestStartIntegrityBeatsCancel(t *testing.T) {
	f := newRx(t, "ic")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := f.host(func(o *artparse.Options) {
		artparse.WithNewWriter(o, func(*evidence.Case, records.Parser, records.WriterOptions) (artparse.IngestWriter, error) {
			return startFailsWith{cancel: cancel}, nil
		})
	}, wbp("ic"))
	sum, err := h.Run(ctx, artparse.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if j := jobOf(t, sum, "ic"); !j.Integrity || sum.Class() != artparse.ClassIntegrity {
		t.Fatalf("job %+v class %v", j, sum.Class())
	}
}

type startFailsWith struct {
	artparse.IngestWriter
	cancel func()
}

func (s startFailsWith) Start(context.Context, records.StartOptions) error {
	s.cancel()
	return errors.Join(evidence.ErrIntegrity, errors.New("raised next_id"))
}
func (startFailsWith) IngestID() string { return "" }
