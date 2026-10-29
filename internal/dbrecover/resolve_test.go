package dbrecover

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// resolveCase is one database fixture of run_test.go's family: which companions
// exist in the manifest, and which Inputs name them.
type resolveCase struct {
	name         string
	wal, journal bool
	pick         func(e *runEnv) Inputs
}

func resolveCases() []resolveCase {
	return []resolveCase{
		{"db only", false, false, func(e *runEnv) Inputs { return Inputs{DB: e.db.ID} }},
		{"db and wal by name", true, false, func(e *runEnv) Inputs { return Inputs{DB: e.db.ID} }},
		{"db and journal by name", false, true, func(e *runEnv) Inputs { return Inputs{DB: e.db.ID} }},
		{"explicit wal", true, true, func(e *runEnv) Inputs { return Inputs{DB: e.db.ID, WAL: e.wal.ID} }},
		{"explicit journal", true, true, func(e *runEnv) Inputs { return Inputs{DB: e.db.ID, Journal: e.journal.ID} }},
		{"no siblings", true, true, func(e *runEnv) Inputs { return Inputs{DB: e.db.ID, NoSiblings: true} }},
	}
}

// envWith builds the scenario database with only the companions asked for.
func envWith(t *testing.T, wal, journal bool) *runEnv {
	t.Helper()
	c := recordstest.NewCase(t)
	f := newBuilderDB(t)
	e := &runEnv{c: c, f: f, db: addFile(t, c, "/data/x.db", f.db)}
	if wal {
		e.wal = addFile(t, c, "/data/x.db-wal", f.wal)
	}
	if journal {
		e.journal = addFile(t, c, "/data/x.db-journal", f.journal)
	}
	return e
}

func idOf(in []InputInfo, role string) string {
	for _, i := range in {
		if i.Role == role {
			return i.ArtifactID
		}
	}
	return ""
}

func TestResolveSiblingsEqualsRunInputs(t *testing.T) {
	for _, tc := range resolveCases() {
		t.Run(tc.name, func(t *testing.T) {
			e := envWith(t, tc.wal, tc.journal)
			in := tc.pick(e)
			opened := 0
			restore := SetOpenHookForTest(func(string, string) { opened++ })
			got, err := ResolveSiblings(e.c, in)
			restore()
			if err != nil {
				t.Fatal(err)
			}
			if opened != 0 {
				t.Errorf("ResolveSiblings opened %d artifacts, want 0 (manifest only)", opened)
			}
			restore = SetOpenHookForTest(func(string, string) { opened++ })
			res := mustRun(t, e.c, in, Options{})
			restore()
			if opened == 0 {
				t.Error("the open seam never fired in Run: the zero count above proves nothing")
			}
			want := Resolved{
				DB: idOf(res.Inputs, "db"), WAL: idOf(res.Inputs, "wal"), Journal: idOf(res.Inputs, "journal"),
				Siblings: res.Siblings,
			}
			if want.DB == "" {
				t.Fatal("the run reports no database input")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ResolveSiblings %+v, Run inputs %+v", got, want)
			}
		})
	}
}

func TestResolveSiblingsRefusesLikeRun(t *testing.T) {
	ambiguous := func(t *testing.T) *runEnv {
		e := newRunEnv(t)
		src := evidence.Source{Kind: "file", DeviceID: "dev1", RemotePath: "/data/x.db-wal"}
		if _, err := e.c.Capture("dev1", "acq1", "copy2/x.db-wal", src, func(w io.Writer) error { _, err := w.Write(e.f.wal); return err }); err != nil {
			t.Fatal(err)
		}
		return e
	}
	t.Run("ambiguous sibling", func(t *testing.T) {
		e := ambiguous(t)
		_, err := ResolveSiblings(e.c, e.in())
		_, runErr := Run(context.Background(), e.c, e.in(), Options{})
		if refusalReason(err) != ReasonAmbiguousSibling || refusalReason(runErr) != ReasonAmbiguousSibling {
			t.Errorf("resolve %v, run %v: want %s from both", err, runErr, ReasonAmbiguousSibling)
		}
		if !errors.Is(err, ErrAmbiguousSibling) {
			t.Errorf("resolve error %v is not ErrAmbiguousSibling", err)
		}
	})
	t.Run("unusable sibling search", func(t *testing.T) {
		e := newRunEnv(t)
		defer SetSiblingScanForTest(func([]evidence.ManifestRecord, evidence.ManifestRecord) SiblingReport { panic("poisoned") })()
		_, err := ResolveSiblings(e.c, e.in())
		if refusalReason(err) != ReasonSiblingUnusable || !errors.Is(err, ErrSiblingSearch) {
			t.Errorf("resolve %v, want %s", err, ReasonSiblingUnusable)
		}
	})
	t.Run("no-siblings with a named companion", func(t *testing.T) {
		e := newRunEnv(t)
		in := Inputs{DB: e.db.ID, WAL: e.wal.ID, NoSiblings: true}
		_, err := ResolveSiblings(e.c, in)
		if refusalReason(err) != ReasonBadOption {
			t.Errorf("resolve %v, want %s", err, ReasonBadOption)
		}
	})
	t.Run("unknown artifact", func(t *testing.T) {
		e := newRunEnv(t)
		for _, in := range []Inputs{{DB: "NOPE"}, {DB: e.db.ID, WAL: "NOPE"}} {
			_, err := ResolveSiblings(e.c, in)
			if !errors.Is(err, evidence.ErrUnknownArtifact) {
				t.Errorf("resolve %+v: %v, want ErrUnknownArtifact", in, err)
			}
		}
	})
	t.Run("one artifact in two roles", func(t *testing.T) {
		e := newRunEnv(t)
		_, err := ResolveSiblings(e.c, Inputs{DB: e.db.ID, WAL: e.db.ID})
		if refusalReason(err) != ReasonBadOption {
			t.Errorf("resolve %v, want %s", err, ReasonBadOption)
		}
	})
}
