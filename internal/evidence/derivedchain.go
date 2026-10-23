package evidence

import (
	"fmt"
	"slices"
	"strings"
)

// maxCycleMembersListed bounds the member ids a cycle problem names.
const maxCycleMembersListed = 8

// chainResult is the outcome of walking one artifact's parent chain.
type chainResult struct {
	TooDeep bool     // more than maxDerivedDepth hops
	Cycle   []string // the ids ON the cycle, sorted; nil when the chain ends
}

// derivedChainWalk follows Source.Derived.ParentID from id. A missing parent or a recorded parent hash
// that differs ends the walk without a finding (checkDerived reports that hop itself, ruling R14).
func derivedChainWalk(byID map[string]ManifestRecord, id string) chainResult {
	order := []string{id}
	at := map[string]int{id: 0}
	cur := id
	for hops := 1; ; hops++ {
		r, ok := byID[cur]
		if !ok || r.Source.Derived == nil {
			return chainResult{}
		}
		parent := r.Source.Derived.ParentID
		if i, seen := at[parent]; seen {
			cycle := slices.Clone(order[i:])
			slices.Sort(cycle)
			return chainResult{Cycle: cycle}
		}
		if hops > maxDerivedDepth {
			return chainResult{TooDeep: true}
		}
		at[parent] = len(order)
		order = append(order, parent)
		cur = parent
	}
}

// cycleGroup is one distinct cycle with the artifacts whose chain reaches it.
type cycleGroup struct {
	members []string
	further int // artifacts that derive into the cycle without being on it
}

// cycleProblem is the single problem a cycle gets.
func cycleProblem(g *cycleGroup) string {
	listed := g.members
	more := ""
	if len(listed) > maxCycleMembersListed {
		more = fmt.Sprintf(" and %d more", len(listed)-maxCycleMembersListed)
		listed = listed[:maxCycleMembersListed]
	}
	quoted := make([]string, len(listed))
	for i, m := range listed {
		quoted[i] = fmt.Sprintf("%q", m)
	}
	return fmt.Sprintf("derived chain has a cycle of %d artifacts (%s%s) and %d further artifacts derive into it",
		len(g.members), strings.Join(quoted, ", "), more, g.further)
}
