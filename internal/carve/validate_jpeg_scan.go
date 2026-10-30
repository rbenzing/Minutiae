package carve

import (
	"errors"
	"math/bits"
)

// The entropy-coded data of every JPEG scan is walked with the frame's Huffman tables (spec 3.3: validated means
// the whole object was walked). The walk decodes only code lengths and counts: it produces no pixel and no
// coefficient value. It requires the scan's block count to be met exactly, every padding bit to be 1, and the
// restart markers to come in order. The one piece of state is the per-coefficient "nonzero" bit the progressive
// refinement scans need, one uint64 per block of a component, capped by jpegRefineCap and checked against the
// declared dimensions before it is allocated. The layout of the scans is from memory (docs/limitations.md).

// jpegRefineCap is the largest refinement state, in bytes, a walk allocates (8 bytes per block).
const jpegRefineCap = 16 << 20

// jpegReject is a rejection reason found by the walk (a window error is returned as itself).
type jpegReject string

func (e jpegReject) Error() string { return "carve: jpeg: " + string(e) }

// jpegHuff is one canonical Huffman table.
type jpegHuff struct {
	set     bool
	mincode [17]int32
	maxcode [17]int32 // -1 where no code has that length
	valptr  [17]int32
	vals    [256]byte
}

// load builds the table from the 16 counts and the symbols; false when the counts overflow the code space.
func (h *jpegHuff) load(counts *[16]byte, syms []byte) bool {
	code, k := int32(0), int32(0)
	for l := 1; l <= 16; l++ {
		n := int32(counts[l-1])
		h.valptr[l], h.mincode[l] = k, code
		code += n
		k += n
		h.maxcode[l] = -1
		if n > 0 {
			h.maxcode[l] = code - 1
		}
		if code > 1<<uint(l) {
			return false
		}
		code <<= 1
	}
	copy(h.vals[:], syms)
	h.set = true
	return true
}

// jpegComp is one frame component.
type jpegComp struct {
	id, h, v   int
	bw, bh     int64 // blocks of the component alone (a non-interleaved scan walks these)
	stateStart int64 // first state word of the component
}

// jpegFrame is the geometry of the frame header and the refinement state.
type jpegFrame struct {
	progressive  bool
	nf           int
	comps        [4]jpegComp
	mcusX, mcusY int64
	stateBlocks  int64
	state        []uint64
}

// setGeometry derives the block grids from the dimensions and sampling factors (all validated by the caller).
func (f *jpegFrame) setGeometry(w, h int64) {
	hmax, vmax := 1, 1
	for i := range f.nf {
		hmax, vmax = max(hmax, f.comps[i].h), max(vmax, f.comps[i].v)
	}
	f.mcusX = (w + int64(8*hmax) - 1) / int64(8*hmax)
	f.mcusY = (h + int64(8*vmax) - 1) / int64(8*vmax)
	f.stateBlocks = 0
	for i := range f.nf {
		c := &f.comps[i]
		cw := (w*int64(c.h) + int64(hmax) - 1) / int64(hmax)
		ch := (h*int64(c.v) + int64(vmax) - 1) / int64(vmax)
		c.bw, c.bh = (cw+7)/8, (ch+7)/8
		c.stateStart = f.stateBlocks
		f.stateBlocks += c.bw * c.bh
	}
}

// jpegScan is one parsed scan header.
type jpegScan struct {
	comps          []int // indexes into the frame
	dc, ac         [4]int
	ss, se, ah, al int
}

// jpegBits reads the entropy-coded bits of a scan, one byte at a time.
type jpegBits struct {
	w   *Window
	pos int64
	acc uint32
	n   uint
}

// next loads the next data byte. A marker (FF followed by anything but 00 or FF) is data that ended too soon;
// FF FF is fill.
func (b *jpegBits) next() error {
	for {
		c, err := b.w.byteChecked(b.pos)
		if err != nil {
			return err
		}
		if c != 0xFF {
			b.pos++
			b.acc, b.n = uint32(c), 8
			return nil
		}
		d, err := b.w.byteChecked(b.pos + 1)
		if err != nil {
			return err
		}
		switch d {
		case 0x00:
			b.pos += 2
			b.acc, b.n = 0xFF, 8
			return nil
		case 0xFF:
			b.pos++
		default:
			return jpegReject("scan-short")
		}
	}
}

func (b *jpegBits) bit() (int32, error) {
	if b.n == 0 {
		if err := b.next(); err != nil {
			return 0, err
		}
	}
	b.n--
	return int32(b.acc >> b.n & 1), nil
}

// skip reads and drops k bits.
func (b *jpegBits) skip(k int) error {
	for range k {
		if _, err := b.bit(); err != nil {
			return err
		}
	}
	return nil
}

