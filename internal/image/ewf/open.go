package ewf

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// opener carries the state of one Open call.
type opener struct {
	r *Reader

	haveVolume bool
	volSeg     int // segment the accepted volume came from
	dataGeo    *geometry

	hdr                        [2]*headerInfo // header, header2
	hdrRaw                     [2][]byte      // decompressed text, to spot a differing repeat
	hashMD5                    *[16]byte
	digestMD5                  *[16]byte
	digestSHA1                 *[20]byte
	hashDamaged, digestDamaged string // why a present hash/digest section is unusable, "" when it is not
	err2                       *error2Info
	unknown                    []string
	unknownCount               int

	// Per-segment limits on what overlapping or repeated sections can make
	// the opener read (see handle).
	budget       int64
	budgetWarned bool
	counts       [kDone + 1]int
}

func ioError(seg int, what string, off int64, err error) error {
	return fmt.Errorf("ewf: segment %d: reading %s at offset %d: %w", seg, what, off, err)
}

// segment validates segment i's header, walks its sections and handles them.
func (o *opener) segment(i int) error {
	n := i + 1
	s := o.r.segs[i]
	if s.R == nil {
		return corrupt(n, "", "segment has no reader")
	}
	if s.Size < segHeaderLen {
		return corrupt(n, "", "segment is %d bytes; the minimum is %d", s.Size, segHeaderLen+descLen)
	}
	var hdr [segHeaderLen]byte
	if err := readFull(s.R, hdr[:], 0); err != nil {
		return ioError(n, "segment header", 0, err)
	}
	switch {
	case bytes.HasPrefix(hdr[:], []byte("EVF2")):
		return unsupported("segment %d is a version 2 (Ex01) file", n)
	case bytes.HasPrefix(hdr[:], []byte("LVF")), bytes.HasPrefix(hdr[:], []byte("LEF2")):
		return unsupported("segment %d is a logical evidence (L01) file", n)
	}
	if s.Size < segHeaderLen+descLen {
		return corrupt(n, "", "segment is %d bytes; the minimum is %d", s.Size, segHeaderLen+descLen)
	}
	if !bytes.Equal(hdr[:8], []byte("EVF\x09\x0d\x0a\xff\x00")) {
		return corrupt(n, "", "bad segment signature % x", hdr[:8])
	}
	if hdr[8] != 1 || binary.LittleEndian.Uint16(hdr[11:]) != 0 {
		return corrupt(n, "", "bad segment header fields (start %#02x, end %#04x)", hdr[8], binary.LittleEndian.Uint16(hdr[11:]))
	}
	if got := int(binary.LittleEndian.Uint16(hdr[9:])); got != n {
		return corrupt(n, "", "segment file %d (%q) declares segment number %d; expected %d", n, s.Name, got, n)
	}

	o.budget = s.Size
	o.counts = [kDone + 1]int{}
	o.budgetWarned = false
	secs, term, err := o.walk(i)
	if err != nil {
		return err
	}
	o.r.secs[i] = secs
	last := i == len(o.r.segs)-1
	switch {
	case !last && term == kDone:
		return corrupt(n, "done", "done section in segment %d, which is not the last of %d segments", n, len(o.r.segs))
	case !last && term != kNext:
		return corrupt(n, "", "segment %d ends without a next section but %d segments follow", n, len(o.r.segs)-n)
	case last && term != kDone:
		o.r.warn.add("E01 set incomplete (no done section)")
	}
	for _, sec := range secs {
		if err := o.handle(n, s, sec); err != nil {
			return err
		}
	}
	for _, sec := range secs {
		if sec.kind == kSectors {
			o.r.sectors[i] = append(o.r.sectors[i], span{start: sec.payloadOff(), end: sec.off + sec.psize})
		}
	}
	return o.tables(i, secs)
}

// maxPerSegment caps how many sections of a kind one segment may have; real
// writers emit one or two. Extra sections are skipped with a warning.
var maxPerSegment = map[kind]int{
	kHeader: 4, kHeader2: 4, kVolume: 2, kDisk: 2, kData: 4, kHash: 4, kDigest: 4, kError2: 4,
}

