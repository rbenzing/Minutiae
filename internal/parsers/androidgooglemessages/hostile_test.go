package androidgooglemessages_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// hostileRowids are the messages of gm-hostile (H1 to H15, H11 being two messages).
func hostileRowids() []int64 {
	var ids []int64
	for id := int64(101); id <= 116; id++ {
		ids = append(ids, id)
	}
	return ids
}

// Spec 10 hostile, plan 4I5 Task 7: every hostile row is exactly one record; a value the store cannot state is stated unknown or
// undecided with the raw class, never repaired or invented (H1 to H6, H13 to H15).
func TestGoogleMessagesHostileInputIsBounded(t *testing.T) {
	res, recs := fixtureRecords(t, "gm-hostile")
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	n := 0
	for _, r := range res.Records {
		if strings.HasPrefix(r.Locator, "sqlite:table=messages;") {
			n++
		}
	}
	if n != 16 || len(recs) != 16 {
		t.Fatalf("%d messages records (%d distinct rowids), want exactly one per hostile row (16)", n, len(recs))
	}
	for _, id := range hostileRowids() {
		if _, ok := recs[id]; !ok {
			t.Errorf("hostile message %d has no record", id)
		}
	}
	for _, id := range []int64{101, 103} { // H1 a TEXT status, H3 a NaN status
		r := recs[id]
		if unknownOf(r)["message_status"] != "type-mismatch" || str(r, "direction") != "unknown" || str(r, "status") != "unknown" {
			t.Errorf("H%d (message %d): unknown %v direction %q status %q", id-100, id, unknownOf(r), str(r, "direction"), str(r, "status"))
		}
	}
	for _, id := range []int64{102, 104} { // H2 a REAL protocol, H4 a BLOB protocol
		r := recs[id]
		if unknownOf(r)["message_protocol"] != "type-mismatch" || str(r, "channel") != "unknown" {
			t.Errorf("message %d: unknown %v channel %q", id, unknownOf(r), str(r, "channel"))
		}
	}
	if r := recs[105]; rawOf(r)["time_basis"] != "undecided" || r.Time != nil { // H5 a TEXT received timestamp
		t.Errorf("H5: time_basis %v time %v", rawOf(r)["time_basis"], r.Time)
	}
	if r := recs[106]; unknownOf(r)["sender_id"] != "type-mismatch" || r.Payload["participants_unknown"] != true { // H6 a TEXT sender
		t.Errorf("H6: unknown %v participants_unknown %v", unknownOf(r), r.Payload["participants_unknown"])
	}
	for _, p := range rolesOf(recs[114]) { // H13 a self sender with a destination: no from
		if strings.HasPrefix(p, "from ") {
			t.Errorf("H13: a from party %q for a self sender", p)
		}
	}
	if r := recs[115]; unknownOf(r)["read"] != "type-mismatch" || has(r, "read") { // H14
		t.Errorf("H14: unknown %v raw %v", unknownOf(r), rawOf(r))
	}
	if r := recs[116]; unknownOf(r)["sms_message_uri"] != "type-mismatch" || has(r, "sms_message_uri") { // H15
		t.Errorf("H15: unknown %v raw %v", unknownOf(r), rawOf(r))
	}
}

// Spec 9, 6.4: NUL, invalid UTF-8 and a right-to-left override in a Body (H9), a member name and a thread title are
// cleaned and flagged, the original bytes are kept beside a Body, and a Summary shows the override as <U+202E>, never
// as the character.
func TestGoogleMessagesHostileTexts(t *testing.T) {
	_, recs := fixtureRecords(t, "gm-hostile")
	h9 := recs[109]
	if h9.Body != "nul\uFFFD bad \uFFFD rtl \u202e end" {
		t.Errorf("H9 Body %q", h9.Body)
	}
	if strings.ContainsRune(h9.Body, 0) {
		t.Error("H9 Body holds a NUL")
	}
	flags, _ := h9.Payload["body_flags"].([]any)
	if len(flags) == 0 || h9.Payload["body_raw_b64"] == nil || h9.Payload["body_raw_len"] == nil {
		t.Errorf("H9: body_flags %v, original bytes %v/%v", flags, h9.Payload["body_raw_b64"], h9.Payload["body_raw_len"])
	}
	if !strings.Contains(h9.Summary, "<U+202E>") || strings.ContainsRune(h9.Summary, '\u202e') {
		t.Errorf("H9 Summary %q does not escape the override", h9.Summary)
	}

	// a member name and a thread title with NUL, an invalid byte and the override
	const bad = "n\x00a\xffb\xe2\x80\xaeC"
	_, got := store{
		participants: []mrow{
			{1, map[string]any{"sub_id": int64(-2), "send_destination": "5550001", "full_name": bad, "contact_id": int64(3)}},
		},
		conversations: []mrow{{1, map[string]any{"name": bad}}},
		members:       memberRows(1, 1),
		messages:      []mrow{{1, map[string]any{"sender_id": int64(1)}}},
	}.parse(t)
	r := got[1]
	if name := partyAt(t, r, 0)["name"]; name != "n\uFFFDa\uFFFDb\u202eC" {
		t.Errorf("member name %q", name)
	}
	if title := threadMap(r)["title"]; title != "n\uFFFDa\uFFFDb\u202eC" {
		t.Errorf("thread title %q", title)
	}
	tf, _ := rawOf(r)["text_flags"].([]any)
	var names []string
	for _, f := range tf {
		names = append(names, f.(string))
	}
	if strings.Join(names, ",") != "from_full_name,thread.title" {
		t.Errorf("text_flags %v, want from_full_name and thread.title", names)
	}
	if strings.ContainsRune(r.Summary, 0) || strings.ContainsRune(r.Summary, '\u202e') {
		t.Errorf("Summary %q holds a raw control character", r.Summary)
	}
}

