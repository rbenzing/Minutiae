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
// journal are attached when non-empty) as Android artifacts, under the lenient harness
// emitter. It fails the test on a record the writer refused or any other broken contract
// rule (Result.Violations) and on an error that is not a typed hostile-input error; a
// panic of the parser propagates and fails the fuzz target.
func FuzzParse(t *testing.T, p parse.Parser, remotePath string, db, wal, journal []byte) {
	t.Helper()
	FuzzParseBundle(t, p, func(h *Harness) Bundle { return fuzzFixture(db, wal, journal).Bundle(h, remotePath) })
}

// FuzzParseIOS is FuzzParse over iOS artifacts (BundleIOS). The harness refuses a record of an
// iOS parser whose artifact is an Android one (under the lenient emitter that is a refused record
// and a warning, not a failure), so an iOS parser is fuzzed through this entry point to have
// its records reach the writer at all (4F-R25).
func FuzzParseIOS(t *testing.T, p parse.Parser, remotePath, logical string, db, wal, journal []byte) {
	t.Helper()
	fuzzIOS(t, p, remotePath, logical, db, wal, journal)
}

// fuzzIOS is FuzzParseIOS returning the lenient Result.
func fuzzIOS(t *testing.T, p parse.Parser, remotePath, logical string, db, wal, journal []byte) Result {
	t.Helper()
	return fuzzRun(t, p, func(h *Harness) Bundle { return fuzzFixture(db, wal, journal).BundleIOS(h, remotePath, logical) })
}

// FuzzParseBundle is the body of FuzzParse with the bundle supplied by build, which is called
// with the harness of the run.
func FuzzParseBundle(t *testing.T, p parse.Parser, build func(h *Harness) Bundle) {
	t.Helper()
	fuzzRun(t, p, build)
}

// fuzzRun does the work of FuzzParseBundle and returns the lenient Result.
func fuzzRun(t *testing.T, p parse.Parser, build func(h *Harness) Bundle) Result {
	t.Helper()
	h := NewHarness(t)
	b := build(h)
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
	return res
}

func fuzzFixture(db, wal, journal []byte) Fixture {
	fx := Fixture{Name: "fuzz", Files: map[string][]byte{"db": db}}
	if len(wal) > 0 {
		fx.Files["wal"] = wal
	}
	if len(journal) > 0 {
		fx.Files["journal"] = journal
	}
	return fx
}
