package apfstest

import "testing"

// The builder's name hash reproduces the hashes a real mkapfs container stores
// for its two directory records (see the oracle: 2989177 and 2828707).
func TestNameHashMatchesRealContainer(t *testing.T) {
	for name, want := range map[string]uint32{"root": 2989177, "private-dir": 2828707} {
		if got := NameHash([]byte(name), true); got != want {
			t.Errorf("NameHash(%q) = %d, want %d", name, got, want)
		}
	}
	if NameHash([]byte("Root"), true) != NameHash([]byte("root"), true) {
		t.Error("case-insensitive hash differs by case")
	}
}
