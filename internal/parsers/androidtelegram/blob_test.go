package androidtelegram_test

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// fixtureBlobs reads column col of every row of table from a committed variant's main file, by rowid.
func fixtureBlobs(t *testing.T, variant, table, col string) map[int64][]byte {
	t.Helper()
	data := fixtureFiles(t, variant)["db"]
	db, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(data), DBSize: int64(len(data))}, parse.NewBudget(1<<30).View())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	tb, err := db.Table(t.Context(), table, []string{col}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64][]byte{}
	i := tb.Col(col)
	err = tb.Scan(t.Context(), func(row sqlitedb.Row) error {
		id, _ := row.Rowid()
		if b, ok := row.Blob(i); ok {
			out[id] = bytes.Clone(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// dataKeys are the data_* keys of a raw map.
func dataKeys(raw map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range raw {
		if strings.HasPrefix(k, "data_") {
			out[k] = v
		}
	}
	return out
}

// Spec 8.4 (call 4), 4I4-P10, ruling T7: decodeBlob over the telegram-hostile rows and Go-built blobs gives each state
// of the vocabulary with its stop keys, exactly as the oracle states them; length, SHA-256 and constructor stand
// beside every non-NULL blob, the encoding flags on their rows. A string length past the end is truncated, never cap
// (T7 withdrew the "string -> cap" row; only the vector count is cap).
func TestTelegramBlobStatesArePinned(t *testing.T) {
	bud := func() *parse.BudgetView { return parse.NewBudget(1 << 30).View() }
	wantState := map[int64]string{
		2001: "unknown-constructor", 2002: "truncated", 2003: "truncated", 2004: "truncated", 2005: "truncated",
		2006: "truncated", 2007: "truncated", 2008: "truncated", 2009: "truncated", 2010: "truncated",
		2011: "malformed", 2012: "malformed", 2013: "cap", 2014: "truncated", 2015: "decoded", 2016: "decoded",
	}
	// pendingOracle: until plan 4I4c Task 6 maps reply_markup, the oracle's data_undecided names it and Go's does not
	exp := pendingOracle(parsertest.LoadFixture(t, "testdata", "telegram-hostile").Expect)
	oracle := map[int64]map[string]any{}
	for _, r := range exp.Records {
		raw, _ := r.Payload["raw"].(map[string]any)
		oracle[r.Rowid] = dataKeys(raw)
	}
	seen := map[string]bool{}
	blobs := fixtureBlobs(t, "telegram-hostile", "messages_v2", "data")
	for id, st := range wantState {
		b, ok := blobs[id]
		if !ok {
			t.Fatalf("telegram-hostile rowid %d has no blob", id)
		}
		raw, got := androidtelegram.DecodeBlob(b, androidtelegram.TypeMessage, bud())
		seen[got] = true
		if got != st || raw["data_state"] != st {
			t.Errorf("rowid %d: state %q (raw %v), want %q", id, got, raw["data_state"], st)
		}
		if !equalJSON(dataKeys(raw), oracle[id]) {
			t.Errorf("rowid %d: keys %v, the oracle states %v", id, dataKeys(raw), oracle[id])
		}
		if raw["data_len"] != int64(len(b)) || raw["data_sha256"] == nil || (len(b) >= 4) != (raw["data_constructor"] != nil) {
			t.Errorf("rowid %d: length, SHA-256 or constructor missing: %v", id, raw)
		}
		if st != "decoded" && raw["data_stop_kind"] == nil {
			t.Errorf("rowid %d: a stop without data_stop_kind: %v", id, raw)
		}
		if strings.HasPrefix(st, "decoded") && raw["data_stop_kind"] != nil {
			t.Errorf("rowid %d: a decoded blob with stop keys: %v", id, raw)
		}
	}
	// the cut rows name the field the cut fell in; the encoding flags stand on their rows (H6, H7)
	for id, path := range map[int64]string{2003: "flags", 2004: "flags2", 2005: "id", 2006: "from_id", 2008: "date", 2009: "message", 2010: "entities"} {
		raw, _ := androidtelegram.DecodeBlob(blobs[id], androidtelegram.TypeMessage, bud())
		if raw["data_stop_path"] != path {
			t.Errorf("rowid %d: data_stop_path %v, want %q", id, raw["data_stop_path"], path)
		}
	}
	if raw, _ := androidtelegram.DecodeBlob(blobs[2015], androidtelegram.TypeMessage, bud()); !strings.Contains(fmt.Sprint(raw["data_flags"]), "length-prefix-255") {
		t.Errorf("rowid 2015: data_flags %v, want length-prefix-255", raw["data_flags"])
	}
	if raw, _ := androidtelegram.DecodeBlob(blobs[2016], androidtelegram.TypeMessage, bud()); raw["data_flags"] != "padding-nonzero" {
		t.Errorf("rowid 2016: data_flags %v, want padding-nonzero", raw["data_flags"])
	}

	// partial: R18, a conference-call action (spec 8.7, Q7) the table holds as an opaque row since plan 4I4b (rulings
	// R2, B2: every MessageAction ID of A-HEAD is a row, opaque unless read), so the stop is opaque and names it
	v181 := fixtureBlobs(t, "telegram-v181", "messages_v2", "data")
	raw, st := androidtelegram.DecodeBlob(v181[18], androidtelegram.TypeMessage, bud())
	seen[st] = true
	want := map[string]any{"data_state": "partial", "data_stop_kind": "opaque", "data_stop_path": "action", "data_stop_id": "0x2ffe2f7a", "data_stop_offset": int64(40)}
	for k, v := range want {
		if raw[k] != v {
			t.Errorf("R18: %s = %v, want %v", k, raw[k], v)
		}
	}
	// decoded-prefix: a users.data blob (a prefix body read to its last pinned field; no stop keys)
	users := fixtureBlobs(t, "telegram-v181", "users", "data")
	raw, st = androidtelegram.DecodeBlob(users[1000001], androidtelegram.TypeUser, bud())
	seen[st] = true
	if st != "decoded-prefix" || raw["data_stop_kind"] != nil {
		t.Errorf("users 1000001: state %q, raw %v", st, raw)
	}
	// budget: a budget that refuses every allocation is a fact about this run, not about the data
	raw, st = androidtelegram.DecodeBlob(v181[1], androidtelegram.TypeMessage, parse.NewBudget(0).View())
	seen[st] = true
	if st != "budget" || raw["data_stop_kind"] != "budget" || raw["data_len"] == nil {
		t.Errorf("R1 under a refusing budget: state %q, raw %v", st, raw)
	}
	// null: R50, a NULL cell (the mapping states it: decodeBlob is only given a blob)
	r50 := byRow(parseFixture(t, "telegram-v181").Records)["messages_v2/50"]
	if raw := rawOf(r50); raw["data_state"] != "null" || raw["data_len"] != nil {
		t.Errorf("R50: raw %v, want data_state null and no length", raw)
	} else {
		seen["null"] = true
	}
	for _, s := range androidtelegram.BlobStates() {
		if !seen[s] {
			t.Errorf("state %q never produced", s)
		}
	}
}

// F8 (spec ruling T3 I1): every decode's charge is freed when its row's mapping returns, so the budget states a
// refused single decode, never an accumulation over rows. 10,000 rows under a job budget of 4 MiB: their decodes
// together charge far more than that (about 1 KiB each: 9 fields at 128 bytes and their paths), one alone a little.
func TestTelegramBlobBudgetIsFreedPerRow(t *testing.T) {
	const rows = 10000
	cells := make([]msgCells, rows)
	for i := range cells {
		m := tlMessage{id: int32(i + 1), from: 1000002, peer: 1000001, date: baseDate, text: "budget row"}
		cells[i] = msgCells{mid: int64(i + 1), uid: 1000002, data: m.bytes()}
	}
	db := goBuiltMessages(181, nil, cells)
	h := parsertest.NewHarness(t)
	p := androidtelegram.Parser{}
	in := inputAt(t, h, p, remoteApp, dbOnly(db), false)
	in.Budget = parse.NewBudget(4 << 20).View()
	res := h.RunLenient(p, in)
	if res.Err != nil {
		t.Fatalf("Parse err %v", res.Err)
	}
	if len(res.Records) != rows {
		t.Fatalf("%d records, want %d", len(res.Records), rows)
	}
	budget := 0
	for _, r := range res.Records {
		if rawOf(r)["data_state"] == "budget" {
			budget++
		}
	}
	if budget != 0 {
		t.Errorf("%d records state budget: the decodes' charge outlived their rows", budget)
	}
	if got := res.Notes["blob_states"]; got != "messages_v2.data=decoded:10000" {
		t.Errorf("note blob_states = %q", got)
	}
}

// equalJSON compares two values as their canonical JSON (int64 and json.Number alike).
func equalJSON(a, b map[string]any) bool {
	ka, kb := slices.Sorted(mapsKeys(a)), slices.Sorted(mapsKeys(b))
	if !slices.Equal(ka, kb) {
		return false
	}
	for _, k := range ka {
		if jsonText(a[k]) != jsonText(b[k]) {
			return false
		}
	}
	return true
}

// The IDs of the reaction rows the pin-conflict rows use (constructors.tsv).
const (
	idMessageReactions = 0x0a339f0b // TL_messageReactions (TLRPC.java:27428@A-HEAD)
	idReactionCountOld = 0x6fb250d1 // TL_reactionCount_layer144: read two ways across the pins, opaque (R3)
	idReactionEmoji    = 0x1b2286b8 // TL_reactionEmoji
	idTLVector         = 0x1cb5c415
)

// reactionsWith is a TL_messageReactions (flags 0) whose results vector holds one element, given encoded, ID first.
func reactionsWith(elem []byte) []byte {
	b := append(le32(idMessageReactions), le32(0)...)
	b = append(b, le32(idTLVector)...)
	b = append(b, le32(1)...)
	return append(b, elem...)
}

// Plan 4I4b design D4, rulings R3, T1b: 0x6fb250d1 is read as (flags, string, count) at 5.14.0 and 8.1.1 and as
// (flags, boxed Reaction, count) at A-HEAD, so its row is opaque and a blob holding it stops there under either
// reading, naming the constructor: both rows are partial at reactions.results[0] with the stop id, never decoded one
// way.
func TestTelegramPinConflictIsOpaque(t *testing.T) {
	c := androidtelegram.TableEntry("ReactionCount", idReactionCountOld)
	if c.Body != tl.BodyOpaque || c.Type != "ReactionCount" {
		t.Errorf("table entry 0x6fb250d1: body %v type %q, want opaque ReactionCount", c.Body, c.Type)
	}
	old := append(append(append(le32(idReactionCountOld), le32(0)...), tlStr("👍")...), le32(2)...)
	boxed := append(append(append(append(le32(idReactionCountOld), le32(0)...), le32(idReactionEmoji)...), tlStr("👍")...), le32(2)...)
	for name, elem := range map[string][]byte{"old reading": old, "new reading": boxed} {
		b := tlMessage{flags: bitReactions, id: 1, from: 1000002, peer: 1000001, date: baseDate, text: "x", tail: reactionsWith(elem)}.bytes()
		raw, state := androidtelegram.DecodeBlob(b, androidtelegram.TypeMessage, parse.NewBudget(1<<20).View())
		want := map[string]any{
			"data_state": "partial", "data_stop_kind": "opaque", "data_stop_path": "reactions.results[0]",
			"data_stop_id": "0x6fb250d1",
		}
		if state != "partial" {
			t.Errorf("%s: state %q, want partial", name, state)
		}
		for k, v := range want {
			if raw[k] != v {
				t.Errorf("%s: raw.%s = %v, want %v", name, k, raw[k], v)
			}
		}
	}
}
