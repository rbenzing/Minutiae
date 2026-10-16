package androidmms

import (
	"strings"

	"github.com/rbenzing/minutiae/internal/parse"
)

// expressible reports whether a device path can be written as an absolute glob that
// matches exactly itself: '*' is the only metacharacter, backslash and NUL are
// refused by the compiler, empty, "." and ".." elements are refused too, and so is a path
// of more than 64 elements (the compiler's cap). It walks the path without splitting it.
func expressible(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "*\\") || strings.ContainsRune(p, 0) {
		return false
	}
	elems := 0
	for rest := p[1:]; ; {
		el, tail, more := strings.Cut(rest, "/")
		if el == "" || el == "." || el == ".." {
			return false
		}
		if elems++; elems > 64 {
			return false
		}
		if !more {
			return true
		}
		rest = tail
	}
}

// sameSource reports whether artifact a comes from the same device, acquisition, partition and
// snapshot as the message's database db (rulings T19, T25 and T46: identity from structure, never
// from a path match alone). Device and acquisition must be present on both sides: an empty one is
// unknown and is never "the same", so it never links. Partition and snapshot are compared as
// recorded (a live file has no snapshot).
func sameSource(a, db parse.Artifact) bool {
	if a.Source.DeviceID == "" || a.Source.DeviceID != db.Source.DeviceID {
		return false
	}
	if a.Acquisition == "" || a.Acquisition != db.Acquisition || a.Source.Partition != db.Source.Partition {
		return false
	}
	x, y := a.Source.Snapshot, db.Source.Snapshot
	if (x == nil) != (y == nil) {
		return false
	}
	return x == nil || *x == *y
}

type lookup int

const (
	lookupNone        lookup = iota
	lookupFound              // exactly one artifact of the message's own source
	lookupMissing            // the case holds no file at that logical path
	lookupOtherSource        // a path match exists, but only on another device, acquisition, partition or snapshot
	lookupAmbiguous          // several matches of the message's own source
)

// resolveAttachment finds the artifact of the case whose logical path is android:<path> and
// whose source is the message database's. Exactly one is a link. None, an inexpressible path
// or only matches from another source leave the attachment unlinked (an unlinked attachment
// means "not found for this source", not "no file"); several matches of the same source (a file
// pulled more than once) leave it unlinked and are counted, because choosing one would be a guess.
func resolveAttachment(in *parse.Input, path string) (parse.Artifact, lookup) {
	if in == nil || in.Lookup == nil || !expressible(path) {
		return parse.Artifact{}, lookupNone
	}
	found := in.Lookup.Find("android:" + path)
	if len(found) == 0 {
		return parse.Artifact{}, lookupMissing
	}
	var same []parse.Artifact
	for _, a := range found {
		if sameSource(a, in.Primary) {
			same = append(same, a)
		}
	}
	switch len(same) {
	case 1:
		return same[0], lookupFound
	case 0:
		return parse.Artifact{}, lookupOtherSource
	}
	return parse.Artifact{}, lookupAmbiguous
}
