package parsertest

import (
	"errors"
	"fmt"
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

// TestFuzzParseIOSAcceptsAnIOSParser: FuzzParseIOS bundles the bytes as an iOS artifact, so a parser declared for iOS emits records that
// the platform check accepts, which FuzzParse (Android artifacts) could never give it (4F-R25).
func TestFuzzParseIOSAcceptsAnIOSParser(t *testing.T) {
	p := iosWellBehaved{WellBehaved{Name: "fz", Version: "1.0.0"}}
	FuzzParseIOS(t, p, "/private/var/parsertest/fz.dat", "ios:HomeDomain/parsertest/fz.dat", []byte("a\nb\n"), nil, nil)
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
