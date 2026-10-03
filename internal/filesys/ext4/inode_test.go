package ext4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// inodeOff is the byte offset of inode n, found through the group descriptor
// table (ipg inodes per group, isz bytes per inode).
func inodeOff(t *testing.T, f *ext4.FS, bs, isz, ipg int, n int) int {
	t.Helper()
	g := (n - 1) / ipg
	_, _, itable, _, _ := f.Group(g)
	return int(itable)*bs + ((n-1)%ipg)*isz
}

func attr(e filesys.Entry, key string) (string, bool) {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return "", false
}

func entryOf(t *testing.T, f *ext4.FS, n uint32) filesys.Entry {
	t.Helper()
	in, err := f.Inode(n)
	if err != nil {
		t.Fatalf("Inode(%d): %v", n, err)
	}
	return ext4.ToEntry("x", nil, in)
}

func wantTime(t *testing.T, what string, got filesys.Timestamp, want time.Time) {
	t.Helper()
	if !got.T.Equal(want) || !got.ZoneKnown {
		t.Errorf("%s = %v (zoneKnown %v), want %v known", what, got.T, got.ZoneKnown, want)
	}
}

func TestInodeFieldsAndTimes(t *testing.T) {
	for _, csum := range []bool{false, true} {
		for _, extents := range []bool{false, true} {
			files := []ext4test.File{{
				Path: "/a", Data: []byte("hello"), Mode: 0o640,
				UID: 70000, GID: 100000,
				Times: [4]int64{1600000000, 1600000100, 1600000200, 1600000300},
				Nsec:  [4]uint32{1, 22, 333, 999999999},
			}, {Path: "/d", Dir: true}}
			img := ext4test.Build(ext4test.Options{MetadataCsum: csum, Extents: extents}, files)
			f := mustOpen(t, img)

			in, err := f.Inode(ext4test.InodeNumber(0))
			if err != nil {
				t.Fatalf("csum=%v extents=%v: %v", csum, extents, err)
			}
			fl := in.Fields()
			if fl.Num != 11 || fl.Mode != 0x81A0 || fl.UID != 70000 || fl.GID != 100000 || fl.Size != 5 || fl.Links != 1 || !fl.CsumOK {
				t.Errorf("csum=%v extents=%v: fields %+v", csum, extents, fl)
			}
			if got, want := fl.Flags&0x80000 != 0, extents; got != want {
				t.Errorf("extents flag = %v, want %v", got, want)
			}
			ts := in.Times()
			wantTime(t, "atime", ts.Accessed, time.Unix(1600000000, 1))
			wantTime(t, "ctime", ts.Changed, time.Unix(1600000100, 22))
			wantTime(t, "mtime", ts.Modified, time.Unix(1600000200, 333))
			wantTime(t, "crtime", ts.Created, time.Unix(1600000300, 999999999))
			if !ts.Deleted.T.IsZero() {
				t.Errorf("dtime = %v, want absent", ts.Deleted.T)
			}

			e := ext4.ToEntry("a", nil, in)
			if e.ID != "inode:11" || e.Type != filesys.TypeFile || e.Size != 5 || e.Mode != 0x81A0 || e.UID != 70000 || e.GID != 100000 {
				t.Errorf("entry %+v", e)
			}
			if v, _ := attr(e, "links"); v != "1" {
				t.Errorf("links attr = %q", v)
			}
			if v, ok := attr(e, "flags"); !ok || !strings.HasPrefix(v, "0x") {
				t.Errorf("flags attr = %q", v)
			}
			if _, ok := attr(e, "checksum"); ok {
				t.Error("checksum attr on a good inode")
			}
			if _, ok := attr(e, "xattrs"); ok {
				t.Error("xattrs attr on an inode without xattrs")
			}

			d := entryOf(t, f, ext4test.InodeNumber(1))
			if d.Type != filesys.TypeDir || d.Mode != 0o40755 {
				t.Errorf("dir entry %+v", d)
			}
			root := entryOf(t, f, 2)
			if v, _ := attr(root, "links"); root.Type != filesys.TypeDir || v != "2" || root.ID != "inode:2" {
				t.Errorf("root entry %+v", root)
			}
		}
	}
}

