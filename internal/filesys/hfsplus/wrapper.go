package hfsplus

import (
	"encoding/binary"
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Classic HFS master directory block (MDB) fields used to find an embedded
// HFS+ volume. Offsets are from Inside Macintosh / TN1150 as remembered
// (drAlBlkSiz @20, drAlBlSt @28, drEmbedSigWord @124, drEmbedExtent @126);
// they are validated against a real wrapped image when one is available.
const (
	mdbSig           = 0x4244 // "BD"
	offMDBAlBlkSiz   = 20     // u32: bytes per HFS allocation block
	offMDBAlBlSt     = 28     // u16: first HFS allocation block, in 512-byte sectors
	offMDBEmbedSig   = 124    // u16: "H+" or "HX" when an HFS+ volume is embedded
	offMDBEmbedStart = 126    // u16: first embedded HFS allocation block
	offMDBEmbedCount = 128    // u16: number of embedded HFS allocation blocks
	mdbMinLen        = 130    // bytes of the MDB the parser needs
	sectorSize       = 512
)

// wrapper is the place of an HFS+ volume embedded in a classic HFS volume.
type wrapper struct {
	base      int64 // byte offset of the embedded volume from the start of the image
	length    int64 // byte length of the embedded extent (may reach past the image: see truncated)
	truncated bool  // the embedded extent ends beyond the image
}

// parseWrapper decodes the MDB in b (the sector at volume byte 1024) of an
// image of size bytes. A classic HFS volume with no embedded HFS+ volume is
// filesys.ErrUnsupported. An embed that is arithmetically impossible, a
// non-512-multiple HFS block size, or an embedded extent that cannot hold a
// volume header (or whose start leaves no room for one in the image) is a
// *filesys.CorruptError. An extent that merely ends beyond the image is
// accepted and flagged truncated: the volume opens clamped, with a warning.
func parseWrapper(b []byte, size int64) (*wrapper, error) {
	if len(b) < mdbMinLen {
		return nil, corrupt("HFS wrapper", vhOffset, "%d bytes is too small for a master directory block", len(b))
	}
	be := binary.BigEndian
	embed := be.Uint16(b[offMDBEmbedSig:])
	if embed != sigHFSPlus && embed != sigHFSX {
		return nil, fmt.Errorf("%w: classic HFS volume without an embedded HFS+ volume", filesys.ErrUnsupported)
	}
	alBlkSiz := be.Uint32(b[offMDBAlBlkSiz:])
	if alBlkSiz == 0 || alBlkSiz%sectorSize != 0 {
		return nil, corrupt("HFS wrapper", vhOffset+offMDBAlBlkSiz, "HFS allocation block size %d is not a non-zero multiple of 512", alBlkSiz)
	}
	alBlSt := int64(be.Uint16(b[offMDBAlBlSt:]))
	startBlock := int64(be.Uint16(b[offMDBEmbedStart:]))
	blockCount := int64(be.Uint16(b[offMDBEmbedCount:]))
	// Every factor is <= 2^32, so these products cannot overflow int64.
	base, ok := filesys.AddOK(alBlSt*sectorSize, startBlock*int64(alBlkSiz))
	length := blockCount * int64(alBlkSiz)
	if !ok {
		return nil, corrupt("HFS wrapper", vhOffset+offMDBEmbedStart, "embedded volume start overflows")
	}
	end, ok := filesys.AddOK(base, length)
	if !ok {
		return nil, corrupt("HFS wrapper", vhOffset+offMDBEmbedStart, "embedded volume at %d, %d bytes long overflows", base, length)
	}
	if length < minVolumeSize {
		return nil, corrupt("HFS wrapper", vhOffset+offMDBEmbedCount, "embedded extent of %d bytes cannot hold a volume header", length)
	}
	w := &wrapper{base: base, length: length}
	if end > size {
		// A truncated image is evidence: the volume opens clamped to what the
		// image holds, with a warning. It must still hold the embedded header.
		if base > size || size-base < minVolumeSize {
			return nil, corrupt("HFS wrapper", vhOffset+offMDBEmbedStart, "embedded volume at %d leaves no room for a volume header in the %d-byte image", base, size)
		}
		w.truncated = true
	}
	return w, nil
}
