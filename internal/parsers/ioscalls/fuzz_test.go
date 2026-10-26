package ioscalls_test

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioscalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// maxSeedBytes is the largest file a fuzz seed may hold (T59): a big seed slows every execution, so the
// fuzzer explores nothing.
const maxSeedBytes = 64 << 10

type fuzzSeed struct {
	name             string
	db, wal, journal []byte
}

// minimalCore is a hand-built Core Data database of 512-byte pages with one ZCALLRECORD row.
func minimalCore() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	b.CreateTable("ZCALLRECORD", "create table ZCALLRECORD(Z_PK integer primary key, ZANSWERED integer, ZORIGINATED integer, ZDATE timestamp, ZDURATION float, ZADDRESS varchar)").
		Insert(1, int64(1), int64(1), int64(0), 700000000.5, 30.0, "+15551234567")
	return b.Bytes()
}

// minimalLegacy is a hand-built call_history.db of 512-byte pages with one call row.
func minimalLegacy() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	b.CreateTable("call", "create table call(ROWID integer primary key autoincrement, address text, date integer, duration integer, flags integer)").
		Insert(1, int64(1), "+15551234567", int64(1682935200), int64(30), int64(4))
	return b.Bytes()
}

// fuzzSeeds are the committed fixtures (all far below the limit) and the two minimal databases.
func fuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	seeds := []fuzzSeed{{name: "minimal-core", db: minimalCore()}, {name: "minimal-legacy", db: minimalLegacy()}}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		seeds = append(seeds, fuzzSeed{name: v, db: fx.Files["db"], wal: fx.Files["wal"], journal: fx.Files["journal"]})
	}
	return seeds
}

func TestFuzzSeedsAreSmall(t *testing.T) {
	seeds := fuzzSeeds(t)
	if len(seeds) != len(variants)+2 {
		t.Errorf("%d seeds for %d variants and the two minimal databases", len(seeds), len(variants))
	}
	for _, s := range seeds {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
}

// FuzzParseIOSCalls mutates whole databases (and their WAL and journal) and requires the contract to hold: no
// panic, no refused record, and only typed errors. It runs through the iOS entry point, because the harness
// refuses an iOS parser's record on an Android artifact. Seeds run under go test; go test -fuzz is run manually.
func FuzzParseIOSCalls(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		parsertest.FuzzParseIOS(t, ioscalls.Parser{}, remoteCore, logicalCore, db, wal, journal)
	})
}

// Every seed is a database that parses to records: a seed that gave none (or only refusals) would let the fuzz
// target pass without ever reaching a mapper.
func TestEveryFuzzSeedEmitsRecords(t *testing.T) {
	for _, s := range fuzzSeeds(t) {
		files := map[string][]byte{"db": s.db}
		if len(s.wal) > 0 {
			files["wal"] = s.wal
		}
		h := parsertest.NewHarness(t)
		b := parsertest.Fixture{Name: s.name, Files: files}.BundleIOS(h, remoteCore, logicalCore)
		p := ioscalls.Parser{}
		res := h.Run(p, b.Input(h, p, false))
		if res.Err != nil || len(res.Violations) != 0 || len(res.Records) == 0 {
			t.Errorf("seed %s: err %v, violations %v, %d records", s.name, res.Err, res.Violations, len(res.Records))
		}
	}
}

func pageSize(db []byte) int {
	ps := int(binary.BigEndian.Uint16(db[16:18]))
	if ps == 1 {
		return 65536
	}
	return ps
}

// Page-level hostile mutations, deterministic: zeroed pages, a flipped page type, an absurd cell count, a wild
// first-free block and a wild cell offset, in every page of each committed database, each fuzzed through the
// iOS entry point.
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	muts := map[string]func(p []byte){
		"zeroed":      func(p []byte) { clear(p) },
		"type flip":   func(p []byte) { p[0] ^= 0x08 },
		"cell count":  func(p []byte) { p[3], p[4] = 0xff, 0xff },
		"first free":  func(p []byte) { p[1], p[2] = 0xff, 0xff },
		"cell offset": func(p []byte) { p[8], p[9] = 0xff, 0xf0 },
	}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		db, wal := fx.Files["db"], fx.Files["wal"]
		ps := pageSize(db)
		if len(db) < 2*ps {
			t.Fatalf("%s: %d bytes is not a database with a table to damage", v, len(db))
		}
		remote, logical := remotesFor(v)
		for pg := range len(db) / ps {
			for name, mut := range muts {
				m := append([]byte(nil), db...)
				page := m[pg*ps : (pg+1)*ps]
				if pg == 0 {
					page = page[100:] // keep the file header: it is covered by its own test
				}
				mut(page)
				t.Run(fmt.Sprintf("%s/page %d/%s", v, pg, name), func(t *testing.T) {
					parsertest.FuzzParseIOS(t, ioscalls.Parser{}, remote, logical, m, wal, nil)
				})
			}
		}
	}
}

// The WAL of coredata-wal with its file header and each frame header damaged: the frames are a 24-byte header
// (page number, commit size, salts, checksums) before each page.
func TestWALHeaderMutationsNeverBreakTheContract(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "coredata-wal")
	db, wal := fx.Files["db"], fx.Files["wal"]
	ps := pageSize(db)
	const fileHdr, frameHdr = 32, 24
	if len(wal) < fileHdr+frameHdr+ps {
		t.Fatalf("the WAL holds %d bytes, not a frame", len(wal))
	}
	fileMuts := map[string]func(h []byte){
		"zeroed":   func(h []byte) { clear(h) },
		"magic":    func(h []byte) { h[0] ^= 0x01 },
		"version":  func(h []byte) { h[4], h[5], h[6], h[7] = 0xff, 0xff, 0xff, 0xff },
		"pagesize": func(h []byte) { h[8], h[9], h[10], h[11] = 0xff, 0xff, 0xff, 0xff },
		"salt":     func(h []byte) { h[16] ^= 0xff },
		"checksum": func(h []byte) { h[24] ^= 0xff },
	}
	frameMuts := map[string]func(h []byte){
		"zeroed":      func(h []byte) { clear(h) },
		"page number": func(h []byte) { h[0], h[1], h[2], h[3] = 0xff, 0xff, 0xff, 0xff },
		"page zero":   func(h []byte) { h[0], h[1], h[2], h[3] = 0, 0, 0, 0 },
		"commit size": func(h []byte) { h[4], h[5], h[6], h[7] = 0xff, 0xff, 0xff, 0xff },
		"checksum":    func(h []byte) { h[16] ^= 0xff },
	}
	for name, mut := range fileMuts {
		m := append([]byte(nil), wal...)
		mut(m[:fileHdr])
		t.Run("file header/"+name, func(t *testing.T) {
			parsertest.FuzzParseIOS(t, ioscalls.Parser{}, remoteCore, logicalCore, db, m, nil)
		})
	}
	frames := (len(wal) - fileHdr) / (frameHdr + ps)
	if frames == 0 {
		t.Fatal("no complete WAL frame")
	}
	for f := range frames {
		at := fileHdr + f*(frameHdr+ps)
		for name, mut := range frameMuts {
			m := append([]byte(nil), wal...)
			mut(m[at : at+frameHdr])
			t.Run(fmt.Sprintf("frame %d/%s", f, name), func(t *testing.T) {
				parsertest.FuzzParseIOS(t, ioscalls.Parser{}, remoteCore, logicalCore, db, m, nil)
			})
		}
	}
}
