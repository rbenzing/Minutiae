package artparse

import (
	"sort"

	"github.com/rbenzing/minutiae/internal/records"
)

// bundleRecovery is what the emitter knows about the recovered members of a job's bundle. A job whose
// bundle holds a recovered member describes nothing live: a live -wal read over a recovered database
// describes a state the database may never have had. So every record of the job is stamped recovered
// and capped by the LOWEST confidence of any recovered member, and a cap that cannot be read is 25.
type bundleRecovery struct {
	members      map[string]artInfo // the recovered members by artifact id
	recoveredIDs []string           // their ids, sorted
	mixed        bool               // live members too
	cap          *int               // the lowest cap of any recovered member; nil when none is known
	capUnknown   bool               // some recovered member's cap cannot be read (cap is then at most 25)
	owner        string             // the recovered member that bounds the cap (lowest, then lowest id): describes the records on live members
}

// newBundleRecovery returns nil when no member is recovered.
func newBundleRecovery(arts map[string]artInfo) *bundleRecovery {
	b := &bundleRecovery{members: map[string]artInfo{}}
	for id, a := range arts {
		if a.recovery != nil {
			b.members[id] = a
			b.recoveredIDs = append(b.recoveredIDs, id)
		}
	}
	if len(b.members) == 0 {
		return nil
	}
	sort.Strings(b.recoveredIDs)
	b.mixed = len(b.members) < len(arts)
	for _, id := range b.recoveredIDs {
		ri := b.members[id].recovery
		if ri.Confidence != nil {
			b.cap = lower(b.cap, ri.Confidence)
		}
		if ri.CapUnknown {
			b.capUnknown = true
			c := capUnknown
			b.cap = lower(b.cap, &c)
		}
		if b.owner == "" || b.effective(id) < b.effective(b.owner) {
			b.owner = id
		}
	}
	return b
}

// effective is the bound a recovered member puts on the bundle (a large number when none is known).
func (b *bundleRecovery) effective(id string) int {
	ri := b.members[id].recovery
	eff := noBound
	if ri.Confidence != nil {
		eff = *ri.Confidence
	}
	if ri.CapUnknown {
		eff = min(eff, capUnknown)
	}
	return eff
}

// stamp marks record c, which names the artifact described by info, as recovered: Deleted, the method of
// the recovered artifact it comes from, the bundle's cap, and payload.recovery. A record on a recovered
// artifact says from-recovered-artifact; a record on a live member of a bundle with a recovered one says
// from-recovered-bundle and names that member. A relation the parser already set stays in relation and
// the host's goes to artifact_relation.
func (b *bundleRecovery) stamp(c *records.Record, info artInfo) {
	src, srcID, relation := info.recovery, info.id, relationFromRec
	if src == nil {
		src, srcID, relation = b.members[b.owner].recovery, b.owner, relationFromBundle
	}
	c.Deleted = true
	c.Recovery = src.Method
	c.Confidence = lower(c.Confidence, b.cap)
	if c.Payload == nil {
		c.Payload = map[string]any{}
	}
	rec, _ := c.Payload["recovery"].(map[string]any)
	if rec == nil {
		rec = map[string]any{}
	}
	rec["artifact_method"] = src.Method
	if src.Confidence != nil {
		rec["artifact_confidence"] = *src.Confidence
	}
	if _, own := rec["relation"]; own {
		rec["artifact_relation"] = relation
	} else {
		rec["relation"] = relation
	}
	if info.recovery == nil {
		rec["bundle_member"] = srcID
		addNote(rec, noteMixedBundle)
	}
	if b.capUnknown {
		addNote(rec, noteCapUnknown)
	}
	c.Payload["recovery"] = rec
}

// addNote appends note to payload.recovery.notes once.
func addNote(rec map[string]any, note string) {
	notes, _ := rec["notes"].([]any)
	for _, n := range notes {
		if n == note {
			return
		}
	}
	rec["notes"] = append(notes, note)
}

// noBound is the bound of a recovered member that puts none on the bundle.
const noBound = 1 << 30
