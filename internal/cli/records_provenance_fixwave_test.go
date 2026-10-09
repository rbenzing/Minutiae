package cli

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

// FB-1: a chain that could not be resolved (a missing, ambiguous, cyclic or too-deep parent) may
// derive from recovered data; its status is unknown, never live, in text and JSON alike.
func TestShowUnresolvedChainStatusIsUnknownNotLive(t *testing.T) {
	dir := damagedCase(t)
	_, out := run(t, Deps{}, "records", "show", "--case", dir, "1")
	requireLines(t, out, "    status:        unknown (provenance chain not resolved; may derive from recovered data)")
	if strings.Contains(out, "status:        live") {
		t.Errorf("an unresolved chain reads live:\n%s", out)
	}
	p, _, _, _ := showProv(t, dir, "1")
	if p.Status != "unknown" {
		t.Errorf("json status %q, want unknown", p.Status)
	}
}

// FB-1: text and JSON share one rule, row by row.
func TestProvStatusKindIsSharedByTextAndJSON(t *testing.T) {
	recovery := &records.RecoveryView{}
	for _, k := range []struct {
		name string
		p    records.Provenance
		row  records.Row
		want string
	}{
		{"resolved, live", records.Provenance{ReachedRoot: true}, records.Row{}, "live"},
		{"unresolved, no evidence of recovery", records.Provenance{}, records.Row{}, "unknown"},
		{"unresolved but a recovery view", records.Provenance{Recovery: recovery}, records.Row{}, "recovered"},
		{"unresolved but a recovered row", records.Provenance{}, records.Row{Recovered: true}, "recovered"},
		{"resolved, recovered row", records.Provenance{ReachedRoot: true}, records.Row{Recovered: true}, "recovered"},
	} {
		t.Run(k.name, func(t *testing.T) {
			if got := provenanceJSON(k.p, k.row).Status; got != k.want {
				t.Errorf("json status %q, want %q", got, k.want)
			}
			text := renderProv(k.p, k.row)
			switch k.want {
			case "live":
				requireLines(t, text, "    status:        live")
			case "unknown":
				requireLines(t, text, "    status:        unknown (provenance chain not resolved; may derive from recovered data)")
			default:
				if !strings.Contains(text, "status:        RECOVERED DATA") {
					t.Errorf("text does not read recovered:\n%s", text)
				}
			}
		})
	}
}

// FB-3: an offset state this code does not know is shown as unknown, never as nothing.
func TestShowUnknownOffsetStateIsShownAsUnknown(t *testing.T) {
	out := renderProv(records.Provenance{ReachedRoot: true, Offset: records.OffsetInfo{State: "future-state\x1b[31m"}}, records.Row{})
	requireLines(t, out, `    image offset unknown (state "future-state\x1b[31m")`)
	if strings.Contains(out, "\x1b") {
		t.Errorf("the state name is not escaped: %q", out)
	}
}

// FB-5: the text output says whether the chain reached the root, like the JSON.
func TestShowTextPrintsReachedRoot(t *testing.T) {
	requireLines(t, renderProv(records.Provenance{ReachedRoot: true}, records.Row{}), "    reached root:  yes")
	requireLines(t, renderProv(records.Provenance{}, records.Row{}), "    reached root:  no")
}
