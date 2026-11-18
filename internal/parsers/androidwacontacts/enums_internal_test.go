package androidwacontacts

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The research source metadata the source bar checks against (plan 4I, 4I-R7). It lives in test code only.
// speaksAbout[column] is the set of sources recorded as speaking about that column; dependent groups count once;
// conflict[column] lists the values in a research conflict row.
var (
	speaksAbout = map[string][]string{"is_whatsapp_user": {"S9", "S10"}}
	dependent   = [][]string{{"S4", "S8"}}
	conflict    = map[string][]int64{}
)

func groupOf(src string) string {
	for _, g := range dependent {
		if slices.Contains(g, src) {
			return g[0]
		}
	}
	return src
}

// enumProblems applies the bar of 4I-R7 (the same code as android.whatsapp.msgstore's, restated: a parser imports no
// other parser): every mapped value names a column the research speaks about and cites at least two sources from that
// column's set, distinct after merging the dependent groups; the value is not in a conflict row. A ref:<run> source
// alone meets the bar.
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
			case !slices.Contains(speak, s):
			default:
				distinct[groupOf(s)] = true
			}
		}
		if len(distinct) < 2 && !ref {
			out = append(out, fmt.Sprintf("%s: %d independent source(s) about the column, want 2 (%s)", id, len(distinct), e.Sources))
		}
	}
	return out
}

// 4I-R7: every value the parser maps cites two independent sources about the same column; is_whatsapp_user 1 is the
// only mapped code (S9, S10), and 0 is stored as it is with no label.
func TestWAContactsEnumTableCitesTwoSources(t *testing.T) {
	if p := enumProblems(enumTable); len(p) > 0 {
		t.Fatalf("the enum table breaks the source bar:\n%s", strings.Join(p, "\n"))
	}
	if len(enumTable) != 1 || enumTable[0] != (enumEntry{"is_whatsapp_user", 1, "whatsapp_user", "S9,S10"}) {
		t.Errorf("enum table %+v, want exactly is_whatsapp_user 1 = whatsapp_user (S9, S10)", enumTable)
	}
	for _, v := range []int64{0, 2, -1} {
		if l, ok := lookupEnum("is_whatsapp_user", v); ok {
			t.Errorf("lookupEnum(is_whatsapp_user, %d) = %q: not supported by two sources", v, l)
		}
	}
	if l, ok := lookupEnum("is_whatsapp_user", 1); !ok || l != "whatsapp_user" {
		t.Errorf("lookupEnum(is_whatsapp_user, 1) = %q, %v", l, ok)
	}
}

// The bar itself refuses what it must (the self-test of the check).
func TestWAContactsEnumBarRefusesWhatItMust(t *testing.T) {
	for name, e := range map[string]enumEntry{
		"dependent sources count once":    {"is_whatsapp_user", 1, "x", "S4,S8"},
		"a source about another column":   {"is_whatsapp_user", 1, "x", "S16,S9"},
		"a column no source speaks about": {"status", 1, "x", "S9,S10"},
		"one source":                      {"is_whatsapp_user", 1, "x", "S9"},
	} {
		if p := enumProblems([]enumEntry{e}); len(p) != 1 {
			t.Errorf("%s: %+v gave %v, want one problem", name, e, p)
		}
	}
	if p := enumProblems([]enumEntry{{"is_whatsapp_user", 2, "x", "ref:android11-2026-01-01"}}); len(p) != 0 {
		t.Errorf("a reference run alone gave %v, want none", p)
	}
}
