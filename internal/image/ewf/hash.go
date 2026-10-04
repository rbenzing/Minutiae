package ewf

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"hash/adler32"
	"strconv"
	"strings"
)

const (
	hashPayloadLen   = 36 // md5[16] unknown[16] adler32
	digestPayloadLen = 80 // md5[16] sha1[20] padding[40] adler32
	error2HeaderLen  = 520
)

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// hash handles a hash section: the MD5 of the media.
func (o *opener) hash(n int, s Segment, sec section) error {
	p, ok, err := readPayload(n, s, sec, hashPayloadLen, hashPayloadLen)
	if err != nil {
		return err
	}
	if !ok {
		o.r.warn.add("segment %d: hash section is smaller than %d bytes; stored hash section damaged, the MD5 cannot be verified", n, hashPayloadLen)
		o.hashDamaged = fmt.Sprintf("stored hash section damaged: segment %d hash section is smaller than %d bytes", n, hashPayloadLen)
		return nil
	}
	if got, want := binary.LittleEndian.Uint32(p[32:]), adler32.Checksum(p[:32]); got != want {
		o.r.warn.add("segment %d: hash section checksum mismatch; stored hash section damaged, the MD5 cannot be verified", n)
		o.hashDamaged = fmt.Sprintf("stored hash section damaged: segment %d hash section checksum mismatch", n)
		return nil
	}
	var v [16]byte
	copy(v[:], p)
	if allZero(v[:]) {
		return nil // no hash was stored
	}
	if o.hashMD5 != nil && *o.hashMD5 != v {
		o.r.warn.add("segment %d: a second hash section holds a different MD5; the first is kept", n)
		return nil
	}
	o.hashMD5 = &v
	return nil
}

// digest handles a digest section: MD5 and SHA-1 of the media.
func (o *opener) digest(n int, s Segment, sec section) error {
	p, ok, err := readPayload(n, s, sec, digestPayloadLen, digestPayloadLen)
	if err != nil {
		return err
	}
	if !ok {
		o.r.warn.add("segment %d: digest section is smaller than %d bytes; stored hash section damaged, the MD5 and SHA-1 cannot be verified", n, digestPayloadLen)
		o.digestDamaged = fmt.Sprintf("stored hash section damaged: segment %d digest section is smaller than %d bytes", n, digestPayloadLen)
		return nil
	}
	if got, want := binary.LittleEndian.Uint32(p[76:]), adler32.Checksum(p[:76]); got != want {
		o.r.warn.add("segment %d: digest section checksum mismatch; stored hash section damaged, the MD5 and SHA-1 cannot be verified", n)
		o.digestDamaged = fmt.Sprintf("stored hash section damaged: segment %d digest section checksum mismatch", n)
		return nil
	}
	var m [16]byte
	var s1 [20]byte
	copy(m[:], p)
	copy(s1[:], p[16:])
	if !allZero(m[:]) {
		if o.digestMD5 != nil && *o.digestMD5 != m {
			o.r.warn.add("segment %d: a second digest section holds a different MD5; the first is kept", n)
		} else {
			o.digestMD5 = &m
		}
	}
	if !allZero(s1[:]) {
		if o.digestSHA1 != nil && *o.digestSHA1 != s1 {
			o.r.warn.add("segment %d: a second digest section holds a different SHA-1; the first is kept", n)
		} else {
			o.digestSHA1 = &s1
		}
	}
	return nil
}

