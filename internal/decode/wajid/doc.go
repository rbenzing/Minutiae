// Package wajid splits a WhatsApp JID as it is stored. It never normalizes a JID, never
// composes "user@server" from two columns and never derives a phone number: the user part,
// the server and the optional agent and device suffixes are substrings of the input, and the
// server class is decided by an exact, case-sensitive comparison. A string that does not split
// is a typed error, not an empty JID.
package wajid
