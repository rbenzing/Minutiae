package androidcontacts_test

import (
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidcontacts"
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

// minimalSeed is a hand-built database of 512-byte pages with one contact, its phone, its account and its aggregate.
func minimalSeed() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	b.CreateTable("mimetypes", "create table mimetypes(_id integer primary key, mimetype text not null)").
		Insert(1, int64(1), "vnd.android.cursor.item/phone_v2")
	b.CreateTable("data", "create table data(_id integer primary key, mimetype_id integer, raw_contact_id integer, is_primary integer, data1 text, data2 text)").
		Insert(1, int64(1), int64(1), int64(1), int64(1), "+15551234567", "2")
	b.CreateTable("accounts", "create table accounts(_id integer primary key, account_name text, account_type text, data_set text)").
		Insert(1, int64(1), "a@example.test", "com.google", nil)
	b.CreateTable("contacts", "create table contacts(_id integer primary key, contact_last_updated_timestamp integer)").
		Insert(1, int64(1), int64(1682935201000))
	b.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, account_id integer, contact_id integer, display_name text)").
		Insert(1, int64(1), int64(1), int64(1), "Alice")
	return b.Bytes()
}

// fuzzSeeds are the committed fixtures that fit under maxSeedBytes and the minimal database. The
// huge-contact fixture (160 KiB, one contact over the list caps) is the exception: it has its own tests and is
// not a seed.
func fuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	seeds := []fuzzSeed{{name: "minimal", db: minimalSeed()}}
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
	if len(seeds) != len(variants) { // every variant but huge-contact, plus the minimal database
		t.Errorf("%d seeds for %d variants (huge-contact excluded) and the minimal database", len(seeds), len(variants))
	}
	for _, s := range seeds {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
}

// FuzzParseContacts mutates whole databases (and their WAL and journal) and requires the
// contract to hold: no panic, no refused record, and only typed errors. Seeds run under
// go test; go test -fuzz is run manually.
func FuzzParseContacts(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		parsertest.FuzzParse(t, androidcontacts.Parser{}, fixtureRemote, db, wal, journal)
	})
}

// sameSizeClass maps a one-byte serial type to a different storage class of the same body
// size (T67, T71): an integer becomes text or a float, text becomes a blob and back, and the
// body-less constants 0 and 1 (types 8 and 9) become empty text and an empty blob, so the record
// stays well formed and the wrong-class cell reaches the mapper. Type 0 (NULL, the rowid alias)
// and the multi-byte serial types (a text or blob of 58 bytes or more) are left alone.
func sameSizeClass(st byte) (byte, bool) {
	switch {
	case st >= 1 && st <= 4:
		return 15 + 2*(st-1), true // int of 1..4 bytes -> text of 1..4 bytes
	case st == 5:
		return 25, true // 48-bit int -> text of 6 bytes (13 + 2*6)
	case st == 8:
		return 13, true // the constant 0 (no body) -> empty text
	case st == 9:
		return 12, true // the constant 1 (no body) -> empty blob
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

// bodySize is the body length of a serial type (the file format's table), -1 for a reserved one.
func bodySize(st int) int {
	switch {
	case st == 0 || st == 8 || st == 9:
		return 0
	case st >= 1 && st <= 4:
		return st
	case st == 5:
		return 6
	case st == 6 || st == 7:
		return 8
	case st >= 12:
		return (st - 12) / 2
	}
	return -1
}

// Every rewrite keeps the body size (the record stays well formed) and changes the storage class.
func TestSameSizeClassKeepsTheSizeAndChangesTheClass(t *testing.T) {
	class := func(st int) string {
		switch {
		case st == 0:
			return "null"
		case st <= 6 || st == 8 || st == 9:
			return "int"
		case st == 7:
			return "float"
		case st%2 == 0:
			return "blob"
		}
		return "text"
	}
	for st := 1; st < 0x80; st++ {
		if st == 10 || st == 11 {
			continue
		}
		ns, ok := sameSizeClass(byte(st))
		if !ok {
			t.Errorf("serial type %d has no same-size rewrite", st)
			continue
		}
		if bodySize(int(ns)) != bodySize(st) || class(int(ns)) == class(st) {
			t.Errorf("serial type %d -> %d: size %d -> %d, class %s -> %s", st, ns, bodySize(st), bodySize(int(ns)), class(st), class(int(ns)))
		}
	}
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
// rewritten to another storage class (wrong-class cells), in every page of three contacts
// fixtures (which hold raw_contacts, data, mimetypes, accounts, contacts and deleted_contacts).
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	muts := map[string]func(p []byte, base int){
		"zeroed":      func(p []byte, _ int) { clear(p) },
		"type flip":   func(p []byte, _ int) { p[0] ^= 0x08 },
		"cell count":  func(p []byte, _ int) { p[3], p[4] = 0xff, 0xff },
		"first free":  func(p []byte, _ int) { p[1], p[2] = 0xff, 0xff },
		"cell offset": func(p []byte, _ int) { p[8], p[9] = 0xff, 0xf0 },
		"serial type": rewriteSerialTypes,
	}
	for _, v := range []string{"unicode-rtl", "deleted-contacts", "multi-account"} {
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
					parsertest.FuzzParse(t, androidcontacts.Parser{}, fixtureRemote, m, nil, nil)
				})
			}
		}
	}
}
