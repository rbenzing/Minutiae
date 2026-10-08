package dbrecover

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// SiblingStatus says how the search for one companion (WAL or journal) ended.
type SiblingStatus string

// The statuses. Found, none, ambiguous, no-identity and other-acquisition come
// from FindSiblings; explicit and disabled are set by the caller when the
// examiner named a companion or turned the search off.
const (
	SiblingFound            SiblingStatus = "found"
	SiblingNone             SiblingStatus = "none"
	SiblingAmbiguous        SiblingStatus = "ambiguous"
	SiblingExplicit         SiblingStatus = "explicit"
	SiblingDisabled         SiblingStatus = "disabled"
	SiblingNoIdentity       SiblingStatus = "no-identity"
	SiblingOtherAcquisition SiblingStatus = "other-acquisition"
)

// ErrAmbiguousSibling: more than one artifact has the identity of a companion.
var ErrAmbiguousSibling = errors.New("dbrecover: more than one candidate for a companion file")

// SiblingResult is the outcome for one companion role. SearchedBy is the
// identity that was searched for, %q-quoted, so it is safe to print.
type SiblingResult struct {
	Status           SiblingStatus
	ArtifactID       string
	Candidates       []string // ambiguous: every candidate, in manifest order
	SearchedBy       string
	OtherAcquisition []string // other-acquisition: artifacts of another acquisition, never paired
}

// SiblingReport holds the results for the WAL and the rollback journal.
type SiblingReport struct{ WAL, Journal SiblingResult }

// Text is a one-line statement of the result. A search that found nothing
// says "none found" and how it searched; it never claims that the file does
// not exist.
func (r SiblingResult) Text(role string) string {
	switch r.Status {
	case SiblingFound:
		return fmt.Sprintf("%s: artifact %s (found by %s)", role, r.ArtifactID, r.SearchedBy)
	case SiblingNone:
		return fmt.Sprintf("%s: none found (searched by %s)", role, r.SearchedBy)
	case SiblingOtherAcquisition:
		return fmt.Sprintf("%s: none found in this acquisition (searched by %s); %s from another acquisition, not paired (name it explicitly to use it)",
			role, r.SearchedBy, quoteList(r.OtherAcquisition))
	case SiblingAmbiguous:
		return fmt.Sprintf("%s: ambiguous, candidates %s (searched by %s)", role, quoteList(r.Candidates), r.SearchedBy)
	case SiblingNoIdentity:
		return fmt.Sprintf("%s: not searched, the database has no path identity (%s)", role, r.SearchedBy)
	case SiblingExplicit:
		return fmt.Sprintf("%s: artifact %s (named by the examiner)", role, r.ArtifactID)
	case SiblingDisabled:
		return fmt.Sprintf("%s: not searched (the search is turned off)", role)
	}
	return fmt.Sprintf("%s: %q", role, string(r.Status))
}

// Err is an ErrAmbiguousSibling listing the candidates when the result is
// ambiguous, nil otherwise.
func (r SiblingResult) Err(role string) error {
	if r.Status != SiblingAmbiguous {
		return nil
	}
	return fmt.Errorf("%w: %s: %s; name one by artifact id or turn the search off", ErrAmbiguousSibling, role, quoteList(r.Candidates))
}

func quoteList(ids []string) string {
	q := make([]string, len(ids))
	for i, id := range ids {
		q[i] = quoteCap(id)
	}
	return strings.Join(q, ", ")
}

// maxQuoted bounds the part of a path shown in SearchedBy; the match itself
// always uses the full string.
const maxQuoted = 256

func quoteCap(s string) string {
	if len(s) <= maxQuoted {
		return fmt.Sprintf("%q", s)
	}
	cut := maxQuoted
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%q", s[:cut]) + fmt.Sprintf("... (%d bytes)", len(s))
}

// siblingScan is the scan FindSiblings guards; a test replaces it to poison it.
var siblingScan = scanSiblings

