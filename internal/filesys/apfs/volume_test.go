package apfs_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

// Test helpers shared by the volume, directory and ID tests.

const (
	roleData   = 1 << 6
	roleSystem = 1
)

func volOpts(vols ...apfstest.Volume) apfstest.Options {
	return apfstest.Options{Blocks: 1024, Volumes: vols}
}

func uuidOf(b byte) [16]byte {
	var u [16]byte
	for i := range u {
		u[i] = b + byte(i)
	}
	return u
}

// richFiles is a small tree: a directory with files, a hard link, a symlink and
// an empty file.
func richFiles() []apfstest.File {
	return []apfstest.File{
		{Path: "/docs", Dir: true, Mode: 0o750, UID: 501, GID: 20},
		{
			Path: "/docs/a.txt", Data: []byte("hello"), Mode: 0o640, UID: 501, GID: 20,
			Times: apfstest.Times{Create: 1_600_000_000_000_000_001, Modify: 1_600_000_000_000_000_002, Change: 1_600_000_000_000_000_003, Access: 1_600_000_000_000_000_004},
		},
		{Path: "/docs/b.txt", Data: []byte("0123456789")},
		{Path: "/docs/link", LinkTo: "/docs/a.txt"},
		{Path: "/sym", Symlink: "docs/a.txt"},
		{Path: "/empty"},
	}
}

func dataVolume(files ...apfstest.File) apfstest.Volume {
	return apfstest.Volume{Name: "Data", UUID: uuidOf(1), Role: roleData, CaseInsensitive: true, NormInsensitive: true, HashedKeys: true, Files: files}
}

func openOpts(t *testing.T, o apfstest.Options) (*apfs.FS, *image) {
	t.Helper()
	im := newImage(t, o)
	return im.mustOpen(), im
}

func mustLookup(t *testing.T, f *apfs.FS, p string) filesys.Entry {
	t.Helper()
	e, err := f.Lookup(p)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", p, err)
	}
	return e
}

// mustReadDir lists dir without the synthetic .snapshots directory every volume
// root carries (the snapshot tests use readDirAll to see it).
func mustReadDir(t *testing.T, f *apfs.FS, dir filesys.Entry) []filesys.Entry {
	t.Helper()
	return slices.DeleteFunc(readDirAll(t, f, dir), filesys.IsSnapshotsDir)
}

// readDirAll lists dir as ReadDir returns it.
func readDirAll(t *testing.T, f *apfs.FS, dir filesys.Entry) []filesys.Entry {
	t.Helper()
	es, err := f.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir.ID, err)
	}
	return es
}

func entryNames(es []filesys.Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	slices.Sort(out)
	return out
}