// Spec 9, R12: a database cut inside a table never reads as a complete, smaller one. Every cut at a page boundary of
// gm-aosp-v2 ends in a typed error, a stated warning or the whole record set; and one cut leaves records with warnings.
func TestGoogleMessagesTruncatedDatabaseIsStated(t *testing.T) {
	files := fixtureFiles(t, "gm-aosp-v2")
	db := files["db"]
	full := parseAt(t, remoteApp, files)
	if full.Err != nil || len(full.Records) == 0 {
		t.Fatalf("whole database: err %v, %d records", full.Err, len(full.Records))
	}
	ps := int(db[16])<<8 | int(db[17])
	if ps == 1 {
		ps = 65536
	}
	stated := 0
	for cut := ps; cut < len(db); cut += ps {
		res := parseAt(t, remoteApp, dbOnly(db[:cut]))
		switch {
		case res.Err != nil:
			// the cut itself is the error: the schema could not be read whole, or the file is corrupt (carry C2)
			if !errors.Is(res.Err, androidgooglemessages.ErrSchemaIncomplete) && !errors.Is(res.Err, sqlitedb.ErrCorrupt) {
				t.Errorf("cut at %d: error %v is neither ErrSchemaIncomplete nor ErrCorrupt", cut, res.Err)
			}
			stated++
		case len(res.Warnings) > 0:
			stated++
			if len(res.Records) == len(full.Records) {
				continue
			}
			// fewer records: a warning must state the cut (a page, a truncation, an incomplete read), not any warning
			if !slices.ContainsFunc(res.Warnings, func(w parsertest.Warning) bool {
				r := strings.ToLower(w.Reason)
				return strings.Contains(r, "page") || strings.Contains(r, "truncat") || strings.Contains(r, "incomplete")
			}) {
				t.Errorf("cut at %d: %d records (of %d) and no warning states the cut: %v", cut, len(res.Records), len(full.Records), res.Warnings)
			}
		default:
			if len(res.Records) != len(full.Records) {
				t.Errorf("cut at %d: %d records (of %d), no warning and no error: a cut database reads as a complete one", cut, len(res.Records), len(full.Records))
			}
		}
		if len(res.Violations) != 0 {
			t.Errorf("cut at %d: contract violations %v", cut, res.Violations)
		}
	}
	if stated == 0 {
		t.Error("no cut was stated: the test checks nothing")
	}
}

// Spec 5.5: a row whose newest version is only in the WAL names the WAL file in its range (M40); a row of an uncommitted
// frame is not a record (M41); the rows of the main file name the main file.
func TestGoogleMessagesWALRowsPointIntoTheWAL(t *testing.T) {
	res, recs := fixtureRecords(t, "gm-aosp-v2-wal")
	m40, ok := recs[40]
	if !ok {
		t.Fatal("M40 (a row only in the WAL) is not a record")
	}
	if _, ok := recs[41]; ok {
		t.Error("M41 (an uncommitted WAL frame) is a record")
	}
	if m40.SourcePath != remoteApp+"-wal" || m40.Range == nil {
		t.Errorf("M40: source %q range %v, want the WAL file %q", m40.SourcePath, m40.Range, remoteApp+"-wal")
	}
	if m1 := recs[1]; m1.SourcePath != remoteApp && m1.SourcePath != remoteApp+"-wal" {
		t.Errorf("M1 source %q", m1.SourcePath)
	}
	if len(warningsWith(res, "wal-frames-not-applied")) == 0 {
		t.Errorf("the uncommitted frames are not stated: %v", res.Warnings)
	}
	files := fixtureFiles(t, "gm-aosp-v2-wal")
	base := parseAt(t, remoteApp, dbOnly(files["db"]))
	if base.Err != nil {
		t.Fatal(base.Err)
	}
	if _, ok := byRowid(t, base)[40]; ok {
		t.Error("M40 is in the main file alone: the WAL test proves nothing")
	}
}

// Spec 5.5: a hot rollback journal is rolled back in the live view (M42, a row of the interrupted transaction, is not a
// record) and the journal is stated.
func TestGoogleMessagesHotJournalIsRolledBack(t *testing.T) {
	res, recs := fixtureRecords(t, "gm-journal")
	if _, ok := recs[42]; ok {
		t.Error("M42 (rolled back) is a record")
	}
	if len(recs) != 2 {
		t.Errorf("%d records, want 2", len(recs))
	}
	if len(warningsWith(res, "journal-hot")) == 0 {
		t.Errorf("the hot journal is not stated: %v", res.Warnings)
	}
}
