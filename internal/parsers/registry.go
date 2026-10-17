// Package parsers is the reviewable list of compiled-in parsers and the rules a
// registry must satisfy. There is no init-time self-registration: All is a literal
// list, so adding a parser is a visible change to this package.
package parsers

import (
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidcalllog"
	"github.com/rbenzing/minutiae/internal/parsers/androidcontacts"
	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
)

// claimsEnabled turns on table claims in Meta (4G flips it): until then a parser
// declaring claims is refused.
const claimsEnabled = false

// All returns every compiled-in parser in a fixed order: 4D adds the Android telephony
// parsers, 4E and 4F append to it.
func All() []parse.Parser {
	return []parse.Parser{
		androidmms.Parser{},
		androidcalllog.Parser{},
		androidcontacts.Parser{},
	}
}
