package sqlitefile_test

// The fixture generator's own walker (plan 3I, Task 14): a minimal reader of
// SQLite pages, cells, records, WAL files and rollback journals written for
// the oracle. It imports nothing of internal/sqlitefile and is written from
// the file format description, not from the library, so a fixture oracle built
// with it cannot be circular. It is deliberately small and strict: it is used
// only on files the generator wrote itself.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf16"
)

type fxFiles struct{ db, wal, journal []byte }

func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func be16(b []byte) int    { return int(binary.BigEndian.Uint16(b)) }

// fxVarint reads a varint; it returns the value and the length.
func fxVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < 8 && i < len(b); i++ {
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	if len(b) >= 9 {
		return v<<8 | uint64(b[8]), 9
	}
	return 0, 0
}

// ---- WAL ----

type fxWalFrame struct {
	slot           int // 1-based
	page, dbsize   uint32
	salt1, salt2   uint32
	check1, check2 uint32
	state          string
	linked         bool
	gen            int
	off            int64 // of the frame header
}

type fxWalFile struct {
	headerValid bool
	big         bool
	ps          int
	ckpt        uint32
	salt1       uint32
	salt2       uint32
	frames      []fxWalFrame
	trailing    int64
	gens        []fxGeneration
	validLen    int
	lastCommit  int // slot, 0 none
}

func fxWalSum(data []byte, big bool, s0, s1 uint32) (uint32, uint32) {
	for i := 0; i+8 <= len(data); i += 8 {
		var x0, x1 uint32
		if big {
			x0, x1 = be32(data[i:]), be32(data[i+4:])
		} else {
			x0, x1 = binary.LittleEndian.Uint32(data[i:]), binary.LittleEndian.Uint32(data[i+4:])
		}
		s0 += x0 + s1
		s1 += x1 + s0
	}
	return s0, s1
}

// fxScanWAL reads a WAL file by the format description.
func fxScanWAL(wal []byte) *fxWalFile {
	w := &fxWalFile{}
	if len(wal) < 32 {
		return w
	}
	magic := be32(wal)
	w.big = magic == 0x377f0683
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return w
	}
	w.ps = int(be32(wal[8:]))
	w.ckpt, w.salt1, w.salt2 = be32(wal[12:]), be32(wal[16:]), be32(wal[20:])
	c0, c1 := fxWalSum(wal[:24], w.big, 0, 0)
	if c0 != be32(wal[24:]) || c1 != be32(wal[28:]) || be32(wal[4:]) != 3007000 || w.ps < 512 || w.ps > 65536 || w.ps&(w.ps-1) != 0 {
		return w
	}
	w.headerValid = true
	slot := 24 + w.ps
	n := (len(wal) - 32) / slot
	w.trailing = int64(len(wal) - 32 - n*slot)
	prev0, prev1 := be32(wal[24:]), be32(wal[28:]) // the header's stored checksum
	chain := true
	broken := false
	for i := 0; i < n; i++ {
		off := 32 + i*slot
		f := wal[off : off+slot]
		fr := fxWalFrame{slot: i + 1, page: be32(f), dbsize: be32(f[4:]), salt1: be32(f[8:]), salt2: be32(f[12:]), check1: be32(f[16:]), check2: be32(f[20:]), off: int64(off)}
		a, b := fxWalSum(f[:8], w.big, prev0, prev1)
		a, b = fxWalSum(f[24:], w.big, a, b)
		fr.linked = a == fr.check1 && b == fr.check2 && fr.page != 0
		sameSalt := fr.salt1 == w.salt1 && fr.salt2 == w.salt2
		switch {
		case chain && sameSalt && fr.linked:
			fr.state = "valid"
		case chain:
			chain = false
			if sameSalt {
				fr.state = "broken"
			} else {
				fr.state = "stale"
			}
			broken = true
		case !sameSalt:
			fr.state = "stale"
		default:
			if broken {
				fr.state = "detached"
			}
		}
		if fr.state == "" {
			fr.state = "detached"
		}
		prev0, prev1 = fr.check1, fr.check2
		w.frames = append(w.frames, fr)
	}
	for i := range w.frames {
		if w.frames[i].state == "valid" {
			w.validLen++
			if w.frames[i].dbsize != 0 {
				w.lastCommit = w.frames[i].slot
			}
		}
	}
	for i := range w.frames {
		if w.frames[i].state == "valid" {
			if w.frames[i].slot <= w.lastCommit {
				w.frames[i].state = "committed"
			} else {
				w.frames[i].state = "uncommitted"
			}
		}
	}
	// "linked" for the oracle is continuity from the previous slot's STORED
	// checksum (the header's for slot 1); recompute it independently of the
	// chain state used above.
	p0, p1 := be32(wal[24:]), be32(wal[28:])
	for i := range w.frames {
		off := 32 + i*slot
		f := wal[off : off+slot]
		a, b := fxWalSum(f[:8], w.big, p0, p1)
		a, b = fxWalSum(f[24:], w.big, a, b)
		w.frames[i].linked = a == w.frames[i].check1 && b == w.frames[i].check2
		p0, p1 = w.frames[i].check1, w.frames[i].check2
	}
	// generations: runs of equal salts, each slot after the first linked.
	for i := range w.frames {
		fr := &w.frames[i]
		if i > 0 {
			pf := &w.frames[i-1]
			if pf.salt1 == fr.salt1 && pf.salt2 == fr.salt2 && fr.linked {
				fr.gen = pf.gen
				g := &w.gens[fr.gen]
				g.Slots++
				if fr.dbsize != 0 {
					g.Commits++
				}
				continue
			}
		}
		g := fxGeneration{Salt1: fr.salt1, Salt2: fr.salt2, FirstSlot: uint32(fr.slot), Slots: 1}
		if fr.dbsize != 0 {
			g.Commits = 1
		}
		fr.gen = len(w.gens)
		w.gens = append(w.gens, g)
	}
	for i := range w.gens {
		g := &w.gens[i]
		g.Age = w.salt1 - g.Salt1
		g.Anchored = g.Salt1 == w.salt1 && g.Salt2 == w.salt2 && g.FirstSlot == 1 && w.frames[g.FirstSlot-1].linkedToHeader(wal, w)
	}
	return w
}

