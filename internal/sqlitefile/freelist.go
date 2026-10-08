package sqlitefile

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
)

// Freelist is the list of free pages as the trunk chain gives it. Trunks and
// Leaves are CLAIMED free: each passed the range, lock-byte, pointer-map and
// repeat checks, but none is proven against the b-trees. A page that a tree also
// owns is found by Layout, which is the authority for double claims.
type Freelist struct {
	Trunks, Leaves []uint32 // in chain order; only pages that passed every check
	HeaderCount    uint32   // the count the header states
	Walked         uint32   // trunks and leaves found (the list that wins over the header)
	Anomalies      []string // one short sentence per problem met (at most maxAnomalies)
}

// maxAnomalies bounds Freelist.Anomalies; the warnings carry the rest.
const maxAnomalies = 100

// listCache holds the freelist and layout a view has computed and the budget
// they are charged: both belong to the view until Release.
type listCache struct {
	laymu     sync.Mutex // serialises Layout calls (it calls Freelist, which takes mu)
	mu        sync.Mutex
	fl        *Freelist
	flCharge  int64
	lay       *Layout
	layCharge int64
}

func (c *listCache) release(b Budget) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := c.flCharge + c.layCharge; n > 0 {
		b.Free(n)
	}
	c.fl, c.flCharge, c.lay, c.layCharge = nil, 0, nil, 0
}

// Freelist walks the freelist from the header's first trunk. Every trunk is
// visited once (a trunk met again is a cycle and ends the walk); a trunk lists
// at most usable/4-2 leaves (more is freelist-leaf-count and the list is cut at
// that maximum); a leaf must lie in 2..Addressable and be neither the lock-byte
// page, a pointer-map page nor a page already on the list, else it is skipped
// with a warning; a walked total that differs from the header count is
// freelist-count (the walked list wins). The result is read once per view and
// shared: callers must not modify it.
func (v *View) Freelist(ctx context.Context) (fl *Freelist, err error) {
	defer guard(&err)
	if ctx == nil {
		ctx = context.Background()
	}
	v.lists.mu.Lock()
	defer v.lists.mu.Unlock()
	if v.lists.fl != nil {
		return v.lists.fl, nil
	}
	l := v.e.newLedger()
	defer l.guard(&err)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fl, err = v.walkFreelist(ctx, l)
	if err != nil {
		return nil, err
	}
	v.lists.fl, v.lists.flCharge = fl, l.n
	return fl, nil
}

func (v *View) walkFreelist(ctx context.Context, l *ledger) (*Freelist, error) {
	info := v.info
	fl := &Freelist{HeaderCount: info.FreelistCount}
	anomaly := func(code string, pgno uint32, format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		v.warn(code, pgno, "%s", msg)
		if len(fl.Anomalies) < maxAnomalies {
			fl.Anomalies = append(fl.Anomalies, msg)
		}
	}
	seen, err := newPageSet(l, v.addr)
	if err != nil {
		return nil, err
	}
	defer seen.release(l)
	maxLeaves := info.UsableSize/4 - 2
	lock := LockBytePage(info.PageSize)
	// usable says why page pg cannot be a freelist page ("" when it can).
	unusable := func(pg uint32) string {
		switch {
		case pg < 2 || pg > v.addr:
			return fmt.Sprintf("page %d is outside 2..%d", pg, v.addr)
		case pg == lock:
			return fmt.Sprintf("page %d is the lock-byte page", pg)
		case info.AutoVacuum != AVNone && isPtrmapPage(info.PageSize, info.Reserved, pg):
			return fmt.Sprintf("page %d is a pointer-map page", pg)
		}
		return ""
	}
	cur := info.FreelistTrunk
	for cur != 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if why := unusable(cur); why != "" {
			anomaly(WarnPageRange, cur, "freelist trunk: %s; the walk ends", why)
			break
		}
		if !seen.mark(cur) {
			anomaly(WarnFreelistCycle, cur, "freelist trunk %d was already met (a cycle or a page listed twice); the walk ends", cur)
			break
		}
		v.e.at("freelist.trunk")
		data, _, err := v.cache.read(cur)
		if err != nil {
			if isUnavailable(err) {
				anomaly(WarnPageUnavailable, cur, "freelist trunk %d cannot be read; the walk ends", cur)
				break
			}
			return nil, err
		}
		if len(data) < 8 {
			anomaly(WarnPageUnavailable, cur, "freelist trunk %d holds only %d bytes; the walk ends", cur, len(data))
			break
		}
		next := binary.BigEndian.Uint32(data)
		count := int64(binary.BigEndian.Uint32(data[4:]))
		if count > int64(maxLeaves) {
			anomaly(WarnFreelistLeafCount, cur, "freelist trunk %d states %d leaves; a page holds at most %d, the list is cut there", cur, count, maxLeaves)
			count = int64(maxLeaves)
		}
		if err := l.alloc(4); err != nil {
			return nil, err
		}
		fl.Trunks = append(fl.Trunks, cur)
		for k := range int(count) {
			if 8+4*k+4 > len(data) {
				anomaly(WarnPageUnavailable, cur, "freelist trunk %d: the leaf slots past byte %d are not in the file", cur, len(data))
				break
			}
			lp := binary.BigEndian.Uint32(data[8+4*k:])
			if why := unusable(lp); why != "" {
				anomaly(WarnPageRange, cur, "freelist trunk %d, leaf %d: %s; skipped", cur, k, why)
				continue
			}
			if !seen.mark(lp) {
				anomaly(WarnFreelistCycle, cur, "freelist trunk %d, leaf %d: page %d is already on the list; skipped", cur, k, lp)
				continue
			}
			if err := l.alloc(4); err != nil { // charged per leaf actually kept
				return nil, err
			}
			fl.Leaves = append(fl.Leaves, lp)
		}
		cur = next
	}
	fl.Walked = uint32(len(fl.Trunks) + len(fl.Leaves))
	if fl.Walked != fl.HeaderCount {
		anomaly(WarnFreelistCount, 0, "the header counts %d free pages, the list walked holds %d", fl.HeaderCount, fl.Walked)
	}
	return fl, nil
}
