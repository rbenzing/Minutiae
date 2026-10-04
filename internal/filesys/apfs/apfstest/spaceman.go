package apfstest

// The space manager: the builder's own encoding of what the Format reference
// of the 2F plan says (spaceman_phys_t, chunk-info blocks, the CIB address
// array or the CAB layer, raw bitmap blocks). Every block the builder writes
// is marked allocated in the bitmaps, plus the internal pool that holds them.

const (
	typeSpacemanCAB = 6
	typeSpacemanCIB = 7

	chunkInfoSize = 32 // chunk_info_t
	cibHeader     = 40 // cib_o (32) + cib_index + cib_chunk_info_count
	cabHeader     = 40

	// spacemanAddrOffset is where the array of CIB (or CAB) addresses starts in
	// the spaceman object (sm_addr_offset of sm_dev[0]); sm_dev[1] has none.
	spacemanAddrOffset = 384

	ipBmBlocks = 2 // blocks of the internal pool's own bitmap, ahead of the pool
)

// ChunkGeo is one chunk of the main device.
type ChunkGeo struct {
	Index  int
	Addr   uint64 // first block of the chunk
	Blocks uint32 // blocks the chunk covers
	Free   uint32 // free blocks (ci_free_count)
	Bitmap uint64 // block of its bitmap, 0 when every block is free (mkapfs does the same)
	// Slot is the block reserved for the chunk's bitmap in the pool (used
	// whether or not Bitmap is 0).
	Slot uint64
}

// SpacemanGeo is where the space manager's objects are and what they say.
type SpacemanGeo struct {
	BlocksPerChunk, ChunksPerCIB, CibsPerCAB int
	Chunks                                   []ChunkGeo
	CIBs, CABs                               []uint64 // block addresses
	AddrOffset                               int      // sm_addr_offset of sm_dev[0]
	// IPBmBase and IPBase are the first blocks of the internal pool's bitmap and
	// of the pool; the pool holds the CABs, CIBs and chunk bitmaps.
	IPBmBase, IPBmBlocks, IPBase, IPBlocks uint64
	FreeCount                              uint64
}

// FreeRuns returns the free blocks of the builder's allocation map as
// half-open block ranges [start, end), in order.
func (g Geo) FreeRuns() [][2]uint64 {
	var out [][2]uint64
	for b := 0; b < len(g.Alloc); {
		if g.Alloc[b] {
			b++
			continue
		}
		s := b
		for b < len(g.Alloc) && !g.Alloc[b] {
			b++
		}
		out = append(out, [2]uint64{uint64(s), uint64(b)})
	}
	return out
}

func (o Options) chunksPerCIB() int {
	if o.ChunksPerCIB > 0 {
		return o.ChunksPerCIB
	}
	return (o.BlockSize - cibHeader) / chunkInfoSize
}

// poolLen is the blocks the internal pool needs: the pool's bitmap, the CABs,
// the CIBs and one bitmap slot per chunk.
func (o Options) poolLen() (n uint64, chunks, cibs, cabs int) {
	bpc := o.BlockSize * 8
	chunks = (o.Blocks + bpc - 1) / bpc
	cibs = (chunks + o.chunksPerCIB() - 1) / o.chunksPerCIB()
	if o.CibsPerCAB > 0 {
		cabs = (cibs + o.CibsPerCAB - 1) / o.CibsPerCAB
	}
	return uint64(ipBmBlocks + cabs + cibs + chunks), chunks, cibs, cabs
}

