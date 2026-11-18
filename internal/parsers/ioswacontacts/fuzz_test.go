package ioswacontacts_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// fuzzParser is the parser with its own typed outcome accepted: a database that lacks the identifying table is the documented
// answer to hostile input, not a contract break. A path refusal is not one (the fuzz place is fixed), so it fails. Every other
// error is still refused by parsertest.FuzzParseIOS unless it is one of the decoder's typed hostile-input errors.
type fuzzParser struct{ ioswacontacts.Parser }

func (p fuzzParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	err := p.Parser.Parse(ctx, in, out)
	if errors.Is(err, ioswacontacts.ErrNoWhatsAppTables) {
		return nil
	}
	return err
}

// maxSeedBytes is the largest file a fuzz seed may hold (T59): a big seed slows every execution, so the fuzzer explores
// nothing. The WhatsApp packages hold it at 16 KiB.
const maxSeedBytes = 16 << 10

// fuzzLogical is the consumer backup domain's logical path: the fuzzed bytes are always a primary at a domain place.
const fuzzLogical = "ios:" + domainConsumer + "/" + dbName

type fuzzSeed struct {
	name             string
	db, wal, journal []byte
}

// richStore is a hand-built ContactsV2 of 512-byte pages with a contact for every state of the JID, the LID and the label the
// mapping tells apart, and the rows that are counted warnings, far below the limit.
func richStore() (*sqlitetest.Builder, *sqlitetest.Table) {
	return contactBuilder(512, nil, func(put putRow) {
		const user = "15550000001@s.whatsapp.net"
		put(1, contactRow(map[string]any{"ZFULLNAME": "Ann Lee", "ZGIVENNAME": "Ann", "ZPHONENUMBER": "+15550000001", "ZPHONENUMBERLABEL": "_$!<Mobile>!$_", "ZWHATSAPPID": user, "ZABOUTTEXT": "busy", "ZABOUTTIMESTAMP": int64(600000000), "ZIDENTIFIER": "A1", "ZLID": "100000000000001@lid"}))
		put(2, contactRow(map[string]any{"ZPHONENUMBER": "+2", "ZPHONENUMBERLABEL": "Work line"}))
		put(3, contactRow(map[string]any{"ZFULLNAME": "Bob", "ZWHATSAPPID": user}))
		put(4, contactRow(map[string]any{"ZWHATSAPPID": user}))
		put(5, contactRow(map[string]any{"ZFULLNAME": "Group", "ZWHATSAPPID": "120363000000000001@g.us"}))
		put(6, contactRow(map[string]any{"ZGIVENNAME": "Cy", "ZWHATSAPPID": user}))
		put(7, contactRow(map[string]any{"ZFULLNAME": "Eve", "ZWHATSAPPID": "1\x00@s.whatsapp.net", "ZLID": "1\x00@lid"}))
		put(8, contactRow(map[string]any{"ZFULLNAME": "Fay", "ZWHATSAPPID": "100000000000009@lid", "ZLID": "100000000000009@lid"}))
		put(9, contactRow(map[string]any{"ZFULLNAME": []byte("blob"), "ZWHATSAPPID": int64(9), "ZLID": int64(9)}))
		put(10, contactRow(map[string]any{"ZFULLNAME": "Hal", "ZWHATSAPPID": "-1", "ZHIGHLIGHTEDNAME": "Hal H"}))
		put(11, contactRow(map[string]any{"ZIDENTIFIER": "only"}))
		for i := int64(12); i < 40; i++ { // padding: a store of several pages for the page-level mutations
			put(i, contactRow(map[string]any{"ZFULLNAME": fmt.Sprintf("pad %d %s", i, "................................................"), "ZWHATSAPPID": fmt.Sprintf("1555%07d@s.whatsapp.net", i)}))
		}
	})
}

// minimalStore is the smallest ContactsV2 that parses to a record: one contact.
func minimalStore() []byte {
	b, _ := contactBuilder(512, nil, func(put putRow) {
		put(1, contactRow(map[string]any{"ZFULLNAME": "Ann", "ZWHATSAPPID": "15550000001@s.whatsapp.net"}))
	})
	return b.Bytes()
}

// walStore is the minimal store with a second contact that only the WAL holds: the database is the state before it, the WAL the
// frames that add it. It is a seed for the WAL reader; it is not required to parse to the second contact.
func walStore() (db, wal []byte) {
	b, tb := contactBuilder(512, nil, func(put putRow) {
		put(1, contactRow(map[string]any{"ZFULLNAME": "Ann", "ZWHATSAPPID": "15550000001@s.whatsapp.net"}))
	})
	db = b.Bytes()
	base := b.Snapshot()
	tb.Insert(2, nil, int64(1), int64(1), nil, nil, "Bob", nil, nil, nil, nil, nil, nil, "15550000002@s.whatsapp.net")
	w := b.NewWAL(false, 1, 2, 0)
	b.CommitTo(w, base)
	return db, w.Bytes()
}