func TestInodeEntryTypesAndRawName(t *testing.T) {
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/l", Symlink: "target"}})
	f := mustOpen(t, img)
	in, _ := f.Inode(11)
	e := ext4.ToEntry("l", []byte{0xff, 'l'}, in)
	if e.Type != filesys.TypeSymlink || e.Size != 6 {
		t.Errorf("symlink entry %+v", e)
	}
	if !bytes.Equal(e.RawName, []byte{0xff, 'l'}) {
		t.Errorf("RawName = %x", e.RawName)
	}
	// Reserved inode 5 is zero: mode 0 maps to TypeOther.
	if z := entryOf(t, f, 5); z.Type != filesys.TypeOther {
		t.Errorf("zero inode type = %v", z.Type)
	}
}

func TestInodeEncryptFlag(t *testing.T) {
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/a"}})
	f := mustOpen(t, img)
	off := inodeOff(t, f, 1024, 256, 128, 11)
	put32(img, off+0x20, 0x800|0x80000)
	f = mustOpen(t, img)
	if e := entryOf(t, f, 11); !e.Encrypted {
		t.Error("ENCRYPT inode flag did not set Entry.Encrypted")
	}
}

func TestInodeExtraEpochBits(t *testing.T) {
	const y2038 = 1 << 31 // 2038-01-19T03:14:08Z, first second that needs the epoch bit
	cases := []struct {
		name string
		sec  int64
		nsec uint32
	}{
		{"y2038", y2038, 5},
		{"2106", 1 << 32, 6},
		{"2242", 1<<33 + 12345, 123456789},
		{"2377", 3<<32 + 7, 8},
		{"pre1970", -86400, 9},
		{"min", -(1 << 31), 0},
	}
	for _, c := range cases {
		img := ext4test.Build(ext4test.Options{}, []ext4test.File{{
			Path: "/a", Times: [4]int64{c.sec, c.sec, c.sec, c.sec}, Nsec: [4]uint32{c.nsec, c.nsec, c.nsec, c.nsec},
		}})
		f := mustOpen(t, img)
		in, err := f.Inode(11)
		if err != nil {
			t.Fatal(err)
		}
		ts := in.Times()
		want := time.Unix(c.sec, int64(c.nsec))
		for name, got := range map[string]filesys.Timestamp{"atime": ts.Accessed, "ctime": ts.Changed, "mtime": ts.Modified, "crtime": ts.Created} {
			wantTime(t, c.name+" "+name, got, want)
		}
	}
	if got := time.Unix(y2038, 0).UTC(); got.Year() != 2038 {
		t.Fatalf("test constant: %v", got)
	}
}

func TestInodeTimesWithoutExtraFields(t *testing.T) {
	times := [4]int64{1600000000, 1600000100, 1600000200, 1600000300}
	nsec := [4]uint32{1, 2, 3, 4}
	for name, o := range map[string]struct {
		isz, extra int
		wantCrtime bool
	}{
		"128-byte inode":      {128, 0, false},
		"extra_isize 4":       {256, 4, false},
		"extra_isize 16":      {256, 16, false},
		"extra_isize 20":      {256, 20, true},
		"extra_isize 24 full": {256, 24, true},
	} {
		img := ext4test.Build(ext4test.Options{InodeSize: o.isz}, []ext4test.File{{Path: "/a", Times: times, Nsec: nsec, ExtraIsize: o.extra}})
		f := mustOpen(t, img)
		in, err := f.Inode(11)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ts := in.Times()
		// Seconds are present; nanoseconds only where the *_extra field fits.
		fits := func(end int) bool { return 128+o.extra >= end }
		ns := func(end int, v uint32) int64 {
			if fits(end) {
				return int64(v)
			}
			return 0
		}
		wantTime(t, name+" atime", ts.Accessed, time.Unix(times[0], ns(0x90, nsec[0])))
		wantTime(t, name+" ctime", ts.Changed, time.Unix(times[1], ns(0x88, nsec[1])))
		wantTime(t, name+" mtime", ts.Modified, time.Unix(times[2], ns(0x8C, nsec[2])))
		if o.wantCrtime {
			wantTime(t, name+" crtime", ts.Created, time.Unix(times[3], ns(0x98, nsec[3])))
		} else if !ts.Created.T.IsZero() {
			t.Errorf("%s: crtime %v, want absent", name, ts.Created.T)
		}
	}
}

