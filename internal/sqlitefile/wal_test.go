package sqlitefile_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// naiveWALSum is the test's own checksum: word by word, no loop sharing with
// the library.
func naiveWALSum(data []byte, big bool, s0, s1 uint32) (uint32, uint32) {
	w := func(p []byte) uint32 {
		if big {
			return uint32(p[0])<<24 | uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
		}
		return uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16 | uint32(p[3])<<24
	}
	for len(data) >= 8 {
		x0, x1 := w(data), w(data[4:])
		s0 = s0 + x0 + s1
		s1 = s1 + x1 + s0
		data = data[8:]
	}
	return s0, s1
}

func TestWALChecksumGolden(t *testing.T) {
	in := []byte{1, 0, 0, 0, 2, 0, 0, 0}
	if a, b := sqlitefile.WALChecksum(in, false, 0, 0); a != 1 || b != 3 {
		t.Errorf("little-endian: (%#x, %#x), want (1, 3)", a, b)
	}
	if a, b := sqlitefile.WALChecksum(in, true, 0, 0); a != 0x01000000 || b != 0x03000000 {
		t.Errorf("big-endian: (%#x, %#x), want (0x01000000, 0x03000000)", a, b)
	}
	// A second unit continues from the first: s0 = 1+3+... by hand.
	two := []byte{1, 0, 0, 0, 2, 0, 0, 0, 5, 0, 0, 0, 7, 0, 0, 0}
	// unit 2: s0 = 1 + 5 + 3 = 9; s1 = 3 + 7 + 9 = 19
	if a, b := sqlitefile.WALChecksum(two, false, 0, 0); a != 9 || b != 19 {
		t.Errorf("two units: (%d, %d), want (9, 19)", a, b)
	}
	// Chaining across calls equals one call.
	r := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // deterministic test input
	buf := make([]byte, 96)
	fillRand(r, buf)
	for _, big := range []bool{false, true} {
		a, b := sqlitefile.WALChecksum(buf, big, 11, 22)
		c, d := sqlitefile.WALChecksum(buf[:40], big, 11, 22)
		c, d = sqlitefile.WALChecksum(buf[40:], big, c, d)
		if a != c || b != d {
			t.Errorf("big=%v: chained (%d,%d) differs from one call (%d,%d)", big, c, d, a, b)
		}
	}
}

func TestWALChecksumMatchesBuilderImplementation(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 11)) //nolint:gosec // deterministic test input
	for range 1000 {
		buf := make([]byte, 8*r.IntN(70))
		fillRand(r, buf)
		s0, s1 := r.Uint32(), r.Uint32()
		for _, big := range []bool{false, true} {
			a, b := sqlitefile.WALChecksum(buf, big, s0, s1)
			c, d := naiveWALSum(buf, big, s0, s1)
			if a != c || b != d {
				t.Fatalf("big=%v len %d: library (%d,%d), independent (%d,%d)", big, len(buf), a, b, c, d)
			}
		}
	}
	// And the builder's own implementation: a WAL it writes verifies as a chain.
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	for _, big := range []bool{false, true} {
		w := b.NewWAL(big, 1, 2, 0)
		for i := range 5 {
			w.Frame(uint32(i+1), pageOf(512, byte(i)), 0)
		}
		w.Frame(6, pageOf(512, 9), 6)
		s, err := sqlitefile.ScanWAL(bytes.NewReader(w.Bytes()), int64(len(w.Bytes())), 512, sqlitefile.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !s.Info.HeaderValid || s.Info.FramesValid != 6 || s.Info.LastCommit != 6 {
			t.Errorf("big=%v: the library does not accept the builder's checksums: %+v", big, s.Info)
		}
	}
}

// pageOf returns a page whose bytes depend on seed.
func pageOf(n int, seed byte) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*13) + seed*17 + 1
	}
	return p
}

// walSpec describes frames: page, and commit (database size after it; 0 = none).
type walSpec struct {
	page, commit uint32
}

