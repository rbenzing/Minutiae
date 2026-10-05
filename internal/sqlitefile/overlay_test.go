package sqlitefile_test

// Live() with a WAL attached (plan 3I, Task 8). Every scenario that says what
// the engine would show is checked against the engine itself, which opens a
// COPY of the files in a temporary directory (it may checkpoint or write).

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const ovPS = 1024

// wfix is a builder database with table t(a, b): a = 3*rowid, b = text.
type wfix struct {
	b    *sqlitetest.Builder
	tb   *sqlitetest.Table
	rows map[int64][]any
}

func newWfix(n int64) *wfix {
	f := &wfix{b: sqlitetest.New(sqlitetest.Options{PageSize: ovPS}), rows: map[int64][]any{}}
	f.tb = f.b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= n; id++ {
		f.set(id, byte(id), true)
	}
	return f
}

// set (re)writes row id with text seeded by seed (always 100 bytes).
func (f *wfix) set(id int64, seed byte, insert bool) {
	v := []any{id * 3, longText(100, seed)}
	if insert {
		f.tb.Insert(id, v...)
	} else {
		f.tb.Update(id, v...)
	}
	f.rows[id] = v
}

func (f *wfix) del(id int64) {
	f.tb.Delete(id)
	delete(f.rows, id)
}

func (f *wfix) want() builderRows {
	var out builderRows
	for id := int64(1); id <= 1000; id++ {
		if v, ok := f.rows[id]; ok {
			out.add(id, v...)
		}
	}
	return out
}

func walMode(db []byte) []byte {
	d := bytes.Clone(db)
	d[18], d[19] = 2, 2
	return d
}

func anyOf(v sqlitefile.Value) any {
	switch v.Kind {
	case sqlitefile.KindInt:
		return v.Int
	case sqlitefile.KindFloat:
		return v.Float
	case sqlitefile.KindText:
		s, _ := v.Text()
		return s
	case sqlitefile.KindBlob:
		return append([]byte{}, v.Bytes...)
	}
	return nil
}

func liveRows(t testing.TB, v *sqlitefile.View) [][]any {
	t.Helper()
	ctx := context.Background()
	tbl, err := v.Table(ctx, "t")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	var out [][]any
	if err := tbl.Rows(ctx, func(r sqlitefile.Row) bool {
		vals := tbl.Resolve(r)
		out = append(out, []any{r.Rowid, anyOf(vals[0]), anyOf(vals[1])})
		return true
	}); err != nil {
		t.Fatalf("Rows: %v", err)
	}
	return out
}

func attachLive(t testing.TB, db, wal []byte, o sqlitefile.Options) (*sqlitefile.DB, *sqlitefile.View, *sqlitefile.WALInfo) {
	t.Helper()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), o)
	if err != nil {
		t.Fatal(err)
	}
	info, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal)))
	if err != nil {
		t.Fatalf("AttachWAL: %v", err)
	}
	return d, d.Live(), info
}

