// Package evidencetest holds the case fixtures and named tamper helpers the
// internal/evidence tests need. It imports only internal/evidence and is
// imported from _test.go files only (a test-only package, see archtest).
package evidencetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// NewCase creates a case in t.TempDir() at the current schema, closed by t.Cleanup.
func NewCase(t testing.TB) *evidence.Case {
	t.Helper()
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "EVCASE", Examiner: "Examiner"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ImageDevice and ImageAcq are the device and acquisition ids AddImage uses.
const (
	ImageDevice = "dev1"
	ImageAcq    = "imp1"
)

// AddImage imports data as a one-segment image artifact (kind "import",
// segment 1 of 1) through Capture and returns its manifest record.
func AddImage(t testing.TB, c *evidence.Case, data []byte) evidence.ManifestRecord {
	t.Helper()
	src := evidence.Source{Kind: "import", DeviceID: ImageDevice, OriginalPath: "disk.img", Segment: 1, Segments: 1}
	rec, err := c.Capture(ImageDevice, ImageAcq, "disk.img", src, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// RecoveredSpec describes a recovered artifact for AddRecovered. Empty fields
// take the defaults noted beside them.
type RecoveredSpec struct {
	Kind, Class, Method, Scope string // defaults: recover, deleted-file, fat-contiguous, ""
	Confidence                 *int   // default 55 (use Mutate to make it nil)
	// Path is relative to the analysis directory; default "recovered/p1-mtfs/000001-a.bin".
	Path            string
	Partition       int // default 1
	PartitionOffset int64
	// ParentSegments is recorded as Derived.ParentSegments (every segment of a split parent).
	ParentSegments []evidence.SegmentRef
	FSType         string // default "mtfs"
	Runs           []evidence.Run
	// Sidecar writes Runs as a runs sidecar artifact (kind runs) and points RunsArtifact at it.
	Sidecar bool
	// Data are the artifact bytes; default: the parent's bytes at Runs.
	Data       []byte
	Alloc      *evidence.AllocSummary // default {Free: sum(Runs)}
	Incomplete bool
	Error      string
	Mutate     func(*evidence.Recovery)
}

// RecoveredAcq is the analysis (acquisition) id AddRecovered writes under.
const RecoveredAcq = "rec1"

// AddRecovered writes a recovered artifact through Capture with the crafted
// Source, as a writer with a bug would: manifest, artifacts.db and the audit
// entry all agree with each other, so only the rules under test can object.
func AddRecovered(t testing.TB, c *evidence.Case, parent evidence.ManifestRecord, parentData []byte, s RecoveredSpec) evidence.ManifestRecord {
	t.Helper()
	if s.Kind == "" {
		s.Kind = evidence.KindRecover
	}
	if s.Class == "" {
		s.Class = evidence.ClassDeletedFile
	}
	if s.Method == "" {
		s.Method = "fat-contiguous"
	}
	if s.Path == "" {
		s.Path = "recovered/p1-mtfs/000001-a.bin"
	}
	if ci, ok := evidence.LookupClass(s.Class); ok && s.Scope == "" {
		s.Scope = ci.Scope // the classes bound to a scope take it from the table
	}
	if s.Partition == 0 {
		s.Partition = 1
	}
	if s.FSType == "" {
		s.FSType = "mtfs"
	}
	conf := 55
	if s.Confidence != nil {
		conf = *s.Confidence
	}
	var sum int64
	var fromRuns bytes.Buffer
	for _, r := range s.Runs {
		sum += r.Length
		if r.Offset >= 0 && r.Length >= 0 && r.Offset+r.Length <= int64(len(parentData)) {
			fromRuns.Write(parentData[r.Offset : r.Offset+r.Length])
		}
	}
	data := s.Data
	if data == nil {
		data = fromRuns.Bytes()
	}
	alloc := evidence.AllocSummary{Free: sum}
	if s.Alloc != nil {
		alloc = *s.Alloc
	}
	rec := &evidence.Recovery{
		Class: s.Class, Method: s.Method, Confidence: &conf, Basis: []string{"dirent:1:1"},
		Alloc: alloc, Scope: s.Scope, Algorithm: evidence.AlgorithmRecover,
	}
	if s.Mutate != nil {
		s.Mutate(rec)
	}
	d := &evidence.Derivation{
		ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: s.Partition,
		PartitionOffset: s.PartitionOffset, FSType: s.FSType, FSPath: "/deleted/a.bin", FSID: "dentry:1:1:1",
		ParentSegments: s.ParentSegments,
		Recovery:       rec,
	}
	dev := parent.Source.DeviceID
	if s.Sidecar {
		var buf bytes.Buffer
		for _, r := range s.Runs {
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		sd := &evidence.Derivation{ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: s.Partition, PartitionOffset: s.PartitionOffset, FSType: s.FSType}
		sc, err := c.Capture(dev, RecoveredAcq, s.Path+".runs.jsonl", evidence.Source{Kind: "runs", DeviceID: dev, Derived: sd}, func(w io.Writer) error {
			_, err := w.Write(buf.Bytes())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		d.RunsArtifact = sc.ID
	} else {
		d.Runs = slices.Clone(s.Runs)
	}
	src := evidence.Source{Kind: s.Kind, DeviceID: dev, Derived: d}
	out, err := c.Capture(dev, RecoveredAcq, s.Path, src, func(w io.Writer) error {
		if _, err := w.Write(data); err != nil {
			return err
		}
		if s.Incomplete {
			msg := s.Error
			if msg == "" {
				msg = "evidencetest: not every run was captured"
			}
			return errors.New(msg)
		}
		return nil
	})
	if s.Incomplete {
		if !out.Incomplete {
			t.Fatalf("evidencetest: record %+v err %v is not incomplete", out, err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	return out
}

// RequireProblem fails unless some problem of rep contains substr; it prints every problem.
func RequireProblem(t testing.TB, rep evidence.VerifyReport, substr string) {
	t.Helper()
	for _, p := range rep.Problems {
		if strings.Contains(p, substr) {
			return
		}
	}
	t.Fatalf("no problem contains %q; problems:\n%s", substr, strings.Join(rep.Problems, "\n"))
}

// RequireNoProblem fails when some problem of rep contains substr.
func RequireNoProblem(t testing.TB, rep evidence.VerifyReport, substr string) {
	t.Helper()
	for _, p := range rep.Problems {
		if strings.Contains(p, substr) {
			t.Fatalf("unexpected problem containing %q: %s\nall problems:\n%s", substr, p, strings.Join(rep.Problems, "\n"))
		}
	}
}

// RequireClean fails unless rep has no problem.
func RequireClean(t testing.TB, rep evidence.VerifyReport) {
	t.Helper()
	if len(rep.Problems) != 0 {
		t.Fatalf("verify is not clean:\n%s", strings.Join(rep.Problems, "\n"))
	}
}

// RequireNotice fails unless some notice of rep contains substr.
func RequireNotice(t testing.TB, rep evidence.VerifyReport, substr string) {
	t.Helper()
	for _, n := range rep.Notices {
		if strings.Contains(n, substr) {
			return
		}
	}
	t.Fatalf("no notice contains %q; notices:\n%s", substr, strings.Join(rep.Notices, "\n"))
}
