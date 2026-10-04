package ewf

import (
	"encoding/binary"
	"fmt"
	"hash/adler32"
	"slices"
)

type kind uint8

const (
	kUnknown kind = iota
	kHeader
	kHeader2
	kVolume
	kDisk
	kData
	kSectors
	kTable
	kTable2
	kHash
	kDigest
	kError2
	kNext
	kDone
)

var kindByName = map[string]kind{
	"header": kHeader, "header2": kHeader2, "volume": kVolume, "disk": kDisk,
	"data": kData, "sectors": kSectors, "table": kTable, "table2": kTable2,
	"hash": kHash, "digest": kDigest, "error2": kError2, "next": kNext, "done": kDone,
}

var kindNames = func() [kDone + 1]string {
	var n [kDone + 1]string
	for s, k := range kindByName {
		n[k] = s
	}
	return n
}()

func (k kind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return "unknown"
}

// section is one validated section descriptor.
type section struct {
	kind       kind
	name       string // on-disk type, kept only for kUnknown (sanitized)
	off        int64  // absolute offset of the descriptor in its segment file
	next, size int64  // as validated by the walk (size is the claimed one)
	psize      int64  // payload bound: min(size, next-off) incl. descriptor; 0 for terminals
}

func (s section) payloadOff() int64 { return s.off + descLen }

func (s section) payloadLen() int64 {
	if s.psize <= descLen {
		return 0
	}
	return s.psize - descLen
}

func (s section) label() string {
	if s.kind == kUnknown {
		return s.name
	}
	return s.kind.String()
}

// walk follows the section chain of segment i. It returns every section in
// file order and the kind that ended the chain: kNext, kDone, or kUnknown
// when the chain runs exactly to the end of the file with no terminal
// section, or (last segment only) when the file is truncated: a descriptor
// that is cut by the end of the file, or whose checksum is valid but whose
// size or next pointer runs past it, ends the walk with a warning and the
// sections walked so far are kept. In any other segment, and for a descriptor
// whose checksum fails, those are corrupt.
func (o *opener) walk(i int) ([]section, kind, error) {
	n := i + 1
	s := o.r.segs[i]
	last := i == len(o.r.segs)-1
	limit := min(s.Size/descLen, maxSections)
	var secs []section
	visited := map[int64]struct{}{}
	off := int64(segHeaderLen)
	var desc [descLen]byte
	// Sections whose size disagrees with next-off are summarised in one
	// warning, so a hostile chain cannot flood the warning list.
	var mismatches int
	var firstMismatch string
	defer func() {
		if mismatches > 0 {
			o.r.warn.add("segment %d: %d section(s) have a size that differs from the next-offset (first: %s)", n, mismatches, firstMismatch)
		}
	}()
	// truncated ends the walk of a cut-off last segment, or reports corruption.
	truncated := func(at int64, format string, args ...any) ([]section, kind, error) {
		if !last {
			return nil, 0, corrupt(n, "", format, args...)
		}
		o.r.warn.add("segment %d truncated at offset %d (%s)", n, at, fmt.Sprintf(format, args...))
		return secs, kUnknown, nil
	}
	for {
		if off == s.Size && len(secs) > 0 {
			return secs, kUnknown, nil // chain ends at EOF without next/done
		}
		if int64(len(secs)) >= limit {
			return nil, 0, corrupt(n, "", "more than %d sections", limit)
		}
		if off > s.Size-descLen {
			return truncated(off, "section descriptor at offset %d does not fit the %d-byte segment", off, s.Size)
		}
		if _, dup := visited[off]; dup {
			return nil, 0, corrupt(n, "", "section chain revisits offset %d", off)
		}
		visited[off] = struct{}{}
		if err := readFull(s.R, desc[:], off); err != nil {
			return nil, 0, ioError(n, "section descriptor", off, err)
		}
		if got, want := binary.LittleEndian.Uint32(desc[72:]), adler32.Checksum(desc[:72]); got != want {
			return nil, 0, corrupt(n, "", "section descriptor at offset %d: checksum mismatch (stored %#08x, computed %#08x)", off, got, want)
		}
		typ := desc[:16]
		if z := slices.Index(typ, 0); z >= 0 {
			typ = typ[:z]
		}
		sec := section{kind: kindByName[string(typ)], off: off}
		if sec.kind == kUnknown {
			sec.name = safeName(string(typ))
		}
		next := binary.LittleEndian.Uint64(desc[16:])
		size := binary.LittleEndian.Uint64(desc[24:])
		label := sec.label()
		end, ok := addOK(uint64(off), size)
		if !ok || end > uint64(s.Size) {
			return truncated(off, "%s section size %d at offset %d runs past the end of the %d-byte segment", label, size, off, s.Size)
		}
		terminal := sec.kind == kNext || sec.kind == kDone
		if size < descLen && !terminal {
			return nil, 0, corrupt(n, label, "size %d is smaller than a section descriptor", size)
		}
		sec.size = int64(size)
		if terminal {
			if next != uint64(off) {
				o.r.warn.add("segment %d: %s section at offset %d has a next pointer (%d) that is not its own offset", n, label, off, next)
			}
			if size != 0 && size != descLen {
				o.r.warn.add("segment %d: %s section at offset %d has size %d (real writers use 0; 76 is also accepted)", n, label, off, size)
			}
			sec.next = off
			secs = append(secs, sec)
			return secs, sec.kind, nil
		}
		switch {
		case next == uint64(off):
			return nil, 0, corrupt(n, label, "next points at its own offset %d", off)
		case next < uint64(off):
			return nil, 0, corrupt(n, label, "next %d points backwards from offset %d", next, off)
		case next < uint64(off)+descLen:
			return nil, 0, corrupt(n, label, "next %d overlaps the descriptor at offset %d", next, off)
		case next > uint64(s.Size):
			// The section itself fits (checked above); the chain does not.
			sec.next = off + int64(size)
			sec.psize = int64(size)
			secs = append(secs, sec)
			return truncated(int64(min(next, 1<<62)), "%s section at offset %d has next %d past the end of the %d-byte segment", label, off, next, s.Size)
		}
		sec.next = int64(next)
		// The payload never extends past the next descriptor, whatever size claims.
		sec.psize = min(int64(size), sec.next-off)
		if size != next-uint64(off) {
			if mismatches == 0 {
				firstMismatch = fmt.Sprintf("%s section at offset %d has size %d but next-offset is %d", label, off, size, next-uint64(off))
			}
			mismatches++
		}
		secs = append(secs, sec)
		off = sec.next
	}
}
