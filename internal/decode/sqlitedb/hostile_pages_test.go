package sqlitedb_test

// The damaged-page matrix and the companion-file cases of the hostile suite
// (see hostile_test.go).

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// probe is one lookup a clean file answers: the row rowid of table holds key in
// column col.
type probe struct {
	table, col string
	rowid      int64
	key        parse.JoinKey
}

const probesPerColumn = 24

// cleanProbes lists, for every rowid table, the first, the middle and the last
// non-virtual column of the clean file (one index build each per image), and
// of each the key of rows spread evenly over the table, the first and the last
// included (at most probesPerColumn per column). Rows are lost whole, so a
// few columns see the same loss as all of them; the cost of the matrix is bounded.
func cleanProbes(t *testing.T, clean []byte) []probe {
	t.Helper()
	d := openBytes(t, clean, nil, nil, bigBudget())
	names, err := d.Tables(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []probe
	for _, n := range names {
		tb, err := d.Table(t.Context(), n, nil, nil)
		if err != nil || tb.WithoutRowid() {
			continue
		}
		var stored []int
		for ci, c := range tb.Cols() {
			if !c.Virtual {
				stored = append(stored, ci)
			}
		}
		var chosen []int
		for _, i := range []int{0, len(stored) / 2, len(stored) - 1} {
			if i >= 0 && i < len(stored) && !slices.Contains(chosen, stored[i]) {
				chosen = append(chosen, stored[i])
			}
		}
		for _, ci := range chosen {
			c := tb.Cols()[ci]
			var col []probe
			err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
				k, ok, kerr := sqlitedb.KeyOf(r, ci)
				id, hasID := r.Rowid()
				if kerr == nil && ok && hasID {
					col = append(col, probe{table: n, col: c.Name, rowid: id, key: k})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(col) <= probesPerColumn {
				out = append(out, col...)
				continue
			}
			for i := range probesPerColumn {
				out = append(out, col[i*(len(col)-1)/(probesPerColumn-1)])
			}
		}
	}
	return out
}

// matchProbes runs every probe through Context.Match on the damaged image:
// the row is matched, or the answer is an error (ErrKeyUndecidable when the
// index could not decide), never a clean empty (B76). A clean empty is accepted
// only when the damaged file's own scan delivers that rowid with a different
// value in the column (the damage changed the value, so the key honestly no
// longer matches). It returns how many probes were answered ErrKeyUndecidable.
func matchProbes(t *testing.T, label string, img []byte, probes []probe) (undecided int) {
	t.Helper()
	b := &testBudget{limit: 128 << 20}
	d, err := sqlitedb.Open(t.Context(), filesOf(img, nil, nil), b)
	if err != nil {
		return 0
	}
	defer d.Release()
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
	defer c.Close()
	for _, p := range probes {
		rows, err := c.Match(p.table, p.col, p.key)
		switch {
		case err == nil && len(rows) > 0:
		case err == nil:
			if !valueChanged(d, p) {
				t.Errorf("%s: Match(%s.%s, row %d) answered a clean empty for a row the clean file holds", label, p.table, p.col, p.rowid)
			}
		case errors.Is(err, sqlitedb.ErrKeyUndecidable):
			undecided++
		default:
			requireTyped(t, fmt.Sprintf("%s: Match(%s.%s, row %d)", label, p.table, p.col, p.rowid), err)
		}
	}
	return undecided
}

// valueChanged reports whether the damaged file's scan delivers p.rowid with a
// key in p.col different from p's.
func valueChanged(d *sqlitedb.DB, p probe) bool {
	tb, err := d.Table(context.Background(), p.table, nil, nil)
	if err != nil {
		return false
	}
	ci := tb.Col(p.col)
	changed := false
	_ = tb.Scan(context.Background(), func(r sqlitedb.Row) error {
		if id, _ := r.Rowid(); id != p.rowid {
			return nil
		}
		if k, ok, kerr := sqlitedb.KeyOf(r, ci); kerr != nil || !ok || k != p.key {
			changed = true
		}
		return sqlitedb.ErrStop
	})
	return changed
}

// pageGeom reads the page size and usable size from the database header.
func pageGeom(db []byte) (pageSize, usable int) {
	pageSize = int(binary.BigEndian.Uint16(db[16:]))
	if pageSize == 1 {
		pageSize = 65536
	}
	return pageSize, pageSize - int(db[20])
}

func readVarint(b []byte) (v uint64, n int) {
	for i := 0; i < 9 && i < len(b); i++ {
		if i == 8 {
			return v<<8 | uint64(b[i]), 9
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return v, len(b)
}

// pageMutation is one deterministic damage to one page (1-based number p).
type pageMutation struct {
	name  string
	apply func(db []byte, p, pageSize, usable int) bool // false: not applicable to this page
}

func pageMutations() []pageMutation {
	hdrOf := func(p int) int {
		if p == 1 {
			return 100
		}
		return 0
	}
	page := func(db []byte, p, ps int) []byte { return db[(p-1)*ps : p*ps] }
	setType := func(v byte) func([]byte, int, int, int) bool {
		return func(db []byte, p, ps, _ int) bool {
			pg := page(db, p, ps)
			if pg[hdrOf(p)] == v {
				return false
			}
			pg[hdrOf(p)] = v
			return true
		}
	}
	cells := func(pg []byte, h int) (n, ptrs int, interior, ok bool) {
		switch pg[h] {
		case 2, 5:
			interior = true
		case 10, 13:
		default:
			return 0, 0, false, false
		}
		ptrs = h + 8
		if interior {
			ptrs += 4
		}
		return int(binary.BigEndian.Uint16(pg[h+3:])), ptrs, interior, true
	}
	return []pageMutation{
		{"type-0x00", setType(0x00)},
		{"type-0xff", setType(0xff)},
		{"type-interior-index", setType(2)},
		{"type-interior-table", setType(5)},
		{"type-leaf-index", setType(10)},
		{"type-leaf-table", setType(13)},
		{"zeroed-pointer-array", func(db []byte, p, ps, _ int) bool {
			pg := page(db, p, ps)
			n, ptrs, _, ok := cells(pg, hdrOf(p))
			if !ok || n == 0 || ptrs+2*n > ps {
				return false
			}
			clear(pg[ptrs : ptrs+2*n])
			return true
		}},
		{"pointer-0xffff", func(db []byte, p, ps, _ int) bool {
			pg := page(db, p, ps)
			n, ptrs, _, ok := cells(pg, hdrOf(p))
			if !ok || n == 0 || ptrs+2 > ps {
				return false
			}
			pg[ptrs], pg[ptrs+1] = 0xff, 0xff
			return true
		}},
		{"interior-child-is-itself", func(db []byte, p, ps, _ int) bool {
			pg := page(db, p, ps)
			h := hdrOf(p)
			n, ptrs, interior, ok := cells(pg, h)
			if !ok || !interior || n == 0 {
				return false
			}
			binary.BigEndian.PutUint32(pg[h+8:], uint32(p)) // the rightmost child
			if off := int(binary.BigEndian.Uint16(pg[ptrs:])); off+4 <= ps {
				binary.BigEndian.PutUint32(pg[off:], uint32(p)) // the first cell's child
			}
			return true
		}},
		{"overflow-pointer-to-page-0", func(db []byte, p, ps, usable int) bool {
			pg := page(db, p, ps)
			n, ptrs, interior, ok := cells(pg, hdrOf(p))
			if !ok || interior || pg[hdrOf(p)] != 13 {
				return false
			}
			x := usable - 35
			m := (usable-12)*32/255 - 23
			changed := false
			for i := range n {
				off := int(binary.BigEndian.Uint16(pg[ptrs+2*i:]))
				if off >= ps {
					continue
				}
				plen, a := readVarint(pg[off:])
				_, b := readVarint(pg[off+a:])
				if plen <= uint64(x) {
					continue
				}
				local := m + int((plen-uint64(m))%uint64(usable-4))
				if local > x {
					local = m
				}
				if at := off + a + b + local; at+4 <= ps {
					binary.BigEndian.PutUint32(pg[at:], 0)
					changed = true
				}
			}
			return changed
		}},
		{"first-four-bytes-point-to-itself", func(db []byte, p, ps, _ int) bool {
			binary.BigEndian.PutUint32(page(db, p, ps), uint32(p)) // an overflow page whose next is itself
			return true
		}},
	}
}

func TestSQLiteDBHostileDamagedPages(t *testing.T) {
	const limit = 128 << 20
	for _, fx := range []string{"addcolumn-short", "overflow-wide"} {
		t.Run(fx, func(t *testing.T) {
			clean, _, _ := loadFixture(t, fx)
			ps, usable := pageGeom(clean)
			npages := len(clean) / ps
			base := exercise(t, filesOf(clean, nil, nil), limit)
			if base.openErr != nil || base.allWarn != 0 {
				t.Fatalf("the clean fixture is not clean: open %v, %d warnings", base.openErr, base.allWarn)
			}
			rowsTotal := 0
			for _, n := range base.rows {
				rowsTotal += n
			}
			if rowsTotal == 0 {
				t.Fatal("the clean fixture scans no rows")
			}
			muts := pageMutations()
			probes := cleanProbes(t, clean)
			if len(probes) == 0 {
				t.Fatal("the clean fixture offers no Match probe")
			}
			applied, lossSeen, matchUndecided := 0, 0, 0
			for p := 1; p <= npages; p++ {
				for _, m := range muts {
					img := slices.Clone(clean)
					if !m.apply(img, p, ps, usable) {
						continue
					}
					applied++
					label := fmt.Sprintf("page %d %s", p, m.name)
					o := exercise(t, filesOf(img, nil, nil), limit)
					if o.openErr != nil {
						continue // a typed refusal is loud
					}
					matchUndecided += matchProbes(t, label, img, probes)
					for n, want := range base.rows {
						if _, listed := o.rows[n]; !listed {
							if o.allWarn == 0 && o.tableErr[n] == nil {
								t.Errorf("%s: table %q vanished with no error and no warning", label, n)
							}
							if o.tableErr[n] != nil && o.allWarn == 0 {
								t.Errorf("%s: table %q refused (%v) with no warning", label, n, o.tableErr[n])
							}
							continue
						}
						if o.scanErr[n] == nil && o.rows[n] < want {
							lossSeen++
							if o.scanWarn == 0 {
								t.Errorf("%s: table %q scanned %d of %d rows with no error and no warning (silent loss)", label, n, o.rows[n], want)
							}
						}
						for _, id := range base.found[n] {
							if slices.Contains(o.getMiss[n], id) && o.warnings == 0 {
								t.Errorf("%s: Get(%s, %d) answered a clean miss for a row the clean file holds, with no warning", label, n, id)
							}
						}
					}
				}
			}
			t.Logf("%d page x mutation images, %d with fewer rows than the clean scan", applied, lossSeen)
			if floor := map[string]int{"addcolumn-short": 10, "overflow-wide": 200}[fx]; applied < floor {
				t.Errorf("only %d mutations applied (want at least %d): the matrix is vacuous", applied, floor)
			}
			if lossSeen == 0 {
				t.Error("no mutation lost a row: the matrix never exercised the loss rule")
			}
			if matchUndecided == 0 {
				t.Error("no Match was answered ErrKeyUndecidable: the matrix never exercised the lossy index")
			}
		})
	}
}
