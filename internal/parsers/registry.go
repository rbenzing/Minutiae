// Package parsers is the reviewable list of compiled-in parsers and the rules a
// registry must satisfy. There is no init-time self-registration: All is a literal
// list, so adding a parser is a visible change to this package.
package parsers

import (
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidcalllog"
	"github.com/rbenzing/minutiae/internal/parsers/androidcontacts"
	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/iosaddressbook"
	"github.com/rbenzing/minutiae/internal/parsers/ioscalls"
	"github.com/rbenzing/minutiae/internal/parsers/safari"
)

// claimsEnabled allows table claims in Meta (plan 4G). A claiming parser also needs a
// generated recovery hash (the "<name>.recover" identity).
const claimsEnabled = true

// All returns every compiled-in parser in a fixed order: 4D adds the Android telephony
// parsers, 4E and 4F append to it.
func All() []parse.Parser {
	return []parse.Parser{
		androidmms.Parser{},
		androidcalllog.Parser{},
		androidcontacts.Parser{},
		ioscalls.Parser{},
		iosaddressbook.Parser{},
		chromium.Parser{},
		safari.Parser{},
	}
}
