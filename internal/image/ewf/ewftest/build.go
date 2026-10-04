// Package ewftest builds EWF version 1 (E01) segment files in memory for
// tests and fuzz seeds. It imports no Minutiae package (not even ewf): the
// builder and the reader are written independently from the format notes so
// that they cannot share a misreading.
//
// Layout written (everything little-endian, Adler-32 checksums):
//
//	segment header (13)  EVF\x09\x0d\x0a\xff\x00, 0x01, segment number u16, 0 u16
//	section descriptor (76)  type[16] next u64 size u64 padding[40] adler32
//	segment 1:  header, header2, volume, [data]
//	every segment: groups of sectors, table, table2
//	last segment:  error2, hash, digest, then done (or next when incomplete)
//	other segments end with next
package ewftest

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"  //nolint:gosec // MD5 is a stored value of the format, not a security control
	"crypto/sha1" //nolint:gosec // SHA-1 is a stored value of the format, not a security control
	"encoding/binary"
	"fmt"
	"hash/adler32"
	"unicode/utf16"
)

// Compress selects how chunks are stored.
type Compress int

const (
	// CompressNone stores every chunk raw followed by its Adler-32.
	CompressNone Compress = iota
	// CompressAll stores every chunk as a zlib stream.
	CompressAll
	// CompressMixed compresses a chunk only when that makes it smaller, like
	// real writers do.
	CompressMixed
)

// BaseMode selects the table base_offset convention.
type BaseMode int

const (
	// BaseZero writes base_offset 0 and absolute segment-file offsets.
	BaseZero BaseMode = iota
	// BaseSectorsData writes the offset of the sectors section descriptor (as
	// real acquisition output does) as base_offset and offsets relative to it.
	BaseSectorsData
)

// Options controls Build. The zero value is a valid single-segment image with
// 512-byte sectors, 64 sectors per chunk, uncompressed chunks and every
// optional section present.
type Options struct {
	BytesPerSector, SectorsPerChunk int // defaults 512, 64
	Compress                        Compress
	ChunksPerTable                  int // default 100
	ChunksPerSegment                int // 0 = one segment
	Base                            BaseMode
	NoTable2, NoTableFooter         bool
	NoHeader, NoHeader2             bool
	Case, Evidence, Description     string
	Examiner, Notes, Acquired       string
	NoHash, NoDigest                bool
	MD5Override, SHA1Override       []byte // stored value that differs from the media's
	ErrorRanges                     [][2]uint32
	NoDone                          bool // writes `next` last (incomplete image)
	SetID                           [16]byte

	// WithData also writes a `data` section (a copy of volume) after volume.
	WithData bool
	// Header2NoBOM omits the UTF-16 byte order mark from header2.
	Header2NoBOM bool
	// HeaderText and Header2Text replace the generated header text (before
	// compression and, for header2, UTF-16 encoding) when non-empty.
	HeaderText, Header2Text string
}

const (
	segHeaderLen = 13
	descLen      = 76
	volumeLen    = 1052
)

var signature = []byte{'E', 'V', 'F', 0x09, 0x0d, 0x0a, 0xff, 0x00}

func put32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func put64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }

type chunk struct {
	data       []byte // bytes as stored (compressed stream, or raw + adler)
	compressed bool
}

func encodeChunk(raw []byte, c Compress) chunk {
	if c != CompressNone {
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		_, _ = zw.Write(raw)
		_ = zw.Close()
		if c == CompressAll || zb.Len() < len(raw) {
			return chunk{data: zb.Bytes(), compressed: true}
		}
	}
	out := make([]byte, len(raw)+4)
	copy(out, raw)
	put32(out[len(raw):], adler32.Checksum(raw))
	return chunk{data: out}
}

type writer struct{ buf []byte }

func (w *writer) descriptor(typ string, next, size int64) {
	d := make([]byte, descLen)
	copy(d, typ)
	put64(d[16:], uint64(next))
	put64(d[24:], uint64(size))
	put32(d[72:], adler32.Checksum(d[:72]))
	w.buf = append(w.buf, d...)
}

// section appends a descriptor followed by payload; next points at whatever
// comes after the payload.
func (w *writer) section(typ string, payload []byte) {
	off := int64(len(w.buf))
	size := int64(descLen + len(payload))
	w.descriptor(typ, off+size, size)
	w.buf = append(w.buf, payload...)
}

// terminal appends a next/done section whose next points at itself.
func (w *writer) terminal(typ string) {
	off := int64(len(w.buf))
	w.descriptor(typ, off, descLen)
}

func segmentHeader(n int) []byte {
	b := make([]byte, segHeaderLen)
	copy(b, signature)
	b[8] = 1
	binary.LittleEndian.PutUint16(b[9:], uint16(n))
	return b
}

func zlibBytes(p []byte) []byte {
	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	_, _ = zw.Write(p)
	_ = zw.Close()
	return zb.Bytes()
}

