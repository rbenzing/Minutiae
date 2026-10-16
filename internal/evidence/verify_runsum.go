package evidence

import "fmt"

const rKindRunSum = "run sum"

// checkRunSums is rule R8: a COMPLETE derived artifact that is not of a recovered kind (R4 judges
// those) holds exactly the bytes its recorded runs name, so the lengths of its runs add up to its
// size. A hole counts for the bytes it stands for. No recorded runs is unknown, not damage; an
// incomplete artifact may legitimately record runs beyond the bytes it holds.
func (c *Case) checkRunSums(ps *problemSet, uniq []ManifestRecord, byID map[string]ManifestRecord) {
	for _, r := range uniq {
		d := r.Source.Derived
		if d == nil || IsRecoveredKind(r.Source.Kind) || r.Source.Kind == "runs" || r.Incomplete {
			continue
		}
		pre := fmt.Sprintf("artifact %q (%q): ", r.ID, r.Path)
		runs, err := c.DerivedRuns(r, byID)
		if err != nil {
			ps.add(rKindRuns, "%sruns unreadable: %v", pre, err)
			continue
		}
		if len(runs) == 0 {
			continue
		}
		sum, err := RunsSum(runs)
		switch {
		case err != nil:
			ps.add(rKindRunSum, "%s%v", pre, err)
		case sum != r.Size:
			ps.add(rKindRunSum, "%sruns cover %d bytes but the artifact holds %d", pre, sum, r.Size)
		}
	}
}
