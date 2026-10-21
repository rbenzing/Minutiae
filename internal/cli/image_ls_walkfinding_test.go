package cli

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// dirChainTable is a chain of n distinct directories below the root, each named name, plus a file at the root.
func dirChainTable(n int, name string) map[string]any {
	ents := []map[string]any{{"id": "1", "parent_id": "", "name": "", "type": "dir"}}
	for i := 2; i < n+2; i++ {
		ents = append(ents, map[string]any{"id": strconv.Itoa(i), "parent_id": strconv.Itoa(i - 1), "name": name, "type": "dir"})
	}
	ents = append(ents, map[string]any{"id": "zz", "parent_id": "1", "name": "z", "type": "file"})
	return map[string]any{"block_size": 512, "entries": ents}
}

// F98: `image ls -r --json` carries each walker finding (cycle, depth, path length) as a warnings item in the
// array, so a JSON consumer sees the listing is not whole. The exit code stays 0 (F88).
func TestImageLsRecursiveJSONCarriesWalkerFindings(t *testing.T) {
	cycle := hostileCycleTable()
	for name, tc := range map[string]struct {
		table map[string]any
		want  string
	}{
		"cycle": {cycle, "cycle"},
		"depth": {dirChainTable(filesys.MaxWalkDepth+10, "d"), "nesting deeper than"},
		"path":  {dirChainTable(200, strings.Repeat("n", 255)), "path longer than"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCLICase(t)
			d := Deps{FSDrivers: mtfsDrivers}
			ref := importRaw(t, c, hostileDisk(t, tc.table))
			code, out, errs := runSplit(context.Background(), t, d, "image", "ls", "--case", c, "-r", "--json", ref)
			if code != 0 {
				t.Fatalf("exit %d, want 0 (F88): %q %q", code, out, errs)
			}
			var items []struct {
				Path     string          `json:"path"`
				Entry    json.RawMessage `json:"entry"`
				Warnings []string        `json:"warnings"`
			}
			if err := json.Unmarshal(jsonPart(out), &items); err != nil {
				t.Fatalf("json: %v\n%.300s", err, out)
			}
			var warned, entries int
			for _, it := range items {
				if len(it.Warnings) == 0 {
					entries++
					continue
				}
				warned++
				if len(it.Warnings) != 1 || !strings.Contains(it.Warnings[0], tc.want) || it.Path == "" || it.Entry != nil {
					t.Errorf("warnings item = %+v; want one %q finding with a path and no entry", it, tc.want)
				}
			}
			if warned != 1 {
				t.Errorf("%d warnings items, want 1", warned)
			}
			if entries == 0 {
				t.Errorf("the entries read before the finding are missing")
			}
		})
	}
}
