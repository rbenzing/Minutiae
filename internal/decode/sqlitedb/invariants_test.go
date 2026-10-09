package sqlitedb_test

import (
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// checkRowInvariants asserts the contract every Row keeps whatever the bytes it
// was read from: the accessors, the states, the kinds and the row flag agree.
func checkRowInvariants(t testing.TB, r sqlitedb.Row) {
	t.Helper()
	anyUnknown := false
	for i := range r.NumCols() {
		st := r.State(i)
		_, iok := r.Int(i)
		_, fok := r.Float(i)
		txt, tok := r.Text(i)
		_, bok := r.Blob(i)
		null := r.IsNull(i)
		value := iok || fok || tok || bok
		if value && st != sqlitedb.StatePresent && st != sqlitedb.StateDefaulted {
			t.Errorf("%s col %d: an accessor answered in state %v", r.Table(), i, st)
		}
		if null && st != sqlitedb.StateNull && st != sqlitedb.StateDefaulted {
			t.Errorf("%s col %d: IsNull in state %v", r.Table(), i, st)
		}
		if r.Unknown(i) {
			anyUnknown = true
			if value || null {
				t.Errorf("%s col %d: unknown (%v) but an accessor answered (value %v, null %v)", r.Table(), i, st, value, null)
			}
		}
		if st == sqlitedb.StatePresent {
			n := 0
			for kind, ok := range map[sqlitefile.Kind]bool{sqlitefile.KindInt: iok, sqlitefile.KindFloat: fok, sqlitefile.KindText: tok, sqlitefile.KindBlob: bok} {
				if ok {
					n++
					if r.Kind(i) != kind {
						t.Errorf("%s col %d: the %v accessor answered for a %v value", r.Table(), i, kind, r.Kind(i))
					}
				}
			}
			if n != 1 {
				t.Errorf("%s col %d: %d accessors answered for a present %v value, want exactly 1", r.Table(), i, n, r.Kind(i))
			}
		}
		if tok && isUTF16Enc(r.Value(i).Enc) && !utf8.Valid(txt) {
			t.Errorf("%s col %d: UTF-16 text is not valid UTF-8", r.Table(), i)
		}
	}
	if got := r.Flags()&sqlitedb.FlagUnknownValues != 0; got != anyUnknown {
		t.Errorf("%s: FlagUnknownValues = %v, some column unknown = %v", r.Table(), got, anyUnknown)
	}
	if st := r.State(-1); st != sqlitedb.StateAbsent {
		t.Errorf("State(-1) = %v", st)
	}
	if st := r.State(r.NumCols()); st != sqlitedb.StateAbsent {
		t.Errorf("State(NumCols) = %v", st)
	}
	if r.Recovered() == nil {
		if rg, _ := r.Range(); rg.Length <= 0 || rg.Offset < 0 {
			t.Errorf("%s: live Range = %+v", r.Table(), rg)
		}
	}
}

func isUTF16Enc(e sqlitefile.Encoding) bool {
	return e == sqlitefile.EncUTF16LE || e == sqlitefile.EncUTF16BE
}

// checkRecoveredInvariants adds what a recovered row promises: it says it is
// recovered and names no locator.
func checkRecoveredInvariants(t testing.TB, r sqlitedb.Row) {
	t.Helper()
	checkRowInvariants(t, r)
	if r.Recovered() == nil {
		t.Errorf("%s: Recovered() is nil", r.Table())
	}
	if loc, ok := r.Locator(); ok {
		t.Errorf("%s: a recovered row has locator %q", r.Table(), loc)
	}
}
