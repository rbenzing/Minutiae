package androidwacontacts_test

import (
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidwacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// maxSeedBytes is the largest file a fuzz seed may hold (T59): a big seed slows every
// execution, so the fuzzer explores nothing.
const maxSeedBytes = 16 << 10

type fuzzSeed struct {
	name             string
	db, wal, journal []byte
}

// minimalSeed is a hand-built database of 512-byte pages with one user and one group row.
func minimalSeed() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	t := b.CreateTable("wa_contacts", "create table wa_contacts(_id integer primary key, jid text not null, is_whatsapp_user integer, status text, number text, display_name text, wa_name text)")
	t.Insert(1, nil, "15550000001@s.whatsapp.net", int64(1), "busy", "+15550000001", "Ann", "Annie")
	t.Insert(2, nil, "120363000000000001@g.us", int64(0), nil, nil, "Family", nil)
	return b.Bytes()
}

// fullSeed is a hand-built wa.db of 512-byte pages with the full constructed column set, a user, a LID, a group and a
// nameless row, and a WAL holding one more row.
func fullSeed() (db, wal []byte) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	t := b.CreateTable("wa_contacts", "CREATE TABLE wa_contacts (_id INTEGER PRIMARY KEY AUTOINCREMENT, jid TEXT NOT NULL, is_whatsapp_user INTEGER, status TEXT, status_timestamp INTEGER, "+
		"number TEXT, raw_contact_id INTEGER, display_name TEXT, phone_type INTEGER, phone_label TEXT, unseen_msg_count INTEGER, photo_ts INTEGER, thumb_ts INTEGER, "+
		"photo_id_timestamp INTEGER, given_name TEXT, family_name TEXT, wa_name TEXT, sort_name TEXT, nickname TEXT, company TEXT, title TEXT, offset INTEGER)")
	t.Insert(1, nil, "15550000001@s.whatsapp.net", int64(1), "busy", int64(1700000000000), "+15550000001", int64(7), "Ann Lee", int64(2), nil, int64(0), nil, nil, nil, "Ann", "Lee", "Annie", "Lee Ann", nil, "Co", nil, nil)
	t.Insert(2, nil, "99887766554433@lid", int64(1), nil, nil, nil, nil, "Bo", nil, nil, nil, nil, nil, nil, nil, nil, "Bo", nil, nil, nil, nil, nil)
	t.Insert(3, nil, "120363000000000001@g.us", int64(0), nil, nil, nil, nil, "Family", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	t.Insert(4, nil, "15550000004@s.whatsapp.net", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	return b.Bytes(), b.NewWAL(false, 1, 2, 0).Bytes()
}

// fuzzSeeds are the hand-built databases and the committed fixtures that fit the limit (the others have 4 KiB pages
// and the same table with the same columns, which the hand-built seeds stand for).
func fuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	fdb, fwal := fullSeed()
	seeds := []fuzzSeed{{name: "minimal", db: minimalSeed()}, {name: "full", db: fdb, wal: fwal}}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if seedTooBig(fx) {
			continue
		}
		seeds = append(seeds, fuzzSeed{name: v, db: fx.Files["db"], wal: fx.Files["wal"], journal: fx.Files["journal"]})
	}
	return seeds
}

func seedTooBig(fx parsertest.Fixture) bool {
	for _, data := range fx.Files {
		if len(data) > maxSeedBytes {
			return true
		}
	}
	return false
}

func TestFuzzSeedsAreSmall(t *testing.T) {
	for _, s := range fuzzSeeds(t) {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
	if db, _ := fullSeed(); len(db) > 8<<10 {
		t.Errorf("the hand-built wa.db seed is %d bytes: it should be a few KiB", len(db))
	}
	if res := parseBytes(t, minimalSeed()); res.res.Err != nil || len(res.res.Records) == 0 {
		t.Errorf("the minimal seed: err %v, %d records", res.res.Err, len(res.res.Records))
	}
}

// FuzzParseAndroidWAContacts mutates whole databases (and their WAL and journal) and requires the
// contract to hold: no panic, no refused record, and only typed errors. Seeds run under
// go test; go test -fuzz is run manually.
func FuzzParseAndroidWAContacts(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		parsertest.FuzzParse(t, androidwacontacts.Parser{}, remoteLive, db, wal, journal)
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
// rewritten to another storage class (wrong-class cells), in every page of the wa.db
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
	for _, v := range []string{"wa-2.11", "wa-business", "wa-deleted"} {
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
					parsertest.FuzzParse(t, androidwacontacts.Parser{}, remoteFor(v), m, nil, nil)
				})
			}
		}
	}
}