// buildSpaceman places the internal pool at block base, marks it allocated in
// alloc and returns the space manager's geometry and the blocks of the pool.
// alloc must already hold every other block the builder wrote.
func (o Options) buildSpaceman(base uint64, alloc []bool) (SpacemanGeo, []Block) {
	bs := o.BlockSize
	bpc := bs * 8
	n, nChunks, nCibs, nCabs := o.poolLen()
	if base+n > uint64(o.Blocks) {
		panic("apfstest: container too small for the space manager")
	}
	for i := range n {
		alloc[base+i] = true
	}
	sp := SpacemanGeo{
		BlocksPerChunk: bpc, ChunksPerCIB: o.chunksPerCIB(), CibsPerCAB: o.CibsPerCAB,
		AddrOffset: spacemanAddrOffset,
		IPBmBase:   base, IPBmBlocks: ipBmBlocks, IPBase: base + ipBmBlocks, IPBlocks: n - ipBmBlocks,
	}
	next := base + ipBmBlocks
	for range nCabs {
		sp.CABs = append(sp.CABs, next)
		next++
	}
	for range nCibs {
		sp.CIBs = append(sp.CIBs, next)
		next++
	}
	var blocks []Block
	for c := range nChunks {
		cg := ChunkGeo{Index: c, Addr: uint64(c * bpc), Slot: next}
		next++
		cg.Blocks = uint32(min(bpc, o.Blocks-c*bpc))
		bm := make([]byte, bs)
		var used int
		for i := range int(cg.Blocks) {
			if alloc[c*bpc+i] {
				bm[i/8] |= 1 << (i % 8)
				used++
			}
		}
		cg.Free = cg.Blocks - uint32(used)
		if used > 0 {
			cg.Bitmap = cg.Slot
			blocks = append(blocks, Block{Addr: cg.Slot, Data: bm})
		}
		sp.FreeCount += uint64(cg.Free)
		sp.Chunks = append(sp.Chunks, cg)
	}

	xid := o.Xid - uint64(o.Checkpoints-1) // like the other objects the builder writes below the checkpoints
	cpc := sp.ChunksPerCIB
	for k, addr := range sp.CIBs {
		b := make([]byte, bs)
		putObjHeader(b, addr, xid, flagPhysical|typeSpacemanCIB, 0)
		first := k * cpc
		cnt := min(cpc, nChunks-first)
		le.PutUint32(b[32:], uint32(k))
		le.PutUint32(b[36:], uint32(cnt))
		for j := range cnt {
			cg := sp.Chunks[first+j]
			e := b[cibHeader+j*chunkInfoSize:]
			le.PutUint64(e[0:], xid)
			le.PutUint64(e[8:], cg.Addr)
			le.PutUint32(e[16:], cg.Blocks)
			le.PutUint32(e[20:], cg.Free)
			le.PutUint64(e[24:], cg.Bitmap)
		}
		sealBlock(b)
		blocks = append(blocks, Block{Addr: addr, Oid: addr, Data: b})
	}
	for k, addr := range sp.CABs {
		b := make([]byte, bs)
		putObjHeader(b, addr, xid, flagPhysical|typeSpacemanCAB, 0)
		first := k * o.CibsPerCAB
		cnt := min(o.CibsPerCAB, nCibs-first)
		le.PutUint32(b[32:], uint32(k))
		le.PutUint32(b[36:], uint32(cnt))
		for j := range cnt {
			le.PutUint64(b[cabHeader+8*j:], sp.CIBs[first+j])
		}
		sealBlock(b)
		blocks = append(blocks, Block{Addr: addr, Oid: addr, Data: b})
	}
	return sp, blocks
}

// writeSpaceman writes spaceman_phys_t: the geometry, the main device with its
// CIB (or CAB) address array, the internal pool; the free queues are not used.
func writeSpaceman(b []byte, o Options, sp SpacemanGeo, xid uint64) {
	bs := o.BlockSize
	putObjHeader(b, oidSpaceman, xid, flagEphemeral|typeSpaceman, 0)
	le.PutUint32(b[32:], uint32(bs))
	le.PutUint32(b[36:], uint32(sp.BlocksPerChunk))
	le.PutUint32(b[40:], uint32(sp.ChunksPerCIB))
	cibsPerCAB := sp.CibsPerCAB
	if cibsPerCAB == 0 {
		cibsPerCAB = (bs - cabHeader) / 8
	}
	le.PutUint32(b[44:], uint32(cibsPerCAB))
	le.PutUint64(b[48:], uint64(o.Blocks))       // sm_dev[0].sm_block_count
	le.PutUint64(b[56:], uint64(len(sp.Chunks))) // sm_chunk_count
	le.PutUint32(b[64:], uint32(len(sp.CIBs)))   // sm_cib_count
	le.PutUint32(b[68:], uint32(len(sp.CABs)))   // sm_cab_count
	le.PutUint64(b[72:], sp.FreeCount)           // sm_free_count
	le.PutUint32(b[80:], uint32(sp.AddrOffset))  // sm_addr_offset
	le.PutUint32(b[148:], 16)                    // sm_ip_bm_tx_multiplier
	le.PutUint64(b[152:], sp.IPBlocks)           // sm_ip_block_count
	le.PutUint32(b[160:], 1)                     // sm_ip_bm_size_in_blocks
	le.PutUint32(b[164:], uint32(sp.IPBmBlocks)) // sm_ip_bm_block_count
	le.PutUint64(b[168:], sp.IPBmBase)           // sm_ip_bm_base
	le.PutUint64(b[176:], sp.IPBase)             // sm_ip_base
	addrs := sp.CIBs
	if len(sp.CABs) > 0 {
		addrs = sp.CABs
	}
	if sp.AddrOffset+8*len(addrs) > bs {
		panic("apfstest: the CIB/CAB address array does not fit the spaceman block")
	}
	le.PutUint32(b[128:], uint32(sp.AddrOffset+8*len(addrs))) // sm_dev[1].sm_addr_offset: right after, empty
	for i, a := range addrs {
		le.PutUint64(b[sp.AddrOffset+8*i:], a)
	}
}
