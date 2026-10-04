package f2fs_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

func attr(e filesys.Entry, key string) (string, bool) {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return "", false
}

// buildOne builds an image holding the single inode in at nid 5 (extra header
// fields as the options and the inode say).
func buildOne(t *testing.T, o f2fstest.Options, in f2fstest.Inode) (*f2fs.FS, []byte) {
	t.Helper()
	in.NID = 5
	o.Nodes = []f2fstest.Node{{NID: 5, Block: f2fstest.InodeBlock(o, in)}}
	img := f2fstest.Build(o, nil)
	return mustOpen(t, img), img
}

func TestInodeFieldsTimesExtraAttr(t *testing.T) {
	o := smallOpts()
	o.ExtraAttr, o.InodeChksum, o.InodeCrtime = true, true, true
	o.UUID = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	f, _ := buildOne(t, o, f2fstest.Inode{
		Mode: 0o100640, UID: 1000, GID: 1001, Links: 3, Size: 123456, Blocks: 31,
		Atime: 1_700_000_001, Ctime: 1_700_000_002, Mtime: 1_700_000_003,
		AtimeNsec: 111, CtimeNsec: 222, MtimeNsec: 333,
		Generation: 77, CurDepth: 4, XattrNID: 8, Flags: 0x10, PIno: 3,
		Name: "hello.txt", DirLevel: 2, Ext: [3]uint32{1, 2, 3},
		NIDs:  [5]uint32{10, 11, 12, 13, 14},
		Extra: true, ProjID: 99, Crtime: 1_600_000_000, CrtimeNsec: 444,
	})
	v, e, err := f.Inode(5)
	if err != nil {
		t.Fatal(err)
	}
	if v.Mode != 0o100640 || v.UID != 1000 || v.GID != 1001 || v.Links != 3 || v.Size != 123456 || v.Blocks != 31 {
		t.Errorf("basic fields: %+v", v)
	}
	if v.Generation != 77 || v.XattrNID != 8 || v.PIno != 3 || v.Flags != 0x10 || v.DirLevel != 2 || v.Ext != [3]uint32{1, 2, 3} {
		t.Errorf("other fields: %+v", v)
	}
	if v.Name != "hello.txt" || v.NameBad {
		t.Errorf("name %q bad=%v", v.Name, v.NameBad)
	}
	if v.NIDs != [5]uint32{10, 11, 12, 13, 14} {
		t.Errorf("i_nid = %v", v.NIDs)
	}
	// Extra header: 36 bytes = 9 words, so the data addresses start 36 bytes
	// into i_addr and 923-9 slots remain (no inline xattr).
	if v.ExtraIsize != 36 || v.ProjID != 99 || v.AddrStart != 360+36 || v.AddrSlots != 923-9 || v.XattrWords != 0 {
		t.Errorf("extra: isize=%d proj=%d start=%d slots=%d xattr=%d", v.ExtraIsize, v.ProjID, v.AddrStart, v.AddrSlots, v.XattrWords)
	}
	ts := func(s int64, ns int64) filesys.Timestamp {
		return filesys.Timestamp{T: time.Unix(s, ns).UTC(), ZoneKnown: true}
	}
	want := filesys.Times{
		Accessed: ts(1_700_000_001, 111), Changed: ts(1_700_000_002, 222), Modified: ts(1_700_000_003, 333),
		Created: ts(1_600_000_000, 444),
	}
	if v.Times != want {
		t.Errorf("times = %+v\nwant   %+v", v.Times, want)
	}
	if !v.Times.Deleted.T.IsZero() {
		t.Error("F2FS has no deletion time")
	}
	if !v.ChecksumChecked || !v.ChecksumOK {
		t.Errorf("checksum checked=%v ok=%v, want both", v.ChecksumChecked, v.ChecksumOK)
	}

	if e.ID != "nid:5" || e.Type != filesys.TypeFile || e.Size != 123456 || e.Mode != 0o100640 || e.UID != 1000 || e.GID != 1001 {
		t.Errorf("entry %+v", e)
	}
	if e.Name != "name" || string(e.RawName) != "raw" {
		t.Errorf("toEntry name/raw: %q %q", e.Name, e.RawName)
	}
	if e.Encrypted || e.Times != want || e.Deleted {
		t.Errorf("entry flags/times: %+v", e)
	}
	if val, _ := attr(e, "flags"); val != "0x10" {
		t.Errorf("flags attr %q", val)
	}
	if val, _ := attr(e, "links"); val != "3" {
		t.Errorf("links attr %q", val)
	}
	for _, k := range []string{"inline", "checksum", "time_ns", "compressed"} {
		if val, ok := attr(e, k); ok {
			t.Errorf("unexpected attr %s=%q", k, val)
		}
	}
}

