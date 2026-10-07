// Package parsertest is the test harness for parsers: a strict emitter that
// validates every record with a real records.Writer, an Input that behaves like
// the host, a determinism check and a set of deliberately misbehaving fake
// parsers for the host's own tests. It is test-only: no non-test file may import
// it (internal/archtest enforces this), because it holds fixtures and parsers
// that must never be linked into the binary.
package parsertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Harness is a temporary case plus the bookkeeping that seals readers when a
// run ends.
type Harness struct {
	T    testing.TB
	Case *evidence.Case

	mu   sync.Mutex
	sets map[*parse.Input]*readerSet

	// test seams
	afterSeal func()
	warnSink  func(ctx context.Context, locator, reason string) error
}

// NewHarness creates a case in t.TempDir() at the current schema, closed by
// t.Cleanup.
func NewHarness(t testing.TB) *Harness {
	t.Helper()
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "PARSETEST", Examiner: "Examiner"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &Harness{T: t, Case: c}
}

// AddAndroidFile stores data as an artifact of a file acquired from an Android
// device, through Case.Capture (so it is hashed, in the manifest and in
// artifacts.db). remotePath is absolute.
func AddAndroidFile(t testing.TB, c *evidence.Case, acq, remotePath string, data []byte) evidence.ManifestRecord {
	t.Helper()
	src := evidence.Source{Kind: "file", DeviceID: "SER1", RemotePath: remotePath}
	rec, err := c.Capture("SER1", acq, strings.TrimPrefix(remotePath, "/"), src, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// FakeHash returns a well-formed parser source hash ("src1:sha256:" and 64 hex
// digits) derived from name.
func FakeHash(name string) string {
	sum := sha256.Sum256([]byte("parsertest:" + name))
	return "src1:sha256:" + hex.EncodeToString(sum[:])
}

// Artifact turns a manifest record into the parse.Artifact the host would hand
// over: the reader is the same sealed type the host uses, and Logical is
// "android:" plus the remote path.
func (h *Harness) Artifact(m evidence.ManifestRecord, data []byte) parse.Artifact {
	return parse.Artifact{
		ID: m.ID, SHA256: m.SHA256, Platform: parse.PlatformAndroid,
		Logical: "android:" + m.Source.RemotePath, Size: m.Size, Incomplete: m.Incomplete,
		Source: parse.SourceInfo{
			Kind: m.Source.Kind, DeviceID: m.Source.DeviceID, RemotePath: m.Source.RemotePath,
			OriginalPath: m.Source.OriginalPath, Partition: m.Source.Partition,
		},
		R: parse.NewSealedReaderAt(bytes.NewReader(data), 0),
	}
}

// readerSet holds every sealed reader handed out for one Input, so Run can seal
// them all when the parse returns. A reader opened after that is sealed at once.
type readerSet struct {
	mu     sync.Mutex
	rs     []*parse.SealedReaderAt
	orig   []parse.Artifact // the bundle as the host holds it: facts and the unwrapped readers, for the after-run checks
	sealed bool
}

func (s *readerSet) wrap(r io.ReaderAt, limit int64) *parse.SealedReaderAt {
	w := parse.NewSealedReaderAt(r, limit)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		w.Seal()
	}
	s.rs = append(s.rs, w)
	return w
}

func (s *readerSet) sealAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealed = true
	for _, r := range s.rs {
		r.Seal()
	}
}

// lookuper is the harness's in-memory parse.Lookuper over the artifacts of one
// bundle. It has no hash check: the host's own tests cover that.
type lookuper struct {
	all   []parse.Artifact // sorted by id; R is the artifact's own reader
	limit int64
	set   *readerSet
}

func (l *lookuper) Find(glob string) []parse.Artifact {
	g, err := parse.CompileGlob(glob)
	if err != nil {
		return nil
	}
	var out []parse.Artifact
	for _, a := range l.all {
		if g.Match(a.Logical) {
			a.R = nil
			out = append(out, a)
		}
	}
	return out
}

func (l *lookuper) Open(a parse.Artifact) (io.ReaderAt, error) {
	for _, have := range l.all {
		if have.ID == a.ID {
			return l.set.wrap(have.R, l.limit), nil
		}
	}
	return nil, fmt.Errorf("parsertest: artifact %q is not in the bundle", a.ID)
}

// Input returns a deep-copied Input (Input.Clone) with default limits and a
// BudgetView of a fresh Budget, for the artifacts of one bundle: primary is the
// first role of p's Meta, others are the other roles. Every reader is a fresh
// parse.SealedReaderAt over the artifact's own: forProbe applies
// Limits.ProbeBytes to each of them (so a parser that reads too much in Probe
// fails with ErrProbeLimit as under the host), and Run seals them all when the
// parse returns, so a retained reader fails with ErrSealed. The Lookuper is a
// small in-memory one over these artifacts.
func (h *Harness) Input(p parse.Parser, primary parse.Artifact, others map[string]parse.Artifact, forProbe bool) *parse.Input {
	h.T.Helper()
	meta := p.Meta()
	if len(meta.Inputs) == 0 {
		h.T.Fatalf("parsertest: %s declares no inputs", meta.Name)
		return nil
	}
	primaryRole := meta.Inputs[0].Role
	lim := parse.DefaultLimits()
	var probe int64
	if forProbe {
		probe = lim.ProbeBytes
	}
	set := &readerSet{}
	lk := &lookuper{limit: probe, set: set}
	seen := map[string]bool{primary.ID: true}
	lk.all = append(lk.all, primary)
	for _, a := range others {
		if !seen[a.ID] {
			seen[a.ID] = true
			lk.all = append(lk.all, a)
		}
	}
	slices.SortFunc(lk.all, func(a, b parse.Artifact) int { return strings.Compare(a.ID, b.ID) })
	set.orig = slices.Clone(lk.all)

	in := &parse.Input{
		Job:       parse.JobInfo{ParseID: "parsertest", Job: 1, Parser: meta.Identity(FakeHash(meta.Name))},
		Primary:   primary,
		Artifacts: map[string]parse.Artifact{},
		Lookup:    lk,
		Limits:    lim,
		Budget:    parse.NewBudget(lim.MemBudget).View(),
	}
	in.Primary.R = set.wrap(primary.R, probe)
	in.Artifacts[primaryRole] = in.Primary
	for role, a := range others {
		if role == primaryRole {
			continue
		}
		a.R = set.wrap(a.R, probe)
		in.Artifacts[role] = a
	}
	c := in.Clone()
	h.mu.Lock()
	if h.sets == nil {
		h.sets = map[*parse.Input]*readerSet{}
	}
	h.sets[c] = set
	h.mu.Unlock()
	return c
}

// readersOf returns the reader set of an Input made by h.Input.
func (h *Harness) readersOf(in *parse.Input) (*readerSet, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sets[in]; ok {
		return s, nil
	}
	return nil, errors.New("parsertest: the Input was not made by Harness.Input")
}
