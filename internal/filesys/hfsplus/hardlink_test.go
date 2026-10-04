package hfsplus_test

import (
	"bytes"
	"errors"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

func linkTree() []hfsplustest.File {
	data := pattern(9000, 20)
	return []hfsplustest.File{
		{Path: "/a", Dir: true},
		{Path: "/b", Dir: true},
		{Path: "/a/first.txt", HardLink: "g", Data: data, Mode: 0o100600, UID: 7, GID: 8, Times: sampleTimes, FileType: "TEXT", FileCreator: "ttxt"},
		{Path: "/b/second.txt", HardLink: "g", Mode: 0o100755, UID: 9, GID: 9, Times: fileTimes(1, 2, 3, 4, 5)},
		{Path: "/b/third.txt", HardLink: "g"},
		{Path: "/b/solo.txt", HardLink: "h", Data: []byte("solo")},
		{Path: "/plain.txt", Data: []byte("plain")},
	}
}

// privateFolder finds the private metadata folder in the root listing.
func privateFolder(t testing.TB, f *hfsplus.FS) filesys.Entry {
	t.Helper()
	for _, e := range readDir(t, f, f.Root()) {
		if attrOf(e, "private_metadata") == "true" {
			return e
		}
	}
	t.Fatal("no private metadata folder in the root")
	return filesys.Entry{}
}

func TestHardLinkResolvesToIndirectNode(t *testing.T) {
	data := pattern(9000, 20)
	img, lay, f := buildTree(t, hfsplustest.Options{}, linkTree())
	inode := lay.Inodes["g"]
	first, second, third := lay.CNIDs["/a/first.txt"], lay.CNIDs["/b/second.txt"], lay.CNIDs["/b/third.txt"]
	if inode == 0 || lay.PrivateFolder == 0 || inode == first {
		t.Fatalf("layout: inode %d private %d", inode, lay.PrivateFolder)
	}
	var seen []filesys.Entry
	for _, c := range []struct{ path, name string }{{"/a/first.txt", "first.txt"}, {"/b/second.txt", "second.txt"}, {"/b/third.txt", "third.txt"}} {
		e, fl := openPath(t, f, c.path)
		seen = append(seen, e)
		if e.Name != c.name || e.ID != idOf(lay.CNIDs[c.path]) {
			t.Errorf("%s: name %q id %s (the link record's own name and cnid)", c.path, e.Name, e.ID)
		}
		// Everything else comes from the iNode.
		if e.Size != 9000 || e.Mode != 0o100600 || e.UID != 7 || e.GID != 8 || e.Type != filesys.TypeFile {
			t.Errorf("%s: size %d mode %o uid %d gid %d type %v", c.path, e.Size, e.Mode, e.UID, e.GID, e.Type)
		}
		if got := e.Times.Modified.T.Unix(); got != int64(sampleTimes.ContentMod)-epoch {
			t.Errorf("%s: mtime %d, the inode's is %d", c.path, got, int64(sampleTimes.ContentMod)-epoch)
		}
		if attrOf(e, "hardlink") != "file" || attrOf(e, "hardlink_inode") != strconv.Itoa(int(inode)) || attrOf(e, "links") != "3" {
			t.Errorf("%s: attrs %+v", c.path, e.Attrs)
		}
		if !bytes.Equal(readAll(t, fl), data) {
			t.Errorf("%s: content differs", c.path)
		}
		if err := filesys.CheckRuns(fl.Runs(), fl.Size(), f.Info().Size); err != nil {
			t.Errorf("%s: %v", c.path, err)
		}
		if got := readRuns(img, fl.Runs()); !bytes.Equal(got, data) {
			t.Errorf("%s: runs differ", c.path)
		}
	}
	if seen[0].ID == seen[1].ID || seen[1].ID == seen[2].ID || seen[0].ID == seen[2].ID {
		t.Error("link entries share an id")
	}
	if first == second || second == third {
		t.Error("builder cnids")
	}
	// ReadDir gives the same entries as Lookup.
	if got := child(t, readDir(t, f, child(t, readDir(t, f, f.Root()), "b")), "second.txt"); got.ID != seen[1].ID || attrOf(got, "links") != "3" {
		t.Errorf("ReadDir entry = %+v", got)
	}
	// The iNode<N> itself, in the private folder: same content, size, mode and times.
	pf := privateFolder(t, f)
	if pf.ID != idOf(lay.PrivateFolder) || pf.Type != filesys.TypeDir {
		t.Fatalf("private folder entry = %+v", pf)
	}
	var in filesys.Entry
	for _, e := range readDir(t, f, pf) {
		if e.Name == "iNode"+strconv.Itoa(int(inode)) {
			in = e
		}
	}
	if in.ID != idOf(inode) {
		t.Fatalf("iNode entry = %+v", in)
	}
	fl, err := f.Open(in)
	if err != nil || !bytes.Equal(readAll(t, fl), data) || in.Size != 9000 || in.Mode != 0o100600 || in.Times.Modified != seen[0].Times.Modified {
		t.Errorf("iNode: %+v, %v", in, err)
	}
	if _, ok := attr(in, "hardlink"); ok {
		t.Errorf("the iNode itself is not a link: %+v", in.Attrs)
	}
	// A group of one.
	if e, _ := f.Lookup("/b/solo.txt"); attrOf(e, "links") != "1" || e.Size != 4 {
		t.Errorf("solo: %+v", e)
	}
	if e, _ := f.Lookup("/plain.txt"); attrOf(e, "hardlink") != "" {
		t.Errorf("plain: %+v", e.Attrs)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings = %q", w)
	}
}

func TestHardLinkPrivateFolderPrefixVariants(t *testing.T) {
	for _, prefix := range []uint16{0, 0x2400, 0x200B} {
		for _, o := range []hfsplustest.Options{{PrivatePrefix: prefix}, {PrivatePrefix: prefix, HFSX: true, CaseSensitive: true}} {
			_, _, f := buildTree(t, o, linkTree())
			e, fl := openPath(t, f, "/a/first.txt")
			if attrOf(e, "hardlink") != "file" || !bytes.Equal(readAll(t, fl), pattern(9000, 20)) {
				t.Errorf("prefix %#x %+v: attrs %+v", prefix, o, e.Attrs)
			}
		}
	}
}

func TestHardLinkDanglingIsFlagged(t *testing.T) {
	for name, tc := range map[string]struct {
		files []hfsplustest.File
		path  string
	}{
		"no private folder": {[]hfsplustest.File{{Path: "/dangling", LinkInode: 9999}}, "/dangling"},
		"a user folder with the same name is not the private folder": {[]hfsplustest.File{
			{Path: "/HFS+ Private Data", Dir: true},
			{Path: "/HFS+ Private Data/iNode9999", Data: []byte("not an inode")},
			{Path: "/dangling", LinkInode: 9999},
		}, "/dangling"},
		"no such iNode": {append(linkTree(), linkFile("/gone", 77777)), "/gone"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, f := buildTree(t, hfsplustest.Options{}, tc.files)
			e, err := f.Lookup(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if attrOf(e, "hardlink") != "dangling" {
				t.Errorf("attrs = %+v", e.Attrs)
			}
			fl, err := f.Open(e)
			wantCorrupt(t, err)
			if fl != nil || errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("Open = %v, %v; want a CorruptError (not ErrNotFound)", fl, err)
			}
			// The other links of a damaged volume still work.
			if name == "no such iNode" {
				if _, fl := openPath(t, f, "/a/first.txt"); fl.Size() != 9000 {
					t.Error("a good link stopped working")
				}
			}
		})
	}
}

