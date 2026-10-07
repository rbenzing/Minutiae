package sqlitefile_test

// Engine oracle, write-ahead log (plan 3I, Task 13): databases in WAL mode are
// written by the engine with the autocheckpoint off and copied while the
// connection is open; the library reads the copy and the engine reads another
// copy of the same bytes.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// walRun is an engine connection in WAL mode that stays open.
type walRun struct {
	t     *testing.T
	path  string
	db    *sql.DB
	txns  int // write transactions committed so far
	ps    int
	modes map[string]bool
}

func newWALRun(t *testing.T, ps, av int, cacheSize int) *walRun {
	t.Helper()
	r := &walRun{t: t, path: filepath.Join(t.TempDir(), "w.db"), ps: ps, modes: map[string]bool{}}
	r.db = openEngine(t, r.path)
	mustExec(t, r.db, "pragma page_size="+strconv.Itoa(ps))
	mustExec(t, r.db, "pragma auto_vacuum="+strconv.Itoa(av))
	if got := pragmaString(t, r.db, "journal_mode=wal"); got != "wal" {
		t.Fatalf("journal_mode = %q", got)
	}
	mustExec(t, r.db, "pragma wal_autocheckpoint=0")
	if cacheSize != 0 {
		mustExec(t, r.db, "pragma cache_size="+strconv.Itoa(cacheSize))
	}
	return r
}

// tx runs the statements as one write transaction.
func (r *walRun) tx(stmts ...string) {
	r.t.Helper()
	tx, err := r.db.Begin()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			_ = tx.Rollback()
			r.t.Fatalf("engine exec %q: %v", q, err)
		}
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatal(err)
	}
	r.txns++
}

// snap copies the database and its WAL (while the connection is open) and
// returns their bytes.
func (r *walRun) snap() (db, wal []byte) {
	r.t.Helper()
	dir := r.t.TempDir()
	mode := copyFiles(r.t, dir, r.path, r.path+"-wal")
	r.modes[mode] = true
	var err error
	if db, err = os.ReadFile(filepath.Join(dir, filepath.Base(r.path))); err != nil {
		r.t.Fatal(err)
	}
	if wal, err = os.ReadFile(filepath.Join(dir, filepath.Base(r.path)+"-wal")); err != nil {
		r.t.Fatal(err)
	}
	return db, wal
}

// walStep is one transaction of a scenario, with the check point after it.
type walStep struct {
	name  string
	stmts []string
}

// growSteps writes several transactions: inserts, updates, deletes, a schema
// change and a table drop; the database grows.
func growSteps() []walStep {
	return []walStep{
		{"create", []string{"create table a(id integer primary key, v)", "create table b(id integer primary key, v)", "create index ia on a(v)"}},
		{"insert a", []string{rowsInsert("a", 1, 300, 120)}},
		{"insert b", []string{rowsInsert("b", 1, 100, 900)}},
		{"update", []string{"update a set v = v || 'u' where id % 3 = 0", "update b set v = substr(v, 1, 50) where id < 20"}},
		{"delete", []string{"delete from a where id % 5 = 0"}},
		{"insert more", []string{rowsInsert("a", 301, 500, 200)}},
		{"alter", []string{"alter table b add column w default 'dw'", "update b set w = 'set' where id < 10"}},
		{"drop", []string{"drop table b"}},
		{"create c", []string{"create table c(id integer primary key, v)", rowsInsert("c", 1, 50, 3000)}},
	}
}

// shrinkSteps run with auto_vacuum=full: freeing pages truncates the file at
// the commit (the commit frame's database size is smaller).
func shrinkSteps() []walStep {
	return []walStep{
		{"create", []string{"create table a(id integer primary key, v)", "create table b(id integer primary key, v)"}},
		{"insert a", []string{rowsInsert("a", 1, 400, 600)}},
		{"insert b", []string{rowsInsert("b", 1, 400, 600)}},
		{"delete half of a", []string{"delete from a where id % 2 = 0"}},
		{"drop b", []string{"drop table b"}},
		{"grow again", []string{rowsInsert("a", 401, 700, 600)}},
		{"delete all of a", []string{"delete from a"}},
	}
}

func walScenarios() []struct {
	name  string
	ps    int
	av    int
	steps []walStep
} {
	return []struct {
		name  string
		ps    int
		av    int
		steps []walStep
	}{
		{"grow-ps1024", 1024, 0, growSteps()},
		{"grow-ps4096", 4096, 0, growSteps()},
		{"shrink-ps1024-av1", 1024, 1, shrinkSteps()},
		{"shrink-ps4096-av1", 4096, 1, shrinkSteps()},
	}
}

// walCheck compares the library's Live() of (db, wal) with the engine's view of a
// copy of the same bytes.
func walCheck(t *testing.T, label string, db, wal []byte) *sqlitefile.DB {
	t.Helper()
	want, err := engineCopyDump(t, "w.db", db, wal, nil)
	if err != nil {
		t.Fatalf("%s: the engine cannot read its own copy: %v", label, err)
	}
	d := openLibrary(t, db, wal, nil)
	v := d.Live()
	t.Cleanup(v.Release)
	if diff, _ := diffDumps(liveDumpRows(t, v), want); diff != "" {
		t.Fatalf("%s: %s", label, diff)
	}
	return d
}