func TestInodeZeroAndDeletedTimes(t *testing.T) {
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{
		{Path: "/zero"},
		{Path: "/gone", Deleted: true, Times: [4]int64{1, 2, 3, 4}},
	})
	f := mustOpen(t, img)
	z, _ := f.Inode(11)
	zt := z.Times()
	for _, ts := range []filesys.Timestamp{zt.Accessed, zt.Changed, zt.Modified, zt.Created, zt.Deleted} {
		if !ts.T.IsZero() {
			t.Errorf("zero raw time decoded as %v", ts.T)
		}
	}
	g, _ := f.Inode(12)
	if fl := g.Fields(); fl.Links != 0 {
		t.Errorf("deleted links = %d", fl.Links)
	}
	wantTime(t, "dtime", g.Times().Deleted, time.Unix(1700000000, 0))
}

// refInodeCsum is a from-scratch statement of the kernel rule for the test:
// crc32c(seed, le32 ino, le32 gen, inode with the checksum fields zeroed).
func refInodeCsum(seed uint32, ino uint32, raw []byte, hasHi bool) uint32 {
	cp := append([]byte(nil), raw...)
	cp[0x7C], cp[0x7D] = 0, 0
	if hasHi {
		cp[0x82], cp[0x83] = 0, 0
	}
	buf := binary.LittleEndian.AppendUint32(nil, ino)
	buf = append(buf, raw[0x64:0x68]...)
	buf = append(buf, cp...)
	return ^crc32.Update(^seed, crc32.MakeTable(crc32.Castagnoli), buf)
}

func TestInodeChecksumMismatchFlagged(t *testing.T) {
	uuid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	for name, o := range map[string]ext4test.Options{
		"uuid seed":    {MetadataCsum: true, UUID: uuid},
		"csum_seed":    {MetadataCsum: true, UUID: uuid, CsumSeed: 0xA5A5A5A5},
		"128-byte":     {MetadataCsum: true, UUID: uuid, InodeSize: 128},
		"4k, 512-byte": {MetadataCsum: true, UUID: uuid, BlockSize: 4096, InodeSize: 512},
	} {
		files := []ext4test.File{{Path: "/a", Data: []byte("x"), Generation: 0xDEADBEEF, Times: [4]int64{5, 6, 7, 8}}}
		img := ext4test.Build(o, files)
		f := mustOpen(t, img)
		bs, isz := orInt(o.BlockSize, 1024), orInt(o.InodeSize, 256)
		off := inodeOff(t, f, bs, isz, 128, 11)

		// The stored checksum matches an independent computation.
		seed := ext4.RawCRC32C(0xFFFFFFFF, uuid[:])
		if o.CsumSeed != 0 {
			seed = o.CsumSeed
		}
		raw := img[off : off+isz]
		hasHi := isz > 128
		want := refInodeCsum(seed, 11, raw, hasHi)
		got := uint32(binary.LittleEndian.Uint16(raw[0x7C:]))
		if hasHi {
			got |= uint32(binary.LittleEndian.Uint16(raw[0x82:])) << 16
		} else {
			want &= 0xFFFF
		}
		if got != want {
			t.Errorf("%s: stored checksum %#x, reference %#x", name, got, want)
		}

		e := entryOf(t, f, 11)
		if v, ok := attr(e, "checksum"); ok {
			t.Errorf("%s: good inode flagged checksum=%s", name, v)
		}

		img[off+0x10] ^= 0x01 // flip a bit in mtime
		f = mustOpen(t, img)
		in, err := f.Inode(11)
		if err != nil {
			t.Fatalf("%s: a checksum mismatch must not be fatal: %v", name, err)
		}
		if in.Fields().CsumOK {
			t.Errorf("%s: csumOK after corruption", name)
		}
		if v, _ := attr(ext4.ToEntry("a", nil, in), "checksum"); v != "bad" {
			t.Errorf("%s: checksum attr = %q, want bad", name, v)
		}
		img[off+0x10] ^= 0x01

		// A damaged high half is caught too (32-bit comparison).
		if hasHi {
			img[off+0x82] ^= 0x80
			f = mustOpen(t, img)
			if in, _ := f.Inode(11); in.Fields().CsumOK {
				t.Errorf("%s: csumOK with a damaged checksum_hi", name)
			}
			img[off+0x82] ^= 0x80
		}

		// All-zero (never used) inodes are not reported as bad.
		f = mustOpen(t, img)
		if in, _ := f.Inode(5); !in.Fields().CsumOK {
			t.Errorf("%s: a zeroed inode is flagged", name)
		}
	}

	// Without metadata_csum nothing is verified.
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/a"}})
	f := mustOpen(t, img)
	img[inodeOff(t, f, 1024, 256, 128, 11)+0x7C] = 0x55
	f = mustOpen(t, img)
	if e := entryOf(t, f, 11); func() bool { _, ok := attr(e, "checksum"); return ok }() {
		t.Error("checksum attr without metadata_csum")
	}
}

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func TestXattrsInInodeAndBlock(t *testing.T) {
	want := []ext4test.Xattr{
		{Name: "user.comment", Value: []byte("hello world")},
		{Name: "security.selinux", Value: []byte("system_u:object_r:etc_t:s0\x00")},
		{Name: "system.posix_acl_access", Value: []byte{2, 0, 0, 0, 1, 0, 6, 0, 0xff, 0xff, 0xff, 0xff}},
		{Name: "trusted.k", Value: nil},
		{Name: "user.odd", Value: []byte("123")},
	}
	for _, csum := range []bool{false, true} {
		for _, inBlock := range []bool{false, true} {
			bit64 := inBlock && csum // also exercise the i_file_acl_high path
			files := []ext4test.File{
				{Path: "/x", Xattrs: want, XattrBlock: inBlock},
				{Path: "/none"},
			}
			img := ext4test.Build(ext4test.Options{MetadataCsum: csum, Bit64: bit64, InodeSize: 512}, files)
			f := mustOpen(t, img)
			in, err := f.Inode(11)
			if err != nil {
				t.Fatal(err)
			}
			if got := in.Fields().FileACL != 0; got != inBlock {
				t.Errorf("csum=%v block=%v: i_file_acl nonzero = %v", csum, inBlock, got)
			}
			got, err := f.Xattrs(in)
			if err != nil {
				t.Fatalf("csum=%v block=%v: %v", csum, inBlock, err)
			}
			if len(got) != len(want) {
				t.Fatalf("csum=%v block=%v: %d xattrs, want %d: %+v", csum, inBlock, len(got), len(want), got)
			}
			for i := range want {
				if got[i].Name != want[i].Name || !bytes.Equal(got[i].Value, want[i].Value) {
					t.Errorf("csum=%v block=%v: xattr %d = %q=%q, want %q=%q", csum, inBlock, i, got[i].Name, got[i].Value, want[i].Name, want[i].Value)
				}
			}
			e := ext4.ToEntry("x", nil, in)
			names := "user.comment,security.selinux,system.posix_acl_access,trusted.k,user.odd"
			if v, _ := attr(e, "xattrs"); v != names {
				t.Errorf("xattrs attr = %q, want %q", v, names)
			}

			none, _ := f.Inode(12)
			if xs, err := f.Xattrs(none); err != nil || len(xs) != 0 {
				t.Errorf("inode without xattrs: %v, %v", xs, err)
			}
		}
	}
}

