package artparse

import (
	"sort"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// recoveryTrail is evidence.RecoveryTrailOf over the snapshot's records: ok is true when the artifact, or
// an ancestor on its derivation chain, carries a recovery description (or is of a recovered kind).
func (s *Snapshot) recoveryTrail(id string) (evidence.RecoveryTrail, bool) {
	s.byRecOnce.Do(func() {
		s.byRec = make(map[string]evidence.ManifestRecord, len(s.records))
		for _, r := range s.records {
			s.byRec[r.ID] = r
		}
	})
	return evidence.RecoveryTrailOf(s.byRec, id)
}

// unknownMethod names the method of an artifact that is recovered but whose description holds none:
// recovered-ness is kept and the missing method is stated, never guessed.
const unknownMethod = "unknown"

// recoveryInfoOf is the parse.RecoveryInfo of a trail: the class and method of the nearest recovered
// artifact and the lowest confidence of the whole chain. CapUnknown says that the cap cannot be read:
// the chain is cut (a missing parent, a cycle, the depth bound) or no link of it carries a confidence.
func recoveryInfoOf(tr evidence.RecoveryTrail) *parse.RecoveryInfo {
	ri := &parse.RecoveryInfo{CapUnknown: tr.Cut || tr.MinConfidence == nil}
	if tr.Recovery != nil {
		ri.Class, ri.Method = tr.Recovery.Class, tr.Recovery.Method
	}
	if ri.Method == "" {
		ri.Method = unknownMethod
	}
	if tr.MinConfidence != nil {
		c := *tr.MinConfidence
		ri.Confidence = &c
	}
	return ri
}

// recoveredInput reports the recovery description of artifact id in the snapshot, or nil when its chain
// holds none (live evidence).
func (s *Snapshot) recoveredInput(id string) *parse.RecoveryInfo {
	tr, ok := s.recoveryTrail(id)
	if !ok {
		return nil
	}
	return recoveryInfoOf(tr)
}

// jobRecovery is the recovery description a plan shows for a job: that of the primary when it is
// recovered input, otherwise of the first recovered companion (by role); nil for a live job.
func jobRecovery(snap *Snapshot, j Job) *parse.RecoveryInfo {
	if ri := snap.recoveredInput(j.Primary.Artifact.ID); ri != nil {
		return ri
	}
	roles := make([]string, 0, len(j.Others))
	for role := range j.Others {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if ri := snap.recoveredInput(j.Others[role].Artifact.ID); ri != nil {
			return ri
		}
	}
	return nil
}
