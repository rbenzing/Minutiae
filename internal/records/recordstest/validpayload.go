package recordstest

// ValidPayload returns a fresh, minimal payload that satisfies the validator of
// the given record type, for tests that need a record of that type but do not care
// what is in it. It is plain data: this package never imports the record-type
// packages, so a test binary that links only records and recordstest stays free
// of validators, and the values here follow the payload contracts (plan 4A, F10)
// that those validators enforce. A test that links recordtypes/all proves the two
// agree (TestValidPayloadPassesEveryValidator).
//
// The six validated types are message, call, contact, web_visit, web_search and
// download; any other type name gets an empty map. Every call returns a new map
// (and new nested values), so a test may add keys to it freely.
func ValidPayload(typ string) map[string]any {
	switch typ {
	case "message":
		return map[string]any{
			"channel": "sms", "direction": "unknown", "kind": "unknown",
			"participants": []any{}, "participants_unknown": true,
		}
	case "call":
		return map[string]any{"direction": "unknown", "outcome": "unknown"}
	case "contact":
		return map[string]any{"display_name": "x"}
	case "web_visit":
		return map[string]any{"url": "https://example.test/", "browser": "unknown-chromium"}
	case "web_search":
		return map[string]any{"term": "x"}
	case "download":
		return map[string]any{"url": "https://example.test/f"}
	}
	return map[string]any{}
}