// linkedToHeader: the frame verifies from the header's stored checksum.
func (f fxWalFrame) linkedToHeader(wal []byte, w *fxWalFile) bool {
	slot := 24 + w.ps
	off := 32 + (f.slot-1)*slot
	fr := wal[off : off+slot]
	a, b := fxWalSum(fr[:8], w.big, be32(wal[24:]), be32(wal[28:]))
	a, b = fxWalSum(fr[24:], w.big, a, b)
	return a == f.check1 && b == f.check2
}

// expect renders the WAL as the oracle's description.
func (w *fxWalFile) expect() *fxWAL {
	e := &fxWAL{HeaderValid: w.headerValid, BigEndian: w.big, Frames: []fxFrame{}, Generations: []fxGeneration{}}
	if !w.headerValid {
		return e
	}
	e.PageSize, e.CheckpointSeq, e.Salt1, e.Salt2 = uint32(w.ps), w.ckpt, w.salt1, w.salt2
	e.FrameSlots = uint32(len(w.frames))
	e.TrailingBytes = w.trailing
	e.FramesValid = uint32(w.validLen)
	e.LastCommit = uint32(w.lastCommit)
	for _, f := range w.frames {
		switch f.state {
		case "committed":
			e.FramesCommitted++
			if f.dbsize != 0 {
				e.Commits++
			}
			e.MaxPageNumber = max(e.MaxPageNumber, f.page)
		case "uncommitted":
			e.FramesUncommitted++
		case "broken":
			e.FramesBroken++
		case "detached":
			e.FramesDetached++
		case "stale":
			e.FramesStale++
		}
		if f.slot == w.lastCommit {
			e.DBPagesAfterCommit = f.dbsize
		}
		e.Frames = append(e.Frames, fxFrame{
			Slot: uint32(f.slot), Page: f.page, DBSize: f.dbsize, Salt1: f.salt1, Salt2: f.salt2,
			Check1: f.check1, Check2: f.check2, State: f.state, Linked: f.linked, Generation: f.gen, Offset: f.off,
		})
	}
	e.Generations = append(e.Generations, w.gens...)
	return e
}

// ---- journal ----

var fxJournalMagic = []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

type fxJrec struct {
	index, seg int
	page       uint32
	off        int64 // of the page data
	ckOK       bool
	applied    bool
}

type fxJournalFile struct {
	present, hot, headerValid, zeroed bool
	ps, sector                        uint32
	initial, nonce                    uint32
	segs                              []fxJournalSeg
	recs                              []fxJrec
	winner                            map[uint32]int
}