// value reads k bits as a number (k <= 16).
func (b *jpegBits) value(k int) (int, error) {
	v := 0
	for range k {
		x, err := b.bit()
		if err != nil {
			return 0, err
		}
		v = v<<1 | int(x)
	}
	return v, nil
}

// decode reads one Huffman symbol.
func (b *jpegBits) decode(h *jpegHuff) (byte, error) {
	code := int32(0)
	for l := 1; l <= 16; l++ {
		x, err := b.bit()
		if err != nil {
			return 0, err
		}
		code = code<<1 | x
		if code <= h.maxcode[l] {
			return h.vals[h.valptr[l]+code-h.mincode[l]], nil
		}
	}
	return 0, jpegReject("bad-code")
}

// align ends a byte-aligned unit: the rest of the current byte must be 1 bits.
func (b *jpegBits) align() error {
	if b.n > 0 && b.acc&(1<<b.n-1) != 1<<b.n-1 {
		return jpegReject("bad-padding")
	}
	b.n = 0
	return nil
}

// restart consumes the RSTn marker numbered want (FF fill may precede it).
func (b *jpegBits) restart(want int) error {
	for {
		c, err := b.w.byteChecked(b.pos)
		if err != nil {
			return err
		}
		if c != 0xFF {
			return jpegReject("restart-order")
		}
		d, err := b.w.byteChecked(b.pos + 1)
		if err != nil {
			return err
		}
		switch {
		case d == 0xFF:
			b.pos++
		case d == 0xD0+byte(want&7):
			b.pos += 2
			return nil
		default:
			return jpegReject("restart-order")
		}
	}
}

// end checks that a marker (not data and not a restart) follows the scan and leaves pos at its FF.
func (b *jpegBits) end() error {
	for {
		c, err := b.w.byteChecked(b.pos)
		if err != nil {
			return err
		}
		if c != 0xFF {
			return jpegReject("scan-overrun")
		}
		d, err := b.w.byteChecked(b.pos + 1)
		if err != nil {
			return err
		}
		switch {
		case d == 0xFF:
			b.pos++
		case d == 0x00 || (d >= 0xD0 && d <= 0xD7):
			return jpegReject("scan-overrun")
		default:
			return nil
		}
	}
}

// walkScan walks the entropy-coded data of one scan starting at pos and returns the position of the FF of the
// marker that ends it.
func (f *jpegFrame) walkScan(w *Window, tabs *[2][4]jpegHuff, sc *jpegScan, ri, pos int64) (int64, error) {
	br := &jpegBits{w: w, pos: pos}
	single := len(sc.comps) == 1
	total := f.mcusX * f.mcusY
	if single {
		c := &f.comps[sc.comps[0]]
		total = c.bw * c.bh
	}
	acScan := f.progressive && sc.ss > 0
	if acScan && f.state == nil {
		f.state = make([]uint64, f.stateBlocks)
	}
	var band uint64
	if acScan {
		band = (^uint64(0) >> uint(63-sc.se)) & (^uint64(0) << uint(sc.ss))
	}
	var eobrun int64
	rst := 0
	for m := int64(0); m < total; {
		interval := total - m
		if ri > 0 {
			if m > 0 && m%ri == 0 {
				if eobrun != 0 {
					return 0, jpegReject("eobrun-overrun")
				}
				if err := br.align(); err != nil {
					return 0, err
				}
				if err := br.restart(rst); err != nil {
					return 0, err
				}
				rst++
			}
			interval = min(interval, ri-m%ri)
		}
		if eobrun > 0 {
			if eobrun > interval {
				return 0, jpegReject("eobrun-overrun")
			}
			if sc.ah != 0 {
				c := &f.comps[sc.comps[0]]
				for i := range eobrun {
					if err := br.skip(bits.OnesCount64(f.state[c.stateStart+m+i] & band)); err != nil {
						return 0, err
					}
				}
			}
			m += eobrun
			eobrun = 0
			continue
		}
		var err error
		switch {
		case acScan && sc.ah == 0:
			eobrun, err = f.acFirst(br, tabs, sc, m)
		case acScan:
			eobrun, err = f.acRefine(br, tabs, sc, m, band)
		default:
			err = f.dcOrSequential(br, tabs, sc)
		}
		if err != nil {
			return 0, err
		}
		m++
	}
	if eobrun != 0 {
		return 0, jpegReject("eobrun-overrun")
	}
	if err := br.align(); err != nil {
		return 0, err
	}
	if err := br.end(); err != nil {
		return 0, err
	}
	return br.pos, nil
}

