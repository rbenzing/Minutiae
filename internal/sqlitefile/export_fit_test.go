package sqlitefile

// SetFitLimit sets the per-page step cap of s (Limits.MaxFitSteps in use).
func SetFitLimit(s *Schema, n int64) { s.maxFit = n }

// FitExamined is FitTables that also reports how many tables it examined.
func FitExamined(s *Schema, values []Value, tier FitTier) (names []string, examined int) {
	return s.fitTablesCount(values, tier)
}

// FitBuildSteps builds the fit index of s (if not yet built) and returns the
// steps the build took.
func FitBuildSteps(s *Schema) int64 { s.fitSets(); return s.fitSteps }
