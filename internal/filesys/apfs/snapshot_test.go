package apfs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

// snapVolume is a volume whose live tree is files and whose snapshots are snaps.
func snapVolume(files []apfstest.File, snaps ...apfstest.Snapshot) apfstest.Volume {
	v := dataVolume(files...)
	v.Snapshots = snaps
	return v
}

func snapOpts(vols ...apfstest.Volume) apfstest.Options {
	return apfstest.Options{Blocks: 4096, Xid: 40, Volumes: vols}
}

// snapNames lists the entry names of the .snapshots directory of /Data.
func snapNames(t *testing.T, f *apfs.FS) []string {
	t.Helper()
	return entryNames(mustReadDir(t, f, mustLookup(t, f, "/Data/.snapshots")))
}

func ns(t time.Time) uint64 { return uint64(t.UnixNano()) }

func TestSnapshotsListed(t *testing.T) {
	c1 := time.Date(2024, 1, 2, 3, 4, 5, 6, time.UTC)
	ch1 := time.Date(2024, 1, 2, 4, 4, 5, 0, time.UTC)
	c2 := time.Date(2025, 5, 6, 7, 8, 9, 0, time.UTC)
	f, _ := openOpts(t, snapOpts(snapVolume(
		[]apfstest.File{{Path: "/live", Data: []byte("x")}},
		apfstest.Snapshot{Name: "first", CreateTime: ns(c1), ChangeTime: ns(ch1), Files: []apfstest.File{{Path: "/a", Data: []byte("a")}}},
		apfstest.Snapshot{Name: "second", CreateTime: ns(c2), ChangeTime: ns(c2), Files: []apfstest.File{{Path: "/b", Data: []byte("b")}}},
	)))
	noWarnings(t, f)

	vol := mustLookup(t, f, "/Data")
	if v, _ := attr(vol, "snapshots"); v != "2" {
		t.Errorf("volume attr snapshots = %q, want 2", v)
	}
	live := readDirAll(t, f, vol)
	sd := byName(t, live, ".snapshots")
	if sd.ID != "snaps:0" || sd.Type != filesys.TypeDir || !filesys.IsSnapshotsDir(sd) {
		t.Fatalf(".snapshots entry = %+v", sd)
	}
	if v, ok := attr(sd, "synthetic"); !ok || v != "snapshots" {
		t.Errorf("synthetic = %q, %v", v, ok)
	}
	if v, _ := attr(sd, "snapshots"); v != "2" {
		t.Errorf("snapshots = %q, want 2", v)
	}
	if got := mustLookup(t, f, "/Data/.snapshots"); got.ID != sd.ID {
		t.Errorf("Lookup(.snapshots) = %+v, want the listed entry", got)
	}

	es := mustReadDir(t, f, sd)
	if len(es) != 2 || es[0].Name != "first" || es[1].Name != "second" {
		t.Fatalf("snapshots = %v, want first, second in xid order", entryNames(es))
	}
	first, second := es[0], es[1]
	if first.ID != "n:0:2:2" || second.ID != "n:0:3:2" || first.Type != filesys.TypeDir || first.Deleted || first.Encrypted {
		t.Errorf("entries %+v %+v", first, second)
	}
	for _, c := range []struct {
		e                    filesys.Entry
		xid                  string
		created, changed     string
		wantMod, wantCreated time.Time
	}{
		{first, "2", c1.Format(time.RFC3339Nano), ch1.Format(time.RFC3339Nano), time.Unix(0, apfstest.DefaultTime).UTC(), time.Unix(0, apfstest.DefaultTime).UTC()},
		{second, "3", c2.Format(time.RFC3339Nano), c2.Format(time.RFC3339Nano), time.Unix(0, apfstest.DefaultTime).UTC(), time.Unix(0, apfstest.DefaultTime).UTC()},
	} {
		for k, want := range map[string]string{"snapshot": "true", "snapshot_xid": c.xid, "snapshot_created": c.created, "snapshot_changed": c.changed} {
			if v, ok := attr(c.e, k); !ok || v != want {
				t.Errorf("%s: attr %s = %q, %v; want %q", c.e.Name, k, v, ok, want)
			}
		}
		if _, ok := attr(c.e, "omap_snapshot"); ok {
			t.Errorf("%s: unexpected omap_snapshot attribute", c.e.Name)
		}
		// Times are the snapshot root inode's, not the metadata's.
		if !c.e.Times.Modified.T.Equal(c.wantMod) || !c.e.Times.Created.T.Equal(c.wantCreated) || !c.e.Times.Modified.ZoneKnown {
			t.Errorf("%s: times %+v, want the root inode's", c.e.Name, c.e.Times)
		}
		if got := mustLookup(t, f, "/Data/.snapshots/"+c.e.Name); got.ID != c.e.ID {
			t.Errorf("Lookup(%s) = %q, want %q", c.e.Name, got.ID, c.e.ID)
		}
	}
	// The snapshot root directories list the old trees.
	if got := entryNames(mustReadDir(t, f, first)); !slices.Equal(got, []string{"a"}) {
		t.Errorf("first lists %v", got)
	}
	if got := entryNames(mustReadDir(t, f, second)); !slices.Equal(got, []string{"b"}) {
		t.Errorf("second lists %v", got)
	}
	// The snapshots directory is a directory, not a file.
	if _, err := f.Open(sd); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open(.snapshots) = %v, want ErrUnsupported", err)
	}
	// The volume's own entry in the container root is unchanged.
	if got := entryNames(mustReadDir(t, f, f.Root())); !slices.Equal(got, []string{"Data"}) {
		t.Errorf("root lists %v", got)
	}
}