// resolveHashes picks the stored MD5/SHA-1: digest wins over hash.
func (o *opener) resolveHashes() {
	r := o.r
	if o.hashMD5 != nil && o.digestMD5 != nil && *o.hashMD5 != *o.digestMD5 {
		r.warn.add("hash and digest sections store different MD5 values; the digest section's is used")
	}
	switch {
	case o.digestMD5 != nil:
		r.md5 = hexOf(o.digestMD5[:])
	case o.hashMD5 != nil:
		r.md5 = hexOf(o.hashMD5[:])
	}
	if o.digestSHA1 != nil {
		r.sha1 = hexOf(o.digestSHA1[:])
	}
	// A hash section that was present but unusable must never read as "the
	// container stores no hash": Verify reports the affected hash unverified.
	// A hash that another valid section still stores is not affected.
	if r.md5 == "" {
		r.md5Damaged = cmp.Or(o.digestDamaged, o.hashDamaged)
	}
	if r.sha1 == "" {
		r.sha1Damaged = o.digestDamaged
	}
}

// error2Info is what the acquirer recorded about sectors it could not read.
type error2Info struct {
	count   uint64 // ranges present in the section(s)
	sectors uint64 // total sectors in all ranges
	ranges  [][2]uint32
}

const describedRanges = 8

func (e *error2Info) describe() string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(e.count, 10))
	b.WriteString(" (")
	for i, r := range e.ranges {
		if i == describedRanges {
			b.WriteString(", ...")
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d+%d", r[0], r[1])
	}
	b.WriteString(")")
	return b.String()
}

// error2 handles an error2 section: a 520-byte header (count u32, padding,
// Adler-32 at 516), count {first_sector u32, number_of_sectors u32} entries
// and an Adler-32 of the entries. The ranges are sectors the acquirer could
// not read; they are reported, not treated as container corruption.
func (o *opener) error2(n int, s Segment, sec section) error {
	hdr, ok, err := readPayload(n, s, sec, error2HeaderLen, error2HeaderLen)
	if err != nil {
		return err
	}
	if !ok {
		o.r.warn.add("segment %d: error2 section is smaller than %d bytes; ignored", n, error2HeaderLen)
		return nil
	}
	if got, want := binary.LittleEndian.Uint32(hdr[516:]), adler32.Checksum(hdr[:516]); got != want {
		o.r.warn.add("segment %d: error2 section header checksum mismatch; acquisition error ranges ignored", n)
		return nil
	}
	claimed := uint64(binary.LittleEndian.Uint32(hdr))
	avail := uint64(sec.payloadLen() - error2HeaderLen)
	count := min(claimed, avail/8)
	hasFooter := count*8+4 <= avail
	if count < claimed {
		o.r.warn.add("segment %d: error2 section claims %d ranges but holds room for %d", n, claimed, count)
	}
	sum := adler32.New()
	info := o.err2
	if info == nil {
		info = &error2Info{}
	}
	var kept [][2]uint32
	var sectors, seen uint64
	const block = 64 << 10
	buf := make([]byte, min(count*8, block))
	off := sec.payloadOff() + error2HeaderLen
	for seen < count {
		take := min(count-seen, block/8)
		b := buf[:take*8]
		if err := readFull(s.R, b, off); err != nil {
			return ioError(n, "error2 entries", off, err)
		}
		_, _ = sum.Write(b) // hash.Hash.Write never fails
		for i := uint64(0); i < take; i++ {
			first := binary.LittleEndian.Uint32(b[8*i:])
			num := binary.LittleEndian.Uint32(b[8*i+4:])
			sectors += uint64(num)
			if len(info.ranges)+len(kept) < maxErrorRanges {
				kept = append(kept, [2]uint32{first, num})
			}
		}
		off += int64(take * 8)
		seen += take
	}
	if hasFooter {
		var f [4]byte
		if err := readFull(s.R, f[:], off); err != nil {
			return ioError(n, "error2 footer", off, err)
		}
		if binary.LittleEndian.Uint32(f[:]) != sum.Sum32() {
			o.r.warn.add("segment %d: error2 entries checksum mismatch; acquisition error ranges ignored", n)
			return nil
		}
	}
	if count == 0 {
		return nil
	}
	info.count += count
	info.sectors += sectors
	info.ranges = append(info.ranges, kept...)
	o.err2 = info
	return nil
}
