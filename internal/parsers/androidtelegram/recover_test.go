package androidtelegram_test

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	tableV2    = "messages_v2"
	notJoined  = "not-joined-recovered" // pinned literally (F20)
	goneUserID = 1000012                // the users row telegram-deleted inserts and deletes in its WAL
)

// deletedBundle is telegram-deleted at the app path in h's case.
func deletedBundle(t *testing.T, h *parsertest.Harness) parsertest.Bundle {
	t.Helper()
	return parsertest.LoadFixture(t, "testdata", "telegram-deleted").Bundle(h, remoteApp)
}

// recoveredColumn is the index of a messages_v2 column in a recovered row (the recover oracle's column order, which
// is the table's).
func recoveredColumn(t *testing.T, col string) int {
	t.Helper()
	cols := parsertest.LoadRecoverExpect(t, "testdata", "telegram-deleted").Columns[tableV2]
	i := slices.Index(cols, col)
	if i < 0 {
		t.Fatalf("the recover oracle lists no messages_v2 column %s: %v", col, cols)
	}
	return i
}

// typedEmitter keeps everything the typed pass hands over: the recovered records with their facts, the unmapped rows,
// the warnings and notes, and the count of live emits (which a typed pass never makes).
type typedEmitter struct {
	recs     []records.Record
	facts    []parse.RecoveredFacts
	unmapped []string
	warnings []string
	notes    map[string]string
	live     int
}

func (e *typedEmitter) Emit(context.Context, records.Record) error {
	e.live++
	return nil
}

func (e *typedEmitter) EmitRecovered(_ context.Context, r records.Record, f parse.RecoveredFacts) error {
	e.recs, e.facts = append(e.recs, r), append(e.facts, f)
	return nil
}

func (e *typedEmitter) Unmapped(_ context.Context, f parse.RecoveredFacts, reason string) error {
	e.unmapped = append(e.unmapped, f.Locator+" "+reason+" "+strings.Join(f.Notes, ","))
	return nil
}

func (e *typedEmitter) Warn(_ context.Context, locator, reason string) error {
	e.warnings = append(e.warnings, locator, reason)
	return nil
}

func (e *typedEmitter) Note(k, v string)      { e.notes[k] = v }
func (e *typedEmitter) Progress(int64, int64) {}

// texts are every text the typed pass emitted (outputTexts over its records and notes, then its warnings, unmapped
// reasons and the facts' locators and notes).
func (e *typedEmitter) texts(t *testing.T) []string {
	t.Helper()
	out := outputTexts(t, parsertest.Result{Records: e.recs, Notes: e.notes})
	out = append(out, e.warnings...)
	out = append(out, e.unmapped...)
	for _, f := range e.facts {
		out = append(out, f.Locator, strings.Join(f.Notes, ","))
	}
	return out
}

// typedRun is the typed recovery pass of android.telegram over files at the app path, as the host runs it: recrun.Run
// with the parser's mappings and claims (F15: a capturing emitter, never AssertClaimedTablesMapRecoveredRows, which
// returns nothing).
func typedRun(t *testing.T, files map[string][]byte) *typedEmitter {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidtelegram.Parser{}
	b := parsertest.Fixture{Files: files}.Bundle(h, remoteApp)
	e := &typedEmitter{notes: map[string]string{}}
	if _, err := recrun.Run(t.Context(), b.Input(h, p, false), e, p.Mappings(), p.Meta().Claims); err != nil {
		t.Fatal(err)
	}
	if e.live != 0 {
		t.Errorf("the typed pass emitted %d live records", e.live)
	}
	return e
}

