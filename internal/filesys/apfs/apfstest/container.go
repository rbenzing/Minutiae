package apfstest

import "encoding/binary"

var le = binary.LittleEndian

// writeSuperblock fills an nx_superblock_t (offsets from the Apple reference;
// see the Format reference of the plan).
func writeSuperblock(b []byte, o Options, g Geo, cp Checkpoint) {
	putObjHeader(b, oidSuperblock, cp.Xid, flagEphemeral|typeNXSuperblock, 0)
	copy(b[32:], nxMagic)
	le.PutUint32(b[36:], uint32(o.BlockSize))
	le.PutUint64(b[40:], uint64(o.Blocks))
	le.PutUint64(b[48:], 0)                // nx_features
	le.PutUint64(b[56:], 0)                // nx_readonly_compatible_features
	le.PutUint64(b[64:], incompatVersion2) // nx_incompatible_features
	copy(b[72:88], o.UUID[:])
	le.PutUint64(b[88:], firstUnusedOid)
	le.PutUint64(b[96:], cp.Xid+1) // nx_next_xid
	le.PutUint32(b[104:], uint32(g.DescCount))
	le.PutUint32(b[108:], uint32(g.DataCount))
	le.PutUint64(b[112:], g.DescBase)
	le.PutUint64(b[120:], g.DataBase)
	le.PutUint32(b[128:], uint32((cp.Index+1)%int(g.DescCount))) // nx_xp_desc_next
	le.PutUint32(b[132:], 0)                                     // nx_xp_data_next
	le.PutUint32(b[136:], uint32(cp.DescIndex))
	le.PutUint32(b[140:], uint32(cp.DescLen))
	le.PutUint32(b[144:], 0) // nx_xp_data_index
	le.PutUint32(b[148:], 1) // nx_xp_data_len
	le.PutUint64(b[152:], g.SpacemanOid)
	le.PutUint64(b[160:], g.Omap)
	le.PutUint64(b[168:], 0) // nx_reaper_oid
	le.PutUint32(b[180:], uint32(max(len(o.Volumes), 1)))
	var flags uint64
	if o.CryptoSW {
		flags |= nxCryptoSW
	}
	le.PutUint64(b[1264:], flags)
}

// writeMap fills a checkpoint_map_phys_t; the first map block of a checkpoint
// carries the spaceman mapping, the last one is flagged CHECKPOINT_MAP_LAST.
func writeMap(b []byte, g Geo, cp Checkpoint, addr uint64, first, last bool) {
	putObjHeader(b, addr, cp.Xid, flagPhysical|typeCheckpointMap, 0)
	var flags, count uint32
	if last {
		flags |= cpmLast
	}
	if first {
		count = 1
		e := b[checkpointMapStart:]
		le.PutUint32(e[0:], flagEphemeral|typeSpaceman) // cpm_type
		le.PutUint32(e[4:], 0)                          // cpm_subtype
		le.PutUint32(e[8:], uint32(g.BlockSize))        // cpm_size
		le.PutUint32(e[12:], 0)                         // cpm_pad
		le.PutUint64(e[16:], 0)                         // cpm_fs_oid
		le.PutUint64(e[24:], g.SpacemanOid)             // cpm_oid
		le.PutUint64(e[32:], cp.Spaceman)               // cpm_paddr
	}
	le.PutUint32(b[32:], flags)
	le.PutUint32(b[36:], count)
}

// writeSpaceman writes the part of spaceman_phys_t that describes the device
// geometry (no chunk-info blocks yet: the space manager task extends this).
func writeSpaceman(b []byte, o Options, xid uint64) {
	bs := o.BlockSize
	putObjHeader(b, oidSpaceman, xid, flagEphemeral|typeSpaceman, 0)
	bpc := uint64(bs) * 8
	le.PutUint32(b[32:], uint32(bs))
	le.PutUint32(b[36:], uint32(bpc))
	le.PutUint32(b[40:], uint32((bs-40)/32))
	le.PutUint32(b[44:], uint32((bs-40)/8))
	le.PutUint64(b[48:], uint64(o.Blocks))             // sm_dev[0].sm_block_count
	le.PutUint64(b[56:], (uint64(o.Blocks)+bpc-1)/bpc) // sm_dev[0].sm_chunk_count
}

// writeOmap writes the omap_phys_t of the container object map (no tree yet:
// the B-tree task adds it).
func writeOmap(b []byte, addr, xid uint64) {
	putObjHeader(b, addr, xid, flagPhysical|typeOmap, 0)
	le.PutUint32(b[32:], 1)              // om_flags: MANUALLY_MANAGED
	le.PutUint32(b[40:], flagPhysical|2) // om_tree_type: physical B-tree
	le.PutUint32(b[44:], flagPhysical|2) // om_snapshot_tree_type
}
