package parsers

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
)

// RecoveryRunnerPath is derived from this package's own path; the runner package is a real
// package, so the two must agree, or the .recover hash would cover a package that is not the runner.
func TestRecoveryRunnerPathIsTheRunnerPackage(t *testing.T) {
	want := reflect.TypeFor[recrun.Report]().PkgPath()
	if got := RecoveryRunnerPath(); got != want {
		t.Fatalf("RecoveryRunnerPath() = %q, the package of recrun.Report is %q", got, want)
	}
}

// The package whose source the .recover identity hashes holds the code that runs the typed recovery.
func TestRecoveryRunnerPathHoldsTheCodeThatRunsTheRecovery(t *testing.T) {
	fn := runtime.FuncForPC(reflect.ValueOf(recrun.Run).Pointer())
	if fn == nil {
		t.Fatal("no function for recrun.Run")
	}
	if want := RecoveryRunnerPath() + ".Run"; fn.Name() != want {
		t.Errorf("recrun.Run is %q, want %q (the runner is not in the hashed package)", fn.Name(), want)
	}
	if !strings.HasSuffix(RecoveryRunnerPath(), "/decode/sqlitedb/recrun") {
		t.Errorf("runner path %q", RecoveryRunnerPath())
	}
}