// secretRowsInWAL is a Go-built database (plan 4I4 Task 14, C1) whose main file holds telegram-v181's users and R1, and
// whose WAL holds, in a first committed frame set, telegram-v181's secret-dialog messages_v2 rows (R8, R9, R10, R57,
// R59: secret documents with their key and IV included) and its enc_chats rows with every column (the key columns
// g, authkey, fauthkey, khash, fprint and data), and in a second the same pages after those rows were deleted. The
// recovered secret rows and the enc_chats cells lie only in WAL frames.
func secretRowsInWAL(t *testing.T) map[string][]byte {
	t.Helper()
	files := fixtureFiles(t, "telegram-v181")
	db, err := sqlitedb.Open(t.Context(), sqlitedb.Files{
		DB: bytes.NewReader(files["db"]), DBSize: int64(len(files["db"])),
		WAL: bytes.NewReader(files["wal"]), WALSize: int64(len(files["wal"])),
	}, parse.NewBudget(1<<30).View())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	sql, err := os.ReadFile("../../../tools/fixtures/schemas/telegram-android/v181.sql")
	if err != nil {
		t.Fatal(err)
	}
	ddl := map[string]string{}
	for _, line := range strings.Split(string(sql), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ";"))
		if rest, ok := strings.CutPrefix(line, "CREATE TABLE "); ok {
			name, _, _ := strings.Cut(rest, " ")
			ddl[name] = line
		}
	}
	type stored struct {
		rowid int64
		vals  []any
	}
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 181})
	out := map[string]*sqlitetest.Table{}
	aside := map[string][]stored{}
	for _, name := range []string{"users", tableV2, androidtelegram.TableEncChats} {
		out[name] = b.CreateTable(name, ddl[name])
		tb, err := db.Table(t.Context(), name, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		uidAt := tb.Col("uid")
		err = tb.Scan(t.Context(), func(row sqlitedb.Row) error {
			id, _ := row.Rowid()
			vals := make([]any, len(tb.Cols()))
			for i, c := range tb.Cols() {
				if !c.RowidAlias {
					vals[i] = goValue(row.Value(i))
				}
			}
			uid, _ := row.Int(uidAt)
			switch {
			case name == androidtelegram.TableEncChats, name == tableV2 && uid > 0 && uid>>62&1 == 1:
				aside[name] = append(aside[name], stored{id, vals})
			case name == "users", name == tableV2 && id == 1:
				out[name].Insert(id, vals...)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(aside[tableV2]) < 4 || len(aside[androidtelegram.TableEncChats]) != 2 {
		t.Fatalf("telegram-v181 gave %d secret rows and %d enc_chats rows", len(aside[tableV2]), len(aside[androidtelegram.TableEncChats]))
	}
	main := b.Bytes()
	w := b.NewWAL(false, 1, 2, 0)
	frameAll := func() {
		n := b.Snapshot().Pages()
		for p := uint32(1); p <= n; p++ {
			commit := uint32(0)
			if p == n {
				commit = n
			}
			w.Frame(p, b.PageBytes(p), commit)
		}
	}
	for name, rows := range aside {
		for _, r := range rows {
			out[name].Insert(r.rowid, r.vals...)
		}
	}
	frameAll()
	for name, rows := range aside {
		for _, r := range rows {
			out[name].Delete(r.rowid)
		}
	}
	frameAll()
	return map[string][]byte{"db": main, "wal": w.Bytes()}
}

// Spec 11 (call 7): the claims are exactly the claimable (total) tables: messages, messages_v2 and users, every row of
// which is a record. media_v*, messages_topics, chats, contacts, users_data, dialogs and enc_chats are never claimed.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := androidtelegram.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	want := []string{"messages", "messages_v2", "users"}
	if cl := androidtelegram.ClaimableTables(); !slices.Equal(got, cl) || !slices.Equal(got, want) {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v = %v", got, cl, want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
	if err := parse.ValidateMeta(p.Meta()); err != nil {
		t.Errorf("Meta with its claims: %v", err)
	}
}

// Spec 11 and 4I2 PF13: the claimed tables map their recovered rows, checked against the oracle the generator computed
// with an independent page walker from the committed telegram-deleted files (a lower bound: every recovered row of a
// claimed table is a typed record, with its raw.live_key_state, T2). A variant with no oracle and no history is checked
// against the empty set, a real claim (a typed record there would be invented). telegram-v181 holds history but has
// no oracle: its WAL holds R52 in a frame that was never committed.
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range variants {
		if v == "telegram-v181" {
			continue
		}
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteApp)
			want := parsertest.RecoverExpect{Variant: v, Claimed: androidtelegram.ClaimableTables()}
			if v == "telegram-deleted" {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
				if len(want.Records) == 0 {
					t.Fatal("the telegram-deleted recover oracle lists no record: the check proves nothing")
				}
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, androidtelegram.Parser{}, b, want)
		})
	}
}

