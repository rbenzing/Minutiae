package hfsplus

import "encoding/binary"

// journalState is what Open learned about the volume's journal. The journal is
// never replayed (that belongs to roadmap sub-project 3); only its state is
// read so the reader can warn, and so Unallocated can refuse when pending
// transactions may have allocated blocks the on-disk bitmap does not show yet.
type journalState int

const (
	journalNone    journalState = iota // the volume is not journaled
	journalClean                       // journaled and nothing is pending
	journalPending                     // journaled with transactions that were never replayed
	journalUnknown                     // journaled but the journal could not be parsed
)

// Journal structures, from memory of TN1150 "Journal" and xnu vfs_journal.h;
// the journal info block layout (flags, offset, size) was confirmed on a real
// mkfs.hfsplus -J image, whose journal is marked kJIJournalNeedInitMask (flags
// 5) with an all-zero journal area; the journal_header layout and the
// endian marker are from memory only (no initialised journal was available).
//
// JournalInfoBlock (big-endian, at allocation block vh.journalInfoBlock):
// flags u32 @0, device_signature[8] u32 @4, offset u64 @36 (bytes from the
// start of the volume), size u64 @44.
//
// journal_header (at that offset), in the byte order of the machine that
// formatted the journal: magic u32 @0, endian u32 @4, start u64 @8, end u64
// @16, size u64 @24. The endian field reads 0x12345678 in the writer's order,
// so the byte order is recovered from it. start == end means nothing pending.
const (
	jibSize          = 52
	offJIBFlags      = 0
	offJIBOffset     = 36
	offJIBSize       = 44
	jibInFS          = 1 // kJIJournalInFSMask
	jibOtherDevice   = 2 // kJIJournalOnOtherDeviceMask
	jibNeedInit      = 4 // kJIJournalNeedInitMask: the journal header was never written
	jnlHeaderLen     = 32
	offJnlMagic      = 0
	offJnlEndian     = 4
	offJnlStart      = 8
	offJnlEnd        = 16
	offJnlSize       = 24
	jnlMagic         = 0x4A4E4C78 // "JNLx"
	jnlEndianMarker  = 0x12345678
	jnlEndianSwapped = 0x78563412
)

// loadJournal reads the journal info block and the journal header and sets
// f.journal, warning when the journal is pending or cannot be understood. An
// unreadable journal is never assumed clean.
func (f *FS) loadJournal() {
	vh := f.vh
	if vh.attributes&attrJournaled == 0 {
		return
	}
	f.journal = journalUnknown
	jb := vh.journalInfoBlock
	if jb == 0 || jb >= vh.totalBlocks {
		f.warn("the volume is journaled but its journal info block %d is outside the %d-block volume; the journal state is unknown", jb, vh.totalBlocks)
		return
	}
	var b [jibSize]byte
	if err := readFull(f.r, b[:], int64(jb)*int64(vh.blockSize)); err != nil {
		f.warn("the journal info block cannot be read (%v); the journal state is unknown", err)
		return
	}
	be := binary.BigEndian
	flags := be.Uint32(b[offJIBFlags:])
	if flags&jibOtherDevice != 0 || flags&jibInFS == 0 {
		f.warn("the journal is not inside the volume (journal info flags %#x); the journal state is unknown", flags)
		return
	}
	off, size := be.Uint64(b[offJIBOffset:]), be.Uint64(b[offJIBSize:])
	declared := uint64(vh.volumeBytes())
	if size < jnlHeaderLen || off > declared || size > declared-off {
		f.warn("the journal (offset %d, size %d) does not fit the %d-byte volume; the journal state is unknown", off, size, declared)
		return
	}
	f.jrnlOff, f.jrnlSize = int64(off), int64(size) // both <= declared <= 2^62: the journal is inside the volume
	if flags&jibNeedInit != 0 {
		f.journal = journalClean // the journal was created but never used: no header, no transactions
		return
	}
	var h [jnlHeaderLen]byte
	if err := readFull(f.r, h[:], int64(off)); err != nil { // off < declared <= 2^62
		f.warn("the journal header cannot be read (%v); the journal state is unknown", err)
		return
	}
	var order binary.ByteOrder
	switch be.Uint32(h[offJnlEndian:]) {
	case jnlEndianMarker:
		order = binary.BigEndian
	case jnlEndianSwapped:
		order = binary.LittleEndian
	default:
		f.warn("the journal header has an unrecognised byte-order marker %#x; the journal state is unknown", be.Uint32(h[offJnlEndian:]))
		return
	}
	if m := order.Uint32(h[offJnlMagic:]); m != jnlMagic {
		f.warn("the journal header has magic %#x, not %#x; the journal state is unknown", m, uint32(jnlMagic))
		return
	}
	start, end, jsize := order.Uint64(h[offJnlStart:]), order.Uint64(h[offJnlEnd:]), order.Uint64(h[offJnlSize:])
	if jsize > size || start > jsize || end > jsize {
		f.warn("the journal header (start %d, end %d, size %d) is inconsistent with its %d-byte area; the journal state is unknown", start, end, jsize, size)
		return
	}
	if start == end {
		f.journal = journalClean
		return
	}
	f.journal = journalPending
	f.warn("the journal has pending transactions (start %d, end %d) that were not replayed: the on-disk volume may be inconsistent and its allocation bitmap stale", start, end)
}
