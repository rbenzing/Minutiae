package androidgooglemessages_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// store is a Go-built bugle_db: the rows of each table by column (a column not named stores the default of its table, an
// explicit nil stores NULL). plainIDs makes participants._id an ordinary column holding the rowid (a key that is not the
// rowid alias: the join guard of Task 3 then makes every join through it undecided).
type store struct {
	participants, conversations, members, messages, parts []mrow
	plainIDs                                              bool
}

func participantDefaults() map[string]any {
	return map[string]any{"sub_id": int64(-2), "sim_slot_id": int64(-1), "contact_id": int64(-1), "blocked": int64(0)}
}

// person is a non-self participant (sub_id -2) whose three destinations are all dest.
func person(id int64, dest string) mrow {
	return mrow{id, map[string]any{"normalized_destination": dest, "send_destination": dest, "display_destination": dest}}
}

// selfRow is a participant with the given sub_id and no destination (a SIM of the device).
func selfRow(id, sub int64) mrow { return mrow{id, map[string]any{"sub_id": sub}} }

func memberRows(conv int64, pids ...int64) []mrow {
	var out []mrow
	for _, p := range pids {
		out = append(out, mrow{0, map[string]any{"conversation_id": conv, "participant_id": p}})
	}
	return out
}

// builder builds the database; the member rows get the rowids 1, 2, ... in the order given.
func (s store) builder(t testing.TB) *sqlitetest.Builder {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 2})
	fill := func(name string, rows []mrow, def map[string]any, plain bool) {
		ddl := v2DDL(t, name)
		if plain {
			ddl = strings.Replace(ddl, "_id INTEGER PRIMARY KEY AUTOINCREMENT", "_id INT", 1)
		}
		tb := b.CreateTable(name, ddl)
		cols := columnsOf(ddl)
		for i, r := range rows {
			id := r.id
			if id == 0 {
				id = int64(i + 1)
			}
			vals := make([]any, len(cols))
			for j, c := range cols {
				v, ok := r.kw[c]
				if !ok {
					v = def[c]
				}
				if c == "_id" {
					v = nil
					if plain {
						v = id
					}
				}
				vals[j] = v
			}
			tb.Insert(id, vals...)
		}
	}
	fill("conversations", s.conversations, nil, false)
	fill("messages", s.messages, messageDefaults(), false)
	fill("parts", s.parts, map[string]any{"conversation_id": int64(1), "timestamp": tT + 1000, "width": int64(-1), "height": int64(-1)}, false)
	fill("participants", s.participants, participantDefaults(), s.plainIDs)
	fill("conversation_participants", s.members, nil, false)
	return b
}

func (s store) parse(t *testing.T) (parsertest.Result, map[int64]records.Record) {
	t.Helper()
	res := parseAt(t, remoteApp, dbOnly(s.builder(t).Bytes()))
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	return res, byRowid(t, res)
}

// rolesOf is "<role> <address>" of each participant of a record.
func rolesOf(r records.Record) []string {
	var out []string
	ps, _ := r.Payload["participants"].([]any)
	for _, p := range ps {
		m, _ := p.(map[string]any)
		out = append(out, m["role"].(string)+" "+m["address"].(string))
	}
	return out
}

func threadMap(r records.Record) map[string]any {
	m, _ := r.Payload["thread"].(map[string]any)
	return m
}

func threadMembers(r records.Record) []string {
	var out []string
	l, _ := threadMap(r)["members"].([]any)
	for _, a := range l {
		out = append(out, a.(string))
	}
	return out
}

func partyAt(t *testing.T, r records.Record, i int) map[string]any {
	t.Helper()
	ps, _ := r.Payload["participants"].([]any)
	if i >= len(ps) {
		t.Fatalf("record %s has %d participants, want index %d", r.Locator, len(ps), i)
	}
	return ps[i].(map[string]any)
}

func warningsWith(res parsertest.Result, sub string) []string {
	var out []string
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, sub) {
			out = append(out, w.Locator+": "+w.Reason)
		}
	}
	return out
}

