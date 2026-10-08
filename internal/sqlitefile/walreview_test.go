package sqlitefile_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// reseal recomputes the header checksum of the WAL in d (its words are
// big-endian when the magic says so), so a patched field is a sound header
// except for the field itself.
func reseal(d []byte) []byte {
	big := binary.BigEndian.Uint32(d)&1 == 1
	c1, c2 := sqlitefile.WALChecksum(d[:24], big, 0, 0)
	binary.BigEndian.PutUint32(d[24:], c1)
	binary.BigEndian.PutUint32(d[28:], c2)
	return d
}

// TestWALHeaderResealedInvalid (Task 7 review, M-3): a header whose checksum is
// correct but whose magic, version or page size is not a WAL's is not a header.
// The engine ignores such a log (page size, magic) or refuses to open the
// database (version); TestEngineWALHeaderResealed records which.
func TestWALHeaderResealedInvalid(t *testing.T) {
	spec := []walSpec{{2, 0}, {3, 4}}
	for _, c := range []struct {
		name  string
		patch func(d []byte)
	}{
		{"magic", func(d []byte) { d[1] ^= 0x10 }},
		{"version", func(d []byte) { binary.BigEndian.PutUint32(d[4:], 3007001) }},
		{"page size not a power of two", func(d []byte) { binary.BigEndian.PutUint32(d[8:], 1000) }},
		{"page size below 512", func(d []byte) { binary.BigEndian.PutUint32(d[8:], 256) }},
		{"page size above 65536", func(d []byte) { binary.BigEndian.PutUint32(d[8:], 131072) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := buildWAL(512, false, 1, 2, spec).Bytes()
			c.patch(d)
			reseal(d)
			s := scanBytes(t, d, 512, sqlitefile.Options{})
			if s.Info.HeaderValid || s.Info.FramesValid != 0 || !warnHas(s.Warnings, sqlitefile.WarnWALHeaderInvalid) {
				t.Errorf("%+v %v", s.Info, s.Warnings)
			}
		})
	}
	// Control: a re-sealed but otherwise untouched header stays valid, for both byte orders.
	for _, big := range []bool{false, true} {
		d := reseal(buildWAL(512, big, 1, 2, spec).Bytes())
		if s := scanBytes(t, d, 512, sqlitefile.Options{}); !s.Info.HeaderValid || s.Info.LastCommit != 2 {
			t.Errorf("big=%v: %+v", big, s.Info)
		}
	}
}

// TestWALFrameSalt2AloneDiffers (M-4): both salts must match the header's.
func TestWALFrameSalt2AloneDiffers(t *testing.T) {
	w := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 4}, {4, 0}, {5, 6}})
	w.PatchFrame(3, 15, 0xee) // the last byte of salt-2 of slot 3; salt-1 matches; the checksum does not cover salts
	s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
	want := []sqlitefile.FrameState{sqlitefile.FrameCommitted, sqlitefile.FrameCommitted, sqlitefile.FrameStale, sqlitefile.FrameDetached}
	if !slices.Equal(states(s), want) || s.Info.LastCommit != 2 {
		t.Errorf("states %v, last commit %d", states(s), s.Info.LastCommit)
	}
}

// TestWALCommitFrameDBSizeOne (M-5): a commit frame is any frame with a
// non-zero database size, 1 included.
func TestWALCommitFrameDBSizeOne(t *testing.T) {
	s := scanBytes(t, buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {1, 1}}).Bytes(), 512, sqlitefile.Options{})
	if s.Info.LastCommit != 2 || s.Info.Commits != 1 || s.Info.DBPagesAfterCommit != 1 || s.Frames[1].State != sqlitefile.FrameCommitted {
		t.Errorf("%+v", s.Info)
	}
}

// TestWALPageSize65536 (M-5): the largest page size is a valid header value.
func TestWALPageSize65536(t *testing.T) {
	d := buildWAL(65536, false, 1, 2, []walSpec{{2, 0}, {3, 4}}).Bytes()
	s := scanBytes(t, d, 65536, sqlitefile.Options{})
	if !s.Info.HeaderValid || s.Info.PageSize != 65536 || s.Info.FrameSlots != 2 || s.Info.LastCommit != 2 {
		t.Errorf("%+v", s.Info)
	}
	// Also when the database page size is unknown: the header page size is used.
	if s := scanBytes(t, d, 0, sqlitefile.Options{}); !s.Info.HeaderValid || s.Info.FrameSlots != 2 {
		t.Errorf("db page size 0: %+v", s.Info)
	}
}

