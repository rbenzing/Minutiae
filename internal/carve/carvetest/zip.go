package carvetest

import (
	"encoding/binary"
	"hash/crc32"
)

// ZIPEntry is one stored file of a synthetic archive.
type ZIPEntry struct {
	Name string
	Data []byte
}

// ZIP builds a ZIP archive with every entry stored (never compressed): a local
// header and the data per entry, the central directory, and the end of
// central directory record followed by the archive comment.
func ZIP(entries []ZIPEntry, comment string) []byte {
	le16 := binary.LittleEndian.AppendUint16
	le32 := binary.LittleEndian.AppendUint32
	var b []byte
	offs := make([]uint32, len(entries))
	for i, e := range entries {
		offs[i] = uint32(len(b))
		b = append(b, 'P', 'K', 3, 4)
		b = le16(b, 20) // version needed
		b = le16(b, 0)  // flags
		b = le16(b, 0)  // method: stored
		b = le16(b, 0)  // time
		b = le16(b, 0x21)
		b = le32(b, crc32.ChecksumIEEE(e.Data))
		b = le32(b, uint32(len(e.Data)))
		b = le32(b, uint32(len(e.Data)))
		b = le16(b, uint16(len(e.Name)))
		b = le16(b, 0) // extra
		b = append(b, e.Name...)
		b = append(b, e.Data...)
	}
	cd := uint32(len(b))
	for i, e := range entries {
		b = append(b, 'P', 'K', 1, 2)
		b = le16(b, 20) // version made by
		b = le16(b, 20) // version needed
		b = le16(b, 0)
		b = le16(b, 0)
		b = le16(b, 0)
		b = le16(b, 0x21)
		b = le32(b, crc32.ChecksumIEEE(e.Data))
		b = le32(b, uint32(len(e.Data)))
		b = le32(b, uint32(len(e.Data)))
		b = le16(b, uint16(len(e.Name)))
		b = le16(b, 0) // extra
		b = le16(b, 0) // comment
		b = le16(b, 0) // disk
		b = le16(b, 0) // internal attributes
		b = le32(b, 0) // external attributes
		b = le32(b, offs[i])
		b = append(b, e.Name...)
	}
	size := uint32(len(b)) - cd
	b = append(b, 'P', 'K', 5, 6)
	b = le16(b, 0)
	b = le16(b, 0)
	b = le16(b, uint16(len(entries)))
	b = le16(b, uint16(len(entries)))
	b = le32(b, size)
	b = le32(b, cd)
	b = le16(b, uint16(len(comment)))
	return append(b, comment...)
}
