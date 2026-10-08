package sqlitefile

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Record is a decoded record.
type Record struct {
	Serials   []uint64
	Values    []Value
	HeaderLen int
	BodyLen   int64 // the sum of the declared value sizes
	// Reserved counts the columns whose serial type is reserved (10, 11), which
	// read as zero-width NULL, as the engine reads them.
	Reserved int
	// Truncated: a value lies (wholly or partly) past the end of the payload
	// that was available, so it is Omitted, and so is every later value. The live
	// view keeps such a row only when a damaged overflow chain caused it
	// (with a warning) and skips a record that declares bytes its intact payload lacks.
	Truncated bool
}

// SerialSize is the byte length of a value of the given serial type. The
// reserved types 10 and 11 take no bytes: the engine reads them as a NULL
// (TestEngineReservedSerialTypesReadAsNull).
func SerialSize(serial uint64) int64 {
	switch serial {
	case 0, 8, 9, 10, 11:
		return 0
	case 1:
		return 1
	case 2:
		return 2
	case 3:
		return 3
	case 4:
		return 4
	case 5:
		return 6
	case 6, 7:
		return 8
	}
	return int64((serial - 12) / 2)
}

func recordInvalid(format string, a ...any) error {
	return &CorruptError{File: FileDB, Reason: "record: " + fmt.Sprintf(format, a...)}
}

// ParseRecordHeader decodes the record header at the start of b: the header
// length varint (which counts itself) and one serial type per column. b is
// the payload or at least its first headerLen bytes. maxCols caps the number
// of columns. bodyLen is the sum of the declared value sizes; it is not
// compared with len(b) (a strict user checks len(b)-headerLen >= bodyLen).
// A header length below its own varint or past b, a serial varint that runs
// past the header, more than maxCols columns
// and a body length that overflows are errors wrapping ErrCorrupt. The reserved
// serial types 10 and 11 are accepted (zero-width NULLs, as the engine reads
// them; the caller counts and warns). A header
// that holds no columns (length 1) is accepted, as the engine does.
func ParseRecordHeader(b []byte, maxCols int) (serials []uint64, headerLen int, bodyLen int64, err error) {
	defer guard(&err)
	pureAt("ParseRecordHeader")
	hl, n := GetVarint(b)
	switch {
	case n == 0:
		return nil, 0, 0, recordInvalid("the header length varint runs past the payload (%d bytes)", len(b))
	case hl < uint64(n):
		return nil, 0, 0, recordInvalid("header length %d is smaller than its own %d bytes", hl, n)
	case hl > uint64(len(b)):
		return nil, 0, 0, recordInvalid("header length %d is larger than the %d bytes available", hl, len(b))
	}
	headerLen = int(hl)
	hdr := b[n:headerLen]
	for len(hdr) > 0 {
		s, k := GetVarint(hdr)
		if k == 0 {
			return nil, 0, 0, recordInvalid("a serial type runs past the header")
		}
		hdr = hdr[k:]
		sz := SerialSize(s)
		if len(serials) >= maxCols {
			return nil, 0, 0, recordInvalid("more than %d columns", maxCols)
		}
		if sz > math.MaxInt64-bodyLen {
			return nil, 0, 0, recordInvalid("the declared body length overflows")
		}
		bodyLen += sz
		serials = append(serials, s)
	}
	return serials, headerLen, bodyLen, nil
}

// readSigned decodes a big-endian two's complement integer of len(b) <= 8 bytes.
func readSigned(b []byte) int64 {
	v := int64(int8(b[0]))
	for _, c := range b[1:] {
		v = v<<8 | int64(c)
	}
	return v
}

// scalar fills v for the serial types that have no variable-length part and
// returns true; text and blob (serial >= 12) return false. raw holds
// SerialSize(serial) bytes.
func scalar(v *Value, serial uint64, raw []byte) bool {
	switch {
	case serial == 0, serial == 10, serial == 11:
		v.Kind = KindNull
	case serial == 8:
		v.Kind, v.Int = KindInt, 0
	case serial == 9:
		v.Kind, v.Int = KindInt, 1
	case serial >= 1 && serial <= 6:
		v.Kind, v.Int = KindInt, readSigned(raw)
	case serial == 7:
		f := math.Float64frombits(binary.BigEndian.Uint64(raw))
		if f != f { // NaN reads as NULL, as the engine does
			v.Kind = KindNull
		} else {
			v.Kind, v.Float = KindFloat, f
		}
	default:
		return false
	}
	return true
}

