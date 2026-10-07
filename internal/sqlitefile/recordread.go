package sqlitefile

import (
	"fmt"
	"math"
)

// valueCost is what one column of a decoded record costs in the budget: its
// serial type and its Value.
const valueCost = 80

// cellCtx says where warnings about a cell are attributed.
type cellCtx struct {
	File   FileKind
	Page   uint32
	Offset int64
}

// readRecord decodes the record in p without assembling the payload: the
// header is read first, then only the columns want accepts (nil: all) are
// materialized, each by reading exactly its bytes. Memory is charged to l
// before it is allocated; the returned held is what the result keeps charged
// (the caller gives it back with l.free when it drops the record). Values
// over the live caps, or past MaxRowBytes, are Omitted with their true Len
// and counted in one cell-too-large warning on w (nil: none); a chain that
// ends early or a payload shorter than its record declares leaves the values
// beyond it Omitted with Truncated set, and a cell-overflow-chain warning.
// A payload whose declared length exceeds MaxPayloadBytes is refused with
// ErrLimit; an invalid header is an error wrapping ErrCorrupt. The caller
// holds the ledger's guard.
func (e *env) readRecord(l *ledger, w *warnings, at cellCtx, p *payload, enc Encoding, want func(col int) bool) (rec Record, held int64, err error) {
	return e.readRecordMode(l, w, at, p, enc, want, false)
}

// readRecordMode is readRecord; with recovered set, a text or blob over
// Limits.MaxRecoveredValueBytes is clipped to that many bytes (Clipped, true Len)
// instead of omitted, as history values are.
func (e *env) readRecordMode(l *ledger, w *warnings, at cellCtx, p *payload, enc Encoding, want func(col int) bool, recovered bool) (rec Record, held int64, err error) {
	lim := e.opts.Limits
	if p.total > lim.MaxPayloadBytes {
		return Record{}, 0, fmt.Errorf("%w: payload of %d bytes is over the %d byte cap", ErrLimit, p.total, lim.MaxPayloadBytes)
	}
	e.at("record.header")
	var first [9]byte
	n, err := p.readAt(first[:], 0)
	if err != nil {
		return Record{}, 0, err
	}
	hl, k := GetVarint(first[:n])
	maxHdr := int64(lim.MaxColumns)*9 + 9
	switch {
	case k == 0:
		return Record{}, 0, recordInvalid("the header length varint is not readable")
	case hl < uint64(k) || hl > uint64(p.total):
		return Record{}, 0, recordInvalid("header length %d is outside 1..%d", hl, p.total)
	case int64(hl) > maxHdr:
		return Record{}, 0, recordInvalid("header length %d is over the %d byte cap for %d columns", hl, maxHdr, lim.MaxColumns)
	}
	// The declared header length is not vouched for by any byte: the buffer grows
	// with the bytes that exist (readValue), never from the declaration.
	hdr, hok, err := readValue(l, p, 0, int64(hl))
	if err != nil {
		return Record{}, 0, err
	}
	if !hok {
		return Record{}, 0, recordInvalid("the %d byte header is not wholly readable", hl)
	}
	serials, headerLen, bodyLen, perr := ParseRecordHeader(hdr, lim.MaxColumns)
	l.free(int64(len(hdr)))
	if perr != nil {
		return Record{}, 0, perr
	}
	cost := int64(len(serials)) * valueCost
	if err := l.alloc(cost); err != nil {
		return Record{}, 0, err
	}
	held = cost
	enc = normEnc(enc)
	rec = Record{Serials: serials, HeaderLen: headerLen, BodyLen: bodyLen, Values: make([]Value, len(serials))}
	acct := rowAcct{lim: lim, recovered: recovered}
	firstReserved := 0
	pos := int64(headerLen)
	for i, s := range serials {
		sz := SerialSize(s)
		v := blank(s, enc)
		if s == 10 || s == 11 {
			if rec.Reserved == 0 {
				firstReserved = i
			}
			rec.Reserved++
		}
		if pos > p.total || sz > p.total-pos { // declared past the end of the payload
			rec.Truncated = true
			rec.Values[i] = v
			pos = math.MaxInt64
			continue
		}
		at0 := pos
		pos += sz
		if want != nil && !want(i) {
			rec.Values[i] = v
			continue
		}
		var buf []byte
		var clip bool
		var scalarBuf [8]byte
		if s >= 12 {
			var take int64
			var omit bool
			take, omit, clip = acct.admit(v.Kind == KindBlob, sz, i)
			if omit {
				rec.Values[i] = v
				continue
			}
			e.at("record.value")
			var ok bool
			if buf, ok, err = readValue(l, p, at0, take); err != nil {
				return Record{}, held, err
			}
			if !ok { // the bytes are not all there: omit, never pad
				rec.Truncated = true
				rec.Values[i] = v
				pos = math.MaxInt64 // every later value is unreadable too
				continue
			}
			held += int64(len(buf))
		} else {
			buf = scalarBuf[:sz]
			e.at("record.value")
			got, rerr := p.readAt(buf, at0)
			if rerr != nil {
				return Record{}, held, rerr
			}
			// A scalar is at most 8 bytes inside the declared payload; an
			// unreadable one (damaged chain) is omitted like any other value.
			if got < len(buf) {
				rec.Truncated = true
				rec.Values[i] = v
				pos = math.MaxInt64
				continue
			}
		}
		v.Omitted = false
		if !scalar(&v, s, buf) {
			v.Bytes, v.Clipped = buf[:len(buf):len(buf)], clip
		}
		rec.Values[i] = v
	}
	if w != nil {
		if acct.capped > 0 {
			w.add(Warning{
				Code: WarnCellTooLarge, File: at.File, Page: at.Page, Offset: at.Offset,
				Msg: fmt.Sprintf("%d values over the caps were omitted; first is column %d of %d bytes", acct.capped, acct.firstCol, acct.firstLen),
			})
		}
		if rec.Reserved > 0 {
			// The engine reads serial types 10 and 11 as a NULL that takes no
			// bytes (TestEngineReservedSerialTypesReadAsNull); the row is kept.
			w.add(Warning{
				Code: WarnRecordReservedSerial, File: at.File, Page: at.Page, Offset: at.Offset,
				Msg: fmt.Sprintf("reserved serial type in %d column(s), first column %d (type %d): read as NULL, as the engine reads it", rec.Reserved, firstReserved, rec.Serials[firstReserved]),
			})
		}
		if why, pg, dead := p.damaged(); dead && rec.Truncated {
			w.add(Warning{
				Code: WarnCellOverflowChain, File: at.File, Page: at.Page, Offset: at.Offset,
				Msg: fmt.Sprintf("overflow chain damaged (page %d): %s", pg, why),
			})
		}
	}
	return rec, held, nil
}

