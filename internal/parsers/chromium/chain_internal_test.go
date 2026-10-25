package chromium

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// stubRow is a hand-built downloads_url_chains row: the columns are id, chain_index, url (nil is NULL).
type stubRow struct {
	rowid int64
	vals  [3]any
}

func (stubRow) Table() string { return "downloads_url_chains" }

func (r stubRow) Rowid() (int64, bool) { return r.rowid, true }

func (r stubRow) IsNull(col int) bool { return r.vals[col] == nil }

func (stubRow) Float(int) (float64, bool) { return 0, false }

func (stubRow) Blob(int) ([]byte, bool) { return nil, false }

func (r stubRow) Int(col int) (int64, bool) {
	v, ok := r.vals[col].(int64)
	return v, ok
}

func (r stubRow) Text(col int) ([]byte, bool) {
	v, ok := r.vals[col].(string)
	return []byte(v), ok
}

func (stubRow) Range() (records.Range, parse.FileRole) {
	return records.Range{}, parse.RoleDB
}

// stubCtx answers Match on downloads_url_chains with its rows in rowid order and nothing else.
type stubCtx struct {
	rows []stubRow
	err  error
}

func (stubCtx) Input() *parse.Input { return &parse.Input{} }
func (stubCtx) Note(string, string) {}
func (stubCtx) Column(table, col string) int {
	if table != "downloads_url_chains" {
		return -1
	}
	return slices.Index([]string{"id", "chain_index", "url"}, col)
}
func (stubCtx) Get(string, int64) (parse.Row, bool, error) { return nil, false, nil }
func (s stubCtx) Match(_, _ string, key parse.JoinKey) ([]parse.Row, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []parse.Row
	for _, r := range s.rows {
		if id, _ := r.vals[0].(int64); id == key.I {
			out = append(out, r)
		}
	}
	return out, nil
}

// 4F-C4: a repeated chain_index cannot be stored in a real file (the table is keyed on (id, chain_index)), so the
// duplicate state is proven here through the real chain mapper: the chain is still listed, in index order and ties by
// rowid.
func TestDuplicateChainIndexIsStatedAndListedByIndexThenRowid(t *testing.T) {
	m := stubCtx{rows: []stubRow{
		{rowid: 1, vals: [3]any{int64(7), int64(1), "https://a.example/1-first"}},
		{rowid: 2, vals: [3]any{int64(7), int64(0), "https://a.example/0"}},
		{rowid: 3, vals: [3]any{int64(7), int64(1), "https://a.example/1-second"}},
		{rowid: 4, vals: [3]any{int64(8), int64(0), "https://other.example/"}},
	}}
	raw := map[string]any{}
	ch, err := chainOf(m, 7, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://a.example/0", "https://a.example/1-first", "https://a.example/1-second"}
	if !slices.Equal(ch.urls, want) || ch.final != "https://a.example/1-second" || ch.undecided {
		t.Errorf("chain %+v, want urls %v ending in the last entry", ch, want)
	}
	if raw["url_chain_state"] != "duplicate-index" {
		t.Errorf("state %v, want duplicate-index", raw["url_chain_state"])
	}
}

// 4F-R11: the 1000-entry cap is applied after sorting, and the final URL is the last entry of the whole chain.
func TestChainOverTheEntryCapKeepsTheFirstEntriesInIndexOrder(t *testing.T) {
	var rows []stubRow
	for i := 1000; i >= 0; i-- { // stored in descending index order
		rows = append(rows, stubRow{rowid: int64(1001 - i), vals: [3]any{int64(1), int64(i), fmt.Sprintf("https://e.example/%04d", i)}})
	}
	raw := map[string]any{}
	ch, err := chainOf(stubCtx{rows: rows}, 1, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.urls) != 1000 {
		t.Fatalf("kept %d entries, want 1000", len(ch.urls))
	}
	if ch.urls[0] != "https://e.example/0000" || ch.urls[999] != "https://e.example/0999" || ch.final != "https://e.example/1000" {
		t.Fatalf("first %q last kept %q final %q", ch.urls[0], ch.urls[999], ch.final)
	}
	if raw["url_chain_truncated"] != true || raw["url_chain_total"] != int64(1001) {
		t.Errorf("raw %v, want url_chain_truncated and url_chain_total 1001", raw)
	}
	if _, ok := raw["url_chain_bytes_truncated"]; ok {
		t.Error("the byte cap was not exceeded")
	}
}

// 4F-R22: a failed answer from the engine other than budget or context makes the chain undecided; a cancelled
// context or a budget stops the job.
func TestChainJoinErrorsAreUndecidedOrFatalByClass(t *testing.T) {
	ch, err := chainOf(stubCtx{err: errors.New("no such table")}, 1, map[string]any{})
	if err != nil || !ch.undecided || len(ch.urls) != 0 {
		t.Errorf("chain %+v err %v, want undecided and no error", ch, err)
	}
	for _, fatalErr := range []error{parse.ErrBudget, context.Canceled} {
		if _, err := chainOf(stubCtx{err: fatalErr}, 1, map[string]any{}); err == nil {
			t.Errorf("error %v from the join was swallowed", fatalErr)
		}
	}
}
