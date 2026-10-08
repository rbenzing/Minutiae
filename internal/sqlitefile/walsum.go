package sqlitefile

import "encoding/binary"

// WALChecksum folds data into the running WAL checksum (s0, s1): for every
// consecutive 8-byte unit of two 32-bit words (x0, x1), read big- or
// little-endian as the WAL magic says, s0 += x0 + s1 and s1 += x1 + s0. The
// header checksum starts from (0, 0); a frame's starts from the previous
// frame's stored checksum. len(data) must be a multiple of 8; a trailing
// remainder is ignored (never read past the buffer).
func WALChecksum(data []byte, bigEndian bool, s0, s1 uint32) (uint32, uint32) {
	var order binary.ByteOrder = binary.LittleEndian
	if bigEndian {
		order = binary.BigEndian
	}
	for i := 0; i+8 <= len(data); i += 8 {
		s0 += order.Uint32(data[i:]) + s1
		s1 += order.Uint32(data[i+4:]) + s0
	}
	return s0, s1
}
