package ioscalendar_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioscalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// fuzzParser is the parser with its own typed outcomes accepted: no CalendarItem table at the system path and a
// schema read in part without CalendarItem are the documented answers to hostile input, not contract breaks. Every
// other error is still refused by parsertest.FuzzParseIOS unless it is one of the decoder's typed hostile-input errors.
type fuzzParser struct{ ioscalendar.Parser }

func (p fuzzParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	err := p.Parser.Parse(ctx, in, out)
	if errors.Is(err, ioscalendar.ErrNoCalendarItemTable) || errors.Is(err, ioscalendar.ErrSchemaIncomplete) {
		return nil
	}
	return err
}

// fuzzIOS runs the contract check at the system path of an extraction (the logical path a backup shares, B4), so the
// fuzzed bytes reach every branch of Probe and Parse that the system path takes.
func fuzzIOS(t *testing.T, db, wal, journal []byte) {
	t.Helper()
	parsertest.FuzzParseIOS(t, fuzzParser{}, extractRemote, systemLogical, db, wal, journal)
}

// maxSeedBytes is the largest file a fuzz seed may hold (T59): a big seed slows every execution, so the fuzzer
// explores nothing.
const maxSeedBytes = 64 << 10

type fuzzSeed struct {
	name             string
	db, wal, journal []byte
}

// minimalSeed is a minimal store of the snapshot's tables: one event that every join reaches (its location, calendar
// and store, an organizer with an identity who is also an attendee, a recurrence rule, an exception date and an
// alarm).
func minimalSeed() []byte {
	s := newBuilt()
	s.add("Location", 1, map[string]any{"title": "Office", "address": "1 Main St", "latitude": 52.52, "longitude": 13.405})
	s.add("Store", 1, map[string]any{"name": "iCloud", "type": int64(2)})
	s.add("Calendar", 1, map[string]any{"title": "Home", "store_id": int64(1)})
	s.add("Identity", 1, map[string]any{"display_name": "Ann", "address": "mailto:ann@example.test"})
	s.add("Participant", 1, map[string]any{"item_id": int64(1), "identity_id": int64(1), "role": int64(2), "type": int64(1), "status": int64(1)})
	s.add("Recurrence", 1, map[string]any{"owner_id": int64(1), "frequency": int64(2), "interval": int64(1)})
	s.add("ExceptionDate", 1, map[string]any{"owner_id": int64(1), "date": float64(cD + 7*cDay)})
	s.add("Alarm", 1, map[string]any{"calendaritem_owner_id": int64(1), "trigger_interval": int64(-900)})
	s.event(1, map[string]any{"summary": "Seed", "location_id": int64(1), "organizer_id": int64(1), "url": "https://example.test/s"})
	return s.bytes()
}

// firstPage is calendar-ios's first page: its schema, with every table root beyond the file.
func firstPage(t testing.TB) []byte {
	db := parsertest.LoadFixture(t, "testdata", "calendar-ios").Files["db"]
	ps := int(db[16])<<8 | int(db[17])
	if ps == 1 {
		ps = 1 << 16
	}
	return append([]byte(nil), db[:ps]...)
}

// fuzzSeeds are the hand-built minimal store and calendar-ios's first page (the committed fixtures are larger than the
// limit).
func fuzzSeeds(t testing.TB) []fuzzSeed {
	return []fuzzSeed{{name: "minimal store", db: minimalSeed()}, {name: "calendar-ios page 1", db: firstPage(t)}}
}

func TestFuzzSeedsAreSmall(t *testing.T) {
	for _, s := range fuzzSeeds(t) {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
	// the minimal seed parses to its one record with every join made, so the fuzzer starts from a store that reaches
	// every join of the mapping
	res := parseAt(t, map[string][]byte{"db": minimalSeed()}, true)
	if res.Err != nil || len(res.Records) != 1 || len(res.Violations) != 0 {
		t.Fatalf("minimal seed: err %v, %d records, violations %v", res.Err, len(res.Records), res.Violations)
	}
	r := res.Records[0]
	for _, k := range []string{"location", "calendar", "organizer", "attendees", "recurrence"} {
		if r.Payload[k] == nil {
			t.Errorf("minimal seed: no %s: the seed does not reach that join", k)
		}
	}
	for _, k := range []string{"exception_dates", "alarms"} {
		if rawOf(r)[k] == nil {
			t.Errorf("minimal seed: no raw.%s: the seed does not reach that join", k)
		}
	}
}

// FuzzParseIOSCalendar mutates whole stores (and their WAL and journal) at the system path and requires the contract
// to hold: no panic, no refused record, and only typed errors. Seeds run under go test; go test -fuzz is run manually.
func FuzzParseIOSCalendar(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		fuzzIOS(t, db, wal, journal)
	})
}

// sameSizeClass maps a one-byte serial type to a different storage class of the same body size (T67, T71), so the
// record stays well formed and the wrong-class cell reaches the mapper. Type 0 and the multi-byte serial types are
// left alone. Restated from android.calendar (parsers share no test code).
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

// Page-level hostile mutations, deterministic: zeroed pages, a flipped page type, an absurd cell count, a wild
// first-free block and cell offset, and every cell's serial types rewritten to another storage class, in every page of
// the minimal store (every join table holds a row, so each mutation reaches every join); and the same six in every
// page of calendar-ios-deleted (512-byte pages with freelist history, the full set within the 60 s of
// docs/testing.md because the store is small).
func TestPageLevelMutationsNeverBreakTheContract(t *testing.T) {
	muts := map[string]func(p []byte, base int){
		"zeroed":      func(p []byte, _ int) { clear(p) },
		"type flip":   func(p []byte, _ int) { p[0] ^= 0x08 },
		"cell count":  func(p []byte, _ int) { p[3], p[4] = 0xff, 0xff },
		"first free":  func(p []byte, _ int) { p[1], p[2] = 0xff, 0xff },
		"cell offset": func(p []byte, _ int) { p[8], p[9] = 0xff, 0xf0 },
		"serial type": rewriteSerialTypes,
	}
	deleted := parsertest.LoadFixture(t, "testdata", "calendar-ios-deleted").Files
	for _, in := range []struct {
		name    string
		db, wal []byte
		only    string // "": every mutation
	}{{"minimal store", minimalSeed(), nil, ""}, {"calendar-ios-deleted", deleted["db"], deleted["wal"], ""}} {
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
					fuzzIOS(t, m, in.wal, nil)
				})
			}
		}
	}
}