func buildWAL(ps int, big bool, salt1, salt2 uint32, frames []walSpec) *sqlitetest.WAL {
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	w := b.NewWAL(big, salt1, salt2, 3)
	for i, f := range frames {
		w.Frame(f.page, pageOf(ps, byte(i)), f.commit)
	}
	return w
}

func scanBytes(t testing.TB, data []byte, dbPS int, o sqlitefile.Options) *sqlitefile.WALScan {
	t.Helper()
	s, err := sqlitefile.ScanWAL(bytes.NewReader(data), int64(len(data)), dbPS, o)
	if err != nil {
		t.Fatalf("ScanWAL: %v", err)
	}
	return s
}

func states(s *sqlitefile.WALScan) []sqlitefile.FrameState {
	var out []sqlitefile.FrameState
	for _, f := range s.Frames {
		out = append(out, f.State)
	}
	return out
}

func warnHas(ws []sqlitefile.Warning, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

func TestWALScanCleanCommit(t *testing.T) {
	for _, big := range []bool{false, true} {
		w := buildWAL(512, big, 100, 200, []walSpec{{2, 0}, {3, 0}, {4, 9}})
		s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
		in := s.Info
		if !in.Present || !in.HeaderValid || in.HeaderProblem != "" || in.BigEndianChecksums != big || in.PageSize != 512 || in.Salt1 != 100 || in.Salt2 != 200 || in.CheckpointSeq != 3 {
			t.Errorf("big=%v: header facts %+v", big, in)
		}
		if in.FrameSlots != 3 || in.FramesValid != 3 || in.FramesCommitted != 3 || in.LastCommit != 3 || in.Commits != 1 || in.FramesUncommitted != 0 ||
			in.DBPagesAfterCommit != 9 || in.MaxPageNumber != 4 || in.TrailingBytes != 0 {
			t.Errorf("big=%v: counts %+v", big, in)
		}
		for i, f := range s.Frames {
			if f.State != sqlitefile.FrameCommitted || !f.Linked || f.Slot != uint32(i+1) || f.Offset != int64(32+i*(24+512)) || f.Generation != 0 {
				t.Errorf("frame %d: %+v", i, f)
			}
		}
		if s.Frames[2].DBSize != 9 || s.Frames[0].Page != 2 {
			t.Errorf("frame fields %+v", s.Frames)
		}
		if len(s.Warnings) != 0 {
			t.Errorf("warnings %v", s.Warnings)
		}
		if len(in.Generations) != 1 || !in.Generations[0].Anchored || in.Generations[0].Age != 0 || in.Generations[0].Slots != 3 || in.Generations[0].Commits != 1 {
			t.Errorf("generations %+v", in.Generations)
		}
	}
}

func TestWALScanTwoTransactions(t *testing.T) {
	w := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 5}, {4, 0}, {5, 0}, {6, 7}, {7, 0}, {8, 0}})
	s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
	in := s.Info
	if in.FramesValid != 7 || in.LastCommit != 5 || in.Commits != 2 || in.FramesCommitted != 5 || in.FramesUncommitted != 2 || in.DBPagesAfterCommit != 7 || in.MaxPageNumber != 6 {
		t.Errorf("%+v", in)
	}
	C, U := sqlitefile.FrameCommitted, sqlitefile.FrameUncommitted
	want := []sqlitefile.FrameState{C, C, C, C, C, U, U}
	if !slices.Equal(states(s), want) {
		t.Errorf("states %v, want %v", states(s), want)
	}
}

func TestWALScanUncommittedTail(t *testing.T) {
	w := buildWAL(512, false, 1, 2, []walSpec{{2, 4}, {3, 0}, {4, 0}})
	s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
	if s.Info.LastCommit != 1 || s.Info.FramesUncommitted != 2 || s.Info.MaxPageNumber != 2 {
		t.Errorf("%+v", s.Info)
	}
	if st := states(s); st[0] != sqlitefile.FrameCommitted || st[1] != sqlitefile.FrameUncommitted || st[2] != sqlitefile.FrameUncommitted {
		t.Errorf("states %v", st)
	}
}