// engineAsIs opens copies of db and wal in the engine and returns the rows of t.
func engineAsIs(t testing.TB, db, wal []byte) [][]any {
	t.Helper()
	p := filepath.Join(t.TempDir(), "w.db")
	if err := os.WriteFile(p, db, 0o600); err != nil {
		t.Fatal(err)
	}
	if wal != nil {
		if err := os.WriteFile(p+"-wal", wal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return engineQuery(t, openEngine(t, p), "select rowid, a, b from t order by rowid")
}

// sameAsEngine asserts Live() shows exactly the rows the engine reads from the
// same files (with a WAL-mode header, as a real WAL database has).
func sameAsEngine(t *testing.T, what string, db, wal []byte, o sqlitefile.Options) *sqlitefile.View {
	t.Helper()
	db = walMode(db)
	_, v, _ := attachLive(t, db, wal, o)
	expectRows(t, what+": engine vs live", engineAsIs(t, db, wal), liveRows(t, v))
	return v
}

// walFramePages reads the page numbers of the frames of a WAL file with its
// own parse of the frame headers.
func walFramePages(wal []byte, ps int) []uint32 {
	var out []uint32
	for off := 32; off+24+ps <= len(wal); off += 24 + ps {
		out = append(out, binary.BigEndian.Uint32(wal[off:]))
	}
	return out
}

func changedPages(since *sqlitetest.Image, b *sqlitetest.Builder) []uint32 {
	var out []uint32
	now := b.Snapshot()
	for n := uint32(1); n <= now.Pages(); n++ {
		if n > since.Pages() || !bytes.Equal(since.Page(n), now.Page(n)) {
			out = append(out, n)
		}
	}
	return out
}

func equalRows(a, b [][]any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		for j := range a[i] {
			if x, ok := a[i][j].([]byte); ok {
				if y, ok := b[i][j].([]byte); !ok || !bytes.Equal(x, y) {
					return false
				}
			} else if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// twoTxns builds a database of 40 rows and a WAL of two transactions, each
// rewriting the first and the last row (two leaves, so two frames each).
func twoTxns(t *testing.T) (f *wfix, db []byte, w *sqlitetest.WAL) {
	t.Helper()
	f = newWfix(40)
	db = f.b.Bytes()
	s0 := f.b.Snapshot()
	w = f.b.NewWAL(false, 0x51, 0x52, 0)
	f.set(1, 101, false)
	f.set(40, 102, false)
	f.b.CommitTo(w, s0)
	s1 := f.b.Snapshot()
	f.set(1, 111, false)
	f.set(40, 112, false)
	f.b.CommitTo(w, s1)
	if p := walFramePages(w.Bytes(), ovPS); len(p) != 4 {
		t.Fatalf("scenario needs 4 frames, has pages %v", p)
	}
	return f, db, w
}

func TestLiveWithoutWALEqualsDB(t *testing.T) {
	f := newWfix(30)
	db := walMode(f.b.Bytes())
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.WAL() != nil {
		t.Fatal("WAL() must be nil before an attach")
	}
	v := d.Live()
	if got, want := v.Info(), d.Info(); got.PageCount != want.PageCount || got.FilePages != want.FilePages || got.SchemaCookie != want.SchemaCookie {
		t.Errorf("Live info %+v, DB info %+v", got, want)
	}
	expectRows(t, "live without WAL", engineAsIs(t, db, nil), liveRows(t, v))
}

func TestLiveAppliesCommittedFrames(t *testing.T) {
	f, db, w := twoTxns(t)
	wal := w.Bytes()
	v := sameAsEngine(t, "two commits", db, wal, sqlitefile.Options{})
	expectRows(t, "live vs builder", liveRows(t, v), f.want())

	ctx := context.Background()
	tbl, err := v.Table(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	row, ok, err := tbl.Get(ctx, 40)
	if err != nil || !ok {
		t.Fatalf("Get 40: %v %v", ok, err)
	}
	l := row.Loc
	if l.File != sqlitefile.FileWAL || l.Frame == 0 {
		t.Fatalf("Loc %+v: want the WAL with a frame", l)
	}
	if got := binary.BigEndian.Uint32(wal[32+(int(l.Frame)-1)*(24+ovPS):]); got != l.Page {
		t.Errorf("frame %d holds page %d, Loc says page %d", l.Frame, got, l.Page)
	}
	inPage := int(l.Offset - l.PageOffset)
	pageInWAL := wal[l.PageOffset : l.PageOffset+ovPS]
	cell := wal[l.Offset : l.Offset+l.Length]
	if !bytes.Equal(cell, pageInWAL[inPage:inPage+int(l.Length)]) || !bytes.Contains(cell, []byte(longText(100, 112))) {
		t.Errorf("the bytes at Loc.Offset of the WAL file are not the cell")
	}
	if !bytes.Equal(pageInWAL, f.b.PageBytes(l.Page)) {
		t.Errorf("the WAL frame is not the builder's page %d", l.Page)
	}
}

func TestLivePageSourceLocations(t *testing.T) {
	_, db, w := twoTxns(t)
	wal := w.Bytes()
	_, v, _ := attachLive(t, walMode(db), wal, sqlitefile.Options{})
	inWAL := map[uint32]uint32{}
	for i, p := range walFramePages(wal, ovPS) {
		inWAL[p] = uint32(i + 1) // the newest slot wins
	}
	for pg := uint32(1); pg <= v.Addressable(); pg++ {
		p, err := v.ReadPage(pg)
		if err != nil {
			t.Fatalf("page %d: %v", pg, err)
		}
		if slot, ok := inWAL[pg]; ok {
			want := sqlitefile.PageLoc{File: sqlitefile.FileWAL, Offset: int64(32 + (int(slot)-1)*(24+ovPS) + 24), Frame: slot}
			if p.Loc != want || !bytes.Equal(p.Data, wal[want.Offset:want.Offset+ovPS]) {
				t.Errorf("page %d: loc %+v, want %+v", pg, p.Loc, want)
			}
		} else if p.Loc.File != sqlitefile.FileDB || p.Loc.Offset != int64(pg-1)*ovPS {
			t.Errorf("page %d: loc %+v, want the database", pg, p.Loc)
		}
	}
}

func TestOverflowProvenanceMixedFileFrames(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: ovPS})
	tb := b.CreateTable("t", "create table t(a, b)")
	blob := longBlob(5000, 7)
	tb.Insert(1, int64(3), blob)
	tb.Insert(2, int64(6), []byte("x"))
	db := b.Bytes()
	ov := tb.Overflow(1)
	if len(ov) < 4 {
		t.Fatalf("scenario needs an overflow chain, has %v", ov)
	}
	s0 := b.Snapshot()
	w := b.NewWAL(false, 1, 2, 0)
	// Same bytes up to past the cell's local part, different after it: only
	// the overflow pages change and the leaf stays in the database.
	nb := bytes.Clone(blob)
	for i := 1100; i < len(nb); i++ {
		nb[i] ^= 0x5a
	}
	tb.Update(1, int64(3), nb)
	b.CommitTo(w, s0)
	wal := w.Bytes()
	slotOf := map[uint32]uint32{}
	for i, p := range walFramePages(wal, ovPS) {
		slotOf[p] = uint32(i + 1)
	}
	if _, leafChanged := slotOf[tb.Root()]; leafChanged {
		t.Fatal("scenario error: the leaf changed")
	}
	_, v, _ := attachLive(t, walMode(db), wal, sqlitefile.Options{})
	ctx := context.Background()
	tbl, err := v.Table(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	row, ok, err := tbl.Get(ctx, 1)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	l := row.Loc
	if l.File != sqlitefile.FileDB || !l.OverflowMixed || l.OverflowTotal != len(ov) {
		t.Fatalf("Loc %+v", l)
	}
	for i, p := range l.Overflow {
		if p.Page != ov[i] {
			t.Fatalf("overflow %d is page %d, builder says %d", i, p.Page, ov[i])
		}
		slot, inWAL := slotOf[p.Page]
		switch {
		case inWAL && (p.At.File != sqlitefile.FileWAL || p.At.Frame != slot):
			t.Errorf("overflow page %d: %+v, want WAL frame %d", p.Page, p.At, slot)
		case !inWAL && p.At.File != sqlitefile.FileDB:
			t.Errorf("overflow page %d: %+v, want the database", p.Page, p.At)
		}
	}
	if got := tbl.Resolve(row); !bytes.Equal(got[1].Bytes, nb) {
		t.Errorf("the value is not the rewritten blob")
	}
	expectRows(t, "overflow", engineAsIs(t, walMode(db), wal), liveRows(t, v))
}

func TestLiveIgnoresUncommittedFrames(t *testing.T) {
	f, db, w := twoTxns(t)
	junk := bytes.Clone(f.b.PageBytes(2))
	for i := 100; i < 200; i++ {
		junk[i] ^= 0xff
	}
	// Valid-chain frames after the last commit, no commit frame.
	w.Frame(2, junk, 0)
	w.Frame(3, junk, 0)
	v := sameAsEngine(t, "uncommitted tail", db, w.Bytes(), sqlitefile.Options{})
	expectRows(t, "uncommitted vs builder", liveRows(t, v), f.want())
	if !viewWarns(v, sqlitefile.WarnWALFramesNotApplied, 0) {
		t.Errorf("the ignored frames must be warned about: %v", warnCodesOf(v))
	}
}

func TestLiveStopsAtLastCommitBeforeBadFrame(t *testing.T) {
	// three transactions of two frames each: commits at frames 2, 4 and 6.
	f := newWfix(40)
	db := f.b.Bytes()
	w := f.b.NewWAL(false, 5, 6, 0)
	var states []builderRows
	prev := f.b.Snapshot()
	for txn := byte(1); txn <= 3; txn++ {
		f.set(1, 100+txn, false)
		f.set(40, 110+txn, false)
		f.b.CommitTo(w, prev)
		prev = f.b.Snapshot()
		states = append(states, f.want())
	}
	for _, tc := range []struct {
		flip  int // the frame that gets a bit flip
		state int // the transaction whose state Live must show
	}{{5, 1}, {6, 1}, {3, 0}, {4, 0}} {
		wal := w.Bytes()
		wal[32+(tc.flip-1)*(24+ovPS)+24+10] ^= 0x04
		v := sameAsEngine(t, "bit flip", db, wal, sqlitefile.Options{})
		expectRows(t, "flip vs builder", liveRows(t, v), states[tc.state])
		if !viewWarns(v, sqlitefile.WarnWALFramesNotApplied, 0) {
			t.Errorf("flip %d: no wal-frames-not-applied warning", tc.flip)
		}
	}
}

func TestLiveIgnoresStaleGeneration(t *testing.T) {
	f := newWfix(40)
	db := f.b.Bytes()
	s0 := f.b.Snapshot()
	w := f.b.NewWAL(false, 5, 6, 0)
	f.set(1, 201, false)
	f.set(40, 202, false)
	f.set(20, 203, false)
	f.b.CommitTo(w, s0) // old generation: three frames
	// New generation on top of the base database: one transaction. The old
	// frames lie beyond the new end with the old salts.
	g := newWfix(40)
	w.Reset(7, 8)
	g.set(5, 211, false)
	g.b.CommitTo(w, s0)
	wal := w.Bytes()
	if n := (len(wal) - 32) / (24 + ovPS); n < 3 {
		t.Fatalf("scenario needs stale frames beyond the new end, %d slots", n)
	}
	v := sameAsEngine(t, "stale generation", db, wal, sqlitefile.Options{})
	expectRows(t, "stale vs builder", liveRows(t, v), g.want())
	if !viewWarns(v, sqlitefile.WarnWALFramesNotApplied, 0) {
		t.Errorf("stale frames must be warned about")
	}
}

func TestLiveLatestCommittedFrameWins(t *testing.T) {
	f := newWfix(40)
	db := f.b.Bytes()
	s0 := f.b.Snapshot()
	w := f.b.NewWAL(false, 5, 6, 0)
	f.set(1, 121, false)
	f.set(40, 122, false)
	f.b.CommitTo(w, s0) // frames 1, 2
	s1 := f.b.Snapshot()
	f.set(1, 131, false)
	f.set(40, 132, false)
	f.b.CommitTo(w, s1) // frames 3, 4: the same two pages again
	v := sameAsEngine(t, "latest wins", db, w.Bytes(), sqlitefile.Options{})
	expectRows(t, "latest vs builder", liveRows(t, v), f.want())
	ctx := context.Background()
	tbl, _ := v.Table(ctx, "t")
	row, _, err := tbl.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if row.Loc.Frame != 3 {
		t.Errorf("row 1 comes from frame %d, want the newer frame 3", row.Loc.Frame)
	}
}

func TestLiveDBSizeFromCommitFrame(t *testing.T) {
	t.Run("shrink", func(t *testing.T) {
		f := newWfix(60)
		db := f.b.Bytes()
		big := f.b.Snapshot()
		w := f.b.NewWAL(false, 1, 2, 0)
		for id := int64(21); id <= 60; id++ {
			f.del(id)
		}
		f.b.CommitTo(w, big)
		small := f.b.Snapshot().Pages()
		if small >= big.Pages() {
			t.Fatalf("scenario: %d pages did not shrink from %d", small, big.Pages())
		}
		v := sameAsEngine(t, "shrink", db, w.Bytes(), sqlitefile.Options{})
		expectRows(t, "shrink vs builder", liveRows(t, v), f.want())
		if v.Addressable() != small || v.Info().PageCount != small {
			t.Errorf("addressable %d, page count %d, want %d", v.Addressable(), v.Info().PageCount, small)
		}
		if _, err := v.ReadPage(small + 1); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
			t.Errorf("page beyond the commit's size: %v", err)
		}
	})
	t.Run("grow", func(t *testing.T) {
		f := newWfix(20)
		db := f.b.Bytes()
		small := f.b.Snapshot()
		w := f.b.NewWAL(false, 1, 2, 0)
		for id := int64(21); id <= 80; id++ {
			f.set(id, byte(id), true)
		}
		f.b.CommitTo(w, small)
		big := f.b.Snapshot().Pages()
		v := sameAsEngine(t, "grow", db, w.Bytes(), sqlitefile.Options{})
		expectRows(t, "grow vs builder", liveRows(t, v), f.want())
		if v.Addressable() != big || big <= small.Pages() {
			t.Errorf("addressable %d, want %d (the database has %d)", v.Addressable(), big, small.Pages())
		}
		if p, err := v.ReadPage(big); err != nil || p.Loc.File != sqlitefile.FileWAL {
			t.Errorf("a page that exists only in the WAL: %v %+v", err, p.Loc)
		}
	})
}

// peakBudget records the highest live charge.
type peakBudget struct {
	mu        sync.Mutex
	cur, peak int64
}

func (b *peakBudget) Alloc(n int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cur += n
	b.peak = max(b.peak, b.cur)
	return nil
}

func (b *peakBudget) Free(n int64) {
	b.mu.Lock()
	b.cur -= n
	b.mu.Unlock()
}

func TestLiveCommitFrameSizeClaimIsBounded(t *testing.T) {
	f := newWfix(20)
	db := walMode(f.b.Bytes())
	dbPages := f.b.Snapshot().Pages()
	w := f.b.NewWAL(false, 1, 2, 0)
	w.Frame(2, f.b.PageBytes(2), 0)
	w.Frame(3, f.b.PageBytes(3), 0)
	w.Frame(4, f.b.PageBytes(4), 0xFFFFFFFF)
	pb := &peakBudget{}
	_, v, _ := attachLive(t, db, w.Bytes(), sqlitefile.Options{Budget: pb})
	if v.Addressable() != dbPages {
		t.Errorf("addressable %d, want the database's %d (the frames name pages inside it)", v.Addressable(), dbPages)
	}
	if !viewWarns(v, sqlitefile.WarnPageCountClamped, 0) {
		t.Errorf("page-count-clamped missing: %v", warnCodesOf(v))
	}
	if _, err := v.Layout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pb.peak > 4<<20 {
		t.Errorf("a claim of 2^32-1 pages cost %d bytes of budget", pb.peak)
	}
	// Frames that really supply new pages raise the bound to what they supply.
	w2 := f.b.NewWAL(false, 1, 2, 0)
	w2.Frame(dbPages+3, bytes.Clone(f.b.PageBytes(2)), 0xFFFFFFFF)
	_, v2, _ := attachLive(t, db, w2.Bytes(), sqlitefile.Options{})
	if v2.Addressable() != dbPages+3 {
		t.Errorf("addressable %d, want database pages plus the supplied ones, %d", v2.Addressable(), dbPages+3)
	}
}

func TestLivePage1FromWAL(t *testing.T) {
	f := newWfix(20)
	db := walMode(f.b.Bytes())
	p1 := bytes.Clone(db[:ovPS])
	binary.BigEndian.PutUint32(p1[40:], 77) // schema cookie
	binary.BigEndian.PutUint32(p1[36:], 5)  // freelist count
	binary.BigEndian.PutUint32(p1[60:], 9)  // user version
	w := f.b.NewWAL(false, 1, 2, 0)
	w.Frame(1, p1, f.b.Snapshot().Pages())
	wal := w.Bytes()
	d, v, _ := attachLive(t, db, wal, sqlitefile.Options{})
	in := v.Info()
	if in.SchemaCookie != 77 || in.FreelistCount != 5 || in.UserVersion != 9 {
		t.Errorf("Live info: cookie %d freelist %d user_version %d", in.SchemaCookie, in.FreelistCount, in.UserVersion)
	}
	if old := d.Info(); old.SchemaCookie == 77 || old.FreelistCount == 5 || old.UserVersion == 9 {
		t.Errorf("DB.Info must keep the as-found values: %+v", old)
	}
	// The engine reads the same header values from the same files.
	p := filepath.Join(t.TempDir(), "w.db")
	if err := os.WriteFile(p, db, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+"-wal", wal, 0o600); err != nil {
		t.Fatal(err)
	}
	e := openEngine(t, p)
	for pragma, want := range map[string]string{"schema_version": "77", "freelist_count": "5", "user_version": "9"} {
		if got := pragmaString(t, e, pragma); got != want {
			t.Errorf("engine %s = %s, want %s", pragma, got, want)
		}
	}
}

func TestWALHeaderInvalidIgnored(t *testing.T) {
	_, db, w := twoTxns(t)
	wal := w.Bytes()
	wal[0] ^= 0xff // not a WAL magic any more
	d, v, info := attachLive(t, walMode(db), wal, sqlitefile.Options{})
	if info.UsedByLive || info.HeaderValid {
		t.Errorf("info %+v", info)
	}
	if !viewWarns(v, sqlitefile.WarnWALHeaderInvalid, 0) {
		t.Errorf("warnings %v", warnCodesOf(v))
	}
	expectRows(t, "invalid header: engine vs live", engineAsIs(t, walMode(db), wal), liveRows(t, v))
	expectRows(t, "invalid header: live vs database", liveRows(t, d.Live()), liveRows(t, v))
}

func TestWALPageSizeMismatchIgnored(t *testing.T) {
	// A header whose page size was changed without re-sealing the checksum is
	// not a header: the engine ignores the log, and so does Live.
	_, db, w := twoTxns(t)
	wal := w.Bytes()
	binary.BigEndian.PutUint32(wal[8:], 2048)
	_, v, info := attachLive(t, walMode(db), wal, sqlitefile.Options{})
	if info.UsedByLive || info.HeaderValid {
		t.Errorf("info %+v", info)
	}
	if !viewWarns(v, sqlitefile.WarnWALPageSizeMismatch, 0) || !viewWarns(v, sqlitefile.WarnWALHeaderInvalid, 0) {
		t.Errorf("warnings %v", warnCodesOf(v))
	}
	expectRows(t, "engine ignores it", engineAsIs(t, walMode(db), wal), liveRows(t, v))
	base := newWfix(40)
	expectRows(t, "equals the database", liveRows(t, v), base.want())
}

// TestWALSoundOtherPageSizeAppliedLikeEngine: a sound log of a larger page
// size is applied by the engine, which reads the first database-page-size
// bytes of each frame (recorded by TestEngineWALPageSizeMismatch). Live does
// the same and says so.
func TestWALSoundOtherPageSizeAppliedLikeEngine(t *testing.T) {
	f := newWfix(40)
	db := f.b.Bytes()
	s0 := f.b.Snapshot()
	f.set(40, 150, false)
	pages := changedPages(s0, f.b)
	w := sqlitetest.New(sqlitetest.Options{PageSize: 2 * ovPS}).NewWAL(false, 1, 2, 0)
	for i, pg := range pages {
		padded := make([]byte, 2*ovPS)
		copy(padded, f.b.PageBytes(pg))
		var commit uint32
		if i == len(pages)-1 {
			commit = s0.Pages()
		}
		w.Frame(pg, padded, commit)
	}
	wal := w.Bytes()
	_, v, info := attachLive(t, walMode(db), wal, sqlitefile.Options{})
	if !info.HeaderValid || !info.UsedByLive || info.PageSize != 2*ovPS {
		t.Fatalf("info %+v", info)
	}
	if !viewWarns(v, sqlitefile.WarnWALPageSizeMismatch, 0) {
		t.Errorf("wal-page-size-mismatch missing: %v", warnCodesOf(v))
	}
	got := liveRows(t, v)
	expectRows(t, "other page size: engine vs live", engineAsIs(t, walMode(db), wal), got)
	expectRows(t, "other page size: live vs builder", got, f.want())
}

func TestWALModeMismatchWarns(t *testing.T) {
	_, db, w := twoTxns(t)
	wal := w.Bytes()
	asIs := bytes.Clone(db) // header bytes 18 and 19 say rollback journal
	if asIs[18] != 1 || asIs[19] != 1 {
		t.Fatalf("builder header is %d/%d", asIs[18], asIs[19])
	}
	_, v, _ := attachLive(t, asIs, wal, sqlitefile.Options{})
	if !viewWarns(v, sqlitefile.WarnWALModeMismatch, 0) {
		t.Errorf("wal-mode-mismatch missing: %v", warnCodesOf(v))
	}
	live := liveRows(t, v)
	engine := engineAsIs(t, asIs, wal)
	expectRows(t, "rollback header with a WAL: engine vs live", engine, live)
	// The recorded probe constant says the engine applies the log: the rows
	// differ from the database alone.
	_, plain, _ := attachLive(t, asIs, wal[:32], sqlitefile.Options{})
	if applies := !equalRows(engine, liveRows(t, plain)); applies != engineAppliesWALWithRollbackHeader {
		t.Errorf("engine applies = %v, recorded constant %v", applies, engineAppliesWALWithRollbackHeader)
	}
	// With a header that says WAL mode there is no such warning.
	_, v2, _ := attachLive(t, walMode(db), wal, sqlitefile.Options{})
	if viewWarns(v2, sqlitefile.WarnWALModeMismatch, 0) {
		t.Errorf("a WAL-mode header must not warn")
	}
}

func TestAttachWALTwice(t *testing.T) {
	_, db, w := twoTxns(t)
	d, _, _ := attachLive(t, db, w.Bytes(), sqlitefile.Options{})
	if _, err := d.AttachWAL(bytes.NewReader(w.Bytes()), int64(len(w.Bytes()))); !errors.Is(err, sqlitefile.ErrAlreadyAttached) {
		t.Errorf("second attach: %v", err)
	}
	if d.WAL() == nil || d.WAL().Info.LastCommit != 4 {
		t.Errorf("WAL() %+v", d.WAL())
	}
}

func TestAttachWALAfterLiveSnapshot(t *testing.T) {
	f, db, w := twoTxns(t)
	db = walMode(db)
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	before := d.Live()
	baseRows := liveRows(t, before)
	if _, err := d.AttachWAL(bytes.NewReader(w.Bytes()), int64(len(w.Bytes()))); err != nil {
		t.Fatal(err)
	}
	expectRows(t, "view taken before the attach", liveRows(t, before), baseRows)
	after := liveRows(t, d.Live())
	expectRows(t, "view taken after the attach", after, f.want())
	if equalRows(baseRows, after) {
		t.Error("the attach changed nothing")
	}
}

func TestLiveOverlayConcurrent(t *testing.T) {
	f, db, w := twoTxns(t)
	db = walMode(db)
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wal := w.Bytes()
	shared := d.Live()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
			t.Errorf("attach: %v", err)
		}
	}()
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := d.Live()
			for pg := uint32(1); pg <= v.Addressable(); pg++ {
				if _, err := v.ReadPage(pg); err != nil {
					t.Errorf("page %d: %v", pg, err)
					return
				}
			}
			_, _ = shared.ReadPage(1)
		}()
	}
	wg.Wait()
	expectRows(t, "after the race", liveRows(t, d.Live()), f.want())
}