func fxScanJournal(j []byte, dbps int) *fxJournalFile {
	f := &fxJournalFile{present: len(j) > 0, winner: map[uint32]int{}}
	if len(j) < 512 {
		return f
	}
	if bytes.Equal(j[:28], make([]byte, 28)) {
		f.zeroed = true
		return f
	}
	if !bytes.Equal(j[:8], fxJournalMagic) {
		return f
	}
	f.headerValid = true
	f.hot = true
	f.sector, f.ps = be32(j[20:]), be32(j[24:])
	if f.ps == 0 {
		f.ps = uint32(dbps)
	}
	f.initial, f.nonce = be32(j[16:]), be32(j[12:])
	ps := int(f.ps)
	sector := int(f.sector)
	step := 4 + ps + 4
	off, seg := 0, 0
	for off+sector <= len(j) && bytes.Equal(j[off:off+8], fxJournalMagic) {
		n := be32(j[off+8:])
		nonce := be32(j[off+12:])
		p := off + sector
		cnt := uint32(0)
		start := len(f.recs)
		for (n == 0xffffffff || cnt < n) && p+step <= len(j) {
			pg := be32(j[p:])
			sum := nonce
			data := j[p+4 : p+4+ps]
			for i := ps - 200; i > 0; i -= 200 {
				sum += uint32(data[i])
			}
			f.recs = append(f.recs, fxJrec{index: len(f.recs), seg: seg, page: pg, off: int64(p + 4), ckOK: sum == be32(j[p+4+ps:])})
			p += step
			cnt++
		}
		f.segs = append(f.segs, fxJournalSeg{Offset: int64(off), DeclaredRecords: n, Records: uint32(len(f.recs) - start)})
		off = (p + sector - 1) / sector * sector
		seg++
	}
	for i := range f.recs {
		r := &f.recs[i]
		if r.ckOK && r.page != 0 {
			if prev, ok := f.winner[r.page]; ok {
				f.recs[prev].applied = false
			}
			f.winner[r.page] = i
			r.applied = true
		}
	}
	return f
}

func (f *fxJournalFile) expect() *fxJournal {
	e := &fxJournal{
		Hot: f.hot, HeaderValid: f.headerValid, ZeroedHeader: f.zeroed, PageSize: f.ps, SectorSize: f.sector,
		InitialPages: f.initial, Nonce: f.nonce, Applied: f.hot, Segments: []fxJournalSeg{}, Records: []fxJournalRec{},
	}
	switch {
	case f.zeroed:
		e.NotAppliedReason = "zeroed-header"
	case !f.hot:
		e.NotAppliedReason = "header-invalid"
	}
	e.Segments = append(e.Segments, f.segs...)
	for _, r := range f.recs {
		e.RecordsTotal++
		if r.ckOK {
			e.RecordsValid++
		}
		if r.applied {
			e.AppliedRecords++
		}
		e.Records = append(e.Records, fxJournalRec{Index: r.index, Segment: r.seg, Page: r.page, Offset: r.off, ChecksumOK: r.ckOK, Applied: r.applied})
	}
	return e
}

// ---- page source ----

// fxState resolves page numbers to bytes for one reading of the files.
type fxState struct {
	f          fxFiles
	ps         int
	wal        *fxWalFile
	jr         *fxJournalFile
	useWAL     bool
	useJournal bool
	latest     map[uint32]int // page -> slot of its newest committed frame
}

func fxNewState(f fxFiles, useWAL, useJournal bool) *fxState {
	s := &fxState{f: f}
	s.ps = be16(f.db[16:])
	if s.ps == 1 {
		s.ps = 65536
	}
	if useWAL && f.wal != nil {
		s.wal = fxScanWAL(f.wal)
		s.useWAL = s.wal.headerValid
		s.latest = map[uint32]int{}
		for _, fr := range s.wal.frames {
			if fr.state == "committed" {
				s.latest[fr.page] = fr.slot
			}
		}
	}
	if useJournal && f.journal != nil {
		s.jr = fxScanJournal(f.journal, s.ps)
		s.useJournal = s.jr.hot
	}
	return s
}

// get returns the page as the state serves it, the file it lies in and the
// file offset of its first byte.
func (s *fxState) get(n uint32) (data []byte, file string, off int64, ok bool) {
	if n == 0 {
		return nil, "", 0, false
	}
	if s.useJournal {
		if n > s.jr.initial {
			return nil, "", 0, false
		}
		if i, w := s.jr.winner[n]; w {
			r := s.jr.recs[i]
			return s.f.journal[r.off : r.off+int64(s.ps)], "journal", r.off, true
		}
	}
	if s.useWAL {
		if slot, w := s.latest[n]; w {
			fr := s.wal.frames[slot-1]
			return s.f.wal[fr.off+24 : fr.off+24+int64(s.ps)], "wal", fr.off + 24, true
		}
	}
	o := int64(n-1) * int64(s.ps)
	if o+int64(s.ps) > int64(len(s.f.db)) {
		return nil, "", 0, false
	}
	return s.f.db[o : o+int64(s.ps)], "db", o, true
}

