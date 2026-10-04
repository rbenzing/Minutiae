package sqlitefile_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// ceilDiv is the test's own ceiling division.
func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// TestOverflowCapDerivation: ONE overflow-chain cap serves the scan and the
// point lookup, derived from the payload cap (the largest thing a chain can be
// needed for: a value cap is below the row cap, which is below the payload
// cap) divided by the content of one overflow page. A legal 64 MiB blob on
// 512-byte pages needs 132104 pages and must never be cut.
func TestOverflowCapDerivation(t *testing.T) {
	lim := sqlitefile.DefaultLimits()
	for _, usable := range []int{480, 500, 512, 1000, 1024, 4096, 32768, 65536} {
		got := sqlitefile.OverflowPageCap(lim, usable)
		chunk := int64(usable - 4)
		if want := ceilDiv(lim.MaxPayloadBytes, chunk); got != want {
			t.Errorf("usable %d: cap %d, want ceil(MaxPayloadBytes/(usable-4)) = %d", usable, got, want)
		}
		if need := ceilDiv(lim.MaxBlobBytes, chunk); got < need {
			t.Errorf("usable %d: cap %d cuts a legal blob of MaxBlobBytes (needs %d pages)", usable, got, need)
		}
		if need := ceilDiv(lim.MaxRowBytes, chunk); got < need {
			t.Errorf("usable %d: cap %d cuts a legal row of MaxRowBytes (needs %d pages)", usable, got, need)
		}
	}
	if got := sqlitefile.OverflowPageCap(lim, 512); got < 132104 {
		t.Errorf("a 64 MiB blob on 512-byte pages needs 132104 overflow pages; cap %d", got)
	}
	// Even at the hard ceilings of every limit the cap stays bounded, so the
	// visited set for a chain is bounded too.
	ceil, _ := sqlitefile.ResolveLimitsReport(sqlitefile.Limits{MaxPayloadBytes: 1 << 62})
	if got := sqlitefile.OverflowPageCap(ceil, 480); got <= 0 || got > sqlitefile.MaxMapVisited {
		t.Errorf("cap at the ceilings = %d, want 1..%d", got, sqlitefile.MaxMapVisited)
	}
	// A lowered payload cap lowers the cap.
	small := lim
	small.MaxPayloadBytes = 4096
	if got := sqlitefile.OverflowPageCap(small, 512); got != 9 {
		t.Errorf("cap for 4096 payload bytes on 512-byte pages = %d, want 9", got)
	}
}

// TestOverflowCapEndsAChain: a payload that asks for more chain than the cap
// stops at the cap with the chain marked damaged (short read, never zeros).
func TestOverflowCapEndsAChain(t *testing.T) {
	const local = 40
	data := payloadData(local + 30*(ovPage-4))
	src := sqlitefile.NewFakeSource()
	pages := make([]uint32, 30)
	for i := range pages {
		pages[i] = uint32(10 + i)
	}
	chainOf(src, data[local:], pages...)
	opts := sqlitefile.Options{Limits: sqlitefile.Limits{MaxPayloadBytes: 4096}} // cap: 9 pages of 508 bytes
	env := sqlitefile.NewTestEnv(opts)
	l := env.Ledger()
	vis, err := env.NewMapVisited(l, 64)
	if err != nil {
		t.Fatal(err)
	}
	p := env.NewTestPayload(src, l, vis, ovPage, cellFor(data, local, pages[0]))
	defer p.Release()
	buf := make([]byte, 20*(ovPage-4))
	got, err := p.ReadAt(buf, int64(local))
	if err != nil {
		t.Fatal(err)
	}
	if want := 9 * (ovPage - 4); got != want {
		t.Errorf("read %d bytes, want exactly the 9 pages the cap allows (%d)", got, want)
	}
	if !bytes.Equal(buf[:got], data[local:local+got]) {
		t.Error("the bytes before the cap are not the data")
	}
	if n := len(p.Chain()); n != 9 {
		t.Errorf("%d chain steps followed, want 9", n)
	}
	if why, _, dead := p.Damaged(); !dead || why == "" {
		t.Errorf("a chain cut by the cap must say why: %q %v", why, dead)
	}
}

// sameValues reports the first difference between two rows' values.
func sameValues(a, b []sqlitefile.Value) error {
	if len(a) != len(b) {
		return fmt.Errorf("%d values against %d", len(a), len(b))
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Kind != y.Kind || x.Omitted != y.Omitted || x.Len != y.Len || x.Int != y.Int || x.Float != y.Float ||
			x.Serial != y.Serial || !bytes.Equal(x.Bytes, y.Bytes) || (x.Bytes == nil) != (y.Bytes == nil) {
			return fmt.Errorf("column %d: %s against %s", i, brief(x), brief(y))
		}
	}
	return nil
}

func brief(v sqlitefile.Value) string {
	return fmt.Sprintf("{kind %d omitted %v len %d bytes %d int %d}", v.Kind, v.Omitted, v.Len, len(v.Bytes), v.Int)
}