// B2 (4I4-P6, spec 11): a recovered row joins no table, even when the rows it names survive live. telegram-deleted's
// recovered row 201 is a user-dialog row of ANN (whose users row is live), 207 a message of the group 2000001 (whose
// chats row is live), 206 an outgoing message of secret chat 77 (whose enc_chats row is live) and the users row
// 1000012 a contact whose contacts and users_data rows are live: none has a thread title or a secret counterparty, and
// every join-derived state is "not-joined-recovered", pinned literally. Each row's own blob is decoded and stated as in
// a live parse: the same cells written live into a copy of the database give the same data_state, Body, kind,
// attachments and raw.media.
func TestTelegramRecoveredRowsJoinNothing(t *testing.T) {
	h := parsertest.NewHarness(t)
	p := androidtelegram.Parser{}
	b := deletedBundle(t, h)
	live := parseFixture(t, "telegram-deleted")
	liveRows := byRow(live.Records)
	if r4 := liveRows["messages_v2/4"]; r4.Payload == nil {
		t.Fatal("R4 is not live in telegram-deleted")
	} else if th, _ := r4.Payload["thread"].(map[string]any); th["title"] != "Family" {
		t.Fatalf("the chats row of group 2000001 is not live (R4's thread %v): the check proves nothing", th)
	}
	if live.Notes["rows.enc_chats"] != "2" || liveRows["users/1000002"].Payload == nil {
		t.Fatalf("enc_chats rows %q, users row of ANN live %v: the check proves nothing", live.Notes["rows.enc_chats"], liveRows["users/1000002"].Payload != nil)
	}
	recovered := map[int64]sqlitefile.RecoveredRow{}
	mapped := map[int64]records.Record{}
	for _, id := range []int64{201, 206, 207} {
		rr, ok := parsertest.FirstClaimedRow(t, h, p, b, tableV2, func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == id })
		if !ok {
			t.Fatalf("no recovered messages_v2 row %d in telegram-deleted", id)
		}
		if _, isLive := liveRows["messages_v2/"+itoa(id)]; isLive {
			t.Fatalf("messages_v2 row %d is live: the check proves nothing", id)
		}
		got := parsertest.MapRecoveredRow(t, h, p, b, rr)
		if got.Err != nil || got.Unmapped != "" || len(got.Records) != 1 {
			t.Fatalf("recovered row %d: err %v, unmapped %q, %d records", id, got.Err, got.Unmapped, len(got.Records))
		}
		recovered[id], mapped[id] = rr, got.Records[0]
	}
	for id, r := range mapped {
		raw := rawOf(r)
		if th, _ := r.Payload["thread"].(map[string]any); th["title"] != nil {
			t.Errorf("recovered row %d: thread.title %v: a recovered row joins nothing", id, th["title"])
		}
		for _, k := range []string{"join_undecided", "account", "chat_data_state", "enc_chats_user"} {
			if v, has := raw[k]; has {
				t.Errorf("recovered row %d: raw.%s = %v: a recovered row joins nothing", id, k, v)
			}
		}
	}
	for id, k := range map[int64]string{201: "thread_user_state", 207: "thread_title_state", 206: "counterparty_state"} {
		if got := rawOf(mapped[id])[k]; got != notJoined {
			t.Errorf("recovered row %d: raw.%s = %#v, want %q", id, k, got, notJoined)
		}
	}
	s := mapped[206]
	if rawOf(s)["secret_chat"] != true || s.Payload["participants_unknown"] != true || !slices.Contains([]string{"null", "[]"}, jsonText(s.Payload["participants"])) {
		t.Errorf("recovered secret row 206: secret_chat %v, participants %v (unknown %v): no counterparty without the join",
			rawOf(s)["secret_chat"], s.Payload["participants"], s.Payload["participants_unknown"])
	}

	// the own blob, as in a live parse: the recovered cells written live into a copy of the database
	cols := parsertest.LoadRecoverExpect(t, "testdata", "telegram-deleted").Columns[tableV2]
	var extra []builtRow
	for id, rr := range recovered {
		cells := map[string]any{}
		for i, c := range cols {
			cells[c] = goValue(rr.Values[i])
		}
		extra = append(extra, builtRow{id, cells})
	}
	copied := byRow(parseAt(t, remoteApp, dbOnly(fixtureCopy(t, "telegram-deleted", "v181.sql", nil, map[string][]builtRow{tableV2: extra}))).Records)
	for id, r := range mapped {
		l, ok := copied["messages_v2/"+itoa(id)]
		if !ok {
			t.Fatalf("the live copy has no row %d", id)
		}
		if r.Body != l.Body || r.Body == "" && id != 206 {
			t.Errorf("recovered row %d: Body %q, live %q", id, r.Body, l.Body)
		}
		for _, k := range []string{"kind", "direction", "attachments"} {
			if jsonText(r.Payload[k]) != jsonText(l.Payload[k]) {
				t.Errorf("recovered row %d: payload.%s %s, live %s", id, k, jsonText(r.Payload[k]), jsonText(l.Payload[k]))
			}
		}
		for _, k := range []string{"data_state", "data_stop_kind", "data_undecided", "media", "text_flags"} {
			if jsonText(rawOf(r)[k]) != jsonText(rawOf(l)[k]) {
				t.Errorf("recovered row %d: raw.%s %s, live %s", id, k, jsonText(rawOf(r)[k]), jsonText(rawOf(l)[k]))
			}
		}
	}
	if rawOf(mapped[206])["media"] == nil || rawOf(mapped[201])["data_state"] != "decoded" {
		t.Errorf("rows 206 and 201: raw.media %v, data_state %v: the comparison proves nothing", rawOf(mapped[206])["media"], rawOf(mapped[201])["data_state"])
	}

	// a recovered users row: its contacts and users_data rows are live, and neither is joined
	ur, ok := parsertest.FirstClaimedRow(t, h, p, b, "users", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == goneUserID })
	if !ok {
		t.Fatal("no recovered users row 1000012 in telegram-deleted")
	}
	if live.Notes["rows.contacts"] != "2" || live.Notes["rows.users_data"] != "2" {
		t.Fatalf("contacts %q, users_data %q rows: the check proves nothing", live.Notes["rows.contacts"], live.Notes["rows.users_data"])
	}
	got := parsertest.MapRecoveredRow(t, h, p, b, ur)
	if got.Err != nil || got.Unmapped != "" || len(got.Records) != 1 {
		t.Fatalf("recovered users row: err %v, unmapped %q, %d records", got.Err, got.Unmapped, len(got.Records))
	}
	c := got.Records[0]
	for _, k := range []string{"contact_row_state", "about_state"} {
		if rawOf(c)[k] != notJoined {
			t.Errorf("recovered users row: raw.%s = %#v, want %q", k, rawOf(c)[k], notJoined)
		}
	}
	for _, k := range []string{"mutual", "about", "join_undecided"} {
		if v, has := rawOf(c)[k]; has {
			t.Errorf("recovered users row: raw.%s = %v: a recovered row joins nothing", k, v)
		}
	}
	if c.Payload["notes"] != nil || rawOf(c)["data_state"] == nil {
		t.Errorf("recovered users row: notes %v, data_state %v", c.Payload["notes"], rawOf(c)["data_state"])
	}
}

