package examine_test

import (
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func TestExtractLongNamesNeverAbort(t *testing.T) {
	c := newCase(t)
	raw1 := "~raw~" + strings.Repeat("A", 340) // 345 bytes: a non-UTF-8 ext4 name in display form
	raw2 := "~raw~" + strings.Repeat("A", 339) + "B"
	cjk := strings.Repeat("日", 85) + ".txt" // 259 bytes
	s, _ := session(t, c, 0,
		fstest.Node{Path: "/" + raw1, Data: []byte("one")},
		fstest.Node{Path: "/" + raw2, Data: []byte("two")},
		fstest.Node{Path: "/" + cjk, Data: []byte("three")},
		fstest.Node{Path: "/" + raw1 + "dir", Dir: true},
		fstest.Node{Path: "/" + raw1 + "dir/" + raw2, Data: []byte("four")})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 4 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	locals := map[string]string{}
	for _, r := range sum.Artifacts {
		for _, comp := range strings.Split(r.Path, "/") {
			if len(comp) > 255 {
				t.Errorf("component of %d bytes in %q", len(comp), r.Path)
			}
		}
		want := map[string]string{"/" + raw1: "one", "/" + raw2: "two", "/" + cjk: "three", "/" + raw1 + "dir/" + raw2: "four"}[r.Source.RemotePath]
		if want == "" || string(readArtifact(t, c, r)) != want {
			t.Errorf("artifact %q (remote %q) content wrong", r.Path, r.Source.RemotePath)
		}
		if r.Source.Derived == nil || r.Source.Derived.FSPath != r.Source.RemotePath {
			t.Errorf("provenance lost the original path: %+v", r.Source)
		}
		if prev, dup := locals[r.Path]; dup && prev != r.Source.RemotePath {
			t.Errorf("distinct files share the local path %q", r.Path)
		}
		locals[r.Path] = r.Source.RemotePath
	}
	verifyOK(t, c)
}

func TestExtractNameErrorFromOSBecomesWarning(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0,
		fstest.Node{Path: "/bad", Data: []byte("x")},
		fstest.Node{Path: "/good", Data: []byte("y")})
	s.SetNewArtifact(func(deviceID, acqID, rel string, src evidence.Source) (*evidence.ArtifactWriter, error) {
		if strings.HasSuffix(rel, "/bad") {
			return nil, &fs.PathError{Op: "open", Path: rel, Err: syscall.ENAMETOOLONG}
		}
		return c.NewArtifact(deviceID, acqID, rel, src)
	})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 1 || sum.Skipped != 1 || len(sum.Warnings) != 1 || sum.Warnings[0].Path != "/bad" {
		t.Fatalf("summary = %+v", sum)
	}
	if !strings.Contains(sum.Warnings[0].Reason, "local") {
		t.Errorf("reason = %q", sum.Warnings[0].Reason)
	}
	if len(auditByAction(t, c, "analysis.end")) != 1 {
		t.Error("the run did not end normally")
	}
}

func TestExtractCaseFailureStillAborts(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 0, fstest.Node{Path: "/f", Data: []byte("x")})
	s.SetNewArtifact(func(string, string, string, evidence.Source) (*evidence.ArtifactWriter, error) {
		return nil, fmt.Errorf("disk on fire")
	})
	if _, err := s.Extract(t.Context(), examine.ExtractOptions{Partition: -1, Paths: []string{"/f"}}); err == nil {
		t.Fatal("a non-name case failure was downgraded to a warning")
	}
}
