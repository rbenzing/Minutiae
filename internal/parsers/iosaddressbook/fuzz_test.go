package iosaddressbook_test

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/iosaddressbook"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// maxSeedBytes is the largest file a fuzz seed may hold (T59): a big seed slows every execution, so the
// fuzzer explores nothing.
const maxSeedBytes = 64 << 10

type fuzzSeed struct {
	name                     string
	db, wal, journal, images []byte
}

// minimalBook is a hand-built address book of 512-byte pages: one person with a phone, its label and a store.
func minimalBook() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	b.CreateTable("ABPerson", "create table ABPerson(ROWID integer primary key autoincrement, First text, Last text, Organization text, StoreID integer)").
		Insert(1, int64(1), "Ada", "Lovelace", nil, int64(1))
	b.CreateTable("ABMultiValue", "create table ABMultiValue(UID integer primary key, record_id integer, property integer, label integer, value text)").
		Insert(1, int64(1), int64(1), int64(3), int64(1), "+15551234567")
	b.CreateTable("ABMultiValueLabel", "create table ABMultiValueLabel(value text)").Insert(1, "_$!<Mobile>!$_")
	b.CreateTable("ABStore", "create table ABStore(ROWID integer primary key autoincrement, Name text)").Insert(1, int64(1), "Local")
	return b.Bytes()
}

// minimalImages is a hand-built images database with one thumbnail row.
func minimalImages() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	b.CreateTable("ABThumbnailImage", "create table ABThumbnailImage(record_id integer, data blob)").Insert(1, int64(1), []byte{1})
	b.CreateTable("ABFullSizeImage", "create table ABFullSizeImage(record_id integer, data blob)").Insert(1, int64(1), []byte{1})
	return b.Bytes()
}

// fuzzSeeds are the committed fixtures that fit the seed limit (huge-contact does not: it is covered by its own
// test) and the minimal databases, the second with an images database.
func fuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	seeds := []fuzzSeed{{name: "minimal", db: minimalBook()}, {name: "minimal-with-images", db: minimalBook(), images: minimalImages()}}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Files["db"]) > maxSeedBytes {
			continue
		}
		seeds = append(seeds, fuzzSeed{name: v, db: fx.Files["db"], wal: fx.Files["wal"], journal: fx.Files["journal"], images: fx.Files["images"]})
	}
	return seeds
}

func TestFuzzSeedsAreSmall(t *testing.T) {
	seeds := fuzzSeeds(t)
	if len(seeds) < len(variants) {
		t.Errorf("%d seeds for %d variants (huge-contact excepted) and the two minimal databases", len(seeds), len(variants))
	}
	for _, s := range seeds {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal, "images": s.images} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
}

// fuzzBook runs the parser over one database as a backup, with the images database when given, through the
// harness's lenient emitter: no panic, no refused record and only typed hostile-input errors.
func fuzzBook(t *testing.T, db, wal, journal, images []byte) {
	t.Helper()
	if len(images) == 0 {
		parsertest.FuzzParseIOS(t, iosaddressbook.Parser{}, remote, logical, db, wal, journal)
		return
	}
	files := map[string][]byte{"db": db, "images": images}
	if len(wal) > 0 {
		files["wal"] = wal
	}
	if len(journal) > 0 {
		files["journal"] = journal
	}
	parsertest.FuzzParseBundle(t, iosaddressbook.Parser{}, func(h *parsertest.Harness) parsertest.Bundle {
		return parsertest.Fixture{Name: "fuzz", Files: files}.BundleIOSBackup(h, "HomeDomain", relative)
	})
}

// FuzzParseAddressBook mutates whole databases (and their WAL, journal and images database) and requires the
// contract to hold. A seed with an images database runs through the backup entry point, because that is the
// only place the images role exists. Seeds run under go test; go test -fuzz is run manually.
func FuzzParseAddressBook(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal, s.images)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal, images []byte) {
		fuzzBook(t, db, wal, journal, images)
	})
}

// Every seed is a database that parses to records: a seed that gave none would let the fuzz target pass
// without ever reaching the mapper.
func TestEveryFuzzSeedEmitsRecords(t *testing.T) {
	for _, s := range fuzzSeeds(t) {
		files := map[string][]byte{"db": s.db}
		if len(s.wal) > 0 {
			files["wal"] = s.wal
		}
		if len(s.images) > 0 {
			files["images"] = s.images
		}
		h := parsertest.NewHarness(t)
		b := parsertest.Fixture{Name: s.name, Files: files}.BundleIOSBackup(h, "HomeDomain", relative)
		p := iosaddressbook.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
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

// pageMutations are the deterministic page-level hostile mutations: zeroed pages, a flipped page type, an
// absurd cell count, a wild first-free block and a wild cell offset.
var pageMutations = map[string]func(p []byte){
	"zeroed":      func(p []byte) { clear(p) },
	"type flip":   func(p []byte) { p[0] ^= 0x08 },
	"cell count":  func(p []byte) { p[3], p[4] = 0xff, 0xff },
	"first free":  func(p []byte) { p[1], p[2] = 0xff, 0xff },
	"cell offset": func(p []byte) { p[8], p[9] = 0xff, 0xf0 },
}

// mutated returns a copy of db with mut applied to page pg (the file header of page 1 kept: it has its own test).
func mutated(db []byte, ps, pg int, mut func([]byte)) []byte {
	m := append([]byte(nil), db...)
	page := m[pg*ps : (pg+1)*ps]
	if pg == 0 {
		page = page[100:]
	}
	mut(page)
	return m
}

// Page-level hostile mutations, in every page of each committed database and of the images database of
// with-images, each fuzzed through the iOS entry points. Every page of current and of the images database is
// mutated; the other variants (which share the schema and the table layout) in their first, second, middle and
// last page, to bound the run time. The subtests run in parallel.
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		db, images := fx.Files["db"], fx.Files["images"]
		ps := pageSize(db)
		if len(db) < 2*ps {
			t.Fatalf("%s: %d bytes is not a database with a table to damage", v, len(db))
		}
		pages := len(db) / ps
		pick := func(n, pg int) bool { return v == "current" || pg <= 1 || pg == n/2 || pg == n-1 }
		for pg := range pages {
			if !pick(pages, pg) {
				continue
			}
			for name, mut := range pageMutations {
				m := mutated(db, ps, pg, mut)
				t.Run(fmt.Sprintf("%s/page %d/%s", v, pg, name), func(t *testing.T) { t.Parallel(); fuzzBook(t, m, nil, nil, images) })
			}
		}
		if len(images) == 0 {
			continue
		}
		ips := pageSize(images)
		for pg := range len(images) / ips {
			for name, mut := range pageMutations {
				m := mutated(images, ips, pg, mut)
				t.Run(fmt.Sprintf("%s/images page %d/%s", v, pg, name), func(t *testing.T) { t.Parallel(); fuzzBook(t, db, nil, nil, m) })
			}
		}
	}
}
