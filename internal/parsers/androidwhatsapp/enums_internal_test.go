package androidwhatsapp

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The research source metadata the source bar checks against (plan 4I, 4I-R7). It lives in test code only.
//
// speaksAbout[column] is the set of sources recorded as speaking about that column. dependent groups count once:
// {S4, S8}. S5 and S6 are dependent for table layout only, never for values. selfContradicting[column][value] lists
// the sources not counted for that value. conflict[column] lists the values in a research conflict row.
var (
	speaksAbout = map[string][]string{
		"message_type":   {"S1", "S3", "S4", "S5", "S6", "S8", "S11"},
		"media_wa_type":  {"S9", "S10"},
		"from_me":        {"S1", "S4", "S8", "S11"},
		"key_from_me":    {"S1", "S9", "S10"},
		"action_type":    {"S1", "S5", "S6"},
		"call_result":    {"S1", "S6"},
		"video_call":     {"S1", "S4", "S8"},
		"call_from_me":   {"S1", "S4", "S8"},
		"add_on_type":    {"S5", "S6", "S11"},
		"is_whatsapp":    {"S9", "S10"},
		"status":         nil,
		"legacy_status":  nil,
		"add_on_from_me": {"S5"},
	}
	dependent         = [][]string{{"S4", "S8"}}
	selfContradicting = map[string]map[int64][]string{
		"action_type": {1: {"S6"}, 4: {"S6"}, 5: {"S6"}, 6: {"S6"}, 10: {"S6"}, 11: {"S6"}, 12: {"S6"}, 13: {"S6"}, 14: {"S6"}, 15: {"S6"}, 20: {"S6"}, 27: {"S6"}, 28: {"S6"}, 56: {"S6"}, 58: {"S6"}},
	}
	conflict = map[string][]int64{
		"message_type": {11, 26, 46, 66},
		"action_type":  {1, 67, 69},
		"call_result":  {0, 4, 7},
		"status":       {0, 4, 5, 6},
	}
)

// groupOf merges the dependent sources into one representative.
func groupOf(src string) string {
	for _, g := range dependent {
		if slices.Contains(g, src) {
			return g[0]
		}
	}
	return src
}

// enumProblems applies the bar of 4I-R7 to a table: every mapped value names a column the research speaks about and
// cites at least two sources from that column's set, distinct after merging the dependent groups, none of them
// contradicting itself about the value; the value is not in a conflict row. A ref:<run> source alone meets the bar.
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
				// a source that does not speak about this column does not count
			case slices.Contains(selfContradicting[e.Column][e.Value], s):
				// a source that contradicts itself about this value does not count
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

// 4I-R7, R4: every value the parser maps cites two independent sources about the same column.
func TestWhatsAppEnumTablesCiteTwoSources(t *testing.T) {
	if p := enumProblems(enumTable); len(p) > 0 {
		t.Fatalf("the enum table breaks the source bar:\n%s", strings.Join(p, "\n"))
	}
	// the columns of this task appear; the delivery status columns have no entry at all (R4)
	have := map[string]bool{}
	for _, e := range enumTable {
		have[e.Column] = true
		if e.Column == "status" {
			t.Errorf("the status column has an entry: %+v", e)
		}
	}
	for _, c := range []string{"message_type", "media_wa_type", "from_me", "key_from_me"} {
		if !have[c] {
			t.Errorf("no entry for column %s", c)
		}
	}
	for _, e := range enumTable {
		if l, ok := lookupEnum(e.Column, e.Value); !ok || l != e.Label {
			t.Errorf("lookupEnum(%s, %d) = %q, %v; the table says %q", e.Column, e.Value, l, ok, e.Label)
		}
	}
	// codes the research does not support are not mapped, whatever the neighbour column says
	for _, c := range []struct {
		col string
		v   int64
	}{{"message_type", 10}, {"message_type", 11}, {"message_type", 14}, {"message_type", 999}, {"media_wa_type", 7}, {"media_wa_type", 15}, {"status", 5}, {"from_me", 2}} {
		if l, ok := lookupEnum(c.col, c.v); ok {
			t.Errorf("lookupEnum(%s, %d) = %q: not supported by two sources", c.col, c.v, l)
		}
	}
}

// The bar itself refuses what it must (the self-test of the check).
func TestWhatsAppEnumBarRefusesWhatItMust(t *testing.T) {
	for name, e := range map[string]enumEntry{
		"dependent sources count once":      {"message_type", 3, "video", "S4,S8"},
		"a source about another column":     {"message_type", 3, "video", "S16,S6"},
		"a conflict value":                  {"message_type", 11, "x", "S6,S3"},
		"a source that contradicts itself":  {"action_type", 11, "group_created", "S1,S6"},
		"a column no source speaks about":   {"status", 5, "sent", "S9,S10"},
		"a legacy value from modern column": {"media_wa_type", 15, "revoked", "S1,S3"},
		"one source":                        {"message_type", 3, "video", "S8"},
	} {
		if p := enumProblems([]enumEntry{e}); len(p) != 1 {
			t.Errorf("%s: %+v gave %v, want one problem", name, e, p)
		}
	}
	for name, e := range map[string]enumEntry{
		"two independent sources": {"message_type", 3, "video", "S8,S6"},
		"a reference run alone":   {"media_wa_type", 7, "x", "ref:android11-2026-01-01"},
	} {
		if p := enumProblems([]enumEntry{e}); len(p) != 0 {
			t.Errorf("%s: %+v gave %v, want none", name, e, p)
		}
	}
}

func TestStrictDecimal(t *testing.T) {
	for in, want := range map[string]struct {
		v  int64
		ok bool
	}{
		"0": {0, true}, "1": {1, true}, "-1": {-1, true}, "15": {15, true}, "999999999999999999": {999999999999999999, true},
		"": {}, " 1": {}, "1 ": {}, "01": {}, "+1": {}, "1.0": {}, "x": {}, "-0": {}, "-": {}, "1000000000000000000": {},
	} {
		if v, ok := strictDecimal([]byte(in)); ok != want.ok || v != want.v {
			t.Errorf("strictDecimal(%q) = %d, %v; want %d, %v", in, v, ok, want.v, want.ok)
		}
	}
}
