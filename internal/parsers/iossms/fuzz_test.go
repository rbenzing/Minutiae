package iossms_test

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/iossms"
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

// minimalSMS is a hand-built sms.db of 512-byte pages: one message and the handle it names.
func minimalSMS() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	b.CreateTable("message", "create table message(ROWID integer primary key autoincrement, guid text, text text, handle_id integer, service text, date integer, is_from_me integer)").
		Insert(1, int64(1), "G1", "hello", int64(1), "iMessage", int64(704628000), int64(0))
	b.CreateTable("handle", "create table handle(ROWID integer primary key autoincrement, id text)").Insert(1, int64(1), "+15551234567")
	return b.Bytes()
}

// minimalWAL is the same database with a second message that only the WAL holds: the database file is the
// state before it, the WAL the frames that add it. It is a seed for the WAL reader; it is not required to
// parse to the second message.
func minimalWAL() (db, wal []byte) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	msg := b.CreateTable("message", "create table message(ROWID integer primary key autoincrement, guid text, text text, handle_id integer, service text, date integer, is_from_me integer)")
	msg.Insert(1, int64(1), "G1", "hello", int64(1), "iMessage", int64(704628000), int64(0))
	b.CreateTable("handle", "create table handle(ROWID integer primary key autoincrement, id text)").Insert(1, int64(1), "+15551234567")
	db = b.Bytes()
	base := b.Snapshot()
	msg.Insert(2, int64(2), "G2", "again", int64(1), "iMessage", int64(704628001), int64(1))
	w := b.NewWAL(false, 1, 2, 0)
	b.CommitTo(w, base)
	return db, w.Bytes()
}

// fuzzSeeds are the committed fixtures that fit the seed limit (none does today: the smallest is 68 KB, and ios17-nanos and ios17-wal
// are covered by the page-level mutations and the oracle) and the minimal databases.
func fuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	wdb, wwal := minimalWAL()
	seeds := []fuzzSeed{{name: "minimal", db: minimalSMS()}, {name: "minimal-wal", db: wdb, wal: wwal}}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Files["db"]) > maxSeedBytes {
			continue
		}
		seeds = append(seeds, fuzzSeed{name: v, db: fx.Files["db"], wal: fx.Files["wal"], journal: fx.Files["journal"]})
	}
	return seeds
}

func TestFuzzSeedsAreSmall(t *testing.T) {
	seeds := fuzzSeeds(t)
	if len(seeds) < 2 {
		t.Errorf("%d seeds: want at least the two minimal databases", len(seeds))
	}
	for _, s := range seeds {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
}

// Every seed is a database that parses to records: a seed that gave none would let the fuzz target pass without
// ever reaching the mapper.
func TestEveryFuzzSeedEmitsRecords(t *testing.T) {
	for _, s := range fuzzSeeds(t) {
		files := map[string][]byte{"db": s.db}
		if len(s.wal) > 0 {
			files["wal"] = s.wal
		}
		h := parsertest.NewHarness(t)
		b := parsertest.Fixture{Name: s.name, Files: files}.BundleIOS(h, dbRemote, dbLogical)
		p := iossms.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		if res.Err != nil || len(res.Violations) != 0 || len(res.Records) == 0 {
			t.Errorf("seed %s: err %v, violations %v, %d records", s.name, res.Err, res.Violations, len(res.Records))
		}
	}
}

// hostileRun is parsertest.FuzzParseBundle with one more rule: a record the writer refused is a failure. The lenient
// harness turns a refusal into a warning, which the shared helper lets pass; for a parser whose every record
// must validate, whatever the input, that hides a mapping bug.
func hostileRun(t *testing.T, build func(h *parsertest.Harness) parsertest.Bundle) {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := build(h)
	p := iossms.Parser{}
	if _, err := p.Probe(t.Context(), b.Input(h, p, true)); err != nil && !parsertest.HostileOK(err) {
		t.Fatalf("Probe returned %v", err)
	}
	res := h.RunLenient(p, b.Input(h, p, false))
	if res.Err != nil && !parsertest.HostileOK(res.Err) {
		t.Fatalf("Parse returned %v", res.Err)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("contract violations: %v", res.Violations)
	}
	for _, w := range res.Warnings {
		if strings.HasPrefix(w.Reason, "record refused") {
			t.Fatalf("the writer refused a record: %+v", w)
		}
	}
}

// FuzzParseIOSSMS mutates whole databases (and their WAL and journal) and requires the contract to hold: no
// panic, no refused record, and only typed hostile-input errors. It runs through the iOS entry point, because
// the harness refuses an iOS parser's record on an Android artifact. Seeds run under go test; go test -fuzz is
// run manually.
func FuzzParseIOSSMS(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		files := map[string][]byte{"db": db}
		if len(wal) > 0 {
			files["wal"] = wal
		}
		if len(journal) > 0 {
			files["journal"] = journal
		}
		hostileRun(t, func(h *parsertest.Harness) parsertest.Bundle {
			return parsertest.Fixture{Name: "fuzz", Files: files}.BundleIOS(h, dbRemote, dbLogical)
		})
	})
}

func pageSize(db []byte) int {
	ps := int(binary.BigEndian.Uint16(db[16:18]))
	if ps == 1 {
		return 65536
	}
	return ps
}

// pageMutations are the deterministic page-level hostile mutations: zeroed pages, a flipped page type, an
// absurd cell count, a wild first-free block and a wild cell offset, and a same-size serial-type rewrite (the
// first cell of a leaf keeps its length but its record header claims other storage classes).
var pageMutations = map[string]func(p []byte){
	"zeroed":      func(p []byte) { clear(p) },
	"type flip":   func(p []byte) { p[0] ^= 0x08 },
	"cell count":  func(p []byte) { p[3], p[4] = 0xff, 0xff },
	"first free":  func(p []byte) { p[1], p[2] = 0xff, 0xff },
	"cell offset": func(p []byte) { p[8], p[9] = 0xff, 0xf0 },
	"serial types": func(p []byte) {
		// rewrite each record-header byte after a plausible header length to another one-byte serial type of a
		// different storage class, leaving every length in place where it matters (a same-size rewrite)
		for i := 0; i+2 < len(p); i++ {
			if p[i] >= 2 && p[i] <= 40 && p[i+1] <= 9 {
				p[i+1] = 7 - p[i+1]%8 // an integer serial becomes a float or a NULL, and back
			}
		}
	},
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

// Page-level hostile mutations, fuzzed through the backup bundle of ios17-nanos (so its attachment files are in
// the case and the attachment links are reached): the first two, the last and every fourth page of the 100-page database (every page takes 45 s). The subtests run in parallel.
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "ios17-nanos")
	db := fx.Files["db"]
	ps := pageSize(db)
	pages := len(db) / ps
	if pages < 8 {
		t.Fatalf("%d pages is not a database with tables to damage", pages)
	}
	for pg := range pages {
		if pg > 1 && pg != pages-1 && pg%4 != 0 {
			continue
		}
		for name, mut := range pageMutations {
			m := mutated(db, ps, pg, mut)
			t.Run(fmt.Sprintf("page %d/%s", pg, name), func(t *testing.T) {
				t.Parallel()
				files := map[string][]byte{"db": m}
				hostileRun(t, func(h *parsertest.Harness) parsertest.Bundle {
					return parsertest.Fixture{Name: "mut", Files: files, Expect: fx.Expect}.BundleIOSBackup(h, domain, rel)
				})
			})
		}
	}
}
