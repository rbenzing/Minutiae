package iossms_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/iossms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// recoverVariants are the fixtures that hold deleted messages, with a recover oracle.
var recoverVariants = []string{"ios17-deleted", "ios17-wal"}

// recoverBundle places a fixture as the backup of UDID1 the typed pass reads (bundleOf; ios17-nanos carries extra files).
func recoverBundle(t *testing.T, h *parsertest.Harness, variant string) parsertest.Bundle {
	t.Helper()
	return bundleOf(h, parsertest.LoadFixture(t, "testdata", variant))
}

// Spec section 14: the claimed table maps its recovered rows, checked against an oracle that was computed by an
// independent page walker from the committed files and the scenario's own record of what it wrote. A variant where
// nothing was deleted has no oracle file: it is checked against the empty set, which is a real claim (a typed record
// there would be invented, check b).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			want := parsertest.RecoverExpect{Variant: v, Claimed: iossms.ClaimableTables()}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, iossms.Parser{}, recoverBundle(t, h, v), want)
		})
	}
}

// 4G-R30 / 4F-R3 / 4h-R6: the claims are exactly the claimable (total) tables, all of them tables of one mapping.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := iossms.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := iossms.ClaimableTables(); !slices.Equal(got, want) || len(got) == 0 {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v", got, want)
	}
	if want := []string{"message"}; !slices.Equal(iossms.ClaimableTables(), want) {
		t.Errorf("ClaimableTables() = %v, want %v (4h-R6)", iossms.ClaimableTables(), want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
}

// The oracle is not vacuous: it lists what the scenario was built to leave behind.
func TestRecoverOracleShape(t *testing.T) {
	x := parsertest.LoadRecoverExpect(t, "testdata", "ios17-deleted")
	typed := map[int64]bool{}
	for _, r := range x.Records {
		if r.Table != "message" || r.Rowid == nil || r.Method != "sqlite-freelist" || r.Basis != "fit" || r.Relation != "unknown" ||
			r.Confidence != 60 || !slices.Equal(r.Notes, []string{"identity-by-fit-only"}) || r.Record.Type != "message" {
			t.Fatalf("ios17-deleted lists %+v, want a deleted message from the freelist, basis fit, 60", r)
		}
		id := *r.Rowid
		typed[id] = true
		// a recovered message joins the LIVE tables (4G-R8): the chat joins were deleted with it, handle 3 is gone
		raw, _ := r.Record.Payload["raw"].(map[string]any)
		if raw["thread_state"] != "no-chat-join" {
			t.Errorf("message %d: thread_state %v, want no-chat-join (its join row was deleted)", id, raw["thread_state"])
		}
		unknown := r.Record.Payload["participants_unknown"] == true
		if id >= 41 {
			if !unknown || raw["handle_state"] != "no-handle-row" {
				t.Errorf("message %d (from the deleted handle 3): participants_unknown %v, handle_state %v, want true and no-handle-row", id, unknown, raw["handle_state"])
			}
		} else if unknown || raw["handle_state"] != nil {
			t.Errorf("message %d (from a live handle): participants_unknown %v, handle_state %v, want a participant", id, unknown, raw["handle_state"])
		}
	}
	for id := int64(21); id <= 45; id++ {
		if !typed[id] {
			t.Errorf("deleted message %d is not listed as a typed record", id)
		}
	}
	if len(typed) != 25 || len(x.Records) != 25 {
		t.Errorf("%d typed messages (%d records), want exactly 25 (21 to 45)", len(typed), len(x.Records))
	}
	if len(x.Unmapped) != 0 {
		t.Errorf("unmapped %+v: every deleted message identifies itself by its guid and is typed", x.Unmapped)
	}
	if len(x.Digests) != len(x.Records) {
		t.Errorf("%d digests for %d records: every want has a digest", len(x.Digests), len(x.Records))
	}
}

// The edit that only the -wal holds leaves the original of message 5 behind in the database file and in earlier WAL frames:
// each is a recovered SUPERSEDED version (basis schema, confidence 75, never a deleted message), all of one text.
func TestRecoverOracleShapeOfTheEditedWALVariant(t *testing.T) {
	x := parsertest.LoadRecoverExpect(t, "testdata", "ios17-wal")
	if len(x.Records) != 4 || len(x.Unmapped) != 0 {
		t.Fatalf("%d records and %d unmapped, want the 4 stored versions of message 5 and none unmapped", len(x.Records), len(x.Unmapped))
	}
	files := map[string]int{}
	for _, r := range x.Records {
		if r.Table != "message" || r.Rowid == nil || *r.Rowid != 5 || r.Method != "sqlite-wal-prior" || r.Basis != "schema" ||
			r.Relation != "superseded-version" || r.Confidence != 75 || r.Record.Body == nil || *r.Record.Body != "to be edited" {
			t.Errorf("ios17-wal lists %+v, want a superseded version of message 5, schema, 75, text %q", r, "to be edited")
		}
		files[r.File]++
	}
	if files["db"] != 1 || files["wal"] != 3 {
		t.Errorf("versions by file %v, want 1 in the database and 3 in the wal frames", files)
	}
}

func TestEveryRecoverOracleHasAFixture(t *testing.T) {
	for _, v := range recoverVariants {
		if !slices.Contains(variants, v) {
			t.Errorf("%s has a recover oracle but is not a fixture variant", v)
		}
		parsertest.LoadRecoverExpect(t, "testdata", v) // also checks it was computed from the committed files
	}
}

func recoveredMessage(t *testing.T, h *parsertest.Harness, b parsertest.Bundle, id int64) sqlitefile.RecoveredRow {
	t.Helper()
	rr, ok := parsertest.FirstClaimedRow(t, h, iossms.Parser{}, b, "message", func(r sqlitefile.RecoveredRow) bool {
		return r.Rowid != nil && *r.Rowid == id
	})
	if !ok {
		t.Fatalf("the fixture delivers no message %d to the typed pass", id)
	}
	return rr
}

func rawMap(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

// plain is the payload of r as JSON decoded generically, so a test reads it the way the oracle states it.
func plain(t *testing.T, r records.Record) map[string]any {
	t.Helper()
	b, err := json.Marshal(r.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Review Focus 3 / 4h-R6: a recovered message whose handle row is gone states participants_unknown and names no address
// and no invented handle id; one whose handle stays has its address.
func TestRecoveredMessageWithoutLiveHandleIsParticipantsUnknown(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := recoverBundle(t, h, "ios17-deleted")

	gone := parsertest.MapRecoveredRow(t, h, iossms.Parser{}, b, recoveredMessage(t, h, b, 41))
	if gone.Err != nil || gone.Unmapped != "" || len(gone.Records) != 1 {
		t.Fatalf("message 41: %+v, want one record", gone)
	}
	p := plain(t, gone.Records[0])
	if p["participants_unknown"] != true {
		t.Errorf("message 41 (handle 3 is deleted): payload %v, want participants_unknown", p)
	}
	if parts, _ := p["participants"].([]any); len(parts) != 0 {
		t.Errorf("message 41 names participants %v although its handle row is gone", parts)
	}
	if rawMap(gone.Records[0])["handle_state"] != "no-handle-row" {
		t.Errorf("message 41: handle_state %v, want no-handle-row", rawMap(gone.Records[0])["handle_state"])
	}
	if strings.Contains(gone.Records[0].Summary, "alice") || strings.Contains(gone.Records[0].Summary, "+1555") {
		t.Errorf("message 41 summary %q names an address that was never read", gone.Records[0].Summary)
	}

	live := parsertest.MapRecoveredRow(t, h, iossms.Parser{}, b, recoveredMessage(t, h, b, 21))
	if live.Err != nil || live.Unmapped != "" || len(live.Records) != 1 {
		t.Fatalf("message 21: %+v, want one record", live)
	}
	q := plain(t, live.Records[0])
	parts, _ := q["participants"].([]any)
	if len(parts) != 1 || q["participants_unknown"] == true {
		t.Fatalf("message 21 (handle 1 stays): participants %v, participants_unknown %v, want its one participant", parts, q["participants_unknown"])
	}
	if first, _ := parts[0].(map[string]any); first["address"] != "+15551230001" || first["role"] != "from" {
		t.Errorf("message 21 participant %v, want +15551230001 from", parts[0])
	}
}

// 4h-R6 (PASSES ON ARRIVAL: the live mapper already states an undecidable join; the test pins that a RECOVERED row
// does the same). A recovered message whose message_attachment_join answer the live database cannot decide lists the
// table in raw.join_undecided and gives neither attachments nor the kind media: unknown is never "no attachment".
// Mutation (done): drop the markUndecided call of attachments() and this test fails on join_undecided. (Returning an empty
// list instead is not observable: the payload omits an empty attachments field, so the raw marker is the only statement.)
func TestRecoveredMessageAttachmentJoinIsNeverEmptyWhenUndecided(t *testing.T) {
	hRow := parsertest.NewHarness(t)
	rr := recoveredMessage(t, hRow, recoverBundle(t, hRow, "ios17-deleted"), 21)
	h := parsertest.NewHarness(t)

	const rows = 800
	d := newSMS(t)
	for i := int64(1); i <= rows; i++ {
		d.msg(i, map[string]any{"text": nil})
		d.attach(i, i, "~/"+att+"x")
	}
	leaves := d.tbl["message_attachment_join"].Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	data := d.b.Bytes()
	ps := d.b.PageSize()
	page := data[int(leaves[1]-1)*ps:][:ps]
	page[3], page[4] = 0, 0 // the middle leaf loses its cells: every answer of the join is short
	live := parsertest.Fixture{Name: "mem", Files: map[string][]byte{"db": data}}.BundleIOS(h, dbRemote, dbLogical)

	res := parsertest.MapRecoveredRow(t, h, iossms.Parser{}, live, rr)
	if res.Err != nil || res.Unmapped != "" || len(res.Records) != 1 {
		t.Fatalf("%+v, want one record", res)
	}
	r := res.Records[0]
	if !slices.Equal(anyStrs(rawMap(r)["join_undecided"]), []string{"message_attachment_join"}) {
		t.Errorf("join_undecided = %v, want [message_attachment_join]", rawMap(r)["join_undecided"])
	}
	if l, ok := r.Payload["attachments"].([]any); ok && len(l) == 0 {
		t.Errorf("an empty attachments list from an undecided join")
	}
	if r.Payload["kind"] == "media" {
		t.Errorf("kind media from an undecided join")
	}
}

func TestAssertClaimedTablesMapRecoveredRowsCatchesAWrongMapper(t *testing.T) {
	odd := func(row parse.Row) bool { id, ok := row.Rowid(); return ok && id%2 == 1 }
	for _, tc := range []struct {
		fault, want string
		f           func(parse.Row, []records.Record) []records.Record
	}{
		{"drops-body", "Body", func(_ parse.Row, r []records.Record) []records.Record { r[0].Body = ""; return r }},
		{"adds-record", "typed records for the cell", func(_ parse.Row, r []records.Record) []records.Record { return append(r, r[0]) }},
		{"sets-recovery", "only the host stamps", func(_ parse.Row, r []records.Record) []records.Record { r[0].Recovery = "sqlite-freelist"; return r }},
		// a fault that corrupts only some records (odd rowids) must be reported too, whatever the oracle lists in full
		{"corrupts-odd-body", "Body", func(row parse.Row, r []records.Record) []records.Record {
			if odd(row) {
				r[0].Body += " (corrupted)"
			}
			return r
		}},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			rec := &parsertest.FailureRecorder{TB: t}
			parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: iossms.Parser{}, Fault: tc.f}, recoverBundle(t, h, "ios17-deleted"),
				parsertest.LoadRecoverExpect(t, "testdata", "ios17-deleted"))
			if got := strings.Join(rec.Errs, "\n"); len(rec.Errs) == 0 || !strings.Contains(got, tc.want) {
				t.Fatalf("the kit did not report the %s fault (wanted %q): %v", tc.fault, tc.want, rec.Errs)
			}
		})
	}
	// the unfaulted mapper passes the same oracle through the same double
	h := parsertest.NewHarness(t)
	rec := &parsertest.FailureRecorder{TB: t}
	parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: iossms.Parser{}}, recoverBundle(t, h, "ios17-deleted"),
		parsertest.LoadRecoverExpect(t, "testdata", "ios17-deleted"))
	if len(rec.Errs) != 0 {
		t.Fatalf("the correct mapper was reported: %v", rec.Errs)
	}
}
