package androidtelegram_test

import (
	"maps"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 13 (call 8), F4, every variant with records parses to exactly the message, call and contact records, warnings
// and notes of its literal oracle, with no contract violation (the users table included, Task 12). The phone-call
// rows compare as call records (Task 11). No variant is dropped.
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
			parsertest.AssertMatchesOracle(t, res, b, pendingOracle(fx.Expect))
		})
	}
	if ran != 9 {
		t.Errorf("%d variants with records, want 9", ran)
	}
}

// pendingOracle is exp without the keys of the plan 4I4c mapping the Go parser does not state yet (plan 4I4c Task 5:
// the oracle restates the Stage B "Raw shapes" before Tasks 6 and 7 map them). It never drops a variant, a record or
// any other key. Markup part (Task 6 deletes it): raw.reply_markup, reply_markup_state, reply_markup_buttons, the
// text-flag names reply_markup.* and reply_markup_buttons.*, and reply_markup in data_undecided (the key removed when
// it empties). Poll and mention part (Task 7 deletes it and the function): raw.poll, poll_answers, poll_results,
// poll_results_options, poll_voters with their _state, _len, _sha256, _unit, _total and _truncated siblings, the
// text-flag names poll.*, poll_answers.* and poll_results.*, and in each raw.entities[i] user (when user_class is set),
// user_from_msg_id, user_from_peer, user_from_peer_class and user_access_hash_present.
func pendingOracle(exp parsertest.Expect) parsertest.Expect {
	out := exp
	out.Records = make([]parsertest.ExpRecord, len(exp.Records))
	for i, r := range exp.Records {
		out.Records[i] = r
		raw, ok := r.Payload["raw"].(map[string]any)
		if !ok {
			continue
		}
		payload := maps.Clone(r.Payload)
		raw = maps.Clone(raw)
		payload["raw"] = raw
		out.Records[i].Payload = payload
		for _, k := range []string{"reply_markup", "reply_markup_state", "reply_markup_buttons"} {
			delete(raw, k)
		}
		for _, base := range []string{"poll", "poll_answers", "poll_results", "poll_results_options", "poll_voters"} {
			for _, suffix := range []string{"", "_state", "_len", "_sha256", "_unit", "_total", "_truncated"} {
				delete(raw, base+suffix)
			}
		}
		pruneList(raw, "text_flags", func(name string) bool {
			for _, p := range []string{"reply_markup.", "reply_markup_buttons.", "poll.", "poll_answers.", "poll_results."} {
				if strings.HasPrefix(name, p) {
					return true
				}
			}
			return false
		})
		pruneList(raw, "data_undecided", func(name string) bool { return name == "reply_markup" })
		if ents, ok := raw["entities"].([]any); ok {
			pruned := make([]any, len(ents))
			for j, e := range ents {
				m, ok := e.(map[string]any)
				if !ok {
					pruned[j] = e
					continue
				}
				m = maps.Clone(m)
				if _, has := m["user_class"]; has {
					delete(m, "user")
				}
				for _, k := range []string{"user_from_msg_id", "user_from_peer", "user_from_peer_class", "user_access_hash_present"} {
					delete(m, k)
				}
				pruned[j] = m
			}
			raw["entities"] = pruned
		}
	}
	return out
}

// pruneList drops the names drop selects from the comma-joined string raw[key], and the key when none is left.
func pruneList(raw map[string]any, key string, drop func(string) bool) {
	s, ok := raw[key].(string)
	if !ok {
		return
	}
	var keep []string
	for _, name := range strings.Split(s, ",") {
		if !drop(name) {
			keep = append(keep, name)
		}
	}
	if len(keep) == 0 {
		delete(raw, key)
		return
	}
	raw[key] = strings.Join(keep, ",")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// locatorRow is "<table>/<rowid>" of a live locator "sqlite:table=<t>;rowid=<n>[;...]".
func locatorRow(loc string) string {
	rest, _ := strings.CutPrefix(loc, "sqlite:table=")
	table, rest, _ := strings.Cut(rest, ";rowid=")
	id, _, _ := strings.Cut(rest, ";")
	return table + "/" + id
}
