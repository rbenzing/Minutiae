package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The kernel-written F2FS recovery fixtures (internal/filesys/f2fs/testdata/recover/) through the real command
// and the real driver registry, with the independent oracle's totals (3D Task 10).
type f2fsRecoverTotals struct {
	Deleted []struct {
		Listed bool `json:"listed"`
		Expect struct {
			Outcome    string `json:"outcome"`
			Skip       string `json:"skip"`
			Cut        bool   `json:"cut"`
			Prefix     bool   `json:"prefix"`
			Confidence int    `json:"confidence"`
		} `json:"expect"`
	} `json:"deleted"`
}

func TestImageRecoverF2FSFixturesVerifyClean(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers real-image fixtures")
	}
	for _, name := range []string{"f2fs-recover-nodiscard", "f2fs-recover-discard"} {
		t.Run(name, func(t *testing.T) {
			img, _ := loadRecoverFixtureImage(t, "f2fs", name) // checks the image against generator.image_sha256
			raw, err := os.ReadFile(filepath.Join("..", "filesys", "f2fs", "testdata", "recover", name+".expect.json"))
			if err != nil {
				t.Fatal(err)
			}
			var o f2fsRecoverTotals
			if err := json.Unmarshal(raw, &o); err != nil {
				t.Fatal(err)
			}
			var recovered, partial, overlap int
			skipped := map[string]int{}
			for _, d := range o.Deleted {
				switch {
				case !d.Listed:
				case d.Expect.Outcome == "skip":
					skipped[d.Expect.Skip]++
				default:
					recovered++
					if d.Expect.Cut || d.Expect.Prefix {
						partial++
					}
					if d.Expect.Confidence == 30 { // the overlap cap
						overlap++
					}
				}
			}
			c := newCLICase(t)
			d := Deps{} // the real driver registry
			ref := importRaw(t, c, gptWhole(img))

			before := auditActions(t, c)
			code, out := run(t, d, "image", "recover", "--case", c, ref, "--list")
			if code != 0 {
				t.Fatalf("--list: %d %s", code, out)
			}
			if acts := auditActions(t, c)[len(before):]; !slices.Equal(acts, []string{"case.open"}) {
				t.Errorf("--list appended %v, want only case.open", acts)
			}
			if n := len(recoveredRecords(t, c)); n != 0 {
				t.Fatalf("%d recovered artifacts after --list", n)
			}

			// every skip the oracle expects is decided (unsupported, reused, no-free-bytes, not-a-file, not-found):
			// none raises the exit code
			code, out = run(t, d, "image", "recover", "--case", c, ref, "--all", "--json")
			if code != 0 {
				t.Fatalf("--all: %d %s", code, out)
			}
			var js jsonRecover
			if err := json.Unmarshal(jsonPart(out), &js); err != nil {
				t.Fatalf("json %q: %v", out, err)
			}
			if js.Recovered != recovered || js.Partial != partial || js.Overlap != overlap {
				t.Errorf("recovered/partial/overlap = %d/%d/%d, the oracle says %d/%d/%d", js.Recovered, js.Partial, js.Overlap, recovered, partial, overlap)
			}
			for reason, n := range skipped {
				if js.SkippedBy[reason] != n {
					t.Errorf("skipped_by[%s] = %d, the oracle says %d", reason, js.SkippedBy[reason], n)
				}
			}
			for reason, n := range js.SkippedBy {
				if skipped[reason] != n {
					t.Errorf("skipped_by[%s] = %d, the oracle expects %d", reason, n, skipped[reason])
				}
			}
			recs := recoveredRecords(t, c)
			if len(recs) != recovered {
				t.Fatalf("%d recovered artifacts in the manifest, the oracle says %d", len(recs), recovered)
			}

			code, out = run(t, d, "case", "verify", "--case", c)
			if code != 0 {
				t.Fatalf("case verify: exit %d, want 0:\n%s", code, out)
			}
			if recovered == 0 {
				if strings.Contains(out, "recovered artifact(s)") {
					t.Errorf("case verify mentions recovered artifacts for a case with none:\n%s", out)
				}
				return
			}
			want := strconv.Itoa(recovered) + " recovered (" + strconv.Itoa(recovered) + " reproduced)"
			if !strings.Contains(out, "NOTICE:") || !strings.Contains(out, "recovered artifact(s)") || !strings.Contains(out, want) {
				t.Fatalf("case verify: want the recovered notice and %q:\n%s", want, out)
			}
			// R6 (reproduce from the image) and R9 (the allocation recheck reads the real filesystem) ran on every one
			run9 := lastVerifyRun(t, c)
			if got := run9["recovered_reproduced"]; got != float64(recovered) {
				t.Errorf("recovered_reproduced = %v, want %d (R6)", got, recovered)
			}
			if got := run9["allocation_rechecked"]; got != float64(recovered) {
				t.Errorf("allocation_rechecked = %v, want the %d deleted-file recoveries (R9)", got, recovered)
			}

			// flip one byte of one recovered artifact: verify recomputes the bytes from the image and exits 4
			var victim string
			for _, r := range recs {
				if r.Size > 0 {
					victim = filepath.Join(c, filepath.FromSlash(r.Path))
					break
				}
			}
			b, err := os.ReadFile(victim) //nolint:gosec // a test reads its own case directory
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)/2] ^= 0xFF
			if err := os.WriteFile(victim, b, 0o600); err != nil { //nolint:gosec // idem
				t.Fatal(err)
			}
			code, out = run(t, d, "case", "verify", "--case", c)
			if code != 4 || !strings.Contains(out, "PROBLEM:") || !strings.Contains(out, "reproduce: byte") {
				t.Errorf("case verify after the flipped byte: exit %d, want 4 with a reproduce: byte problem:\n%s", code, out)
			}
		})
	}
}
