package apfstest

import "encoding/binary"

// Object types, storage flags and magics: the builder's own copy of the
// on-disk constants (it never imports the reader's, so a misreading in the
// reader cannot be mirrored here by accident).
const (
	typeNXSuperblock  = 1
	typeSpaceman      = 5
	typeOmap          = 0xb
	typeCheckpointMap = 0xc

	flagEphemeral = 0x80000000
	flagPhysical  = 0x40000000

	cpmLast = 1 // CHECKPOINT_MAP_LAST

	nxMagic         = "NXSB"
	fletcherModulus = 0xFFFFFFFF

	oidSuperblock    = 1
	oidSpaceman      = 1024
	firstUnusedOid   = 1026
	incompatVersion2 = 0x2
	nxCryptoSW       = 0x4

	checkpointMapStart = 40
)

// Fletcher64 is the builder's OWN implementation of the object checksum: the
// value stored in the first 8 bytes of an object, computed over b[8:] as
// little-endian 32-bit words (a trailing partial word is ignored). A
// straightforward one-word-at-a-time version, deliberately unlike the
// reader's, so that agreement between the two is meaningful.
func Fletcher64(b []byte) uint64 {
	var sum1, sum2 uint64
	if len(b) >= 8 {
		body := b[8:]
		for i := 0; i+4 <= len(body); i += 4 {
			sum1 = (sum1 + uint64(binary.LittleEndian.Uint32(body[i:]))) % fletcherModulus
			sum2 = (sum2 + sum1) % fletcherModulus
		}
	}
	c1 := fletcherModulus - (sum1+sum2)%fletcherModulus
	c2 := fletcherModulus - (sum1+c1)%fletcherModulus
	return c2<<32 | c1
}

// Seal recomputes and stores the Fletcher-64 of block blk of img, for tests
// that patched a block.
func Seal(img []byte, blockSize int, blk uint64) {
	off := int(blk) * blockSize
	sealBlock(img[off : off+blockSize])
}

func sealBlock(b []byte) {
	binary.LittleEndian.PutUint64(b[0:], Fletcher64(b))
}

func putObjHeader(b []byte, oid, xid uint64, typ, subtype uint32) {
	binary.LittleEndian.PutUint64(b[8:], oid)
	binary.LittleEndian.PutUint64(b[16:], xid)
	binary.LittleEndian.PutUint32(b[24:], typ)
	binary.LittleEndian.PutUint32(b[28:], subtype)
}
