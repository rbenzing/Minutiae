package androidtelegram

import "strings"

// place says where a cache4.db lies (4I4-P7, the 4I3-P5 shape, ruling B4).
type place int

const (
	placeApp   place = iota // the app's files directory or one account<x> directory below it: every refusal is loud
	placeOther              // anywhere else (reached only through parse run --artifact or a parse plan --scan candidate)
)

// The path facts of the app (spec 6.1: MS:310-317): account 0 keeps files/cache4.db, account N files/account<N>/cache4.db.
const (
	appPackage    = "org.telegram.messenger"
	appFilesDir   = "/" + appPackage + "/files/"
	dbFileName    = "cache4.db"
	accountPrefix = "account"
	// maxAccountDigits is this plan's bound on the digits of an account directory (F34, limitation item 1): the app's
	// account count is small, so a longer tail is not read as a number.
	maxAccountDigits = 6
)

// The account and Android user values that are not a number (4I4-P7).
const (
	accountUndecided = "undecided" // an account<x> directory whose <x> is not 1 to 6 ASCII digits
	valueUnknown     = "unknown"   // not at the app place: the path says nothing of the account or the user
)

// classifyPlace reads the place and the account from the logical path alone, byte-exact (4I4-P7): "0" for
// files/cache4.db, the digit string as written for files/account<digits>/cache4.db (007 stays 007, F34), undecided for
// any other account<x> element, and unknown away from the app place.
func classifyPlace(logical string) (place, string) {
	i := strings.LastIndex(logical, appFilesDir)
	if i < 0 {
		return placeOther, valueUnknown
	}
	rest := logical[i+len(appFilesDir):]
	if rest == dbFileName {
		return placeApp, "0"
	}
	dir, file, ok := strings.Cut(rest, "/")
	if !ok || file != dbFileName || !strings.HasPrefix(dir, accountPrefix) {
		return placeOther, valueUnknown
	}
	if n := dir[len(accountPrefix):]; isDigits(n, maxAccountDigits) {
		return placeApp, n
	}
	return placeApp, accountUndecided
}

// databaseUser is the Android user whose app data holds the database (4I4-P7): 0 under /data/data/<package>/, n under
// /data/user/<n>/<package>/ (n of 1 to 6 ASCII digits, as written), else unknown.
func databaseUser(logical string) string {
	p := strings.TrimPrefix(logical, "android:")
	if strings.HasPrefix(p, "/data/data/"+appPackage+"/") {
		return "0"
	}
	rest, ok := strings.CutPrefix(p, "/data/user/")
	if !ok {
		return valueUnknown
	}
	n, tail, ok := strings.Cut(rest, "/")
	if !ok || !isDigits(n, maxAccountDigits) || !strings.HasPrefix(tail, appPackage+"/") {
		return valueUnknown
	}
	return n
}

// isDigits says s is 1 to max ASCII digits, no sign, no other character (the check of androidwhatsapp's userOf,
// restated: parsers import no other parser).
func isDigits(s string, maxLen int) bool {
	if len(s) < 1 || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
