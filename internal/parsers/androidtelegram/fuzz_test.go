package androidtelegram_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// fuzzParser is the parser with its own typed outcomes accepted: a database at the app path without the message table
// of its layout, a schema read in part without the signature, and a user version outside the band are the documented
// answers to hostile input (spec 8.1, 12), not contract breaks. Every other error is still refused by
// parsertest.FuzzParse unless it is one of the decoder's typed hostile-input errors.
type fuzzParser struct{ androidtelegram.Parser }

func (p fuzzParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	err := p.Parser.Parse(ctx, in, out)
	for _, typed := range []error{androidtelegram.ErrNoMessageTable, androidtelegram.ErrSchemaIncomplete, androidtelegram.ErrUnsupportedVersion} {
		if errors.Is(err, typed) {
			return nil
		}
	}
	return err
}

// maxSeedBytes is the largest file a fuzz seed may hold (T59): a big seed slows every execution, so the fuzzer
// explores nothing.
const maxSeedBytes = 64 << 10

type fuzzSeed struct {
	name             string
	db, wal, journal []byte
}

// minimalSeed is a minimal v181 database: one incoming message with a reply copy in a user dialog, one outgoing
// message in a basic group, its chats row and the users row of the sender (Go-built, one page per table).
func minimalSeed() []byte {
	in := tlMessage{id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "seed", tail: tlStr("/sdcard/s.jpg")}
	reply := tlMessage{id: 9, from: idSelf, peer: idAnn, date: baseDate, text: "quoted"}
	group := tlMessage{flags: bitOut, id: 2, peerObj: peerChat(2000001), date: baseDate, text: "to the group"}
	v2 := []msgCells{
		{mid: 1, uid: idAnn, data: in.bytes(), replydata: reply.bytes()},
		{mid: 2, uid: -2000001, out: int64(1), read: int64(0), data: group.bytes()},
	}
	first := "Ann"
	users := builtTable{name: "users", ddl: ddlUsers, cols: []string{"uid", "name", "status", "data"}, alias: "uid", rows: []builtRow{
		{idAnn, map[string]any{"name": "ann", "status": int64(-100), "data": tlUser(idAnn, 0, false, &first, nil, nil, nil)}},
	}}
	return goBuiltWith(181, v2, users, chatsTable(builtRow{2000001, map[string]any{"name": "family", "data": tlChat(2000001, "Family")}}))
}

// firstPage is telegram-v181's first page: its schema, with every table root beyond the file.
func firstPage(t testing.TB) []byte {
	db := parsertest.LoadFixture(t, "testdata", "telegram-v181").Files["db"]
	ps := int(db[16])<<8 | int(db[17])
	return append([]byte(nil), db[:ps]...)
}

// fuzzSeeds are the hand-built minimal database and telegram-v181's first page (the committed fixtures are larger
// than the limit once decompressed).
func fuzzSeeds(t testing.TB) []fuzzSeed {
	return []fuzzSeed{{name: "minimal-v181", db: minimalSeed()}, {name: "telegram-v181 page 1", db: firstPage(t)}}
}

func TestFuzzSeedsAreSmall(t *testing.T) {
	for _, s := range fuzzSeeds(t) {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
	// the minimal seed parses to its three records (two messages and one contact), so the fuzzer starts from a
	// database that reaches every mapping and the chats join
	res := parseAt(t, remoteApp, dbOnly(minimalSeed()))
	if res.Err != nil || len(res.Records) != 3 || len(res.Violations) != 0 {
		t.Fatalf("minimal seed: err %v, %d records, violations %v", res.Err, len(res.Records), res.Violations)
	}
	if title := threadOf(byRow(res.Records)["messages_v2/2"])["title"]; title != "Family" {
		t.Errorf("minimal seed: the group message's title %v, want Family from the chats join", title)
	}
}

// FuzzParseAndroidTelegram mutates whole databases (and their WAL and journal) at the app path and requires the
// contract to hold: no panic, no refused record, and only typed errors (spec 12, 13). Seeds run under go test;
// go test -fuzz is run manually (docs/testing.md).
func FuzzParseAndroidTelegram(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		parsertest.FuzzParse(t, fuzzParser{}, remoteApp, db, wal, journal)
	})
}

