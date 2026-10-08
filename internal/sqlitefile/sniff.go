package sqlitefile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// SniffKind classifies the start of a file.
type SniffKind uint8

// The kinds Sniff reports.
const (
	SniffEmpty SniffKind = iota
	SniffTooSmall
	SniffSQLite
	SniffWAL
	SniffJournal
	SniffNotSQLite
	SniffLooksEncrypted // not SQLite, and high entropy without a known container magic: a hint, never an assertion
)

// Sniffed is the result of Sniff.
type Sniffed struct {
	Kind     SniffKind
	Size     int64
	Entropy  float64 // Shannon bits per byte of what was read
	PageSize int     // from the header when the kind carries one and it is valid, else 0
}

const (
	sniffMax        = 4096 // most bytes Sniff reads
	sniffMinEntropy = 512  // fewest bytes for the encryption hint
	// entropyThreshold is the bits per byte from which content looks
	// encrypted or compressed.
	entropyThreshold = 7.2
	headerSize       = 100
)

const dbMagic = "SQLite format 3\x00"

const journalMagic = "\xd9\xd5\x05\xf9\x20\xa1\x63\xd7"

// sniffResult is Sniff's result with what Open needs: why the file is not a
// database, and the bytes read.
type sniffResult struct {
	Sniffed
	reason NotSQLiteReason // set when the file is not taken for a database
	buf    []byte
}

// Sniff classifies the start of the file read through r; size is
// authoritative. It reads at most 4096 bytes.
func Sniff(r io.ReaderAt, size int64) (s Sniffed, err error) {
	defer guard(&err)
	res, err := sniff(r, size)
	if err != nil {
		return Sniffed{}, err
	}
	return res.Sniffed, nil
}

func sniff(r io.ReaderAt, size int64) (sniffResult, error) {
	if size < 0 {
		return sniffResult{}, fmt.Errorf("sqlitefile: negative file size %d", size)
	}
	buf := make([]byte, min(size, sniffMax))
	if len(buf) > 0 {
		if err := readFull(r, buf, 0); err != nil {
			return sniffResult{}, err
		}
	}
	res := sniffResult{Sniffed: Sniffed{Size: size, Entropy: Entropy(buf)}, buf: buf}
	hasMagic := len(buf) >= len(dbMagic) && string(buf[:len(dbMagic)]) == dbMagic
	switch {
	case size == 0:
		res.Kind, res.reason = SniffEmpty, ReasonEmpty
		return res, nil
	case !hasMagic && len(buf) >= 4 && isWALMagic(binary.BigEndian.Uint32(buf)):
		res.Kind, res.reason = SniffWAL, ReasonBadMagic
		if len(buf) >= 12 {
			res.PageSize = validOrZero(int(binary.BigEndian.Uint32(buf[8:])))
		}
		return res, nil
	case !hasMagic && len(buf) >= len(journalMagic) && string(buf[:len(journalMagic)]) == journalMagic:
		res.Kind, res.reason = SniffJournal, ReasonBadMagic
		if len(buf) >= 28 {
			res.PageSize = validOrZero(int(binary.BigEndian.Uint32(buf[24:])))
		}
		return res, nil
	case size < headerSize:
		res.Kind, res.reason = SniffTooSmall, ReasonTooSmall
		return res, nil
	case hasMagic && len(buf) > headerSize && (buf[headerSize] == 0x05 || buf[headerSize] == 0x0d):
		res.Kind = SniffSQLite
		res.PageSize = headerPageSize(binary.BigEndian.Uint16(buf[16:]))
		return res, nil
	case hasMagic:
		res.reason = ReasonPage1Invalid
	default:
		res.reason = ReasonBadMagic
	}
	res.Kind = SniffNotSQLite
	if len(buf) >= sniffMinEntropy && res.Entropy >= entropyThreshold && (hasMagic || !knownContainer(buf)) {
		res.Kind = SniffLooksEncrypted
	}
	return res, nil
}

func isWALMagic(m uint32) bool { return m == 0x377f0682 || m == 0x377f0683 }

func validOrZero(n int) int {
	if validPageSize(n) {
		return n
	}
	return 0
}

// headerPageSize decodes the header's 2-byte page size field: 1 means 65536;
// 0 is returned for a value that is not a page size.
func headerPageSize(v uint16) int {
	if v == 1 {
		return maxPageSize
	}
	if int(v) <= maxPageSize/2 {
		return validOrZero(int(v))
	}
	return 0
}

// containerMagics are the starts of common compressed, archive, media and
// markup formats, which rule out the encryption hint. Only multi-byte magics
// are listed: a one-byte prefix would drop the hint from about 1 in 128 random
// files, and text formats (JSON, source) have far too little entropy to reach
// the hint anyway.
var containerMagics = [...]string{
	"\x1f\x8b", "PK\x03\x04", "PK\x05\x06", "PK\x07\x08", "\x89PNG", "\xff\xd8\xff", "bplist", "<?xml",
	"\x28\xb5\x2f\xfd", "\xfd7zXZ\x00", "BZh", "7z\xbc\xaf\x27\x1c", "Rar!\x1a\x07", "%PDF", "OggS", "\x1a\x45\xdf\xa3",
	"GIF8", "RIFF", "ID3",
}

// knownContainer reports whether b starts with a known container magic (an
// MP4-family file carries its magic at offset 4).
func knownContainer(b []byte) bool {
	for _, m := range containerMagics {
		if bytes.HasPrefix(b, []byte(m)) {
			return true
		}
	}
	return len(b) >= 8 && string(b[4:8]) == "ftyp"
}

// Entropy returns the Shannon entropy of b in bits per byte (0 for empty).
func Entropy(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var counts [256]int
	for _, c := range b {
		counts[c]++
	}
	n := float64(len(b))
	var h float64
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}

// notSQLite builds the error Open returns for a file that is not a database.
func (s sniffResult) notSQLite() error {
	return &NotSQLiteError{Reason: s.reason, Size: s.Size, Entropy: s.Entropy, LooksEncrypted: s.Kind == SniffLooksEncrypted}
}
