package dbrecover

import "github.com/rbenzing/minutiae/internal/sqlitefile"

// locatorFor is the locator of a recovered row. The grammar lives in
// sqlitefile.RecoveredLocator, the one implementation every record made from a
// recovered row shares.
func locatorFor(r sqlitefile.RecoveredRow) string { return sqlitefile.RecoveredLocator(r) }
