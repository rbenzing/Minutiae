package sqlitefile_test

// Exact expectations of the ordered scan, derived from the shape of the tree (LX-4 U67, U70, U71, U72).

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// pageShape is what the page structure alone says about one page of an index tree: its entry
// count, its child pages (left to right) and whether its entries have a lower and an upper bound.
// A page has a lower bound unless every step down to it was through a first child, an upper bound
// unless every step was through the right-most child. The root has neither.
type pageShape struct {
	entries  int
	children []uint32
	lo, hi   bool
}

func treeShape(data []byte, ps int, root uint32) map[uint32]*pageShape {
	out := map[uint32]*pageShape{}
	var walk func(pg uint32, lo, hi bool)
	walk = func(pg uint32, lo, hi bool) {
		p := pageAt(data, ps, pg)
		offs := cellOffsets(p)
		s := &pageShape{entries: len(offs), lo: lo, hi: hi}
		out[pg] = s
		if p[0] != 0x02 {
			return
		}
		for i, off := range offs {
			c := binary.BigEndian.Uint32(p[off:])
			s.children = append(s.children, c)
			walk(c, lo || i > 0, true)
		}
		right := binary.BigEndian.Uint32(p[8:12])
		s.children = append(s.children, right)
		walk(right, lo || len(offs) > 0, hi)
	}
	walk(root, false, false)
	return out
}

// expectedOrderCompares is the number of key comparisons the ordered scan makes: one with the
// previous entry for every entry but the first, one with the lower bound for every entry that has
// one and one with the upper bound for every entry that has one.
func expectedOrderCompares(shape map[uint32]*pageShape) (compares, entries int) {
	for _, s := range shape {
		entries += s.entries
		if s.lo {
			compares += s.entries
		}
		if s.hi {
			compares += s.entries
		}
	}
	return compares + entries - 1, entries
}

func TestOrderCheckComparisonCountIsExact(t *testing.T) {
	const n = 300
	b, _ := woTable(n)
	data := b.Bytes()
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	tbl, err := v.Table(context.Background(), "w")
	if err != nil {
		t.Fatal(err)
	}
	want, entries := expectedOrderCompares(treeShape(data, osPS, tbl.RootPage()))
	if entries != n {
		t.Fatalf("the shape holds %d entries, want %d", entries, n)
	}
	before := v.OrderCompares()
	if got := scanTableRows(t, v, "w"); len(got.rows) != n {
		t.Fatalf("%d rows", len(got.rows))
	}
	if c := int(v.OrderCompares() - before); c != want {
		t.Errorf("%d comparisons, want %d = (n-1) + entries with a lower bound + entries with an upper bound", c, want)
	}
}

// exprOracle counts the entries the scan cannot decide on an expression column, from the shape
// alone: an entry is counted iff its own first value, its predecessor's, or that of a bound of its
// page is text. The bounds of a page are the entries just before and just after its subtree in key
// order.
func exprOracle(t *testing.T, data []byte, rows []sqlitefile.Row) int {
	t.Helper()
	ps := int(binary.BigEndian.Uint16(data[16:18]))
	pages := map[uint32]bool{}
	for _, r := range rows {
		pages[r.Loc.Page] = true
	}
	isChild := map[uint32]bool{}
	for pg := range pages {
		p := pageAt(data, ps, pg)
		if p[0] != 0x02 {
			continue
		}
		for _, off := range cellOffsets(p) {
			isChild[binary.BigEndian.Uint32(p[off:])] = true
		}
		isChild[binary.BigEndian.Uint32(p[8:12])] = true
	}
	var root uint32
	for pg := range pages {
		if !isChild[pg] {
			root = pg
		}
	}
	if root == 0 {
		t.Fatal("no root page")
	}
	shape := treeShape(data, ps, root)
	lowest, highest := map[uint32]int{}, map[uint32]int{}
	for i, r := range rows {
		if _, ok := lowest[r.Loc.Page]; !ok {
			lowest[r.Loc.Page] = i
		}
		highest[r.Loc.Page] = i
	}
	var span func(pg uint32) (int, int)
	span = func(pg uint32) (int, int) {
		lo, hi := len(rows), -1
		if _, ok := lowest[pg]; ok {
			lo, hi = lowest[pg], highest[pg]
		}
		for _, c := range shape[pg].children {
			cl, ch := span(c)
			lo, hi = min(lo, cl), max(hi, ch)
		}
		return lo, hi
	}
	text := func(i int) bool {
		return i >= 0 && i < len(rows) && rows[i].Values[0].Kind == sqlitefile.KindText
	}
	n := 0
	for i, r := range rows {
		counted := text(i) || text(i-1)
		if r.Loc.Page != root {
			lo, hi := span(r.Loc.Page)
			counted = counted || text(lo-1) || text(hi+1)
		}
		if counted {
			n++
		}
	}
	return n
}