func TestInodeLayoutVariants(t *testing.T) {
	flex := func() f2fstest.Options {
		o := smallOpts()
		o.ExtraAttr, o.InlineXattr = true, true
		return o
	}
	plain := func() f2fstest.Options { o := smallOpts(); o.ExtraAttr = true; return o }
	crtime := func() f2fstest.Options { o := plain(); o.InodeCrtime = true; return o }
	tests := []struct {
		name                string
		o                   f2fstest.Options
		in                  f2fstest.Inode
		extra, xattr, start int
		slots               int
		created             bool
		inlineAttr          string
	}{
		{
			name: "no extra header", o: smallOpts(), in: f2fstest.Inode{Mode: 0o100644},
			extra: 0, xattr: 0, start: 360, slots: 923,
		},
		{
			name: "no extra header, inline xattr uses the default 50 words", o: smallOpts(),
			in:    f2fstest.Inode{Mode: 0o100644, Inline: f2fstest.InlineXattr},
			extra: 0, xattr: 50, start: 360, slots: 873, inlineAttr: "xattr",
		},
		{
			name: "extra header and default inline xattr", o: plain(),
			in:    f2fstest.Inode{Mode: 0o100644, Extra: true, Inline: f2fstest.InlineXattr | f2fstest.InlineData},
			extra: 36, xattr: 50, start: 396, slots: 923 - 9 - 50, inlineAttr: "data,xattr",
		},
		{
			name: "flexible inline xattr records its size", o: flex(),
			in:    f2fstest.Inode{Mode: 0o040755, Extra: true, Inline: f2fstest.InlineXattr | f2fstest.InlineDentry, InlineXattrSize: 20},
			extra: 36, xattr: 20, start: 396, slots: 923 - 9 - 20, inlineAttr: "dentry,xattr",
		},
		{
			name: "flexible: the recorded size applies even without the inline xattr flag", o: flex(),
			in:    f2fstest.Inode{Mode: 0o100644, Extra: true, InlineXattrSize: 20},
			extra: 36, xattr: 20, start: 396, slots: 923 - 9 - 20,
		},
		{
			name: "flexible with a recorded size of zero reserves nothing", o: flex(),
			in:    f2fstest.Inode{Mode: 0o100644, Extra: true},
			extra: 36, xattr: 0, start: 396, slots: 923 - 9,
		},
		{
			name: "flexible volume, inode without an extra header: default 50 words", o: flex(),
			in:    f2fstest.Inode{Mode: 0o100644, Inline: f2fstest.InlineXattr},
			extra: 0, xattr: 50, start: 360, slots: 873, inlineAttr: "xattr",
		},
		{
			name: "an inline dentry reserves the default 50 words without the xattr flag", o: plain(),
			in:    f2fstest.Inode{Mode: 0o040755, Extra: true, Inline: f2fstest.InlineDentry},
			extra: 36, xattr: 50, start: 396, slots: 923 - 9 - 50, inlineAttr: "dentry",
		},
		{
			name: "no extra header, no flags: nothing reserved even for a directory", o: smallOpts(),
			in:    f2fstest.Inode{Mode: 0o040755},
			extra: 0, xattr: 0, start: 360, slots: 923,
		},
		{
			name: "short 24-byte header reaches crtime", o: crtime(),
			in:    f2fstest.Inode{Mode: 0o100644, Extra: true, ExtraIsize: 24, Crtime: 5},
			extra: 24, start: 384, slots: 923 - 6, created: true,
		},
		{
			name: "20-byte header is too short for crtime", o: crtime(),
			in:    f2fstest.Inode{Mode: 0o100644, Extra: true, ExtraIsize: 20, Crtime: 5},
			extra: 20, start: 380, slots: 923 - 5,
		},
		{
			name: "crtime without the volume feature is ignored", o: plain(),
			in:    f2fstest.Inode{Mode: 0o100644, Extra: true, Crtime: 5},
			extra: 36, start: 396, slots: 923 - 9,
		},
		{
			name: "crtime of zero is absent", o: crtime(),
			in:    f2fstest.Inode{Mode: 0o100644, Extra: true},
			extra: 36, start: 396, slots: 923 - 9,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := buildOne(t, tc.o, tc.in)
			v, e, err := f.Inode(5)
			if err != nil {
				t.Fatal(err)
			}
			if v.ExtraIsize != tc.extra || v.XattrWords != tc.xattr || v.AddrStart != tc.start || v.AddrSlots != tc.slots {
				t.Errorf("extra=%d xattr=%d start=%d slots=%d; want %d %d %d %d",
					v.ExtraIsize, v.XattrWords, v.AddrStart, v.AddrSlots, tc.extra, tc.xattr, tc.start, tc.slots)
			}
			if got := !v.Times.Created.T.IsZero(); got != tc.created {
				t.Errorf("crtime present = %v, want %v", got, tc.created)
			}
			got, _ := attr(e, "inline")
			if got != tc.inlineAttr {
				t.Errorf("inline attr %q, want %q", got, tc.inlineAttr)
			}
		})
	}
}

