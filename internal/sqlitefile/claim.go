package sqlitefile

// ClaimVerdict says whether a recovered row belongs to a claim and, when not, why.
type ClaimVerdict string

// The verdicts of ClaimMatch, in the order the checks run.
const (
	ClaimYes        ClaimVerdict = "claimed"
	ClaimIndexEntry ClaimVerdict = "index-entry"
	ClaimNotClaimed ClaimVerdict = "not-claimed"
	ClaimBasis      ClaimVerdict = "basis-not-claimable"
	ClaimNoLiveFit  ClaimVerdict = "no-live-fit"
	ClaimFitLimit   ClaimVerdict = "fit-limit"
)

// ClaimMatch is the one predicate that says a recovered row belongs to a claim;
// the generic recovery run (its exclusion of claimed tables) and the typed pass
// both use it, so the two can never disagree about which row belongs to whom.
// s is the LIVE schema and claimed reports whether the named table is claimed
// (the caller compares names ASCII case-insensitively, as SQLite does).
//
// A row is claimed (ClaimYes) only when it is a table row (not an index entry),
// its table is claimed, its basis is schema or fit (never guess or none), and
// its values fit the LIVE definition of that table at the strict tier. A row
// carved from page slack is never claimed, whatever its basis (ClaimNotClaimed):
// it stays with the generic run. A schema-basis row that no longer fits today's
// definition (a dropped and recreated table) is ClaimNoLiveFit, so it is never
// read through the wrong columns; a fit that cannot be afforded (the schema's
// step cap Limits.MaxFitSteps) is ClaimFitLimit, which is not a claim.
//
// Only the labelled table's definition is examined: the cost is its column
// count plus one, whatever the size of the schema.
func (s *Schema) ClaimMatch(r RecoveredRow, claimed func(table string) bool) ClaimVerdict {
	v, _ := s.claimMatch(r, claimed)
	return v
}

// claimMatch is ClaimMatch that also returns the fit steps it charged.
func (s *Schema) claimMatch(r RecoveredRow, claimed func(string) bool) (ClaimVerdict, int64) {
	switch {
	case r.Method == MethodPageSlack || r.SlackRegion != "":
		return ClaimNotClaimed, 0
	case r.Index != "":
		return ClaimIndexEntry, 0
	case r.Table == "" || claimed == nil || !claimed(r.Table):
		return ClaimNotClaimed, 0
	case r.TableBasis != BasisSchema && r.TableBasis != BasisFit:
		return ClaimBasis, 0
	}
	it := s.claimItem(r.Table)
	if it == nil {
		return ClaimNoLiveFit, 0
	}
	cost := it.cost()
	if s.fitLimited || cost > s.fitCap() {
		return ClaimFitLimit, 0
	}
	if !it.matches(r.Values, FitStrict) {
		return ClaimNoLiveFit, cost
	}
	return ClaimYes, cost
}

// claimItem is the fit item of the live table of that name (ASCII fold), nil
// when the schema holds no table of it the fit can name. The name map is built
// once from the fit index.
func (s *Schema) claimItem(name string) *fitItem {
	s.claimOnce.Do(func() {
		items := s.fitSets().tables.items
		s.claimByName = make(map[string]*fitItem, len(items))
		for i := range items {
			k := asciiLower(items[i].name)
			if _, dup := s.claimByName[k]; !dup {
				s.claimByName[k] = &items[i]
			}
		}
	})
	return s.claimByName[asciiLower(name)]
}
