package dbrecover

import (
	"crypto/sha256"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// locatorFor is the locator of a recovered row. The grammar lives in
// sqlitefile.RecoveredLocator, the one implementation every record made from a
// recovered row shares.
func locatorFor(r sqlitefile.RecoveredRow) string { return sqlitefile.RecoveredLocator(r) }

// LocatorDigest is the 16-byte form of a locator that Options.KeepClaimed holds:
// the first 16 bytes of its SHA-256 (4G-R25). A digest collision can only keep a
// claimed row that was not asked for; it never drops one.
func LocatorDigest(locator string) (d [16]byte) {
	s := sha256.Sum256([]byte(locator))
	copy(d[:], s[:])
	return d
}
