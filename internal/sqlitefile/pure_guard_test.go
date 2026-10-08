package sqlitefile_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// TestPureParseFunctionsRecoverPanics: the five exported parse functions that
// return an error turn a panic into a *PanicError (ErrInternal), as every other
// exported entry point does; called again without the fault they work.
func TestPureParseFunctionsRecoverPanics(t *testing.T) {
	leaf := make([]byte, 64)
	leaf[0] = 0x0d // table leaf, no cells
	h, err := sqlitefile.ParsePageHeader(leaf, 2)
	if err != nil {
		t.Fatal(err)
	}
	rec := []byte{2, 1, 7} // header length 2, one 8-bit integer, value 7
	calls := map[string]func() error{
		"ParsePageHeader": func() error { _, err := sqlitefile.ParsePageHeader(leaf, 2); return err },
		"CellPointers":    func() error { _, err := sqlitefile.CellPointers(leaf, h, len(leaf)); return err },
		"ParseCell": func() error {
			_, err := sqlitefile.ParseCell(leaf, len(leaf), h, 8)
			if err != nil && !errors.Is(err, sqlitefile.ErrCorrupt) {
				return err
			}
			return nil
		},
		"ParseRecordHeader": func() error { _, _, _, err := sqlitefile.ParseRecordHeader(rec, 10); return err },
		"DecodeRecord": func() error {
			_, err := sqlitefile.DecodeRecord(rec, sqlitefile.EncUTF8, sqlitefile.Limits{})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); err != nil {
			t.Errorf("%s without a fault: %v", name, err)
		}
		restore := sqlitefile.SetPureHook(func(string) { panic("injected") })
		err := call()
		restore()
		wantPanicError(t, name, err)
		if err := call(); err != nil {
			t.Errorf("%s again: %v", name, err)
		}
	}
}
