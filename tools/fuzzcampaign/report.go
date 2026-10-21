package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// safe renders untrusted text for a terminal: control characters, bidirectional and other
// format controls and line breaks become \uXXXX escapes, so a note can never move the cursor,
// recolour the screen or reorder the line.
func safe(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			sb.WriteString(`\\`)
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ':
			fmt.Fprintf(&sb, `\u%04x`, r)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// Summary renders one line per target, in plan order, and the totals.
func Summary(rs []RunResult) string {
	var sb strings.Builder
	counts := map[Outcome]int{}
	for _, r := range rs {
		counts[r.Outcome]++
		fmt.Fprintf(&sb, "%-12s %s.%s  execs=%d new=%d elapsed=%s",
			string(r.Outcome), safe(r.Target.Pkg), safe(r.Target.Name), r.Execs, r.NewInteresting, r.Elapsed.Round(time.Second))
		if r.Note != "" {
			sb.WriteString("  note: " + safe(r.Note))
		}
		sb.WriteString("\n")
		for _, f := range r.Findings {
			sb.WriteString("             finding: " + safe(f) + "\n")
		}
	}
	fmt.Fprintf(&sb, "%d target(s):", len(rs))
	for _, o := range []Outcome{OK, Failed, Stalled, NoProgress, Timeout, BuildFailed, Skipped} {
		if counts[o] > 0 {
			fmt.Fprintf(&sb, " %d %s", counts[o], o)
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// ResultsJSON encodes the results.
func ResultsJSON(rs []RunResult) ([]byte, error) { return json.MarshalIndent(rs, "", "  ") }

// writeFile is os.WriteFile; a variable so a test can make the write fail part way.
var writeFile = os.WriteFile

// WriteResultsFile writes results.json in dir through a temporary file and a rename, so a reader
// (or a container that dies) never sees a half-written file. A failure is an error.
func WriteResultsFile(dir string, rs []RunResult) error {
	b, err := ResultsJSON(rs)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "results.json.tmp")
	if err := writeFile(tmp, b, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "results.json")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ParseResultsJSON decodes the results written by ResultsJSON.
func ParseResultsJSON(b []byte) ([]RunResult, error) {
	var rs []RunResult
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, err
	}
	return rs, nil
}
