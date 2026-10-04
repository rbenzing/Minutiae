package hfsplus

import (
	"encoding/binary"
	"errors"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Attributes B-tree (extended attributes). Key (after keyLength): pad u16,
// fileID u32 @2, startBlock u32 @6, nameLength u16 @10, name UTF-16BE @12;
// order: fileID, name (binary), startBlock. Record: recordType u32 (0x10 inline
// data, 0x20 fork data, 0x30 extents), reserved; inline data has its size at
// byte 12 and the value from byte 16. Layout from TN1150 as remembered; it is
// covered by builder images only (the real fixtures have no attributes file).
const (
	attrRecInline  = 0x10
	attrRecFork    = 0x20
	attrRecExtents = 0x30

	attrKeyMin       = 12  // pad, fileID, startBlock, nameLength
	maxAttrNameUnits = 127 // longest attribute name
	maxAttrRecords   = 4096
	maxAttrShown     = 32
	maxAttrValue     = 64 << 10 // longest inline value read (only decmpfs and cprotect values are read at all)

	attrForkRecMin = 8 + 80 // type, reserved, HFSPlusForkData

	nameDecmpfs  = "com.apple.decmpfs"
	nameCProtect = "com.apple.system.cprotect"
)

var (
	decmpfsUnits  = unitsOf(nameDecmpfs)
	cprotectUnits = unitsOf(nameCProtect)
)

// attrInfo is what the attributes tree says about one catalog node.
type attrInfo struct {
	names    []string // distinct attribute names in tree order, at most maxAttrShown
	more     int      // distinct names past maxAttrShown
	cprotect bool     // a com.apple.system.cprotect attribute exists
	decmpfs  bool     // a com.apple.decmpfs attribute exists
	value    []byte   // its inline value (nil when it is stored as a fork or is too long)
	unread   bool     // the tree could not be read (completely): the info may be missing
}

// attrTree returns the attributes tree, nil when the volume has none or when
// it cannot be opened (that is warned about once and not retried).
func (f *FS) attrTree() (*btree, error) {
	f.treeMu.Lock()
	broken := f.attrsBroken
	f.treeMu.Unlock()
	if broken {
		return nil, nil
	}
	t, err := f.tree(treeAttributes)
	if err != nil && errors.Is(err, filesys.ErrCorrupt) {
		f.treeMu.Lock()
		f.attrsBroken = true
		f.treeMu.Unlock()
		f.warn("the attributes B-tree cannot be read, so extended attributes are not reported: %v", err)
		return nil, nil
	}
	return t, err
}

// attributesOf collects the attributes of a catalog node: it descends to the
// first record of the node's fileID and reads the contiguous run (at most
// maxAttrRecords records). Hostile records (a size past the record, a name past
// the key) are warned about and skipped. Only an I/O error is returned; a
// damaged tree gives what was read, flagged unread.
func (f *FS) attributesOf(cnid uint32) (attrInfo, error) {
	var out attrInfo
	t, err := f.attrTree()
	if err != nil || t == nil {
		return out, err
	}
	cmp := func(key []byte) (int, error) {
		if len(key) < 6 {
			return 0, corrupt("attributes B-tree", -1, "key of %d bytes has no file id", len(key))
		}
		if binary.BigEndian.Uint32(key[2:]) < cnid {
			return -1, nil
		}
		return 1, nil // every record of this id sorts at or after (cnid, empty name)
	}
	var last []uint16
	haveLast := false
	examined, bad := 0, 0
	var firstBad string
	skip := func(why string) {
		if bad++; bad == 1 {
			firstBad = why
		}
	}
	err = t.scanFrom(cmp, func(rec []byte) (bool, error) {
		key, data, err := splitRecord(rec)
		if err != nil || len(key) < 6 {
			skip("a record without a usable key")
			return true, nil
		}
		id := binary.BigEndian.Uint32(key[2:])
		if id < cnid {
			return true, nil
		}
		if id > cnid {
			return false, nil
		}
		if examined++; examined > maxAttrRecords {
			f.warn("attributes of id %d: more than %d records; the rest are not read", cnid, maxAttrRecords)
			out.unread = true
			return false, nil
		}
		if len(key) < attrKeyMin {
			skip("a key too short for its fields")
			return true, nil
		}
		n := int(binary.BigEndian.Uint16(key[10:]))
		if n > maxAttrNameUnits || attrKeyMin+2*n > len(key) {
			skip("a name of " + strconv.Itoa(n) + " units that does not fit its key")
			return true, nil
		}
		name := readUnits(key[12:], n)
		if len(data) < 4 {
			skip("a record without a type")
			return true, nil
		}
		var value []byte
		switch typ := binary.BigEndian.Uint32(data); typ {
		case attrRecInline:
			if len(data) < 16 {
				skip("an inline record too short for its size field")
				return true, nil
			}
			size := binary.BigEndian.Uint32(data[12:])
			if uint64(size) > uint64(len(data)-16) {
				skip("an inline value of " + strconv.FormatUint(uint64(size), 10) + " bytes that runs past its record")
				return true, nil
			}
			value = data[16 : 16+size]
		case attrRecFork:
			if len(data) < attrForkRecMin {
				skip("a fork-data record shorter than its fork")
				return true, nil
			}
		case attrRecExtents:
			return true, nil // continues a fork-data attribute: no name of its own
		default:
			skip("a record of unknown type " + strconv.FormatUint(uint64(typ), 10))
			return true, nil
		}
		if haveLast && slices.Equal(last, name) {
			return true, nil
		}
		last, haveLast = name, true
		if len(out.names) < maxAttrShown {
			s, _ := displayName(name)
			out.names = append(out.names, s)
		} else {
			out.more++
		}
		switch {
		case slices.Equal(name, cprotectUnits):
			out.cprotect = true
		case slices.Equal(name, decmpfsUnits):
			out.decmpfs = true
			if value != nil && len(value) <= maxAttrValue {
				out.value = slices.Clone(value)
			}
		}
		return true, nil
	})
	if bad > 0 {
		f.warn("attributes of id %d: %d damaged records were skipped (first: %s)", cnid, bad, firstBad)
	}
	switch {
	case err == nil:
	case errors.Is(err, filesys.ErrCorrupt):
		out.unread = true
		f.warn("the attributes of id %d cannot be read completely: %v", cnid, err)
	default:
		return attrInfo{}, err
	}
	return out, nil
}
