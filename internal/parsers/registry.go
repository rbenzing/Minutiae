// Package parsers is the reviewable list of compiled-in parsers and the rules a
// registry must satisfy. There is no init-time self-registration: All is a literal
// list, so adding a parser is a visible change to this package.
package parsers

import (
	"github.com/rbenzing/minutiae/internal/parse"
)

// claimsEnabled turns on table claims in Meta (4G flips it): until then a parser
// declaring claims is refused.
const claimsEnabled = false

// All returns every compiled-in parser in a fixed order. The list is empty in 4A;
// 4D to 4F append to it.
func All() []parse.Parser {
	return []parse.Parser{}
}
