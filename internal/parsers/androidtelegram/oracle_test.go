package androidtelegram_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// Spec 13 (call 8), F4: every variant with records parses to exactly the message records, warnings and notes of its
// literal oracle (the message tables; users is Task 12's), with no contract violation, once pendingOracle has taken
// out what later tasks map. No variant is dropped.
func TestAndroidTelegramMatchesOracle(t *testing.T) {
	ran := 0
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Expect.Records) == 0 {
			continue // refused (version band, no message table): the probe and parse tests state those
		}
		ran++
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := fx.Bundle(h, remoteApp)
			p := androidtelegram.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			res.Records = dropPendingCalls(t, v, res.Records)
			parsertest.AssertMatchesOracle(t, res, b, pendingOracle(fx.Expect), "messages", "messages_v2")
		})
	}
	if ran != 9 {
		t.Errorf("%d variants with records, want 9", ran)
	}
}

// pendingCallRows are the phone-call service rows (R11-R17, P6, and the layers' service rows that carry a phone-call
// action), which become call records in Task 11: until then they are dropped from both sides (F4), by variant and
// "<table>/<rowid>".
func pendingCallRows() map[string][]string {
	return map[string][]string{
		"telegram-v181": {
			"messages_v2/11", "messages_v2/12", "messages_v2/13", "messages_v2/14", "messages_v2/15", "messages_v2/16",
			"messages_v2/17",
		},
		"telegram-v159": {"messages_v2/11"},
		"telegram-v64":  {"messages/107"},
		"telegram-layers": {
			"messages_v2/1003", "messages_v2/1007", "messages_v2/1008", "messages_v2/1022", "messages_v2/1033",
			"messages_v2/1034", "messages_v2/1042", "messages_v2/1046",
		},
	}
}

// dropPendingCalls removes the records of the pending call rows from a run (the other side of pendingOracle's drop).
func dropPendingCalls(t *testing.T, variant string, recs []records.Record) []records.Record {
	t.Helper()
	rows := pendingCallRows()[variant]
	var out []records.Record
	dropped := 0
	for _, r := range recs {
		if slices.Contains(rows, locatorRow(r.Locator)) {
			dropped++
			continue
		}
		out = append(out, r)
	}
	if dropped != len(rows) {
		t.Errorf("%s: %d records of the %d pending call rows", variant, dropped, len(rows))
	}
	return out
}

// locatorRow is "<table>/<rowid>" of a live locator "sqlite:table=<t>;rowid=<n>[;...]".
func locatorRow(loc string) string {
	rest, _ := strings.CutPrefix(loc, "sqlite:table=")
	table, rest, _ := strings.Cut(rest, ";rowid=")
	id, _, _ := strings.Cut(rest, ";")
	return table + "/" + id
}

// The raw keys of later tasks (F4): each later task deletes its entries here, and Task 12 deletes pendingOracle.
func pendingRawKeys() []string {
	return []string{
		// Task 9: media summary, forward header, trailer, reply copy
		"media", "fwd", "fwd_msg_id", "trailer_state", "trailer_len", "trailer_sha256", "attach_path", "attach_path_state",
		"attach_path_flags", "attach_params", "attach_params_total", "attach_params_truncated", "reply_copy",
		// Task 10: peers, participants, thread joins, secret chats
		"peer_id", "sender_state", "sender_basis", "counterparty_state", "thread_title_state", "thread_user_state",
		"join_undecided", "secret_chat", "dialog_class_state",
	}
}

// pendingRawPrefixes are the raw key prefixes of later tasks: the reply copy's blob keys (Task 9) and the joined
// chats blob's (Task 10).
func pendingRawPrefixes() []string { return []string{"replydata_", "chat_data_"} }

// pendingOracle is the interim filter of TestAndroidTelegramMatchesOracle (F4): a copy of exp without what Tasks 9-12
// map. (a) The raw keys of pendingRawKeys and pendingRawPrefixes are deleted, and so is the payload's attachments
// (Task 9); raw.enc_chat_id is kept only on a pre-84 high-id secret row (raw.dialog_basis, F10), and a dialog class
// the chats join or the peer resolves (chat, channel, community) is the interim "negative" (Task 10). (b) The
// participants are the interim unknown ones and the thread is its id alone (Task 10); Summary is not compared
// (Task 10 names the counterparty). (c) The call records of the pending call rows are dropped (Task 11), as
// dropPendingCalls drops them from the run. The users.data warning is Task 12's. No variant is dropped.
func pendingOracle(exp parsertest.Expect) parsertest.Expect {
	var out parsertest.Expect
	bs, err := json.Marshal(exp)
	if err != nil {
		panic(err)
	}
	dec := json.NewDecoder(bytes.NewReader(bs))
	dec.UseNumber() // ids above 2^53 (a pre-84 packed mid) stay exact
	if err := dec.Decode(&out); err != nil {
		panic(err)
	}
	calls := pendingCallRows()[exp.Variant]
	recs := out.Records[:0]
	for _, r := range out.Records {
		if r.Table == "users" {
			recs = append(recs, r) // not compared: AssertMatchesOracle is given the message tables only
			continue
		}
		if slices.Contains(calls, r.Table+"/"+itoa(r.Rowid)) {
			continue
		}
		r.Summary = nil
		pendingPayload(r.Payload)
		recs = append(recs, r)
	}
	out.Records = recs
	warns := out.Warnings[:0]
	for _, w := range out.Warnings {
		if w.Table == "users" || w.Locator == "sqlite:table=users" {
			continue
		}
		warns = append(warns, w)
	}
	out.Warnings = warns
	return out
}

// pendingPayload rewrites one message payload to the interim form of Task 8.
func pendingPayload(p map[string]any) {
	delete(p, "attachments")
	p["participants"] = []any{}
	p["participants_unknown"] = true
	if th, ok := p["thread"].(map[string]any); ok {
		p["thread"] = map[string]any{"id": th["id"]}
	}
	raw, _ := p["raw"].(map[string]any)
	for _, k := range pendingRawKeys() {
		delete(raw, k)
	}
	for k := range raw {
		for _, pre := range pendingRawPrefixes() {
			if strings.HasPrefix(k, pre) {
				delete(raw, k)
			}
		}
	}
	if _, pre84 := raw["dialog_basis"]; !pre84 {
		delete(raw, "enc_chat_id")
	}
	switch raw["dialog_class"] {
	case "chat", "channel", "community":
		raw["dialog_class"] = "negative"
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