// TestLiveMatchesEngineWrittenWAL: a WAL written by the engine itself (salts,
// checksums and frame layout as the engine makes them), with and without page
// 1 and file growth in it, read through Live equals what the engine reads.
func TestLiveMatchesEngineWrittenWAL(t *testing.T) {
	for _, grow := range []bool{false, true} {
		f := newWALFixture(t, grow)
		dst := t.TempDir()
		copyFiles(t, dst, f.db, f.wal)
		dbBytes, err := os.ReadFile(filepath.Join(dst, "w.db"))
		if err != nil {
			t.Fatal(err)
		}
		walBytes, err := os.ReadFile(filepath.Join(dst, "w.db-wal"))
		if err != nil {
			t.Fatal(err)
		}
		_, v, info := attachLive(t, dbBytes, walBytes, sqlitefile.Options{})
		if !info.UsedByLive || info.LastCommit == 0 {
			t.Fatalf("grow=%v: the engine's WAL is not used: %+v", grow, info)
		}
		ctx := context.Background()
		tbl, err := v.Table(ctx, "t")
		if err != nil {
			t.Fatal(err)
		}
		var got [][]any
		if err := tbl.Rows(ctx, func(r sqlitefile.Row) bool {
			vals := tbl.Resolve(r)
			got = append(got, []any{anyOf(vals[0]), anyOf(vals[1])})
			return true
		}); err != nil {
			t.Fatal(err)
		}
		want := engineQuery(t, openEngine(t, filepath.Join(dst, "w.db")), "select id, v from t order by id")
		expectRows(t, "engine-written WAL", want, got)
		if len(got) != 60 {
			t.Errorf("grow=%v: %d rows, want 60", grow, len(got))
		}
	}
}

