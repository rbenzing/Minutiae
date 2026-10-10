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
