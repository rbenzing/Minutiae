package hfsplus

import (
	"errors"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Hard links (TN1150 "Hard Links", from memory; the real fixtures hold none, so
// this is covered by builder images only): a file record whose Finder fileType
// is 'hlnk' and creator 'hfs+' is a link whose BSD "special" field is the inode
// number N; the data is the file iNode<N> in the private metadata folder, a
// child of the root named by four identical U+0000/U+2400/U+200B units and
// "HFS+ Private Data". The iNode's own special field is the link count. A
// directory hard link has fileType 'fldr' and creator 'MACS'.
const (
	ftypeHardLink    = 0x686C6E6B // 'hlnk'
	fcreatorHardLink = 0x6866732B // 'hfs+'
	ftypeDirLink     = 0x666C6472 // 'fldr'
	fcreatorDirLink  = 0x4D414353 // 'MACS'
)

type linkKind int

const (
	linkNone     linkKind = iota
	linkFile              // resolved to an iNode
	linkDir               // a directory hard link: not followed
	linkDangling          // the private folder or the iNode does not exist
	linkInvalid           // the iNode is unusable (itself a link, not a file, unreadable)
)

// linkInfo is the result of resolveLink. inode is the indirect node's record
// for linkFile.
type linkInfo struct {
	kind     linkKind
	inodeNum uint32
	inode    catalogRecord
	why      string // for dangling and invalid links
}

func isFileLink(r *catalogRecord) bool {
	return r.typ == recFile && r.fileType == ftypeHardLink && r.fileCreator == fcreatorHardLink
}

func isDirLink(r *catalogRecord) bool {
	return r.typ == recFile && r.fileType == ftypeDirLink && r.fileCreator == fcreatorDirLink
}

// privateFolder returns the CNID of the hard-link private metadata folder, 0
// when the volume has none. The result is cached (a miss too); an I/O error is
// returned and not cached. The names are found by the catalog descent alone, so
// a volume without links costs no directory scan.
func (f *FS) privateFolder() (uint32, error) {
	f.privMu.Lock()
	done, id := f.privDone, f.privID
	f.privMu.Unlock()
	if done {
		return id, nil
	}
	id = 0
	for _, prefix := range []uint16{0, 0x2400, 0x200B} {
		name := append([]uint16{prefix, prefix, prefix, prefix}, privateDataName...)
		k, r, err := f.findRecordTree(rootFolderID, name)
		switch {
		case err == nil:
			if r.typ == recFolder && isPrivateMetadataName(k.name) {
				id = r.id
			}
		case errors.Is(err, filesys.ErrNotFound):
		case errors.Is(err, filesys.ErrCorrupt):
			f.warn("the hard-link private folder cannot be looked up: %v", err)
		default:
			return 0, err
		}
		if id != 0 {
			break
		}
	}
	f.privMu.Lock()
	f.privDone, f.privID = true, id
	f.privMu.Unlock()
	return id, nil
}

// resolveLink classifies a file record and, for a file hard link, finds its
// iNode. Only an I/O error is returned: a missing or unusable target is a
// dangling or invalid link, described by why.
func (f *FS) resolveLink(r *catalogRecord) (linkInfo, error) {
	switch {
	case isDirLink(r):
		return linkInfo{kind: linkDir, inodeNum: r.bsd.special}, nil
	case !isFileLink(r):
		return linkInfo{}, nil
	}
	n := r.bsd.special
	li := linkInfo{kind: linkDangling, inodeNum: n}
	priv, err := f.privateFolder()
	if err != nil {
		return linkInfo{}, err
	}
	if priv == 0 {
		li.why = "the volume has no private metadata folder"
		return li, nil
	}
	_, ir, err := f.findRecordTree(priv, unitsOf("iNode"+strconv.FormatUint(uint64(n), 10)))
	switch {
	case err == nil:
	case errors.Is(err, filesys.ErrNotFound):
		li.why = "iNode" + strconv.FormatUint(uint64(n), 10) + " does not exist"
		return li, nil
	case errors.Is(err, filesys.ErrCorrupt):
		li.kind, li.why = linkInvalid, "iNode"+strconv.FormatUint(uint64(n), 10)+" cannot be read: "+err.Error()
		return li, nil
	default:
		return linkInfo{}, err
	}
	switch {
	case ir.typ != recFile:
		li.kind, li.why = linkInvalid, "iNode"+strconv.FormatUint(uint64(n), 10)+" is not a file"
	case isFileLink(&ir) || isDirLink(&ir):
		li.kind, li.why = linkInvalid, "iNode"+strconv.FormatUint(uint64(n), 10)+" is itself a link record"
	default:
		li.kind, li.inode = linkFile, ir
	}
	return li, nil
}
