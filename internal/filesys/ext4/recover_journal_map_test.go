package ext4

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// These tests drive mapStaleWith with the home source and the journal source on builder images whose depth-1 file keeps
// its extent leaf at home or in the journal (3G-R12).

var rjmUUID = [16]byte{0xA1, 0xB2, 0xC3, 0xD4, 0xE5, 0xF6, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

type rjmImg struct {
	f    *FS
	img  []byte
	l    *ext4test.Layout
	loc  ext4test.FileLoc
	data []byte
	in   *inode
}

func rjmPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13) ^ byte(i>>8) ^ 0x5A
	}
	return b
}

func rjmOpts() ext4test.Options {
	return ext4test.Options{Extents: true, MetadataCsum: true, Bit64: true, UUID: [16]byte{9, 8, 7, 6, 5, 4, 3, 2, 1}, JournalBlocks: 64, BlocksPerGroup: 2048}
}

type rjmJBD func(img []byte, l *ext4test.Layout, loc ext4test.FileLoc) []ext4test.JTxn

type rjmPost func(img []byte, l *ext4test.Layout, loc ext4test.FileLoc)

// rjmBuild builds a deleted file, lets jbd return the journal transactions (it sees the image as it is before the
// unlink), applies unlink and post, writes the journal and opens the image. in is the on-disk inode of the file.
func rjmBuild(t *testing.T, o ext4test.Options, file ext4test.File, unlink ext4test.UnlinkEffects, jbd rjmJBD, post rjmPost) rjmImg {
	t.Helper()
	file.Path, file.Deleted = "/gone.bin", true
	if file.Generation == 0 {
		file.Generation = 7
	}
	img, l := ext4test.BuildLayout(o, []ext4test.File{file})
	loc := l.File("/gone.bin")
	var txns []ext4test.JTxn
	if jbd != nil {
		txns = jbd(img, l, loc)
	}
	l.Unlink(img, "/gone.bin", unlink)
	if post != nil {
		post(img, l, loc)
	}
	l.WriteJournal(img, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 10, Start: 1, UUID: rjmUUID, Txns: txns})
	f, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Clone(img[loc.InodeOffset : loc.InodeOffset+int64(l.InodeSize)])
	in, _, err := f.decodeInodeQuiet(loc.Inode, raw)
	if err != nil {
		t.Fatal(err)
	}
	return rjmImg{f: f, img: img, l: l, loc: loc, data: file.Data, in: in}
}

func (r rjmImg) prepare(t *testing.T, use filesys.JournalUse) *jrecState {
	t.Helper()
	if err := r.f.PrepareJournalRecovery(context.Background(), use); err != nil {
		t.Fatal(err)
	}
	r.f.jrecMu.Lock()
	defer r.f.jrecMu.Unlock()
	if r.f.jrec == nil || r.f.jrec.idx == nil {
		t.Fatal("PrepareJournalRecovery left no journal state")
	}
	return r.f.jrec
}

// txnOf is the index of the transaction of sequence seq.
func txnOf(t *testing.T, s *jrecState, seq uint32) uint32 {
	t.Helper()
	for i := range s.idx.txns {
		if s.idx.txns[i].seq == seq {
			return uint32(i)
		}
	}
	t.Fatalf("no transaction of sequence %d", seq)
	return 0
}

func rjmTxn(seq uint32, blk int, data []byte) ext4test.JTxn {
	return ext4test.JTxn{Seq: seq, At: -1, Tags: []ext4test.JTag{{FSBlock: uint64(blk), Data: data}}}
}

