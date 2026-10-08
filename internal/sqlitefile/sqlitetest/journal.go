package sqlitetest

import (
	"fmt"
	"slices"
)

var journalMagic = []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

// Journal builds a rollback journal file byte by byte. Its checksum is its own
// implementation (no code shared with sqlitefile), so a test comparing the two
// is not circular.
type Journal struct {
	pageSize int
	sector   int // the layout sector: where headers and padding fall
	nonce    uint32
	initial  uint32
	buf      []byte
	segs     []jseg
}

type jseg struct {
	off   int
	n     uint32 // records appended
	fixed bool   // SetHeader wrote the count: Record no longer updates it
}

// NewJournal starts a journal for the builder's page size with a header at
// offset 0 padded to sector bytes (sector must be at least 28 so the header
// fits; hostile header FIELDS are set with SetHeader, the layout stays
// valid). nonce seeds the record checksums; initialPages is the database size
// in pages the header records.
func (b *Builder) NewJournal(sector int, nonce uint32, initialPages uint32) *Journal {
	if sector < 32 {
		panic(fmt.Sprintf("sqlitetest: journal layout sector %d is below 32", sector))
	}
	j := &Journal{pageSize: b.o.PageSize, sector: sector, nonce: nonce, initial: initialPages}
	j.startSegment()
	return j
}

func put32j(p []byte, v uint32) {
	p[0], p[1], p[2], p[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

// startSegment appends a header at the next multiple of the sector size.
func (j *Journal) startSegment() {
	off := (len(j.buf) + j.sector - 1) / j.sector * j.sector
	j.buf = append(j.buf, make([]byte, off+j.sector-len(j.buf))...)
	h := j.buf[off : off+28]
	copy(h, journalMagic)
	put32j(h[12:], j.nonce)
	put32j(h[16:], j.initial)
	put32j(h[20:], uint32(j.sector))
	put32j(h[24:], uint32(j.pageSize))
	j.segs = append(j.segs, jseg{off: off})
}

// NewSegment pads to the sector and starts another header (the transaction
// synced mid-way).
func (j *Journal) NewSegment() { j.startSegment() }

// cksum is the record checksum: the nonce plus the bytes at pageSize-200,
// pageSize-400, ... while the index is above zero.
func (j *Journal) cksum(data []byte) uint32 {
	c := j.nonce
	for i := len(data) - 200; i > 0; i -= 200 {
		c += uint32(data[i])
	}
	return c
}

// Record appends a record for page with the correct checksum. data must be
// exactly one page.
func (j *Journal) Record(page uint32, data []byte) { j.RawRecord(page, data, j.cksum(data)) }

// RawRecord appends a record with an explicit checksum.
func (j *Journal) RawRecord(page uint32, data []byte, cksum uint32) {
	if len(data) != j.pageSize {
		panic(fmt.Sprintf("sqlitetest: journal record data is %d bytes, the page size is %d", len(data), j.pageSize))
	}
	var r [4]byte
	put32j(r[:], page)
	j.buf = append(j.buf, r[:]...)
	j.buf = append(j.buf, data...)
	put32j(r[:], cksum)
	j.buf = append(j.buf, r[:]...)
	s := &j.segs[len(j.segs)-1]
	s.n++
	if !s.fixed {
		put32j(j.buf[s.off+8:], s.n)
	}
}

// Zero zeroes the first 28 bytes, as the PERSIST journal mode does after a
// commit.
func (j *Journal) Zero() { clear(j.buf[:28]) }

// SuperJournal appends the super-journal trailer: the lock-byte page marker,
// the name, its length, the sum of its bytes and the magic.
func (j *Journal) SuperJournal(name string) {
	var w [4]byte
	put32j(w[:], uint32((1<<30)/j.pageSize+1))
	j.buf = append(j.buf, w[:]...)
	j.buf = append(j.buf, name...)
	put32j(w[:], uint32(len(name)))
	j.buf = append(j.buf, w[:]...)
	var sum uint32
	for i := 0; i < len(name); i++ {
		sum += uint32(name[i])
	}
	put32j(w[:], sum)
	j.buf = append(j.buf, w[:]...)
	j.buf = append(j.buf, journalMagic...)
}

// SetHeader overwrites the fields of the CURRENT (latest) header: the record
// count, nonce, initial size, sector size and page size, for hostile and
// rule cases. The layout is unchanged; after it Record no longer updates this
// header's count.
func (j *Journal) SetHeader(nRec, nonce, initial, sector, pageSize uint32) {
	s := &j.segs[len(j.segs)-1]
	s.fixed = true
	h := j.buf[s.off : s.off+28]
	put32j(h[8:], nRec)
	put32j(h[12:], nonce)
	put32j(h[16:], initial)
	put32j(h[20:], sector)
	put32j(h[24:], pageSize)
}

// Bytes returns a copy of the whole file.
func (j *Journal) Bytes() []byte { return slices.Clone(j.buf) }

// NewSegmentNonce starts another segment like NewSegment, but its header carries
// its own nonce, which checksums the records after it: SQLite writes a fresh
// nonce in every header.
func (j *Journal) NewSegmentNonce(nonce uint32) {
	j.nonce = nonce
	j.startSegment()
}