func byName(t *testing.T, es []filesys.Entry, name string) filesys.Entry {
	t.Helper()
	for _, e := range es {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %q in %v", name, entryNames(es))
	return filesys.Entry{}
}

func attr(e filesys.Entry, key string) (string, bool) {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return "", false
}

func attrs(e filesys.Entry, key string) []string {
	var out []string
	for _, kv := range e.Attrs {
		if kv.Key == key {
			out = append(out, kv.Value)
		}
	}
	return out
}

func rawName(b []byte) string { return "~raw~" + base64.RawURLEncoding.EncodeToString(b) }

func TestOpenVolumes(t *testing.T) {
	data := dataVolume(richFiles()...)
	data.NumSnapshots = 2
	sys := apfstest.Volume{Name: "System", UUID: uuidOf(0x40), Role: roleSystem, Files: []apfstest.File{{Path: "/x"}}}
	f, im := openOpts(t, volOpts(data, sys))

	in := f.Info()
	if want := []string{"Data", "System"}; !slices.Equal(in.Volumes, want) {
		t.Errorf("Info.Volumes = %q, want %q", in.Volumes, want)
	}
	if in.Encrypted {
		t.Error("Info.Encrypted set with no encrypted volume")
	}
	for _, want := range []string{
		`vol[0] "Data": role=data, case-insensitive, normalization-insensitive, unencrypted, snapshots=2`,
		`vol[1] "System": role=system, unencrypted, snapshots=0`,
	} {
		if !contains(in.Features, want) {
			t.Errorf("Info.Features lacks %q: %q", want, in.Features)
		}
	}
	if len(in.Warnings) != 0 {
		t.Errorf("warnings: %q", in.Warnings)
	}

	v0, ok := f.VolumeFields(0)
	if !ok || !v0.Readable || string(v0.Name) != "Data" || v0.UUID != uuidOf(1) || v0.Role != roleData ||
		v0.Incompat != 0x9 || v0.FsFlags != 1 || v0.FsIndex != 0 || v0.Snapshots != 2 ||
		!v0.CaseInsensitive || !v0.NormInsn || v0.Encrypted || v0.Sealed || v0.Features != 2 {
		t.Errorf("volume 0 = %+v", v0)
	}
	v1, _ := f.VolumeFields(1)
	if v1.Incompat != 0 || v1.CaseInsensitive || v1.Role != roleSystem || v1.FsIndex != 1 {
		t.Errorf("volume 1 = %+v", v1)
	}
	if _, ok := f.VolumeFields(2); ok {
		t.Error("a volume in an empty slot")
	}

	// Unknown incompatible bits are a warning; the volume is still read.
	o := volOpts(apfstest.Volume{Name: "V", Incompat: 0x100})
	f2, _ := openOpts(t, o)
	if !hasWarn(f2, "unknown incompatible feature bits 0x100") {
		t.Errorf("warnings: %q", f2.Info().Warnings)
	}
	if es := mustReadDir(t, f2, f2.Root()); len(es) != 1 {
		t.Errorf("root lists %d entries", len(es))
	}

	// A sealed volume is flagged and warned about (its hashes are not verified).
	o = volOpts(apfstest.Volume{Name: "S", Sealed: true, TreeMaxKeys: 2, Files: []apfstest.File{{Path: "/a"}, {Path: "/b"}, {Path: "/c"}}})
	f3, _ := openOpts(t, o)
	if !hasWarn(f3, "sealed") || !contains(f3.Info().Features, `vol[0] "S": role=none, unencrypted, sealed, snapshots=0`) {
		t.Errorf("sealed volume: %q %q", f3.Info().Warnings, f3.Info().Features)
	}
	if got := entryNames(mustReadDir(t, f3, mustLookup(t, f3, "/S"))); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("sealed volume lists %q", got)
	}

	// Slots beyond nx_max_file_systems are ignored with a warning.
	im.patchSupers(func(b []byte) { le.PutUint32(b[offMaxFS:], 1) })
	f4 := im.mustOpen()
	if got := f4.Info().Volumes; !slices.Equal(got, []string{"Data"}) || !hasWarn(f4, "beyond nx_max_file_systems") {
		t.Errorf("volumes %q warnings %q", got, f4.Info().Warnings)
	}

	// A volume whose apfs_fs_index is not its slot warns.
	vb := im.blk(im.g.Volumes[1].Super)
	le.PutUint32(vb[36:], 7)
	im.seal(im.g.Volumes[1].Super)
	im.patchSupers(func(b []byte) { le.PutUint32(b[offMaxFS:], 2) })
	f5 := im.mustOpen()
	if !hasWarn(f5, "apfs_fs_index 7") {
		t.Errorf("warnings: %q", f5.Info().Warnings)
	}
}