// mapOf maps the entry's inode from the journal source of the transaction of sequence seq.
func (r rjmImg) mapOf(t *testing.T, s *jrecState, seq uint32) staleMap {
	t.Helper()
	m, err := r.f.mapStaleWith(r.in, min(r.in.size, r.f.size), s.source(txnOf(t, s, seq)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func rjmLeafFile() ext4test.File {
	return ext4test.File{Data: rjmPattern(5*1024 + 100), ExtentLeaves: 1}
}

func rjmReuse(img []byte, l *ext4test.Layout, loc ext4test.FileLoc) {
	l.SetBlockFree(img, loc.Tree[0], false)
}

func concatRunsOf(img []byte, runs []filesys.Run) []byte {
	var out []byte
	for _, r := range runs {
		out = append(out, img[r.Offset:r.Offset+r.Length]...)
	}
	return out
}

func TestMapStaleWithHomeEqualsMapStale(t *testing.T) {
	blockMap := rjmOpts()
	blockMap.Extents, blockMap.MetadataCsum = false, false
	scenarios := []struct {
		name   string
		o      ext4test.Options
		file   ext4test.File
		unlink ext4test.UnlinkEffects
		post   rjmPost
	}{
		{"depth 0", rjmOpts(), ext4test.File{Data: rjmPattern(4000)}, ext4test.DebugfsRm, nil},
		{"depth 1, leaf free", rjmOpts(), rjmLeafFile(), ext4test.DebugfsRm, nil},
		{"depth 1, leaf reused", rjmOpts(), rjmLeafFile(), ext4test.DebugfsRm, rjmReuse},
		{"depth 1, leaf checksum", rjmOpts(), rjmLeafFile(), ext4test.DebugfsRm, func(img []byte, l *ext4test.Layout, loc ext4test.FileLoc) {
			img[loc.Tree[0]*l.BlockSize+l.BlockSize-1] ^= 0xFF
		}},
		{"depth 1, leaf damaged", rjmOpts(), rjmLeafFile(), ext4test.DebugfsRm, func(img []byte, l *ext4test.Layout, loc ext4test.FileLoc) {
			img[loc.Tree[0]*l.BlockSize] ^= 0xFF
		}},
		{"block map", blockMap, ext4test.File{Data: rjmPattern(20 * 1024)}, ext4test.DebugfsRm, nil},
		{"cleared", rjmOpts(), ext4test.File{Data: rjmPattern(4000)}, ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroSize: true, ClearExtentRoot: true}, nil},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			r := rjmBuild(t, sc.o, sc.file, sc.unlink, nil, sc.post)
			for _, limit := range []int64{r.in.size, 1024, 1, 0, r.f.size + 5} {
				want, werr := r.f.mapStale(r.in, limit)
				got, gerr := r.f.mapStaleWith(r.in, limit, homeSource{})
				if !reflect.DeepEqual(want, got) || (werr == nil) != (gerr == nil) {
					t.Errorf("limit %d: mapStaleWith(home) = %+v, %v; mapStale = %+v, %v", limit, got, gerr, want, werr)
				}
			}
		})
	}
}

func TestJournalMapTreeBlockFromJournalCopy(t *testing.T) {
	r := rjmBuild(t, rjmOpts(), rjmLeafFile(), ext4test.DebugfsRm,
		func(img []byte, l *ext4test.Layout, loc ext4test.FileLoc) []ext4test.JTxn {
			return []ext4test.JTxn{rjmTxn(10, loc.Tree[0], l.BlockCopy(img, loc.Tree[0])), rjmTxn(11, 3, make([]byte, 1024))}
		}, rjmReuse)
	leaf := r.loc.Tree[0]
	if m, err := r.f.mapStale(r.in, r.in.size); err != nil || m.stop != "map-block-not-free@0" {
		t.Fatalf("the home map of a reused leaf = %+v, %v; want the 3C stop map-block-not-free@0", m, err)
	}
	s := r.prepare(t, filesys.JournalUseCommitted)
	m := r.mapOf(t, s, 11)
	if m.stop != "" || !bytes.Equal(concatRunsOf(r.img, m.runs), r.data) {
		t.Fatalf("stop %q, runs reproduce the file: %v; want the full map from the journal copy", m.stop, bytes.Equal(concatRunsOf(r.img, m.runs), r.data))
	}
	if !reflect.DeepEqual(m.tree, []uint64{uint64(leaf)}) || !reflect.DeepEqual(m.treeFrom, []string{"journal:10"}) {
		t.Errorf("tree %v from %v; want the leaf from journal:10", m.tree, m.treeFrom)
	}
}

