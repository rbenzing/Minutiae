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

// Page-level hostile mutations, deterministic: zeroed pages, a flipped page type, an
// absurd cell count, a wild first-free block and cell offset, in every page of the
// call log fixtures.
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	muts := map[string]func(p []byte){
		"zeroed":      func(p []byte) { clear(p) },
		"type flip":   func(p []byte) { p[0] ^= 0x08 },
		"cell count":  func(p []byte) { p[3], p[4] = 0xff, 0xff },
		"first free":  func(p []byte) { p[1], p[2] = 0xff, 0xff },
		"cell offset": func(p []byte) { p[8], p[9] = 0xff, 0xf0 },
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
				page := m[pg*ps : (pg+1)*ps]
				if pg == 0 {
					page = page[100:] // keep the file header: it is covered by its own test
				}
				mut(page)
				t.Run(fmt.Sprintf("%s/page %d/%s", v, pg, name), func(t *testing.T) {
					parsertest.FuzzParse(t, androidcalllog.Parser{}, remoteFor(v), m, nil, nil)
				})
			}
		}
	}
}