// valueProbe is how many bytes of a text or blob are read into the stack before
// anything is allocated for it; valueStep is the size of the first heap buffer.
const (
	valueProbe = 64
	valueStep  = 4096
)

// readValue reads take bytes of the payload at off into a new buffer. take is
// what the record declares, which no byte of the file vouches for, so memory
// is never sized from it directly: a probe on the stack proves the first bytes
// exist, the first buffer is at most valueStep bytes, and from there the
// buffer at most doubles, each step after the bytes of the previous one were
// read. What is allocated is therefore bounded by a small multiple of the
// bytes that really exist. The buffer's size is charged to l; ok is false (and
// nothing stays charged) when the bytes are not all there.
func readValue(l *ledger, p *payload, off, take int64) (buf []byte, ok bool, err error) {
	var probe [valueProbe]byte
	n := min(take, valueProbe)
	got, err := p.readAt(probe[:n], off)
	if err != nil || int64(got) < n {
		return nil, false, err
	}
	size := min(take, valueStep)
	if err := l.alloc(size); err != nil {
		return nil, false, err
	}
	buf = make([]byte, size)
	copy(buf, probe[:n])
	fill := func(from int64) (bool, error) {
		got, err := p.readAt(buf[from:], off+from)
		if err != nil {
			l.free(int64(len(buf)))
			return false, err
		}
		if int64(got) < int64(len(buf))-from {
			l.free(int64(len(buf)))
			return false, nil
		}
		return true, nil
	}
	if ok, err := fill(n); !ok {
		return nil, false, err
	}
	for int64(len(buf)) < take {
		have := int64(len(buf))
		next := min(take, 2*have)
		if err := l.alloc(next); err != nil {
			l.free(have)
			return nil, false, err
		}
		grown := make([]byte, next)
		copy(grown, buf)
		l.free(have)
		buf = grown
		if ok, err := fill(have); !ok {
			return nil, false, err
		}
	}
	return buf, true, nil
}
