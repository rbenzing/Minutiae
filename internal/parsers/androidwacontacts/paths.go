package androidwacontacts

import "strings"

// place says where a database lies relative to the app (4I-R3).
type place int

const (
	placeLive   place = iota // the app's live databases directory: parsed
	placeBackup              // a WhatsApp backup folder: reported, never parsed
	placeOther               // anything else a live glob matched (external Android/data, an unusual prefix): never parsed as live
)

// The two packages of the app.
const (
	pkgConsumer = "com.whatsapp"
	pkgBusiness = "com.whatsapp.w4b"
)

// The app labels of raw.app.
const (
	appConsumer = "whatsapp"
	appBusiness = "whatsapp_business"
	appUnknown  = "unknown"
)

// dbPath is what the logical path of a database says about its place, its app and its Android user.
type dbPath struct {
	Place     place
	Package   string // the package directory of a LIVE or OTHER database, "" for a backup
	App       string // appConsumer, appBusiness or appUnknown: from Package, or for a backup from the external folder
	User      int    // the Android user of a LIVE database
	UserKnown bool   // true only for LIVE
}

func appOfPackage(pkg string) string {
	switch pkg {
	case pkgConsumer:
		return appConsumer
	case pkgBusiness:
		return appBusiness
	}
	return appUnknown
}

func isPackage(s string) bool { return s == pkgConsumer || s == pkgBusiness }

// userOf reads a decimal of 1 to 6 digits, no sign, no other character.
func userOf(s string) (int, bool) {
	if len(s) < 1 || len(s) > 6 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// classifyDB decides from the logical path (with or without the "android:" prefix) which place a database is in.
//
// LIVE: /data/data/<pkg>/databases/<file> (user 0), /data/user/<n>/<pkg>/databases/<file> (user n),
// /data/<pkg>/databases/<file> (user 0, a /data partition extraction) and /user/<n>/<pkg>/databases/<file> (user n,
// a partition extraction). BACKUP: <...>/WhatsApp/Databases/<file> or <...>/WhatsApp Business/Databases/<file>.
// The same classification as android.whatsapp.msgstore (4I-R3), restated here because a parser imports no other parser.
// Everything else is OTHER.
func classifyDB(logical string) dbPath {
	p := strings.TrimPrefix(logical, "android:")
	if !strings.HasPrefix(p, "/") {
		return dbPath{Place: placeOther, App: appUnknown}
	}
	el := strings.Split(p[1:], "/")
	for _, e := range el {
		if e == "" {
			return dbPath{Place: placeOther, App: appUnknown}
		}
	}
	live := func(pkg string, user int) dbPath {
		return dbPath{Place: placeLive, Package: pkg, App: appOfPackage(pkg), User: user, UserKnown: true}
	}
	switch {
	case len(el) == 5 && el[0] == "data" && el[1] == "data" && isPackage(el[2]) && el[3] == "databases":
		return live(el[2], 0)
	case len(el) == 6 && el[0] == "data" && el[1] == "user" && isPackage(el[3]) && el[4] == "databases":
		if n, ok := userOf(el[2]); ok {
			return live(el[3], n)
		}
	case len(el) == 4 && el[0] == "data" && isPackage(el[1]) && el[2] == "databases":
		return live(el[1], 0)
	case len(el) == 5 && el[0] == "user" && isPackage(el[2]) && el[3] == "databases":
		if n, ok := userOf(el[1]); ok {
			return live(el[2], n)
		}
	}
	n := len(el)
	if n >= 3 && el[n-2] == "Databases" {
		switch el[n-3] {
		case "WhatsApp":
			return dbPath{Place: placeBackup, App: appConsumer}
		case "WhatsApp Business":
			return dbPath{Place: placeBackup, App: appBusiness}
		}
	}
	if n >= 3 && el[n-2] == "databases" && isPackage(el[n-3]) {
		return dbPath{Place: placeOther, Package: el[n-3], App: appOfPackage(el[n-3])}
	}
	return dbPath{Place: placeOther, App: appUnknown}
}
