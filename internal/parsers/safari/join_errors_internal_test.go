package safari

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// memRow is a row of another implementation with typed cells.
type memRow struct{ cells []any }

func (memRow) Table() string        { return "t" }
func (memRow) Rowid() (int64, bool) { return 1, true }
func (r memRow) IsNull(c int) bool  { return r.cells[c] == nil }
func (r memRow) Int(c int) (int64, bool) {
	v, ok := r.cells[c].(int64)
	return v, ok
}
func (r memRow) Float(c int) (float64, bool) { f, ok := r.cells[c].(float64); return f, ok }
func (r memRow) Text(c int) ([]byte, bool) {
	s, ok := r.cells[c].(string)
	return []byte(s), ok
}
func (memRow) Blob(int) ([]byte, bool) { return nil, false }
func (memRow) Range() (records.Range, parse.FileRole) {
	return records.Range{Offset: 1, Length: 10}, "db"
}
func (memRow) Locator() (string, bool) { return "sqlite:table=t;rowid=1", true }

// errCtx answers every join with err and every column by its position in cols.
type errCtx struct {
	cols []string
	err  error
}

func (errCtx) Input() *parse.Input {
	return &parse.Input{Artifacts: map[string]parse.Artifact{"db": {ID: "a", Size: 100}}}
}
func (errCtx) Note(string, string) {}
func (c errCtx) Column(_, col string) int {
	for i, n := range c.cols {
		if n == col {
			return i
		}
	}
	return -1
}

func (c errCtx) Get(string, int64) (parse.Row, bool, error)               { return nil, false, c.err }
func (c errCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) { return nil, c.err }

// A budget or a cancelled context returns to the runner (the job fails loudly); any other join failure
// is about that row or that join (4F-R22, R30).
func TestJoinErrorClasses(t *testing.T) {
	budget := fmt.Errorf("join: %w", parse.ErrBudget)
	visit := memRow{cells: []any{int64(1)}}
	item := memRow{cells: []any{int64(1), "https://a.example/", nil, nil}}

	_, err := mapVisit(errCtx{cols: []string{"history_item"}, err: budget}, visit)
	if !errors.Is(err, parse.ErrBudget) {
		t.Errorf("Get with a budget error: %v, want the budget error returned", err)
	}
	_, err = mapVisit(errCtx{cols: []string{"history_item"}, err: errors.New("corrupt page")}, visit)
	var re *rowrun.RowError
	if !errors.As(err, &re) {
		t.Errorf("Get with another error: %v, want a RowError", err)
	}
	_, err = mapItem(errCtx{cols: itemColumns(), err: budget}, item)
	if !errors.Is(err, parse.ErrBudget) {
		t.Errorf("Match with a budget error: %v, want the budget error returned", err)
	}
	recs, err := mapItem(errCtx{cols: itemColumns(), err: errors.New("index failed")}, item)
	if err != nil || len(recs) != 1 {
		t.Fatalf("Match with another error: %v, %d records, want one join_undecided record", err, len(recs))
	}
	raw, _ := recs[0].Payload["raw"].(map[string]any)
	if raw["visitless"] != nil || raw["join_undecided"] == nil {
		t.Errorf("raw %v", raw)
	}
}
