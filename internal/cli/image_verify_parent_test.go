package cli

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

func TestImageLsVerifyParentExits4(t *testing.T) {
	e := newImgEnv(t)
	recs, err := manifest(e.c)
	if err != nil {
		t.Fatal(err)
	}
	var parent evidence.ManifestRecord
	for _, r := range recs {
		if r.Source.Kind == "import" {
			parent = r
		}
	}
	if code, out := e.image(t, "ls", e.ref, "/", "--verify-parent"); code != 0 {
		t.Fatalf("clean parent with --verify-parent: %d %s", code, out)
	}
	evidencetest.FlipArtifactByte(t, e.c, parent, parent.Size/2)
	code, out := e.image(t, "ls", e.ref, "/", "--verify-parent")
	if code != ExitIntegrity {
		t.Fatalf("tampered parent with --verify-parent: %d %s; want exit %d", code, out, ExitIntegrity)
	}
	// Without the flag the cheap size check cannot tell: the command must not
	// claim an integrity failure it did not check for.
	if code, out := e.image(t, "ls", e.ref, "/"); code == ExitIntegrity {
		t.Fatalf("default ls reported an integrity error without checking the hash: %s", out)
	}
	if code, out := run(t, e.d, "case", "verify", "--case", e.c); code != ExitIntegrity {
		t.Fatalf("case verify stays the authority: %d %s", code, out)
	}
}

// Every command that opens an image offers --verify-parent and a tampered parent
// ends in exit 4 on each; a clean parent still works. image recover keeps the default.
func TestImageCommandsVerifyParentExit4(t *testing.T) {
	e := newImgEnv(t)
	recs, err := manifest(e.c)
	if err != nil {
		t.Fatal(err)
	}
	parent := recs[0]
	cmds := map[string][]string{
		"info":          {"info", e.ref},
		"info --verify": {"info", e.ref, "--verify"},
		"stat":          {"stat", e.ref, "/docs/a.txt"},
		"extract":       {"extract", e.ref, "/docs/a.txt"},
		"unalloc":       {"unalloc", e.ref, "-p", "1"},
	}
	for name, args := range cmds {
		if code, out := e.image(t, args[0], append([]string{"--verify-parent"}, args[1:]...)...); code == ExitIntegrity || code == ExitUsage {
			t.Fatalf("%s: clean parent with --verify-parent: %d %s", name, code, out)
		}
	}
	evidencetest.FlipArtifactByte(t, e.c, parent, parent.Size/2)
	for name, args := range cmds {
		full := append([]string{"--verify-parent"}, args[1:]...)
		if code, out := e.image(t, args[0], full...); code != ExitIntegrity {
			t.Errorf("%s: tampered parent: %d %s; want exit %d", name, code, out, ExitIntegrity)
		}
	}
	if code, out := e.image(t, "recover", "--verify-parent", e.ref); code != ExitUsage {
		t.Errorf("image recover must not offer --verify-parent: %d %s", code, out)
	}
}