func headerText(o Options) string {
	if o.HeaderText != "" {
		return o.HeaderText
	}
	r := "n"
	switch o.Compress {
	case CompressAll:
		r = "b"
	case CompressMixed:
		r = "f"
	}
	return "1\nmain\nc\tn\ta\te\tt\tav\tov\tm\tu\tp\tr\n" +
		o.Case + "\t" + o.Evidence + "\t" + o.Description + "\t" + o.Examiner + "\t" + o.Notes +
		"\tewftest\tTest\t" + o.Acquired + "\t" + o.Acquired + "\t0\t" + r + "\n\n"
}

func header2Text(o Options) string {
	if o.Header2Text != "" {
		return o.Header2Text
	}
	return headerText(o)
}

func utf16le(s string, bom bool) []byte {
	var out []byte
	if bom {
		out = append(out, 0xff, 0xfe)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func volumePayload(o Options, nChunks int, sectors uint64) []byte {
	v := make([]byte, volumeLen)
	v[0] = 1 // fixed disk
	put32(v[4:], uint32(nChunks))
	put32(v[8:], uint32(o.SectorsPerChunk))
	put32(v[12:], uint32(o.BytesPerSector))
	put64(v[16:], sectors)
	v[36] = 1 // media flags: physical
	switch o.Compress {
	case CompressAll:
		v[52] = 2
	case CompressMixed:
		v[52] = 1
	}
	put32(v[56:], uint32(o.SectorsPerChunk))
	copy(v[64:], o.SetID[:])
	put32(v[1048:], adler32.Checksum(v[:1048]))
	return v
}

func tablePayload(entries []uint32, base uint64, footer bool) []byte {
	p := make([]byte, 24+4*len(entries), 24+4*len(entries)+4)
	put32(p[0:], uint32(len(entries)))
	put64(p[8:], base)
	put32(p[20:], adler32.Checksum(p[:20]))
	for i, e := range entries {
		put32(p[24+4*i:], e)
	}
	if footer {
		p = binary.LittleEndian.AppendUint32(p, adler32.Checksum(p[24:]))
	}
	return p
}

func hashPayloads(o Options, media []byte) (hashP, digestP []byte) {
	m := md5.Sum(media)  //nolint:gosec // format-stored hash
	s := sha1.Sum(media) //nolint:gosec // format-stored hash
	md5v, sha1v := m[:], s[:]
	if o.MD5Override != nil {
		md5v = o.MD5Override
	}
	if o.SHA1Override != nil {
		sha1v = o.SHA1Override
	}
	hashP = make([]byte, 36)
	copy(hashP, md5v)
	put32(hashP[32:], adler32.Checksum(hashP[:32]))
	digestP = make([]byte, 80)
	copy(digestP, md5v)
	copy(digestP[16:], sha1v)
	put32(digestP[76:], adler32.Checksum(digestP[:76]))
	return hashP, digestP
}

func error2Payload(ranges [][2]uint32) []byte {
	p := make([]byte, 520+8*len(ranges)+4)
	put32(p[0:], uint32(len(ranges)))
	put32(p[516:], adler32.Checksum(p[:516]))
	for i, r := range ranges {
		put32(p[520+8*i:], r[0])
		put32(p[520+8*i+4:], r[1])
	}
	put32(p[520+8*len(ranges):], adler32.Checksum(p[520:520+8*len(ranges)]))
	return p
}

// Build returns one []byte per segment file, segment 1 first. The media
// length must be a multiple of BytesPerSector.
func Build(o Options, media []byte) [][]byte {
	if o.BytesPerSector == 0 {
		o.BytesPerSector = 512
	}
	if o.SectorsPerChunk == 0 {
		o.SectorsPerChunk = 64
	}
	if o.ChunksPerTable <= 0 {
		o.ChunksPerTable = 100
	}
	if len(media)%o.BytesPerSector != 0 {
		panic(fmt.Sprintf("ewftest: media length %d is not a multiple of %d", len(media), o.BytesPerSector))
	}
	chunkSize := o.BytesPerSector * o.SectorsPerChunk
	nChunks := (len(media) + chunkSize - 1) / chunkSize
	chunks := make([]chunk, nChunks)
	for i := range chunks {
		hi := min((i+1)*chunkSize, len(media))
		chunks[i] = encodeChunk(media[i*chunkSize:hi], o.Compress)
	}
	perSeg := o.ChunksPerSegment
	if perSeg <= 0 {
		perSeg = max(nChunks, 1)
	}
	nSeg := max(1, (nChunks+perSeg-1)/perSeg)
	hashP, digestP := hashPayloads(o, media)

	segs := make([][]byte, nSeg)
	for s := range nSeg {
		w := &writer{buf: segmentHeader(s + 1)}
		if s == 0 {
			if !o.NoHeader {
				w.section("header", zlibBytes([]byte(headerText(o))))
			}
			if !o.NoHeader2 {
				w.section("header2", zlibBytes(utf16le(header2Text(o), !o.Header2NoBOM)))
			}
			vol := volumePayload(o, nChunks, uint64(len(media)/o.BytesPerSector))
			w.section("volume", vol)
			if o.WithData {
				w.section("data", vol)
			}
		}
		lo, hi := s*perSeg, min(nChunks, (s+1)*perSeg)
		for g := lo; g < hi; g += o.ChunksPerTable {
			writeGroup(w, o, chunks[g:min(hi, g+o.ChunksPerTable)])
		}
		last := s == nSeg-1
		if last {
			if len(o.ErrorRanges) > 0 {
				w.section("error2", error2Payload(o.ErrorRanges))
			}
			if !o.NoHash {
				w.section("hash", hashP)
			}
			if !o.NoDigest {
				w.section("digest", digestP)
			}
		}
		if last && !o.NoDone {
			w.terminal("done")
		} else {
			w.terminal("next")
		}
		segs[s] = w.buf
	}
	return segs
}

func writeGroup(w *writer, o Options, group []chunk) {
	sectorsOff := int64(len(w.buf))
	dataStart := sectorsOff + descLen
	var payload []byte
	entries := make([]uint32, len(group))
	var base uint64
	if o.Base == BaseSectorsData {
		base = uint64(sectorsOff)
	}
	for i, c := range group {
		pos := dataStart + int64(len(payload))
		if o.Base == BaseSectorsData {
			pos -= sectorsOff
		}
		if pos >= 1<<31 {
			panic("ewftest: chunk offset does not fit 31 bits")
		}
		e := uint32(pos)
		if c.compressed {
			e |= 0x80000000
		}
		entries[i] = e
		payload = append(payload, c.data...)
	}
	w.section("sectors", payload)
	tp := tablePayload(entries, base, !o.NoTableFooter)
	w.section("table", tp)
	if !o.NoTable2 {
		w.section("table2", tp)
	}
}

// Section describes one section descriptor of a segment file.
type Section struct {
	Type               string
	Offset, Next, Size int64
}

// Sections leniently re-parses the section descriptors of a segment produced
// by Build (or a mutation of it): it never panics, stops at a terminal
// section, a self or backwards pointer, or the end of the data, and does not
// verify checksums.
func Sections(seg []byte) []Section {
	var out []Section
	off := int64(segHeaderLen)
	for len(out) < 1<<20 && off >= 0 && off+descLen <= int64(len(seg)) {
		d := seg[off : off+descLen]
		typ := d[:16]
		if i := bytes.IndexByte(typ, 0); i >= 0 {
			typ = typ[:i]
		}
		s := Section{
			Type:   string(typ),
			Offset: off,
			Next:   int64(binary.LittleEndian.Uint64(d[16:])),
			Size:   int64(binary.LittleEndian.Uint64(d[24:])),
		}
		out = append(out, s)
		if s.Next <= off {
			break
		}
		off = s.Next
	}
	return out
}

// FixDescriptor rewrites the type, next, size and Adler-32 of the descriptor
// at s.Offset from s.
func FixDescriptor(seg []byte, s Section) {
	d := seg[s.Offset : s.Offset+descLen]
	clear(d[:16])
	copy(d[:16], s.Type)
	put64(d[16:], uint64(s.Next))
	put64(d[24:], uint64(s.Size))
	put32(d[72:], adler32.Checksum(d[:72]))
}

// SetU32 stores v little-endian at seg[off:].
func SetU32(seg []byte, off int, v uint32) { put32(seg[off:], v) }

// FixAdler stores the Adler-32 of seg[from:to] at seg[at:].
func FixAdler(seg []byte, from, to, at int) { put32(seg[at:], adler32.Checksum(seg[from:to])) }

// ChunkLoc says where the builder put one chunk.
type ChunkLoc struct {
	Segment        int // 1-based segment file number
	Offset, Length int64
	Compressed     bool
}

// ChunkLocs lists, in chunk order, where each chunk lives, by re-parsing the
// `table` sections (not table2) of segs. A chunk ends at the next chunk or at
// the end of its sectors section.
func ChunkLocs(segs [][]byte) []ChunkLoc {
	var out []ChunkLoc
	for si, seg := range segs {
		secs := Sections(seg)
		var sectorsEnd int64
		for _, s := range secs {
			switch s.Type {
			case "sectors":
				sectorsEnd = min(s.Offset+s.Size, int64(len(seg)))
			case "table":
				p := s.Offset + descLen
				if p+24 > int64(len(seg)) {
					continue
				}
				n := int64(binary.LittleEndian.Uint32(seg[p:]))
				base := int64(binary.LittleEndian.Uint64(seg[p+8:]))
				n = min(n, (int64(len(seg))-p-24)/4)
				entry := func(i int64) (int64, bool) {
					e := binary.LittleEndian.Uint32(seg[p+24+4*i:])
					return base + int64(e&0x7fffffff), e&0x80000000 != 0
				}
				for i := range n {
					off, comp := entry(i)
					end := sectorsEnd
					if i+1 < n {
						end, _ = entry(i + 1)
					}
					out = append(out, ChunkLoc{Segment: si + 1, Offset: off, Length: end - off, Compressed: comp})
				}
			}
		}
	}
	return out
}
