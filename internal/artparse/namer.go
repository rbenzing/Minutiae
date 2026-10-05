package artparse

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Logical is the logical path an artifact was given, with the platform and the namer.
type Logical struct{ Path, Platform, Namer string }

// Env is what namers may consult: the run snapshot's device platforms. Namers
// never open artifacts.
type Env struct {
	DevicePlatform map[string]string // device id -> "android" | "ios"; absent = unknown or conflicting
}

// Namer derives a logical path from a manifest record.
type Namer interface {
	Name() string
	Logical(m evidence.ManifestRecord, env Env) (Logical, bool)
}

// ErrNotParserInput is returned when an artifact that is named explicitly is
// not live evidence a parser may read.
var ErrNotParserInput = errors.New("artifact is not parser input")

// DefaultNamers are the device-file namer and the extract namer, in that order.
func DefaultNamers() []Namer { return []Namer{deviceFileNamer{}, extractNamer{}} }

const (
	maxLogicalPath = 4096
	reasonSnapshot = "extracted from a filesystem snapshot (use --include-snapshots)"
)

// classify applies the F6 allowlist of artifact kinds. snapshotExcluded is true
// when the only reason for refusing is the snapshot rule.
func classify(m evidence.ManifestRecord, includeSnapshots bool) (ok bool, why string, snapshotExcluded bool) {
	switch m.Source.Kind {
	case "file", "backup":
		return true, "", false
	case "extract":
		d := m.Source.Derived
		switch {
		case d == nil:
			return false, "extract artifact without a derivation", false
		case d.Snapshot != nil && !includeSnapshots:
			return false, reasonSnapshot, true
		}
		return true, "", false
	case "":
		return false, "artifact has no source kind", false
	default:
		return false, fmt.Sprintf("source kind %q is not parser input", m.Source.Kind), false
	}
}

// Eligible reports whether a manifest record may be parser input: live files,
// live extracts and backups, plus snapshot extracts when includeSnapshots.
func Eligible(m evidence.ManifestRecord, includeSnapshots bool) (ok bool, why string) {
	ok, why, _ = classify(m, includeSnapshots)
	return ok, why
}

// NormalizeAndroidPath returns the canonical form of an absolute Android path:
// it must start with "/", "//" collapses, and a NUL, backslash, "." or ".."
// element, a trailing "/", an empty path or more than 4096 bytes mean "no path".
func NormalizeAndroidPath(p string) (string, bool) {
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	rel, ok := normalizeElems(p[1:], p)
	if !ok {
		return "", false
	}
	return "/" + rel, true
}

// normalizeIOSRelative is the same for the part after an iOS domain: no leading "/".
func normalizeIOSRelative(p string) (string, bool) {
	if strings.HasPrefix(p, "/") {
		return "", false
	}
	return normalizeElems(p, p)
}

func normalizeElems(rest, whole string) (string, bool) {
	if len(whole) > maxLogicalPath || strings.ContainsAny(whole, "\x00\\") || strings.HasSuffix(whole, "/") {
		return "", false
	}
	var out []string
	for _, e := range strings.Split(rest, "/") {
		switch e {
		case "":
			// "//" collapses
		case ".", "..":
			return "", false
		default:
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return "", false
	}
	return strings.Join(out, "/"), true
}

// SourceInfo is the parse.SourceInfo of a manifest record (a copy: the snapshot
// reference is not shared).
func SourceInfo(m evidence.ManifestRecord) parse.SourceInfo {
	s := m.Source
	si := parse.SourceInfo{
		Kind: s.Kind, DeviceID: s.DeviceID, RemotePath: s.RemotePath, OriginalPath: s.OriginalPath, Partition: s.Partition,
	}
	if d := s.Derived; d != nil {
		si.FSType, si.FSPath = d.FSType, d.FSPath
		si.Encrypted, si.ParentIncomplete = d.Encrypted, d.ParentIncomplete
		if d.Snapshot != nil {
			si.Snapshot = &parse.SnapshotInfo{Name: d.Snapshot.Name, Xid: d.Snapshot.Xid}
		}
	}
	return si
}

// WhyNoLogical says, for plan output, why a record has no logical path.
func WhyNoLogical(m evidence.ManifestRecord, env Env) string {
	switch m.Source.Kind {
	case "file":
		if _, ok := env.DevicePlatform[m.Source.DeviceID]; !ok {
			return "platform unknown for device " + m.Source.DeviceID
		}
		return "remote path is not a usable path"
	case "extract":
		return "filesystem type or path has no platform mapping"
	case "backup":
		return "iOS backup paths are not mapped yet"
	}
	return "no logical path"
}