// FindSiblings looks in the manifest snapshot man for the WAL and the rollback
// journal of the database db by manifest identity (decision D8): never by
// guessing from a similar name. It never returns an error and never panics.
func FindSiblings(man []evidence.ManifestRecord, db evidence.ManifestRecord) (rep SiblingReport) {
	defer func() {
		if p := recover(); p != nil {
			res := SiblingResult{Status: SiblingNoIdentity, SearchedBy: fmt.Sprintf("the search failed with an internal error: %v", p)}
			rep = SiblingReport{WAL: res, Journal: res}
		}
	}()
	return siblingScan(man, db)
}

// acquisitionKey is "artifacts/<device>/<acquisition>" from a manifest path.
func acquisitionKey(path string) (string, bool) {
	parts := strings.Split(path, "/")
	if len(parts) < 4 || parts[0] != "artifacts" {
		return "", false
	}
	for _, p := range parts[:3] {
		if p == "" || p == ".." || p == "." {
			return "", false
		}
	}
	return strings.Join(parts[:3], "/"), true
}

func sameSnapshot(a, b *evidence.SnapshotRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func scanSiblings(man []evidence.ManifestRecord, db evidence.ManifestRecord) SiblingReport {
	return SiblingReport{
		WAL:     scanOne(man, db, "-wal"),
		Journal: scanOne(man, db, "-journal"),
	}
}

func scanOne(man []evidence.ManifestRecord, db evidence.ManifestRecord, suffix string) SiblingResult {
	noID := func(why string) SiblingResult {
		return SiblingResult{Status: SiblingNoIdentity, SearchedBy: why}
	}
	var same, other []string
	var by string
	switch db.Source.Kind {
	case "file", "backup":
		key, ok := acquisitionKey(db.Path)
		if db.Source.RemotePath == "" || db.Source.DeviceID == "" || !ok {
			return noID("an artifact of kind " + quoteCap(db.Source.Kind) + " without a device, a remote path and an acquisition")
		}
		want := db.Source.RemotePath + suffix
		by = fmt.Sprintf("device %s, remote path %s, acquisition %s", quoteCap(db.Source.DeviceID), quoteCap(want), quoteCap(key))
		for _, r := range man {
			if r.ID == db.ID || (r.Source.Kind != "file" && r.Source.Kind != "backup") {
				continue
			}
			if r.Source.DeviceID != db.Source.DeviceID || r.Source.RemotePath != want {
				continue
			}
			if k, ok := acquisitionKey(r.Path); ok && k == key {
				same = append(same, r.ID)
			} else {
				other = append(other, r.ID)
			}
		}
	case "extract", evidence.KindRecover:
		d := db.Source.Derived
		if d == nil || d.FSPath == "" || d.ParentID == "" {
			return noID("an artifact of kind " + quoteCap(db.Source.Kind) + " without a filesystem path and a parent")
		}
		want := d.FSPath + suffix
		snap := "none"
		if d.Snapshot != nil {
			snap = fmt.Sprintf("%s xid %d", quoteCap(d.Snapshot.Name), d.Snapshot.Xid)
		}
		by = fmt.Sprintf("parent %s, partition %d, snapshot %s, path %s", quoteCap(d.ParentID), d.Partition, snap, quoteCap(want))
		for _, r := range man {
			rd := r.Source.Derived
			if r.ID == db.ID || (r.Source.Kind != "extract" && r.Source.Kind != evidence.KindRecover) || rd == nil {
				continue
			}
			if rd.ParentID == d.ParentID && rd.Partition == d.Partition && sameSnapshot(rd.Snapshot, d.Snapshot) && rd.FSPath == want {
				same = append(same, r.ID)
			}
		}
	default:
		return noID("an artifact of kind " + quoteCap(db.Source.Kind) + " has no identity to search by")
	}
	switch {
	case len(same) == 1:
		return SiblingResult{Status: SiblingFound, ArtifactID: same[0], SearchedBy: by}
	case len(same) > 1:
		return SiblingResult{Status: SiblingAmbiguous, Candidates: same, SearchedBy: by}
	case len(other) > 0:
		return SiblingResult{Status: SiblingOtherAcquisition, OtherAcquisition: other, SearchedBy: by}
	}
	return SiblingResult{Status: SiblingNone, SearchedBy: by}
}
