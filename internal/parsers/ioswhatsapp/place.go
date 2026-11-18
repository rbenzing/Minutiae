package ioswhatsapp

import "strings"

// place says where a ChatStorage.sqlite was found (plan 4I2 "Globs and places", 4I2-P2): in the backup domain of one of
// the two apps, in an extracted app group container, or nowhere the globs name. The zero value is placeOther, so an
// unclassified path is never trusted as WhatsApp's.
type place int

const (
	placeOther place = iota
	placeDomain
	placeAppGroup
)

// The backup domains of the two apps (spec 6.11; the Business domain rests on a single source, see docs/limitations.md).
const (
	domainConsumer = "AppDomainGroup-group.net.whatsapp.WhatsApp.shared"
	domainBusiness = "AppDomainGroup-group.net.whatsapp.WhatsAppSMB.shared"
)

const (
	dbName       = "ChatStorage.sqlite"
	appGroupRoot = "ios:HomeDomain/Containers/Shared/AppGroup/"
)

// dbPlace is the classification of a primary's logical path.
type dbPlace struct {
	Place place
	// App is "whatsapp" or "whatsapp_business" for placeDomain (the domain names the app, spec 6.8), "" otherwise.
	App string
	// Container is the directory of the primary's logical path: the backup domain ("ios:<domain>") or the app group
	// container ("ios:HomeDomain/Containers/Shared/AppGroup/<id>").
	Container string
}

// classifyPlace restates the three primary globs: the database sits directly in the domain or directly in an app group
// container, never deeper, and a companion (-wal, -journal, -shm) is never a primary (4I-R21).
func classifyPlace(logical string) dbPlace {
	for _, d := range []struct{ domain, app string }{{domainConsumer, "whatsapp"}, {domainBusiness, "whatsapp_business"}} {
		if logical == "ios:"+d.domain+"/"+dbName {
			return dbPlace{Place: placeDomain, App: d.app, Container: "ios:" + d.domain}
		}
	}
	rest, ok := strings.CutPrefix(logical, appGroupRoot)
	if !ok {
		return dbPlace{}
	}
	id, ok := strings.CutSuffix(rest, "/"+dbName)
	if !ok || id == "" || strings.Contains(id, "/") {
		return dbPlace{}
	}
	return dbPlace{Place: placeAppGroup, Container: appGroupRoot + id}
}
