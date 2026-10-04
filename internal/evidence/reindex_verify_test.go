package evidence

import (
	"slices"
	"strings"
	"testing"
)

// TestDanglingReindexes: which announced reindexes the audit log leaves unconcluded. An error entry
// concludes the reindex it names, a done entry concludes the reindex it names and every earlier
// announcement (a finished rebuild supersedes a cut-short one), an entry that does not decode is a
// problem and concludes nothing, and a conclusion that names no open announcement (none ever, another
// id, or a reindex already concluded) is a problem and concludes nothing.
func TestDanglingReindexes(t *testing.T) {
	start := func(seq int64, id string) AuditEntry {
		return AuditEntry{Seq: seq, Action: ActionReindex, Details: ReindexStart{ReindexID: id, Tables: FTSTables()}.Details()}
	}
	failed := func(seq int64, id string) AuditEntry {
		return AuditEntry{Seq: seq, Action: ActionReindexError, Details: ReindexFailure{ReindexID: id, Error: "x"}.Details()}
	}
	done := func(seq int64, id string) AuditEntry {
		return AuditEntry{Seq: seq, Action: ActionReindexDone, Details: ReindexDone{ReindexID: id, Docs: map[string]int64{}}.Details()}
	}
	ids := func(ds []danglingReindex) []string {
		out := []string{}
		for _, d := range ds {
			out = append(out, d.id)
		}
		return out
	}
	const orphan = "which no earlier records.reindex announced, or which was already concluded"
	tests := []struct {
		name     string
		entries  []AuditEntry
		want     []string
		problems []string // one substring per expected problem, in order
	}{
		{"none", nil, []string{}, nil},
		{"concluded by done", []AuditEntry{start(1, "a"), done(2, "a")}, []string{}, nil},
		{"concluded by error", []AuditEntry{start(1, "a"), failed(2, "a")}, []string{}, nil},
		{"announced only", []AuditEntry{start(1, "a")}, []string{"a"}, nil},
		{
			"an error of another id is an orphan and concludes nothing",
			[]AuditEntry{start(1, "a"), failed(2, "b")},
			[]string{"a"},
			[]string{`audit seq 2: records.reindex.error names reindex "b", ` + orphan},
		},
		{
			"a done of another id is an orphan and concludes nothing",
			[]AuditEntry{start(1, "a"), done(2, "b")},
			[]string{"a"},
			[]string{`audit seq 2: records.reindex.done names reindex "b", ` + orphan},
		},
		{
			"a done with no announcement",
			[]AuditEntry{done(1, "zzz")},
			[]string{},
			[]string{`audit seq 1: records.reindex.done names reindex "zzz", ` + orphan},
		},
		{
			"an error with no announcement",
			[]AuditEntry{failed(1, "zzz")},
			[]string{},
			[]string{`audit seq 1: records.reindex.error names reindex "zzz", ` + orphan},
		},
		{
			"a second conclusion of the same reindex",
			[]AuditEntry{start(1, "a"), done(2, "a"), done(3, "a")},
			[]string{},
			[]string{`audit seq 3: records.reindex.done names reindex "a", ` + orphan},
		},
		{
			"an error after the done of the same reindex",
			[]AuditEntry{start(1, "a"), done(2, "a"), failed(3, "a")},
			[]string{},
			[]string{`audit seq 3: records.reindex.error names reindex "a", ` + orphan},
		},
		{
			"a reindex superseded by a later done cannot be concluded again",
			[]AuditEntry{start(1, "a"), start(2, "b"), done(3, "b"), failed(4, "a")},
			[]string{},
			[]string{`audit seq 4: records.reindex.error names reindex "a", ` + orphan},
		},
		{"a later done concludes an earlier dangling one", []AuditEntry{start(1, "a"), start(2, "b"), done(3, "b")}, []string{}, nil},
		{"a dangling one after a done stays", []AuditEntry{start(1, "a"), done(2, "a"), start(3, "b")}, []string{"b"}, nil},
		{"two dangling", []AuditEntry{start(1, "a"), failed(2, "a"), start(3, "b"), start(4, "c")}, []string{"b", "c"}, nil},
		{
			"undecodable start",
			[]AuditEntry{{Seq: 1, Action: ActionReindex, Details: map[string]any{"reindex_id": 5}}},
			[]string{},
			[]string{"audit seq 1: records.reindex details unreadable"},
		},
		{
			"undecodable done concludes nothing",
			[]AuditEntry{start(1, "a"), {Seq: 2, Action: ActionReindexDone, Details: map[string]any{"bogus": true}}},
			[]string{"a"},
			[]string{"audit seq 2: records.reindex.done details unreadable"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := &VerifyReport{Problems: []string{}, Notices: []string{}}
			got := ids(danglingReindexes(tc.entries, rep))
			if !slices.Equal(got, tc.want) {
				t.Errorf("dangling = %v, want %v", got, tc.want)
			}
			if len(rep.Problems) != len(tc.problems) {
				t.Fatalf("problems = %q, want %d matching %q", rep.Problems, len(tc.problems), tc.problems)
			}
			for i, p := range rep.Problems {
				if !strings.Contains(p, tc.problems[i]) {
					t.Errorf("problem %d = %q, want it to contain %q", i, p, tc.problems[i])
				}
			}
		})
	}
}