func TestWALScanNoCommitAtAll(t *testing.T) {
	w := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 0}})
	s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
	in := s.Info
	if in.LastCommit != 0 || in.FramesCommitted != 0 || in.FramesValid != 2 || in.FramesUncommitted != 2 || in.Commits != 0 || in.MaxPageNumber != 0 || in.DBPagesAfterCommit != 0 {
		t.Errorf("%+v", in)
	}
	for _, f := range s.Frames {
		if f.State != sqlitefile.FrameUncommitted {
			t.Errorf("frame %d is %s", f.Slot, f.State)
		}
	}
}

func TestWALScanStopsAtBadChecksum(t *testing.T) {
	spec := []walSpec{{2, 0}, {3, 4}, {4, 0}, {5, 0}, {6, 7}}
	for k := 1; k <= len(spec); k++ {
		w := buildWAL(512, false, 1, 2, spec)
		orig := w.Bytes()[32+(k-1)*(24+512)+24+10]
		w.PatchFrame(k, 24+10, orig^0x04) // one data bit
		s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
		in := s.Info
		if int(in.FramesValid) != k-1 {
			t.Errorf("k=%d: FramesValid %d", k, in.FramesValid)
		}
		wantCommit := uint32(0)
		for i := 0; i < k-1; i++ {
			if spec[i].commit != 0 {
				wantCommit = uint32(i + 1)
			}
		}
		if in.LastCommit != wantCommit {
			t.Errorf("k=%d: LastCommit %d, want %d", k, in.LastCommit, wantCommit)
		}
		for i, f := range s.Frames {
			slot := i + 1
			var want sqlitefile.FrameState
			switch {
			case slot < k && uint32(slot) <= wantCommit:
				want = sqlitefile.FrameCommitted
			case slot < k:
				want = sqlitefile.FrameUncommitted
			case slot == k:
				want = sqlitefile.FrameBroken
			default:
				want = sqlitefile.FrameDetached
			}
			if f.State != want {
				t.Errorf("k=%d slot %d: %s, want %s", k, slot, f.State, want)
			}
		}
		if in.FramesBroken != 1 || int(in.FramesDetached) != len(spec)-k || in.FramesStale != 0 {
			t.Errorf("k=%d: broken %d detached %d stale %d", k, in.FramesBroken, in.FramesDetached, in.FramesStale)
		}
	}
}

func TestWALScanPageZeroInvalid(t *testing.T) {
	w := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {0, 0}, {4, 5}})
	s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
	if s.Info.FramesValid != 1 || s.Info.LastCommit != 0 {
		t.Errorf("%+v", s.Info)
	}
	if st := states(s); st[1] != sqlitefile.FrameBroken || st[2] != sqlitefile.FrameDetached {
		t.Errorf("states %v", st)
	}
	if !s.Frames[1].Linked {
		t.Error("the page-0 frame still verifies from its predecessor")
	}
}

func TestWALScanSaltMismatchIsStale(t *testing.T) {
	w := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 4}, {4, 0}, {5, 0}, {6, 7}})
	w.PatchFrame(3, 11, 0xee) // salt-1 of slot 3 differs; the checksum does not cover salts
	s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
	if s.Info.FramesValid != 2 || s.Info.LastCommit != 2 {
		t.Errorf("%+v", s.Info)
	}
	want := []sqlitefile.FrameState{sqlitefile.FrameCommitted, sqlitefile.FrameCommitted, sqlitefile.FrameStale, sqlitefile.FrameDetached, sqlitefile.FrameDetached}
	if !slices.Equal(states(s), want) {
		t.Errorf("states %v, want %v", states(s), want)
	}
	if s.Info.FramesStale != 1 || s.Info.FramesDetached != 2 {
		t.Errorf("stale %d detached %d", s.Info.FramesStale, s.Info.FramesDetached)
	}
}

func TestWALScanTornFrame(t *testing.T) {
	w := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 4}, {4, 5}})
	data := w.Bytes()
	data = data[:len(data)-100]
	s := scanBytes(t, data, 512, sqlitefile.Options{})
	if s.Info.FrameSlots != 2 || s.Info.TrailingBytes != 536-100 || len(s.Frames) != 2 || s.Info.LastCommit != 2 {
		t.Errorf("%+v", s.Info)
	}
	if !warnHas(s.Warnings, sqlitefile.WarnWALTornTail) {
		t.Errorf("no wal-torn-tail: %v", s.Warnings)
	}
}

