package hfsplustest

import (
	"encoding/binary"
	"strconv"
)

// privateFolderName is the name of the hard-link metadata folder: four NUL
// units and "HFS+ Private Data".
func privateFolderName(prefix uint16) []uint16 {
	return append([]uint16{prefix, prefix, prefix, prefix}, unitsOf("HFS+ Private Data")...)
}

// expandLinks turns HardLink groups into link records plus the private metadata
// folder and its iNode files, appended after the listed files (so the listed
// files keep CNIDs 16 .. 16+len-1). It returns the files to build, the CNID of
// the private folder (0 without groups) and the inode number of each group.
func expandLinks(files []File, prefix uint16) ([]File, uint32, map[string]uint32) {
	out := append([]File(nil), files...)
	var order []string
	count := map[string]uint32{}
	for _, f := range files {
		if f.HardLink != "" {
			if count[f.HardLink] == 0 {
				order = append(order, f.HardLink)
			}
			count[f.HardLink]++
		}
	}
	for i := range out {
		if out[i].LinkInode != 0 {
			out[i].FileType, out[i].FileCreator, out[i].Special = "hlnk", "hfs+", out[i].LinkInode
		}
	}
	if len(order) == 0 {
		return out, 0, nil
	}
	priv := 16 + uint32(len(files))
	inodes := map[string]uint32{}
	for gi, g := range order {
		inodes[g] = priv + 1 + uint32(gi)
	}
	donated := map[string]bool{}
	iNodes := map[string]File{}
	for i, f := range files {
		g := f.HardLink
		if g == "" {
			continue
		}
		n := inodes[g]
		if !donated[g] {
			donated[g] = true
			in := f
			in.HardLink = ""
			name := "iNode" + strconv.FormatUint(uint64(n), 10)
			in.Path = "/.hfs-private/" + name
			in.NameUnits = unitsOf(name)
			in.Special = count[g]
			iNodes[g] = in
		}
		link := f
		link.HardLink = ""
		link.Data, link.Rsrc, link.Attrs = nil, nil, nil
		link.DataLogical, link.RsrcLogical, link.RsrcBlocks, link.Fragment, link.OwnerFlags = 0, 0, 0, 0, 0
		link.FileType, link.FileCreator, link.Special = "hlnk", "hfs+", n
		out[i] = link
	}
	out = append(out, File{Path: "/.hfs-private", Dir: true, NameUnits: privateFolderName(prefix), Mode: 0o40555})
	for _, g := range order {
		out = append(out, iNodes[g])
	}
	return out, priv, inodes
}

// fileFork is where a file's forks were placed.
type fileFork struct {
	data, rsrc           []Extent
	dataBlocks, rsrcBlks uint32
}

func ceilDiv(n, d int) int { return (n + d - 1) / d }

// allocForks places the data and resource forks of every file in order from
// block cur on: a fragmented data fork leaves one free block after each of its
// extents. It returns the placement and the first block after the last fork.
func allocForks(files []File, bs, cur int) ([]fileFork, int) {
	out := make([]fileFork, len(files))
	for i, f := range files {
		if f.Dir {
			continue
		}
		fk := &out[i]
		if nb := ceilDiv(len(f.Data), bs); nb > 0 {
			fk.dataBlocks = uint32(nb)
			if f.Fragment == 0 {
				fk.data = []Extent{{uint32(cur), uint32(nb)}}
				cur += nb
			} else {
				for left := nb; left > 0; {
					n := min(left, int(f.Fragment))
					fk.data = append(fk.data, Extent{uint32(cur), uint32(n)})
					cur += n + 1
					left -= n
				}
			}
		}
		if nb := ceilDiv(len(f.Rsrc), bs); nb > 0 {
			fk.rsrcBlks = uint32(nb)
			fk.rsrc = []Extent{{uint32(cur), uint32(nb)}}
			cur += nb
		}
	}
	return out, cur
}

// overflowOf returns the extents-overflow records of a fork's extents past the
// eighth.
func overflowOf(fileID uint32, resource bool, exts []Extent) []OverflowRecord {
	if len(exts) <= 8 {
		return nil
	}
	var covered uint32
	for _, e := range exts[:8] {
		covered += e.Count
	}
	var out []OverflowRecord
	for rest := exts[8:]; len(rest) > 0; {
		n := min(8, len(rest))
		out = append(out, OverflowRecord{FileID: fileID, Resource: resource, StartBlock: covered, Extents: rest[:n]})
		for _, e := range rest[:n] {
			covered += e.Count
		}
		rest = rest[n:]
	}
	return out
}

// fileOverflow lists the overflow records of every file's forks.
func fileOverflow(forks []fileFork) []OverflowRecord {
	var out []OverflowRecord
	for i, fk := range forks {
		id := 16 + uint32(i)
		out = append(out, overflowOf(id, false, fk.data)...)
		out = append(out, overflowOf(id, true, fk.rsrc)...)
	}
	return out
}

// attrKey is an attributes-tree key with its keyLength prefix: keyLength,
// pad, fileID, startBlock, name length and name.
func attrKey(fileID, startBlock uint32, name []uint16) []byte {
	be := binary.BigEndian
	k := make([]byte, 2+12+2*len(name))
	be.PutUint16(k[0:], uint16(12+2*len(name)))
	be.PutUint32(k[4:], fileID)
	be.PutUint32(k[8:], startBlock)
	be.PutUint16(k[12:], uint16(len(name)))
	for i, u := range name {
		be.PutUint16(k[14+2*i:], u)
	}
	return k
}

// attrRecords lists the attributes-tree records of every file, sorted by
// (fileID, name, startBlock).
func attrRecords(files []File) []rec {
	be := binary.BigEndian
	var recs []rec
	for i, f := range files {
		id := 16 + uint32(i)
		for _, a := range f.Attrs {
			name := unitsOf(a.Name)
			var data []byte
			if a.ForkData {
				data = make([]byte, 8+80)
				be.PutUint32(data[0:], 0x20)
				be.PutUint64(data[8:], uint64(len(a.Value)))
			} else {
				data = make([]byte, 16+len(a.Value))
				be.PutUint32(data[0:], 0x10)
				be.PutUint32(data[12:], uint32(len(a.Value)))
				copy(data[16:], a.Value)
			}
			recs = append(recs, rec{key: attrKey(id, 0, name), data: data, parent: id, name: name})
		}
	}
	sortCatalog(recs, true, false)
	return recs
}
