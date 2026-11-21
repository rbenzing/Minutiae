package androidgooglemessages_test

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// B2: the self is excluded from the member parties, the thread members and the group count by the cited rule; a member whose
// self-ness cannot be decided stays a member, the group flag is omitted and both are stated.
func TestGoogleMessagesSelfIsExcludedFromMembers(t *testing.T) {
	_, core := fixtureRecords(t, "gm-aosp-v2")
	m3 := core[3] // C2 lists P3, the self P1, P2 and P4
	if got := threadMembers(m3); !reflect.DeepEqual(got, []string{"+15550000003", "5550000002", "5550000004"}) || threadMap(m3)["group"] != true || rawOf(m3)["members_total"] != int64(3) {
		t.Errorf("M3: members %v group %v members_total %v", got, threadMap(m3)["group"], rawOf(m3)["members_total"])
	}
	m34 := core[34] // C4 lists P2 and P6, whose sub_id is NULL
	_, group := threadMap(m34)["group"]
	if rawOf(m34)["self_state"] != "undecided" || group || rawOf(m34)["group_state"] != "undecided" ||
		!reflect.DeepEqual(threadMembers(m34), []string{"5550000002", "5550000006"}) {
		t.Errorf("M34: self_state %v group_state %v members %v group present %v", rawOf(m34)["self_state"], rawOf(m34)["group_state"], threadMembers(m34), group)
	}
	// a conversation of the self and one other: one member, not a group
	_, got := store{
		participants:  []mrow{selfRow(1, 1), person(2, "+15550000002")},
		conversations: []mrow{{1, map[string]any{"name": "pair"}}},
		members:       memberRows(1, 1, 2),
		messages:      []mrow{{1, map[string]any{"sender_id": int64(1), "message_status": int64(1)}}},
	}.parse(t)
	r := got[1]
	if !reflect.DeepEqual(threadMembers(r), []string{"+15550000002"}) || threadMap(r)["group"] != false || rawOf(r)["members_total"] != int64(1) || has(r, "self_state") {
		t.Errorf("[self, P2]: members %v group %v raw %v", threadMembers(r), threadMap(r)["group"], rawOf(r))
	}
	// a SIM whose sub_id is a TEXT cell is undecided too, and listed
	_, got = store{
		participants:  []mrow{{1, map[string]any{"sub_id": "x", "normalized_destination": "+15550000001"}}, person(2, "+15550000002")},
		conversations: []mrow{{1, map[string]any{}}},
		members:       memberRows(1, 1, 2),
		messages:      []mrow{{1, map[string]any{"message_status": int64(1), "sender_id": int64(2)}}},
	}.parse(t)
	r = got[1]
	if rawOf(r)["self_state"] != "undecided" || unknownOf(r)["participants.sub_id"] != "type-mismatch" || len(threadMembers(r)) != 2 {
		t.Errorf("TEXT sub_id: raw %v members %v", rawOf(r), threadMembers(r))
	}
}

// B3: members are ordered by the conversation_participants rowid, read once per conversation per job and cut at 1,000 with one
// warning per conversation.
func TestGoogleMessagesMembersAreOrderedAndCut(t *testing.T) {
	_, got := store{
		participants:  []mrow{selfRow(1, 1), person(2, "+15550000002"), person(3, "+15550000003"), person(4, "+15550000004")},
		conversations: []mrow{{1, map[string]any{}}},
		members:       memberRows(1, 4, 2, 3), // rowids 1, 2, 3
		messages:      []mrow{{1, map[string]any{"sender_id": int64(1), "message_status": int64(1)}}},
	}.parse(t)
	want := []string{"+15550000004", "+15550000002", "+15550000003"}
	if r := got[1]; !reflect.DeepEqual(threadMembers(r), want) || !reflect.DeepEqual(rolesOf(r), []string{"member " + want[0], "member " + want[1], "member " + want[2]}) {
		t.Errorf("members %v parties %v, want the rowid order %v", threadMembers(r), rolesOf(got[1]), want)
	}

	// gm-hostile C6: 1,001 members, 1,000 kept
	res, h := fixtureRecords(t, "gm-hostile")
	for _, id := range []int64{111, 112} {
		r := h[id]
		if rawOf(r)["members_total"] != int64(1001) || rawOf(r)["members_cut"] != true || len(threadMembers(r)) != 1000 {
			t.Errorf("message %d: members_total %v members_cut %v members %d", id, rawOf(r)["members_total"], rawOf(r)["members_cut"], len(threadMembers(r)))
		}
	}
	// message 112 is outgoing: its parties are the 1,000 kept members
	if n := len(rolesOf(h[112])); n != 1000 {
		t.Errorf("message 112: %d parties, want 1000", n)
	}
	w := warningsWith(res, "googlemessages-members-cut")
	if len(w) != 1 || w[0] != "sqlite:table=conversation_participants: googlemessages-members-cut: conversation 6: 1001 members, 1000 kept" {
		t.Errorf("members-cut warnings %q", w)
	}
	if r := h[101]; has(r, "members_cut") {
		t.Errorf("a conversation of one member has members_cut")
	}

	// the members of a conversation are read once per job (B3): one conversation_participants match per conversation
	for _, c := range []struct {
		variant string
		convs   int
	}{{"gm-aosp-v2", 5}, {"gm-hostile", 2}} {
		var mu sync.Mutex
		matches := map[string]int{}
		on := func(table string) { mu.Lock(); matches[table]++; mu.Unlock() }
		fx := parsertest.LoadFixture(t, "testdata", c.variant)
		hn := parsertest.NewHarness(t)
		p := countingParser{on: on}
		res := hn.RunLenient(p, inputAt(t, hn, p, remoteApp, fx.Files, false))
		if res.Err != nil {
			t.Fatalf("%s: %v", c.variant, res.Err)
		}
		if got := matches["conversation_participants"]; got != c.convs {
			t.Errorf("%s: %d conversation_participants matches for %d messages, want one per conversation (%d)", c.variant, got, len(byRowid(t, res)), c.convs)
		}
	}
}