func TestRootListsVolumes(t *testing.T) {
	data := dataVolume(richFiles()...)
	data.NumSnapshots = 3
	data.LastModTime = 1_500_000_000_000_000_000
	sys := apfstest.Volume{Name: "System", UUID: uuidOf(0x40), Role: roleSystem}
	f, im := openOpts(t, volOpts(data, sys))

	root := f.Root()
	if root.ID != "apfs:root" || root.Type != filesys.TypeDir {
		t.Fatalf("Root = %+v", root)
	}
	es := mustReadDir(t, f, root)
	if len(es) != 2 {
		t.Fatalf("root lists %d volumes", len(es))
	}
	d := es[0]
	if d.Name != "Data" || d.ID != "n:0:0:2" || d.Type != filesys.TypeDir || d.Encrypted || d.Deleted || d.RawName != nil {
		t.Errorf("volume 0 entry = %+v", d)
	}
	wantAttrs := []filesys.KV{
		{Key: "volume", Value: "0"},
		{Key: "uuid", Value: "01020304-0506-0708-090a-0b0c0d0e0f10"},
		{Key: "role", Value: "data"},
		{Key: "case_insensitive", Value: "true"},
		{Key: "normalization_insensitive", Value: "true"},
		{Key: "encrypted", Value: "false"},
		{Key: "snapshots", Value: "3"},
	}
	if !slices.Equal(d.Attrs, wantAttrs) {
		t.Errorf("volume 0 attrs = %v, want %v", d.Attrs, wantAttrs)
	}
	// Times come from the volume's root inode: the builder's default clock.
	for name, ts := range map[string]filesys.Timestamp{"modified": d.Times.Modified, "accessed": d.Times.Accessed, "changed": d.Times.Changed, "created": d.Times.Created} {
		if ts.T.UnixNano() != apfstest.DefaultTime || !ts.ZoneKnown {
			t.Errorf("%s = %+v", name, ts)
		}
	}
	if !d.Times.Deleted.T.IsZero() {
		t.Error("a Deleted time on a volume")
	}
	if d.Mode != 0o40755 {
		t.Errorf("volume mode %#o", d.Mode)
	}
	s := es[1]
	if s.Name != "System" || s.ID != "n:1:0:2" {
		t.Errorf("volume 1 entry = %+v", s)
	}
	if v, _ := attr(s, "role"); v != "system" {
		t.Errorf("role = %q", v)
	}
	if lk := mustLookup(t, f, "/Data"); lk.ID != d.ID || lk.Name != "Data" {
		t.Errorf("Lookup(/Data) = %+v", lk)
	}
	if lk := mustLookup(t, f, "/"); lk.ID != root.ID {
		t.Errorf("Lookup(/) = %+v", lk)
	}

	// Without a readable root inode the volume time falls back to
	// apfs_last_mod_time, as Modified only.
	data.Reorder = func(r []apfstest.FSRecord) []apfstest.FSRecord {
		return slices.DeleteFunc(r, func(x apfstest.FSRecord) bool { return x.ID == 2 && x.Type == apfstest.TypeInode })
	}
	f2, _ := openOpts(t, volOpts(data))
	e := mustReadDir(t, f2, f2.Root())[0]
	if e.Times.Modified.T.UnixNano() != 1_500_000_000_000_000_000 || !e.Times.Created.T.IsZero() || !e.Times.Accessed.T.IsZero() || !e.Times.Changed.T.IsZero() {
		t.Errorf("fallback times = %+v", e.Times)
	}
	if !hasWarn(f2, "root inode cannot be read") {
		t.Errorf("warnings: %q", f2.Info().Warnings)
	}
	_ = im
}

func TestVolumeNameEdgeCases(t *testing.T) {
	bad := []byte{0xff, 'x'}
	vols := []apfstest.Volume{
		{Name: "Data"}, {Name: "Data"}, {Name: "."}, {Name: ".."}, {Name: "a/b"}, {RawName: bad},
	}
	f, _ := openOpts(t, volOpts(vols...))
	es := mustReadDir(t, f, f.Root())
	want := []struct {
		name string
		raw  []byte
	}{
		{"Data", nil},
		{"Data~1", []byte("Data")},
		{rawName([]byte(".")), []byte(".")},
		{rawName([]byte("..")), []byte("..")},
		{rawName([]byte("a/b")), []byte("a/b")},
		{rawName(bad), bad},
	}
	if len(es) != len(want) {
		t.Fatalf("%d volumes: %q", len(es), entryNames(es))
	}
	for i, w := range want {
		if es[i].Name != w.name || !bytes.Equal(es[i].RawName, w.raw) || (w.raw == nil) != (es[i].RawName == nil) {
			t.Errorf("volume %d: name %q raw %q, want %q %q", i, es[i].Name, es[i].RawName, w.name, w.raw)
		}
		// Lookup accepts the displayed form and it names that volume.
		if got := mustLookup(t, f, "/"+w.name); got.ID != es[i].ID {
			t.Errorf("Lookup(%q) = %s, want %s", w.name, got.ID, es[i].ID)
		}
		// The displayed raw form re-encodes to the stored bytes.
		if es[i].RawName != nil && strings.HasPrefix(es[i].Name, "~raw~") {
			if got, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(es[i].Name, "~raw~")); err != nil || !bytes.Equal(got, es[i].RawName) {
				t.Errorf("volume %d display %q does not re-encode to %q", i, es[i].Name, es[i].RawName)
			}
		}
	}
	if want := []string{"Data", "Data~1", rawName([]byte(".")), rawName([]byte("..")), rawName([]byte("a/b")), rawName(bad)}; !slices.Equal(f.Info().Volumes, want) {
		t.Errorf("Info.Volumes = %q", f.Info().Volumes)
	}
	// An alias of the stored name finds the first volume with it.
	if got := mustLookup(t, f, "/"+rawName([]byte("Data"))); got.ID != "n:0:0:2" {
		t.Errorf("alias of Data -> %s", got.ID)
	}
	// "." and ".." are not volumes; non-canonical aliases and case folds miss.
	for _, p := range []string{"/.", "/..", "/data", "/~raw~Lg==", "/~raw~Lg=", "/~raw~", "/a", "/Data~2"} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", p, err)
		}
	}
	if len(f.Info().Warnings) != 0 {
		t.Errorf("warnings: %q", f.Info().Warnings)
	}
}