func TestInodeOutOfRange(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Groups: 2}, nil)
	f := mustOpen(t, img)
	count := le32(img, offInodesCount)
	for _, n := range []uint32{0, count + 1, math.MaxUint32} {
		_, err := f.Inode(n)
		var ce *filesys.CorruptError
		if !errors.As(err, &ce) {
			t.Errorf("Inode(%d) = %v, want CorruptError", n, err)
		}
	}
	if _, err := f.Inode(count); err != nil {
		t.Errorf("Inode(count): %v", err)
	}

	// s_inodes_count larger than groups * inodes_per_group is only warned
	// about: the group index must still be bounded by the descriptor table.
	put32(img, offInodesCount, count*4)
	f = mustOpen(t, img)
	if _, err := f.Inode(count*2 + 1); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("inode beyond the last group = %v, want CorruptError", err)
	}

	// A group whose inode table lies outside the filesystem is unreadable.
	img = ext4test.Build(ext4test.Options{Groups: 2}, nil)
	put32(img, 2*1024+32+0x8, 0xFFFFFF) // group 1 bg_inode_table_lo
	f = mustOpen(t, img)
	if _, err := f.Inode(129); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("inode in a group with a bad table = %v, want CorruptError", err)
	}
	if _, err := f.Inode(11); err != nil {
		t.Errorf("inode in a good group: %v", err)
	}
}

