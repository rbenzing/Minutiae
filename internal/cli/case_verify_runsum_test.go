package cli

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// Rule R8 is an integrity problem: a complete derived artifact whose runs do not add up to its size
// makes `case verify` exit 4 and name the artifact; the same case with matching runs is clean.
func TestCaseVerifyRunSumMismatchExits4(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs []evidence.Run
		code int
	}{
		{"runs add up", []evidence.Run{{Offset: 100, Length: 8}}, 0},
		{"runs fall short", []evidence.Run{{Offset: 100, Length: 4}}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := recordstest.NewCase(t)
			img := recordstest.AddArtifact(t, c, "img.bin", make([]byte, 4096))
			f := recordstest.AddDerivedWith(t, c, "f.bin", evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
				ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/f", FSID: "nid:5", Runs: tc.runs,
			}}, []byte("abcdefgh"))
			dir := c.Dir
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			code, out := run(t, Deps{}, "case", "verify", "--case", dir)
			if code != tc.code {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			if tc.code == 4 && (!strings.Contains(out, "PROBLEM:") || !strings.Contains(out, f.ID) || !strings.Contains(out, "runs cover 4 bytes but the artifact holds 8")) {
				t.Fatalf("the problem does not name the artifact and the sums:\n%s", out)
			}
		})
	}
}