func TestWALScanBigEndianMagic(t *testing.T) {
	spec := []walSpec{{2, 0}, {3, 4}, {4, 0}}
	le := scanBytes(t, buildWAL(512, false, 5, 6, spec).Bytes(), 512, sqlitefile.Options{})
	be := scanBytes(t, buildWAL(512, true, 5, 6, spec).Bytes(), 512, sqlitefile.Options{})
	if !be.Info.BigEndianChecksums || le.Info.BigEndianChecksums {
		t.Error("BigEndianChecksums")
	}
	if be.Info.FramesValid != le.Info.FramesValid || be.Info.LastCommit != le.Info.LastCommit || !slices.Equal(states(be), states(le)) {
		t.Errorf("big %+v little %+v", be.Info, le.Info)
	}
	if be.Frames[0].Check1 == le.Frames[0].Check1 {
		t.Error("the two endiannesses gave the same checksum")
	}
	// Little-endian words read with the big-endian magic do not verify.
	data := buildWAL(512, false, 5, 6, spec).Bytes()
	data[3] |= 1
	bad := scanBytes(t, data, 512, sqlitefile.Options{})
	if bad.Info.HeaderValid {
		t.Error("a header written little-endian was accepted with the big-endian magic")
	}
}

func TestWALHeaderInvalid(t *testing.T) {
	spec := []walSpec{{2, 0}, {3, 4}}
	for _, c := range []struct {
		name  string
		patch func(d []byte) []byte
		slots bool
	}{
		{"bad magic", func(d []byte) []byte { d[0] = 0; return d }, true},
		{"bad version", func(d []byte) []byte { d[7]++; return d }, true},
		{"header checksum off by one", func(d []byte) []byte { d[31] ^= 1; return d }, true},
		{"size below the header", func(d []byte) []byte { return d[:20] }, false},
		{"one byte", func(d []byte) []byte { return d[:1] }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := c.patch(buildWAL(512, false, 1, 2, spec).Bytes())
			s := scanBytes(t, data, 512, sqlitefile.Options{})
			if !s.Info.Present || s.Info.HeaderValid || s.Info.HeaderProblem == "" || !warnHas(s.Warnings, sqlitefile.WarnWALHeaderInvalid) {
				t.Fatalf("%+v %v", s.Info, s.Warnings)
			}
			if s.Info.FramesValid != 0 || s.Info.LastCommit != 0 {
				t.Errorf("an invalid header anchors nothing: %+v", s.Info)
			}
			if !c.slots {
				if len(s.Frames) != 0 || s.Info.FrameSlots != 0 {
					t.Errorf("frames from a file below the header size: %d", len(s.Frames))
				}
				return
			}
			if len(s.Frames) != 2 {
				t.Fatalf("%d frames", len(s.Frames))
			}
			for _, f := range s.Frames {
				if f.State != sqlitefile.FrameUnanchored {
					t.Errorf("slot %d is %s", f.Slot, f.State)
				}
			}
			for _, g := range s.Info.Generations {
				if g.Anchored || g.Age != 0 {
					t.Errorf("generation %+v", g)
				}
			}
		})
	}
}

