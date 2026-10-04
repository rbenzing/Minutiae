package sqlitetest

// Byte-level encoders for the database header and an empty b-tree page. They
// are written from the file-format description, never from sqlitefile's
// decoders.

func put16(b []byte, v uint16) { b[0], b[1] = byte(v>>8), byte(v) }

func put32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

func get32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// writeHeader writes the 100-byte database header of an empty one-page file.
func writeHeader(p []byte, o Options) {
	copy(p, "SQLite format 3\x00")
	ps := uint16(o.PageSize) // 65536 wraps to 0 ...
	if o.PageSize == 65536 {
		ps = 1 // ... and is stored as 1
	}
	put16(p[16:], ps)
	p[18], p[19] = 1, 1 // write / read version: rollback journal
	p[20] = byte(o.Reserved)
	p[21], p[22], p[23] = 64, 32, 32 // payload fractions
	put32(p[24:], o.ChangeCounter)
	put32(p[28:], 1) // database size in pages
	// 32..43: freelist trunk, freelist count and schema cookie stay 0
	put32(p[44:], 4) // schema format
	if o.AutoVacuum != 0 {
		put32(p[52:], 1) // largest root page: non-zero means autovacuum is on
	}
	put32(p[56:], uint32(o.Encoding))
	put32(p[60:], uint32(o.UserVersion))
	if o.AutoVacuum == 2 {
		put32(p[64:], 1) // incremental vacuum
	}
	put32(p[68:], o.AppID)
	put32(p[92:], o.ChangeCounter) // version-valid-for
	put32(p[96:], SQLiteVersion)
}

// emptyTableLeaf writes an empty table leaf b-tree page header at hdr:
// flag 0x0d, no freeblock, no cells, content starting at the usable size
// (stored as 0 when that is 65536), no fragmented bytes.
func emptyTableLeaf(p []byte, hdr, usable int) {
	p[hdr] = 0x0d
	put16(p[hdr+5:], uint16(usable)) // 65536 wraps to 0, as the format says
}
