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
	nextOid := uint64(firstUnusedOid)
	if len(o.Volumes) > 0 {
		nextOid = max(nextOid, VolumeOid(len(o.Volumes)-1)+1)
	}
	le.PutUint64(b[88:], nextOid)
	le.PutUint64(b[96:], cp.Xid+1) // nx_next_xid
	le.PutUint32(b[104:], uint32(g.DescCount))
	le.PutUint32(b[108:], uint32(g.DataCount))
	le.PutUint64(b[112:], g.DescBase)
	le.PutUint64(b[120:], g.DataBase)
	le.PutUint32(b[128:], uint32((cp.Index+1)%int(g.DescCount)))              // nx_xp_desc_next
	le.PutUint32(b[132:], uint32((cp.DataIndex+cp.DataLen)%int(g.DataCount))) // nx_xp_data_next
	le.PutUint32(b[136:], uint32(cp.DescIndex))
	le.PutUint32(b[140:], uint32(cp.DescLen))
	le.PutUint32(b[144:], uint32(cp.DataIndex))
	le.PutUint32(b[148:], uint32(cp.DataLen))
	le.PutUint64(b[152:], g.SpacemanOid)
	le.PutUint64(b[160:], g.Omap)
	le.PutUint64(b[168:], 0) // nx_reaper_oid
	le.PutUint32(b[180:], uint32(max(len(o.Volumes), 1)))
	for i := range o.Volumes {
		le.PutUint64(b[184+8*i:], VolumeOid(i)) // nx_fs_oid[i]
	}
	for i := range o.Volumes {
		le.PutUint64(b[184+8*i:], VolumeOid(i)) // nx_fs_oid[i]
	}
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

// writeOmap writes the omap_phys_t of the container object map; its tree is
// packed by Build.
func writeOmap(b []byte, addr, xid, treeOid, snapTreeOid uint64, snapCount int) {
	OmapPhys(b, addr, xid, 1 /* MANUALLY_MANAGED */, uint32(snapCount), treeOid, snapTreeOid)
}
