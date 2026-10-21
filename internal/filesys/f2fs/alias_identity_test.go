package f2fs_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// Z74 (identity): a stored name that is also the display form of another record is ambiguous, in either
// directory order, never the stored name.
func TestLookupStoredNameThatIsAnotherRecordsAliasIsAmbiguous(t *testing.T) {
	stored := f2fstest.File{Path: "/d/lit", RawName: []byte("~raw~_w"), Inline: true}
	other := f2fstest.File{Path: "/d/bin", RawName: []byte{0xff}, Inline: true}
	for _, order := range [][]f2fstest.File{{stored, other}, {other, stored}} {
		f, _, _ := treeFS(t, treeOpts(), append([]f2fstest.File{{Path: "/d", Dir: true}}, order...))
		_, err := f.Lookup("/d/~raw~_w")
		if !errors.Is(err, filesys.ErrAmbiguous) {
			t.Errorf("Lookup(~raw~_w) = %v; want ErrAmbiguous", err)
		}
		// the raw bytes themselves are exact and unambiguous
		if e, err := f.Lookup("/d/\xff"); err != nil || string(rawName(e)) != "\xff" {
			t.Errorf("Lookup(0xff) = %+v, %v", e, err)
		}
	}
	f, _, _ := treeFS(t, treeOpts(), []f2fstest.File{{Path: "/d", Dir: true}, stored})
	if e, err := f.Lookup("/d/~raw~_w"); err != nil || e.Name != "~raw~_w" {
		t.Errorf("a lone stored ~raw~ name = %+v, %v", e, err)
	}
}

// The record the display form stands for may lie beyond a cut: the stored name cannot be taken as the answer.
func TestLookupStoredAliasShapedNameBeforeACutIsIncomplete(t *testing.T) {
	files := []f2fstest.File{
		{Path: "/d", Dir: true},
		{Path: "/d/lit", RawName: []byte("~raw~_w"), Inline: true},
		{Path: "/d/p1", Inline: true},
		{Path: "/d/p2", Inline: true},
		{Path: "/d/p3", Inline: true},
		{Path: "/d/bin", RawName: []byte{0xff}, Inline: true},
	}
	f, _, _ := treeFS(t, treeOpts(), files)
	f.SetDirEntryCap(3)
	_, err := f.Lookup("/d/~raw~_w")
	wantIncomplete(t, "an alias-shaped stored name with the possible alias beyond the cap", err, true)
	g, _, _ := treeFS(t, treeOpts(), files)
	g.SetDirEntryCap(3)
	if e, err := g.Lookup("/d/p1"); err != nil || e.Name != "p1" {
		t.Errorf("a plain stored name before the cut = %+v, %v", e, err)
	}
}

// Z72: the ~enc~ forms in an encrypted directory are strict too.
func TestLookupEncAliasRejectsNonCanonicalForms(t *testing.T) {
	f, _, _ := treeFS(t, treeOpts(), []f2fstest.File{
		{Path: "/enc", Dir: true, Encrypted: true},
		{Path: "/enc/c", RawName: []byte{0xfe}, Data: []byte("c"), Encrypted: true},
	})
	if _, err := f.Lookup("/enc/~enc~_g"); err != nil {
		t.Fatalf("canonical ~enc~ form: %v", err)
	}
	for _, s := range []string{"~enc~", "~enc~_h", "~enc~_g=", "~enc~_\ng", "~enc~_\r\ng", "~enc~+g", "~enc~!!!", "~raw~_g", "~ENC~_g", "~enc~~enc~_g"} {
		wantNoEntry(t, f, "/enc/"+s)
	}
}