// blank returns an unmaterialized value of the given serial type.
func blank(serial uint64, enc Encoding) Value {
	v := Value{Serial: serial, Omitted: true}
	switch {
	case serial == 0, serial == 10, serial == 11:
		v.Kind = KindNull
	case serial >= 1 && serial <= 6, serial == 8, serial == 9:
		v.Kind = KindInt
	case serial == 7:
		v.Kind = KindFloat
	case serial >= 12 && serial%2 == 0:
		v.Kind, v.Len = KindBlob, SerialSize(serial)
	case serial >= 13:
		v.Kind, v.Len, v.Enc = KindText, SerialSize(serial), enc
	}
	return v
}

func normEnc(e Encoding) Encoding {
	if e == EncUTF16LE || e == EncUTF16BE {
		return e
	}
	return EncUTF8
}

// rowAcct applies the caps to the text and blob values of one row.
type rowAcct struct {
	lim       Limits
	recovered bool
	rowBytes  int64
	rowFull   bool  // MaxRowBytes was reached: every later text and blob is omitted
	capped    int   // live values omitted over a cap or the row cap
	firstCol  int   // column of the first of them
	firstLen  int64 // and its length
}

// admit decides what to materialize of a text or blob of n bytes in column
// col: take is the number of bytes to keep; omit says keep none; clip says
// take is a prefix (recovered values only).
func (a *rowAcct) admit(blob bool, n int64, col int) (take int64, omit, clip bool) {
	take = n
	switch {
	case a.recovered && n > a.lim.MaxRecoveredValueBytes:
		take, clip = a.lim.MaxRecoveredValueBytes, true
	case !a.recovered && !blob && n > a.lim.MaxTextBytes, !a.recovered && blob && n > a.lim.MaxBlobBytes:
		take, omit = 0, true
	}
	if !omit && (a.rowFull || take > a.lim.MaxRowBytes-a.rowBytes) {
		a.rowFull = true
		take, omit, clip = 0, true, false
	}
	if omit {
		if a.capped == 0 {
			a.firstCol, a.firstLen = col, n
		}
		a.capped++
		return 0, true, false
	}
	a.rowBytes += take
	return take, false, clip
}

// DecodeRecord decodes the record in b, the whole payload. Text and blob
// values alias b (copy them to keep them past b). A value that lies past the
// end of b is Omitted and Truncated is set; bytes past the declared body are
// tolerated. Values over the live caps of lim are Omitted with their true
// Len. The values of one row are bounded by lim.MaxRowBytes.
func DecodeRecord(b []byte, enc Encoding, lim Limits) (_ Record, err error) {
	defer guard(&err)
	pureAt("DecodeRecord")
	return decodeRecord(b, enc, lim.withDefaults(), false)
}

// decodeRecord is DecodeRecord; with recovered set, over-cap values are
// clipped to MaxRecoveredValueBytes (Clipped) instead of omitted.
func decodeRecord(b []byte, enc Encoding, lim Limits, recovered bool) (Record, error) {
	serials, hl, body, err := ParseRecordHeader(b, lim.MaxColumns)
	if err != nil {
		return Record{}, err
	}
	enc = normEnc(enc)
	rec := Record{Serials: serials, HeaderLen: hl, BodyLen: body, Values: make([]Value, len(serials))}
	acct := rowAcct{lim: lim, recovered: recovered}
	pos := int64(hl)
	for i, s := range serials {
		sz := SerialSize(s)
		if s == 10 || s == 11 {
			rec.Reserved++
		}
		v := blank(s, enc)
		if pos > int64(len(b)) || sz > int64(len(b))-pos { // not wholly inside b
			rec.Truncated = true
			rec.Values[i] = v
			pos = math.MaxInt64 // every later value is outside too
			continue
		}
		raw := b[pos : pos+sz : pos+sz]
		pos += sz
		v.Omitted = false
		if scalar(&v, s, raw) {
			rec.Values[i] = v
			continue
		}
		take, omit, clip := acct.admit(v.Kind == KindBlob, sz, i)
		if omit {
			v.Omitted = true
		} else {
			v.Bytes, v.Clipped = raw[:take:take], clip
		}
		rec.Values[i] = v
	}
	return rec, nil
}