func TestInodeTypesFlagsAndInvalidTimes(t *testing.T) {
	o := smallOpts()
	types := map[uint16]filesys.EntryType{
		0o100644: filesys.TypeFile, 0o040755: filesys.TypeDir, 0o120777: filesys.TypeSymlink,
		0o020600: filesys.TypeOther, 0o060600: filesys.TypeOther, 0o010600: filesys.TypeOther, 0o140600: filesys.TypeOther,
	}
	for mode, want := range types {
		f, _ := buildOne(t, o, f2fstest.Inode{Mode: mode})
		_, e, err := f.Inode(5)
		if err != nil || e.Type != want {
			t.Errorf("mode %#o: type %v, %v; want %v", mode, e.Type, err, want)
		}
	}

	t.Run("encrypted", func(t *testing.T) {
		f, _ := buildOne(t, o, f2fstest.Inode{Mode: 0o100600, Flags: 0x800})
		v, e, _ := f.Inode(5)
		if !v.Encrypted || !e.Encrypted {
			t.Error("F2FS_ENCRYPT_FL not reported as encrypted")
		}
	})
	t.Run("compressed", func(t *testing.T) {
		f, _ := buildOne(t, o, f2fstest.Inode{Mode: 0o100600, Flags: 0x4})
		v, e, _ := f.Inode(5)
		if val, _ := attr(e, "compressed"); !v.Compressed || val != "true" {
			t.Errorf("F2FS_COMPR_FL on a file: compressed=%v attr=%q", v.Compressed, val)
		}
		// A directory carries the flag only to hand it to new children.
		f, _ = buildOne(t, o, f2fstest.Inode{Mode: 0o040755, Flags: 0x4})
		if v, _, _ := f.Inode(5); v.Compressed {
			t.Error("a directory with F2FS_COMPR_FL is not a compressed file")
		}
	})
	t.Run("nanoseconds out of range are clamped and flagged", func(t *testing.T) {
		f, _ := buildOne(t, o, f2fstest.Inode{Mode: 0o100644, Atime: 100, Mtime: 200, Ctime: 300, MtimeNsec: 1_000_000_000, AtimeNsec: math.MaxUint32})
		v, e, _ := f.Inode(5)
		if got := v.Times.Modified.T; got != time.Unix(200, 999_999_999).UTC() {
			t.Errorf("mtime = %v, want clamped to 200.999999999", got)
		}
		if got := v.Times.Accessed.T; got != time.Unix(100, 999_999_999).UTC() {
			t.Errorf("atime = %v", got)
		}
		if val, _ := attr(e, "time_ns"); val != "invalid" {
			t.Errorf("time_ns attr %q", val)
		}
	})
	t.Run("seconds beyond year 9999 are omitted and flagged", func(t *testing.T) {
		f, _ := buildOne(t, o, f2fstest.Inode{Mode: 0o100644, Atime: math.MaxUint64, Mtime: 253402300800, Ctime: 253402300799})
		v, e, _ := f.Inode(5)
		if !v.Times.Accessed.T.IsZero() || !v.Times.Modified.T.IsZero() {
			t.Error("out-of-range time not omitted")
		}
		if v.Times.Changed.T.Unix() != 253402300799 {
			t.Errorf("last representable time lost: %v", v.Times.Changed.T)
		}
		if val, _ := attr(e, "time"); val != "invalid" {
			t.Errorf("time attr %q", val)
		}
	})
	t.Run("zero times are absent", func(t *testing.T) {
		f, _ := buildOne(t, o, f2fstest.Inode{Mode: 0o100644})
		v, _, _ := f.Inode(5)
		if v.Times != (filesys.Times{}) {
			t.Errorf("times = %+v", v.Times)
		}
	})
	t.Run("bad name length is not fatal", func(t *testing.T) {
		f, _ := buildOne(t, o, f2fstest.Inode{Mode: 0o100644, RawOverrides: map[int][]byte{88: {0xFF, 0xFF, 0, 0}}})
		v, _, err := f.Inode(5)
		if err != nil || !v.NameBad || v.Name != "" {
			t.Errorf("name %q bad=%v err=%v", v.Name, v.NameBad, err)
		}
	})
}