func TestWALSlotSizeRules(t *testing.T) {
	spec := []walSpec{{2, 0}, {3, 4}, {4, 5}}
	hostile := func(v uint32) []byte {
		d := buildWAL(4096, false, 1, 2, spec).Bytes()
		d[8], d[9], d[10], d[11] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
		return d
	}
	for _, v := range []uint32{100, 3000, 131072, 0, 1 << 31} {
		data := hostile(v)
		cb := &capBudget{limit: 1 << 20}
		s := scanBytes(t, data, 0, sqlitefile.Options{Budget: cb})
		if len(s.Frames) != 0 || s.Info.FrameSlots != 0 || s.Info.TrailingBytes != int64(len(data))-32 || s.Info.PageSize != 0 {
			t.Errorf("page size %d, no database size: %+v", v, s.Info)
		}
		if cb.peak > 4096 {
			t.Errorf("page size %d: %d bytes charged for a file with no slots", v, cb.peak)
		}
		if !warnHas(s.Warnings, sqlitefile.WarnWALHeaderInvalid) {
			t.Errorf("page size %d: no wal-header-invalid", v)
		}
		// With the database's page size the slots follow it and nothing is anchored.
		s = scanBytes(t, data, 4096, sqlitefile.Options{})
		if len(s.Frames) != 3 || s.Info.PageSize != 4096 {
			t.Fatalf("page size %d, database 4096: %d frames, page size %d", v, len(s.Frames), s.Info.PageSize)
		}
		for _, f := range s.Frames {
			if f.State != sqlitefile.FrameUnanchored {
				t.Errorf("page size %d: slot %d is %s", v, f.Slot, f.State)
			}
		}
	}
	t.Run("a valid header page size differing from the database's wins", func(t *testing.T) {
		data := buildWAL(4096, false, 1, 2, spec).Bytes()
		s := scanBytes(t, data, 8192, sqlitefile.Options{})
		if s.Info.PageSize != 4096 || s.Info.FrameSlots != 3 || s.Info.LastCommit != 3 {
			t.Errorf("%+v", s.Info)
		}
		if !warnHas(s.Warnings, sqlitefile.WarnWALPageSizeMismatch) {
			t.Errorf("no mismatch fact: %v", s.Warnings)
		}
		same := scanBytes(t, data, 4096, sqlitefile.Options{})
		if warnHas(same.Warnings, sqlitefile.WarnWALPageSizeMismatch) {
			t.Error("a mismatch warning for equal page sizes")
		}
	})
	t.Run("a database with an invalid header uses the WAL's page size", func(t *testing.T) {
		data := buildWAL(1024, false, 1, 2, spec).Bytes()
		s := scanBytes(t, data, 0, sqlitefile.Options{})
		if s.Info.PageSize != 1024 || s.Info.FrameSlots != 3 || s.Info.LastCommit != 3 || len(s.Warnings) != 0 {
			t.Errorf("%+v %v", s.Info, s.Warnings)
		}
	})
}

func TestWALScanEmptyFile(t *testing.T) {
	s := scanBytes(t, nil, 4096, sqlitefile.Options{})
	if s.Info.Present || s.Info.HeaderValid || len(s.Frames) != 0 || len(s.Warnings) != 0 || s.Info.Size != 0 {
		t.Errorf("%+v %v", s.Info, s.Warnings)
	}
	if _, err := sqlitefile.ScanWAL(bytes.NewReader(nil), -1, 0, sqlitefile.Options{}); err == nil {
		t.Error("a negative size must be refused")
	}
}

// resetScenario writes generation A (6 frames, commit at the last), resets and
// writes generation B (2 frames, commit at the last).
func resetScenario() *sqlitetest.WAL {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	w := b.NewWAL(false, 10, 20, 0)
	for i := range 5 {
		w.Frame(uint32(i+2), pageOf(512, byte(i)), 0)
	}
	w.Frame(7, pageOf(512, 5), 7)
	w.Reset(11, 21)
	w.Frame(2, pageOf(512, 50), 0)
	w.Frame(3, pageOf(512, 51), 7)
	return w
}