// journalStore is the minimal store with a hot journal that rolls a rewrite of contact 1 back.
func journalStore() (db, journal []byte) {
	b, tb := contactBuilder(512, nil, func(put putRow) {
		put(1, contactRow(map[string]any{"ZFULLNAME": "before", "ZWHATSAPPID": "15550000001@s.whatsapp.net"}))
	})
	snap := b.Snapshot()
	tb.Update(1, nil, int64(1), int64(1), nil, nil, "after", nil, nil, nil, nil, nil, nil, "15550000001@s.whatsapp.net")
	root := tb.Root()
	db = b.Bytes()
	j := b.NewJournal(512, 0x5eed0001, snap.Pages())
	j.Record(root, snap.Page(root))
	return db, j.Bytes()
}

// fuzzSeeds are the hand-built databases and the first page of the committed ios-contactsv2 (the fixtures themselves are over
// the limit).
func fuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	rich, _ := richStore()
	wdb, wal := walStore()
	jdb, jrn := journalStore()
	first := parsertest.LoadFixture(t, "testdata", "ios-contactsv2").Files["db"][:4096]
	return []fuzzSeed{
		{name: "minimal", db: minimalStore()},
		{name: "rich", db: rich.Bytes()},
		{name: "minimal-wal", db: wdb, wal: wal},
		{name: "minimal-journal", db: jdb, journal: jrn},
		{name: "ios-contactsv2 first page", db: append([]byte(nil), first...)},
	}
}

func TestFuzzSeedsAreSmall(t *testing.T) {
	seeds := fuzzSeeds(t)
	if len(seeds) != 5 {
		t.Errorf("%d seeds, want the five hand-built and committed ones", len(seeds))
	}
	for _, s := range seeds {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
	// the hand-built databases parse to records through the fuzz entry itself (the bundle FuzzParseIOS builds: BundleIOS at
	// extractRemote and fuzzLogical): a seed that gave none would let the target pass without reaching the mapper
	for _, s := range seeds {
		if s.name == "ios-contactsv2 first page" {
			continue
		}
		files := map[string][]byte{"db": s.db}
		if s.wal != nil {
			files["wal"] = s.wal
		}
		if s.journal != nil {
			files["journal"] = s.journal
		}
		h := parsertest.NewHarness(t)
		b := parsertest.Fixture{Name: "fuzz", Files: files}.BundleIOS(h, extractRemote, fuzzLogical)
		p := fuzzParser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		if res.Err != nil || len(res.Records) == 0 {
			t.Errorf("seed %s: err %v, %d records", s.name, res.Err, len(res.Records))
		}
	}
}

// FuzzParseIOSWhatsAppContacts mutates whole databases (and their WAL and journal) and requires the contract to hold: no panic,
// no refused record, and only typed errors. It runs through the iOS entry point at the consumer domain's logical path, because
// the harness refuses an iOS parser's record on an Android artifact. Seeds run under go test; go test -fuzz is run manually.
func FuzzParseIOSWhatsAppContacts(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		parsertest.FuzzParseIOS(t, fuzzParser{}, extractRemote, fuzzLogical, db, wal, journal)
	})
}

// sameSizeClass maps a one-byte serial type to a different storage class of the same body size (T67, T71): an integer becomes
// text or a float, text becomes a blob and back, and the body-less constants 0 and 1 (types 8 and 9) become empty text and an
// empty blob, so the record stays well formed and the wrong-class cell reaches the mapper. Type 0 (NULL, the rowid alias) and
// the multi-byte serial types (a text or blob of 58 bytes or more) are left alone.
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

// rewriteSerialTypes gives every cell of a table leaf page (type 0x0d) the same-size storage class of another kind in each
// one-byte serial type of its record header. base is the offset the caller stripped from the page start (100 on page 1), so
// the cell pointers, which count from the page start, are shifted by it.
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

// Page-level hostile mutations, deterministic: zeroed pages, a flipped page type, an absurd cell count, a wild first-free
// block and cell offset, and every cell's serial types rewritten to another storage class (wrong-class cells), in every page
// of the hand-built CallHistory that has a row in every table.
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	muts := map[string]func(p []byte, base int){
		"zeroed":      func(p []byte, _ int) { clear(p) },
		"type flip":   func(p []byte, _ int) { p[0] ^= 0x08 },
		"cell count":  func(p []byte, _ int) { p[3], p[4] = 0xff, 0xff },
		"first free":  func(p []byte, _ int) { p[1], p[2] = 0xff, 0xff },
		"cell offset": func(p []byte, _ int) { p[8], p[9] = 0xff, 0xf0 },
		"serial type": rewriteSerialTypes,
	}
	b, _ := richStore()
	db := b.Bytes()
	const ps = 512
	if len(db) < 8*ps {
		t.Fatalf("%d bytes is not a database with tables to damage", len(db))
	}
	ran := 0
	for pg := 0; pg < len(db)/ps; pg++ {
		for name, mut := range muts {
			m := append([]byte(nil), db...)
			page, base := m[pg*ps:(pg+1)*ps], 0
			if pg == 0 {
				page, base = page[100:], 100 // keep the file header: it is covered by its own test
			}
			mut(page, base)
			t.Run(fmt.Sprintf("page %d/%s", pg, name), func(t *testing.T) {
				t.Parallel()
				parsertest.FuzzParseIOS(t, fuzzParser{}, extractRemote, fuzzLogical, m, nil, nil)
			})
			ran++
		}
	}
	if ran < len(muts)*8 {
		t.Errorf("%d mutations ran, want every page of the store", ran)
	}
}
