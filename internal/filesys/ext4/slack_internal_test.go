package ext4

import (
	"encoding/binary"
	"math/rand/v2"
	"slices"
	"testing"
)

// referenceSlackScan is the plain definition of slackScan: every candidate is
// judged by plausibleName on its own bytes.
func referenceSlackScan(w *dirScan, b []byte, from, to int) (found [][2]int) {
	for q := from; q+direntHeader < to; {
		inode := binary.LittleEndian.Uint32(b[q:])
		nameLen, _ := w.nameLen(b[q:])
		end := q + direntHeader + nameLen
		if inode >= 1 && inode <= w.f.sb.inodesCount && nameLen > 0 && end <= to && plausibleName(b[q+direntHeader:end], w.enc) {
			found = append(found, [2]int{q, nameLen})
			q += (direntHeader + nameLen + 3) &^ 3
			continue
		}
		q += 4
	}
	return found
}

// TestSlackScanMatchesPlausibleName: the indexed check finds exactly the
// records that plausibleName accepts, with and without encryption, on regions
// full of NUL, '/', dots and valid-looking headers.
func TestSlackScanMatchesPlausibleName(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test input, not security
	alphabet := []byte{0, 0, '/', '.', 'a', 'b', 1, 2, 5, 9, 12}
	for _, enc := range []bool{false, true} {
		total := 0
		f := &FS{sb: &superblock{inodesCount: 200, incompat: incompatFiletype}, slackScanCap: maxSlackScan, dirRecordCap: maxDirRecords}
		for iter := range 3000 {
			b := make([]byte, 64+rng.IntN(200))
			for i := range b {
				b[i] = alphabet[rng.IntN(len(alphabet))]
			}
			w := &dirScan{f: f, in: &inode{num: 2}, wantDeleted: true, enc: enc}
			var got [][2]int
			w.visit = func(r *dirRec) bool {
				got = append(got, [2]int{r.off, len(r.name)})
				return true
			}
			g := &region{b: b, limit: len(b), blk: 1, slack: true}
			from := 4 * rng.IntN(4)
			w.slackScan(g, from, len(b))
			total += len(got)
			if want := referenceSlackScan(w, b, from, len(b)); !slices.Equal(got, want) {
				t.Fatalf("enc=%v iteration %d: found %v, want %v\nregion %v", enc, iter, got, want, b)
			}
		}
		if total < 50 {
			t.Errorf("enc=%v: only %d records found over all regions; the test is not exercising acceptance", enc, total)
		}
	}
}
