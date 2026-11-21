package androidgooglemessages_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
)

// Spec 10, 12: every committed variant is parsed at the app path with a stated outcome: a count of messages records and a
// warning, or the typed refusal. The table lists exactly the variants of the fixture list, so a new variant is not forgotten.
func TestParserCoversEveryVariant(t *testing.T) {
	type outcome struct {
		messages int    // messages records
		warn     string // a warning every run of the variant carries ("" none required)
		err      error  // the typed error, nil for a run that parses
		anyErr   bool   // the file is no database: no records, whatever the typed outcome
	}
	want := map[string]outcome{
		"gm-aosp-v2":            {messages: 36, warn: "googlemessages-direction-conflict"},
		"gm-aosp-v1":            {messages: 3},
		"gm-aosp-v2-wal":        {messages: 3, warn: "wal-frames-not-applied"},
		"gm-journal":            {messages: 2, warn: "journal-hot"},
		"gm-proprietary-shaped": {messages: 3, warn: "googlemessages-schema-uncited"},
		"gm-other-kind":         {err: androidgooglemessages.ErrNoSignature},
		"gm-not-sqlite":         {anyErr: true},
		"gm-no-members":         {messages: 3, warn: "googlemessages-join-undecided: conversation_participants"},
		"gm-deleted":            {messages: 2},
		"gm-rebuilt":            {messages: 2},
		"gm-hostile":            {messages: 16, warn: "googlemessages-members-cut"},
	}
	for _, v := range variants {
		o, ok := want[v]
		if !ok {
			t.Errorf("variant %s has no stated outcome", v)
			continue
		}
		t.Run(v, func(t *testing.T) {
			res := parseAt(t, remoteApp, fixtureFiles(t, v))
			switch {
			case o.anyErr:
				if res.Err == nil && len(res.Records) != 0 {
					t.Errorf("%d records from a file that is no database", len(res.Records))
				}
			case o.err != nil:
				if !errors.Is(res.Err, o.err) || len(res.Records) != 0 {
					t.Errorf("err %v, %d records, want %v and none", res.Err, len(res.Records), o.err)
				}
			default:
				if res.Err != nil {
					t.Fatalf("Parse: %v", res.Err)
				}
				if got := len(byRowid(t, res)); got != o.messages {
					t.Errorf("%d messages records, want %d", got, o.messages)
				}
				if o.warn != "" && len(warningsWith(res, o.warn)) == 0 {
					t.Errorf("no warning with %q: %v", o.warn, res.Warnings)
				}
			}
		})
	}
	if len(want) != len(variants) {
		t.Errorf("%d stated outcomes for %d variants", len(want), len(variants))
	}
}
