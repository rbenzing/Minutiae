package apfs

import (
	"strconv"
	"strings"
)

// Entry IDs. Every decimal field is canonical: digits only, no sign, space,
// "0x" or leading zero (except a lone "0"), no overflow.
//
//	apfs:root          the container root (it lists the volumes)
//	n:<vol>:<x>:<ino>  a file-system object: <vol> is the nx_fs_oid slot (0..99),
//	                   <x> is 0 for the live view or the xid of a snapshot view,
//	                   <ino> the inode number (1..2^60-1)
//	snaps:<vol>        the synthetic .snapshots directory of a volume
//
// A volume appears under the root as its root directory n:<vol>:0:2.
const (
	rootID = "apfs:root"

	maxIno     = 1<<60 - 1 // object ids have 60 bits
	rootIno    = 2         // APFS_ROOT_DIR_INO_NUM
	privDirIno = 3         // PRIV_DIR_INO_NUM
)

type idKind int

const (
	idNone  idKind = iota // not an ID this reader produces
	idRoot                // the container root
	idNode                // a file-system object
	idSnaps               // the .snapshots directory of a volume
)

func nodeID(vol int, view, ino uint64) string {
	return "n:" + strconv.Itoa(vol) + ":" + strconv.FormatUint(view, 10) + ":" + strconv.FormatUint(ino, 10)
}

// canonDecimal parses a canonical unsigned decimal.
func canonDecimal(s string) (uint64, bool) {
	if s == "" || len(s) > 20 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// parseEntryID parses an Entry ID strictly. It only checks the syntax and the
// ranges that need no disk (volume < 100, ino in 1..2^60-1); whether the volume
// exists or the view is a known snapshot is for the caller.
func parseEntryID(id string) (kind idKind, vol int, view, ino uint64, ok bool) {
	if id == rootID {
		return idRoot, 0, 0, 0, true
	}
	if rest, found := strings.CutPrefix(id, "snaps:"); found {
		v, ok := canonDecimal(rest)
		if !ok || v >= nxMaxFileSystems {
			return idNone, 0, 0, 0, false
		}
		return idSnaps, int(v), 0, 0, true
	}
	rest, found := strings.CutPrefix(id, "n:")
	if !found {
		return idNone, 0, 0, 0, false
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 {
		return idNone, 0, 0, 0, false
	}
	v, ok1 := canonDecimal(parts[0])
	x, ok2 := canonDecimal(parts[1])
	n, ok3 := canonDecimal(parts[2])
	if !ok1 || !ok2 || !ok3 || v >= nxMaxFileSystems || n == 0 || n > maxIno {
		return idNone, 0, 0, 0, false
	}
	return idNode, int(v), x, n, true
}
