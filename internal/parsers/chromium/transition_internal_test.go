package chromium

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestTransitionDecoding(t *testing.T) {
	rows := []struct {
		in    int64
		core  string
		quals []string
		left  int64
		ok    bool
	}{
		{838860801, "typed", []string{"from_address_bar", "chain_start", "chain_end"}, 0, true}, // 0x32000001, ascending bit order
		{-1610612736, "link", []string{"chain_end", "server_redirect"}, 0, true},                // 0xA0000000: a server redirect is stored negative
		{-2147483648, "link", []string{"server_redirect"}, 0, true},                             // 0x80000000
		{0, "link", nil, 0, true},
		{42, "unknown", nil, 0, true}, // core 42 is no core transition: unknown, never guessed
		{1 | 0x100 | 0x10000000, "typed", []string{"chain_start"}, 0x100, true},
		{1 << 33, "", nil, 0, false},
		{-(1 << 31) - 1, "", nil, 0, false},
		{1<<32 - 1, "unknown", []string{"blocked", "forward_back", "from_address_bar", "home_page", "from_api", "chain_start", "chain_end", "client_redirect", "server_redirect"}, 0x7FFF00, true},
		{1 << 32, "", nil, 0, false},
	}
	for _, r := range rows {
		core, quals, left, ok := decodeTransition(r.in)
		if ok != r.ok || (ok && (core != r.core || !slices.Equal(quals, r.quals) || left != r.left)) {
			t.Errorf("decodeTransition(%d) = %q %v %d %v; want %q %v %d %v", r.in, core, quals, left, ok, r.core, r.quals, r.left, r.ok)
		}
	}
}

var constRow = regexp.MustCompile(`^\| (transition_core|transition_qualifier) \| (\d+) \| ([a-z_]+) \|`)

// The names the parser knows are exactly the transition rows of the committed cross-check (4F-R27).
func TestTransitionTableFollowsTheCrossCheck(t *testing.T) {
	raw, err := os.ReadFile("../../../tools/fixtures/schemas/chromium/CROSSCHECK.md")
	if err != nil {
		t.Fatal(err)
	}
	core := map[int64]string{}
	qual := map[int64]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		m := constRow.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if m[1] == "transition_core" {
			core[n] = m[3]
		} else {
			qual[n] = m[3]
		}
	}
	if len(core) != 11 || len(qual) != 9 {
		t.Fatalf("the cross-check holds %d core and %d qualifier rows, want 11 and 9", len(core), len(qual))
	}
	for n, name := range core {
		if got, quals, left, ok := decodeTransition(n); !ok || got != name || quals != nil || left != 0 {
			t.Errorf("core %d: %q %v %d %v, the table says %q", n, got, quals, left, ok, name)
		}
	}
	for mask, name := range qual {
		if got, quals, left, ok := decodeTransition(mask); !ok || got != "link" || !slices.Equal(quals, []string{name}) || left != 0 {
			t.Errorf("qualifier %#x: %q %v %d %v, the table says %q", mask, got, quals, left, ok, name)
		}
	}
	// the parser knows no core and no qualifier name the table lacks
	for v := int64(0); v < 256; v++ {
		if got, _, _, _ := decodeTransition(v); got != "unknown" && core[v] != got {
			t.Errorf("core %d decodes to %q, which the table does not list", v, got)
		}
	}
	for bit := 8; bit < 32; bit++ {
		mask := int64(1) << bit
		_, quals, left, _ := decodeTransition(mask)
		if _, listed := qual[mask]; !listed && (len(quals) != 0 || left != mask) {
			t.Errorf("bit %d is not in the table but decodes to %v with leftover %d", bit, quals, left)
		}
	}
}
