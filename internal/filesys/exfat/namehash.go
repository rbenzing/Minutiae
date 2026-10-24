package exfat

// nameHash is the NameHash of a File entry set's Stream Extension, as the
// published exFAT specification defines it (Stream Extension DirectoryEntry,
// NameHash field, section 7.6, with its algorithm): over the up-cased name's
// UTF-16LE bytes, low byte first, the 16-bit hash is rotated right by one bit
// and the byte added. up is the volume's up-case mapping; the caller verifies
// only with a usable table.
func nameHash(up func(uint16) uint16, units []uint16) uint16 {
	var h uint16
	for _, u := range units {
		u = up(u)
		for _, c := range [2]byte{byte(u), byte(u >> 8)} {
			h = (h&1)<<15 | h>>1
			h += uint16(c)
		}
	}
	return h
}