func TestWALGenerationsAfterReset(t *testing.T) {
	s := scanBytes(t, resetScenario().Bytes(), 512, sqlitefile.Options{})
	in := s.Info
	if in.FramesValid != 2 || in.LastCommit != 2 || in.FramesStale != 4 || in.FrameSlots != 6 {
		t.Fatalf("%+v", in)
	}
	for i, f := range s.Frames {
		want := sqlitefile.FrameStale
		if i < 2 {
			want = sqlitefile.FrameCommitted
		}
		if f.State != want {
			t.Errorf("slot %d: %s", f.Slot, f.State)
		}
		if wantLinked := i != 2; f.Linked != wantLinked {
			t.Errorf("slot %d: Linked %v", f.Slot, f.Linked)
		}
	}
	if len(in.Generations) != 2 {
		t.Fatalf("generations %+v", in.Generations)
	}
	b, a := in.Generations[0], in.Generations[1]
	if b.Salt1 != 11 || b.Salt2 != 21 || b.FirstSlot != 1 || b.Slots != 2 || !b.Anchored || b.Age != 0 || b.Commits != 1 {
		t.Errorf("current generation %+v", b)
	}
	if a.Salt1 != 10 || a.Salt2 != 20 || a.FirstSlot != 3 || a.Slots != 4 || a.Anchored || a.Age != 1 || a.Commits != 1 {
		t.Errorf("older generation %+v", a)
	}
	if s.Frames[0].Generation != 0 || s.Frames[5].Generation != 1 {
		t.Errorf("frame generations %d %d", s.Frames[0].Generation, s.Frames[5].Generation)
	}
}

func TestWALGenerationAgeOrdersNotBySlot(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	w := b.NewWAL(false, 10, 20, 0)
	for i := range 6 {
		w.Frame(uint32(i+2), pageOf(512, byte(i)), 0)
	}
	w.Reset(11, 21)
	for i := range 4 {
		w.Frame(uint32(i+2), pageOf(512, byte(20+i)), 0)
	}
	w.Reset(12, 22)
	for i := range 2 {
		w.Frame(uint32(i+2), pageOf(512, byte(40+i)), 5)
	}
	s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
	var ages []uint32
	for _, g := range s.Info.Generations {
		ages = append(ages, g.Age)
	}
	if !slices.Equal(ages, []uint32{0, 1, 2}) {
		t.Fatalf("ages %v (generations %+v)", ages, s.Info.Generations)
	}
	if g := s.Info.Generations; g[0].FirstSlot != 1 || g[1].FirstSlot != 3 || g[2].FirstSlot != 5 || g[1].Salt1 != 11 || g[2].Salt1 != 10 {
		t.Errorf("%+v", g)
	}
	// Age is a difference of salt-1 modulo 2^32.
	bb := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	w2 := bb.NewWAL(false, 0xFFFFFFFF, 1, 0)
	w2.Frame(2, pageOf(512, 1), 0)
	w2.Frame(3, pageOf(512, 2), 0)
	w2.Reset(1, 2) // salt-1 wrapped by two
	w2.Frame(2, pageOf(512, 3), 3)
	s2 := scanBytes(t, w2.Bytes(), 512, sqlitefile.Options{})
	if len(s2.Info.Generations) != 2 || s2.Info.Generations[1].Age != 2 {
		t.Errorf("wrapped age: %+v", s2.Info.Generations)
	}
}

func TestWALLinkedNeverPromotes(t *testing.T) {
	s := scanBytes(t, resetScenario().Bytes(), 512, sqlitefile.Options{})
	linkedStale := 0
	for _, f := range s.Frames {
		if f.State == sqlitefile.FrameStale && f.Linked {
			linkedStale++
		}
	}
	if linkedStale != 3 {
		t.Fatalf("%d linked stale frames, the scenario has 3", linkedStale)
	}
	if s.Info.FramesValid != 2 || s.Info.LastCommit != 2 || s.Info.MaxPageNumber != 3 || s.Info.DBPagesAfterCommit != 7 {
		t.Errorf("a linked stale run changed the chain: %+v", s.Info)
	}
}

func TestWALScanMaxPageNumberAndDBSizeAreBounds(t *testing.T) {
	w := buildWAL(512, false, 1, 2, []walSpec{{7, 0}, {2, 0}, {5, 0xFFFFFFFF}})
	cb := &capBudget{limit: 1 << 20}
	s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{Budget: cb})
	if s.Info.DBPagesAfterCommit != 0xFFFFFFFF || s.Info.MaxPageNumber != 7 {
		t.Errorf("%+v", s.Info)
	}
	if cb.peak > 64<<10 {
		t.Errorf("%d bytes charged for 3 frames", cb.peak)
	}
}

