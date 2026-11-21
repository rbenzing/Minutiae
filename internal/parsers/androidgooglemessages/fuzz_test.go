package androidgooglemessages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// fuzzParser is the parser with its own typed outcomes accepted: a database at the app path without the signature and a
// schema read in part are the documented answers to hostile input (spec 5.4, 9), not contract breaks. Every other error
// is still refused by parsertest.FuzzParse unless it is one of the decoder's typed hostile-input errors.
type fuzzParser struct{ androidgooglemessages.Parser }

func (p fuzzParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	err := p.Parser.Parse(ctx, in, out)
	for _, typed := range []error{androidgooglemessages.ErrNoSignature, androidgooglemessages.ErrSchemaIncomplete} {
		if errors.Is(err, typed) {
			return nil
		}
	}
	return err
}

// maxSeedBytes is the largest file a fuzz seed may hold: a big seed slows every execution, so the fuzzer explores nothing.
const maxSeedBytes = 64 << 10

type fuzzSeed struct {
	name             string
	db, wal, journal []byte
}

// minimalSeed is a minimal v2 database: a self row, two participants, a group conversation with its members, one incoming
// and one outgoing message and their parts (Go-built, one page per table).
func minimalSeed(t testing.TB) []byte {
	return store{
		participants:  []mrow{selfRow(1, 1), person(2, "+15550000002"), person(3, "+15550000003")},
		conversations: []mrow{{1, map[string]any{"name": "Seed"}}},
		members:       memberRows(1, 1, 2, 3),
		messages:      []mrow{{1, map[string]any{"sender_id": int64(2)}}, {2, map[string]any{"sender_id": int64(1), "message_status": int64(1)}}},
		parts: []mrow{
			{1, map[string]any{"message_id": int64(1), "text": "seed"}},
			{2, map[string]any{"message_id": int64(2), "uri": "content://mms/part/2", "content_type": "image/png"}},
		},
	}.builder(t).Bytes()
}

// firstPage is gm-aosp-v2's first page: its schema, with every table root beyond the file.
func firstPage(t testing.TB) []byte {
	db := parsertest.LoadFixture(t, "testdata", "gm-aosp-v2").Files["db"]
	ps := int(db[16])<<8 | int(db[17])
	return append([]byte(nil), db[:ps]...)
}

func fuzzSeeds(t testing.TB) []fuzzSeed {
	return []fuzzSeed{{name: "minimal-v2", db: minimalSeed(t)}, {name: "gm-aosp-v2 page 1", db: firstPage(t)}}
}

func TestFuzzSeedsAreSmall(t *testing.T) {
	for _, s := range fuzzSeeds(t) {
		for role, data := range map[string][]byte{"db": s.db, "wal": s.wal, "journal": s.journal} {
			if len(data) > maxSeedBytes {
				t.Errorf("seed %s %s is %d bytes, over %d", s.name, role, len(data), maxSeedBytes)
			}
		}
	}
	// the minimal seed parses to its two records, so the fuzzer starts from a database that reaches the joins
	res := parseAt(t, remoteApp, dbOnly(minimalSeed(t)))
	if res.Err != nil || len(res.Records) != 2 || len(res.Violations) != 0 {
		t.Fatalf("minimal seed: err %v, %d records, violations %v", res.Err, len(res.Records), res.Violations)
	}
	if got := byRowid(t, res); len(threadMembers(got[1])) != 2 || got[1].Body != "seed" {
		t.Errorf("minimal seed: members %v body %q", threadMembers(got[1]), got[1].Body)
	}
}

// FuzzParseAndroidGoogleMessages mutates whole databases (and their WAL and journal) at the app path and requires the
// contract to hold: no panic, no refused record, and only typed errors (spec 9, 12). Seeds run under go test; go test
// -fuzz is run manually (docs/testing.md).
func FuzzParseAndroidGoogleMessages(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		parsertest.FuzzParse(t, fuzzParser{}, remoteApp, db, wal, journal)
	})
}
