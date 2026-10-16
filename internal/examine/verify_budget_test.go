package examine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func TestVerifyContainerScanBudget(t *testing.T) {
	c := newCase(t)
	s, data := e01ChunkSession(t, c, []fstest.Node{{Path: "/a.txt", Data: pattern(8*blk, 5)}}, -1)
	limit := int64(3 * e01Chunk)
	s.SetScanBudget(examine.ScanBudget{Bytes: limit})
	cv, err := s.VerifyContainer(context.Background(), nil)
	if !errors.Is(err, examine.ErrScanBudget) {
		t.Fatalf("VerifyContainer err = %v, want ErrScanBudget", err)
	}
	if cv.Result != "scan-budget" || cv.BytesHashed <= 0 || cv.BytesHashed >= int64(len(data)) || cv.BytesHashed > limit+int64(e01Chunk) {
		t.Fatalf("cv = %+v (media %d, limit %d)", cv, len(data), limit)
	}
	if cv.MD5.Computed != "" || cv.SHA1.Computed != "" {
		t.Fatalf("a budget stop reported computed hashes: %+v", cv)
	}
	ends := auditByAction(t, c, "image.verify")
	if len(ends) != 1 || ends[0].Details["result"] != "scan-budget" || detailNum(ends[0].Details["scan_limit"]) != limit {
		t.Fatalf("image.verify entries = %+v", ends)
	}
}

func TestVerifyContainerUnlimitedMatches(t *testing.T) {
	c := newCase(t)
	s, _ := e01ChunkSession(t, c, []fstest.Node{{Path: "/a.txt", Data: pattern(8*blk, 5)}}, -1)
	s.SetScanBudget(examine.ScanBudget{Unlimited: true})
	cv, err := s.VerifyContainer(context.Background(), nil)
	if err != nil || cv.Result != "match" {
		t.Fatalf("cv = %+v, err %v", cv, err)
	}
	if limit, _ := s.ScanLimit(); limit != 0 {
		t.Fatalf("ScanLimit with Unlimited = %d, want 0", limit)
	}
}

func TestScanLimitDefaultsOnlyForImplausibleClaims(t *testing.T) {
	c := newCase(t)
	s, _ := e01ChunkSession(t, c, []fstest.Node{{Path: "/a.txt", Data: []byte("x")}}, -1)
	if limit, why := s.ScanLimit(); limit != 0 || why != "" {
		t.Fatalf("plausible image: ScanLimit = %d, %q; want no default limit", limit, why)
	}
	s.SetImplausibleForTest(true)
	limit, why := s.ScanLimit()
	if limit != examine.DefaultImplausibleScanBytes || why == "" {
		t.Fatalf("implausible image: ScanLimit = %d, %q", limit, why)
	}
	s.SetScanBudget(examine.ScanBudget{Bytes: 123})
	if limit, _ := s.ScanLimit(); limit != 123 {
		t.Fatalf("explicit budget ignored: %d", limit)
	}
}
