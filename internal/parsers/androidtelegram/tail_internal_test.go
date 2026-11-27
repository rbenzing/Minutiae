package androidtelegram

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/tl"
)

// idOf is the constructor ID of the table row named name.
func idOf(t *testing.T, tab tl.Table, name string) uint32 {
	t.Helper()
	for k, c := range tab {
		if c.Name == name {
			return k.ID
		}
	}
	t.Fatalf("no constructor %s in the table", name)
	return 0
}

// Plan 4I4b Task 6 review m2 (unknown is never an empty value): a boxed TL_reactionEmoji whose emoticon was not read
// (the decode stopped inside it) states no reaction, not "emoji:".
func TestReactionOfStatesNoEmptyEmoji(t *testing.T) {
	tab := newTable()
	v := newView(tl.Result{Fields: []tl.Named{{Path: "r", Kind: tl.KObject, ID: idOf(t, tab, "TL_reactionEmoji"), Type: "Reaction"}}})
	v.tab = tab
	if s, ok := reactionOf(v, "r", map[string]any{}); ok || s != "" {
		t.Errorf("reactionOf with no emoticon read = %q, %v; want no reaction", s, ok)
	}
}

// Plan 4I4b Task 6 review m3 (B8): sameReplies compares decoded values by an explicit kind switch; a difference of any
// kind, a key on one side only, a list's length or an item, and a different _total all differ.
func TestSameRepliesComparesEveryShapeDifference(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"replies": int64(3), "comments": true, "channel": "channel:5", "recent_repliers": []any{"user:1", "user:2"}, "recent_repliers_total": int64(2)}
	}
	with := func(f func(m map[string]any)) map[string]any { m := base(); f(m); return m }
	for _, c := range []struct {
		name string
		a, b map[string]any
		want bool
	}{
		{"equal", base(), base(), true},
		{"empty", map[string]any{}, map[string]any{}, true},
		{"integer", base(), with(func(m map[string]any) { m["replies"] = int64(4) }), false},
		{"string", base(), with(func(m map[string]any) { m["channel"] = "channel:6" }), false},
		{"boolean", base(), with(func(m map[string]any) { m["comments"] = false }), false},
		{"key missing on one side", base(), with(func(m map[string]any) { delete(m, "comments") }), false},
		{"key missing on the other side", with(func(m map[string]any) { delete(m, "channel") }), base(), false},
		{"list length", base(), with(func(m map[string]any) { m["recent_repliers"] = []any{"user:1"} }), false},
		{"list item", base(), with(func(m map[string]any) { m["recent_repliers"] = []any{"user:1", "user:3"} }), false},
		{"list against scalar", base(), with(func(m map[string]any) { m["recent_repliers"] = "user:1" }), false},
		{"total", base(), with(func(m map[string]any) { m["recent_repliers_total"] = int64(3) }), false},
		{"kind of integer", base(), with(func(m map[string]any) { m["replies"] = "3" }), false},
	} {
		if got := sameReplies(c.a, c.b); got != c.want {
			t.Errorf("%s: sameReplies = %v, want %v", c.name, got, c.want)
		}
	}
}
