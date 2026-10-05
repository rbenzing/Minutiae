package sqlitetest

import (
	"fmt"
	"slices"
)

// WAL builds a write-ahead log file byte by byte. Its checksum is its own
// implementation (word by word, no shared code with sqlitefile), so a test
// comparing the two is not circular.
type WAL struct {
	pageSize int
	big      bool
	salt1    uint32
	salt2    uint32
	ckpt     uint32
	buf      []byte
	slots    int    // frames of the current generation
	c1, c2   uint32 // running checksum: the last frame's (or the header's)
}

// NewWAL starts a WAL for the builder's page size. Words of the checksum are
// big-endian when bigEndian is set (magic 0x377f0683), else little-endian.
func (b *Builder) NewWAL(bigEndian bool, salt1, salt2, ckptSeq uint32) *WAL {
	w := &WAL{pageSize: b.o.PageSize, big: bigEndian, salt1: salt1, salt2: salt2, ckpt: ckptSeq}
	w.buf = make([]byte, 32)
	w.writeHeader()
	return w
}

func put32be(p []byte, v uint32) {
	p[0], p[1], p[2], p[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

func (w *WAL) word(p []byte) uint32 {
	if w.big {
		return uint32(p[0])<<24 | uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	}
	return uint32(p[3])<<24 | uint32(p[2])<<16 | uint32(p[1])<<8 | uint32(p[0])
}

// sum is the WAL checksum over p (a multiple of 8 bytes), continuing (a, b).
func (w *WAL) sum(p []byte, a, b uint32) (uint32, uint32) {
	for i := 0; i < len(p); i += 8 {
		a = a + w.word(p[i:]) + b
		b = b + w.word(p[i+4:]) + a
	}
	return a, b
}

func (w *WAL) writeHeader() {
	h := w.buf[:32]
	magic := uint32(0x377f0682)
	if w.big {
		magic++
	}
	put32be(h[0:], magic)
	put32be(h[4:], 3007000)
	put32be(h[8:], uint32(w.pageSize))
	put32be(h[12:], w.ckpt)
	put32be(h[16:], w.salt1)
	put32be(h[20:], w.salt2)
	w.c1, w.c2 = w.sum(h[:24], 0, 0)
	put32be(h[24:], w.c1)
	put32be(h[28:], w.c2)
}

func (w *WAL) slotSize() int { return 24 + w.pageSize }

// Frame appends a frame for page continuing the checksum chain. data must be
// exactly one page. commitDBPages is the database size in pages after the
// commit; non-zero makes the frame a commit frame. A frame lands at the slot
// after the current generation's last, overwriting whatever bytes lay there
// (a Reset leaves the old generation's bytes beyond the new end).
func (w *WAL) Frame(page uint32, data []byte, commitDBPages uint32) {
	if len(data) != w.pageSize {
		panic(fmt.Sprintf("sqlitetest: WAL frame data is %d bytes, the page size is %d", len(data), w.pageSize))
	}
	off := 32 + w.slots*w.slotSize()
	end := off + w.slotSize()
	if len(w.buf) < end {
		w.buf = append(w.buf, make([]byte, end-len(w.buf))...)
	}
	f := w.buf[off:end]
	put32be(f[0:], page)
	put32be(f[4:], commitDBPages)
	put32be(f[8:], w.salt1)
	put32be(f[12:], w.salt2)
	copy(f[24:], data)
	a, b := w.sum(f[:8], w.c1, w.c2)
	a, b = w.sum(f[24:], a, b)
	put32be(f[16:], a)
	put32be(f[20:], b)
	w.c1, w.c2 = a, b
	w.slots++
}

// Reset starts a new generation at slot 1 with new salts (the checkpoint
// sequence is incremented). The old frames stay in the file beyond the new
// end.
func (w *WAL) Reset(salt1, salt2 uint32) {
	w.salt1, w.salt2 = salt1, salt2
	w.ckpt++
	w.slots = 0
	w.writeHeader()
}

// Bytes returns a copy of the whole file.
func (w *WAL) Bytes() []byte { return slices.Clone(w.buf) }

// PatchFrame overwrites bytes of the frame at the 1-based slot, at off bytes
// from the start of the frame (its 24-byte header comes first, then the page).
// Checksums are not recomputed: it makes hostile files. It panics when the
// range lies outside the slot.
func (w *WAL) PatchFrame(slot int, off int, p ...byte) {
	if slot < 1 || off < 0 || off+len(p) > w.slotSize() || 32+slot*w.slotSize() > len(w.buf) {
		panic(fmt.Sprintf("sqlitetest: WAL patch of %d bytes at %d in slot %d is outside the file", len(p), off, slot))
	}
	copy(w.buf[32+(slot-1)*w.slotSize()+off:], p)
}

// CommitTo appends one frame per page that differs from since (or that since
// does not have), in page order, the last marked as the commit frame with the
// builder's current page count. Nothing is appended when no page changed.
func (b *Builder) CommitTo(w *WAL, since *Image) {
	b.settle()
	var changed []uint32
	for i, p := range b.pages {
		n := uint32(i + 1)
		if since == nil || n > since.Pages() || !slices.Equal(p, since.Page(n)) {
			changed = append(changed, n)
		}
	}
	for i, n := range changed {
		var commit uint32
		if i == len(changed)-1 {
			commit = uint32(len(b.pages))
		}
		w.Frame(n, b.pages[n-1], commit)
	}
}
