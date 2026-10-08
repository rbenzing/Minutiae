package sqlitedb_test

// The damaged-page matrix and the companion-file cases of the hostile suite
// (see hostile_test.go).

import (
	"encoding/binary"
	"fmt"
	"slices"
	"testing"
)

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
			applied, lossSeen := 0, 0
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
							if o.warnings == 0 {
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
		})
	}
}
