package apfstest

// dataAlloc hands out the blocks that hold file content. It starts after the
// last volume object, so the size of the volumes never depends on it.
type dataAlloc struct {
	bs     int
	next   uint64
	blocks []Block
}

func (d *dataAlloc) take(n int) uint64 {
	a := d.next
	d.next += uint64(n)
	return a
}

// put stores data (zero-padded to whole blocks) at block addr.
func (d *dataAlloc) put(addr uint64, data []byte) {
	for off := 0; off < len(data); off += d.bs {
		b := make([]byte, d.bs)
		copy(b, data[off:])
		d.blocks = append(d.blocks, Block{Addr: addr + uint64(off/d.bs), Data: b})
	}
}

type segKind int

const (
	segData segKind = iota
	segHole
	segGap
)

type segment struct {
	kind         segKind
	first, count int // logical blocks
}

// inRanges reports whether block i of size bs lies in one of the ranges.
func inRanges(rs [][2]int64, i, bs int) bool {
	off := int64(i * bs)
	for _, r := range rs {
		if r[0]%int64(bs) != 0 || r[1]%int64(bs) != 0 {
			panic("apfstest: hole or gap range is not block-aligned")
		}
		if off >= r[0] && off < r[0]+r[1] {
			return true
		}
	}
	return false
}

// layout writes the content of f into data blocks and returns the extent
// records for it (logical order) and the first data block. Explicit extents
// replace the automatic layout (their relative block numbers are resolved).
func (d *dataAlloc) layout(f File) (exts []Extent, start uint64) {
	bs := d.bs
	nb := (len(f.Data) + bs - 1) / bs
	if f.Extents != nil {
		start = d.next
		if nb > 0 {
			d.take(nb)
			d.put(start, f.Data)
		}
		for _, e := range f.Extents {
			if e.Rel {
				e.Phys += start
			}
			exts = append(exts, e)
		}
		return exts, start
	}
	if nb == 0 {
		return nil, d.next
	}
	var segs []segment
	for i := range nb {
		k := segData
		switch {
		case inRanges(f.Holes, i, bs):
			k = segHole
		case inRanges(f.Gaps, i, bs):
			k = segGap
		}
		if n := len(segs); n > 0 && segs[n-1].kind == k {
			segs[n-1].count++
		} else {
			segs = append(segs, segment{kind: k, first: i, count: 1})
		}
	}
	// Split data segments into pieces.
	pieces := max(f.Fragments, f.SplitContig)
	var out []segment
	for _, s := range segs {
		if s.kind != segData || pieces < 2 {
			out = append(out, s)
			continue
		}
		n := min(pieces, s.count)
		per := s.count / n
		for p := range n {
			c := per
			if p == n-1 {
				c = s.count - per*(n-1)
			}
			out = append(out, segment{kind: segData, first: s.first + p*per, count: c})
		}
	}
	// Physical placement: logical order, or reversed with Fragments.
	var dataIdx []int
	total := 0
	for i, s := range out {
		if s.kind == segData {
			dataIdx = append(dataIdx, i)
			total += s.count
		}
	}
	start = d.next
	if total > 0 {
		d.take(total)
	}
	phys := make([]uint64, len(out))
	cur := start
	order := orderOf(dataIdx, f.Fragments > 1)
	for _, i := range order {
		phys[i] = cur
		lo := out[i].first * bs
		hi := min(lo+out[i].count*bs, len(f.Data))
		d.put(cur, f.Data[lo:hi])
		cur += uint64(out[i].count)
	}
	for i, s := range out {
		switch s.kind {
		case segData:
			exts = append(exts, Extent{Logical: uint64(s.first * bs), Length: uint64(s.count * bs), Phys: phys[i], Crypto: f.ExtentCrypto})
		case segHole:
			exts = append(exts, Extent{Logical: uint64(s.first * bs), Length: uint64(s.count * bs)})
		}
	}
	return exts, start
}

// orderOf returns idx, or a reversed copy.
func orderOf(idx []int, reverse bool) []int {
	out := append([]int(nil), idx...)
	if reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// ExtentVal encodes a j_file_extent_val_t.
func ExtentVal(e Extent) []byte {
	v := make([]byte, 24)
	le.PutUint64(v, e.Length&0x00ffffffffffffff|uint64(e.Flags)<<56)
	le.PutUint64(v[8:], e.Phys)
	le.PutUint64(v[16:], e.Crypto)
	return v
}