func TestInodeChecksumMismatchFlagged(t *testing.T) {
	o := smallOpts()
	o.ExtraAttr, o.InodeChksum = true, true
	o.UUID = [16]byte{0xde, 0xad, 0xbe, 0xef}
	good := f2fstest.Inode{Mode: 0o100644, Size: 7, Extra: true, Generation: 12345, Name: "x"}

	t.Run("good", func(t *testing.T) {
		f, _ := buildOne(t, o, good)
		v, e, _ := f.Inode(5)
		if !v.ChecksumChecked || !v.ChecksumOK {
			t.Error("a correct checksum was not accepted")
		}
		if val, ok := attr(e, "checksum"); ok {
			t.Errorf("checksum attr %q on a good inode", val)
		}
	})
	t.Run("wrong stored value", func(t *testing.T) {
		bad := good
		bad.BadChecksum = true
		f, _ := buildOne(t, o, bad)
		v, e, err := f.Inode(5)
		if err != nil {
			t.Fatalf("a checksum mismatch must not fail the read: %v", err)
		}
		if v.ChecksumOK {
			t.Error("mismatch accepted")
		}
		if val, _ := attr(e, "checksum"); val != "bad" {
			t.Errorf("checksum attr %q, want bad", val)
		}
	})
	t.Run("every covered byte matters", func(t *testing.T) {
		// Flip one byte at several places after sealing: before the checksum
		// word, in i_generation, after the word (an address slot, the nid
		// array) and in the footer. The footer nid is not tested here: it
		// selects the node.
		_, img := buildOne(t, o, good)
		l := f2fstest.Geometry(o)
		base := int(l.Main) * 4096
		for _, off := range []int{0, 16, 68, 100, 372, 400, 3000, 4052, 4076, 4080, 4093} {
			img2 := append([]byte(nil), img...)
			img2[base+off] ^= 0x01
			f := mustOpen(t, img2)
			v, e, err := f.Inode(5)
			if off == 4076 { // footer ino: also breaks the inode identity
				_ = asCorrupt(t, err)
				continue
			}
			if err != nil {
				t.Errorf("offset %d: %v", off, err)
				continue
			}
			if v.ChecksumOK {
				t.Errorf("flipping byte %d went unnoticed", off)
			}
			if val, _ := attr(e, "checksum"); val != "bad" {
				t.Errorf("offset %d: checksum attr %q", off, val)
			}
		}
	})
	t.Run("not verified without extra attr", func(t *testing.T) {
		noExtra := good
		noExtra.Extra = false
		f, _ := buildOne(t, o, noExtra)
		v, e, _ := f.Inode(5)
		if v.ChecksumChecked {
			t.Error("checksum verified on an inode without the extra header")
		}
		if _, ok := attr(e, "checksum"); ok {
			t.Error("checksum attr without a check")
		}
	})
	t.Run("not verified when the header stops before the checksum", func(t *testing.T) {
		short := good
		short.ExtraIsize = 8
		f, _ := buildOne(t, o, short)
		if v, _, err := f.Inode(5); err != nil || v.ChecksumChecked {
			t.Errorf("err=%v checked=%v", err, v.ChecksumChecked)
		}
	})
	t.Run("not verified without the volume feature", func(t *testing.T) {
		o2 := o
		o2.InodeChksum = false
		bad := good
		bad.BadChecksum = true
		f, _ := buildOne(t, o2, bad)
		if v, _, _ := f.Inode(5); v.ChecksumChecked {
			t.Error("checksum verified on a volume without inode_checksum")
		}
	})
	t.Run("a different uuid seeds a different checksum", func(t *testing.T) {
		_, img := buildOne(t, o, good)
		sb32 := func(i []byte) {
			i[1024+108] ^= 0xff
			i[4096+1024+108] ^= 0xff
		}
		sb32(img)
		// sb_checksum is off in this image, so the superblocks still parse.
		f := mustOpen(t, img)
		if v, _, _ := f.Inode(5); v.ChecksumOK {
			t.Error("checksum accepted under a different uuid")
		}
	})
}

