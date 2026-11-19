// Package all links the payload contract of every record type that has one: it
// blank-imports message, call, contact, web and calendar, whose init functions
// install the validators of the seven validated types (message, call, contact,
// web_visit, web_search, download, calendar_event). It is the one place that lists
// the contract packages: a binary that wants payload validation (the CLI) imports
// this package, and a package that must stay validator-free (the records tests)
// does not.
//
// Importing the package from several places is safe: Go runs its init once, so
// each validator is registered exactly once.
package all

import (
	_ "github.com/rbenzing/minutiae/internal/recordtypes/calendar" // registers the calendar_event validator
	_ "github.com/rbenzing/minutiae/internal/recordtypes/call"     // registers the call validator
	_ "github.com/rbenzing/minutiae/internal/recordtypes/contact"  // registers the contact validator
	_ "github.com/rbenzing/minutiae/internal/recordtypes/message"  // registers the message validator
	_ "github.com/rbenzing/minutiae/internal/recordtypes/web"      // registers the web validator
)