// payloadCost is how many payload bytes the handler of sec will read.
func payloadCost(sec section) int64 {
	plen := sec.payloadLen()
	switch sec.kind {
	case kHeader, kHeader2:
		return min(plen, maxHeaderZ)
	case kVolume, kDisk, kData:
		return min(plen, volumePayloadLen)
	case kHash:
		return min(plen, hashPayloadLen)
	case kDigest:
		return min(plen, digestPayloadLen)
	case kError2:
		return plen
	}
	return 0
}

// admit applies the per-kind count cap and the per-segment payload budget
// (the segment's own size: sections whose payloads overlap cannot make Open
// read more than the file holds). It reports whether sec is processed.
func (o *opener) admit(n int, sec section) bool {
	class := sec.kind
	if class == kDisk {
		class = kVolume // volume and disk share one cap
	}
	if limit, ok := maxPerSegment[sec.kind]; ok {
		o.counts[class]++
		if o.counts[class] > limit {
			o.r.warn.add("segment %d: more than %d %s sections; the extra ones are skipped", n, limit, sec.label())
			return false
		}
	}
	if payloadCost(sec) > o.budget {
		if !o.budgetWarned {
			o.budgetWarned = true
			o.r.warn.add("segment %d: section payloads overlap or exceed the segment (%s at offset %d); the remaining sections are skipped", n, sec.label(), sec.off)
		}
		return false
	}
	o.budget -= payloadCost(sec)
	return true
}

// handle processes one section of segment n.
func (o *opener) handle(n int, s Segment, sec section) error {
	if !o.admit(n, sec) {
		return nil
	}
	switch sec.kind {
	case kHeader, kHeader2:
		return o.header(n, s, sec)
	case kVolume, kDisk:
		return o.volume(n, s, sec)
	case kData:
		return o.data(n, s, sec)
	case kTable, kTable2:
		if !o.haveVolume {
			return corrupt(n, sec.label(), "table section at offset %d precedes any volume section", sec.off)
		}
	case kHash:
		return o.hash(n, s, sec)
	case kDigest:
		return o.digest(n, s, sec)
	case kError2:
		return o.error2(n, s, sec)
	case kUnknown:
		o.unknownCount++
		if len(o.unknown) < maxUnknownKinds && !contains(o.unknown, sec.name) {
			o.unknown = append(o.unknown, sec.name)
		}
	}
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// readPayload reads up to want payload bytes of sec (fewer when the section
// is smaller); ok is false when the section is smaller than min.
func readPayload(n int, s Segment, sec section, minLen, want int64) ([]byte, bool, error) {
	plen := sec.payloadLen()
	if plen < minLen {
		return nil, false, nil
	}
	buf := make([]byte, min(plen, want))
	if err := readFull(s.R, buf, sec.payloadOff()); err != nil {
		return nil, false, ioError(n, sec.label()+" section", sec.payloadOff(), err)
	}
	return buf, true, nil
}

// finish runs the image-level checks and assembles the metadata.
func (o *opener) finish() error {
	r := o.r
	if !o.haveVolume {
		return corrupt(0, "", "no volume section")
	}
	if o.dataGeo != nil && *o.dataGeo != r.geo {
		r.warn.add("data section geometry differs from the volume section")
	}
	o.resolveHashes()
	if e := o.err2; e != nil {
		r.warn.add("acquisition error: %d sector range(s) (%d sectors) were unreadable at acquisition and are zero-filled in the image", e.count, e.sectors)
	}
	r.err2 = o.err2
	if covered := int64(len(r.refs)); covered < int64(r.geo.chunks) {
		// A missing or truncated segment or table: the image opens, and the
		// chunks without an entry fail on read (never zero-filled). A missing
		// table in the middle of the set also makes every later chunk
		// unreadable (Reader.gap), as their indexes cannot be proven.
		r.warn.add("chunk table covers %d of %d chunks (missing or truncated segment or table); reads of the other chunks fail", covered, r.geo.chunks)
	}
	r.cache = newChunkCache(cacheCapacity(r.geo.chunkSize))
	r.meta = o.metadata()
	return nil
}