func TestInodeHostile(t *testing.T) {
	flexExtra := func() f2fstest.Options {
		o := smallOpts()
		o.ExtraAttr, o.InlineXattr = true, true
		return o
	}
	extra := func() f2fstest.Options { o := smallOpts(); o.ExtraAttr = true; return o }
	tests := []struct {
		name string
		o    f2fstest.Options
		in   f2fstest.Inode
	}{
		{"extra_isize 0xFFFF", extra(), f2fstest.Inode{Extra: true, ExtraIsize: 0xFFFF}},
		{"extra_isize one word past the area", extra(), f2fstest.Inode{Extra: true, ExtraIsize: 923*4 + 4}},
		{"extra_isize not a multiple of 4", extra(), f2fstest.Inode{Extra: true, ExtraIsize: 38}},
		{"extra_isize below the header", extra(), f2fstest.Inode{Extra: true, ExtraIsize: 2}},
		{"extra_isize zero", extra(), f2fstest.Inode{Extra: true, ExtraIsize: 0, RawOverrides: map[int][]byte{360: {0, 0}}}},
		{"extra header on a volume without the feature", smallOpts(), f2fstest.Inode{Extra: true}},
		{"flexible inline xattr size 0xFFFF", flexExtra(), f2fstest.Inode{Extra: true, Inline: f2fstest.InlineXattr, InlineXattrSize: 0xFFFF}},
		{"flexible inline xattr larger than the area by one", flexExtra(), f2fstest.Inode{Extra: true, Inline: f2fstest.InlineXattr, InlineXattrSize: 903 + 1}},
		{"flexible inline xattr size below the xattr header", flexExtra(), f2fstest.Inode{Extra: true, Inline: f2fstest.InlineXattr, InlineXattrSize: 5}},
		{"flexible inline xattr size zero with the flag", flexExtra(), f2fstest.Inode{Extra: true, Inline: f2fstest.InlineXattr, InlineXattrSize: 0}},
		{"flexible recorded size that cannot fit, flag clear", flexExtra(), f2fstest.Inode{Extra: true, InlineXattrSize: 0xFFFF}},
		{"default inline xattr does not fit beside a full extra header", extra(), f2fstest.Inode{Extra: true, ExtraIsize: 923 * 4, Inline: f2fstest.InlineXattr}},
		{"size beyond int64", smallOpts(), f2fstest.Inode{Size: 1 << 63}},
		{"not an inode (footer ino differs)", smallOpts(), f2fstest.Inode{FooterIno: 9}},
		{"footer nid differs", smallOpts(), f2fstest.Inode{FooterNID: 9}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := buildOne(t, tc.o, tc.in)
			_, _, err := f.Inode(5)
			_ = asCorrupt(t, err)
		})
	}

	t.Run("the largest legal reservation is accepted", func(t *testing.T) {
		o := flexExtra()
		f, _ := buildOne(t, o, f2fstest.Inode{Extra: true, Inline: f2fstest.InlineXattr, InlineXattrSize: 903})
		v, _, err := f.Inode(5)
		if err != nil || v.AddrSlots != 923-9-903 {
			t.Errorf("slots=%d err=%v", v.AddrSlots, err)
		}
	})

	t.Run("nid beyond the NAT", func(t *testing.T) {
		o := smallOpts()
		l := f2fstest.Geometry(o)
		f := mustOpen(t, f2fstest.Build(o, nil))
		for _, nid := range []uint32{l.NATCapacity(), l.NATCapacity() + 1, 1 << 31, math.MaxUint32} {
			_, _, err := f.Inode(nid)
			_ = asCorrupt(t, err)
		}
	})

	t.Run("block address outside the main area", func(t *testing.T) {
		o := smallOpts()
		l := f2fstest.Geometry(o)
		for _, addr := range []uint32{1, l.Main - 1, l.BlockCount, l.BlockCount + 7, math.MaxUint32} {
			oo := o
			oo.Nodes = []f2fstest.Node{{NID: 5, Addr: addr, Block: fileInode(o, 5, 1)}}
			f := mustOpen(t, f2fstest.Build(oo, nil))
			_, _, err := f.Inode(5)
			_ = asCorrupt(t, err)
		}
	})

	t.Run("reading from a truncated image", func(t *testing.T) {
		o := smallOpts()
		l := f2fstest.Geometry(o)
		last := l.BlockCount - 1
		o.Nodes = []f2fstest.Node{{NID: 5, Addr: last, Block: fileInode(o, 5, 1)}}
		img := f2fstest.Build(o, nil)
		f := mustOpen(t, img[:int(last)*4096+100]) // the last block is cut off
		_, _, err := f.Inode(5)
		_ = asCorrupt(t, err) // a short read is a property of the data, not an I/O error
	})
}

