package dbrecover

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// ParserName is the parser name of every record this package writes.
const ParserName = "sqlite-recover"

// Version is the parser version. After 3J releases, any change to the rows
// this package delivers bumps it, also one the rules hash does not cover.
const Version = "1.0.0"

// mappingRules is the version of the row-mapping rules the hash covers.
const mappingRules = "1"

// version is what ParserIdentity reports; only export_test.go replaces it.
var version = Version

// Bounds of a delivered row (plan decision D12); defaults, ceilings and the
// fixed text sizes are part of the rules hash.
const (
	DefaultMaxPayloadBytes = 8 << 20
	MaxPayloadBytesCeiling = 12 << 20
	DefaultMaxBodyBytes    = 256 << 10
	MaxBodyBytesCeiling    = 1 << 20
	SummaryBytes           = 512
	LocatorNameBytes       = sqlitefile.LocatorNameBytes
)

var methods = []string{
	sqlitefile.MethodFreelist, sqlitefile.MethodJournalBefore, sqlitefile.MethodJournalPersist,
	sqlitefile.MethodJournalRolledBack, sqlitefile.MethodPageSlack, sqlitefile.MethodWALPrior,
	sqlitefile.MethodWALStale, sqlitefile.MethodWALUncommitted,
}

var bases = []sqlitefile.TableBasis{sqlitefile.BasisFit, sqlitefile.BasisGuess, sqlitefile.BasisNone, sqlitefile.BasisSchema}

// Methods returns the eight recovery method tokens of sqlitefile, sorted.
func Methods() []string {
	out := slices.Clone(methods)
	slices.Sort(out)
	return out
}

// RulesHash is the hex SHA-256 over Version, the mapping rules version, the
// confidence of every method x basis and the D12 bounds. It covers the
// confidence table and the mapping constants only; a change to delivered rows
// that touches neither still needs a Version bump.
func RulesHash() string { return rulesHashFor(sqlitefile.Confidence) }

func rulesHashFor(conf func(sqlitefile.RecoveredRow) int) string {
	h := sha256.New()
	put := func(k, v string) { h.Write([]byte(k + "=" + v + "\n")) }
	put("version", version)
	put("mapping", mappingRules)
	for _, m := range Methods() {
		for _, b := range bases {
			put("conf:"+m+":"+string(b), strconv.Itoa(conf(sqlitefile.RecoveredRow{Method: m, TableBasis: b})))
		}
	}
	for _, c := range []struct {
		k string
		v int
	}{
		{"payload-default", DefaultMaxPayloadBytes},
		{"payload-ceiling", MaxPayloadBytesCeiling},
		{"body-default", DefaultMaxBodyBytes},
		{"body-ceiling", MaxBodyBytesCeiling},
		{"summary", SummaryBytes},
		{"locator-name", LocatorNameBytes},
	} {
		put(c.k, strconv.Itoa(c.v))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ParserIdentity is the identity the writer records for this parser.
func ParserIdentity() records.Parser {
	return records.Parser{Name: ParserName, Version: version, Hash: RulesHash()}
}