// countingReader records the blocks read.
type countingReader struct {
	b  []byte
	bs int
	mu chan struct{}
	r  map[uint64]int
}

func newCountingReader(b []byte, bs int) *countingReader {
	return &countingReader{b: b, bs: bs, mu: make(chan struct{}, 1), r: map[uint64]int{}}
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.mu <- struct{}{}
	for blk := uint64(off) / uint64(c.bs); blk*uint64(c.bs) < uint64(off)+uint64(len(p)); blk++ {
		c.r[blk]++
	}
	<-c.mu
	return bytes.NewReader(c.b).ReadAt(p, off)
}

func (c *countingReader) touched(lo, hi uint64) bool {
	for b := range c.r {
		if b >= lo && b < hi {
			return true
		}
	}
	return false
}

func TestEncryptedVolumeIsErrEncrypted(t *testing.T) {
	plain := dataVolume(richFiles()...)
	enc := apfstest.Volume{Name: "Enc", UUID: uuidOf(9), Role: roleData, Encrypted: true, Files: []apfstest.File{{Path: "/secret", Data: []byte("x")}, {Path: "/d", Dir: true}}}
	o := volOpts(plain, enc)
	im := newImage(t, o)
	eg := im.g.Volumes[1]
	// The encrypted volume's object map and tree are garbage the reader must
	// never look at.
	for b := eg.Omap; b < eg.End; b++ {
		for i := range im.blk(b) {
			im.blk(b)[i] = 0xA5
		}
	}
	cr := newCountingReader(im.b, im.bs)
	f, err := apfs.Open(cr, int64(len(im.b)))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Info().Encrypted {
		t.Error("Info.Encrypted not set")
	}
	if !contains(f.Info().Features, `vol[1] "Enc": role=data, encrypted, snapshots=0`) {
		t.Errorf("features: %q", f.Info().Features)
	}
	es := mustReadDir(t, f, f.Root())
	if len(es) != 2 || es[0].Encrypted || !es[1].Encrypted || es[1].Type != filesys.TypeDir || es[1].ID != "n:1:0:2" {
		t.Fatalf("root = %+v", es)
	}
	if v, _ := attr(es[1], "encrypted"); v != "true" {
		t.Errorf("encrypted attr = %q", v)
	}
	if !es[1].Times.Created.T.IsZero() {
		t.Error("an encrypted volume shows inode times")
	}

	isEnc := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, filesys.ErrEncrypted) {
			t.Errorf("%s: %v, want ErrEncrypted", what, err)
		} else if !strings.Contains(err.Error(), `"Enc"`) {
			t.Errorf("%s: the error does not name the volume: %v", what, err)
		}
	}
	_, err = f.ReadDir(es[1])
	isEnc("ReadDir(volume)", err)
	lk, err := f.Lookup("/Enc")
	if err != nil || !lk.Encrypted || lk.ID != "n:1:0:2" {
		t.Errorf("Lookup(/Enc) = %+v, %v", lk, err)
	}
	for _, p := range []string{"/Enc/secret", "/Enc/nothing", "/Enc/d/x"} {
		_, err = f.Lookup(p)
		isEnc("Lookup "+p, err)
	}
	for _, id := range []string{"n:1:0:2", "n:1:0:16", "n:1:0:99999", "n:1:5:2"} {
		_, err = f.Open(filesys.Entry{ID: id})
		if id == "n:1:5:2" { // an unknown view is rejected before the volume is considered
			if !errors.Is(err, filesys.ErrNotFound) && !errors.Is(err, filesys.ErrEncrypted) {
				t.Errorf("Open(%s) = %v", id, err)
			}
			continue
		}
		isEnc("Open "+id, err)
		_, err = f.ReadDir(filesys.Entry{ID: id})
		isEnc("ReadDir "+id, err)
	}
	// A forged "plain" encrypted volume stays ErrEncrypted.
	forged := es[1]
	forged.Encrypted, forged.Type, forged.Attrs = false, filesys.TypeFile, nil
	_, err = f.ReadDir(forged)
	isEnc("forged ReadDir", err)

	// The plain volume works, and Walk reports the encrypted one and goes on.
	var files int
	var encErrs []string
	err = filesys.Walk(f, f.Root(), "/", func(p string, e filesys.Entry, werr error) error {
		switch {
		case werr != nil:
			if errors.Is(werr, filesys.ErrEncrypted) {
				encErrs = append(encErrs, p)
			} else {
				t.Errorf("Walk error at %s: %v", p, werr)
			}
		case e.Type == filesys.TypeFile && strings.HasPrefix(p, "/Data/"):
			files++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(encErrs, []string{"/Enc"}) {
		t.Errorf("Walk reported encrypted at %q", encErrs)
	}
	if files != 4 { // a.txt, b.txt, the hard link and empty; the symlink is not a file
		t.Errorf("Walk found %d files in the plain volume, want 4", files)
	}
	if cr.touched(eg.Omap, eg.End) {
		t.Errorf("the reader touched the encrypted volume's object map or tree: %v", cr.r)
	}
	if len(f.Info().Warnings) != 0 {
		t.Errorf("warnings: %q", f.Info().Warnings)
	}
}

func TestUnreadableVolumeIsListed(t *testing.T) {
	im := newImage(t, volOpts(dataVolume(richFiles()...), apfstest.Volume{Name: "Two"}, apfstest.Volume{Name: "Three"}))
	// Slot 1: the magic is gone. Slot 2: its oid is not in the container map.
	copy(im.blk(im.g.Volumes[1].Super)[32:], "XXXX")
	im.seal(im.g.Volumes[1].Super)
	im.patchSupers(func(b []byte) { le.PutUint64(b[184+8*2:], 424242) })
	f := im.mustOpen()

	es := mustReadDir(t, f, f.Root())
	if len(es) != 3 {
		t.Fatalf("root lists %d volumes", len(es))
	}
	for i, name := range map[int]string{1: "~unreadable~1", 2: "~unreadable~2"} {
		e := es[i]
		if e.Name != name || e.Type != filesys.TypeDir {
			t.Errorf("volume %d entry = %+v", i, e)
		}
		if v, ok := attr(e, "unreadable"); !ok || v != "true" {
			t.Errorf("volume %d lacks unreadable=true: %v", i, e.Attrs)
		}
		if _, err := f.ReadDir(e); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("ReadDir(unreadable volume %d) = %v, want ErrCorrupt", i, err)
		}
		if _, err := f.Lookup("/" + name + "/x"); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("Lookup below unreadable volume %d = %v", i, err)
		}
	}
	if !hasWarn(f, "volume 1", "unreadable") || !hasWarn(f, "volume 2", "unreadable") {
		t.Errorf("warnings: %q", f.Info().Warnings)
	}
	if got := f.Info().Volumes; !slices.Equal(got, []string{"Data", "~unreadable~1", "~unreadable~2"}) {
		t.Errorf("Info.Volumes = %q", got)
	}
	if !contains(f.Info().Features, "vol[1] unreadable") {
		t.Errorf("features: %q", f.Info().Features)
	}
	// The readable volume still works.
	if got := entryNames(mustReadDir(t, f, mustLookup(t, f, "/Data"))); !slices.Contains(got, "docs") {
		t.Errorf("Data lists %q", got)
	}
}

