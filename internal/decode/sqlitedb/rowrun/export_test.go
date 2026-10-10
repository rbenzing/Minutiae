package rowrun

import (
	"context"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// RunWithSeam is Run with a hook called once the database is open.
func RunWithSeam(ctx context.Context, in *parse.Input, out parse.Emitter, steps []Step, afterOpen func(*sqlitedb.DB)) (Report, error) {
	return run(ctx, in, out, steps, &seams{afterOpen: afterOpen})
}

// CloseForTest closes mc the way the runner does and returns what the runner would return.
func CloseForTest(mc *sqlitedb.Context) error {
	r := &runner{mc: mc}
	return r.closeContext()
}

// UnmappedForTest warns about row the way the runner does for an unmapped row.
func UnmappedForTest(ctx context.Context, out parse.Emitter, row sqlitedb.Row, reason string) error {
	r := &runner{ctx: ctx, out: out}
	return r.unmapped(&TableReport{}, row, reason)
}

// ForwardForTest forwards the library warnings of db after the runner has already warned n times.
func ForwardForTest(ctx context.Context, db *sqlitedb.DB, out parse.Emitter, n int) error {
	r := &runner{ctx: ctx, out: out, db: db, warned: n}
	return r.forwardWarnings()
}

// CloseNilForTest closes a runner that holds no Context.
func CloseNilForTest() error { return (&runner{}).closeContext() }

// MatchKeyForTest is Match with an arbitrary key, to force an undecidable key on a clean table.
func MatchKeyForTest(m parse.MapContext, table, col string, key parse.JoinKey) ([]parse.Row, bool, error) {
	return match(m, table, col, key)
}
