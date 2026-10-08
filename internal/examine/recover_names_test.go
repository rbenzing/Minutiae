package examine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

func TestRecoverNameCollisionRetries(t *testing.T) {
	e := newRecEnv(t, delNode("/a.bin", pat(1, bs)))
	calls := 0
	e.s.SetNewArtifact(func(dev, acq, rel string, src evidence.Source) (*evidence.ArtifactWriter, error) {
		calls++
		if calls == 1 {
			return nil, evidence.ErrArtifactExists
		}
		return e.c.NewArtifact(dev, acq, rel, src)
	})
	sum := e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != 1 || sum.Recovered != 1 || calls != 2 {
		t.Fatalf("artifacts %d recovered %d calls %d, want the second name used", len(recs), sum.Recovered, calls)
	}
	if !strings.Contains(recs[0].Path, "000001-a") {
		t.Errorf("path = %q", recs[0].Path)
	}
	verifyOK(t, e.c)
}

func TestRecoverRefusesOverwrite(t *testing.T) {
	e := newRecEnv(t, delNode("/a.bin", pat(1, bs)))
	const planted = "evidence that must survive"
	var plantedPath string
	e.s.SetNewArtifact(func(dev, acq, rel string, src evidence.Source) (*evidence.ArtifactWriter, error) {
		if plantedPath == "" {
			plantedPath = filepath.Join(e.c.Dir, "artifacts", dev, acq, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(plantedPath), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plantedPath, []byte(planted), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return e.c.NewArtifact(dev, acq, rel, src)
	})
	sum := e.recover(t, examine.RecoverOptions{All: true})
	got, err := os.ReadFile(plantedPath)
	if err != nil || string(got) != planted {
		t.Fatalf("the file at the target name changed: %q, %v", got, err)
	}
	recs := recovered(t, e.c)
	if len(recs) != 1 || sum.Recovered != 1 {
		t.Fatalf("artifacts %d recovered %d, want the retry under another name", len(recs), sum.Recovered)
	}
	if filepath.Join(e.c.Dir, filepath.FromSlash(recs[0].Path)) == plantedPath {
		t.Error("the artifact took the planted file's name")
	}
}

func TestRecoveredNeverCollidesWithExtract(t *testing.T) {
	e := newRecEnv(t, fstest.Node{Path: "/live.bin", Data: pat(1, bs)}, delNode("/gone.bin", pat(2, bs)))
	if _, err := e.s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true}); err != nil {
		t.Fatal(err)
	}
	e.recover(t, examine.RecoverOptions{All: true})
	if len(recovered(t, e.c)) != 1 {
		t.Fatalf("%d recovered artifacts, want 1", len(recovered(t, e.c)))
	}
	paths := map[string]bool{}
	for _, r := range mustManifest(t, e.c) {
		if paths[r.Path] {
			t.Errorf("duplicate path %q", r.Path)
		}
		paths[r.Path] = true
		switch r.Source.Kind {
		case "extract":
			if strings.Contains(r.Path, "/recovered/") {
				t.Errorf("extract artifact in the recovered namespace: %q", r.Path)
			}
		case "recover":
			if !strings.Contains(r.Path, "/recovered/") {
				t.Errorf("recover artifact outside the recovered namespace: %q", r.Path)
			}
		}
	}
	verifyOK(t, e.c)
}

// Bytes the tools would rewrite in this source if they were spelled as escapes.
var (
	nulS  = string(rune(0))
	escS  = string(rune(0x1b))
	bidiS = string(rune(0x202e))
)

func TestRecoverLongAndUnsafeNames(t *testing.T) {
	long := strings.Repeat("n", 255)
	hostile := map[string]string{
		"PLACEHOLDER-NUL": "nul" + nulS + "name", "PLACEHOLDER-BIDI": "bidi" + bidiS + "exe.txt", "PLACEHOLDER-ESC": "e" + escS + "[31mr",
		"PLACEHOLDER-DOTDOT": "..", "PLACEHOLDER-DOT": ".", "PLACEHOLDER-EMPTY": "", "PLACEHOLDER-RAW": "~raw~AAAA",
	}
	nodes := []fstest.Node{delNode("/"+long, pat(1, bs))}
	i := 2
	for from := range hostile {
		nodes = append(nodes, delNode("/"+from, pat(byte(i), bs)))
		i++
	}
	// The MTFS reader refuses these names itself, so a hook hands them to the examine layer.
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.rewriteKids = func(kids []filesys.Entry) []filesys.Entry {
			for k := range kids {
				if to, ok := hostile[kids[k].Name]; ok {
					kids[k].Name = to
				}
			}
			return kids
		}
	}, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != len(nodes) || sum.Recovered != len(nodes) {
		t.Fatalf("artifacts %d recovered %d, want all %d (warnings %q)", len(recs), sum.Recovered, len(nodes), warnReasons(t, e.c))
	}
	paths := map[string]bool{}
	for _, r := range recs {
		paths[r.Source.Derived.FSPath] = true
		for _, comp := range strings.Split(r.Path, "/") {
			if len(comp) > 255 || strings.ContainsAny(comp, nulS+escS) || strings.Contains(comp, bidiS) || comp == ".." || comp == "." || comp == "" {
				t.Errorf("unsafe path component %q in %q", comp, r.Path)
			}
		}
		if !strings.Contains(r.Path, "/recovered/p1-mtfs/") {
			t.Errorf("path = %q", r.Path)
		}
	}
	for _, want := range []string{"/" + long, "/..", "/.", "/nul" + nulS + "name", "/bidi" + bidiS + "exe.txt", "/~raw~AAAA"} {
		if !paths[want] {
			t.Errorf("FSPath %q not kept as the original", want)
		}
	}
	verifyOK(t, e.c)
}