// TestWALSmallerPageSizeSpillsLikeEngine: a sound log of a SMALLER page size
// is applied by the engine too, and it reads a whole database page from each
// frame's data offset, so the bytes after a short frame are whatever follows
// it in the file (measured: the engine shows a damaged leaf, not the old one
// and not the new one). Live reads the same bytes; when they would run past
// the end of the file the page is unavailable (bytes past EOF are never
// zeros), where the engine zero-fills.
func TestWALSmallerPageSizeSpillsLikeEngine(t *testing.T) {
	f := newWfix(40)
	db := walMode(f.b.Bytes())
	s0 := f.b.Snapshot()
	f.set(40, 150, false)
	pages := changedPages(s0, f.b)
	w := sqlitetest.New(sqlitetest.Options{PageSize: ovPS / 2}).NewWAL(false, 1, 2, 0)
	for i, pg := range pages {
		d := make([]byte, ovPS/2)
		copy(d, f.b.PageBytes(pg))
		var commit uint32
		if i == len(pages)-1 {
			commit = s0.Pages()
		}
		w.Frame(pg, d, commit)
	}
	wal := append(w.Bytes(), make([]byte, 2*ovPS)...) // zero bytes the page spills into
	_, v, info := attachLive(t, db, wal, sqlitefile.Options{})
	if !info.UsedByLive || info.PageSize != ovPS/2 {
		t.Fatalf("info %+v", info)
	}
	p, err := v.ReadPage(pages[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := append(bytes.Clone(f.b.PageBytes(pages[0])[:ovPS/2]), make([]byte, ovPS/2)...); !bytes.Equal(p.Data, want) {
		t.Errorf("the page is not the frame's bytes followed by the file's next bytes")
	}
	engine := engineAsIs(t, db, wal)
	if equalRows(engine, engineAsIs(t, db, nil)) || equalRows(engine, func() [][]any { r := f.want(); return [][]any(r) }()) {
		t.Fatalf("the engine's answer is no longer the damaged page; re-measure")
	}
	// Without the trailing bytes the frame's page runs past the end of the file.
	_, v2, _ := attachLive(t, db, w.Bytes(), sqlitefile.Options{})
	if _, err := v2.ReadPage(pages[0]); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("a page running past the end of the WAL: %v", err)
	}
}

func TestStatusReportsAttachedWAL(t *testing.T) {
	_, db, w := twoTxns(t)
	d, _, _ := attachLive(t, db, w.Bytes(), sqlitefile.Options{})
	if s := d.Status(); s.WAL == nil || s.WAL.LastCommit != 4 || !s.WAL.UsedByLive {
		t.Errorf("status %+v", s.WAL)
	}
	d2, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d2.Status().WAL != nil {
		t.Error("no WAL attached, Status().WAL must be nil")
	}
}