// TestWALBrokenFrameStartsAGeneration (M-6): a frame with the header salts
// whose checksum fails does not continue the generation of the frames before it.
func TestWALBrokenFrameStartsAGeneration(t *testing.T) {
	w := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 4}, {4, 0}, {5, 6}})
	orig := w.Bytes()[32+2*(24+512)+24+10]
	w.PatchFrame(3, 24+10, orig^0x04)
	s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
	g := s.Info.Generations
	if len(g) != 2 || g[0].FirstSlot != 1 || g[0].Slots != 2 || g[1].FirstSlot != 3 || g[1].Slots != 2 {
		t.Errorf("generations %+v", g)
	}
}

// walScenario is three commits of table t (rows 1..20 before the log, 21..40,
// 41..60 and 61..80 committed) in a log of the given byte order.
type walScenario struct {
	db0    []byte
	full   []byte
	rows   [3][][]any // after commit A, B, C
	commit [3]int     // slot of the commit frame of A, B, C
	first  [3]int     // first slot of the transaction A, B, C
	ps     int
}

func newWalScenario(t *testing.T, big bool) *walScenario {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tb := b.CreateTable("t", "create table t(a, b)")
	var rows builderRows
	add := func(from, to int64) {
		for id := from; id <= to; id++ {
			text := fmt.Sprintf("row%d-%s", id, longText(120, byte(id)))
			tb.Insert(id, id*3, text)
			rows.add(id, id*3, text)
		}
	}
	add(1, 20)
	s := &walScenario{ps: 1024}
	snap, db0 := b.Snapshot(), b.Bytes()
	s.db0 = db0
	w := b.NewWAL(big, 0x1000, 0x2000, 0)
	slots := 0
	for i := range 3 {
		add(int64(21+20*i), int64(40+20*i))
		b.CommitTo(w, snap)
		snap = b.Snapshot()
		n := (len(w.Bytes()) - 32) / (24 + 1024)
		s.first[i], s.commit[i] = slots+1, n
		slots = n
		s.rows[i] = append([][]any(nil), rows...)
	}
	s.full = w.Bytes()
	return s
}

// TestBuilderWALEdgeCasesMatchEngine extends TestBuilderWALMatchesEngine
// (Task 7 review, M-9): for both checksum byte orders, a salt flip on the first
// frame of a transaction, a bad checksum on the frame after a later commit and
// on a later commit frame, a torn commit frame and a file cut one byte short
// all stop the engine at the previous commit, as the scan says.
func TestBuilderWALEdgeCasesMatchEngine(t *testing.T) {
	for _, big := range []bool{false, true} {
		sc := newWalScenario(t, big)
		slot := func(n int) int { return 32 + (n-1)*(24+sc.ps) }
		check := func(t *testing.T, d []byte, wantCommit int, want [][]any) {
			t.Helper()
			s := scanBytes(t, d, sc.ps, sqlitefile.Options{})
			if int(s.Info.LastCommit) != wantCommit {
				t.Errorf("the scan says last commit %d, want %d (%+v)", s.Info.LastCommit, wantCommit, s.Info)
			}
			expectRows(t, "engine", engineRowsWithWAL(t, sc.db0, d), want)
		}
		name := fmt.Sprintf("big=%v/", big)
		t.Run(name+"clean", func(t *testing.T) { check(t, sc.full, sc.commit[2], sc.rows[2]) })
		t.Run(name+"salt flip on the first frame of transaction B", func(t *testing.T) {
			d := bytes.Clone(sc.full)
			d[slot(sc.first[1])+8] ^= 0x01
			check(t, d, sc.commit[0], sc.rows[0])
		})
		t.Run(name+"bad checksum on the first frame after commit B", func(t *testing.T) {
			d := bytes.Clone(sc.full)
			d[slot(sc.first[2])+16] ^= 0x01
			check(t, d, sc.commit[1], sc.rows[1])
		})
		t.Run(name+"bad checksum on the commit frame of C", func(t *testing.T) {
			d := bytes.Clone(sc.full)
			d[slot(sc.commit[2])+20] ^= 0x01
			check(t, d, sc.commit[1], sc.rows[1])
		})
		t.Run(name+"torn commit frame", func(t *testing.T) {
			check(t, sc.full[:slot(sc.commit[2])+24+500], sc.commit[1], sc.rows[1])
		})
		t.Run(name+"cut one byte short", func(t *testing.T) {
			check(t, sc.full[:len(sc.full)-1], sc.commit[1], sc.rows[1])
		})
	}
}

// engineOpenWAL opens copies of the scenario database and the given log in the
// engine and returns the error of counting the rows of t (nil: 20 rows).
func engineOpenWAL(t *testing.T, db, wal []byte, want int) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), "w.db")
	if err := os.WriteFile(p, db, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+"-wal", wal, 0o600); err != nil {
		t.Fatal(err)
	}
	e := openEngine(t, p)
	var n int
	if err := e.QueryRow("select count(*) from t").Scan(&n); err != nil {
		return err
	}
	if n != want {
		return fmt.Errorf("%d rows, want %d", n, want)
	}
	return nil
}

