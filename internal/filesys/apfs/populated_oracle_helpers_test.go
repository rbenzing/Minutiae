package apfs_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// The oracle writes exactly two storage words. An unknown word must be an
// error: it is never treated as inline, which would let a re-emitted oracle
// with a new word pass unchecked.
func TestPopStorageAcceptsExactlyTheOracleWords(t *testing.T) {
	if got, err := popStorage("stream"); err != nil || got != filesys.XattrStream {
		t.Errorf("stream = %q, %v", got, err)
	}
	if got, err := popStorage("embedded"); err != nil || got != filesys.XattrInline {
		t.Errorf("embedded = %q, %v", got, err)
	}
	for _, w := range []string{"", "inline", "Stream", "EMBEDDED", "fork", "stream ", "other"} {
		if got, err := popStorage(w); err == nil {
			t.Errorf("storage word %q was accepted as %q", w, got)
		}
	}
}

// The value check runs whenever the entry has xattr names, so a node with
// names and no values in the oracle cannot slip through unchecked.
func TestPopNeedsValueCheckWheneverNamesExist(t *testing.T) {
	if !popNeedsValueCheck(popNode{Xattrs: []string{"user.a"}}) {
		t.Error("an entry with xattr names and no oracle values is not checked")
	}
	if popNeedsValueCheck(popNode{}) {
		t.Error("an entry with no xattrs at all needs no value check")
	}
	n := popNode{}
	n.XattrValues = map[string]struct {
		Size    int64  `json:"size"`
		Sha256  string `json:"sha256"`
		Storage string `json:"storage"`
	}{"user.a": {}}
	if !popNeedsValueCheck(n) {
		t.Error("an entry with oracle values is not checked")
	}
}
