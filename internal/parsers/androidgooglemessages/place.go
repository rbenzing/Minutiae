package androidgooglemessages

import "strings"

// place says where a bugle_db lies (spec 5.2, the 4I3/4I4 places rule).
type place int

const (
	placeApp   place = iota // <package>/databases/bugle_db of one of the two packages: every refusal is loud
	placeOther              // anywhere else (reached only through parse run --artifact or a parse plan --scan candidate)
)

// The packages of the app and the app labels of raw.app and the note app (spec 5.2).
const (
	pkgGoogle      = "com.google.android.apps.messaging"
	pkgAOSP        = "com.android.messaging"
	appGoogle      = "google_messages"
	appAOSP        = "aosp_messaging"
	appUnknown     = "unknown"
	dbFileName     = "bugle_db"
	userUndecided  = "undecided" // the note database_user of a path that does not say (4I R5 shape)
	maxUserDigits  = 6           // the digits of an Android user directory
	databasesDir   = "databases"
	dataDataPrefix = "/data/data/"
	dataUserPrefix = "/data/user/"
)

func appOfPackage(pkg string) (string, bool) {
	switch pkg {
	case pkgGoogle:
		return appGoogle, true
	case pkgAOSP:
		return appAOSP, true
	}
	return appUnknown, false
}

// classifyPlace reads the place and the app label from the logical path alone (with or without the "android:"
// prefix): placeApp when it ends in <package>/databases/bugle_db for one of the two packages, else placeOther with the
// label unknown.
func classifyPlace(logical string) (place, string) {
	p := strings.TrimPrefix(logical, "android:")
	el := strings.Split(p, "/")
	n := len(el)
	if n < 3 || el[n-1] != dbFileName || el[n-2] != databasesDir {
		return placeOther, appUnknown
	}
	if app, ok := appOfPackage(el[n-3]); ok {
		return placeApp, app
	}
	return placeOther, appUnknown
}

// databaseUser is the Android user whose app data holds the database (spec 5.2): 0 under /data/data/<package>/, n under
// /data/user/<n>/<package>/ (n of 1 to 6 ASCII digits, as written), else undecided.
func databaseUser(logical string) string {
	p := strings.TrimPrefix(logical, "android:")
	if rest, ok := strings.CutPrefix(p, dataDataPrefix); ok {
		pkg, _, _ := strings.Cut(rest, "/")
		if _, known := appOfPackage(pkg); known {
			return "0"
		}
		return userUndecided
	}
	rest, ok := strings.CutPrefix(p, dataUserPrefix)
	if !ok {
		return userUndecided
	}
	n, tail, ok := strings.Cut(rest, "/")
	if !ok || !isDigits(n, maxUserDigits) {
		return userUndecided
	}
	pkg, _, _ := strings.Cut(tail, "/")
	if _, known := appOfPackage(pkg); !known {
		return userUndecided
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
