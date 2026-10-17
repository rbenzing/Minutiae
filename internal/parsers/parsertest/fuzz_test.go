package parsertest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
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