// The lower-bound tie clause: the entry before e2 cannot be compared (its expression column holds
// text), but the lower bound of e2's page is an entry with the same key, which is decidable and, in
// a unique key, a repeated key. Only the bound can flag e2.
func TestOrderScanLowerBoundTieFlagsWhenThePredecessorIsUndecided(t *testing.T) {
	b, tb := woKeyPairTable()
	data := b.Bytes()
	root := tb.Root()
	rootPage := pageAt(data, osPS, root)
	l1 := binary.BigEndian.Uint32(rootPage[cellOffsets(rootPage)[1]:]) // the leaf between separators 0 and 1
	sepB := rootPage[keyByteOffset(rootPage, 0)+1]                     // the second key column of separator 0
	p := pageAt(data, osPS, l1)
	e1, e2 := keyByteOffset(p, 0), keyByteOffset(p, 1)
	p[e1+1] = 'x'      // e1: the second key column becomes a one-byte text ...
	p[e1-2] = 13 + 2*1 // ... serial type 15
	p[e2+1] = sepB     // e2 repeats the key of the separator
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	order := sqlitefile.KeyOrder{Cols: []sqlitefile.KeyColumn{{}, {Expr: true}}, Unique: true}
	var rows []sqlitefile.Row
	if _, err := v.ScanTreeOrdered(context.Background(), root, order, func(r sqlitefile.Row) bool {
		rows = append(rows, r.Clone())
		return true
	}); err != nil {
		t.Fatal(err)
	}
	var flagged []int
	e2At := -1
	for i, r := range rows {
		if r.KeyRangeViolation {
			flagged = append(flagged, i)
		}
		if r.Loc.Page == l1 && r.Loc.Cell == 1 {
			e2At = i
		}
	}
	if e2At < 0 || len(flagged) != 1 || flagged[0] != e2At {
		t.Fatalf("flagged %v, want exactly the entry at %d (a repeat of its lower bound)", flagged, e2At)
	}
	if n, found := uncheckedCount(t, v); !found || n == 0 {
		t.Errorf("the undecided predecessor comparison is not counted: %d %v", n, found)
	}
}

// Every exit of the ordered scan gives the bounds it holds back to the budget: a context cancelled
// in the middle of the tree, a refused charge, and a depth limit that skips the subtrees under open
// frames.
func TestOrderScanReleasesBoundsOnEveryExit(t *testing.T) {
	b, _ := keyTable(t, 2000)
	data := b.Bytes()
	scan := func(ctx context.Context, opts sqlitefile.Options, visit func(sqlitefile.Row) bool) error {
		_, v := openLive(t, data, opts)
		defer v.Release()
		tbl, err := v.Table(ctx, "wt")
		if err != nil {
			return err
		}
		_, err = tbl.RowsLoss(ctx, visit)
		return err
	}
	t.Run("cancel", func(t *testing.T) {
		rb := newRecBudget(256 << 20)
		ctx, cancel := context.WithCancel(context.Background())
		n := 0
		err := scan(ctx, sqlitefile.Options{Budget: rb}, func(sqlitefile.Row) bool {
			if n++; n == 900 {
				cancel()
			}
			return true
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("scan error %v, want context.Canceled", err)
		}
		rb.check(t)
	})
	t.Run("error", func(t *testing.T) {
		probe := newRecBudget(256 << 20)
		if err := scan(context.Background(), sqlitefile.Options{Budget: probe}, func(sqlitefile.Row) bool { return true }); err != nil {
			t.Fatal(err)
		}
		rb := newRecBudget(probe.peak / 2) // too small for the whole scan
		err := scan(context.Background(), sqlitefile.Options{Budget: rb}, func(sqlitefile.Row) bool { return true })
		if !errors.Is(err, sqlitefile.ErrBudget) || rb.refused == 0 {
			t.Fatalf("scan error %v (refused %d), want ErrBudget", err, rb.refused)
		}
		rb.check(t)
	})
	t.Run("early stop under open frames", func(t *testing.T) {
		rb := newRecBudget(256 << 20)
		n := 0
		if err := scan(context.Background(), sqlitefile.Options{Budget: rb}, func(sqlitefile.Row) bool { n++; return n < 900 }); err != nil {
			t.Fatal(err)
		}
		if n != 900 {
			t.Fatalf("stopped after %d rows, want 900", n)
		}
		rb.check(t)
	})
	t.Run("limit", func(t *testing.T) {
		rb := newRecBudget(256 << 20)
		n := 0
		err := scan(context.Background(), sqlitefile.Options{Budget: rb, Limits: sqlitefile.Limits{MaxBTreeDepth: 2}}, func(sqlitefile.Row) bool { n++; return true })
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 || n >= 2000 {
			t.Fatalf("%d rows under a depth limit of 2 on a 3-level tree, want some but not all", n)
		}
		rb.check(t)
	})
}
