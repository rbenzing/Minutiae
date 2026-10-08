package evidence_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

type runsEnv struct {
	c      *evidence.Case
	parent evidence.ManifestRecord
}

func newRunsEnv(t *testing.T) runsEnv {
	t.Helper()
	c := recordstest.NewCase(t)
	return runsEnv{c: c, parent: recordstest.AddArtifact(t, c, "image.bin", make([]byte, 4096))}
}

func (e runsEnv) index(t *testing.T) *evidence.ManifestIndex {
	t.Helper()
	recs, err := e.c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	return evidence.NewManifestIndex(recs)
}

// withSidecar adds a derived artifact that names sc as its runs sidecar.
func (e runsEnv) withSidecar(t *testing.T, kind, sc string) evidence.ManifestRecord {
	t.Helper()
	src := evidence.Source{Kind: kind, DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: e.parent.ID, ParentSHA256: e.parent.SHA256, Partition: 1, FSType: "mtfs", RunsArtifact: sc,
	}}
	return recordstest.AddDerivedWith(t, e.c, "derived-"+sc, src, []byte("payload"))
}

func (e runsEnv) rawSidecar(t *testing.T, name string, body string) evidence.ManifestRecord {
	t.Helper()
	src := evidence.Source{Kind: "runs", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: e.parent.ID, ParentSHA256: e.parent.SHA256, Partition: 1, FSType: "mtfs",
	}}
	return recordstest.AddDerivedWith(t, e.c, name, src, []byte(body))
}

func TestCheckedRunsInline(t *testing.T) {
	e := newRunsEnv(t)
	rec := recordstest.AddDerived(t, e.c, e.parent, "a", "extract", []byte("hello"), nil)
	got, err := e.c.CheckedRuns(e.index(t), rec)
	if err != nil || !slices.Equal(got, []evidence.Run{{Offset: 0, Length: 5}}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestCheckedRunsNone(t *testing.T) {
	e := newRunsEnv(t)
	empty := recordstest.AddDerived(t, e.c, e.parent, "empty", "extract", nil, nil)
	for name, rec := range map[string]evidence.ManifestRecord{"derived without runs": empty, "not derived": e.parent} {
		if _, err := e.c.CheckedRuns(e.index(t), rec); !errors.Is(err, evidence.ErrNoRuns) {
			t.Errorf("%s: err %v, want ErrNoRuns", name, err)
		}
	}
}

func TestCheckedRunsSidecar(t *testing.T) {
	e := newRunsEnv(t)
	want := []evidence.Run{{Offset: 0, Length: 10}, {Offset: 100, Length: 20}}
	sc := recordstest.AddRunsSidecar(t, e.c, e.parent, "r.jsonl", want)
	rec := e.withSidecar(t, "extract", sc.ID)
	got, err := e.c.CheckedRuns(e.index(t), rec)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestCheckedRunsUnallocatedExport(t *testing.T) {
	e := newRunsEnv(t)
	lines := []evidence.UnallocRun{{Offset: 0, Length: 10, ImageOffset: 512}, {Offset: 10, Length: 5, ImageOffset: 2048}}
	bin, _ := recordstest.AddUnallocatedExport(t, e.c, e.parent, lines, make([]byte, 15))
	got, err := e.c.CheckedRuns(e.index(t), bin)
	if want := []evidence.Run{{Offset: 512, Length: 10}, {Offset: 2048, Length: 5}}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %v, %v want %v", got, err, want)
	}
	// the plain format is not interchangeable with the run map
	plain := recordstest.AddRunsSidecar(t, e.c, e.parent, "plain.jsonl", []evidence.Run{{Offset: 512, Length: 10}})
	rec := e.withSidecar(t, "unallocated", plain.ID)
	if _, err := e.c.CheckedRuns(e.index(t), rec); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("plain sidecar under unallocated: err %v, want ErrIntegrity", err)
	}
}

// E18: the image runs of an export are not required to be disjoint.
func TestCheckedRunsDoNotAssumeDisjointRuns(t *testing.T) {
	e := newRunsEnv(t)
	lines := []evidence.UnallocRun{{Offset: 0, Length: 10, ImageOffset: 100}, {Offset: 10, Length: 10, ImageOffset: 105}}
	bin, _ := recordstest.AddUnallocatedExport(t, e.c, e.parent, lines, make([]byte, 20))
	got, err := e.c.CheckedRuns(e.index(t), bin)
	if want := []evidence.Run{{Offset: 100, Length: 10}, {Offset: 105, Length: 10}}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("overlapping export runs: got %v, %v want %v", got, err, want)
	}
	sc := recordstest.AddRunsSidecar(t, e.c, e.parent, "ov.jsonl", []evidence.Run{{Offset: 0, Length: 10}, {Offset: 5, Length: 10}})
	rec := e.withSidecar(t, "extract", sc.ID)
	if got, err := e.c.CheckedRuns(e.index(t), rec); err != nil || len(got) != 2 {
		t.Errorf("overlapping sidecar runs: %v, %v", got, err)
	}
}