func linkFile(path string, inode uint32) hfsplustest.File {
	return hfsplustest.File{Path: path, LinkInode: inode}
}

func TestHardLinkChainIsCorrupt(t *testing.T) {
	img, lay, _ := buildTree(t, hfsplustest.Options{}, linkTree())
	inode := lay.Inodes["h"]
	name := []uint16{'i', 'N', 'o', 'd', 'e'}
	for _, c := range strconv.Itoa(int(inode)) {
		name = append(name, uint16(c))
	}
	p := findRec(t, img, lay, lay.PrivateFolder, name)
	copy(img[p.data+48:], "hlnk")
	copy(img[p.data+52:], "hfs+")
	be.PutUint32(img[p.data+44:], inode+1) // the iNode is itself a link record
	f := open(t, img)
	e, err := f.Lookup("/b/solo.txt")
	if err != nil {
		t.Fatal(err)
	}
	if v := attrOf(e, "hardlink"); v != "invalid" {
		t.Errorf("hardlink = %q, attrs %+v", v, e.Attrs)
	}
	if _, err := f.Open(e); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Open of a chained link = %v", err)
	}
	if !hasWarning(f.Info(), "hard link") {
		t.Errorf("warnings = %q", f.Info().Warnings)
	}
	// The unrelated group is fine.
	if _, fl := openPath(t, f, "/a/first.txt"); fl.Size() != 9000 {
		t.Error("an unrelated link stopped working")
	}
}

func TestDirectoryHardLinkIsNotFollowed(t *testing.T) {
	_, _, f := buildTree(t, hfsplustest.Options{}, []hfsplustest.File{
		{Path: "/dlink", FileType: "fldr", FileCreator: "MACS", Special: 321, Mode: 0o100755},
		{Path: "/dir", Dir: true},
	})
	e, err := f.Lookup("/dlink")
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != filesys.TypeOther || attrOf(e, "hardlink") != "dir" || attrOf(e, "hardlink_inode") != "321" {
		t.Errorf("entry = %+v", e)
	}
	if _, err := f.Open(e); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("Open = %v", err)
	}
	if _, err := f.ReadDir(e); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("ReadDir = %v", err)
	}
	// It is still listed (and so does not break the walk).
	if got := nameList(readDir(t, f, f.Root())); len(got) != 2 {
		t.Errorf("listing = %v", got)
	}
}
