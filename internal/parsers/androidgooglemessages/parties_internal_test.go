package androidgooglemessages

import (
	"strconv"
	"testing"
)

// Spec 6.3, B2: the constants of the self rule and of the unresolved contact are the facts of facts.tsv (the self is every
// participant whose sub_id differs from self.other_than_self_sub_id), pinned to their source there.
func TestGoogleMessagesSelfRuleMatchesFacts(t *testing.T) {
	want := map[string]int64{"self.other_than_self_sub_id": otherThanSelfSubID, "contact_id.unresolved": contactUnresolved}
	got := map[string]string{}
	for _, f := range loadFacts(t) {
		if _, ok := want[f.key]; ok {
			got[f.key] = f.value
		}
		if f.key == "self.is_self" && f.value != "sub_id != "+strconv.FormatInt(otherThanSelfSubID, 10) {
			t.Errorf("self.is_self is %q, the Go rule is sub_id != %d", f.value, otherThanSelfSubID)
		}
	}
	for k, w := range want {
		if got[k] != strconv.FormatInt(w, 10) {
			t.Errorf("%s: facts.tsv %q, Go constant %d", k, got[k], w)
		}
	}
}

// Task 5 review I1 (4I5-P6): every cached member set is charged a fixed base, so conversations with no member cannot fill
// the cache uncharged.
func TestGoogleMessagesMemberSetIsChargedABase(t *testing.T) {
	if got := (&memberSet{}).cost(); got != setBaseCost || got <= 0 {
		t.Errorf("an empty member set costs %d, want the base %d", got, setBaseCost)
	}
	s := &memberSet{Unknown: map[string]any{"a": 1}, Joins: []string{"x", "y"}}
	if got, want := s.cost(), int64(setBaseCost+64+32); got != want {
		t.Errorf("cost %d, want %d", got, want)
	}
}