// selfDestinations are the destinations of the participants rows that are the self (sub_id != -2), read from the engine.
func selfDestinations(t *testing.T, files map[string][]byte) map[string]bool {
	t.Helper()
	f := sqlitedb.Files{DB: bytes.NewReader(files["db"]), DBSize: int64(len(files["db"]))}
	if w, ok := files["wal"]; ok {
		f.WAL, f.WALSize = bytes.NewReader(w), int64(len(w))
	}
	if j, ok := files["journal"]; ok {
		f.Journal, f.JournalSize = bytes.NewReader(j), int64(len(j))
	}
	db, err := sqlitedb.Open(t.Context(), f, parse.NewBudget(1<<30).View())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	tb, err := db.Table(t.Context(), "participants", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	sub := tb.Col("sub_id")
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		if v, ok := r.Int(sub); !ok || v == -2 {
			return nil
		}
		for _, c := range []string{"normalized_destination", "send_destination", "display_destination"} {
			if b, ok := r.Text(tb.Col(c)); ok && len(b) > 0 {
				out[string(b)] = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// Spec 6.3, B2, 4I5-P3: the self participant (sub_id != -2) is never an address: no participant and no thread member of any
// record of any variant equals a destination of a self row; a self sender with a destination (H13) states sender_is_self and
// gives no from; so does the SIM of M33.
func TestGoogleMessagesSelfIsNeverAnAddress(t *testing.T) {
	selfDests := 0
	for _, v := range variants {
		files := fixtureFiles(t, v)
		if len(parsertest.LoadFixture(t, "testdata", v).Expect.Records) == 0 {
			continue
		}
		dests := selfDestinations(t, files)
		selfDests += len(dests)
		res := parseAt(t, remoteApp, files)
		if res.Err != nil {
			t.Fatalf("%s: %v", v, res.Err)
		}
		for id, r := range byRowid(t, res) {
			for _, p := range rolesOf(r) {
				if _, addr, _ := strings.Cut(p, " "); dests[addr] {
					t.Errorf("%s message %d: participant %q is a destination of a self row", v, id, p)
				}
			}
			for _, a := range threadMembers(r) {
				if dests[a] {
					t.Errorf("%s message %d: thread member %q is a destination of a self row", v, id, a)
				}
			}
		}
	}
	if selfDests == 0 {
		t.Error("no variant holds a self row with a destination: the test checks nothing (H13 should)")
	}
	_, hostile := fixtureRecords(t, "gm-hostile")
	if r := hostile[114]; rawOf(r)["sender_is_self"] != true || len(rolesOf(r)) != 1 || strings.HasPrefix(rolesOf(r)[0], "from") {
		t.Errorf("H13: sender_is_self %v, participants %v", rawOf(r)["sender_is_self"], rolesOf(r))
	}
	_, core := fixtureRecords(t, "gm-aosp-v2")
	for _, id := range []int64{19, 33} { // M19: an incoming status with a self sender; M33: the second SIM sends
		r := core[id]
		for _, p := range rolesOf(r) {
			if strings.HasPrefix(p, "from ") {
				t.Errorf("M%d: has a from party %q for a self sender", id, p)
			}
		}
		if rawOf(r)["sender_is_self"] != true {
			t.Errorf("M%d: sender_is_self %v, want true", id, rawOf(r)["sender_is_self"])
		}
	}
	// a Go-built store: the self row with a destination is a member row of the conversation and the sender of an
	// incoming message; the destination reaches no party and no member
	_, got := store{
		participants:  []mrow{{1, map[string]any{"sub_id": int64(1), "normalized_destination": "+15559999999", "send_destination": "+15559999999"}}, person(2, "+15550000002")},
		conversations: []mrow{{1, map[string]any{"name": "x"}}},
		members:       memberRows(1, 1, 2),
		messages:      []mrow{{1, map[string]any{"sender_id": int64(1)}}, {2, map[string]any{"sender_id": int64(1), "message_status": int64(1)}}},
	}.parse(t)
	for id, r := range got {
		if s := strings.Join(rolesOf(r), "|") + "|" + strings.Join(threadMembers(r), "|"); strings.Contains(s, "9999999") {
			t.Errorf("message %d: the self destination reached the record: %s", id, s)
		}
	}
	if !reflect.DeepEqual(threadMembers(got[2]), []string{"+15550000002"}) {
		t.Errorf("thread members %v, want only the other participant", threadMembers(got[2]))
	}
}

// 4I5-C9, spec 6.3: the store keeps no per-message recipients, so an outgoing message lists the conversation's current
// non-self members with the role member and says the membership is current; no record of any variant has the role to.
func TestGoogleMessagesOutgoingPartiesAreCurrentMembers(t *testing.T) {
	outgoing := 0
	for _, v := range variants {
		if len(parsertest.LoadFixture(t, "testdata", v).Expect.Records) == 0 {
			continue
		}
		res := parseAt(t, remoteApp, fixtureFiles(t, v))
		if res.Err != nil {
			t.Fatalf("%s: %v", v, res.Err)
		}
		for id, r := range byRowid(t, res) {
			for _, p := range rolesOf(r) {
				if strings.HasPrefix(p, "to ") {
					t.Errorf("%s message %d: role to in %v", v, id, rolesOf(r))
				}
			}
			if str(r, "direction") == "out" && len(rolesOf(r)) > 0 {
				outgoing++
				if rawOf(r)["members_basis"] != "current" {
					t.Errorf("%s message %d: outgoing with members and members_basis %v", v, id, rawOf(r)["members_basis"])
				}
				for _, p := range rolesOf(r) {
					if !strings.HasPrefix(p, "member ") {
						t.Errorf("%s message %d: outgoing party %q is not a member", v, id, p)
					}
				}
			}
		}
	}
	if outgoing == 0 {
		t.Error("no outgoing record with members")
	}
	_, core := fixtureRecords(t, "gm-aosp-v2")
	if got := rolesOf(core[2]); !reflect.DeepEqual(got, []string{"member 5550000002"}) {
		t.Errorf("M2: %v", got)
	}
	if got := rolesOf(core[3]); !reflect.DeepEqual(got, []string{"member +15550000003", "member 5550000002", "member 5550000004"}) {
		t.Errorf("M3: %v", got)
	}
	// a member row deleted after the message was written: the record lists the remaining members only
	s := store{
		participants:  []mrow{selfRow(1, 1), person(2, "+15550000002"), person(3, "+15550000003")},
		conversations: []mrow{{1, map[string]any{"name": "g"}}},
		members:       memberRows(1, 1, 2, 3),
		messages:      []mrow{{1, map[string]any{"sender_id": int64(1), "message_status": int64(1)}}},
	}
	_, before := s.parse(t)
	if got := rolesOf(before[1]); !reflect.DeepEqual(got, []string{"member +15550000002", "member +15550000003"}) {
		t.Fatalf("before the delete: %v", got)
	}
	b := s.builder(t)
	b.Object("conversation_participants").Delete(3) // the member row of participant 2
	res := parseAt(t, remoteApp, dbOnly(b.Bytes()))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	after := byRowid(t, res)[1]
	if got := rolesOf(after); !reflect.DeepEqual(got, []string{"member +15550000002"}) || rawOf(after)["members_basis"] != "current" {
		t.Errorf("after the delete: %v, members_basis %v", got, rawOf(after)["members_basis"])
	}
}

// Spec 6.2 cross-check, B7: only a decided sender that contradicts a decided direction makes the direction unknown (the
// status stays), with raw.direction_conflict and one counted warning; no sender, a dangling sender and an undecided self-ness
// are not conflicts.
func TestGoogleMessagesDirectionConflictIsStated(t *testing.T) {
	res, core := fixtureRecords(t, "gm-aosp-v2")
	m19, m20 := core[19], core[20]
	for _, c := range []struct {
		r      records.Record
		status string
		dir    string
		self   bool
	}{{m19, "received", "in", true}, {m20, "sent", "out", false}} {
		dc, _ := rawOf(c.r)["direction_conflict"].(map[string]any)
		if str(c.r, "direction") != "unknown" || str(c.r, "status") != c.status || dc["status_direction"] != c.dir || dc["sender_is_self"] != c.self {
			t.Errorf("%s: direction %q status %q direction_conflict %v", c.r.Locator, str(c.r, "direction"), str(c.r, "status"), dc)
		}
	}
	if w := warningsWith(res, "googlemessages-direction-conflict"); len(w) != 1 || !strings.HasSuffix(w[0], "googlemessages-direction-conflict: 2 records") {
		t.Errorf("direction-conflict warnings %v, want exactly one: 2 records", w)
	}
	for _, id := range []int64{15, 16, 35} { // no sender, a dangling sender, an undecided self-ness
		if r := core[id]; str(r, "direction") != "in" || has(r, "direction_conflict") {
			t.Errorf("M%d: direction %q, direction_conflict %v: not a conflict", id, str(r, "direction"), rawOf(r)["direction_conflict"])
		}
	}
	// no other record of any variant is a conflict unless it is M19 or M20
	for _, v := range variants[1:] {
		if len(parsertest.LoadFixture(t, "testdata", v).Expect.Records) == 0 {
			continue
		}
		res, recs := fixtureRecords(t, v)
		for id, r := range recs {
			if has(r, "direction_conflict") {
				t.Errorf("%s message %d: unexpected direction_conflict", v, id)
			}
		}
		if w := warningsWith(res, "googlemessages-direction-conflict"); len(w) != 0 {
			t.Errorf("%s: %v", v, w)
		}
	}
}

// B7: an uncited status is direction unknown even when the sender is decidable; the sender fact stays in raw.sender_is_self.
func TestGoogleMessagesUncitedStatusIsUnknownEvenWithSender(t *testing.T) {
	_, core := fixtureRecords(t, "gm-aosp-v2")
	m11 := core[11] // status 150, a non-self sender
	if str(m11, "direction") != "unknown" || str(m11, "status") != "unknown" || rawOf(m11)["sender_is_self"] != false || has(m11, "direction_conflict") {
		t.Errorf("M11: direction %q status %q raw %v", str(m11, "direction"), str(m11, "status"), rawOf(m11))
	}
	if !reflect.DeepEqual(rolesOf(m11), []string{"from 5550000002"}) {
		t.Errorf("M11: participants %v", rolesOf(m11))
	}
	// status 150 with a self sender: no direction, the self is no party, the sender fact is stated
	_, got := store{
		participants:  []mrow{selfRow(1, 1), person(2, "+15550000002")},
		conversations: []mrow{{1, map[string]any{"name": "x"}}},
		members:       memberRows(1, 2),
		messages:      []mrow{{1, map[string]any{"sender_id": int64(1), "message_status": int64(150)}}},
	}.parse(t)
	r := got[1]
	if str(r, "direction") != "unknown" || rawOf(r)["sender_is_self"] != true || r.Payload["participants_unknown"] != true || len(rolesOf(r)) != 0 ||
		rawOf(r)["sender_state"] != "self" || has(r, "direction_conflict") || rawOf(r)["message_status"] != int64(150) {
		t.Errorf("status 150, self sender: direction %q, payload %v", str(r, "direction"), r.Payload)
	}
}

// Spec 6.3, R9: the address is send_destination, else normalized_destination, else display_destination; the name only with
// a resolved contact_id; an empty, altered or over-long address is not an address and says why.
func TestGoogleMessagesAddressBasisAndName(t *testing.T) {
	_, core := fixtureRecords(t, "gm-aosp-v2")
	if m1 := core[1]; rawOf(m1)["from_address_basis"] != "send_destination" || partyAt(t, m1, 0)["name"] != "Ann Example" || rawOf(m1)["from_name_basis"] != "cached_contact" {
		t.Errorf("M1: raw %v, party %v", rawOf(m1), partyAt(t, m1, 0))
	}
	if m4 := core[4]; rawOf(m4)["from_address_basis"] != "normalized_destination" || partyAt(t, m4, 0)["name"] != nil || rawOf(m4)["from_full_name"] != "+1 555-000-0003" || has(m4, "from_name_basis") {
		t.Errorf("M4: raw %v, party %v", rawOf(m4), partyAt(t, m4, 0))
	}
	big := string(bytes.Repeat([]byte{0xff}, 400)) // 400 bytes stored, 1,200 once each invalid byte is U+FFFD
	sender := func(id int64, kw map[string]any) mrow {
		kw["sub_id"] = int64(-2)
		return mrow{id, kw}
	}
	_, got := store{
		participants: []mrow{
			selfRow(1, 1),
			sender(2, map[string]any{"display_destination": "(555) 0002"}),
			sender(3, map[string]any{"send_destination": "", "normalized_destination": "+15550003"}),
			sender(4, map[string]any{}),
			sender(5, map[string]any{"send_destination": "", "normalized_destination": "", "display_destination": ""}),
			sender(6, map[string]any{"send_destination": "55\x0050"}),
			sender(7, map[string]any{"send_destination": big}),
			sender(8, map[string]any{"send_destination": "5550008", "full_name": "Eight", "contact_id": int64(9)}),
			sender(9, map[string]any{"send_destination": "5550009", "full_name": "Nine", "contact_id": int64(-1)}),
			sender(10, map[string]any{"send_destination": "5550010", "full_name": "Ten", "contact_id": nil}),
			sender(11, map[string]any{"send_destination": "5550011", "full_name": "El\xffeven", "contact_id": int64(3)}),
		},
		conversations: []mrow{{1, map[string]any{"name": "x"}}},
		messages: []mrow{
			{2, map[string]any{"sender_id": int64(2)}},
			{3, map[string]any{"sender_id": int64(3)}},
			{4, map[string]any{"sender_id": int64(4)}},
			{5, map[string]any{"sender_id": int64(5)}},
			{6, map[string]any{"sender_id": int64(6)}},
			{7, map[string]any{"sender_id": int64(7)}},
			{8, map[string]any{"sender_id": int64(8)}},
			{9, map[string]any{"sender_id": int64(9)}},
			{10, map[string]any{"sender_id": int64(10)}},
			{11, map[string]any{"sender_id": int64(11)}},
		},
	}.parse(t)
	for id, basis := range map[int64]string{2: "display_destination", 3: "normalized_destination", 8: "send_destination"} {
		if r := got[id]; rawOf(r)["from_address_basis"] != basis || len(rolesOf(r)) != 1 {
			t.Errorf("message %d: basis %v, parties %v", id, rawOf(r)["from_address_basis"], rolesOf(r))
		}
	}
	for id, state := range map[int64]string{4: "empty", 5: "empty", 6: "altered", 7: "too-long"} {
		r := got[id]
		if rawOf(r)["from_address_state"] != state || r.Payload["participants_unknown"] != true || len(rolesOf(r)) != 0 || has(r, "from_address_basis") {
			t.Errorf("message %d: from_address_state %v, participants %v, raw %v", id, rawOf(r)["from_address_state"], rolesOf(r), rawOf(r))
		}
	}
	if r := got[6]; rawOf(r)["from_address_text"] != "55�50" {
		t.Errorf("altered address: from_address_text %v", rawOf(r)["from_address_text"])
	}
	if r := got[7]; rawOf(r)["from_address_len"] != int64(400) || rawOf(r)["from_address_sha256"] != sha([]byte(big)) { // never cut, even after cleaning
		t.Errorf("a destination over 1 KiB once cleaned: raw %v", rawOf(r))
	}
	if r := got[8]; partyAt(t, r, 0)["name"] != "Eight" || rawOf(r)["from_name_basis"] != "cached_contact" || rawOf(r)["from_contact_id"] != int64(9) {
		t.Errorf("a resolved contact: party %v raw %v", partyAt(t, r, 0), rawOf(r))
	}
	for _, id := range []int64{9, 10} { // contact_id -1 and NULL: the full name is raw only
		r := got[id]
		if partyAt(t, r, 0)["name"] != nil || has(r, "from_name_basis") || rawOf(r)["from_full_name"] == nil {
			t.Errorf("message %d: party %v raw %v", id, partyAt(t, r, 0), rawOf(r))
		}
	}
	if r := got[11]; partyAt(t, r, 0)["name"] != "El�even" || !reflect.DeepEqual(rawOf(r)["text_flags"], []any{"from_full_name"}) {
		t.Errorf("a name with an invalid byte: party %v raw %v", partyAt(t, r, 0), rawOf(r))
	}
	// the hostile variant: H12 has a 2 KiB destination, never an address
	_, h := fixtureRecords(t, "gm-hostile")
	if r := h[113]; rawOf(r)["from_address_state"] != "too-long" || rawOf(r)["from_address_len"] != int64(2048) || rawOf(r)["from_address_sha256"] != sha(bytes.Repeat([]byte("8"), 2048)) || len(rolesOf(r)) != 0 {
		t.Errorf("H12: raw %v parties %v", rawOf(r), rolesOf(r))
	}
}

// R6, spec 6.3: a join the guard cannot decide is stated undecided in the record (raw.join_undecided), counted in one
// warning per table, and never read as a missing row.
func TestGoogleMessagesJoinUndecidedIsStated(t *testing.T) {
	res, got := store{
		plainIDs:      true, // participants._id is not the rowid alias: every join through it is undecided
		participants:  []mrow{selfRow(1, 1), person(2, "+15550000002")},
		conversations: []mrow{{1, map[string]any{"name": "x"}}},
		members:       memberRows(1, 1, 2),
		messages: []mrow{
			{1, map[string]any{}}, {2, map[string]any{"message_status": int64(1), "sender_id": int64(1)}},
		},
	}.parse(t)
	if res.Notes["join_undecided.participants"] == "" {
		t.Errorf("notes %v: no join_undecided.participants", res.Notes)
	}
	for id, r := range got {
		if !reflect.DeepEqual(rawOf(r)["join_undecided"], []any{"participants"}) {
			t.Errorf("message %d: join_undecided %v, want [participants]", id, rawOf(r)["join_undecided"])
		}
		for _, k := range []string{"sender_state", "self_row_state", "members_state"} {
			if rawOf(r)[k] != "undecided" {
				t.Errorf("message %d: %s = %v, want undecided (never no-participants-row)", id, k, rawOf(r)[k])
			}
		}
		if r.Payload["participants_unknown"] != true || has(r, "sender_is_self") {
			t.Errorf("message %d: payload %v", id, r.Payload)
		}
	}
	if w := warningsWith(res, "googlemessages-join-undecided: participants: 2 records"); len(w) != 1 || !strings.HasPrefix(w[0], "sqlite:table=participants") {
		t.Errorf("warnings %v, want one counted warning for participants: 2 records", res.Warnings)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "no-participants-row") {
			t.Errorf("warning %q", w.Reason)
		}
	}
	// the conversations table is undecided the same way
	_, got = store{
		participants:  []mrow{person(2, "+15550000002")},
		conversations: []mrow{{1, map[string]any{"name": "x"}}},
		members:       memberRows(1, 2),
		messages:      []mrow{{1, map[string]any{}}},
	}.parse(t)
	if r := got[1]; has(r, "join_undecided") || rawOf(r)["sender_state"] != nil {
		t.Errorf("a decided store: join_undecided %v raw %v", rawOf(r)["join_undecided"], rawOf(r))
	}
}

// R3, R5: an unreadable sender_id cell is listed and the sender is undecided, never read as NULL (H6 has a TEXT
// sender_id).
func TestGoogleMessagesUnreadableSenderIsUndecided(t *testing.T) {
	_, h := fixtureRecords(t, "gm-hostile")
	r := h[106]
	if rawOf(r)["sender_state"] != "undecided" || unknownOf(r)["sender_id"] != "type-mismatch" || has(r, "sender_id") || r.Payload["participants_unknown"] != true {
		t.Errorf("H6: raw %v", rawOf(r))
	}
}
