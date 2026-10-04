package ewf

import (
	"encoding/binary"
	"hash/adler32"
)

const (
	volumeSectionSize = 1128 // descriptor + 1052-byte payload
	volumePayloadLen  = 1052
)

// geometry is the validated content of a volume section.
type geometry struct {
	mediaType   uint8
	compression uint8
	chunks      uint32 // number_of_chunks
	spc         uint32 // sectors per chunk
	bps         uint32 // bytes per sector
	sectors     uint64
	chunkSize   int64
	size        int64 // media size in bytes
}

// parseVolume validates a 1052-byte volume payload. Field offsets (from the
// public format notes, confirmed against real images by the fixture tests):
// media_type @0, number_of_chunks @4, sectors_per_chunk @8,
// bytes_per_sector @12, number_of_sectors u64 @16, compression_level @52,
// checksum @1048 over bytes 0..1047.
func parseVolume(seg int, name string, p []byte) (geometry, error) {
	var g geometry
	if len(p) != volumePayloadLen {
		return g, corrupt(seg, name, "payload is %d bytes, expected %d", len(p), volumePayloadLen)
	}
	if got, want := binary.LittleEndian.Uint32(p[1048:]), adler32.Checksum(p[:1048]); got != want {
		return g, corrupt(seg, name, "checksum mismatch (stored %#08x, computed %#08x)", got, want)
	}
	g.mediaType = p[0]
	g.compression = p[52]
	g.chunks = binary.LittleEndian.Uint32(p[4:])
	g.spc = binary.LittleEndian.Uint32(p[8:])
	g.bps = binary.LittleEndian.Uint32(p[12:])
	g.sectors = binary.LittleEndian.Uint64(p[16:])

	switch g.bps {
	case 512, 1024, 2048, 4096:
	default:
		return g, corrupt(seg, name, "bytes_per_sector %d is not one of 512, 1024, 2048, 4096", g.bps)
	}
	if g.spc == 0 {
		return g, corrupt(seg, name, "sectors_per_chunk is 0")
	}
	cs, ok := mulOK(uint64(g.spc), uint64(g.bps))
	if !ok || cs > maxChunkSize {
		return g, corrupt(seg, name, "chunk of %d sectors * %d bytes exceeds the %d-byte limit", g.spc, g.bps, maxChunkSize)
	}
	g.chunkSize = int64(cs)
	size, ok := mulOK(g.sectors, uint64(g.bps))
	if !ok || size > 1<<63-1 {
		return g, corrupt(seg, name, "number_of_sectors %d * %d bytes overflows", g.sectors, g.bps)
	}
	g.size = int64(size)
	want := size / cs
	if size%cs != 0 {
		want++
	}
	if want > maxChunks {
		return g, corrupt(seg, name, "media needs %d chunks; the limit is %d", want, maxChunks)
	}
	if uint64(g.chunks) != want {
		return g, corrupt(seg, name, "number_of_chunks %d disagrees with the %d chunks implied by number_of_sectors %d", g.chunks, want, g.sectors)
	}
	return g, nil
}

// volume handles a volume or disk section.
func (o *opener) volume(n int, s Segment, sec section) error {
	label := sec.label()
	if sec.size != volumeSectionSize {
		return unsupported("segment %d: %s section is %d bytes (only the %d-byte layout is supported)", n, label, sec.size, volumeSectionSize)
	}
	p, _, err := readPayload(n, s, sec, volumePayloadLen, volumePayloadLen)
	if err != nil {
		return err
	}
	if o.haveVolume {
		// A repeated volume must describe the same media; a damaged repeat
		// is a warning because the first one is already validated.
		g, err := parseVolume(n, label, p)
		switch {
		case err != nil:
			o.r.warn.add("segment %d: second %s section ignored: %v", n, label, err)
		case g != o.r.geo:
			o.r.warn.add("segment %d: second %s section differs from the first (segment %d)", n, label, o.volSeg)
		}
		return nil
	}
	g, err := parseVolume(n, label, p)
	if err != nil {
		return err
	}
	o.r.geo = g
	o.haveVolume = true
	o.volSeg = n
	return nil
}

// data handles a data section (a copy of volume): only cross-checked.
func (o *opener) data(n int, s Segment, sec section) error {
	if sec.size != volumeSectionSize {
		o.r.warn.add("segment %d: data section is %d bytes, expected %d; ignored", n, sec.size, volumeSectionSize)
		return nil
	}
	p, _, err := readPayload(n, s, sec, volumePayloadLen, volumePayloadLen)
	if err != nil {
		return err
	}
	g, err := parseVolume(n, "data", p)
	if err != nil {
		o.r.warn.add("segment %d: data section ignored: %v", n, err)
		return nil
	}
	if o.dataGeo == nil {
		o.dataGeo = &g
	} else if *o.dataGeo != g {
		o.r.warn.add("segment %d: data section differs from the first data section", n)
	}
	return nil
}