func TestSnapshotsDirIsShownWhenEmpty(t *testing.T) {
	f, _ := openOpts(t, snapOpts(dataVolume(apfstest.File{Path: "/a", Data: []byte("a")})))
	noWarnings(t, f)
	sd := byName(t, readDirAll(t, f, mustLookup(t, f, "/Data")), ".snapshots")
	if v, _ := attr(sd, "snapshots"); v != "0" {
		t.Errorf("snapshots = %q, want 0", v)
	}
	if es := mustReadDir(t, f, sd); len(es) != 0 {
		t.Errorf("empty volume lists %v", entryNames(es))
	}
	if _, err := f.Lookup("/Data/.snapshots/nothing"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup of a missing snapshot = %v, want ErrNotFound", err)
	}
	noWarnings(t, f)
}

// Review Focus 4: the live tree changed after the snapshot.
func TestSnapshotViewSeesOldContent(t *testing.T) {
	oldRewritten, newRewritten := pattern(2*bs+100, 1), pattern(3*bs, 2)
	oldGone, added := pattern(bs+7, 3), pattern(500, 4)
	oldDeep, newDeep := pattern(50, 5), pattern(60, 6)
	f, _ := openOpts(t, snapOpts(snapVolume(
		[]apfstest.File{
			{Path: "/rewritten", Data: newRewritten},
			{Path: "/added", Data: added},
			{Path: "/docs/deep.txt", Data: newDeep},
		},
		apfstest.Snapshot{Name: "S", Files: []apfstest.File{
			{Path: "/rewritten", Data: oldRewritten},
			{Path: "/gone", Data: oldGone},
			{Path: "/docs/deep.txt", Data: oldDeep},
		}},
	)))

	liveNames := entryNames(readDirAll(t, f, mustLookup(t, f, "/Data")))
	if want := []string{".snapshots", "added", "docs", "rewritten"}; !slices.Equal(liveNames, want) {
		t.Errorf("live view lists %v, want %v", liveNames, want)
	}
	snapNames := entryNames(mustReadDir(t, f, mustLookup(t, f, "/Data/.snapshots/S")))
	if want := []string{"docs", "gone", "rewritten"}; !slices.Equal(snapNames, want) {
		t.Errorf("snapshot view lists %v, want %v", snapNames, want)
	}

	read := func(p string) []byte {
		t.Helper()
		return readAllAt(t, openPath(t, f, p))
	}
	for _, c := range []struct {
		path string
		want []byte
	}{
		{"/Data/rewritten", newRewritten},
		{"/Data/added", added},
		{"/Data/docs/deep.txt", newDeep},
		{"/Data/.snapshots/S/rewritten", oldRewritten},
		{"/Data/.snapshots/S/gone", oldGone},
		{"/Data/.snapshots/S/docs/deep.txt", oldDeep},
	} {
		if got := read(c.path); !bytes.Equal(got, c.want) {
			t.Errorf("%s: content differs (got %d bytes, want %d)", c.path, len(got), len(c.want))
		}
	}
	for _, p := range []string{"/Data/gone", "/Data/.snapshots/S/added"} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%s) = %v, want ErrNotFound", p, err)
		}
	}
	// The old extents are not the live ones, and their runs reproduce the bytes.
	oldRuns := openPath(t, f, "/Data/.snapshots/S/rewritten").Runs()
	newRuns := openPath(t, f, "/Data/rewritten").Runs()
	if len(oldRuns) == 0 || slices.Equal(oldRuns, newRuns) {
		t.Errorf("runs old %v new %v: the views must map different blocks", oldRuns, newRuns)
	}
	if err := filesys.CheckRuns(oldRuns, int64(len(oldRewritten)), f.Info().Size); err != nil {
		t.Errorf("CheckRuns(old): %v", err)
	}
	noWarnings(t, f)

	// An entry built outside ReadDir opens in its view, from its ID alone.
	e := mustLookup(t, f, "/Data/.snapshots/S/rewritten")
	forged := filesys.Entry{ID: e.ID, Name: "x", Size: 1, Type: filesys.TypeDir, Deleted: true}
	fl, err := f.Open(forged)
	if err != nil || !bytes.Equal(readAllAt(t, fl), oldRewritten) {
		t.Errorf("Open(forged fields) = %v", err)
	}
}

