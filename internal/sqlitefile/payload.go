package sqlitefile

import (
	"context"
	"encoding/binary"
	"fmt"
)

// stepCost is what one recorded chain step costs in the budget.
const stepCost = 64

// chainStep is one overflow page a payload followed: its number and where
// its image lies (the provenance of the bytes it supplied).
type chainStep struct {
	Page uint32
	At   PageLoc
}

// payload is the payload of one cell: the local part held in the page, and
// the overflow chain walked lazily, only as far as a read needs. Nothing
// proportional to the declared payload length is ever allocated. Reads are
// forward-friendly (one page is remembered) but may go back: a page already
// followed is read again from its recorded number, never re-marked.
//
// The chain walk is iterative. Every page of the chain is range-checked and
// marked in the visitor before it is read, so a cycle, a page shared with
// another cell or an out-of-range pointer ends the chain: the bytes behind it
// are unreadable (short reads, never zeros) and damage records why.
type payload struct {
	src    pageSource
	l      *ledger
	vis    visitor
	ctx    context.Context // polled once per followed page; nil: never
	usable int
	local  []byte
	total  int64 // declared payload length
	head   uint32
	// maxSteps is the longest chain followed (the one overflow cap of the
	// package, overflowPageCap); 0 means no cap (only the visited set bounds it).
	maxSteps int64

	next  uint32 // page the chain continues at (valid while !dead)
	steps []chainStep
	dead  bool   // the chain is proven damaged past len(steps)
	why   string // the reason, once dead
	pgno  uint32 // the page where it died (0 when it ended with no page)

	win    []byte // content of the page of step winIdx
	winIdx int64  // -1: none
}

// newPayload prepares the payload of cell c, whose overflow chain (if any)
// is read from src and recorded in vis. Memory held for the chain's steps is
// charged to l; release gives it back.
func newPayload(src pageSource, l *ledger, vis visitor, usable int, maxSteps int64, c Cell) *payload {
	return &payload{
		src: src, l: l, vis: vis, usable: usable, maxSteps: maxSteps,
		local: c.LocalBytes, total: c.PayloadLen, head: c.OverflowHead, next: c.OverflowHead,
		winIdx: -1,
	}
}

// chunk is the payload bytes one overflow page holds.
func (p *payload) chunk() int64 { return int64(p.usable - 4) }

// provenance returns the overflow pages followed so far, in chain order.
func (p *payload) provenance() []chainStep { return append([]chainStep(nil), p.steps...) }

// damaged reports whether some read hit bytes that cannot be had, and why.
func (p *payload) damaged() (reason string, page uint32, ok bool) { return p.why, p.pgno, p.dead }

// release gives back the memory charged for the recorded steps.
func (p *payload) release() {
	p.l.free(int64(len(p.steps)) * stepCost)
	p.steps = nil
	p.win, p.winIdx = nil, -1
}

func (p *payload) die(page uint32, format string, a ...any) {
	p.dead, p.pgno, p.why = true, page, fmt.Sprintf(format, a...)
}

// readAt copies up to len(dst) payload bytes starting at off into dst and
// returns how many. n < len(dst) means the payload ends there: either off+len
// reaches the declared length, or the bytes are unreadable because the chain
// is damaged (damaged says why). err is only for an I/O error, a refused
// budget charge or a cancelled context, never for damage.
func (p *payload) readAt(dst []byte, off int64) (n int, err error) {
	if off < 0 || off >= p.total || len(dst) == 0 {
		return 0, nil
	}
	want := int64(len(dst))
	if p.total-off < want {
		want = p.total - off
	}
	localLen := int64(len(p.local))
	if off < localLen {
		c := copy(dst[:want], p.local[off:])
		n += c
		off += int64(c)
	}
	chunk := p.chunk()
	if chunk <= 0 { // a usable size the header check never lets through
		p.die(0, "usable size %d leaves no room for overflow content", p.usable)
		return n, nil
	}
	for int64(n) < want {
		idx := (off - localLen) / chunk
		within := (off - localLen) % chunk
		content, err := p.page(idx)
		if err != nil {
			return n, err
		}
		if content == nil { // damaged: p.dead says why
			return n, nil
		}
		if int64(len(content)) > chunk {
			content = content[:chunk]
		}
		if within >= int64(len(content)) {
			p.die(p.steps[idx].Page, "overflow page %d holds %d of its %d content bytes", p.steps[idx].Page, len(content), chunk)
			return n, nil
		}
		c := copy(dst[n:want], content[within:])
		n += c
		off += int64(c)
	}
	return n, nil
}

// page returns the content bytes (after the 4-byte next pointer) of chain
// step idx, walking the chain to it as needed. A nil result with a nil error
// means the chain is damaged before idx.
func (p *payload) page(idx int64) ([]byte, error) {
	if idx == p.winIdx {
		return p.win, nil
	}
	for int64(len(p.steps)) <= idx {
		if p.dead {
			return nil, nil
		}
		if p.ctx != nil {
			if err := p.ctx.Err(); err != nil {
				return nil, err
			}
		}
		cur := p.next
		switch {
		case p.maxSteps > 0 && int64(len(p.steps)) >= p.maxSteps:
			p.die(cur, "the chain is longer than the %d overflow pages any payload can need", p.maxSteps)
			return nil, nil
		case cur == 0:
			p.die(0, "the chain ends after %d overflow pages but the payload needs more", len(p.steps))
			return nil, nil
		case !p.src.has(cur):
			p.die(cur, "overflow page %d is not in the file", cur)
			return nil, nil
		case !p.vis.mark(cur):
			if mv, ok := p.vis.(*mapVisitor); ok && mv.full {
				p.die(cur, "overflow page %d not followed: the visited-set capacity of %d pages is reached", cur, mv.max)
				return nil, nil
			}
			p.die(cur, "overflow page %d was already met (a cycle or a page shared with another structure)", cur)
			return nil, nil
		}
		data, loc, err := p.src.read(cur)
		if err != nil {
			if isUnavailable(err) {
				p.die(cur, "overflow page %d is unavailable", cur)
				return nil, nil
			}
			return nil, err
		}
		if len(data) < 4 {
			p.die(cur, "overflow page %d holds %d bytes, too few for its next pointer", cur, len(data))
			return nil, nil
		}
		if err := p.l.alloc(stepCost); err != nil {
			return nil, err
		}
		p.steps = append(p.steps, chainStep{Page: cur, At: loc})
		p.next = binary.BigEndian.Uint32(data)
		p.win, p.winIdx = data[4:], int64(len(p.steps)-1)
	}
	if idx == p.winIdx {
		return p.win, nil
	}
	data, _, err := p.src.read(p.steps[idx].Page)
	if err != nil {
		if isUnavailable(err) {
			p.die(p.steps[idx].Page, "overflow page %d is unavailable", p.steps[idx].Page)
			return nil, nil
		}
		return nil, err
	}
	if len(data) < 4 {
		p.die(p.steps[idx].Page, "overflow page %d holds %d bytes, too few for its next pointer", p.steps[idx].Page, len(data))
		return nil, nil
	}
	p.win, p.winIdx = data[4:], idx
	return p.win, nil
}