func TestCheckedRunsSidecarSameSizeSwapIsIntegrity(t *testing.T) {
	e := newRunsEnv(t)
	sc := recordstest.AddRunsSidecar(t, e.c, e.parent, "r.jsonl", []evidence.Run{{Offset: 0, Length: 10}})
	rec := e.withSidecar(t, "extract", sc.ID)
	orig, err := os.ReadFile(filepath.Join(e.c.Dir, filepath.FromSlash(sc.Path)))
	if err != nil {
		t.Fatal(err)
	}
	swapped := []byte(strings.Replace(string(orig), "10", "99", 1))
	recordstest.SetArtifactFileBytes(t, e.c.Dir, sc.ID, swapped)
	if _, err := e.c.CheckedRuns(e.index(t), rec); !errors.Is(err, evidence.ErrIntegrity) || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("err %v, want ErrIntegrity naming the hash", err)
	}
}

func TestCheckedRunsSidecarProblems(t *testing.T) {
	good := `{"offset":0,"length":1}` + "\n"
	type setup func(t *testing.T, e runsEnv) (*evidence.ManifestIndex, evidence.ManifestRecord)
	rows := []struct {
		name, want string
		run        setup
	}{
		{"not in manifest", "not in the manifest", func(t *testing.T, e runsEnv) (*evidence.ManifestIndex, evidence.ManifestRecord) {
			return e.index(t), e.withSidecar(t, "extract", "no-such-id")
		}},
		{"id twice", "appears 2 times", func(t *testing.T, e runsEnv) (*evidence.ManifestIndex, evidence.ManifestRecord) {
			sc := e.rawSidecar(t, "s.jsonl", good)
			rec := e.withSidecar(t, "extract", sc.ID)
			recs, _ := e.c.Manifest()
			return evidence.NewManifestIndex(append(recs, sc)), rec
		}},
		{"kind not runs", "not runs", func(t *testing.T, e runsEnv) (*evidence.ManifestIndex, evidence.ManifestRecord) {
			other := recordstest.AddDerived(t, e.c, e.parent, "o", "extract", []byte(good), nil)
			return e.index(t), e.withSidecar(t, "extract", other.ID)
		}},
		{"other parent", "not derived from parent", func(t *testing.T, e runsEnv) (*evidence.ManifestIndex, evidence.ManifestRecord) {
			p2 := recordstest.AddArtifact(t, e.c, "image2.bin", make([]byte, 64))
			src := evidence.Source{Kind: "runs", DeviceID: "dev1", Derived: &evidence.Derivation{ParentID: p2.ID, ParentSHA256: p2.SHA256, Partition: 1, FSType: "mtfs"}}
			sc := recordstest.AddDerivedWith(t, e.c, "s.jsonl", src, []byte(good))
			return e.index(t), e.withSidecar(t, "extract", sc.ID)
		}},
		{"file deleted", "sidecar", func(t *testing.T, e runsEnv) (*evidence.ManifestIndex, evidence.ManifestRecord) {
			sc := e.rawSidecar(t, "s.jsonl", good)
			rec := e.withSidecar(t, "extract", sc.ID)
			if err := os.Remove(filepath.Join(e.c.Dir, filepath.FromSlash(sc.Path))); err != nil {
				t.Fatal(err)
			}
			return e.index(t), rec
		}},
		{"symlink", "sidecar", func(t *testing.T, e runsEnv) (*evidence.ManifestIndex, evidence.ManifestRecord) {
			sc := e.rawSidecar(t, "s.jsonl", good)
			rec := e.withSidecar(t, "extract", sc.ID)
			full := filepath.Join(e.c.Dir, filepath.FromSlash(sc.Path))
			copyPath := filepath.Join(t.TempDir(), "copy")
			if err := os.WriteFile(copyPath, []byte(good), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(full); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(copyPath, full); err != nil {
				t.Skipf("cannot create a symlink here: %v", err)
			}
			return e.index(t), rec
		}},
		{"size differs", "sidecar", func(t *testing.T, e runsEnv) (*evidence.ManifestIndex, evidence.ManifestRecord) {
			sc := e.rawSidecar(t, "s.jsonl", good)
			rec := e.withSidecar(t, "extract", sc.ID)
			recordstest.SetArtifactFileBytes(t, e.c.Dir, sc.ID, []byte(good+good))
			return e.index(t), rec
		}},
		{"line over 256 bytes", "longer than", func(t *testing.T, e runsEnv) (*evidence.ManifestIndex, evidence.ManifestRecord) {
			sc := e.rawSidecar(t, "s.jsonl", `{"offset":0,"length":1}`+strings.Repeat(" ", 300)+"\n")
			return e.index(t), e.withSidecar(t, "extract", sc.ID)
		}},
		{"unknown field", "unknown field", func(t *testing.T, e runsEnv) (*evidence.ManifestIndex, evidence.ManifestRecord) {
			sc := e.rawSidecar(t, "s.jsonl", `{"offset":0,"length":1,"x":1}`+"\n")
			return e.index(t), e.withSidecar(t, "extract", sc.ID)
		}},
		{"too many lines", "more than", func(t *testing.T, e runsEnv) (*evidence.ManifestIndex, evidence.ManifestRecord) {
			sc := e.rawSidecar(t, "s.jsonl", strings.Repeat(good, evidence.MaxRecoveredRuns+1))
			return e.index(t), e.withSidecar(t, "extract", sc.ID)
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			e := newRunsEnv(t)
			ix, rec := row.run(t, e)
			_, err := e.c.CheckedRuns(ix, rec)
			if !errors.Is(err, evidence.ErrIntegrity) || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("err %v, want ErrIntegrity containing %q", err, row.want)
			}
		})
	}
}

func TestCheckedRunsBothInlineAndSidecarIsIntegrity(t *testing.T) {
	e := newRunsEnv(t)
	sc := recordstest.AddRunsSidecar(t, e.c, e.parent, "r.jsonl", []evidence.Run{{Offset: 0, Length: 1}})
	src := evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: e.parent.ID, ParentSHA256: e.parent.SHA256, Partition: 1, FSType: "mtfs",
		Runs: []evidence.Run{{Offset: 0, Length: 1}}, RunsArtifact: sc.ID,
	}}
	rec := recordstest.AddDerivedWith(t, e.c, "both", src, []byte("x"))
	if _, err := e.c.CheckedRuns(e.index(t), rec); !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("err %v, want ErrIntegrity", err)
	}
}

func TestCheckedRunsReadsManifestOnce(t *testing.T) {
	e := newRunsEnv(t)
	sc := recordstest.AddRunsSidecar(t, e.c, e.parent, "r.jsonl", []evidence.Run{{Offset: 0, Length: 3}})
	rec := e.withSidecar(t, "extract", sc.ID)
	ix := e.index(t)
	if err := os.WriteFile(filepath.Join(e.c.Dir, "manifest.jsonl"), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := e.c.CheckedRuns(ix, rec); err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
}
