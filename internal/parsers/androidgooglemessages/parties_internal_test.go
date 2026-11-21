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