func TestSnapshotViewIDsDistinctFromLive(t *testing.T) {
	files := []apfstest.File{{Path: "/d1/f", Data: []byte("1")}, {Path: "/d1/d2/g", Data: []byte("2")}}
	snap := func(name string) apfstest.Snapshot { return apfstest.Snapshot{Name: name, Files: files} }
	f, _ := openOpts(t, snapOpts(snapVolume(files, snap("s1"), snap("s2"))))
	dirs := map[string]string{} // ID to path
	err := filesys.Walk(f, f.Root(), "/", func(p string, e filesys.Entry, werr error) error {
		if werr != nil {
			t.Errorf("Walk reported %q: %v", p, werr)
			return nil
		}
		if e.Type == filesys.TypeDir {
			if prev, dup := dirs[e.ID]; dup {
				t.Errorf("directory ID %s is %q and %q", e.ID, prev, p)
			}
			dirs[e.ID] = p
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// Per view: the root directory, d1 and d1/d2; plus .snapshots.
	if len(dirs) != 3*3+1 {
		t.Errorf("%d directories visited, want 10: %v", len(dirs), dirs)
	}
	for _, view := range []string{"0", "2", "3"} {
		n := 0
		for id := range dirs {
			if strings.HasPrefix(id, "n:0:"+view+":") {
				n++
			}
		}
		if n != 3 {
			t.Errorf("view %s: %d directories, want 3", view, n)
		}
	}
	noWarnings(t, f)
}

func TestSnapshotDeletedFlagSkipped(t *testing.T) {
	for name, s := range map[string]apfstest.Snapshot{
		"deleted":  {Name: "gone", Deleted: true},
		"reverted": {Name: "gone", Reverted: true},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := openOpts(t, snapOpts(snapVolume(nil, apfstest.Snapshot{Name: "keep"}, s)))
			if got := snapNames(t, f); !slices.Equal(got, []string{"keep"}) {
				t.Fatalf("snapshots = %v, want only keep", got)
			}
			if !hasWarn(f, `"gone"`, "deleted or reverted") {
				t.Errorf("warnings %q", f.Info().Warnings)
			}
			if v, _ := attr(mustLookup(t, f, "/Data/.snapshots"), "snapshots"); v != "1" {
				t.Errorf("snapshots attr = %q, want 1", v)
			}
			// The omitted snapshot's view does not exist.
			if _, err := f.ReadDir(filesys.Entry{ID: "n:0:3:2"}); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("ReadDir of the omitted view = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestSnapshotMissingFromOmapTree(t *testing.T) {
	f, _ := openOpts(t, snapOpts(snapVolume(nil,
		apfstest.Snapshot{Name: "known", Files: []apfstest.File{{Path: "/k", Data: []byte("k")}}},
		apfstest.Snapshot{Name: "orphan", NoOmapSnapshot: true, Files: []apfstest.File{{Path: "/o", Data: []byte("o")}}},
	)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data/.snapshots"))
	if len(es) != 2 {
		t.Fatalf("snapshots = %v", entryNames(es))
	}
	if v, ok := attr(byName(t, es, "orphan"), "omap_snapshot"); !ok || v != "missing" {
		t.Errorf("orphan: omap_snapshot = %q, %v", v, ok)
	}
	if _, ok := attr(byName(t, es, "known"), "omap_snapshot"); ok {
		t.Errorf("known has an omap_snapshot attribute")
	}
	if !hasWarn(f, `"orphan"`, "no record in the object map's snapshot tree") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
	// It is still browsable.
	if got := entryNames(mustReadDir(t, f, byName(t, es, "orphan"))); !slices.Equal(got, []string{"o"}) {
		t.Errorf("orphan lists %v", got)
	}
}

func TestSnapshotBadSblockFallsBack(t *testing.T) {
	old := pattern(bs, 7)
	for name, s := range map[string]apfstest.Snapshot{
		"no copy":       {NoSblock: true},
		"bad checksum":  {BadSblock: true},
		"copy in image": {},
	} {
		t.Run(name, func(t *testing.T) {
			s.Name = "S"
			s.Files = []apfstest.File{{Path: "/old", Data: old}}
			f, _ := openOpts(t, snapOpts(snapVolume([]apfstest.File{{Path: "/new", Data: []byte("n")}}, s)))
			// Every view resolves through the volume object map at the snapshot's xid,
			// so the live root oid still finds the snapshot's tree.
			if got := entryNames(mustReadDir(t, f, mustLookup(t, f, "/Data/.snapshots/S"))); !slices.Equal(got, []string{"old"}) {
				t.Fatalf("snapshot lists %v", got)
			}
			if got := readAllAt(t, openPath(t, f, "/Data/.snapshots/S/old")); !bytes.Equal(got, old) {
				t.Errorf("content differs")
			}
			warned := hasWarn(f, `snapshot "S"`, "live root tree oid is used")
			if name == "copy in image" {
				noWarnings(t, f)
			} else if !warned {
				t.Errorf("warnings %q", f.Info().Warnings)
			}
		})
	}
}

func TestSnapshotXidOutOfRangeSkipped(t *testing.T) {
	f, _ := openOpts(t, snapOpts(snapVolume(nil,
		apfstest.Snapshot{Name: "ok"},
		apfstest.Snapshot{Name: "future", Xid: 999},
		apfstest.Snapshot{Name: "dup", Xid: 2},
	)))
	if got := snapNames(t, f); !slices.Equal(got, []string{"ok"}) {
		t.Errorf("snapshots = %v, want only ok", got)
	}
	if !hasWarn(f, `"future"`, "xid 999", "outside 1..") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
	if !hasWarn(f, "more than one snapshot with xid 2") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
	if _, err := f.ReadDir(filesys.Entry{ID: "n:0:999:2"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("ReadDir(view 999) = %v, want ErrNotFound", err)
	}
}

func TestSnapshotDuplicateNames(t *testing.T) {
	f, _ := openOpts(t, snapOpts(snapVolume(nil,
		apfstest.Snapshot{Name: "dup", Files: []apfstest.File{{Path: "/one", Data: []byte("1")}}},
		apfstest.Snapshot{Name: "dup", Files: []apfstest.File{{Path: "/two", Data: []byte("2")}}},
	)))
	es := mustReadDir(t, f, mustLookup(t, f, "/Data/.snapshots"))
	if len(es) != 2 || es[0].Name != "dup" || es[1].Name != "dup~3" {
		t.Fatalf("snapshots = %v, want dup and dup~3", entryNames(es))
	}
	if es[0].RawName != nil || string(es[1].RawName) != "dup" {
		t.Errorf("RawName = %q / %q, want none / dup", es[0].RawName, es[1].RawName)
	}
	for p, want := range map[string]string{"/Data/.snapshots/dup/one": "1", "/Data/.snapshots/dup~3/two": "2"} {
		if got := readAllAt(t, openPath(t, f, p)); string(got) != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
}

func TestSnapshotsDirNameCollision(t *testing.T) {
	f, _ := openOpts(t, snapOpts(snapVolume(
		[]apfstest.File{{Path: "/.snapshots/inner.txt", Data: []byte("real")}},
		apfstest.Snapshot{Name: "S", Files: []apfstest.File{{Path: "/s", Data: []byte("s")}}},
	)))
	es := readDirAll(t, f, mustLookup(t, f, "/Data"))
	alias := rawName([]byte(".snapshots"))
	if got := entryNames(es); !slices.Equal(got, slices.Sorted(slices.Values([]string{".snapshots", alias}))) {
		t.Fatalf("root lists %v, want the synthetic .snapshots and %s", got, alias)
	}
	synth, realDir := byName(t, es, ".snapshots"), byName(t, es, alias)
	if synth.ID != "snaps:0" || !filesys.IsSnapshotsDir(synth) {
		t.Errorf("synthetic entry %+v", synth)
	}
	if string(realDir.RawName) != ".snapshots" || filesys.IsSnapshotsDir(realDir) || realDir.ID == synth.ID {
		t.Errorf("real directory entry %+v", realDir)
	}
	// The synthetic one wins; the real one is reached through its alias.
	if got := mustLookup(t, f, "/Data/.snapshots"); got.ID != "snaps:0" {
		t.Errorf("Lookup(.snapshots) = %q", got.ID)
	}
	if got := readAllAt(t, openPath(t, f, "/Data/"+alias+"/inner.txt")); string(got) != "real" {
		t.Errorf("real file = %q", got)
	}
	if _, err := f.Lookup("/Data/.snapshots/inner.txt"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(/Data/.snapshots/inner.txt) = %v, want ErrNotFound", err)
	}
	// A case-insensitive volume does not fold onto the shadowed real directory.
	if _, err := f.Lookup("/Data/.SNAPSHOTS/S"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(.SNAPSHOTS) = %v, want ErrNotFound", err)
	}
}

func TestSnapshotOfEncryptedVolumeIsErrEncrypted(t *testing.T) {
	enc := apfstest.Volume{Name: "Enc", UUID: uuidOf(9), Encrypted: true, Snapshots: []apfstest.Snapshot{{Name: "S"}}}
	f, _ := openOpts(t, snapOpts(dataVolume(apfstest.File{Path: "/a", Data: []byte("a")}), enc))
	for what, call := range map[string]func() error{
		"Lookup .snapshots": func() error { _, err := f.Lookup("/Enc/.snapshots"); return err },
		"Lookup snapshot":   func() error { _, err := f.Lookup("/Enc/.snapshots/S"); return err },
		"ReadDir snaps":     func() error { _, err := f.ReadDir(filesys.Entry{ID: "snaps:1"}); return err },
		"ReadDir view":      func() error { _, err := f.ReadDir(filesys.Entry{ID: "n:1:2:2"}); return err },
		"Open in view":      func() error { _, err := f.Open(filesys.Entry{ID: "n:1:2:16"}); return err },
		"SnapshotPath":      func() error { _, err := f.SnapshotPath("/Enc/a", "S"); return err },
	} {
		if err := call(); !errors.Is(err, filesys.ErrEncrypted) {
			t.Errorf("%s = %v, want ErrEncrypted", what, err)
		}
	}
	// The unencrypted volume is unaffected.
	if got := readAllAt(t, openPath(t, f, "/Data/a")); string(got) != "a" {
		t.Errorf("Data/a = %q", got)
	}
}

func TestSnapshotPath(t *testing.T) {
	odd := []byte{0xff, 'x'}
	one := snapOpts(snapVolume(nil,
		apfstest.Snapshot{Name: "daily"},
		apfstest.Snapshot{Name: "weird\nname"},
		apfstest.Snapshot{RawName: odd},
	))
	f, _ := openOpts(t, one)
	oddAlias := rawName(odd)
	for _, c := range []struct{ p, snap, want string }{
		{"/Data/docs/a.txt", "daily", "/Data/.snapshots/daily/docs/a.txt"},
		{"Data/docs//a.txt/", "daily", "/Data/.snapshots/daily/docs/a.txt"},
		{"/Data", "daily", "/Data/.snapshots/daily"},
		{"/", "daily", "/Data/.snapshots/daily"}, // exactly one volume
		{"", "daily", "/Data/.snapshots/daily"},
		{"/Data/x", oddAlias, "/Data/.snapshots/" + oddAlias + "/x"},
		{"/Data/x", rawName([]byte("daily")), "/Data/.snapshots/daily/x"},
		{"/" + rawName([]byte("Data")) + "/x", "daily", "/Data/.snapshots/daily/x"},
	} {
		got, err := f.SnapshotPath(c.p, c.snap)
		if err != nil || got != c.want {
			t.Errorf("SnapshotPath(%q, %q) = %q, %v; want %q", c.p, c.snap, got, err, c.want)
		}
	}
	// The result is a real path.
	p, _ := f.SnapshotPath("/Data", "daily")
	if got := mustLookup(t, f, p); got.Name != "daily" {
		t.Errorf("Lookup(%q) = %+v", p, got)
	}

	// Unknown snapshot: ErrNotFound, names quoted so nothing injects a line.
	_, err := f.SnapshotPath("/Data/x", "nope\n\x1b[31m")
	if !errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("unknown snapshot: %v, want ErrNotFound", err)
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\n\x1b") || !strings.Contains(msg, `"daily"`) || !strings.Contains(msg, `"weird\nname"`) {
		t.Errorf("message %q must quote every name and hold no raw control byte", msg)
	}
	if _, err := f.SnapshotPath("/Nope/x", "daily"); !errors.Is(err, filesys.ErrNotFound) || strings.Contains(err.Error(), "\n") {
		t.Errorf("unknown volume: %v, want ErrNotFound", err)
	}
	if _, err := f.SnapshotPath("/Data/x", ""); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("empty snapshot name: %v, want ErrNotFound", err)
	}

	t.Run("many snapshots", func(t *testing.T) {
		var snaps []apfstest.Snapshot
		for i := range 25 {
			snaps = append(snaps, apfstest.Snapshot{Name: fmt.Sprintf("s%02d", i)})
		}
		f, _ := openOpts(t, snapOpts(snapVolume(nil, snaps...)))
		_, err := f.SnapshotPath("/Data", "missing")
		if !errors.Is(err, filesys.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
		if n := strings.Count(err.Error(), `"s`); n != 20 || !strings.Contains(err.Error(), "and 5 more") {
			t.Errorf("message lists %d names: %v", n, err)
		}
	})
	t.Run("several volumes", func(t *testing.T) {
		other := apfstest.Volume{Name: "Other", UUID: uuidOf(4)}
		f, _ := openOpts(t, snapOpts(snapVolume(nil, apfstest.Snapshot{Name: "daily"}), other))
		if _, err := f.SnapshotPath("/", "daily"); !errors.Is(err, filesys.ErrNeedsVolume) {
			t.Errorf("SnapshotPath(/) = %v, want ErrNeedsVolume", err)
		}
		if got, err := f.SnapshotPath("/Data/x", "daily"); err != nil || got != "/Data/.snapshots/daily/x" {
			t.Errorf("SnapshotPath = %q, %v", got, err)
		}
		if _, err := f.SnapshotPath("/Other/x", "daily"); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("a snapshot of another volume: %v, want ErrNotFound", err)
		}
	})
}

func TestSnapshotterInterface(_ *testing.T) {
	var _ filesys.Snapshotter = (*apfs.FS)(nil)
}

func TestSnapshotForgedViewIDsAreRejectedQuickly(t *testing.T) {
	f, _ := openOpts(t, snapOpts(snapVolume([]apfstest.File{{Path: "/a", Data: []byte("a")}}, apfstest.Snapshot{Name: "S"})))
	scans := f.Scans()
	for _, id := range []string{"n:0:77:2", "n:0:1:2", "n:0:4:2", "n:5:2:2", "n:0:18446744073709551615:2", "n:0:02:2", "n:0:2:0"} {
		if _, err := f.ReadDir(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("ReadDir(%s) = %v, want ErrNotFound", id, err)
		}
		if _, err := f.Open(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Open(%s) = %v, want ErrNotFound", id, err)
		}
	}
	if got := f.Scans(); got != scans {
		t.Errorf("forged IDs cost %d tree scans", got-scans)
	}
	// A real view opens.
	if _, err := f.ReadDir(filesys.Entry{ID: "n:0:2:2"}); err != nil {
		t.Errorf("ReadDir(n:0:2:2): %v", err)
	}
}

// Hostile snapshot metadata records are skipped with a warning (or ignored),
// never trusted and never a panic; the good snapshot stays.
func TestSnapshotMetadataHostile(t *testing.T) {
	long := strings.Repeat("n", 300)
	put16 := func(v []byte, off int, n uint16) []byte {
		v = slices.Clone(v)
		v[off], v[off+1] = byte(n), byte(n>>8)
		return v
	}
	for _, c := range []struct {
		name string
		snap apfstest.Snapshot
		mut  func(k, v []byte) ([]byte, []byte)
		warn string
	}{
		{"name_len 0", apfstest.Snapshot{}, func(k, v []byte) ([]byte, []byte) { return k, put16(v, 48, 0) }, "name length does not fit"},
		{"name_len past the value", apfstest.Snapshot{}, func(k, v []byte) ([]byte, []byte) { return k, put16(v, 48, 0xffff) }, "name length does not fit"},
		{"name not NUL-terminated", apfstest.Snapshot{}, func(k, v []byte) ([]byte, []byte) { v = slices.Clone(v); v[len(v)-1] = 'x'; return k, v }, "NUL-terminated"},
		{"value too short", apfstest.Snapshot{}, func(k, v []byte) ([]byte, []byte) { return k, v[:49] }, "shorter than the 50 fixed bytes"},
		{"key too long", apfstest.Snapshot{}, func(k, v []byte) ([]byte, []byte) { return append(slices.Clone(k), 1, 2, 3), v }, "is not a snapshot metadata key"},
		{"empty name", apfstest.Snapshot{}, func(k, v []byte) ([]byte, []byte) { v = put16(v, 48, 1); v[50] = 0; return k, v }, "name is empty"},
		{"name too long", apfstest.Snapshot{Name: long}, nil, "longer than 255"},
		{"name with a NUL", apfstest.Snapshot{Name: "a\x00b"}, nil, "holds a NUL"},
		{"unknown record type", apfstest.Snapshot{}, func(k, v []byte) ([]byte, []byte) { k = slices.Clone(k); k[7] = k[7]&0x0f | 5<<4; return k, v }, "of type 5 is ignored"},
	} {
		t.Run(c.name, func(t *testing.T) {
			bad := c.snap
			if bad.Name == "" {
				bad.Name = "bad"
			}
			bad.MutateMeta = c.mut
			f, _ := openOpts(t, snapOpts(snapVolume(nil, apfstest.Snapshot{Name: "keep"}, bad)))
			if got := snapNames(t, f); !slices.Equal(got, []string{"keep"}) {
				t.Errorf("snapshots = %q, want only keep", got)
			}
			if !hasWarn(f, c.warn) {
				t.Errorf("warnings %q lack %q", f.Info().Warnings, c.warn)
			}
			for _, w := range f.Info().Warnings {
				if strings.ContainsAny(w, "\x00\n\x1b") {
					t.Errorf("warning carries raw bytes: %q", w)
				}
			}
		})
	}
}

func TestSnapshotViewsAreConcurrentSafe(t *testing.T) {
	data := pattern(2*bs, 8)
	f, _ := openOpts(t, snapOpts(snapVolume(
		[]apfstest.File{{Path: "/f", Data: []byte("live")}},
		apfstest.Snapshot{Name: "A", Files: []apfstest.File{{Path: "/f", Data: data}}},
		apfstest.Snapshot{Name: "B", Files: []apfstest.File{{Path: "/f", Data: data}}},
	)))
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				for _, p := range []string{"/Data/.snapshots/A/f", "/Data/.snapshots/B/f"} {
					e, err := f.Lookup(p)
					if err != nil {
						t.Errorf("goroutine %d: Lookup(%s): %v", g, p, err)
						return
					}
					fl, err := f.Open(e)
					if err != nil {
						t.Errorf("goroutine %d: Open(%s): %v", g, p, err)
						return
					}
					buf := make([]byte, fl.Size())
					if n, err := fl.ReadAt(buf, 0); n != len(buf) && !errors.Is(err, io.EOF) || !bytes.Equal(buf, data) {
						t.Errorf("goroutine %d: %s read %d, %v", g, p, n, err)
						return
					}
				}
				if _, err := f.SnapshotPath("/Data/f", "A"); err != nil {
					t.Errorf("goroutine %d: %v", g, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