func TestWALScanCaps(t *testing.T) {
	spec := make([]walSpec, 10)
	for i := range spec {
		spec[i] = walSpec{uint32(i + 2), 0}
	}
	spec[9].commit = 5
	w := buildWAL(512, false, 1, 2, spec)
	s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{Limits: sqlitefile.Limits{MaxWALFrames: 4}})
	if len(s.Frames) != 4 || s.Info.FrameSlots != 10 || s.Info.FramesValid != 4 || s.Info.LastCommit != 0 {
		t.Errorf("frames %d %+v", len(s.Frames), s.Info)
	}
	if !warnHas(s.Warnings, sqlitefile.WarnLimitReached) {
		t.Errorf("no limit-reached: %v", s.Warnings)
	}
}

// rangeReader records every read.
type rangeReader struct {
	r    io.ReaderAt
	mu   sync.Mutex
	rng  [][2]int64
	fail error
}

func (c *rangeReader) ReadAt(p []byte, off int64) (int, error) {
	c.mu.Lock()
	c.rng = append(c.rng, [2]int64{off, off + int64(len(p))})
	c.mu.Unlock()
	if c.fail != nil && off > 0 {
		return 0, c.fail
	}
	return c.r.ReadAt(p, off)
}

func TestWALScanReadsEachByteOnce(t *testing.T) {
	spec := make([]walSpec, 4000) // more than one 1 MiB chunk of 536-byte slots
	for i := range spec {
		spec[i] = walSpec{uint32(i%50 + 2), 0}
	}
	spec[3999].commit = 60
	data := buildWAL(512, false, 1, 2, spec).Bytes()
	data = append(data, 1, 2, 3, 4, 5) // a torn tail that must not be read
	rr := &rangeReader{r: bytes.NewReader(data)}
	s, err := sqlitefile.ScanWAL(rr, int64(len(data)), 512, sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Info.FramesValid != 4000 || s.Info.LastCommit != 4000 {
		t.Fatalf("%+v", s.Info)
	}
	sort.Slice(rr.rng, func(i, j int) bool { return rr.rng[i][0] < rr.rng[j][0] })
	var total, prevEnd int64
	for i, r := range rr.rng {
		if i > 0 && r[0] < prevEnd {
			t.Fatalf("ranges overlap: %v then %v", rr.rng[i-1], r)
		}
		prevEnd = r[1]
		total += r[1] - r[0]
	}
	if total > int64(len(data)) || len(rr.rng) < 3 {
		t.Errorf("read %d bytes of %d in %d reads", total, len(data), len(rr.rng))
	}
	if total != int64(len(data))-5 {
		t.Errorf("read %d bytes, the complete slots and the header are %d", total, len(data)-5)
	}
	for _, r := range rr.rng[1:] {
		if (r[0]-32)%536 != 0 {
			t.Errorf("read at %d is not aligned to a slot", r[0])
		}
	}
}

func TestWALScanBudgetCharged(t *testing.T) {
	spec := make([]walSpec, 20)
	for i := range spec {
		spec[i] = walSpec{uint32(i + 2), 0}
	}
	few := &capBudget{limit: 1 << 30}
	scanBytes(t, buildWAL(512, false, 1, 2, spec[:4]).Bytes(), 512, sqlitefile.Options{Budget: few})
	many := &capBudget{limit: 1 << 30}
	scanBytes(t, buildWAL(512, false, 1, 2, spec).Bytes(), 512, sqlitefile.Options{Budget: many})
	if few.used <= 0 || many.used <= few.used {
		t.Errorf("the result keeps what it holds charged: 4 frames %d, 20 frames %d", few.used, many.used)
	}
	spec[19].commit = 30
	committed := &capBudget{limit: 1 << 30}
	scanBytes(t, buildWAL(512, false, 1, 2, spec).Bytes(), 512, sqlitefile.Options{Budget: committed})
	if committed.used <= many.used {
		t.Errorf("the page-to-frame map is charged: %d vs %d", committed.used, many.used)
	}
	refuse := &capBudget{limit: 100}
	data := buildWAL(512, false, 1, 2, spec).Bytes()
	_, err := sqlitefile.ScanWAL(bytes.NewReader(data), int64(len(data)), 512, sqlitefile.Options{Budget: refuse})
	if !errors.Is(err, sqlitefile.ErrBudget) {
		t.Errorf("a refusing budget gave %v", err)
	}
	if refuse.used != 0 {
		t.Errorf("a failed scan left %d bytes charged", refuse.used)
	}
}

func TestWALScanIOError(t *testing.T) {
	data := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 4}}).Bytes()
	boom := errors.New("disk on fire")
	_, err := sqlitefile.ScanWAL(&rangeReader{r: bytes.NewReader(data), fail: boom}, int64(len(data)), 512, sqlitefile.Options{})
	if !errors.Is(err, boom) {
		t.Errorf("got %v", err)
	}
}

