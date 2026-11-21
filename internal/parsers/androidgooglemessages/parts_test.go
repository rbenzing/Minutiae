package androidgooglemessages_test

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// partsByRowid indexes the parts records (the orphans) of a run by the rowid of their locator.
func partsByRowid(t testing.TB, res parsertest.Result) map[int64]records.Record {
	t.Helper()
	out := map[int64]records.Record{}
	for _, r := range res.Records {
		rest, ok := strings.CutPrefix(r.Locator, "sqlite:table=parts;rowid=")
		if !ok {
			continue
		}
		num, _, _ := strings.Cut(rest, ";")
		id, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			t.Fatalf("locator %q", r.Locator)
		}
		out[id] = r
	}
	return out
}

// partsOf is the number of parts rows of the files as the engine reads them and the message_id of each by rowid.
func partsOf(t *testing.T, files map[string][]byte) (count int, messageIDs map[int64]int64) {
	t.Helper()
	f := sqlitedb.Files{DB: bytes.NewReader(files["db"]), DBSize: int64(len(files["db"]))}
	db, err := sqlitedb.Open(t.Context(), f, parse.NewBudget(1<<30).View())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	tb, err := db.Table(t.Context(), "parts", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	col := tb.Col("message_id")
	messageIDs = map[int64]int64{}
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		id, _ := r.Rowid()
		v, _ := r.Int(col)
		messageIDs[id] = v
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count, messageIDs
}

// Spec 6.4, 4I5-P9: the Body is the text parts in part id order joined with a newline (not the insertion order), the source
// is the part, and the kind follows the parts.
func TestGoogleMessagesBodyFromPartsInIdOrder(t *testing.T) {
	_, core := fixtureRecords(t, "gm-aosp-v2")
	if m4 := core[4]; m4.Body != "Look\nsecond line" || str(m4, "text_source") != "part" || str(m4, "kind") != "media" {
		t.Errorf("M4: body %q text_source %q kind %q", m4.Body, str(m4, "text_source"), str(m4, "kind"))
	}
	_, got := store{
		participants:  []mrow{person(2, "+15550000002")},
		conversations: []mrow{{1, map[string]any{}}},
		messages:      []mrow{{1, nil}},
		parts: []mrow{ // inserted 30, 10, 20: read in id order
			{30, map[string]any{"message_id": int64(1), "text": "c"}},
			{10, map[string]any{"message_id": int64(1), "text": "a"}},
			{20, map[string]any{"message_id": int64(1), "text": "b"}},
		},
	}.parse(t)
	r := got[1]
	if r.Body != "a\nb\nc" || str(r, "text_source") != "part" || str(r, "kind") != "text" || rawOf(r)["parts"] != int64(3) {
		t.Errorf("body %q text_source %q kind %q raw %v", r.Body, str(r, "text_source"), str(r, "kind"), rawOf(r))
	}
	if !strings.Contains(r.Summary, "a b c") {
		t.Errorf("summary %q does not state the body", r.Summary)
	}
}

// R11, spec 8.2: a live part of no message is a record of its own (the orphans 900 and 901 of gm-aosp-v2), counted in the
// note orphan_parts and never a row warning.
func TestGoogleMessagesOrphanPartIsStatedNotDropped(t *testing.T) {
	res := parseAt(t, remoteApp, fixtureFiles(t, "gm-aosp-v2"))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	parts := partsByRowid(t, res)
	if len(parts) != 2 {
		t.Fatalf("%d parts records, want the orphans 900 and 901", len(parts))
	}
	p900, p901 := parts[900], parts[901]
	if p900.Body != "orphan text" || rawOf(p900)["part_message_id"] != int64(999) || rawOf(p900)["part_state"] != "orphan" ||
		str(p900, "channel") != "unknown" || str(p900, "direction") != "unknown" || p900.Payload["participants_unknown"] != true {
		t.Errorf("part 900: body %q raw %v payload %v", p900.Body, rawOf(p900), p900.Payload)
	}
	if p901.Body != "null message id" || rawOf(p901)["part_message_id_state"] != "null" || has(p901, "part_message_id") {
		t.Errorf("part 901: body %q raw %v", p901.Body, rawOf(p901))
	}
	if res.Notes["orphan_parts"] != "2" {
		t.Errorf("note orphan_parts = %q, want 2", res.Notes["orphan_parts"])
	}
	for _, w := range res.Warnings {
		if strings.Contains(w.Locator, "table=parts") {
			t.Errorf("warning for a part: %s: %s", w.Locator, w.Reason)
		}
	}
}

// stepsParser runs the live steps of android.googlemessages with every Get and Match reported and the budgets of the member
// cache and of the decided id set given.
type stepsParser struct {
	androidgooglemessages.Parser
	memberBudget, idBudget *parse.BudgetView
	on                     func(step, op, table string)
}

func (p stepsParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	_, err := rowrun.Run(ctx, in, out, androidgooglemessages.CountingSteps(in, p.memberBudget, p.idBudget, p.on))
	return err
}

// B6, R11, R12: every part is accounted for against the decided id set: joined, orphan record or row warning. The parts step
// never asks the messages table.
func TestGoogleMessagesPartsPassUsesTheDecidedIdSet(t *testing.T) {
	files := fixtureFiles(t, "gm-aosp-v2")
	total, partMsg := partsOf(t, files)

	// (a) joined + orphan records + row warnings == the engine's rows, and an orphan names no live message
	res := parseAt(t, remoteApp, files)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	live := byRowid(t, res)
	orphans := partsByRowid(t, res)
	joined, err := strconv.Atoi(res.Notes["parts.joined"])
	if err != nil {
		t.Fatalf("note parts.joined = %q", res.Notes["parts.joined"])
	}
	warned := len(warningsWith(res, "table=parts"))
	if joined+len(orphans)+warned != total || joined == 0 {
		t.Errorf("joined %d + orphans %d + warnings %d != %d parts rows", joined, len(orphans), warned, total)
	}
	for id, r := range orphans {
		if m, ok := rawOf(r)["part_message_id"].(int64); ok {
			if _, isLive := live[m]; isLive {
				t.Errorf("orphan part %d names the live message %d", id, m)
			}
		}
	}

	// (b) a budget that admits the first 10 ids only: no orphan, every part not joined is a row warning, the sum holds
	var ids []int64
	for id := range live {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	first := map[int64]bool{}
	for _, id := range ids[:10] {
		first[id] = true
	}
	wantJoined := 0
	for _, m := range partMsg {
		if first[m] {
			wantJoined++
		}
	}
	h := parsertest.NewHarness(t)
	p := stepsParser{idBudget: parse.NewBudget(10 * androidgooglemessages.IDCost).View(), on: func(string, string, string) {}}
	res = h.RunLenient(p, inputAt(t, h, p, remoteApp, files, false))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	const reason = "part's message is undecided: the message-id set is incomplete (budget)"
	undecided := 0
	for _, w := range res.Warnings {
		if strings.Contains(w.Locator, "table=parts") {
			if !strings.Contains(w.Reason, reason) {
				t.Errorf("warning %s: %s", w.Locator, w.Reason)
			}
			undecided++
		}
	}
	if n := len(partsByRowid(t, res)); n != 0 {
		t.Errorf("%d orphan records under an incomplete set, want none", n)
	}
	if undecided != total-wantJoined || wantJoined == 0 {
		t.Errorf("%d undecided parts, want %d of %d (the parts of the first 10 messages join)", undecided, total-wantJoined, total)
	}

	// (c) the parts step never reads the messages table
	var mu sync.Mutex
	calls := map[string]int{}
	h = parsertest.NewHarness(t)
	p = stepsParser{on: func(step, op, table string) {
		mu.Lock()
		defer mu.Unlock()
		calls[step+" "+op+" "+table]++
	}}
	res = h.RunLenient(p, inputAt(t, h, p, remoteApp, files, false))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	for _, k := range []string{"parts get messages", "parts match messages"} {
		if calls[k] != 0 {
			t.Errorf("%s: %d calls during the parts step", k, calls[k])
		}
	}
	if calls["messages match parts"] == 0 {
		t.Errorf("the messages step never asked for its parts: %v", calls)
	}
}

// Review Focus 2, spec 6.4, R6: a parts join that cannot be decided is no Body and body_state undecided with parts in
// join_undecided, one counted warning; it is never "no-parts".
func TestGoogleMessagesUndecidedPartsJoinIsNotNoBody(t *testing.T) {
	var msgs, parts []mrow
	for i := int64(1); i <= 60; i++ {
		msgs = append(msgs, mrow{i, nil})
		parts = append(parts, mrow{i, map[string]any{"message_id": i, "text": strings.Repeat("t", 300)}})
	}
	s := store{participants: []mrow{person(2, "+15550000002")}, conversations: []mrow{{1, map[string]any{}}}, messages: msgs, parts: parts}
	b := s.builder(t)
	leaves := b.Object("parts").Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d parts leaves, want several", len(leaves))
	}
	dmg := b.PageBytes(leaves[1])
	dmg[3], dmg[4] = 0, 0 // cell count 0: the engine calls the page corrupt
	res := parseAt(t, remoteApp, dbOnly(b.Bytes()))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	got := byRowid(t, res)
	if len(got) != 60 {
		t.Fatalf("%d messages records, want 60", len(got))
	}
	undecided := 0
	for id, r := range got {
		raw := rawOf(r)
		if raw["body_state"] == "no-parts" {
			t.Errorf("message %d: body_state no-parts, its parts were lost", id)
		}
		if raw["body_state"] == "undecided" {
			undecided++
			if !reflect.DeepEqual(raw["join_undecided"], []any{"parts"}) || r.Body != "" || has(r, "parts") {
				t.Errorf("message %d: undecided with raw %v body %q", id, raw, r.Body)
			}
		} else if r.Body == "" {
			t.Errorf("message %d: no Body and body_state %v", id, raw["body_state"])
		}
	}
	if undecided == 0 {
		t.Fatal("no message has an undecided parts join")
	}
	want := "googlemessages-join-undecided: parts: " + strconv.Itoa(undecided) + " records"
	if w := warningsWith(res, "googlemessages-join-undecided: parts"); len(w) != 1 || !strings.HasSuffix(w[0], want) {
		t.Errorf("warnings %q, want %q", w, want)
	}
}

// Spec 6.5: an attachment carries source_ref and mime only; the telephony part file beside it is never looked up.
func TestGoogleMessagesAttachmentsAreNeverLookedUp(t *testing.T) {
	check := func(label string, r records.Record) {
		t.Helper()
		atts, _ := r.Payload["attachments"].([]any)
		if len(atts) != 1 {
			t.Fatalf("%s: %d attachments, want 1", label, len(atts))
		}
		a, _ := atts[0].(map[string]any)
		if !reflect.DeepEqual(a, map[string]any{"mime": "image/jpeg", "source_ref": "content://mms/part/90"}) {
			t.Errorf("%s: attachment %v, want mime and source_ref only (no artifact_id, name or size)", label, a)
		}
	}
	_, alone := fixtureRecords(t, "gm-aosp-v2")
	check("alone", alone[4])

	files := fixtureFiles(t, "gm-aosp-v2")
	h := parsertest.NewHarness(t)
	p := androidgooglemessages.Parser{}
	in := inputAt(t, h, p, remoteApp, files, false)
	parsertest.AddAndroidFile(t, h.Case, "acq1", "/data/data/com.android.providers.telephony/app_parts/PART_90", []byte("\xff\xd8\xff jpeg bytes"))
	res := h.RunLenient(p, in)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	check("beside the part file", byRowid(t, res)[4])
}

// Spec 6.4, 6.5: a part with both text and uri is both; a part with neither is counted; a NULL content_type is no mime; a
// width stored as -1 stays -1; an unreadable width is listed, never read as absent.
func TestGoogleMessagesPartShapes(t *testing.T) {
	_, core := fixtureRecords(t, "gm-aosp-v2")
	m29 := core[29]
	if rawOf(m29)["parts_text_and_uri"] != int64(1) || m29.Body != "both" || str(m29, "kind") != "media" {
		t.Errorf("M29: body %q kind %q raw %v", m29.Body, str(m29, "kind"), rawOf(m29))
	}
	if atts, _ := m29.Payload["attachments"].([]any); len(atts) != 1 || !reflect.DeepEqual(atts[0], map[string]any{"mime": "text/plain", "source_ref": "content://mms/part/91"}) {
		t.Errorf("M29: attachments %v", m29.Payload["attachments"])
	}
	m30 := core[30]
	if rawOf(m30)["parts_empty"] != int64(1) || str(m30, "kind") != "unknown" || m30.Body != "" || has(m30, "attachment_parts") || m30.Payload["attachments"] != nil {
		t.Errorf("M30: kind %q raw %v", str(m30, "kind"), rawOf(m30))
	}
	_, got := store{
		participants:  []mrow{person(2, "+15550000002")},
		conversations: []mrow{{1, map[string]any{}}},
		messages:      []mrow{{1, nil}, {2, nil}},
		parts: []mrow{
			{1, map[string]any{"message_id": int64(1), "uri": "content://mms/part/1", "content_type": nil, "width": int64(-1), "height": int64(5)}},
			{2, map[string]any{"message_id": int64(2), "uri": "content://mms/part/2", "content_type": "image/png", "width": "x", "height": nil}},
		},
	}.parse(t)
	a1, _ := got[1].Payload["attachments"].([]any)
	ap1, _ := rawOf(got[1])["attachment_parts"].([]any)
	if len(a1) != 1 || !reflect.DeepEqual(a1[0], map[string]any{"source_ref": "content://mms/part/1"}) ||
		len(ap1) != 1 || !reflect.DeepEqual(ap1[0], map[string]any{"part_id": int64(1), "width": int64(-1), "height": int64(5)}) {
		t.Errorf("NULL content_type: attachments %v attachment_parts %v", a1, ap1)
	}
	ap2, _ := rawOf(got[2])["attachment_parts"].([]any)
	if len(ap2) != 1 || !reflect.DeepEqual(ap2[0], map[string]any{"part_id": int64(2)}) || unknownOf(got[2])["parts.width"] != "type-mismatch" {
		t.Errorf("unreadable width: attachment_parts %v unknown %v", ap2, unknownOf(got[2]))
	}
}
