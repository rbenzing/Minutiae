package evidence

import (
	"slices"
	"testing"
)

// TestDanglingReindexes: which announced reindexes the audit log leaves unconcluded. An error entry
// concludes the reindex it names, a done entry concludes every earlier announcement (a finished
// rebuild supersedes a cut-short one), an entry that does not decode is a problem and concludes
// nothing.
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
	tests := []struct {
		name     string
		entries  []AuditEntry
		want     []string
		problems int
	}{
		{"none", nil, []string{}, 0},
		{"concluded by done", []AuditEntry{start(1, "a"), done(2, "a")}, []string{}, 0},
		{"concluded by error", []AuditEntry{start(1, "a"), failed(2, "a")}, []string{}, 0},
		{"announced only", []AuditEntry{start(1, "a")}, []string{"a"}, 0},
		{"an error of another id concludes nothing", []AuditEntry{start(1, "a"), failed(2, "b")}, []string{"a"}, 0},
		{"a later done concludes an earlier dangling one", []AuditEntry{start(1, "a"), start(2, "b"), done(3, "b")}, []string{}, 0},
		{"a dangling one after a done stays", []AuditEntry{start(1, "a"), done(2, "a"), start(3, "b")}, []string{"b"}, 0},
		{"two dangling", []AuditEntry{start(1, "a"), failed(2, "a"), start(3, "b"), start(4, "c")}, []string{"b", "c"}, 0},
		{"undecodable start", []AuditEntry{{Seq: 1, Action: ActionReindex, Details: map[string]any{"reindex_id": 5}}}, []string{}, 1},
		{"undecodable done concludes nothing", []AuditEntry{start(1, "a"), {Seq: 2, Action: ActionReindexDone, Details: map[string]any{"bogus": true}}}, []string{"a"}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := &VerifyReport{Problems: []string{}, Notices: []string{}}
			got := ids(danglingReindexes(tc.entries, rep))
			if !slices.Equal(got, tc.want) || len(rep.Problems) != tc.problems {
				t.Errorf("dangling = %v with problems %v, want %v with %d problems", got, rep.Problems, tc.want, tc.problems)
			}
		})
	}
}
