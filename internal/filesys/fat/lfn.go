package fat

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

const (
	attrReadOnly = 0x01
	attrHidden   = 0x02
	attrSystem   = 0x04
	attrVolume   = 0x08
	attrDir      = 0x10
	attrArchive  = 0x20
	attrLFN      = 0x0F // read-only|hidden|system|volume: a long-name entry
	attrLFNMask  = 0x3F

	lfnUnitsPerEntry = 13
	maxLFNEntries    = 20 // a long name has at most 255 UTF-16 units: 20 entries

	// rawPrefix marks the display form of a name that cannot be shown as it is:
	// "~raw~" + base64url of the on-disk name bytes (UTF-16LE for a long name).
	rawPrefix = "~raw~"
)

// isLFN reports whether the 32-byte entry has the long-name attribute pattern.
func isLFN(e []byte) bool { return e[11]&attrLFNMask == attrLFN }

// lfnChecksum is the checksum of an 11-byte short name stored in every
// long-name entry of its set.
func lfnChecksum(short []byte) byte {
	var sum byte
	for _, c := range short[:11] {
		sum = (sum&1)<<7 + sum>>1 + c
	}
	return sum
}

// lfnOffsets are the byte offsets of the 13 UTF-16LE units of a long-name entry.
var lfnOffsets = [lfnUnitsPerEntry]int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}

// lfnChunk copies the 13 units of a long-name entry into dst.
func lfnChunk(e []byte, dst []uint16) {
	for i, o := range lfnOffsets {
		dst[i] = binary.LittleEndian.Uint16(e[o:])
	}
}

// lfnAsm reassembles a live long-name set (entries come highest ordinal first).
// It is a small state machine fed one entry at a time; any irregularity breaks
// the set, which is then ignored (the short entry gets its 8.3 name).
type lfnAsm struct {
	active bool // a set is in progress and intact
	broken bool // long-name entries were seen that do not form a valid set
	expect int  // ordinal expected next; 1 once the set is complete
	n      int  // ordinal of the first (last-flagged) entry
	sum    byte
	units  [maxLFNEntries * lfnUnitsPerEntry]uint16
}

func (a *lfnAsm) reset() { a.active, a.broken = false, false }

// add feeds a live long-name entry.
func (a *lfnAsm) add(e []byte) {
	ord := e[0]
	if ord&0x40 != 0 { // first entry on disk: the highest ordinal
		n := int(ord &^ 0x40)
		if n < 1 || n > maxLFNEntries {
			a.active, a.broken = false, true
			return
		}
		a.active, a.broken = true, false
		a.n, a.expect, a.sum = n, n, e[13]
		a.store(n, e)
		return
	}
	if !a.active || a.expect <= 1 || ord == 0 || int(ord) != a.expect-1 || e[13] != a.sum {
		a.active, a.broken = false, true
		return
	}
	a.expect--
	a.store(int(ord), e)
}

func (a *lfnAsm) store(ord int, e []byte) {
	lfnChunk(e, a.units[(ord-1)*lfnUnitsPerEntry:ord*lfnUnitsPerEntry])
}

// take ends the set at a short entry (name11 are its 11 name bytes). It returns
// the long name units when a complete set with a matching checksum precedes it;
// orphan reports long-name entries that did not form such a set.
func (a *lfnAsm) take(name11 []byte) (units []uint16, ok, orphan bool) {
	defer a.reset()
	if a.active && a.expect == 1 && a.sum == lfnChecksum(name11) {
		if u := trimUnits(a.units[:a.n*lfnUnitsPerEntry]); len(u) > 0 {
			return append([]uint16(nil), u...), true, false
		}
	}
	return nil, false, a.active || a.broken
}

// deletedLFN reassembles the name of a deleted short entry from the deleted
// long-name entries immediately before it (pending, in disk order). Their
// ordinals are lost, so they are taken by position: the nearest is ordinal 1.
// The set's checksum covers the original short name, whose first byte deletion
// overwrote with 0xE5; exactly one first byte reproduces the checksum, and the
// set is accepted only if that byte is a legal short-name start that agrees with
// the long name (its upper-cased first letter or digit). At least one entry is
// required; entries are taken while their checksum equals that of the nearest.
func deletedLFN(pending [][32]byte, name11 []byte) ([]uint16, bool) {
	if len(pending) == 0 {
		return nil, false
	}
	sum := pending[len(pending)-1][13]
	first, ok := recoverFirstByte(name11, sum)
	if !ok {
		return nil, false
	}
	var units []uint16
	n := 0
	for i := len(pending) - 1; i >= 0 && n < maxLFNEntries; i-- {
		if pending[i][13] != sum {
			break
		}
		var c [lfnUnitsPerEntry]uint16
		lfnChunk(pending[i][:], c[:])
		units = append(units, c[:]...)
		n++
	}
	units = trimUnits(units)
	if len(units) == 0 || !agrees(first, units) {
		return nil, false
	}
	return units, true
}

