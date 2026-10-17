package androidmms_test

import (
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// maxSeedBytes is the largest file a fuzz seed may hold. A seed of hundreds of kilobytes
// slows the target to well under one execution a second, so the fuzzer explores nothing
// (T59); the big fixtures stay as ordinary tests.
const maxSeedBytes = 64 << 10

type fuzzSeed struct {
	name             string
	db, wal, journal []byte
}

// minimalSeed is a hand-built database of 512-byte pages with one sms row and one pdu row.
func minimalSeed() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	b.CreateTable("sms", "create table sms(_id integer primary key, address text, date integer, type integer, body text)").
		Insert(1, int64(1), "+15551234567", int64(1682935200000), int64(1), "hi")
	b.CreateTable("pdu", "create table pdu(_id integer primary key, date integer, msg_box integer, m_type integer)").
		Insert(1, int64(1), int64(1682935200), int64(1), int64(132))
	return b.Bytes()
}

// fuzzSeeds are the small committed fixtures (every file at most maxSeedBytes, which
// includes the uncheckpointed-WAL fixture) and the minimal hand-built database.
func fuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	seeds := []fuzzSeed{{name: "minimal", db: minimalSeed()}}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		small := true
		for _, data := range fx.Files {
			small = small && len(data) <= maxSeedBytes
		}
		if small {
			seeds = append(seeds, fuzzSeed{name: v, db: fx.Files["db"], wal: fx.Files["wal"], journal: fx.Files["journal"]})
		}
	}
	return seeds
}

func TestFuzzSeedsAreSmallAndCoverTheWAL(t *testing.T) {
	seeds := fuzzSeeds(t)
	names := map[string]bool{}
	for _, s := range seeds {
		names[s.name] = true
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
	if !names["minimal"] || !names["wal-uncheckpointed"] {
		t.Errorf("seeds %v lack the minimal database or the WAL fixture", names)
	}
	if len(seeds) < 4 {
		t.Errorf("only %d seeds", len(seeds))
	}
}

// FuzzParseMMSSMS mutates whole databases (and their WAL and journal) and requires the
// contract to hold: no panic, no refused record, and only typed errors. Seeds run under
// go test; go test -fuzz is run manually.
func FuzzParseMMSSMS(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		parsertest.FuzzParse(t, androidmms.Parser{}, remotePath, db, wal, journal)
	})
}

// Page-level hostile mutations, deterministic: zeroed pages, a flipped page type, an
// absurd cell count, a wild first-free block and cell offset, in every page of the small
// UTF-16 fixture and of the uncheckpointed-WAL fixture's database, and damage to the
// header, to every frame header and to every frame page of that WAL.
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	muts := map[string]func(p []byte){
		"zeroed":      func(p []byte) { clear(p) },
		"type flip":   func(p []byte) { p[0] ^= 0x08 },
		"cell count":  func(p []byte) { p[3], p[4] = 0xff, 0xff },
		"first free":  func(p []byte) { p[1], p[2] = 0xff, 0xff },
		"cell offset": func(p []byte) { p[8], p[9] = 0xff, 0xf0 },
	}
	walHeader := map[string]func(w []byte){
		"magic":     func(w []byte) { w[0] ^= 0xff },
		"page size": func(w []byte) { w[8], w[9], w[10], w[11] = 0xff, 0xff, 0xff, 0xff },
		"salt":      func(w []byte) { w[16] ^= 0xff },
		"checksum":  func(w []byte) { w[24] ^= 0xff },
		"zeroed":    func(w []byte) { clear(w) },
	}
	frameHeader := map[string]func(h []byte){
		"page number": func(h []byte) { h[0], h[1], h[2], h[3] = 0xff, 0xff, 0xff, 0xff },
		"commit size": func(h []byte) { h[4], h[5], h[6], h[7] = 0xff, 0xff, 0xff, 0xff },
		"salt":        func(h []byte) { h[8] ^= 0xff },
		"checksum":    func(h []byte) { h[16] ^= 0xff },
		"zeroed":      func(h []byte) { clear(h) },
	}
	for _, variant := range []string{"utf16", "wal-uncheckpointed"} {
		fx := parsertest.LoadFixture(t, "testdata", variant)
		db, wal := fx.Files["db"], fx.Files["wal"]
		ps := int(db[16])<<8 | int(db[17])
		if ps == 1 {
			ps = 65536
		}
		run := func(name string, d, w []byte) {
			t.Run(variant+"/"+name, func(t *testing.T) { parsertest.FuzzParse(t, androidmms.Parser{}, remotePath, d, w, nil) })
		}
		for pg := 0; pg < len(db)/ps; pg++ {
			for name, mut := range muts {
				m := append([]byte(nil), db...)
				page := m[pg*ps : (pg+1)*ps]
				if pg == 0 {
					page = page[100:] // keep the file header: it is covered by its own test
				}
				mut(page)
				run(fmt.Sprintf("page %d/%s", pg, name), m, wal)
			}
		}
		if variant == "wal-uncheckpointed" && len(wal) < 32+24+ps {
			t.Fatalf("the WAL fixture has %d bytes: no frame to damage", len(wal))
		}
		if len(wal) == 0 {
			continue
		}
		for name, bad := range walHeader {
			m := append([]byte(nil), wal...)
			bad(m[:32])
			run("wal header/"+name, db, m)
		}
		frame := 24 + ps // the WAL is a 32-byte header, then frames of a 24-byte header and a page
		for off := 32; off+frame <= len(wal); off += frame {
			for name, bad := range frameHeader {
				m := append([]byte(nil), wal...)
				bad(m[off : off+24])
				run(fmt.Sprintf("frame@%d header/%s", off, name), db, m)
			}
			for name, mut := range muts {
				m := append([]byte(nil), wal...)
				mut(m[off+24 : off+frame])
				run(fmt.Sprintf("frame@%d page/%s", off, name), db, m)
			}
		}
	}
}