func TestInodeSizeOverflowIsCorrupt(t *testing.T) {
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/f"}, {Path: "/d", Dir: true}})
	f := mustOpen(t, img)
	off := inodeOff(t, f, 1024, 256, 128, 11)

	put32(img, off+0x4, 0xFFFFFFFF)
	put32(img, off+0x6C, 0x7FFFFFFF)
	f = mustOpen(t, img)
	in, err := f.Inode(11)
	if err != nil || in.Fields().Size != math.MaxInt64 {
		t.Fatalf("size = MaxInt64: %+v, %v", in, err)
	}

	put32(img, off+0x6C, 0x80000000)
	f = mustOpen(t, img)
	_, err = f.Inode(11)
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) {
		t.Fatalf("size > MaxInt64: %v, want CorruptError", err)
	}

	// Directories ignore i_size_high unless large_dir is set (kernel ext4_isize).
	doff := inodeOff(t, f, 1024, 256, 128, 12)
	put32(img, doff+0x4, 4096)
	put32(img, doff+0x6C, 0x80000000)
	f = mustOpen(t, img)
	in, err = f.Inode(12)
	if err != nil || in.Fields().Size != 4096 {
		t.Errorf("dir size = %+v, %v", in, err)
	}
}

func TestInodeBadExtraIsizeFallsBack(t *testing.T) {
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/a", Times: [4]int64{10, 20, 30, 40}, Nsec: [4]uint32{1, 1, 1, 1}}})
	f := mustOpen(t, img)
	off := inodeOff(t, f, 1024, 256, 128, 11)
	for _, bad := range []uint16{2, 3, 200, 0xFFFF} {
		put16(img, off+0x80, bad)
		f = mustOpen(t, img)
		in, err := f.Inode(11)
		if err != nil {
			t.Fatalf("extra_isize %d: %v", bad, err)
		}
		wantTime(t, "atime", in.Times().Accessed, time.Unix(10, 0))
		if !in.Times().Created.T.IsZero() {
			t.Errorf("extra_isize %d: crtime read from an invalid extra area", bad)
		}
		if v, _ := attr(ext4.ToEntry("a", nil, in), "extra_isize"); v != "bad" {
			t.Errorf("extra_isize %d: attr = %q", bad, v)
		}
		if _, err := f.Xattrs(in); err != nil {
			t.Errorf("extra_isize %d: xattrs error %v", bad, err)
		}
	}
}

// xattrTable builds an entry table by hand: one entry per spec, at offset 0.
func xattrEntryBytes(nameLen byte, index byte, valueOff uint16, inum, valueSize uint32, name string) []byte {
	e := make([]byte, 16, 16+len(name)+4)
	e[0], e[1] = nameLen, index
	binary.LittleEndian.PutUint16(e[2:], valueOff)
	binary.LittleEndian.PutUint32(e[4:], inum)
	binary.LittleEndian.PutUint32(e[8:], valueSize)
	e = append(e, name...)
	for len(e)%4 != 0 {
		e = append(e, 0)
	}
	return e
}

func pad(b []byte, n int) []byte { return append(b, make([]byte, n-len(b))...) }

