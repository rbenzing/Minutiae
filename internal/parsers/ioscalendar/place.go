package ioscalendar

// place says where a Calendar.sqlitedb lies (4I3-P5, ruling B4).
type place int

const (
	placeSystem place = iota // the system calendar path: the path names the store, so every refusal is loud
	placeOther               // anywhere else (reached only through parse run --artifact or a parse plan --scan candidate)
)

// systemPath is the logical path of the system calendar store, in a backup (HomeDomain) and in an extraction alike
// (the extract namer maps /private/var/mobile/... to ios:HomeDomain/..., artparse/logical.go).
const systemPath = "ios:HomeDomain/Library/Calendar/Calendar.sqlitedb"

// classifyPlace reads the place from the logical path alone, byte-exact.
func classifyPlace(logical string) place {
	if logical == systemPath {
		return placeSystem
	}
	return placeOther
}