// ---- records and cells ----

type fxCellV struct {
	page     uint32
	file     string
	off      int64 // of the cell in its file
	length   int
	rowid    int64
	hasRowid bool
	vals     []any
	hex      []byte
	overflow int // overflow pages the payload came from
}

func fxDecodeRecord(b []byte, enc int) ([]any, error) {
	hl, n := fxVarint(b)
	if n == 0 || int(hl) > len(b) {
		return nil, errors.New("bad record header")
	}
	var serials []uint64
	for p := n; p < int(hl); {
		v, k := fxVarint(b[p:])
		if k == 0 {
			return nil, errors.New("bad serial")
		}
		serials = append(serials, v)
		p += k
	}
	p := int(hl)
	out := make([]any, 0, len(serials))
	take := func(k int) ([]byte, error) {
		if p+k > len(b) {
			return nil, errors.New("record body short")
		}
		x := b[p : p+k]
		p += k
		return x, nil
	}
	for _, st := range serials {
		switch {
		case st == 0:
			out = append(out, nil)
		case st >= 1 && st <= 6, st == 8, st == 9:
			sizes := map[uint64]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8, 8: 0, 9: 0}
			x, err := take(sizes[st])
			if err != nil {
				return nil, err
			}
			switch st {
			case 8:
				out = append(out, int64(0))
			case 9:
				out = append(out, int64(1))
			default:
				var v int64
				if len(x) > 0 && x[0]&0x80 != 0 {
					v = -1
				}
				for _, c := range x {
					v = v<<8 | int64(c)
				}
				out = append(out, v)
			}
		case st == 7:
			x, err := take(8)
			if err != nil {
				return nil, err
			}
			out = append(out, math.Float64frombits(binary.BigEndian.Uint64(x)))
		case st >= 12 && st%2 == 0:
			x, err := take(int(st-12) / 2)
			if err != nil {
				return nil, err
			}
			out = append(out, append([]byte(nil), x...))
		case st >= 13:
			x, err := take(int(st-13) / 2)
			if err != nil {
				return nil, err
			}
			out = append(out, fxText(x, enc))
		default:
			return nil, fmt.Errorf("reserved serial %d", st)
		}
	}
	return out, nil
}

func fxText(b []byte, enc int) string {
	if enc == 1 {
		return string(b)
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if enc == 2 {
			u = append(u, binary.LittleEndian.Uint16(b[i:]))
		} else {
			u = append(u, binary.BigEndian.Uint16(b[i:]))
		}
	}
	return string(utf16.Decode(u))
}

// fxLocal is the local payload size by the format description.
func fxLocal(usable int, leafTable bool, p int) (local int, spills bool) {
	var x int
	if leafTable {
		x = usable - 35
	} else {
		x = (usable-12)*64/255 - 23
	}
	if p <= x {
		return p, false
	}
	m := (usable-12)*32/255 - 23
	k := m + (p-m)%(usable-4)
	if k <= x {
		return k, true
	}
	return m, true
}

func (s *fxState) usable() int { return s.ps - int(s.f.db[20]) }

func (s *fxState) enc() int { return int(be32(s.f.db[56:])) }

// fxCellAt decodes the cell at offset co of a b-tree page image (pageOff is
// the file offset of the image). typ is the page type byte.
func (s *fxState) cellAt(page []byte, pgno uint32, file string, pageOff int64, typ byte, co int) (fxCellV, error) {
	c := fxCellV{page: pgno, file: file}
	p := co
	if typ == 0x02 || typ == 0x05 {
		p += 4
	}
	var payload uint64
	switch typ {
	case 0x0d:
		v, n := fxVarint(page[p:])
		if n == 0 {
			return c, errors.New("bad cell")
		}
		payload, p = v, p+n
		r, n := fxVarint(page[p:])
		c.rowid, c.hasRowid, p = int64(r), true, p+n
	case 0x0a, 0x02:
		v, n := fxVarint(page[p:])
		if n == 0 {
			return c, errors.New("bad cell")
		}
		payload, p = v, p+n
	default:
		return c, fmt.Errorf("page type %#x", typ)
	}
	local, spills := fxLocal(s.usable(), typ == 0x0d, int(payload))
	if p+local > len(page) || (spills && p+local+4 > len(page)) {
		return c, errors.New("cell past page")
	}
	buf := append([]byte(nil), page[p:p+local]...)
	end := p + local
	if spills {
		next := be32(page[end:])
		end += 4
		for next != 0 && len(buf) < int(payload) {
			op, _, _, ok := s.get(next)
			if !ok {
				return c, errors.New("overflow page unavailable")
			}
			take := min(int(payload)-len(buf), s.usable()-4)
			buf = append(buf, op[4:4+take]...)
			next = be32(op)
			c.overflow++
		}
	}
	vals, err := fxDecodeRecord(buf, s.enc())
	if err != nil {
		return c, err
	}
	c.vals = vals
	c.off = pageOff + int64(co)
	c.length = end - co
	c.hex = append([]byte(nil), page[co:end]...)
	return c, nil
}

