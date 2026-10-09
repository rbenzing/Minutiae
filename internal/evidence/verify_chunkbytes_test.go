package evidence

import "testing"

// TestVerifyChunkByteCutSplitsChunks lowers the byte cut so that rows of a few hundred bytes end a
// chunk early: the chunk sizes then follow the bytes, not the row count, and every row is still read once.
func TestVerifyChunkByteCutSplitsChunks(t *testing.T) {
	old := verifyChunkBytes
	verifyChunkBytes = 1
	t.Cleanup(func() { verifyChunkBytes = old })
	c, art := caseWithArtifact(t)
	insertRawRecords(t, c, art.ID, 1, 7) // seven unaudited rows: verify reports problems for them, this test only reads the chunk sizes
	var chunks []int
	rep, err := c.verify(func(table string, rows int) {
		if table == "records" {
			chunks = append(chunks, rows)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.RecordsChecked != 7 {
		t.Fatalf("checked %d records, want 7", rep.RecordsChecked)
	}
	for _, n := range chunks[:len(chunks)-1] {
		if n != 1 {
			t.Fatalf("with a 1-byte cut every full chunk holds one row, got %v", chunks)
		}
	}
}