// TestEngineWALHeaderResealed records what the engine does with a header that
// is re-sealed (correct checksum) but wrong: a wrong magic or header page size
// makes it ignore the log; an unsupported VERSION makes it refuse to open the
// database at all. Live() follows the second (see overlay_test).
func TestEngineWALHeaderResealed(t *testing.T) {
	sc := newWalScenario(t, false)
	for _, c := range []struct {
		name    string
		patch   func(d []byte)
		refuses bool
	}{
		{"wrong magic", func(d []byte) { d[1] ^= 0x10 }, false},
		{"wrong header page size", func(d []byte) { binary.BigEndian.PutUint32(d[8:], 2048) }, false},
		{"unsupported version", func(d []byte) { binary.BigEndian.PutUint32(d[4:], 3007001) }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := bytes.Clone(sc.full)
			c.patch(d)
			reseal(d)
			err := engineOpenWAL(t, walMode(sc.db0), d, 20)
			if c.refuses {
				if err == nil {
					t.Fatal("the engine opened a database whose WAL has an unsupported version")
				}
				t.Logf("engine: %v", err)
				return
			}
			if err != nil {
				t.Errorf("the engine must ignore the log and show the 20 rows of the database: %v", err)
			}
		})
	}
}

// TestEngineWALCommitDBSizeOne: a commit frame of database size 1 is a commit:
// the engine shrinks the database to one page and finds it malformed, as the
// scan (one commit, size 1) and Live() say.
func TestEngineWALCommitDBSizeOne(t *testing.T) {
	sc := newWalScenario(t, false)
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	w := b.NewWAL(false, 0x1000, 0x2000, 0)
	w.Frame(1, bytes.Clone(sc.db0[:1024]), 1)
	err := engineOpenWAL(t, walMode(sc.db0), w.Bytes(), 20)
	if err == nil {
		t.Fatal("the engine still shows the table after a commit that left one page")
	}
	t.Logf("engine: %v", err)
	s := scanBytes(t, w.Bytes(), 1024, sqlitefile.Options{})
	if s.Info.Commits != 1 || s.Info.DBPagesAfterCommit != 1 || s.Info.LastCommit != 1 {
		t.Errorf("%+v", s.Info)
	}
}

// TestLiveRefusesAWALWithUnsupportedVersion: the engine refuses to open a
// database whose WAL header is sound (magic, page size, checksum) but carries an
// unsupported version (TestEngineWALHeaderResealed). Live() must not present the
// database without that WAL as if it were live: every read fails with a typed
// error (ErrEngineRefuses) and the wal-header-invalid warning says why.
// AsFound is the raw view and is unaffected.
func TestLiveRefusesAWALWithUnsupportedVersion(t *testing.T) {
	sc := newWalScenario(t, false)
	wal := bytes.Clone(sc.full)
	binary.BigEndian.PutUint32(wal[4:], 3007001)
	reseal(wal)
	db := walMode(sc.db0)
	_, v, info := attachLive(t, db, wal, sqlitefile.Options{})
	if info.UsedByLive || info.HeaderValid || !info.VersionRefused {
		t.Fatalf("info %+v", info)
	}
	if !viewWarns(v, sqlitefile.WarnWALHeaderInvalid, 0) {
		t.Errorf("wal-header-invalid missing: %v", warnCodesOf(v))
	}
	var re *sqlitefile.EngineRefusalError
	if _, err := v.ReadPage(1); !errors.Is(err, sqlitefile.ErrEngineRefuses) || !errors.As(err, &re) || re.File != sqlitefile.FileWAL {
		t.Errorf("ReadPage(1): %v", err)
	}
	if _, err := v.Table(context.Background(), "t"); !errors.Is(err, sqlitefile.ErrEngineRefuses) {
		t.Errorf("Table: %v", err)
	}
	if _, err := v.Schema(context.Background()); !errors.Is(err, sqlitefile.ErrEngineRefuses) {
		t.Errorf("Schema: %v", err)
	}
	// The database file as found (no WAL attached) still reads its 20 rows.
	_, plain := openLive(t, db, sqlitefile.Options{})
	expectRows(t, "as found", liveRows(t, plain), sc.base20())
	// A wrong magic is not a refusal: the engine ignores the log, so does Live.
	bad := bytes.Clone(sc.full)
	bad[1] ^= 0x10
	reseal(bad)
	_, v2, info2 := attachLive(t, db, bad, sqlitefile.Options{})
	if info2.VersionRefused {
		t.Error("a wrong magic is not a version refusal")
	}
	expectRows(t, "wrong magic", liveRows(t, v2), sc.base20())
}

// base20 are the rows of the database file before the log.
func (s *walScenario) base20() [][]any {
	var out [][]any
	for id := int64(1); id <= 20; id++ {
		out = append(out, []any{id, id * 3, fmt.Sprintf("row%d-%s", id, longText(120, byte(id)))})
	}
	return out
}
