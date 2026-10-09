package evidence

import "testing"

// TestVerifyChunkByteCutSplitsChunks lowers the byte cut to two rows' worth so that the chunk sizes
// follow the bytes, not the row count (5000): seven rows must come in at least three chunks, every
// chunk but the last at or under the cut, and every row read exactly once. Removing the cut, or
// inflating it, leaves one chunk of seven rows and fails this test.
func TestVerifyChunkByteCutSplitsChunks(t *testing.T) {
	c, art := caseWithArtifact(t)
	// the size streamRecords adds per row of insertRawRecords: payload '{}', summary 'raw', type 'event', artifact id
	rowBytes := len("{}") + len("raw") + len("event") + len(art.ID)
	cut := 2 * rowBytes
	old := verifyChunkBytes
	verifyChunkBytes = cut
	t.Cleanup(func() { verifyChunkBytes = old })
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
	if len(chunks) < 3 {
		t.Fatalf("a cut of two rows must split seven rows into at least 3 chunks, got %v", chunks)
	}
	sum := 0
	for i, n := range chunks {
		sum += n
		if i < len(chunks)-1 && n*rowBytes > cut {
			t.Fatalf("chunk %d holds %d rows of %d bytes, over the cut of %d: %v", i, n, rowBytes, cut, chunks)
		}
	}
	if sum != 7 {
		t.Fatalf("the chunks %v cover %d rows, want each of the 7 exactly once", chunks, sum)
	}
}
