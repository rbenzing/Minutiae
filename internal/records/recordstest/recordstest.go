package recordstest

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

// ArtifactSize is the size the byte ranges of Records fit in: add an artifact
// of at least this many bytes for the records to point at.
const ArtifactSize = 1 << 20

// NewCase creates a case in t.TempDir() at the current schema, closed by t.Cleanup.
func NewCase(t testing.TB) *evidence.Case {
	t.Helper()
	c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: "RECCASE", Examiner: "Examiner"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

var testSource = evidence.Source{Kind: "file", DeviceID: "dev1"}

// AddArtifact stores data as a new artifact (through Case.Capture, so it is
// hashed, in the manifest and in artifacts.db) and returns its manifest record.
func AddArtifact(t testing.TB, c *evidence.Case, name string, data []byte) evidence.ManifestRecord {
	t.Helper()
	rec, err := c.Capture("dev1", "acq1", name, testSource, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// AddIncompleteArtifact is AddArtifact for an artifact whose acquisition failed
// after data was written: it is kept and flagged incomplete.
func AddIncompleteArtifact(t testing.TB, c *evidence.Case, name string, data []byte) evidence.ManifestRecord {
	t.Helper()
	rec, err := c.Capture("dev1", "acq1", name, testSource, func(w io.Writer) error {
		if _, err := w.Write(data); err != nil {
			return err
		}
		return errors.New("recordstest: acquisition cut off")
	})
	if err == nil || !rec.Incomplete {
		t.Fatalf("incomplete artifact: record %+v, err %v", rec, err)
	}
	return rec
}

// Records returns n valid, deterministic records for artifactID (seed picks the
// content): mixed types, tied and untimed timestamps, deleted and recovered
// ones, local-offset and local-unknown bases, extra times and byte ranges that
// fit in an artifact of ArtifactSize bytes.
func Records(artifactID string, n int, seed int64) []records.Record {
	rng := rand.New(rand.NewPCG(uint64(seed), 0x6d696e75)) //nolint:gosec // a deterministic test generator, not security
	types := []string{"message", "call", "contact", "calendar_event", "location", "web_visit", "file", "event", "note"}
	out := make([]records.Record, 0, n)
	for i := 0; i < n; i++ {
		typ := types[rng.IntN(len(types))]
		// the free-form keys sit next to the minimal valid payload of the type, so a binary that links the
		// payload validators ingests the generator's records unchanged (unknown fields are allowed)
		payload := ValidPayload(typ)
		payload["i"], payload["seed"], payload["text"] = i, seed, "t"+strconv.Itoa(rng.IntN(1000))
		payload["nested"] = map[string]any{"k": []any{"a", int64(rng.IntN(100)), true, nil}}
		r := records.Record{
			Type:       typ,
			ArtifactID: artifactID,
			Summary:    "record " + strconv.Itoa(i) + " of seed " + strconv.FormatInt(seed, 10),
			Payload:    payload,
		}
		if rng.IntN(3) > 0 {
			r.SourcePath = "/data/" + strconv.Itoa(rng.IntN(20)) + "/file" + strconv.Itoa(rng.IntN(5)) + ".db"
		}
		if rng.IntN(4) == 0 {
			r.Locator = "sqlite:table=t" + strconv.Itoa(rng.IntN(4)) + ";row=" + strconv.Itoa(i)
		}
		if rng.IntN(2) == 0 {
			off := int64(rng.IntN(ArtifactSize - 4096))
			r.Range = &records.Range{Offset: off, Length: int64(rng.IntN(4096))}
		}
		// ties: only 40 distinct instants, so many records share a timestamp
		if rng.IntN(5) > 0 { // about one in five is untimed
			t0 := time.Unix(1_600_000_000+int64(rng.IntN(40))*3600, int64(rng.IntN(3))*500_000_000).UTC()
			tm := records.Time{T: t0}
			switch rng.IntN(4) {
			case 1:
				tm.Basis, tm.OffsetMin = records.BasisLocalOffset, (rng.IntN(48)-24)*30
			case 2:
				tm.Basis = records.BasisLocalUnknown
			}
			r.Time = &tm
			if rng.IntN(6) == 0 {
				end := tm
				end.T = end.T.Add(time.Minute)
				r.TimeEnd = &end
			}
		}
		for j, kind := range []string{"created", "accessed", "modified"} {
			if rng.IntN(5) == 0 {
				r.Times = append(r.Times, records.NamedTime{Kind: kind, Time: records.Time{
					T:     time.Unix(1_500_000_000+int64(rng.IntN(1000))*60+int64(j), 0).UTC(),
					Basis: records.BasisLocalUnknown,
				}})
			}
		}
		if rng.IntN(7) == 0 {
			r.Deleted = true
			if rng.IntN(2) == 0 {
				r.Recovery = "carve"
			}
		}
		if rng.IntN(3) == 0 {
			c := rng.IntN(101)
			r.Confidence = &c
		}
		if rng.IntN(10) == 0 {
			r.Body = "body of record " + strconv.Itoa(i) + "\né€"
		}
		out = append(out, r)
	}
	return out
}

// Ingest runs one complete ingest of recs for parser p covering artifacts and
// fails the test on any error.
func Ingest(t testing.TB, c *evidence.Case, p records.Parser, artifacts []string, recs []records.Record) records.IngestResult {
	t.Helper()
	ctx := context.Background()
	w, err := records.NewWriter(c, p, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(ctx, records.StartOptions{Artifacts: artifacts}); err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if err := w.Add(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// AddDerived stores data as an artifact derived from parent (a runs-covered copy
// of the parent's first len(data) bytes) through Capture. kind is the source
// kind ("recover", "extract", ...); recovery is the artifact's Recovery or nil.
// The parent must hold at least len(data) bytes.
func AddDerived(t testing.TB, c *evidence.Case, parent evidence.ManifestRecord, name, kind string, data []byte, recovery *evidence.Recovery) evidence.ManifestRecord {
	t.Helper()
	d := &evidence.Derivation{
		ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: 1, FSType: "mtfs",
		FSPath: "/" + name, FSID: "dentry:1:1:1", Recovery: recovery,
	}
	if len(data) > 0 {
		d.Runs = []evidence.Run{{Offset: 0, Length: int64(len(data))}}
	}
	src := evidence.Source{Kind: kind, DeviceID: "dev1", Derived: d}
	rec, err := c.Capture("dev1", "acq-derived", name, src, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// AddRecovered stores a deleted-file artifact derived from parent whose Recovery
// carries confidence (nil = none), consistent with its runs, so only the rule
// under test can object.
func AddRecovered(t testing.TB, c *evidence.Case, parent evidence.ManifestRecord, name string, data []byte, confidence *int) evidence.ManifestRecord {
	t.Helper()
	rv := &evidence.Recovery{
		Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: confidence,
		Basis: []string{"dentry:1:1:1"}, Alloc: evidence.AllocSummary{Free: int64(len(data))},
		Algorithm: evidence.AlgorithmRecover,
	}
	return AddDerived(t, c, parent, name, evidence.KindRecover, data, rv)
}
