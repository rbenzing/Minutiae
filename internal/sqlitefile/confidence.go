package sqlitefile

// Confidence is the rank of a recovered row (spec 3, section 3.3 and the D2
// basis tiers): an ordinal 0..100, never a probability. The method sets a base;
// the table basis caps it (fit 60, guess 40, none 30; schema no cap) and the
// lowest applies. It is a pure function of Method and TableBasis: no value,
// location or note of the row changes it. An unknown method is 0. Callers apply
// the artifact's confidence on top with CapConfidence.
func Confidence(r RecoveredRow) int {
	base, ok := methodBase[r.Method]
	if !ok {
		return 0
	}
	return min(base, basisCap(r.TableBasis))
}

// methodBase is the base confidence of each recovery method.
var methodBase = map[string]int{
	MethodWALPrior:          75,
	MethodJournalBefore:     70,
	MethodFreelist:          60,
	MethodWALStale:          55,
	MethodWALUncommitted:    45,
	MethodJournalRolledBack: 45,
	MethodJournalPersist:    35,
	MethodPageSlack:         25,
}

// basisCap is the ceiling a table basis puts on a row. A basis the rule does
// not know is capped like none: a row is never ranked above what is proven.
func basisCap(b TableBasis) int {
	switch b {
	case BasisSchema:
		return 100
	case BasisFit:
		return 60
	case BasisGuess:
		return 40
	}
	return 30
}

// Band names a confidence: "high" (70 and up), "medium" (40 to 69) or "low".
func Band(c int) string {
	switch {
	case c >= 70:
		return "high"
	case c >= 40:
		return "medium"
	}
	return "low"
}

// CapConfidence applies an artifact's confidence as a ceiling: min(c, *artifact),
// and c itself when artifact is nil. It never raises c.
func CapConfidence(c int, artifact *int) int {
	if artifact == nil {
		return c
	}
	return min(c, *artifact)
}