func TestInodeMutatedNeverPanics(t *testing.T) {
	o := smallOpts()
	o.ExtraAttr, o.InodeChksum, o.InodeCrtime, o.InlineXattr = true, true, true, true
	_, img := buildOne(t, o, f2fstest.Inode{Mode: 0o100644, Extra: true, Inline: f2fstest.InlineXattr, InlineXattrSize: 10, Name: "n"})
	l := f2fstest.Geometry(o)
	base := int(l.Main) * 4096
	// Overwrite each of the first 400 bytes of the inode with 0xFF, 0x00 and
	// 0x80, and the footer, and require a clean error or a value.
	for off := 0; off < 400; off++ {
		for _, b := range []byte{0xFF, 0x00, 0x80} {
			img2 := append([]byte(nil), img...)
			img2[base+off] = b
			f := mustOpen(t, img2)
			_, _, _ = f.Inode(5)
		}
	}
	for off := 4040; off < 4096; off++ {
		img2 := append([]byte(nil), img...)
		img2[base+off] ^= 0xFF
		f := mustOpen(t, img2)
		_, _, _ = f.Inode(5)
	}
}

func TestParseNodeID(t *testing.T) {
	for id, want := range map[string]uint32{"nid:1": 1, "nid:3": 3, "nid:4294967295": math.MaxUint32, "nid:100": 100} {
		if got, err := f2fs.ParseNodeID(id); err != nil || got != want {
			t.Errorf("ParseNodeID(%q) = %d, %v; want %d", id, got, err, want)
		}
	}
	for _, id := range []string{
		"", "nid:", "nid:0", "nid:01", "nid:007", "nid:+5", "nid:-5", "nid: 5", "nid:5 ", " nid:5",
		"nid:0x5", "nid:0X5", "nid:5a", "nid:5:", "nid:5:6", "nid:4294967296", "nid:99999999999999999999",
		"NID:5", "Nid:5", "nid5", "inode:5", "dentry:3:0:1", "id:5", "nid:\x00", "nid:٥", "nid:5\n", "nid:1_0",
	} {
		_, err := f2fs.ParseNodeID(id)
		if !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("ParseNodeID(%q): err = %v, want ErrNotFound", id, err)
		}
	}
}

