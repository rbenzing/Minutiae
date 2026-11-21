package androidgooglemessages

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fact is one row of tools/fixtures/schemas/google-messages/facts.tsv.
type fact struct{ key, value, constant, contract, pin, source string }

func loadFacts(t *testing.T) []fact {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "google-messages", "facts.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	var out []fact
	for i, ln := range strings.Split(string(b), "\n") {
		if i == 0 || ln == "" {
			continue
		}
		f := strings.Split(ln, "\t")
		for len(f) < 7 {
			f = append(f, "")
		}
		out = append(out, fact{f[0], f[1], f[2], f[3], f[4], f[5]})
	}
	if len(out) == 0 {
		t.Fatal("facts.tsv has no rows")
	}
	return out
}

// codeProblems lists every way table differs from the facts of prefix (message_protocol or message_status): a fact
// with no Go entry, an entry with no fact, a label that is not labelOf(constant), another contract or another source.
func codeProblems(prefix string, table map[int64]code, facts []fact) []string {
	var out []string
	seen := map[int64]bool{}
	for _, f := range facts {
		if !strings.HasPrefix(f.key, prefix+".") {
			continue
		}
		v, err := strconv.ParseInt(f.value, 10, 64)
		if err != nil {
			out = append(out, f.key+": the value is not an integer")
			continue
		}
		seen[v] = true
		c, ok := table[v]
		switch {
		case !ok:
			out = append(out, f.key+": no Go entry")
		case c.Label != labelOf(f.constant):
			out = append(out, f.key+": label "+c.Label+", want "+labelOf(f.constant))
		case c.Contract != f.contract:
			out = append(out, f.key+": contract "+c.Contract+", want "+f.contract)
		case c.Source != f.source+"@"+f.pin:
			out = append(out, f.key+": source "+c.Source+", want "+f.source+"@"+f.pin)
		}
	}
	for v := range table {
		if !seen[v] {
			out = append(out, prefix+"."+strconv.FormatInt(v, 10)+": a Go entry with no fact")
		}
	}
	return out
}

func clone(m map[int64]code) map[int64]code {
	out := make(map[int64]code, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Spec 6.1, 6.2, 4I5-P4: the Go code tables equal facts.tsv (label, contract, source), and a table that drifts is
// reported, in each of the ways the check names.
func TestGoogleMessagesCodeTableMatchesFacts(t *testing.T) {
	facts := loadFacts(t)
	for prefix, table := range map[string]map[int64]code{"message_protocol": protocolCodes, "message_status": statusCodes} {
		if got := codeProblems(prefix, table, facts); len(got) != 0 {
			t.Errorf("%s: %v", prefix, got)
		}
		if len(table) == 0 {
			t.Errorf("%s: an empty table", prefix)
		}
	}
	if labelOf("PROTOCOL_MMS_PUSH_NOTIFICATION") != "mms_push_notification" || labelOf("BUGLE_STATUS_INCOMING_COMPLETE") != "incoming_complete" ||
		labelOf("PROTOCOL_UNKNOWN") != "unknown" || labelOf("BUGLE_STATUS_UNKNOWN") != "unknown" {
		t.Error("labelOf does not strip the prefix and lowercase")
	}
	// planted drift is reported
	extra := clone(statusCodes)
	extra[55] = code{Label: "planted", Contract: "out/sent", Source: "MessageData.java:1@M"}
	if got := codeProblems("message_status", extra, facts); len(got) != 1 || !strings.Contains(got[0], "a Go entry with no fact") {
		t.Errorf("a planted extra entry gave %v", got)
	}
	changed := clone(statusCodes)
	c := changed[1]
	c.Contract = "in/received"
	changed[1] = c
	if got := codeProblems("message_status", changed, facts); len(got) != 1 || !strings.Contains(got[0], "contract") {
		t.Errorf("a planted contract change gave %v", got)
	}
	missing := clone(protocolCodes)
	delete(missing, 2)
	if got := codeProblems("message_protocol", missing, facts); len(got) != 1 || !strings.Contains(got[0], "no Go entry") {
		t.Errorf("a planted missing entry gave %v", got)
	}
}
