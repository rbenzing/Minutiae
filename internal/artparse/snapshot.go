package artparse

import (
	"fmt"
	"reflect"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// caseSource is what a snapshot reads from the case.
type caseSource interface {
	Manifest() ([]evidence.ManifestRecord, error)
	ReadAudit() ([]evidence.AuditEntry, error)
}

// Snapshot is everything a run reads about the case: the manifest records, an
// id index and the device platforms, read once. Discovery and every later
// check work from it and never re-read the manifest.
type Snapshot struct {
	records []evidence.ManifestRecord
	byID    map[string]int
	env     Env
}

// TakeSnapshot reads the manifest once and the audit log once. A manifest id or
// path held by more than one record is evidence.ErrIntegrity.
func (h *Host) TakeSnapshot() (*Snapshot, error) {
	recs, err := h.src.Manifest()
	if err != nil {
		return nil, fmt.Errorf("manifest unreadable: %w", err)
	}
	audit, err := h.src.ReadAudit()
	if err != nil {
		return nil, err
	}
	s := &Snapshot{records: recs, byID: make(map[string]int, len(recs))}
	paths := make(map[string]string, len(recs))
	for i, m := range recs {
		if _, dup := s.byID[m.ID]; dup {
			return nil, fmt.Errorf("%w: artifact id %q appears more than once in the manifest", evidence.ErrIntegrity, m.ID)
		}
		if other, dup := paths[m.Path]; dup {
			return nil, fmt.Errorf("%w: artifact path %q is held by %q and %q", evidence.ErrIntegrity, m.Path, other, m.ID)
		}
		s.byID[m.ID], paths[m.Path] = i, m.ID
	}
	s.env = buildEnv(recs, audit)
	return s, nil
}

// Env is the device platform evidence of the snapshot.
func (s *Snapshot) Env() Env { return s.env }

// Record returns the snapshot's manifest record of an artifact id.
func (s *Snapshot) Record(id string) (evidence.ManifestRecord, bool) {
	i, ok := s.byID[id]
	if !ok {
		return evidence.ManifestRecord{}, false
	}
	return s.records[i], true
}

// Matches reports whether m equals the snapshot's record of the same id in id,
// path, size, SHA-256, incomplete flag and source.
func (s *Snapshot) Matches(m evidence.ManifestRecord) bool {
	w, ok := s.Record(m.ID)
	return ok && w.Path == m.Path && w.Size == m.Size && w.SHA256 == m.SHA256 &&
		w.Incomplete == m.Incomplete && reflect.DeepEqual(w.Source, m.Source)
}

// buildEnv derives the platform of each device from the case's own evidence: an
// acquire.start of type "logical" (Android) or "ios.backup" (iOS), or an info
// artifact pulled with exec:/shell: (Android) or lockdown: (iOS). A device with
// evidence for both platforms, or none, has no platform.
func buildEnv(recs []evidence.ManifestRecord, audit []evidence.AuditEntry) Env {
	seen := map[string]map[string]bool{}
	note := func(dev, platform string) {
		if dev == "" {
			return
		}
		if seen[dev] == nil {
			seen[dev] = map[string]bool{}
		}
		seen[dev][platform] = true
	}
	for _, e := range audit {
		if e.Action != "acquire.start" {
			continue
		}
		switch e.Details["type"] {
		case "logical":
			note(e.DeviceID, parse.PlatformAndroid)
		case "ios.backup":
			note(e.DeviceID, parse.PlatformIOS)
		}
	}
	for _, m := range recs {
		if m.Source.Kind != "info" {
			continue
		}
		switch rp := m.Source.RemotePath; {
		case hasAnyPrefix(rp, "exec:", "shell:"):
			note(m.Source.DeviceID, parse.PlatformAndroid)
		case hasAnyPrefix(rp, "lockdown:"):
			note(m.Source.DeviceID, parse.PlatformIOS)
		}
	}
	env := Env{DevicePlatform: map[string]string{}}
	for dev, ps := range seen {
		if len(ps) != 1 {
			continue
		}
		for p := range ps {
			env.DevicePlatform[dev] = p
		}
	}
	return env
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if len(s) >= len(p) && s[:len(p)] == p {
			return true
		}
	}
	return false
}