func fxHeaderBase(pgno uint32) int {
	if pgno == 1 {
		return 100
	}
	return 0
}

// leafCells decodes every cell of a leaf page image (not interior).
func (s *fxState) leafCells(page []byte, pgno uint32, file string, pageOff int64) ([]fxCellV, error) {
	b := fxHeaderBase(pgno)
	typ := page[b]
	if typ != 0x0d && typ != 0x0a {
		return nil, fmt.Errorf("page %d is not a leaf (%#x)", pgno, typ)
	}
	n := be16(page[b+3:])
	var out []fxCellV
	for i := 0; i < n; i++ {
		co := be16(page[b+8+2*i:])
		c, err := s.cellAt(page, pgno, file, pageOff, typ, co)
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, nil
}

// walk visits the cells of the b-tree at root in key order (index and WITHOUT
// ROWID interior cells are entries too).
func (s *fxState) walk(root uint32, visit func(fxCellV)) error {
	return s.walkAt(root, 0, visit)
}

func (s *fxState) walkAt(pgno uint32, depth int, visit func(fxCellV)) error {
	if depth > 20 {
		return errors.New("tree too deep")
	}
	page, file, off, ok := s.get(pgno)
	if !ok {
		return fmt.Errorf("page %d unavailable", pgno)
	}
	b := fxHeaderBase(pgno)
	typ := page[b]
	n := be16(page[b+3:])
	switch typ {
	case 0x0d, 0x0a:
		cells, err := s.leafCells(page, pgno, file, off)
		if err != nil {
			return err
		}
		for _, c := range cells {
			visit(c)
		}
		return nil
	case 0x05, 0x02:
		for i := 0; i < n; i++ {
			co := be16(page[b+12+2*i:])
			if err := s.walkAt(be32(page[co:]), depth+1, visit); err != nil {
				return err
			}
			if typ == 0x02 {
				c, err := s.cellAt(page, pgno, file, off, typ, co)
				if err != nil {
					return err
				}
				visit(c)
			}
		}
		return s.walkAt(be32(page[b+8:]), depth+1, visit)
	}
	return fmt.Errorf("page %d type %#x", pgno, typ)
}

// freelist walks the trunk chain from the header of the db file.
func fxFreelistOf(s *fxState) (trunks, leaves []uint32) {
	p1, _, _, ok := s.get(1)
	if !ok {
		return nil, nil
	}
	t := be32(p1[32:])
	seen := map[uint32]bool{}
	for t != 0 && !seen[t] {
		seen[t] = true
		page, _, _, ok := s.get(t)
		if !ok {
			break
		}
		trunks = append(trunks, t)
		n := int(be32(page[4:]))
		for i := 0; i < n && 8+4*i+4 <= len(page); i++ {
			leaves = append(leaves, be32(page[8+4*i:]))
		}
		t = be32(page)
	}
	return trunks, leaves
}

// schemaRows reads sqlite_schema (page 1) of the state.
type fxSchemaObj struct {
	typ, name, tbl string
	root           uint32
	sql            string
}

func (s *fxState) schemaRows() ([]fxSchemaObj, error) {
	var out []fxSchemaObj
	err := s.walk(1, func(c fxCellV) {
		if len(c.vals) < 5 {
			return
		}
		o := fxSchemaObj{}
		o.typ, _ = c.vals[0].(string)
		o.name, _ = c.vals[1].(string)
		o.tbl, _ = c.vals[2].(string)
		if r, ok := c.vals[3].(int64); ok {
			o.root = uint32(r)
		}
		o.sql, _ = c.vals[4].(string)
		out = append(out, o)
	})
	return out, err
}
