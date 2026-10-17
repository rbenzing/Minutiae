package parsertest

import (
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// HostileOK reports whether err is a typed error a hostile database may legitimately
// produce. A recovered panic (sqlitedb.ErrInternal), a released database or companion
// files that do not belong are bugs and are not accepted.
func HostileOK(err error) bool {
	for _, target := range []error{
		sqlitedb.ErrNotSQLite, sqlitedb.ErrEncrypted, sqlitedb.ErrCorrupt, sqlitedb.ErrLiveUnavailable,
		sqlitedb.ErrEngineRefuses, sqlitedb.ErrUnsupportedSchema, sqlitedb.ErrNoSuchTable, sqlitedb.ErrWithoutRowid,
		sqlitedb.ErrIndexLimit, parse.ErrBudget, parse.ErrProbeLimit, rowrun.ErrContext,
		sqlitefile.ErrLimit, sqlitefile.ErrPageUnavailable, sqlitefile.ErrBudget, context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// FuzzParse runs p's Probe and Parse over a bundle made of the given bytes (wal and
// journal are attached when non-empty) under the lenient harness emitter. It fails the
// test on a record the writer refused or any other broken contract rule
// (Result.Violations) and on an error that is not a typed hostile-input error; a panic
// of the parser propagates and fails the fuzz target.
func FuzzParse(t *testing.T, p parse.Parser, remotePath string, db, wal, journal []byte) {
	t.Helper()
	h := NewHarness(t)
	fx := Fixture{Name: "fuzz", Files: map[string][]byte{"db": db}}
	if len(wal) > 0 {
		fx.Files["wal"] = wal
	}
	if len(journal) > 0 {
		fx.Files["journal"] = journal
	}
	b := fx.Bundle(h, remotePath)
	if _, err := p.Probe(t.Context(), b.Input(h, p, true)); err != nil && !HostileOK(err) {
		t.Fatalf("Probe returned %v", err)
	}
	res := h.RunLenient(p, b.Input(h, p, false))
	if res.Err != nil && !HostileOK(res.Err) {
		t.Fatalf("Parse returned %v", res.Err)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("contract violations: %v", res.Violations)
	}
}
