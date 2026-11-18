package ioswhatsapp

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The research source metadata the source bar checks against (plan 4I2, 4I2-R8, "Enum, source and unit tables"). It lives in
// test code only. speaksAbout[column] is the set of sources recorded as speaking about that column; conflict[column]
// lists the values in a research conflict row. There are no dependent source groups for these columns.
var (
	speaksAbout = map[string][]string{
		"ZMESSAGETYPE":    {"S1", "S7", "S12", "S16", "S17", "S18", "S20", "S21", "S22"},
		"ZGROUPEVENTTYPE": {"S16", "S17", "S18"},
		"ZMESSAGESTATUS":  {"S18", "S20", "S24"},
		"ZISFROMME":       {"S1", "S7", "S10", "S12", "S19"},
		"ZSESSIONTYPE":    {"S12", "S16"},
	}
	conflict = map[string][]int64{
		"ZGROUPEVENTTYPE": {1, 9, 12},
	}
)

// enumProblems applies the bar of 4I2-R8 to a table: every mapped value names a column the research speaks about and
// cites at least two distinct sources from that column's set; the value is not in a conflict row. A ref:<run> source alone
// meets the bar.
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

// 4I2-R8: every value the parser maps cites two distinct sources about the same column.
func TestIOSWhatsAppChatsEnumTableCitesTwoSources(t *testing.T) {
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
		if !have[c] {
			t.Errorf("no entry for column %s", c)
		}
	}
	// codes the research does not support, or states in conflict, are not mapped
	for _, c := range []struct {
		col string
		v   int64
	}{
		{"ZMESSAGETYPE", 9},
		{"ZMESSAGETYPE", 12},
		{"ZMESSAGETYPE", 13},
		{"ZMESSAGETYPE", 38},
		{"ZMESSAGETYPE", 39},
		{"ZMESSAGETYPE", 40},
		{"ZMESSAGETYPE", 999},
		{"ZGROUPEVENTTYPE", 1},
		{"ZGROUPEVENTTYPE", 9},
		{"ZGROUPEVENTTYPE", 12},
		{"ZMESSAGESTATUS", 0},
		{"ZMESSAGESTATUS", 1},
		{"ZMESSAGESTATUS", 2},
		{"ZMESSAGESTATUS", 3},
		{"ZMESSAGESTATUS", 5},
		{"ZISFROMME", 2},
	} {
		if l, ok := lookupEnum(c.col, c.v); ok {
			t.Errorf("lookupEnum(%s, %d) = %q: not supported by two sources", c.col, c.v, l)
		}
	}
	// the mapped values the rulings name
	for _, c := range []struct {
		col, want string
		v         int64
	}{
		{"ZMESSAGETYPE", "video", 2},
		{"ZMESSAGETYPE", "audio", 3},
		{"ZMESSAGETYPE", "revoked", 14},
		{"ZGROUPEVENTTYPE", "participant_added", 2},
		{"ZMESSAGESTATUS", "delivered", 6},
		{"ZMESSAGESTATUS", "read_by_recipient", 8},
		{"ZISFROMME", "out", 1},
		{"ZSESSIONTYPE", "status", 3},
	} {
		if l, ok := lookupEnum(c.col, c.v); !ok || l != c.want {
			t.Errorf("lookupEnum(%s, %d) = %q, %v, want %q", c.col, c.v, l, ok, c.want)
		}
	}
}

// The bar itself refuses what it must (the self-test of the check).
func TestIOSWhatsAppChatsEnumBarRefusesWhatItMust(t *testing.T) {
	for name, e := range map[string]enumEntry{
		"one source":                      {"ZMESSAGETYPE", 3, "audio", "S12"},
		"a source twice":                  {"ZMESSAGETYPE", 3, "audio", "S12,S12"},
		"a conflict value":                {"ZGROUPEVENTTYPE", 12, "x", "S16,S17"},
		"a source about another column":   {"ZGROUPEVENTTYPE", 2, "x", "S16,S12"},
		"a column no source speaks about": {"ZOUTCOME", 0, "answered", "S1,S7"},
		"a wrong-column entry":            {"ZMESSAGESTATUS", 6, "delivered", "S12,S16"},
	} {
		if p := enumProblems([]enumEntry{e}); len(p) != 1 {
			t.Errorf("%s: %+v gave %v, want one problem", name, e, p)
		}
	}
	for name, e := range map[string]enumEntry{
		"two sources":           {"ZMESSAGETYPE", 3, "audio", "S12,S16"},
		"a reference run alone": {"ZMESSAGETYPE", 9, "x", "ref:ios-2026-01-01"},
	} {
		if p := enumProblems([]enumEntry{e}); len(p) != 0 {
			t.Errorf("%s: %+v gave %v, want none", name, e, p)
		}
	}
}