func TestJournalMapStopsAtTreeBlockNewerThanCopy(t *testing.T) {
	r := rjmBuild(t, rjmOpts(), rjmLeafFile(), ext4test.DebugfsRm,
		func(img []byte, l *ext4test.Layout, loc ext4test.FileLoc) []ext4test.JTxn {
			return []ext4test.JTxn{rjmTxn(10, 3, make([]byte, 1024)), rjmTxn(11, loc.Tree[0], l.BlockCopy(img, loc.Tree[0]))}
		}, nil) // the leaf is free at home: the home block may hold a later state than the copy at T=10
	s := r.prepare(t, filesys.JournalUseCommitted)
	m := r.mapOf(t, s, 10)
	if m.stop != "map-block-newer-than-copy@0" || len(m.runs) != 0 {
		t.Errorf("stop %q, %d runs; want map-block-newer-than-copy@0 and no run", m.stop, len(m.runs))
	}
	// At T=11 the copy itself is the state.
	if m := r.mapOf(t, s, 11); m.stop != "" || !reflect.DeepEqual(m.treeFrom, []string{"journal:11"}) {
		t.Errorf("at T=11 stop %q from %v; want the copy of 11", m.stop, m.treeFrom)
	}
}

func TestJournalMapHomeWhenNeverLogged(t *testing.T) {
	logged := func(_ []byte, _ *ext4test.Layout, _ ext4test.FileLoc) []ext4test.JTxn {
		return []ext4test.JTxn{rjmTxn(10, 3, make([]byte, 1024))}
	}
	r := rjmBuild(t, rjmOpts(), rjmLeafFile(), ext4test.DebugfsRm, logged, nil)
	m := r.mapOf(t, r.prepare(t, filesys.JournalUseCommitted), 10)
	if m.stop != "" || !reflect.DeepEqual(m.treeFrom, []string{"home"}) || !bytes.Equal(concatRunsOf(r.img, m.runs), r.data) {
		t.Errorf("stop %q from %v; want the full map with the leaf read at home", m.stop, m.treeFrom)
	}
	r = rjmBuild(t, rjmOpts(), rjmLeafFile(), ext4test.DebugfsRm, logged, rjmReuse)
	if m := r.mapOf(t, r.prepare(t, filesys.JournalUseCommitted), 10); m.stop != "map-block-not-free@0" {
		t.Errorf("stop %q; want map-block-not-free@0 for an allocated home block", m.stop)
	}
}

func TestJournalMapCopyChecksumUnderInode(t *testing.T) {
	r := rjmBuild(t, rjmOpts(), rjmLeafFile(), ext4test.DebugfsRm,
		func(img []byte, l *ext4test.Layout, loc ext4test.FileLoc) []ext4test.JTxn {
			cp := l.BlockCopy(img, loc.Tree[0])
			tail := 12 + int(binary.LittleEndian.Uint16(cp[4:]))*12
			cp[tail] ^= 0xFF
			return []ext4test.JTxn{rjmTxn(10, loc.Tree[0], cp)}
		}, nil)
	m := r.mapOf(t, r.prepare(t, filesys.JournalUseCommitted), 10)
	if m.stop != "map-block-checksum@0" || len(m.runs) != 0 {
		t.Errorf("stop %q, %d runs; want map-block-checksum@0", m.stop, len(m.runs))
	}
}

func TestJournalMapRevokedTreeCopyIsUsed(t *testing.T) {
	r := rjmBuild(t, rjmOpts(), rjmLeafFile(), ext4test.DebugfsRm,
		func(img []byte, l *ext4test.Layout, loc ext4test.FileLoc) []ext4test.JTxn {
			rv := ext4test.JTxn{Seq: 11, At: -1, Revokes: []uint64{uint64(loc.Tree[0])}}
			return []ext4test.JTxn{rjmTxn(10, loc.Tree[0], l.BlockCopy(img, loc.Tree[0])), rv}
		}, rjmReuse)
	m := r.mapOf(t, r.prepare(t, filesys.JournalUseCommitted), 11)
	if m.stop != "" || !reflect.DeepEqual(m.treeFrom, []string{"journal:10:revoked"}) || !bytes.Equal(concatRunsOf(r.img, m.runs), r.data) {
		t.Errorf("stop %q from %v; want the revoked copy used and named revoked", m.stop, m.treeFrom)
	}
}
