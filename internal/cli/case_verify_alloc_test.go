package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// A deleted-file artifact whose runs lie where the filesystem reports allocated space fails case verify with
// exit 4, though its bytes are the image's own (R5 and R6 are clean).
func TestCaseVerifyForgedAllocationExits4(t *testing.T) {
	e := newRecoverEnv(t, twoDeleted()...)
	if code, out := e.recover(t, "--all"); code != 0 {
		t.Fatalf("recover: %d %s", code, out)
	}
	if code, out := run(t, e.d, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("a real recovery must verify before the forgery: %d %s", code, out)
	}
	c, err := evidence.Open(e.c)
	if err != nil {
		t.Fatal(err)
	}
	// the first sectors of the disk hold the partition table: never unallocated space of the filesystem
	evidencetest.AddRecovered(t, c, e.rec, recDisk(e.nodes...), evidencetest.RecoveredSpec{
		Runs: []evidence.Run{{Offset: 0, Length: 512}}, Path: "recovered/p1-mtfs/900001-forged.bin", PartitionOffset: freedPartStart,
	})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, e.d, "case", "verify", "--case", e.c)
	if code != 4 || !strings.Contains(out, "allocation: run 0 (0+512) lies outside the space the filesystem reports unallocated") {
		t.Fatalf("exit %d, want 4 with the allocation problem:\n%s", code, out)
	}
	if strings.Contains(out, "reproduce:") {
		t.Errorf("R6 must be clean for the forgery:\n%s", out)
	}
}

// R9 re-opens the parent with the drivers of the command line, exactly as image recover did.
func TestAllocationRecheckSeesTestDrivers(t *testing.T) {
	e := newRecoverEnv(t, twoDeleted()...)
	if code, out := e.recover(t, "--all"); code != 0 {
		t.Fatalf("recover: %d %s", code, out)
	}
	n := len(recoveredRecords(t, e.c))
	code, out := run(t, e.d, "case", "verify", "--case", e.c, "--json")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var rep struct {
		Rechecked int      `json:"allocation_rechecked"`
		Problems  []string `json:"problems"`
	}
	if err := json.Unmarshal(jsonPart(out), &rep); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if rep.Rechecked != n || n != 2 || len(rep.Problems) != 0 {
		t.Errorf("allocation_rechecked %d of %d recoveries, problems %q", rep.Rechecked, n, rep.Problems)
	}
	// without the test drivers the same case cannot be re-opened: a problem, never a silent pass
	code, out = run(t, Deps{}, "case", "verify", "--case", e.c)
	if code != 4 || !strings.Contains(out, "allocation: partition 1 of parent") {
		t.Errorf("without the drivers: exit %d, want 4 with the cannot-be-opened problem:\n%s", code, out)
	}
}

// freedDisk is a recovery-capable MTFS disk holding one deleted node per data slice whose blocks the
// filesystem reports free, and the image offset of each slice (R9 re-opens such a parent with mtfsDrivers).
func freedDisk(t *testing.T, data ...[]byte) (disk []byte, at []int64) {
	t.Helper()
	nodes := make([]fstest.Node, len(data))
	for i, d := range data {
		nodes[i] = fstest.Node{Path: fmt.Sprintf("/f%d.bin", i), Data: d, Deleted: true, Freed: true}
	}
	disk = recDisk(nodes...)
	for _, d := range data {
		i := bytes.Index(disk, d)
		if i < 0 || bytes.Contains(disk[i+1:], d) {
			t.Fatalf("a %d-byte slice is not found exactly once in the disk", len(d))
		}
		at = append(at, int64(i))
	}
	return disk, at
}

// freedPartStart is the byte offset of the filesystem partition in a recDisk image (its first LBA is 40).
const freedPartStart = 40 * 512
