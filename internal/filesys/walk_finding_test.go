package filesys_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// F91: callers tell the walker's own findings (cycle, depth, path length) from a listing that failed or was
// cut without matching magic Structure/Offset values.
func TestIsWalkFinding(t *testing.T) {
	tbl := `{"block_size":512,"entries":[
		{"id":"1","parent_id":"","name":"","type":"dir"},
		{"id":"2","parent_id":"1","name":"a","type":"dir"},
		{"id":"2","parent_id":"2","name":"loop","type":"dir"}]}`
	img := fstest.Encode([]byte(tbl), nil)
	fsys, err := fstest.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	var finding error
	if err := filesys.Walk(fsys, fsys.Root(), "/", func(p string, _ filesys.Entry, werr error) error {
		if p == "/a/loop" {
			finding = werr
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if finding == nil || !filesys.IsWalkFinding(finding) {
		t.Errorf("the walker's cycle finding %v is not a walk finding", finding)
	}
	if !filesys.IsWalkFinding(fmt.Errorf("wrapped: %w", finding)) {
		t.Error("a wrapped finding is not recognised")
	}
	for name, err := range map[string]error{
		"nil":                nil,
		"limit":              fmt.Errorf("cap: %w", filesys.ErrLimit),
		"parser corrupt":     &filesys.CorruptError{Structure: "directory", Offset: 64, Reason: "bad"},
		"same magic, parser": &filesys.CorruptError{Structure: "directory", Offset: -1, Reason: "bad"},
		"plain":              errors.New("boom"),
	} {
		if filesys.IsWalkFinding(err) {
			t.Errorf("%s: %v is not a walker finding", name, err)
		}
	}
}
