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
	if err := l.alloc(int64(hl)); err != nil {
		return Record{}, 0, err
	}
	hdr := make([]byte, hl)
	if got, err := p.readAt(hdr, 0); err != nil {
		return Record{}, 0, err
	} else if got < len(hdr) {
		l.free(int64(hl))
		return Record{}, 0, recordInvalid("the %d byte header is not wholly readable", hl)
	}
	serials, headerLen, bodyLen, perr := ParseRecordHeader(hdr, lim.MaxColumns)
	l.free(int64(hl))
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
	acct := rowAcct{lim: lim}
	pos := int64(headerLen)
	for i, s := range serials {
		sz := SerialSize(s)
		v := blank(s, enc)
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
		var scalarBuf [8]byte
		if s >= 12 {
			take, omit, _ := acct.admit(v.Kind == KindBlob, sz, i)
			if omit {
				rec.Values[i] = v
				continue
			}
			if err := l.alloc(take); err != nil {
				return Record{}, held, err
			}
			buf = make([]byte, take)
		} else {
			buf = scalarBuf[:sz]
		}
		e.at("record.value")
		got, err := p.readAt(buf, at0)
		if err != nil {
			if s >= 12 {
				l.free(int64(len(buf)))
			}
			return Record{}, held, err
		}
		if got < len(buf) { // the bytes are not all there: omit, never pad
			if s >= 12 {
				l.free(int64(len(buf)))
			}
			rec.Truncated = true
			rec.Values[i] = v
			continue
		}
		v.Omitted = false
		if !scalar(&v, s, buf) {
			v.Bytes = buf[:len(buf):len(buf)]
			held += int64(len(buf))
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
		if why, pg, dead := p.damaged(); dead && rec.Truncated {
			w.add(Warning{
				Code: WarnCellOverflowChain, File: at.File, Page: at.Page, Offset: at.Offset,
				Msg: fmt.Sprintf("overflow chain damaged (page %d): %s", pg, why),
			})
		}
	}
	return rec, held, nil
}
