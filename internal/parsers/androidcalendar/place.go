package androidcalendar

import "strings"

// place says where a calendar.db lies (4I3-P5, ruling B4).
type place int

const (
	placeProvider place = iota // the calendar provider's own databases directory: every refusal is loud
	placeOther                 // anywhere else (reached only through parse run --artifact or a parse plan --scan candidate)
)

// providerSuffix is the end of the logical path of the provider's database, for every Android user.
const providerSuffix = "/com.android.providers.calendar/databases/calendar.db"

// classifyPlace reads the place from the logical path alone, byte-exact.
func classifyPlace(logical string) place {
	if strings.HasSuffix(logical, providerSuffix) {
		return placeProvider
	}
	return placeOther
}
