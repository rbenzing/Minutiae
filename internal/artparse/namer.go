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
// not an artifact kind a parser may read.
var ErrNotParserInput = errors.New("artifact is not parser input")

// DefaultNamers are the device-file namer, the extract namer and the recovered namer, in that order.
func DefaultNamers() []Namer {
	return []Namer{deviceFileNamer{}, extractNamer{}, recoveredNamer{}}
}

const (
	maxLogicalPath  = 4096
	reasonSnapshot  = "extracted from a filesystem snapshot (use --include-snapshots)"
	reasonRecovered = "recovered or carved data (use --include-recovered or name it with --artifact)"
)

// exclusion is the reason classify gives for leaving an otherwise acceptable kind out of discovery.
type exclusion int

const (
	exclNone      exclusion = iota
	exclSnapshot            // a snapshot extract without --include-snapshots
	exclRecovered           // recovered input without --include-recovered or --artifact
)

// classify applies the allowlist of artifact kinds. Live files, backups and live extracts are input;
// snapshot extracts need includeSnapshots; recovered and carved artifacts, and extracts that have a
// recovered ancestor (recoveredAncestor, which only the manifest can tell), need includeRecovered; every
// other kind (partition, serial, info, import, runs, unallocated, slack, journal, report, anything
// unknown) is never input. excl is the single reason of a refusal that a flag lifts.
func classify(m evidence.ManifestRecord, includeSnapshots, includeRecovered, recoveredAncestor bool) (ok bool, why string, excl exclusion) {
	switch m.Source.Kind {
	case "file", "backup":
		return true, "", exclNone
	case "extract":
		d := m.Source.Derived
		switch {
		case d == nil:
			return false, "extract artifact without a derivation", exclNone
		case d.Snapshot != nil && !includeSnapshots:
			return false, reasonSnapshot, exclSnapshot
		case recoveredAncestor && !includeRecovered:
			return false, reasonRecovered, exclRecovered
		}
		return true, "", exclNone
	case "recover", "carve":
		if !includeRecovered {
			return false, reasonRecovered, exclRecovered
		}
		return true, "", exclNone
	case "":
		return false, "artifact has no source kind", exclNone
	default:
		return false, fmt.Sprintf("source kind %q is not parser input", m.Source.Kind), exclNone
	}
}

// Eligible reports whether a manifest record may be parser input: live files, live extracts and
// backups, plus snapshot extracts when includeSnapshots and recover or carve artifacts when
// includeRecovered. It sees one record, so it cannot know that an extract has a recovered ancestor:
// discovery checks that against the manifest.
func Eligible(m evidence.ManifestRecord, includeSnapshots, includeRecovered bool) (ok bool, why string) {
	ok, why, _ = classify(m, includeSnapshots, includeRecovered, false)
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
	case "extract", "recover":
		return "filesystem type or path has no platform mapping"
	case "carve":
		return "carved data has no filesystem path"
	case "backup":
		return "iOS backup paths are not mapped yet"
	}
	return "no logical path"
}
