package ioswacalls

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The research source metadata the source bar checks against (plan 4I2, 4I2-R8, "Enum, source and unit tables"). It lives in
// test code only. speaksAbout[column] is the set of sources recorded as speaking about that column; conflict[column] lists the
// values in a research conflict row. ZVIDEO is listed because sources speak about the column, but no source states the meaning
// of a value (PF6, amendment 4I2-A3), so it must have NO entry in the table.
var (
	speaksAbout = map[string][]string{
		"ZOUTCOME":  {"S1", "S7"},
		"ZVIDEO":    {"S1", "S7", "S12", "S18"},
		"ZINCOMING": {"S1", "S7"},
	}
	conflict = map[string][]int64{
		"ZOUTCOME": {4},
	}
	unmappedByDesign = map[string]bool{"ZVIDEO": true}
)

// enumProblems applies the bar of 4I2-R8 to a table: every mapped value names a column the research speaks about and cites at
// least two distinct sources from that column's set; the value is not in a conflict row. A ref:<run> source alone meets the bar.
func enumProblems(table []enumEntry) []string {
	var out []string
	for _, e := range table {
		id := fmt.Sprintf("%s=%d", e.Column, e.Value)
		speak, known := speaksAbout[e.Column]
		if !known || len(speak) == 0 {
			out = append(out, id+": no source speaks about this column")
			continue
		}
		if slices.Contains(conflict[e.Column], e.Value) {
			out = append(out, id+": the value is in a research conflict row")
			continue
		}
		distinct := map[string]bool{}
		ref := false
		for _, s := range strings.Split(e.Sources, ",") {
			switch {
			case strings.HasPrefix(s, "ref:"):
				ref = true
			case slices.Contains(speak, s): // a source that does not speak about this column does not count
				distinct[s] = true
			}
		}
		if len(distinct) < 2 && !ref {
			out = append(out, fmt.Sprintf("%s: %d independent source(s) about the column, want 2 (%s)", id, len(distinct), e.Sources))
		}
	}
	return out
}

// 4I2-R8, PF6: every value the parser maps cites two distinct sources about the same column; ZVIDEO maps nothing.
func TestIOSWhatsAppCallsEnumTableCitesTwoSources(t *testing.T) {
	if len(enumTable) == 0 {
		t.Fatal("the enum table is empty")
	}
	if p := enumProblems(enumTable); len(p) > 0 {
		t.Fatalf("the enum table breaks the source bar:\n%s", strings.Join(p, "\n"))
	}
	have := map[string]bool{}
	for _, e := range enumTable {
		have[e.Column] = true
		if l, ok := lookupEnum(e.Column, e.Value); !ok || l != e.Label {
			t.Errorf("lookupEnum(%s, %d) = %q, %v; the table says %q", e.Column, e.Value, l, ok, e.Label)
		}
	}
	for c := range speaksAbout {
		if have[c] == unmappedByDesign[c] {
			t.Errorf("column %s: has entries %v, but unmapped by design is %v", c, have[c], unmappedByDesign[c])
		}
	}
	// codes the research does not support, or states in conflict, are not mapped: ZVIDEO 0 and 1 included (no per-value source)
	for _, c := range []struct {
		col string
		v   int64
	}{
		{"ZOUTCOME", 2},
		{"ZOUTCOME", 4},
		{"ZOUTCOME", 99},
		{"ZOUTCOME", -1},
		{"ZVIDEO", 0},
		{"ZVIDEO", 1},
		{"ZVIDEO", 2},
		{"ZINCOMING", 2},
		{"ZINCOMING", 7},
		{"ZINCOMING", -1},
	} {
		if l, ok := lookupEnum(c.col, c.v); ok {
			t.Errorf("lookupEnum(%s, %d) = %q: not supported by two sources", c.col, c.v, l)
		}
	}
	for _, c := range []struct {
		col, want string
		v         int64
	}{
		{"ZOUTCOME", "answered", 0}, {"ZOUTCOME", "missed", 1}, {"ZINCOMING", "out", 0}, {"ZINCOMING", "in", 1},
	} {
		if l, ok := lookupEnum(c.col, c.v); !ok || l != c.want {
			t.Errorf("lookupEnum(%s, %d) = %q, %v, want %q", c.col, c.v, l, ok, c.want)
		}
	}
	if len(enumTable) != 4 {
		t.Errorf("%d entries, want exactly the four codes of ZOUTCOME and ZINCOMING", len(enumTable))
	}
}

// The bar itself refuses what it must (the self-test of the check).
func TestIOSWhatsAppCallsEnumBarRefusesWhatItMust(t *testing.T) {
	for name, e := range map[string]enumEntry{
		"one source":                      {"ZOUTCOME", 0, "answered", "S1"},
		"a source twice":                  {"ZOUTCOME", 0, "answered", "S1,S1"},
		"a conflict value":                {"ZOUTCOME", 4, "x", "S1,S7"},
		"a source about another column":   {"ZINCOMING", 1, "in", "S1,S12"},
		"a column no source speaks about": {"ZMISSED", 0, "x", "S1,S7"},
	} {
		if p := enumProblems([]enumEntry{e}); len(p) != 1 {
			t.Errorf("%s: %+v gave %v, want one problem", name, e, p)
		}
	}
	for name, e := range map[string]enumEntry{
		"two sources":           {"ZOUTCOME", 0, "answered", "S1,S7"},
		"a reference run alone": {"ZVIDEO", 1, "video", "ref:ios-2026-01-01"},
	} {
		if p := enumProblems([]enumEntry{e}); len(p) != 0 {
			t.Errorf("%s: %+v gave %v, want none", name, e, p)
		}
	}
}