// dcOrSequential walks one MCU of a baseline scan or of a progressive DC scan.
func (f *jpegFrame) dcOrSequential(br *jpegBits, tabs *[2][4]jpegHuff, sc *jpegScan) error {
	for ci, fi := range sc.comps {
		c := &f.comps[fi]
		blocks := 1
		if len(sc.comps) > 1 {
			blocks = c.h * c.v
		}
		for range blocks {
			switch {
			case f.progressive && sc.ah != 0: // DC refinement: one bit per block
				if err := br.skip(1); err != nil {
					return err
				}
			default:
				s, err := br.decode(&tabs[0][sc.dc[ci]])
				if err != nil {
					return err
				}
				if s > 15 {
					return jpegReject("bad-code")
				}
				if err := br.skip(int(s)); err != nil {
					return err
				}
				if f.progressive {
					continue
				}
				if err := f.sequentialAC(br, &tabs[1][sc.ac[ci]]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// sequentialAC walks coefficients 1..63 of a baseline block.
func (f *jpegFrame) sequentialAC(br *jpegBits, ac *jpegHuff) error {
	for k := 1; k < 64; {
		rs, err := br.decode(ac)
		if err != nil {
			return err
		}
		r, s := int(rs>>4), int(rs&15)
		if s == 0 {
			switch r {
			case 0:
				return nil
			case 15:
				if k+16 > 64 {
					return jpegReject("run-past-end")
				}
				k += 16
				continue
			}
			return jpegReject("bad-code")
		}
		k += r
		if k > 63 {
			return jpegReject("run-past-end")
		}
		if err := br.skip(s); err != nil {
			return err
		}
		k++
	}
	return nil
}

// acFirst walks one block of a progressive AC first scan and returns the blocks an EOB run still covers.
func (f *jpegFrame) acFirst(br *jpegBits, tabs *[2][4]jpegHuff, sc *jpegScan, m int64) (int64, error) {
	c := &f.comps[sc.comps[0]]
	st := f.state[c.stateStart+m:]
	ac := &tabs[1][sc.ac[0]]
	for k := sc.ss; k <= sc.se; {
		rs, err := br.decode(ac)
		if err != nil {
			return 0, err
		}
		r, s := int(rs>>4), int(rs&15)
		if s == 0 {
			if r == 15 {
				if k+16 > sc.se+1 {
					return 0, jpegReject("run-past-end")
				}
				k += 16
				continue
			}
			n := int64(1) << uint(r)
			if r > 0 {
				extra, err := br.value(r)
				if err != nil {
					return 0, err
				}
				n += int64(extra)
			}
			return n - 1, nil
		}
		k += r
		if k > sc.se {
			return 0, jpegReject("run-past-end")
		}
		if err := br.skip(s); err != nil {
			return 0, err
		}
		st[0] |= 1 << uint(k)
		k++
	}
	return 0, nil
}

// acRefine walks one block of a progressive AC refinement scan and returns the blocks an EOB run still covers.
func (f *jpegFrame) acRefine(br *jpegBits, tabs *[2][4]jpegHuff, sc *jpegScan, m int64, band uint64) (int64, error) {
	c := &f.comps[sc.comps[0]]
	st := f.state[c.stateStart+m:]
	ac := &tabs[1][sc.ac[0]]
	k := sc.ss
	var run int64 = -1 // -1: no EOB run begun in this block
	for k <= sc.se {
		rs, err := br.decode(ac)
		if err != nil {
			return 0, err
		}
		r, s := int(rs>>4), int(rs&15)
		if s != 0 {
			if s != 1 {
				return 0, jpegReject("bad-refinement")
			}
			if err := br.skip(1); err != nil { // the sign of the newly nonzero coefficient
				return 0, err
			}
		} else if r != 15 {
			run = int64(1) << uint(r)
			if r > 0 {
				extra, err := br.value(r)
				if err != nil {
					return 0, err
				}
				run += int64(extra)
			}
			break
		}
		// Skip r zero coefficients (16 for a ZRL), reading a correction bit for every nonzero one passed.
		for ; k <= sc.se; k++ {
			if st[0]>>uint(k)&1 != 0 {
				if err := br.skip(1); err != nil {
					return 0, err
				}
				continue
			}
			if r--; r < 0 {
				break
			}
		}
		if k > sc.se {
			return 0, jpegReject("run-past-end")
		}
		if s != 0 {
			st[0] |= 1 << uint(k)
		}
		k++
	}
	if run < 0 {
		return 0, nil
	}
	// The EOB run ends this block: the rest of its band gets correction bits.
	if err := br.skip(bits.OnesCount64(st[0] & band & (^uint64(0) << uint(k)))); err != nil {
		return 0, err
	}
	return run - 1, nil
}

// isJPEGReject reports whether err is a walk rejection (and its reason) rather than a window refusal.
func isJPEGReject(err error) (string, bool) {
	var r jpegReject
	if errors.As(err, &r) {
		return string(r), true
	}
	return "", false
}
