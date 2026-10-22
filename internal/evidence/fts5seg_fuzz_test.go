package evidence

import (
	"errors"
	"strings"
	"testing"
)

// fuzzFTS5Input splits a fuzz input into the two pages it stands for: the first two bytes are the
// length of the first page (clamped), the rest is the second page. A single seed page has no second page.
func fuzzFTS5Input(data []byte) (first, second []byte) {
	if len(data) < 2 {
		return data, nil
	}
	n := int(data[0])<<8 | int(data[1])
	rest := data[2:]
	if n > len(rest) {
		n = len(rest)
	}
	return rest[:n], rest[n:]
}

// encodeFTS5Pair is the inverse of fuzzFTS5Input.
func encodeFTS5Pair(first, second []byte) []byte {
	out := []byte{byte(len(first) >> 8), byte(len(first))}
	out = append(out, first...)
	return append(out, second...)
}

// FuzzFTS5Leaf feeds one or two arbitrary pages to a fresh segment reader. The decoder must never
// panic, every error must be an ftsLeafError, and a decode that reports success must have taken every
// byte of the pages exactly once and reported positions that ascend per (term, rowid, column).
func FuzzFTS5Leaf(f *testing.F) {
	f.Add(encodeFTS5Pair(goodPage(), nil))
	f.Add(encodeFTS5Pair(goodPage(), (&leafBuilder{}).raw(20, 2, 2).page(4)))
	db := genuineFTS(f)
	fill(f, db, 1, 3, 6)
	probeExec(f, db, "INSERT INTO records_fts(rowid, summary, body) VALUES (100, 's', ?)", strings.Repeat("alpha beta gamma delta ", 900))
	seeds := 0
	for _, table := range FTSTables() {
		for _, seg := range ftsStructureOf(f, db, table) {
			pages := ftsLeafPages(f, db, table, seg.ID)
			for i, p := range pages {
				if len(p.block) > 1<<16 {
					continue
				}
				var next []byte
				if i+1 < len(pages) {
					next = pages[i+1].block
				}
				if seeds < 60 {
					f.Add(encodeFTS5Pair(p.block, next))
					seeds++
				}
			}
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		first, second := fuzzFTS5Input(data)
		r := newFTSSegReader(ftsSegment{ID: 1, First: 1, Last: 2})
		type key struct {
			term string
			row  int64
			col  int
		}
		last := map[key]int64{}
		ordered := true
		sink := ftsSink{Posting: func(term []byte, rowid int64, col int, off int64) {
			k := key{string(term), rowid, col}
			if prev, ok := last[k]; ok && off <= prev {
				ordered = false
			}
			last[k] = off
		}}
		var err error
		taken := 0
		for i, p := range [][]byte{first, second} {
			if i == 1 && p == nil {
				break
			}
			before := r.consumed
			if err = r.Page(int64(i+1), p, sink); err != nil {
				break
			}
			if r.consumed-before != len(p) {
				t.Fatalf("page %d: %d bytes taken of %d", i+1, r.consumed-before, len(p))
			}
			taken++
		}
		if err == nil {
			err = r.Finish()
		}
		if err != nil {
			var le *ftsLeafError
			if !errors.Is(err, errFTSLeaf) || !errors.As(err, &le) {
				t.Fatalf("an error that is not an ftsLeafError: %v", err)
			}
			return
		}
		if !ordered {
			t.Fatalf("positions do not ascend after a successful decode of %d pages", taken)
		}
	})
}
