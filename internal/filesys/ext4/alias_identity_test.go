package ext4_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// Z74 (identity): a stored name that is also the display form of another record is ambiguous, in either
// directory order, never the stored name.
func TestLookupStoredNameThatIsAnotherRecordsAliasIsAmbiguous(t *testing.T) {
	for _, order := range [][]string{{"~raw~_w", "\xff"}, {"\xff", "~raw~_w"}} {
		f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, dFiles(order...)))
		_, err := f.Lookup("/d/~raw~_w")
		if !errors.Is(err, filesys.ErrAmbiguous) {
			t.Errorf("order %q: Lookup(~raw~_w) = %v; want ErrAmbiguous", order, err)
		}
		// the raw bytes themselves are exact and unambiguous
		if e, err := f.Lookup("/d/\xff"); err != nil || string(rawName(e)) != "\xff" {
			t.Errorf("order %q: Lookup(0xff) = %+v, %v", order, e, err)
		}
	}
	// the stored name alone (nothing for the alias to stand for) is still found
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, dFiles("~raw~_w", "other")))
	if e, err := f.Lookup("/d/~raw~_w"); err != nil || e.Name != "~raw~_w" {
		t.Errorf("a lone stored ~raw~ name = %+v, %v", e, err)
	}
}

// The record the display form stands for may lie beyond a cut: the stored name cannot be taken as the answer.
func TestLookupStoredAliasShapedNameBeforeACutIsIncomplete(t *testing.T) {
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, dFiles("~raw~_w", "p1", "p2", "p3", "\xff")))
	f.SetDirRecordCap(3)
	_, err := f.Lookup("/d/~raw~_w")
	wantIncomplete(t, "an alias-shaped stored name with the possible alias beyond the cap", err, true)
	// a stored name that is not alias-shaped keeps standing on a cut scan
	g := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, dFiles("plain", "p1", "p2", "p3", "p4")))
	g.SetDirRecordCap(3)
	if e, err := g.Lookup("/d/plain"); err != nil || e.Name != "plain" {
		t.Errorf("a plain stored name before the cut = %+v, %v", e, err)
	}
}

// Z72: the ~enc~ forms in an encrypted directory are strict too: empty, padded, line breaks, the standard
// alphabet, other prefixes and a non-canonical spelling are all ErrNotFound; the canonical one is found.
func TestLookupEncAliasRejectsNonCanonicalForms(t *testing.T) {
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true, Encrypt: true}, []ext4test.File{{Path: "/enc/\xfe", Data: []byte("c")}}))
	if _, err := f.Lookup("/enc/~enc~_g"); err != nil {
		t.Fatalf("canonical ~enc~ form: %v", err)
	}
	for _, s := range []string{"~enc~", "~enc~_h", "~enc~_g=", "~enc~_\ng", "~enc~_\r\ng", "~enc~+g", "~enc~!!!", "~raw~_g", "~ENC~_g", "~enc~~enc~_g"} {
		if e, err := f.Lookup("/enc/" + s); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %+v, %v; want ErrNotFound", s, e, err)
		}
	}
}