func TestInodeFooterFlagAndNATOwner(t *testing.T) {
	// A node whose footer flag records a position in a node tree (offset > 0)
	// is a direct/indirect/xattr node, never an inode.
	for _, ofs := range []uint32{1, 2, 5, 0xFFFFFF} {
		f, _ := buildOne(t, smallOpts(), f2fstest.Inode{Mode: 0o100644, FooterFlag: ofs << 7})
		_, _, err := f.Inode(5)
		ce := asCorrupt(t, err)
		if !strings.Contains(ce.Reason, "not an inode node") {
			t.Errorf("offset %d: reason %q", ofs, ce.Reason)
		}
		if _, err := f.Open(filesys.Entry{ID: "nid:5"}); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("offset %d: Open = %v", ofs, err)
		}
	}
	// Low flag bits (cold, fsync, dentry marks) are fine.
	f, _ := buildOne(t, smallOpts(), f2fstest.Inode{Mode: 0o100644, FooterFlag: 0x7F})
	if _, _, err := f.Inode(5); err != nil {
		t.Errorf("low flag bits: %v", err)
	}

	// A NAT entry that names a different owner warns once; the inode still reads.
	o := smallOpts()
	o.Nodes = []f2fstest.Node{{NID: 5, Ino: 9, Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: 5, Mode: 0o100644})}}
	f = mustOpen(t, f2fstest.Build(o, nil))
	if len(f.Info().Warnings) != 0 {
		t.Fatalf("warnings at Open: %q", f.Info().Warnings)
	}
	if _, _, err := f.Inode(5); err != nil {
		t.Fatal(err)
	}
	if !hasWarning(f.Info(), "NAT entry records owner ino 9") {
		t.Errorf("warnings %q", f.Info().Warnings)
	}
	g, _ := buildOne(t, smallOpts(), f2fstest.Inode{Mode: 0o100644})
	if _, _, err := g.Inode(5); err != nil || len(g.Info().Warnings) != 0 {
		t.Errorf("consistent NAT owner: err %v warnings %q", err, g.Info().Warnings)
	}
}