// sameSizeClass maps a one-byte serial type to a different storage class of the same body size (T67, T71), so the
// record stays well formed and the wrong-class cell reaches the mapper. Type 0 and the multi-byte serial types are
// left alone (restated from androidcalendar/fuzz_test.go, F29).
func sameSizeClass(st byte) (byte, bool) {
	switch {
	case st >= 1 && st <= 4:
		return 15 + 2*(st-1), true // int of 1..4 bytes -> text of 1..4 bytes
	case st == 5:
		return 25, true // 48-bit int -> text of 6 bytes
	case st == 8:
		return 13, true // the constant 0 -> empty text
	case st == 9:
		return 12, true // the constant 1 -> empty blob
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

// rewriteSerialTypes gives every cell of a table leaf page (type 0x0d) the same-size storage class of another kind in
// each one-byte serial type of its record header. base is the offset the caller stripped from the page start (100 on
// page 1).
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
		_, a, ok := varint(at)
		if !ok {
			continue
		}
		_, b, ok := varint(at + a)
		if !ok {
			continue
		}
		hs, c, ok := varint(at + a + b)
		if !ok {
			continue
		}
		for j := at + a + b + c; j < at+a+b+hs && j < len(p); j++ {
			if p[j] >= 0x80 {
				break
			}
			if ns, ok := sameSizeClass(p[j]); ok {
				p[j] = ns
			}
		}
	}
}

// Page-level hostile mutations, deterministic (spec 12, damage): zeroed pages, a flipped page type, an absurd cell
// count, a wild first-free block and cell offset, and every cell's serial types rewritten to another storage class, in
// every page of the minimal seed; and the serial-type rewrite (wrong-class cells reaching the mappings) in every page
// of telegram-deleted (freelist history and a WAL), where all six would take longer than the 60 s of docs/testing.md.
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	muts := map[string]func(p []byte, base int){
		"zeroed":      func(p []byte, _ int) { clear(p) },
		"type flip":   func(p []byte, _ int) { p[0] ^= 0x08 },
		"cell count":  func(p []byte, _ int) { p[3], p[4] = 0xff, 0xff },
		"first free":  func(p []byte, _ int) { p[1], p[2] = 0xff, 0xff },
		"cell offset": func(p []byte, _ int) { p[8], p[9] = 0xff, 0xf0 },
		"serial type": rewriteSerialTypes,
	}
	deleted := fixtureFiles(t, "telegram-deleted")
	for _, in := range []struct {
		name    string
		db, wal []byte
		only    string // "": every mutation
	}{{"minimal-v181", minimalSeed(), nil, ""}, {"telegram-deleted", deleted["db"], deleted["wal"], "serial type"}} {
		ps := int(in.db[16])<<8 | int(in.db[17])
		if len(in.db) < 2*ps {
			t.Fatalf("%s: %d bytes is not a database with a table to damage", in.name, len(in.db))
		}
		for pg := 0; pg < len(in.db)/ps; pg++ {
			for name, mut := range muts {
				if in.only != "" && name != in.only {
					continue
				}
				m := append([]byte(nil), in.db...)
				page, base := m[pg*ps:(pg+1)*ps], 0
				if pg == 0 {
					page, base = page[100:], 100 // keep the file header: it is covered by its own test
				}
				mut(page, base)
				t.Run(fmt.Sprintf("%s/page %d/%s", in.name, pg, name), func(t *testing.T) {
					parsertest.FuzzParse(t, fuzzParser{}, remoteApp, m, in.wal, nil)
				})
			}
		}
	}
}
