package evidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// FuzzManifestStrictRoundTrip: whatever record this build marshals is never reported by the strict rule,
// and an arbitrary line never panics it.
func FuzzManifestStrictRoundTrip(f *testing.F) {
	f.Add("id1", "artifacts/a", int64(5), "ab", "cd", "extract", "dev", "/p", "t", false, true, int32(1), int64(0), "err", int64(512))
	f.Add("", "", int64(0), "", "", "", "", "", "", false, false, int32(0), int64(-1), "", int64(0))
	f.Add("i", "p", int64(1<<40), "s", "m", "import", "d", "", "", true, true, int32(-3), int64(7), "e", int64(1))
	f.Fuzz(func(t *testing.T, id, path string, size int64, sha, md5, kind, dev, remote, tm string, incomplete, derived bool, part int32, off int64, errText string, runLen int64) {
		rec := ManifestRecord{
			ID: id, Path: path, Size: size, SHA256: sha, MD5: md5, Started: tm, Finished: tm,
			Incomplete: incomplete, Error: errText,
			Source: Source{Kind: kind, DeviceID: dev, RemotePath: remote, Partition: remote},
		}
		if derived {
			rec.Source.Derived = &Derivation{
				ParentID: id, ParentSHA256: sha, Partition: int(part), PartitionOffset: off, FSPath: remote,
				Runs:  []Run{{Offset: off, Length: runLen}, {Offset: -1, Length: runLen}},
				Times: map[string]string{"modified": tm, "Created": remote},
				AcquisitionGaps: &AcquisitionGapNote{
					Bytes: runLen, Ranges: int(part), First: []Run{{Offset: off, Length: runLen}},
				},
				Snapshot: &SnapshotRef{Name: tm, Xid: uint64(size)},
				Recovery: &Recovery{Class: kind, Method: remote, Basis: []string{tm}, Params: map[string]string{"k": tm}},
			}
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Skip()
		}
		var back ManifestRecord
		if err := json.Unmarshal(b, &back); err != nil {
			t.Skip()
		}
		if p := manifestLineProblem(b, back); p != "" {
			t.Fatalf("a line this build wrote is reported: %s\n%s", p, b)
		}
		if p := manifestLineWalk(b, b); p != "" {
			t.Fatalf("the full walk reports a line this build wrote: %s\n%s", p, b)
		}
		_ = manifestLineWalk([]byte(tm+remote), b) // never panics
		_ = manifestLineWalk([]byte(id), b)
	})
}

// BenchmarkManifestStrictWalk reads a 100,000-line manifest of extract-shaped records with inline runs
// through the strict reader; no threshold (K31).
func BenchmarkManifestStrictWalk(b *testing.B) {
	const lines = 100_000
	runs := make([]Run, 8)
	for i := range runs {
		runs[i] = Run{Offset: int64(i) * 4096, Length: 4096}
	}
	var buf []byte
	for i := range lines {
		rec := ManifestRecord{
			ID: fmt.Sprintf("%032x", i), Path: fmt.Sprintf("artifacts/dev1/x%d/p1-ext4/a%d.bin", i, i), Size: 32768,
			SHA256: fmt.Sprintf("%064x", i), MD5: fmt.Sprintf("%032x", i), Started: "2026-10-23T10:00:00Z", Finished: "2026-10-23T10:00:01Z",
			Source: Source{Kind: "extract", DeviceID: "dev1", Derived: &Derivation{
				ParentID: "p", ParentSHA256: "s", Partition: 1, FSType: "ext4", FSPath: fmt.Sprintf("/a%d.bin", i),
				Mode: 0o644, Times: map[string]string{"modified": "2024-01-02T03:04:05Z"}, Runs: runs,
			}},
		}
		l, err := json.Marshal(rec)
		if err != nil {
			b.Fatal(err)
		}
		buf = append(append(buf, l...), '\n')
	}
	p := filepath.Join(b.TempDir(), "manifest.jsonl")
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		recs, strict, err := readManifestStrict(p)
		if err != nil || len(recs) != lines || len(strict) != 0 {
			b.Fatalf("%d records, %d strict, %v", len(recs), len(strict), err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/lines, "ns/line")
}

// TestManifestLineProblemMissingObject: a whole nested object omitted from the line is tolerated only
// when the record's value is all zero (K25).
func TestManifestLineProblemMissingObject(t *testing.T) {
	zero := ManifestRecord{ID: "z"}
	line, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "source")
	noSource, _ := json.Marshal(m)
	if p := manifestLineProblem(noSource, zero); p != "" {
		t.Errorf("an omitted all-zero source is reported: %s", p)
	}
	nonZero := ManifestRecord{ID: "z", Source: Source{Kind: "import", DeviceID: "d"}}
	if p := manifestLineProblem(noSource, nonZero); p != "source: missing" {
		t.Errorf("an omitted non-zero source: got %q, want %q", p, "source: missing")
	}
}