func warnCodeSet(v *sqlitefile.View) []string {
	seen := map[string]bool{}
	for _, w := range v.Warnings() {
		seen[w.Code] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// getAndScan reads rowid from two fresh views of data, once by point lookup
// and once by scan, and returns both rows and both views.
func getAndScan(t *testing.T, data []byte, root uint32, rowid int64, o sqlitefile.Options) (get, scan sqlitefile.Row, gv, sv *sqlitefile.View) {
	t.Helper()
	_, gv = openLive(t, data, o)
	defer gv.Release()
	g, ok, err := gv.LookupRowid(context.Background(), root, rowid)
	if err != nil || !ok {
		t.Fatalf("Get %d: ok %v err %v", rowid, ok, err)
	}
	_, sv = openLive(t, data, o)
	defer sv.Release()
	for _, r := range scanRows(t, sv, root, sqlitefile.TableTree) {
		if r.Rowid == rowid {
			return g, r, gv, sv
		}
	}
	t.Fatalf("scan did not deliver rowid %d", rowid)
	return
}

// TestLookupAndScanAgreeOnALegalHugeBlob: a 64 MiB blob (the live blob cap) on
// 512-byte pages needs a chain of 132104 pages. Get and scan both read it
// whole, byte for byte, and agree on everything.
func TestLookupAndScanAgreeOnALegalHugeBlob(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 64 MiB database")
	}
	blob := make([]byte, 64<<20)
	for i := range blob {
		blob[i] = byte(i*7 + i>>9)
	}
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a, b)")
	tb.Insert(1, "small", int64(1))
	tb.Insert(2, blob, int64(2))
	tb.Insert(3, "after", int64(3))
	data := b.Bytes()
	if n := len(tb.Overflow(2)); n < 132000 {
		t.Fatalf("chain of %d pages: the fixture is not the huge-blob case", n)
	}
	g, s, gv, sv := getAndScan(t, data, tb.Root(), 2, sqlitefile.Options{})
	for name, r := range map[string]sqlitefile.Row{"Get": g, "scan": s} {
		v := r.Values[0]
		if v.Kind != sqlitefile.KindBlob || v.Omitted || v.Len != 64<<20 || !bytes.Equal(v.Bytes, blob) {
			t.Errorf("%s: the legal 64 MiB blob was not read whole: %s", name, brief(v))
		}
		if r.Values[1].Int != 2 {
			t.Errorf("%s: the column after the blob: %s", name, brief(r.Values[1]))
		}
	}
	if err := sameValues(g.Values, s.Values); err != nil {
		t.Errorf("Get and scan differ: %v", err)
	}
	if g.Loc.OverflowTotal != s.Loc.OverflowTotal || g.Loc.OverflowTotal < 132000 {
		t.Errorf("overflow pages: Get %d, scan %d", g.Loc.OverflowTotal, s.Loc.OverflowTotal)
	}
	if a, b := warnCodeSet(gv), warnCodeSet(sv); !slices.Equal(a, b) || len(a) != 0 {
		t.Errorf("warnings: Get %v, scan %v, want none", a, b)
	}
}

// TestLookupAndScanAgreeOnDamagedChains: every chain defect gives the same row
// (values, omissions, lengths, provenance) and the same warning codes through
// Get and through scan.
func TestLookupAndScanAgreeOnDamagedChains(t *testing.T) {
	big := bytes.Repeat([]byte("0123456789abcdef"), 2000) // 32000 bytes: a 64-page chain on 512-byte pages
	for _, c := range []struct {
		name   string
		mutate func(data []byte, chain []uint32)
		warn   string // a code both must raise ("" none)
	}{
		{"intact", func([]byte, []uint32) {}, ""},
		{"ends early", func(d []byte, ch []uint32) { copy(pageAt(d, 512, ch[3])[:4], []byte{0, 0, 0, 0}) }, sqlitefile.WarnCellOverflowChain},
		{"cycle to an earlier page", func(d []byte, ch []uint32) {
			binary.BigEndian.PutUint32(pageAt(d, 512, ch[4]), ch[1])
		}, sqlitefile.WarnCellOverflowChain},
		{"points outside the file", func(d []byte, ch []uint32) {
			binary.BigEndian.PutUint32(pageAt(d, 512, ch[2]), 99999999)
		}, sqlitefile.WarnCellOverflowChain},
		{"points at itself", func(d []byte, ch []uint32) {
			binary.BigEndian.PutUint32(pageAt(d, 512, ch[2]), ch[2])
		}, sqlitefile.WarnCellOverflowChain},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
			tb := b.CreateTable("t", "create table t(a, b)")
			tb.Insert(1, "one", int64(1))
			tb.Insert(2, big, int64(2))
			data := b.Bytes()
			chain := tb.Overflow(2)
			if len(chain) < 60 {
				t.Fatalf("chain of %d pages", len(chain))
			}
			c.mutate(data, chain)
			g, s, gv, sv := getAndScan(t, data, tb.Root(), 2, sqlitefile.Options{})
			if err := sameValues(g.Values, s.Values); err != nil {
				t.Errorf("Get and scan differ: %v", err)
			}
			if g.Loc.OverflowTotal != s.Loc.OverflowTotal {
				t.Errorf("overflow pages followed: Get %d, scan %d", g.Loc.OverflowTotal, s.Loc.OverflowTotal)
			}
			a, bb := warnCodeSet(gv), warnCodeSet(sv)
			if !slices.Equal(a, bb) {
				t.Errorf("warning codes: Get %v, scan %v", a, bb)
			}
			if c.warn != "" && !slices.Contains(a, c.warn) {
				t.Errorf("no %s warning: %v", c.warn, a)
			}
			if c.warn == "" && len(a) != 0 {
				t.Errorf("an intact chain warned: %v", a)
			}
		})
	}
}