func TestXattrHostile(t *testing.T) {
	ok := xattrEntryBytes(1, 1, 60, 0, 4, "a")
	cases := []struct {
		name    string
		region  []byte
		wantErr bool
		wantN   int
	}{
		{"valid", func() []byte { r := pad(ok, 64); copy(r[60:], "VVVV"); return r }(), false, 1},
		{"all zero is an empty table", make([]byte, 64), false, 0},
		{"value offset outside the area", pad(xattrEntryBytes(1, 1, 61, 0, 4, "a"), 64), true, 0},
		{"value offset huge", pad(xattrEntryBytes(1, 1, 0xFFFF, 0, 4, "a"), 64), true, 0},
		{"value size outside the area", pad(xattrEntryBytes(1, 1, 40, 0, 1000, "a"), 64), true, 0},
		{"value size 4 GiB", pad(xattrEntryBytes(1, 1, 8, 0, 0xFFFFFFFF, "a"), 64), true, 0},
		{"name_len past the end", pad(xattrEntryBytes(200, 1, 0, 0, 0, "a"), 64), true, 0},
		{"name_len 255 at the very end", append(make([]byte, 0), xattrEntryBytes(255, 1, 0, 0, 0, "")...), true, 0},
		{"truncated entry header", []byte{1, 1, 0, 0, 0, 0, 0, 0}, true, 0},
		{"table without terminator", xattrEntryBytes(1, 1, 0, 0, 0, "a"), false, 1},
		{"zero value size anywhere", pad(xattrEntryBytes(1, 1, 0xFFFF, 0, 0, "a"), 64), false, 1},
		{"ea_inode value is not read", pad(xattrEntryBytes(1, 1, 0xFFFF, 77, 1<<30, "a"), 64), false, 1},
		{"unknown name index", pad(xattrEntryBytes(1, 200, 0, 0, 0, "a"), 64), false, 1},
		{"empty region", nil, false, 0},
	}
	for _, c := range cases {
		got, err := ext4.ParseXattrs(c.region, 0)
		var ce *filesys.CorruptError
		if c.wantErr != errors.As(err, &ce) {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.wantErr)
		}
		if c.wantN != len(got) {
			t.Errorf("%s: %d entries, want %d", c.name, len(got), c.wantN)
		}
	}

	// Entries before a bad one are kept.
	two := append(xattrEntryBytes(1, 1, 0, 0, 0, "a"), xattrEntryBytes(200, 1, 0, 0, 0, "b")...)
	got, err := ext4.ParseXattrs(pad(two, 64), 0)
	if err == nil || len(got) != 1 || got[0].Name != "user.a" {
		t.Errorf("partial parse = %+v, %v", got, err)
	}

	// An entry table that could only loop forever (every record claims zero
	// length) cannot: each entry consumes at least 16 bytes.
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test input, not security
	for range 20000 {
		region := make([]byte, rng.IntN(200))
		for i := range region {
			if rng.IntN(3) == 0 {
				region[i] = byte(rng.IntN(256))
			}
		}
		_, _ = ext4.ParseXattrs(region, 0)
		_, _ = ext4.ParseXattrs(region, rng.IntN(300)-10)
	}
}

func TestXattrHostileInImage(t *testing.T) {
	xs := []ext4test.Xattr{{Name: "user.a", Value: []byte("v")}}
	for _, inBlock := range []bool{false, true} {
		img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/a", Xattrs: xs, XattrBlock: inBlock}})
		f := mustOpen(t, img)
		off := inodeOff(t, f, 1024, 256, 128, 11)
		var entry int
		if inBlock {
			in, _ := f.Inode(11)
			entry = int(in.Fields().FileACL)*1024 + 32
		} else {
			entry = off + 128 + 32 + 4
		}
		put16(img, entry+2, 0xFFFF) // e_value_offs
		put32(img, entry+8, 4)      // e_value_size
		f = mustOpen(t, img)
		in, err := f.Inode(11) // the damaged xattrs do not stop the inode being read
		if err != nil {
			t.Fatalf("block=%v: Inode: %v", inBlock, err)
		}
		if _, err := f.Xattrs(in); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("block=%v: Xattrs = %v, want CorruptError", inBlock, err)
		}
		if _, ok := attr(ext4.ToEntry("a", nil, in), "xattrs_error"); !ok {
			t.Errorf("block=%v: entry has no xattrs_error attr", inBlock)
		}
	}

	// i_file_acl pointing outside the filesystem, and a block with a bad magic.
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/a", Xattrs: xs, XattrBlock: true}})
	f := mustOpen(t, img)
	off := inodeOff(t, f, 1024, 256, 128, 11)
	acl := int(le32(img, off+0x68))
	img[acl*1024] ^= 0xFF
	f = mustOpen(t, img)
	in, _ := f.Inode(11)
	if _, err := f.Xattrs(in); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("bad xattr block magic: %v", err)
	}
	put32(img, off+0x68, 0x7FFFFFFF)
	f = mustOpen(t, img)
	in, _ = f.Inode(11)
	if _, err := f.Xattrs(in); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("i_file_acl outside the filesystem: %v", err)
	}
}