// Ruling T2 (F30, facts.tsv write_verb.*): putReinserts follows the facts (true when any pinned put of a message table
// is REPLACE INTO). When true, the typed pass states each recovered message's relation to the live table by its own
// (mid, uid): R2's older images (REPLACE INTO gave R2 a new rowid) are "present" (the message's key is still live),
// and come only through the recovery emitter with the host's relation, never as live records; the mapping states no
// deletion of its own (ruling T8: the host's Deleted stamp on every typed record marks recovered provenance, 4G-R7);
// rows 201-207 are "absent"; a row whose mid cell cannot be read is "undecided". R1's edit image follows the pinned
// write_verb.messages_v2.edit as the put does: an UPDATE keeps the rowid, so the older image carries the host's
// superseded-version relation; a re-inserting verb gives a new rowid, so the older image is absent-from-live with
// live_key_state "present". telegram-v181's R52, written in a WAL frame that was never committed, is a typed record of
// the host's uncommitted facts with live_key_state "absent" (review M4). When false no recovered record has
// live_key_state. No live record of any variant ever has it.
func TestTelegramRecoveredLiveKeyStateFollowsTheWriteVerb(t *testing.T) {
	// putReinserts equals the facts: TestTelegramFactsMatchTheCode
	reinserts := androidtelegram.PutReinserts
	edits := factValues(t, "write_verb.messages_v2.edit")
	if len(edits) == 0 || slices.ContainsFunc(edits, func(v string) bool { return v != edits[0] }) {
		t.Fatalf("write_verb.messages_v2.edit is %v: one verb at every pin is what this test branches on", edits)
	}
	res := typedRun(t, fixtureFiles(t, "telegram-deleted"))
	byState := map[string]int{}
	for i, r := range res.recs {
		f, raw := res.facts[i], rawOf(r)
		state, has := raw["live_key_state"]
		if r.Type == "contact" {
			if has {
				t.Errorf("%s: a users row states live_key_state %v", f.Locator, state)
			}
			continue
		}
		if !reinserts {
			if has {
				t.Errorf("%s: live_key_state %v while no put re-inserts", f.Locator, state)
			}
			continue
		}
		byState[state.(string)]++
		// the mapping states no deletion of its own (T8: Deleted is the host's provenance stamp, made after the mapping)
		_, delField := r.Payload["deleted"]
		if r.Deleted || delField || r.Recovery != "" || f.Relation == "" {
			t.Errorf("%s: Deleted %v, payload.deleted present %v, Recovery %q, relation %q: the mapping states no deletion and the host gives the relation",
				f.Locator, r.Deleted, delField, r.Recovery, f.Relation)
		}
		// a recovered locator names the cell, not the rowid: the rows are told apart by their own mid
		switch mid := raw["mid"]; {
		case mid == int64(102):
			if state != "present" || r.Body != "Hi Ann" || f.Relation != "absent-from-live" {
				t.Errorf("%s: R2's older image: live_key_state %v, relation %q, Body %q", f.Locator, state, f.Relation, r.Body)
			}
		case slices.Contains([]any{int64(201), int64(202), int64(203), int64(204), int64(205), int64(-12), int64(207)}, mid):
			if state != "absent" {
				t.Errorf("%s: deleted row (mid %v): live_key_state %v, want absent", f.Locator, mid, state)
			}
		case mid == int64(101):
			want := struct{ state, relation string }{"present", "absent-from-live"}
			if edits[0] == "UPDATE" {
				want.relation = "superseded-version"
			}
			if r.Body != "Hello Ann here" || state != want.state || f.Relation != want.relation {
				t.Errorf("%s: R1's edit image under %s: Body %q, live_key_state %v, relation %q, want %+v", f.Locator, edits[0], r.Body, state, f.Relation, want)
			}
		default:
			t.Errorf("%s: an unexpected recovered message (mid %v, live_key_state %v)", f.Locator, mid, state)
		}
	}
	if reinserts && (byState["present"] == 0 || byState["absent"] == 0) {
		t.Errorf("live_key_state counts %v: both present and absent must occur, or the check proves nothing", byState)
	}

	if reinserts {
		// a recovered row whose mid cell cannot be read is undecided, never absent
		h := parsertest.NewHarness(t)
		p := androidtelegram.Parser{}
		b := deletedBundle(t, h)
		rr, ok := parsertest.FirstClaimedRow(t, h, p, b, tableV2, func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 201 })
		if !ok {
			t.Fatal("no recovered row 201")
		}
		rr.Values[recoveredColumn(t, "mid")].Unread = true
		got := parsertest.MapRecoveredRow(t, h, p, b, rr)
		if got.Err != nil || len(got.Records) != 1 || rawOf(got.Records[0])["live_key_state"] != "undecided" {
			t.Errorf("row 201 with an unreadable mid: err %v, unmapped %q, records %d, live_key_state %v", got.Err, got.Unmapped, len(got.Records),
				func() any {
					if len(got.Records) == 0 {
						return nil
					}
					return rawOf(got.Records[0])["live_key_state"]
				}())
		}
	}

	// review M4: R52 lies only in an uncommitted WAL frame of telegram-v181
	r52 := 0
	v181 := typedRun(t, fixtureFiles(t, "telegram-v181"))
	for i, r := range v181.recs {
		if rawOf(r)["mid"] != int64(148) {
			continue
		}
		r52++
		f := v181.facts[i]
		if f.Relation != "uncommitted" || f.Origin != "wal-uncommitted" || f.Method != "sqlite-wal-uncommitted" || f.Confidence != 45 ||
			f.WAL == nil || f.WAL.Committed || rawOf(r)["live_key_state"] != "absent" || r.Body != "in the WAL, uncommitted R52" {
			t.Errorf("R52 %s: relation %q, origin %q, method %q, confidence %d, wal %+v, live_key_state %v, Body %q", f.Locator,
				f.Relation, f.Origin, f.Method, f.Confidence, f.WAL, rawOf(r)["live_key_state"], r.Body)
		}
	}
	if r52 != 1 {
		t.Errorf("telegram-v181: %d typed records of R52, want 1", r52)
	}

	for _, v := range variants {
		for _, r := range parseAt(t, remoteApp, fixtureFiles(t, v)).Records {
			if s, has := rawOf(r)["live_key_state"]; has {
				t.Errorf("%s %s: a live record states live_key_state %v", v, r.Locator, s)
			}
		}
	}
}
