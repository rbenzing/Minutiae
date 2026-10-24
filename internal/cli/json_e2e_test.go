package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// e2eHostile holds a bidi override, NEL, a bidi isolate, a right-to-left mark, a tag character and an
// escape sequence: runes that are not graphic and must never reach a terminal raw.
const e2eHostile = "\u202e\u0085\u2066\u200f\U000e0001\x1b[31m"

// requireTerminalSafe fails when out holds a control character other than the newline, U+007F or a
// non-graphic rune at or above U+0080.
func requireTerminalSafe(t *testing.T, what, out string) {
	t.Helper()
	for _, r := range out {
		if r == '\n' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && !unicode.IsGraphic(r)) {
			t.Errorf("%s: raw U+%04X in the output", what, r)
			return
		}
	}
}

// jsonStrings decodes out (one JSON value) and returns every string in it, keys included.
func jsonStrings(t *testing.T, what, out string) map[string]bool {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%s: not JSON: %v\n%s", what, err, out)
	}
	got := map[string]bool{}
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case string:
			got[x] = true
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				got[k] = true
				walk(e)
			}
		}
	}
	walk(v)
	return got
}

func TestJSONOutputIsTerminalSafeEndToEnd(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "dir\u202eevil", make([]byte, recordstest.ArtifactSize))
	summary := "alpha " + e2eHostile
	body := "beta " + e2eHostile
	recordstest.Ingest(t, c, recParser, []string{art.ID}, []records.Record{
		{Type: "note", ArtifactID: art.ID, Summary: summary, Body: body, Payload: map[string]any{}},
		{Type: "note", ArtifactID: art.ID, Summary: "alpha plain", Payload: map[string]any{}},
	})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	hostileType := "t" + e2eHostile
	recordstest.SetRecordColumn(t, dir, 1, "type", hostileType)
	recordstest.SetRecordColumn(t, dir, 1, "payload", "{\"note\":\"x\u202ey\"}")

	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"list", []string{"records", "list", "--case", dir}, []string{summary, hostileType}},
		{"show", []string{"records", "show", "--case", dir, "1", "--payload"}, []string{summary, body, hostileType, "x\u202ey"}},
		{"search", []string{"records", "search", "--case", dir, "alpha"}, []string{summary}},
		{"stats", []string{"records", "stats", "--case", dir, "--by", "type"}, []string{hostileType}},
	} {
		code, out, errs := runSplit(t.Context(), t, Deps{}, append([]string{"--json"}, tc.args...)...)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s\n%s", tc.name, code, out, errs)
		}
		requireTerminalSafe(t, tc.name, out)
		got := jsonStrings(t, tc.name, out)
		for _, w := range tc.want {
			if !got[w] {
				t.Errorf("%s: the decoded output lacks the stored string %q", tc.name, w)
			}
		}
	}

	// a listing of a filesystem entry named with a bidi override and NEL
	cs := newCLICase(t)
	d := Deps{FSDrivers: mtfsDrivers}
	name := "a\u202eb\u0085c"
	ref := importRaw(t, cs, hostileDisk(t, map[string]any{"block_size": 512, "label": "x", "entries": []map[string]any{
		{"id": "1", "parent_id": "", "name": "", "type": "dir"},
		{"id": "2", "parent_id": "1", "name": name, "type": "file"},
	}}))
	code, out, errs := runSplit(t.Context(), t, d, "--json", "image", "ls", "-r", "--case", cs, ref)
	if code != 0 {
		t.Fatalf("image ls: exit %d\n%s\n%s", code, out, errs)
	}
	requireTerminalSafe(t, "image ls", out)
	found := false
	for s := range jsonStrings(t, "image ls", out) {
		found = found || strings.HasSuffix(s, name)
	}
	if !found {
		t.Errorf("image ls: no decoded string ends in %q:\n%s", name, out)
	}
}