// Literal raw-byte time decoding, independent of the builder's encoder.
func TestInodeRawTimeBytes(t *testing.T) {
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/a"}})
	f := mustOpen(t, img)
	off := inodeOff(t, f, 1024, 256, 128, 11)
	put32(img, off+0x8, 0x80000000) // atime: -2^31 ...
	put32(img, off+0x8C, 0x1)       // ... + epoch bit 1 => 2^31 = 2038-01-19T03:14:08Z
	put32(img, off+0xC, 0xFFFFFFFF) // ctime: -1, no epoch bits
	put32(img, off+0x84, 0)
	put32(img, off+0x10, 100)              // mtime with invalid nanoseconds
	put32(img, off+0x88, 1_000_000_000<<2) // 1e9 ns
	put32(img, off+0x90, 200)              // crtime
	put32(img, off+0x94, 0xFFFFFFFC|3)     // ns 1073741823 (max 30-bit), epoch 3
	f = mustOpen(t, img)
	in, err := f.Inode(11)
	if err != nil {
		t.Fatal(err)
	}
	ts := in.Times()
	if want := time.Date(2038, 1, 19, 3, 14, 8, 0, time.UTC); !ts.Accessed.T.Equal(want) {
		t.Errorf("atime = %v, want %v", ts.Accessed.T, want)
	}
	if want := time.Date(1969, 12, 31, 23, 59, 59, 0, time.UTC); !ts.Changed.T.Equal(want) {
		t.Errorf("ctime = %v, want %v", ts.Changed.T, want)
	}
	if want := time.Unix(100, 999999999); !ts.Modified.T.Equal(want) {
		t.Errorf("mtime = %v, want clamped %v", ts.Modified.T, want)
	}
	if want := time.Unix(200+3<<32, 999999999); !ts.Created.T.Equal(want) {
		t.Errorf("crtime = %v, want %v", ts.Created.T, want)
	}
	if v, _ := attr(ext4.ToEntry("a", nil, in), "time_ns"); v != "invalid" {
		t.Errorf("time_ns attr = %q, want invalid", v)
	}

	// Valid nanoseconds carry no attr.
	put32(img, off+0x88, 999_999_999<<2)
	put32(img, off+0x94, 999_999_999<<2)
	f = mustOpen(t, img)
	in, _ = f.Inode(11)
	if _, ok := attr(ext4.ToEntry("a", nil, in), "time_ns"); ok {
		t.Error("time_ns attr on valid nanoseconds")
	}
}

func TestInodeSizeHighIgnoredAttr(t *testing.T) {
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/d", Dir: true}, {Path: "/l", Symlink: "x"}, {Path: "/f"}})
	f := mustOpen(t, img)
	for _, n := range []int{11, 12, 13} {
		put32(img, inodeOff(t, f, 1024, 256, 128, n)+0x6C, 7)
	}
	f = mustOpen(t, img)
	for _, n := range []uint32{11, 12} {
		e := entryOf(t, f, n)
		if v, _ := attr(e, "size_high_ignored"); v != "7" {
			t.Errorf("inode %d: size_high_ignored = %q, want 7", n, v)
		}
		if e.Size >= 1<<32 {
			t.Errorf("inode %d: size %d includes the ignored high half", n, e.Size)
		}
	}
	// A regular file uses the high half: no attr.
	e := entryOf(t, f, 13)
	if _, ok := attr(e, "size_high_ignored"); ok || e.Size != 7<<32 {
		t.Errorf("regular file: size %d, attr present %v", e.Size, ok)
	}
}

func TestXattrErrorOffsetIsAbsolute(t *testing.T) {
	xs := []ext4test.Xattr{{Name: "user.a", Value: []byte("v")}}
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/a", Xattrs: xs}})
	f := mustOpen(t, img)
	off := inodeOff(t, f, 1024, 256, 128, 11)
	entry := off + 128 + 32 + 4
	put16(img, entry+2, 0xFFFF)
	put32(img, entry+8, 4)
	f = mustOpen(t, img)
	in, _ := f.Inode(11)
	_, err := f.Xattrs(in)
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) || ce.Offset != int64(entry) {
		t.Errorf("err = %v (offset %v), want a CorruptError at image offset %d", err, ce, entry)
	}

	img = ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/a", Xattrs: xs, XattrBlock: true}})
	f = mustOpen(t, img)
	off = inodeOff(t, f, 1024, 256, 128, 11)
	blk := int(le32(img, off+0x68))
	put16(img, blk*1024+32+2, 0xFFFF)
	put32(img, blk*1024+32+8, 4)
	f = mustOpen(t, img)
	in, _ = f.Inode(11)
	_, err = f.Xattrs(in)
	if !errors.As(err, &ce) || ce.Offset != int64(blk*1024+32) {
		t.Errorf("block: err = %v, want a CorruptError at image offset %d", err, blk*1024+32)
	}
}
