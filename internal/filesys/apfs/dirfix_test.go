package apfs_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

// The stored name hash of a directory record is checked for hashed keys and
// pure-ASCII names (normalization is the identity there); a mismatch keeps the
// entry, adds name_hash=bad and one warning.
func TestDrecNameHashIsVerified(t *testing.T) {
	// The two hashes a real container stores (see the oracle fixtures).
	for name, want := range map[string]uint32{"root": 2989177, "private-dir": 2828707} {
		if got, ok := apfs.NameHash([]byte(name), true); !ok || got != want {
			t.Errorf("NameHash(%q) = %d, %v; want %d", name, got, ok, want)
		}
	}
	if h1, _ := apfs.NameHash([]byte("Root"), true); h1 != 2989177 {
		t.Errorf("a case-insensitive hash must fold ASCII case, got %d", h1)
	}
	if h1, _ := apfs.NameHash([]byte("Root"), false); h1 == 2989177 {
		t.Error("a case-sensitive hash must not fold")
	}
	if _, ok := apfs.NameHash([]byte("café"), true); ok {
		t.Error("a non-ASCII name must not be verified (it needs normalization)")
	}

	files := []apfstest.File{
		{Path: "/good", Data: []byte("1")},
		{Path: "/Bad", Data: []byte("22"), BadHash: true},
		{Path: "/café", Data: []byte("333"), BadHash: true}, // not verified
	}
	for _, ci := range []bool{true, false} {
		v := dataVolume(files...)
		v.CaseInsensitive, v.NormInsensitive = ci, ci
		f, _ := openOpts(t, volOpts(v))
		es := mustReadDir(t, f, mustLookup(t, f, "/Data"))
		if got := entryNames(es); !slices.Equal(got, []string{"Bad", "café", "good"}) {
			t.Fatalf("ci=%v lists %q", ci, got)
		}
		for name, wantBad := range map[string]bool{"good": false, "Bad": true, "café": false} {
			v, ok := attr(byName(t, es, name), "name_hash")
			if ok != wantBad || (ok && v != "bad") {
				t.Errorf("ci=%v %s: name_hash = %q, %v; want bad=%v", ci, name, v, ok, wantBad)
			}
		}
		if w := f.Info().Warnings; len(w) != 1 || !hasWarn(f, `"Bad"`, "hash") {
			t.Errorf("ci=%v warnings: %q, want one for \"Bad\"", ci, w)
		}
		// Lookup scans by name, so a bad hash never hides the entry.
		e := mustLookup(t, f, "/Data/Bad")
		if v, ok := attr(e, "name_hash"); !ok || v != "bad" || e.Size != 2 {
			t.Errorf("ci=%v Lookup(Bad) = %+v", ci, e)
		}
		if ci {
			if e := mustLookup(t, f, "/Data/bad"); e.Name != "Bad" {
				t.Errorf("case-insensitive Lookup(bad) = %+v", e)
			}
		}
	}

	// Plain keys carry no hash: nothing to verify.
	v := dataVolume(files...)
	v.HashedKeys = false
	f, _ := openOpts(t, volOpts(v))
	for _, e := range mustReadDir(t, f, mustLookup(t, f, "/Data")) {
		if _, ok := attr(e, "name_hash"); ok {
			t.Errorf("%s: name_hash on a plain key", e.Name)
		}
	}
	noWarnings(t, f)
}

// A node-read budget that runs out during a listing is not a per-entry
// "inode unreadable": before the first entry the listing fails, later it is
// cut with a warning.
func TestNodeBudgetExhaustedInListing(t *testing.T) {
	im := newImage(t, volOpts(dataVolume(bigDir(40)...)))
	var failed, partial, full int
	for n := int64(0); n < 400; n += 3 {
		f := im.mustOpen()
		vol := mustLookup(t, f, "/Data")
		f.SetNodeReads(n)
		es, err := f.ReadDir(vol)
		for _, e := range es {
			if v, ok := attr(e, "inode"); ok && v == "unreadable" {
				t.Fatalf("budget %d: entry %q degraded to inode=unreadable", n, e.Name)
			}
		}
		switch {
		case err != nil:
			if !errors.Is(err, filesys.ErrCorrupt) || len(es) != 0 {
				t.Fatalf("budget %d: ReadDir = %d entries, %v; want a CorruptError and no entries", n, len(es), err)
			}
			failed++
		case len(es) < 41: // 40 entries and the synthetic .snapshots directory
			if len(es) == 0 || !hasWarn(f, "listing", "partial") {
				t.Fatalf("budget %d: %d entries, warnings %q; want a partial listing with a warning", n, len(es), f.Info().Warnings)
			}
			partial++
		default:
			full++
		}
	}
	if failed == 0 || partial == 0 || full == 0 {
		t.Errorf("outcomes: %d failed, %d partial, %d full; the test must reach all three", failed, partial, full)
	}
}

func TestSnapsIDsOfMissingOrEncryptedVolumes(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(richFiles()...), apfstest.Volume{Name: "Enc", Encrypted: true}))
	scans := f.Scans()
	for _, id := range []string{"snaps:2", "snaps:7", "snaps:99"} {
		for what, call := range map[string]func() error{
			"Open":    func() error { _, err := f.Open(filesys.Entry{ID: id}); return err },
			"ReadDir": func() error { _, err := f.ReadDir(filesys.Entry{ID: id}); return err },
		} {
			if err := call(); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("%s(%s) = %v, want ErrNotFound (no such volume)", what, id, err)
			}
		}
	}
	if _, err := f.Open(filesys.Entry{ID: "snaps:1"}); !errors.Is(err, filesys.ErrEncrypted) {
		t.Errorf("Open(snaps:1) = %v, want ErrEncrypted", err)
	}
	if _, err := f.ReadDir(filesys.Entry{ID: "snaps:1"}); !errors.Is(err, filesys.ErrEncrypted) {
		t.Errorf("ReadDir(snaps:1) = %v, want ErrEncrypted", err)
	}
	if _, err := f.Open(filesys.Entry{ID: "snaps:0"}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open(snaps:0) = %v, want ErrUnsupported (a directory)", err)
	}
	if got := f.Scans(); got != scans {
		t.Errorf("%d tree scans for synthetic IDs", got-scans)
	}
}

func TestUnreadableVolumeNameNeverCollides(t *testing.T) {
	im := newImage(t, volOpts(apfstest.Volume{Name: "~unreadable~1"}, apfstest.Volume{Name: "Two"}))
	copy(im.blk(im.g.Volumes[1].Super)[32:], "XXXX")
	im.seal(im.g.Volumes[1].Super)
	f := im.mustOpen()
	got := f.Info().Volumes
	if len(got) != 2 || got[0] == got[1] {
		t.Errorf("Info.Volumes = %q: display names must be unique", got)
	}
	for i, want := range []string{"~unreadable~1", "~unreadable~1~1"} {
		if got[i] != want {
			t.Errorf("volume %d display = %q, want %q", i, got[i], want)
		}
	}
	for i := range got {
		if e, err := f.Lookup("/" + got[i]); err != nil || e.ID != fmt.Sprintf("n:%d:0:2", i) {
			t.Errorf("Lookup(%q) = %+v, %v", got[i], e, err)
		}
	}
}