// recoverFirstByte finds the byte b such that the short name b+name11[1:] has
// the checksum sum. The checksum is a bijection of the first byte, so there is
// exactly one.
func recoverFirstByte(name11 []byte, sum byte) (byte, bool) {
	var tmp [11]byte
	copy(tmp[:], name11)
	for b := range 256 {
		tmp[0] = byte(b)
		if lfnChecksum(tmp[:]) == sum {
			return byte(b), true
		}
	}
	return 0, false
}

// agrees reports whether first can be the first byte of the short name of a
// file with the long name units: a legal short-name byte, and when the long
// name starts (after spaces and dots) with an ASCII letter or digit, its upper
// case.
func agrees(first byte, units []uint16) bool {
	if first < 0x21 || first == 0x7F || strings.IndexByte(`"*+,/:;<=>?[\]|`, first) >= 0 {
		return false
	}
	for _, u := range units {
		if u == ' ' || u == '.' {
			continue
		}
		switch {
		case u >= 'a' && u <= 'z':
			return first == byte(u)-'a'+'A'
		case u >= 'A' && u <= 'Z' || u >= '0' && u <= '9':
			return first == byte(u)
		}
		break
	}
	return true
}

// trimUnits cuts a name at its 0x0000 terminator; without one, trailing 0xFFFF
// padding is dropped.
func trimUnits(u []uint16) []uint16 {
	for i, c := range u {
		if c == 0 {
			return u[:i]
		}
	}
	for len(u) > 0 && u[len(u)-1] == 0xFFFF {
		u = u[:len(u)-1]
	}
	return u
}

// longName returns the display name of long-name units and the raw bytes to keep
// as Entry.RawName. A name that is valid UTF-16 without '/' and that is not "."
// or ".." is shown as it is (raw is nil); any other is shown losslessly as
// "~raw~" + base64url(UTF-16LE).
func longName(u []uint16) (name string, raw []byte) {
	valid := true
	for i := 0; i < len(u); i++ {
		switch c := u[i]; {
		case c >= 0xD800 && c < 0xDC00:
			if i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000 {
				i++
			} else {
				valid = false
			}
		case c >= 0xDC00 && c < 0xE000:
			valid = false
		}
	}
	if valid {
		name = string(utf16.Decode(u))
		if name != "" && name != "." && name != ".." && !strings.Contains(name, "/") {
			return name, nil
		}
	}
	raw = make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(raw[2*i:], c)
	}
	return rawPrefix + base64.RawURLEncoding.EncodeToString(raw), raw
}

func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// shortName renders the 8.3 name of a short entry. NTRes bits 3 and 4 make the
// base and the extension lower case. The first byte 0x05 stands for 0xE5; for a
// deleted entry the lost first byte is shown as '_'. shown is the readable
// name (used for lookups); name and raw are the Entry.Name and Entry.RawName:
// raw is the 11 on-disk bytes when they are not plain printable ASCII (the
// codepage is unknown), and a name that cannot be shown as it is (empty, "." or
// "..", or containing '/') is shown in the "~raw~" form.
func shortName(name11 []byte, ntres byte, deleted bool) (shown, name string, raw []byte) {
	var b [11]byte
	copy(b[:], name11)
	if deleted {
		b[0] = '_'
	} else if b[0] == 0x05 {
		b[0] = 0xE5
	}
	base, ext := oemString(b[:8]), oemString(b[8:])
	if ntres&0x08 != 0 {
		base = asciiLower(base)
	}
	if ntres&0x10 != 0 {
		ext = asciiLower(ext)
	}
	shown = base
	if ext != "" {
		shown += "." + ext
	}
	for i, c := range b {
		if (i > 0 || !deleted) && (c < 0x20 || c > 0x7E) {
			raw = append([]byte(nil), name11[:11]...)
			break
		}
	}
	if shown == "" || shown == "." || shown == ".." || strings.Contains(shown, "/") {
		return shown, rawPrefix + base64.RawURLEncoding.EncodeToString(name11[:11]), append([]byte(nil), name11[:11]...)
	}
	return shown, shown, raw
}

// attrString renders the attribute byte as letters (R H S V D A), "-" for none.
func attrString(a byte) string {
	var sb strings.Builder
	for _, f := range []struct {
		bit byte
		c   byte
	}{{attrReadOnly, 'R'}, {attrHidden, 'H'}, {attrSystem, 'S'}, {attrVolume, 'V'}, {attrDir, 'D'}, {attrArchive, 'A'}} {
		if a&f.bit != 0 {
			sb.WriteByte(f.c)
		}
	}
	if sb.Len() == 0 {
		return "-"
	}
	return sb.String()
}
