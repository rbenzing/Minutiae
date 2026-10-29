package sqlitefile

// ClaimMatchSteps is ClaimMatch that also reports the fit steps it charged.
func ClaimMatchSteps(s *Schema, r RecoveredRow, claimed func(string) bool) (ClaimVerdict, int64) {
	return s.claimMatch(r, claimed)
}
