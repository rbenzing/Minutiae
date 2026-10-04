package f2fs_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

func wantNoEntry(t *testing.T, f *f2fs.FS, path string) {
	t.Helper()
	if e, err := f.Lookup(path); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(%q) = %+v, %v; want ErrNotFound", path, e, err)
	}
}

// "." and ".." are never found by Lookup, not even through the case-folding
// comparison of a casefold directory that holds a record with such a name.
func TestLookupCasefoldNeverFindsDots(t *testing.T) {
	f, _, _ := treeFS(t, treeOpts(), []f2fstest.File{
		{Path: "/cf", Dir: true, Casefold: true},
		{Path: "/cf/first", Data: []byte("1")},
		{Path: "/cf/dot", RawName: []byte("."), Data: []byte("d")},
		{Path: "/cf/dotdot", RawName: []byte(".."), Data: []byte("dd")},
	})
	for _, p := range []string{"/cf/.", "/cf/..", "/cf/./first"} {
		wantNoEntry(t, f, p)
	}
	if _, err := f.Lookup("/cf/FIRST"); err != nil {
		t.Errorf("case-folded lookup of a normal name: %v", err)
	}
}

// Each name has one display spelling: the aliases are decoded strictly, so a
// base64 string with stray trailing bits does not stand for a name; and in a
// plain directory "~raw~" stands only for names ReadDir shows in that form.
func TestLookupAliasesAreCanonical(t *testing.T) {
	raw := []byte{0xff}                                                      // "_w" canonical; "_x" decodes to the same byte non-strictly
	cipher := []byte{0xfe}                                                   // "_g"; "_h" is the non-canonical spelling
	literal := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte("abc")) // a plain name that looks like a display form
	f, _, _ := treeFS(t, treeOpts(), []f2fstest.File{
		{Path: "/bin", RawName: raw, Data: []byte("b")},
		{Path: "/abc", Data: []byte("abc")},
		{Path: "/enc", Dir: true, Encrypted: true},
		{Path: "/enc/c", RawName: cipher, Data: []byte("c"), Encrypted: true},
		{Path: "/lit", Dir: true},
		{Path: "/lit/plain", RawName: []byte("abc"), Data: []byte("plain")},
		{Path: "/lit/shadow", RawName: []byte(literal), Data: []byte("shadow")},
	})
	if _, err := f.Lookup("/~raw~_w"); err != nil {
		t.Errorf("canonical ~raw~ alias: %v", err)
	}
	wantNoEntry(t, f, "/~raw~_x")
	if _, err := f.Lookup("/enc/~enc~_g"); err != nil {
		t.Errorf("canonical ~enc~ alias: %v", err)
	}
	wantNoEntry(t, f, "/enc/~enc~_h")

	// "abc" is a plain name: its ~raw~ spelling is not an alias for it.
	wantNoEntry(t, f, "/"+literal)
	// ... unless a directory holds a name that is literally that string: the
	// exact match wins, and "abc" stays reachable under its own name.
	e, err := f.Lookup("/lit/" + literal)
	if err != nil || !strings.Contains(e.ID, "nid:") {
		t.Fatalf("exact name that looks like an alias: %+v, %v", e, err)
	}
	fl, err := f.Open(e)
	if err != nil {
		t.Fatal(err)
	}
	if got := readWhole(t, fl); string(got) != "shadow" {
		t.Errorf("literal name resolved to %q, want the file literally named so", got)
	}
	if _, err := f.Lookup("/lit/abc"); err != nil {
		t.Errorf("plain name: %v", err)
	}
}

// Past the per-scan cap the live entries keep precedence: deleted entries are
// cut off first (at half the cap, with a warning) and later live entries are
// still listed; a live entry beyond the cap is cut off with a warning.
func TestDirEntryCapPrefersLiveEntries(t *testing.T) {
	var files []f2fstest.File
	for i := range 20 {
		files = append(files, f2fstest.File{Path: fmt.Sprintf("/d%02d", i), Data: []byte("x"), Deleted: true})
	}
	for i := range 4 {
		files = append(files, f2fstest.File{Path: fmt.Sprintf("/live%d", i), Data: []byte("y")})
	}
	f, _, _ := treeFS(t, treeOpts(), files)
	f.SetDirEntryCap(10)
	es, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	var live, del []string
	for _, e := range es {
		if e.Deleted {
			del = append(del, e.Name)
		} else {
			live = append(live, e.Name)
		}
	}
	if !slices.Equal(live, []string{"live0", "live1", "live2", "live3"}) || len(del) != 5 {
		t.Errorf("live %v, deleted %d; want all 4 live entries and 5 (half the cap) deleted ones", live, len(del))
	}
	if !hasWarning(f.Info(), "deleted entries") {
		t.Errorf("no warning about the deleted entries: %v", f.Info().Warnings)
	}
	if _, err := f.Lookup("/live3"); err != nil {
		t.Errorf("Lookup of a live entry behind the deleted ones: %v", err)
	}

	// Live entries beyond the cap are cut off, with a warning.
	files = nil
	for i := range 14 {
		files = append(files, f2fstest.File{Path: fmt.Sprintf("/f%02d", i), Data: []byte("x")})
	}
	f, _, _ = treeFS(t, treeOpts(), files)
	f.SetDirEntryCap(10)
	es, err = f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 10 || !hasWarning(f.Info(), "more than 10 entries") {
		t.Errorf("%d entries, warnings %v; want 10 and the cap warning", len(es), f.Info().Warnings)
	}
}

// An alias must be spelled exactly as ReadDir shows it: CR/LF embedded in the
// text (which a base64 decoder silently skips) and other variants are not
// aliases.
func TestLookupAliasRejectsNonCanonicalSpelling(t *testing.T) {
	f, _, _ := treeFS(t, treeOpts(), []f2fstest.File{
		{Path: "/bin", RawName: []byte{0xff, 0xfe, 0xfd}, Data: []byte("b")},
		{Path: "/enc", Dir: true, Encrypted: true},
		{Path: "/enc/c", RawName: []byte{0xfe, 0x01, 0x02}, Data: []byte("c"), Encrypted: true},
	})
	canon := base64.RawURLEncoding.EncodeToString([]byte{0xff, 0xfe, 0xfd})
	encCanon := base64.RawURLEncoding.EncodeToString([]byte{0xfe, 0x01, 0x02})
	if _, err := f.Lookup("/~raw~" + canon); err != nil {
		t.Fatalf("canonical alias: %v", err)
	}
	if _, err := f.Lookup("/enc/~enc~" + encCanon); err != nil {
		t.Fatalf("canonical enc alias: %v", err)
	}
	for _, bad := range []string{canon[:2] + "\n" + canon[2:], canon[:2] + "\r\n" + canon[2:], canon + "\n", canon + "="} {
		wantNoEntry(t, f, "/~raw~"+bad)
		wantNoEntry(t, f, "/enc/~enc~"+encCanon[:3]+"\n"+encCanon[3:])
	}
}