func TestVolumeSuperblockBadChecksumWarns(t *testing.T) {
	im := newImage(t, volOpts(dataVolume(richFiles()...)))
	im.blk(im.g.Volumes[0].Super)[300] ^= 0xFF // inside apfs_fs_alloc_count's neighbourhood; not sealed
	f := im.mustOpen()
	if !hasWarn(f, "volume 0 superblock", "bad checksum") {
		t.Errorf("warnings: %q", f.Info().Warnings)
	}
	// It is still used.
	if got := entryNames(mustReadDir(t, f, mustLookup(t, f, "/Data"))); !slices.Contains(got, "docs") {
		t.Errorf("Data lists %q", got)
	}
	if in := f.Info(); !slices.Equal(in.Volumes, []string{"Data"}) {
		t.Errorf("Info.Volumes = %q", in.Volumes)
	}
}

// A volume oid is a virtual oid already handed out: it is below nx_next_oid.
func TestVolumeOidBelowNextOid(t *testing.T) {
	im := newImage(t, volOpts(dataVolume(richFiles()...)))
	if f := im.mustOpen(); hasWarn(f, "nx_next_oid") {
		t.Fatalf("a consistent container warns: %q", f.Info().Warnings)
	}
	im.patchSupers(func(b []byte) { le.PutUint64(b[88:], 1000) }) // nx_next_oid below the volume's oid
	f := im.mustOpen()
	if !hasWarn(f, "volume slot 0", "not below nx_next_oid (1000)") {
		t.Errorf("warnings: %q", f.Info().Warnings)
	}
	// The volume is still read.
	if got := f.Info().Volumes; !slices.Equal(got, []string{"Data"}) {
		t.Errorf("Info.Volumes = %q", got)
	}
}
