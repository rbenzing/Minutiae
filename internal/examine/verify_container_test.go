package examine_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// Container verification must not depend on the partition table: a damaged
// image is exactly when it matters. The chunk holding the primary GPT header
// (chunk 0) and one of the chunks holding the backup GPT (the last) are made
// unreadable; Open fails, OpenContainer and VerifyContainer do not, the
// image.verify entry is written before partition parsing is attempted, and the
// partition failure is reported apart.
func TestVerifyContainerNeedsNoPartitionTable(t *testing.T) {
	media := e01Media()
	last := (len(media) - 1) / e01Chunk
	for _, tc := range []struct {
		name  string
		chunk int
	}{{"primary GPT", 0}, {"backup GPT", last}} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCase(t)
			bad := bytes.Repeat([]byte{0x55}, e01Chunk+4) // the right length, a wrong Adler-32
			recs := importE01(t, c, ewftest.Options{
				SectorsPerChunk: 8, ChunksPerSegment: 3, Compress: ewftest.CompressMixed,
				Override: map[int]ewftest.RawChunk{tc.chunk: {Data: bad}},
			}, media)

			if _, err := examine.Open(c, recs[0].ID, mtfsOpts()); !errors.Is(err, image.ErrChunkCorrupt) {
				t.Fatalf("Open = %v, want the chunk error", err)
			}

			s, err := examine.OpenContainer(c, recs[0].ID, mtfsOpts())
			if err != nil {
				t.Fatalf("OpenContainer: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			cv, err := s.VerifyContainer(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if cv.Result != "unverified" || cv.BadChunk != int64(tc.chunk) {
				t.Fatalf("%+v", cv)
			}
			es := verifyEntries(t, c)
			if len(es) != 1 || es[0].Details["result"] != "unverified" || num(es[0].Details["first_bad_chunk"]) != tc.chunk {
				t.Fatalf("image.verify entries: %+v", es)
			}
			perr := s.ReadPartitions()
			if perr == nil || !errors.Is(perr, image.ErrChunkCorrupt) || !strings.Contains(perr.Error(), "partition table") {
				t.Fatalf("ReadPartitions = %v", perr)
			}
			info := s.Info() // container facts stay available
			if info.Format != "ewf" || info.Size != int64(len(media)) || info.PartitionError == "" || len(info.Partitions) != 0 {
				t.Fatalf("Info = %+v", info)
			}
			caseOK(t, c)
		})
	}
}

// A readable image: OpenContainer then ReadPartitions is Open.
func TestOpenContainerThenReadPartitions(t *testing.T) {
	c := newCase(t)
	recs := importE01(t, c, ewftest.Options{SectorsPerChunk: 8, ChunksPerSegment: 3}, e01Media())
	s, err := examine.OpenContainer(c, recs[0].ID, mtfsOpts())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.Table != nil {
		t.Fatal("OpenContainer parsed the partition table")
	}
	if err := s.ReadPartitions(); err != nil {
		t.Fatal(err)
	}
	if s.Table == nil || s.Table.Scheme != "gpt" || len(s.Info().Partitions) != 1 || s.Info().PartitionError != "" {
		t.Fatalf("table %+v", s.Table)
	}
}

// A failed audit append is returned with the verification result (never
// swallowed, never turned into a different result).
func TestVerifyContainerAuditFailureIsReturnedWithResult(t *testing.T) {
	c := newCase(t)
	recs := importE01(t, c, ewftest.Options{SectorsPerChunk: 8, ChunksPerSegment: 3}, e01Media())
	s := openSession(t, c, recs[0].ID)
	if err := c.Audit.Close(); err != nil { // appends now fail
		t.Fatal(err)
	}
	cv, err := s.VerifyContainer(context.Background(), nil)
	if err == nil {
		t.Fatal("a failed audit append was not reported")
	}
	if cv.Result != "match" || cv.BytesHashed != cv.Size {
		t.Fatalf("the verification result was lost: %+v", cv)
	}
}