// 4I5-P6: a refused budget charge stops the caching; the records are the same, read per message.
func TestGoogleMessagesMemberCacheRefusedChargeKeepsTheRecords(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "gm-aosp-v2")
	run := func(budget *parse.BudgetView) (map[string]int, []records.Record) {
		matches := map[string]int{}
		h := parsertest.NewHarness(t)
		p := countingParser{budget: budget, on: func(table string) { matches[table]++ }}
		res := h.RunLenient(p, inputAt(t, h, p, remoteApp, fx.Files, false))
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		return matches, res.Records
	}
	cachedMatches, cached := run(nil)
	refusedMatches, refused := run(parse.NewBudget(0).View())
	if len(cached) != len(refused) {
		t.Fatalf("%d records cached, %d with the cache refused", len(cached), len(refused))
	}
	for i := range cached {
		if cached[i].Locator != refused[i].Locator || !reflect.DeepEqual(cached[i].Payload, refused[i].Payload) {
			t.Errorf("record %d differs when the cache is refused:\n%v\n%v", i, cached[i].Payload, refused[i].Payload)
			break
		}
	}
	if refusedMatches["conversation_participants"] <= cachedMatches["conversation_participants"] {
		t.Errorf("matches %d with the cache refused, %d cached: the refused cache should read per message", refusedMatches["conversation_participants"], cachedMatches["conversation_participants"])
	}
}

// countingParser runs the live messages mapping of android.googlemessages with its Match calls counted.
type countingParser struct {
	androidgooglemessages.Parser
	budget *parse.BudgetView
	on     func(table string)
}

func (p countingParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	_, err := rowrun.Run(ctx, in, out, []rowrun.Step{{Mapping: androidgooglemessages.CountingMapping(in, p.budget, p.on), Identifying: true}})
	return err
}

// Review focus 4: a member row naming no participants row is counted, never listed and never a group member.
func TestGoogleMessagesDanglingMemberIsStated(t *testing.T) {
	_, got := store{
		participants:  []mrow{person(2, "+15550000002")},
		conversations: []mrow{{1, map[string]any{}}},
		members:       memberRows(1, 2, 999),
		messages:      []mrow{{1, map[string]any{"sender_id": int64(2), "message_status": int64(1)}}},
	}.parse(t)
	r := got[1]
	if !reflect.DeepEqual(threadMembers(r), []string{"+15550000002"}) || rawOf(r)["members_dangling"] != int64(1) || threadMap(r)["group"] != false || rawOf(r)["members_total"] != int64(1) {
		t.Errorf("[P2, 999]: members %v raw %v group %v", threadMembers(r), rawOf(r), threadMap(r)["group"])
	}
	// a conversation with no member row at all is stated none, not a group and not unknown
	_, got = store{
		participants:  []mrow{person(2, "+15550000002")},
		conversations: []mrow{{1, map[string]any{}}},
		messages:      []mrow{{1, map[string]any{"message_status": int64(1), "sender_id": nil}}},
	}.parse(t)
	if r := got[1]; rawOf(r)["members_state"] != "none" || r.Payload["participants_unknown"] != true || strings.Contains(strings.Join(rolesOf(r), ","), "member") {
		t.Errorf("no members: raw %v parties %v", rawOf(r), rolesOf(r))
	}
}
