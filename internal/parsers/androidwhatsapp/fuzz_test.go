package androidwhatsapp_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidwhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// fuzzParser is the parser with its own typed outcomes accepted: a database with neither message table, one whose schema is
// damaged beyond reading, a deleted_chat_job that could not be read whole (every record was emitted first) and a database
// outside the live directory are the documented answers to hostile input, not contract breaks. Every other error is still
// refused by parsertest.FuzzParse unless it is one of the decoder's typed hostile-input errors.
type fuzzParser struct{ androidwhatsapp.Parser }

func (p fuzzParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	err := p.Parser.Parse(ctx, in, out)
	for _, typed := range []error{androidwhatsapp.ErrNeitherTable, androidwhatsapp.ErrSchemaUndecidable, androidwhatsapp.ErrPurgeJobsIncomplete, androidwhatsapp.ErrNotLiveDatabase} {
		if errors.Is(err, typed) {
			return nil
		}
	}
	return err
}

// maxSeedBytes is the largest file a fuzz seed may hold (T59): a big seed slows every
// execution, so the fuzzer explores nothing.
const maxSeedBytes = 64 << 10

type fuzzSeed struct {
	name             string
	db, wal, journal []byte
}

// legacySeed, transitionSeed and modernSeed are hand-built databases of the three generations, each far below the limit.
func legacySeed() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	m := b.CreateTable("messages", "create table messages(_id integer primary key, key_remote_jid text, key_from_me integer, key_id text, status integer, "+
		"timestamp integer, media_wa_type text, data text, remote_resource text, quoted_row_id integer)")
	b.CreateTable("chat_list", "create table chat_list(_id integer primary key, key_remote_jid text, subject text)").Insert(1, nil, "15550000001@s.whatsapp.net", "T")
	b.CreateTable("messages_quotes", "create table messages_quotes(_id integer primary key, key_id text, data text)")
	m.Insert(1, nil, "-1", nil, "K01", nil, nil, nil, nil, nil, nil)
	m.Insert(2, nil, "15550000001@s.whatsapp.net", int64(0), "K02", int64(0), goodTS, "0", "hello", nil, nil)
	m.Insert(3, nil, "120363000000000001@g.us", int64(1), "K03", int64(5), goodTS, "1", nil, "15550000002@s.whatsapp.net", nil)
	return b.Bytes()
}

func modernSeed(withLegacy bool) []byte {
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		jidRow(tb, 2, "120363000000000001", "g.us", "120363000000000001@g.us")
		tb["chat"].Insert(1, nil, int64(1), nil)
		tb["chat"].Insert(2, nil, int64(2), nil)
		msgRow(tb, 1, 1, 0, nil, 0, goodTS, "hello")
		msgRow(tb, 2, 2, 0, int64(1), 0, goodTS, "group")
		msgRow(tb, 3, 1, 0, nil, 4, goodTS, "card")
		tb["message_vcard"].Insert(1, nil, int64(3), "BEGIN:VCARD\r\nEND:VCARD\r\n")
		tb["call_log"].Insert(1, nil, int64(1), int64(0), "C1", goodTS, int64(0), int64(30), int64(5), int64(0))
		tb["message_add_on"].Insert(1, nil, int64(1), int64(0), "A1", nil, int64(1), goodTS, int64(56))
		tb["message_add_on_reaction"].Insert(1, nil, "x", goodTS)
		tb["deleted_chat_job"].Insert(1, nil, int64(2), int64(5), nil, nil, nil)
	})
	_ = withLegacy
	return db
}

func transitionSeed() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	m := b.CreateTable("messages", "create table messages(_id integer primary key, key_remote_jid text, key_from_me integer, key_id text, status integer, "+
		"timestamp integer, media_wa_type text, data text)")
	n := b.CreateTable("message", "create table message(_id integer primary key, chat_row_id integer, from_me integer, key_id text, "+
		"sender_jid_row_id integer, status integer, timestamp integer, received_timestamp integer, message_type integer, text_data text)")
	b.CreateTable("chat", "create table chat(_id integer primary key, jid_row_id integer, subject text)").Insert(1, nil, int64(1), nil)
	b.CreateTable("jid", "create table jid(_id integer primary key, user text, server text, agent integer, device integer, raw_string text)").
		Insert(1, nil, "15550000001", "s.whatsapp.net", nil, nil, "15550000001@s.whatsapp.net")
	m.Insert(1, nil, "-1", nil, "K01", nil, nil, nil, nil)
	m.Insert(2, nil, "15550000001@s.whatsapp.net", int64(0), "K02", int64(0), goodTS, "0", "hello")
	n.Insert(2, nil, int64(1), int64(0), "K02", int64(0), int64(0), goodTS, goodTS, int64(0), "hello")
	return b.Bytes()
}

// seedTooBig are the committed variants whose files exceed the seed limit (the modern and transition stores have 20 tables on
// 4 KiB pages): the hand-built seeds above stand for them.
func seedTooBig(fx parsertest.Fixture) bool {
	for _, data := range fx.Files {
		if len(data) > maxSeedBytes {
			return true
		}
	}
	return false
}

// fuzzSeeds are the hand-built databases and the committed fixtures that fit the limit.
func fuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	seeds := []fuzzSeed{{name: "legacy", db: legacySeed()}, {name: "transition", db: transitionSeed()}, {name: "modern", db: modernSeed(false)}}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if seedTooBig(fx) {
			continue
		}
		seeds = append(seeds, fuzzSeed{name: v, db: fx.Files["db"], wal: fx.Files["wal"], journal: fx.Files["journal"]})
	}
	return seeds
}

func TestFuzzSeedsAreSmall(t *testing.T) {
	seeds := fuzzSeeds(t)
	big := 0
	for _, v := range variants {
		if seedTooBig(parsertest.LoadFixture(t, "testdata", v)) {
			big++
		}
	}
	if big == 0 || len(seeds) != len(variants)-big+3 {
		t.Errorf("%d seeds for %d variants (%d too big) and the three hand-built databases", len(seeds), len(variants), big)
	}
	for _, s := range seeds {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
	// each generation is seeded: a hand-built database of each parses to records
	for name, db := range map[string][]byte{"legacy": legacySeed(), "transition": transitionSeed(), "modern": modernSeed(false)} {
		if res := runBytes(t, db, nil, remoteLive); res.Err != nil || len(res.Records) == 0 {
			t.Errorf("seed %s: err %v, %d records", name, res.Err, len(res.Records))
		}
	}
}

// FuzzParseAndroidWhatsApp mutates whole databases (and their WAL and journal) and requires the
// contract to hold: no panic, no refused record, and only typed errors. Seeds run under
// go test; go test -fuzz is run manually.
func FuzzParseAndroidWhatsApp(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		parsertest.FuzzParse(t, fuzzParser{}, remoteLive, db, wal, journal)
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
// rewritten to another storage class (wrong-class cells), in every page of the WhatsApp
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
	for _, v := range []string{"android-legacy-2.11", "android-modern-migrated"} {
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
					parsertest.FuzzParse(t, fuzzParser{}, remoteFor(v), m, nil, nil)
				})
			}
		}
	}
}
