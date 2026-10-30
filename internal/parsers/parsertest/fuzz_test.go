package parsertest

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

func TestHostileOKAcceptsTypedErrorsOnly(t *testing.T) {
	for _, err := range []error{sqlitedb.ErrCorrupt, fmt.Errorf("open: %w", sqlitedb.ErrNotSQLite), sqlitedb.ErrEncrypted} {
		if !HostileOK(err) {
			t.Errorf("%v refused", err)
		}
	}
	for _, err := range []error{sqlitedb.ErrInternal, sqlitedb.ErrReleased, sqlitedb.ErrBadFiles, errors.New("x")} {
		if HostileOK(err) {
			t.Errorf("%v accepted: a recovered panic or a host bug is not a hostile-input error", err)
		}
	}
}

func TestFuzzParseAcceptsAWellBehavedParser(t *testing.T) {
	FuzzParse(t, WellBehaved{Name: "fz", Version: "1.0.0"}, "/data/parsertest/fz.dat", []byte("a\nb\n"), nil, nil)
}

// iosWellBehaved is WellBehaved declared for iOS, to run the iOS fuzz entry point.
type iosWellBehaved struct{ WellBehaved }

func (p iosWellBehaved) Meta() parse.Meta {
	m := p.WellBehaved.Meta()
	m.Platforms = []string{parse.PlatformIOS}
	m.Inputs[0].Globs = []string{"ios:**/parsertest/" + p.Name + ".dat"}
	return m
}

// FuzzParseIOS must hand the parser iOS artifacts: the records of an iOS parser then pass the platform
// check. Over Android artifacts (the Bundle of FuzzParse) every record would be refused with a warning,
// which the lenient emitter does not turn into a failure, so the test requires records and no warning.
func TestFuzzParseIOSAcceptsAnIOSParser(t *testing.T) {
	p := iosWellBehaved{WellBehaved{Name: "fz", Version: "1.0.0"}}
	res := fuzzIOS(t, p, "/private/var/parsertest/fz.dat", "ios:HomeDomain/parsertest/fz.dat", []byte("a\nb\n"), nil, nil)
	if len(res.Records) != 2 || len(res.Warnings) != 0 {
		t.Errorf("records %d warnings %v, want 2 records and none", len(res.Records), res.Warnings)
	}
}

func TestFuzzParseBundleUsesTheBundleItIsGiven(t *testing.T) {
	p := WellBehaved{Name: "fz", Version: "1.0.0"}
	called := false
	FuzzParseBundle(t, p, func(h *Harness) Bundle {
		called = true
		return Fixture{Name: "fuzz", Files: map[string][]byte{"db": []byte("a\n")}}.Bundle(h, "/data/parsertest/fz.dat")
	})
	if !called {
		t.Error("the build function was not called")
	}
}

// failRecorder is a testing.TB whose Fatalf stops the goroutine and records the failure instead of failing the
// real test, so a test can require that a shared helper fails.
type failRecorder struct {
	testing.TB
	failed bool
	msg    string
}

func (f *failRecorder) Fatalf(format string, args ...any) {
	f.failed, f.msg = true, fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (f *failRecorder) Errorf(format string, args ...any) {
	f.failed, f.msg = true, fmt.Sprintf(format, args...)
}

func (f *failRecorder) Fatal(args ...any) { f.Fatalf("%s", fmt.Sprint(args...)) }

// runFuzzHelper runs fn in its own goroutine (Fatalf ends it) and reports whether the recorder saw a failure.
func runFuzzHelper(t *testing.T, fn func(tb testing.TB)) *failRecorder {
	t.Helper()
	rec := &failRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(rec)
	}()
	<-done
	return rec
}

// The shared fuzz helpers run the lenient emitter, which turns a record the writer refused into a warning. A
// refused record is a mapping bug, so the helpers must fail on it (4E carry-over, Task 16).
func TestFuzzHelpersFailOnARefusedRecord(t *testing.T) {
	bad := Invalid{Name: "fz", Version: "1.0.0", Mode: InvalidNulInSummary, N: 1}
	good := WellBehaved{Name: "fz", Version: "1.0.0"}
	data := []byte("a\nb\n")
	cases := map[string]func(tb testing.TB, p parse.Parser){
		"FuzzParse": func(tb testing.TB, p parse.Parser) { FuzzParse(tb, p, "/data/parsertest/fz.dat", data, nil, nil) },
		"FuzzParseBundle": func(tb testing.TB, p parse.Parser) {
			FuzzParseBundle(tb, p, func(h *Harness) Bundle {
				return Fixture{Name: "fuzz", Files: map[string][]byte{"db": data}}.Bundle(h, "/data/parsertest/fz.dat")
			})
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := runFuzzHelper(t, func(tb testing.TB) { run(tb, bad) }); !rec.failed || !strings.Contains(rec.msg, "refused") {
				t.Errorf("a refused record did not fail the helper (failed %v: %q)", rec.failed, rec.msg)
			}
			if rec := runFuzzHelper(t, func(tb testing.TB) { run(tb, good) }); rec.failed {
				t.Errorf("a well-behaved parser failed the helper: %s", rec.msg)
			}
		})
	}
	t.Run("FuzzParseIOS", func(t *testing.T) {
		p := iosWellBehaved{WellBehaved{Name: "fz", Version: "1.0.0"}}
		// An Android-platform parser over iOS artifacts: every record is refused on the platform rule.
		rec := runFuzzHelper(t, func(tb testing.TB) {
			FuzzParseIOS(tb, bad, "/private/var/parsertest/fz.dat", "ios:HomeDomain/parsertest/fz.dat", data, nil, nil)
		})
		if !rec.failed || !strings.Contains(rec.msg, "refused") {
			t.Errorf("a refused record did not fail FuzzParseIOS (failed %v: %q)", rec.failed, rec.msg)
		}
		if rec := runFuzzHelper(t, func(tb testing.TB) {
			FuzzParseIOS(tb, p, "/private/var/parsertest/fz.dat", "ios:HomeDomain/parsertest/fz.dat", data, nil, nil)
		}); rec.failed {
			t.Errorf("a well-behaved iOS parser failed FuzzParseIOS: %s", rec.msg)
		}
	})
}