func TestWALScanNeverPanics(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 11)) //nolint:gosec // deterministic test input
	base := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 4}, {4, 5}}).Bytes()
	for range 300 {
		d := slices.Clone(base)
		for range 1 + r.IntN(4) {
			d[r.IntN(len(d))] ^= byte(1 << r.IntN(8))
		}
		d = d[:r.IntN(len(d)+1)]
		if _, err := sqlitefile.ScanWAL(bytes.NewReader(d), int64(len(d)), []int{0, 512, 4096}[r.IntN(3)], sqlitefile.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	_ = fmt.Sprint
}

func fillRand(r *rand.Rand, p []byte) {
	for i := range p {
		p[i] = byte(r.UintN(256))
	}
}

// notAppliedWarning returns the wal-frames-not-applied warnings of a scan.
func notAppliedWarning(ws []sqlitefile.Warning) []sqlitefile.Warning {
	var out []sqlitefile.Warning
	for _, w := range ws {
		if w.Code == sqlitefile.WarnWALFramesNotApplied {
			out = append(out, w)
		}
	}
	return out
}

// TestWALFramesNotAppliedWarning: frames the engine does not apply may hold
// evidence, so a scan says so once, with a count and the first slot of each
// class (broken, detached, stale, uncommitted).
func TestWALFramesNotAppliedWarning(t *testing.T) {
	t.Run("broken detached uncommitted", func(t *testing.T) {
		// commits at 2 and 6; slot 3 and 4 uncommitted, bit flip in 5.
		w := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 4}, {4, 0}, {5, 0}, {6, 0}, {7, 9}, {8, 9}})
		w.PatchFrame(5, 24+10, w.Bytes()[32+4*(24+512)+24+10]^1)
		s := scanBytes(t, w.Bytes(), 512, sqlitefile.Options{})
		got := notAppliedWarning(s.Warnings)
		if len(got) != 1 {
			t.Fatalf("want exactly one wal-frames-not-applied warning, got %v", s.Warnings)
		}
		m := got[0].Msg
		if got[0].File != sqlitefile.FileWAL {
			t.Errorf("file %v", got[0].File)
		}
		for _, want := range []string{"uncommitted 2 (first slot 3)", "broken 1 (first slot 5)", "detached 2 (first slot 6)", "stale 0"} {
			if !strings.Contains(m, want) {
				t.Errorf("message %q lacks %q", m, want)
			}
		}
	})
	t.Run("stale", func(t *testing.T) {
		s := scanBytes(t, resetScenario().Bytes(), 512, sqlitefile.Options{})
		got := notAppliedWarning(s.Warnings)
		if len(got) != 1 {
			t.Fatalf("got %v", s.Warnings)
		}
		for _, want := range []string{"stale 4 (first slot 3)", "broken 0", "detached 0", "uncommitted 0"} {
			if !strings.Contains(got[0].Msg, want) {
				t.Errorf("message %q lacks %q", got[0].Msg, want)
			}
		}
	})
	t.Run("clean WAL raises none", func(t *testing.T) {
		s := scanBytes(t, buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 4}}).Bytes(), 512, sqlitefile.Options{})
		if got := notAppliedWarning(s.Warnings); len(got) != 0 {
			t.Errorf("clean WAL warned: %v", got)
		}
	})
}
