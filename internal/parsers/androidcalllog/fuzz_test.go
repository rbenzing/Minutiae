package androidcalllog_test

import (
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidcalllog"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// maxSeedBytes is the largest file a fuzz seed may hold (T59): a big seed slows every
// execution, so the fuzzer explores nothing.
const maxSeedBytes = 64 << 10

type fuzzSeed struct {
	name             string
	db, wal, journal []byte
}

// minimalSeed is a hand-built database of 512-byte pages with one calls row.
func minimalSeed() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	b.CreateTable("calls", "create table calls(_id integer primary key, number text, date integer, duration integer, type integer)").
		Insert(1, int64(1), "+15551234567", int64(1682935200000), int64(30), int64(1))
	return b.Bytes()
}

// fuzzSeeds are the committed fixtures (all far below the limit) and the minimal database.
func fuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	seeds := []fuzzSeed{{name: "minimal", db: minimalSeed()}}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		seeds = append(seeds, fuzzSeed{name: v, db: fx.Files["db"], wal: fx.Files["wal"], journal: fx.Files["journal"]})
	}
	return seeds
}

func TestFuzzSeedsAreSmall(t *testing.T) {
	seeds := fuzzSeeds(t)
	if len(seeds) != len(variants)+1 {
		t.Errorf("%d seeds for %d variants and the minimal database", len(seeds), len(variants))
	}
	for _, s := range seeds {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
}

// FuzzParseCallLog mutates whole databases (and their WAL and journal) and requires the
// contract to hold: no panic, no refused record, and only typed errors. Seeds run under
// go test; go test -fuzz is run manually.
func FuzzParseCallLog(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		parsertest.FuzzParse(t, androidcalllog.Parser{}, remoteCalllog, db, wal, journal)
	})
}

// sameSizeClass maps a one-byte serial type to a different storage class of the same body
// size (T67): an integer becomes text or a float, text becomes a blob and back, so the record
// stays well formed and the wrong-class cell reaches the mapper.
func sameSizeClass(st byte) (byte, bool) {
	switch {
	case st >= 1 && st <= 4:
		return 15 + 2*(st-1), true // int of 1..4 bytes -> text of 1..4 bytes
	case st == 6:
		return 7, true // int64 -> float64
	case st == 7:
		return 6, true // float64 -> int64
	case st >= 12 && st < 0x80 && st%2 == 1:
		return st - 1, true // text -> blob
	case st >= 12 && st < 0x80:
		return st + 1, true // blob -> text
	}
	return 0, false
}

// rewriteSerialTypes gives every cell of a table leaf page (type 0x0d) the same-size storage
// class of another kind in each one-byte serial type of its record header. base is the offset
// the caller stripped from the page start (100 on page 1), so the cell pointers, which count
// from the page start, are shifted by it.
func rewriteSerialTypes(p []byte, base int) {
	if len(p) < 8 || p[0] != 0x0d {
		return
	}
	n := int(p[3])<<8 | int(p[4])
	varint := func(at int) (v, size int, ok bool) {
		for ; size < 9 && at+size < len(p); size++ {
			b := p[at+size]
			v = v<<7 | int(b&0x7f)
			if b < 0x80 {
				return v, size + 1, true
			}
		}
		return 0, 0, false
	}
	for i := 0; i < n && 8+2*i+2 <= len(p); i++ {
		at := (int(p[8+2*i])<<8 | int(p[9+2*i])) - base
		if at < 0 || at >= len(p) {
			continue
		}
		_, a, ok := varint(at) // payload length
		if !ok {
			continue
		}
		_, b, ok := varint(at + a) // rowid
		if !ok {
			continue
		}
		hs, c, ok := varint(at + a + b) // header size, itself included
		if !ok {
			continue
		}
		start := at + a + b + c
		for j := start; j < at+a+b+hs && j < len(p); j++ {
			if p[j] >= 0x80 {
				break // a multi-byte serial type: leave the rest of the header alone
			}
			if ns, ok := sameSizeClass(p[j]); ok {
				p[j] = ns
			}
		}
	}
}

// Page-level hostile mutations, deterministic: zeroed pages, a flipped page type, an
// absurd cell count, a wild first-free block and cell offset, and every cell's serial types
// rewritten to another storage class (wrong-class cells), in every page of the call log
// fixtures.
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	muts := map[string]func(p []byte, base int){
		"zeroed":      func(p []byte, _ int) { clear(p) },
		"type flip":   func(p []byte, _ int) { p[0] ^= 0x08 },
		"cell count":  func(p []byte, _ int) { p[3], p[4] = 0xff, 0xff },
		"first free":  func(p []byte, _ int) { p[1], p[2] = 0xff, 0xff },
		"cell offset": func(p []byte, _ int) { p[8], p[9] = 0xff, 0xf0 },
		"serial type": rewriteSerialTypes,
	}
	for _, v := range []string{"in-contacts2", "features-subid"} {
		db := parsertest.LoadFixture(t, "testdata", v).Files["db"]
		ps := int(db[16])<<8 | int(db[17])
		if ps == 1 {
			ps = 65536
		}
		if len(db) < 2*ps {
			t.Fatalf("%s: %d bytes is not a database with a table to damage", v, len(db))
		}
		for pg := 0; pg < len(db)/ps; pg++ {
			for name, mut := range muts {
				m := append([]byte(nil), db...)
				page, base := m[pg*ps:(pg+1)*ps], 0
				if pg == 0 {
					page, base = page[100:], 100 // keep the file header: it is covered by its own test
				}
				mut(page, base)
				t.Run(fmt.Sprintf("%s/page %d/%s", v, pg, name), func(t *testing.T) {
					parsertest.FuzzParse(t, androidcalllog.Parser{}, remoteFor(v), m, nil, nil)
				})
			}
		}
	}
}