// TestLiveMatchesEngineWALUncheckpointed: after every transaction Live() equals
// the engine's view of the copy, and WALInfo counts the transactions.
func TestLiveMatchesEngineWALUncheckpointed(t *testing.T) {
	for _, sc := range walScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			r := newWALRun(t, sc.ps, sc.av, 0)
			var lastPages uint32
			shrank := false
			for _, st := range sc.steps {
				r.tx(st.stmts...)
				db, wal := r.snap()
				d := walCheck(t, st.name, db, wal)
				if pc := d.Live().Info().PageCount; pc < lastPages {
					shrank = true
				} else if pc > lastPages {
					lastPages = pc
				}
				w := d.Status().WAL
				if w == nil || !w.Present || !w.HeaderValid || !w.UsedByLive {
					t.Fatalf("%s: WAL status %+v", st.name, w)
				}
				if int(w.Commits) != r.txns || w.FramesUncommitted != 0 || w.LastCommit == 0 || w.LastCommit != w.FramesCommitted || w.LastCommit != w.FramesValid {
					t.Errorf("%s: WAL commits %d (scenario ran %d), last commit %d, committed %d, valid %d, uncommitted %d",
						st.name, w.Commits, r.txns, w.LastCommit, w.FramesCommitted, w.FramesValid, w.FramesUncommitted)
				}
				if w.FramesBroken+w.FramesDetached+w.FramesStale != 0 || w.TrailingBytes != 0 {
					t.Errorf("%s: damaged frames in an engine-written WAL: %+v", st.name, w)
				}
			}
			if strings.HasPrefix(sc.name, "shrink") && !shrank {
				t.Errorf("the shrinking scenario never shrank the database")
			}
			if len(r.modes) > 1 || !r.modes[copyDirect] {
				t.Logf("copy-while-open fallback used: %v", r.modes)
			}
		})
	}
}

// TestLivePagesEqualCheckpointedDB: after the engine checkpoints (TRUNCATE) a
// copy, every page 1..PageCount of Live() of the original pair is byte-equal to
// the page of the checkpointed database file.
func TestLivePagesEqualCheckpointedDB(t *testing.T) {
	for _, sc := range walScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			r := newWALRun(t, sc.ps, sc.av, 0)
			for i, st := range sc.steps {
				r.tx(st.stmts...)
				if i != len(sc.steps)/2 && i != len(sc.steps)-1 {
					continue
				}
				db, wal := r.snap()
				d := openLibrary(t, db, wal, nil)
				v := d.Live()
				dir := t.TempDir()
				p := filepath.Join(dir, "c.db")
				if err := os.WriteFile(p, db, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p+"-wal", wal, 0o600); err != nil {
					t.Fatal(err)
				}
				e, err := sql.Open("sqlite", p)
				if err != nil {
					t.Fatal(err)
				}
				e.SetMaxOpenConns(1)
				var busy, logFrames, ckpt int
				if err := e.QueryRow("pragma wal_checkpoint(truncate)").Scan(&busy, &logFrames, &ckpt); err != nil || busy != 0 {
					t.Fatalf("checkpoint: busy %d err %v", busy, err)
				}
				_ = e.Close()
				got, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				pages := v.Info().PageCount
				if int64(len(got)) != int64(pages)*int64(sc.ps) {
					t.Errorf("%s: checkpointed file has %d bytes, Live() has %d pages of %d", st.name, len(got), pages, sc.ps)
				}
				for pg := uint32(1); pg <= pages; pg++ {
					lp, err := v.ReadPage(pg)
					if err != nil {
						t.Fatalf("%s: page %d: %v", st.name, pg, err)
					}
					off := int64(pg-1) * int64(sc.ps)
					if off+int64(sc.ps) > int64(len(got)) {
						break
					}
					if string(lp.Data) != string(got[off:off+int64(sc.ps)]) {
						t.Fatalf("%s: page %d of Live() differs from the checkpointed file", st.name, pg)
					}
				}
				v.Release()
			}
		})
	}
}

// TestLiveWALWithSpilledUncommittedFrames: a small page cache and a large open
// transaction make the engine write uncommitted frames into the WAL; Live()
// equals the engine's view, which excludes them.
func TestLiveWALWithSpilledUncommittedFrames(t *testing.T) {
	r := newWALRun(t, 1024, 0, 0)
	r.tx("create table a(id integer primary key, v)", rowsInsert("a", 1, 200, 200))
	mustExec(t, r.db, "pragma cache_size=10")
	mustExec(t, r.db, "pragma cache_spill=10")
	tx, err := r.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{rowsInsert("a", 201, 3000, 400), "update a set v = v || 'x' where id < 100"} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db, wal := r.snap()
	d := walCheck(t, "spilled", db, wal)
	w := d.Status().WAL
	if w == nil || w.FramesUncommitted == 0 {
		t.Fatalf("no uncommitted frame was spilled: %+v", w)
	}
	if int(w.Commits) != r.txns {
		t.Errorf("commits %d, committed transactions %d", w.Commits, r.txns)
	}
	if !hasCode(d.Warnings(), sqlitefile.WarnWALFramesNotApplied) {
		t.Errorf("no wal-frames-not-applied warning: %v", d.Warnings())
	}
}
