package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/version"
)

func testDeps(out *bytes.Buffer) Deps {
	return Deps{In: strings.NewReader(""), Out: out, Err: io.Discard}
}

func TestVersionCommand(t *testing.T) {
	var out bytes.Buffer
	if code := Run([]string{"version"}, testDeps(&out)); code != ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), version.Version) {
		t.Fatalf("output %q does not contain version %q", out.String(), version.Version)
	}
}

func TestVersionCommandJSON(t *testing.T) {
	var out bytes.Buffer
	if code := Run([]string{"version", "--json"}, testDeps(&out)); code != ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	var v map[string]string
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", out.String(), err)
	}
	if v["version"] != version.Version {
		t.Fatalf("version = %q", v["version"])
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{{"nope"}, {"version", "--bogus"}, {"version", "extra"}} {
		var out bytes.Buffer
		if code := Run(args, testDeps(&out)); code != ExitUsage {
			t.Errorf("Run(%v) = %d, want %d", args, code, ExitUsage)
		}
	}
}

func TestExitCodeMapping(t *testing.T) {
	if ExitCode(nil) != ExitOK {
		t.Error("nil should map to ExitOK")
	}
	if ExitCode(errors.New("x")) != ExitError {
		t.Error("plain error should map to ExitError")
	}
	if ExitCode(usageErrorf("bad %s", "flag")) != ExitUsage {
		t.Error("usage error should map to ExitUsage")
	}
}

// J101: integrity first. An integrity failure joined with a usage error is exit 4 in either order, so moving the
// integrity case below the usage cases of ExitCode fails here.
func TestExitCodeIntegrityBeatsUsageInBothOrders(t *testing.T) {
	usage := usageErrorf("bad flag")
	for name, err := range map[string]error{
		"integrity then usage": errors.Join(evidence.ErrIntegrity, usage),
		"usage then integrity": errors.Join(usage, evidence.ErrIntegrity),
		"wrapped both":         fmt.Errorf("x: %w", errors.Join(usage, fmt.Errorf("y: %w", evidence.ErrIntegrity))),
		"needs upgrade":        errors.Join(evidence.ErrNeedsUpgrade, evidence.ErrIntegrity),
	} {
		if got := ExitCode(err); got != ExitIntegrity {
			t.Errorf("%s: exit %d, want 4", name, got)
		}
	}
}

func TestArgValidators(t *testing.T) {
	cmd := newVersionCmd(Deps{}, &rootOptions{})
	if err := minArgs(2)(cmd, []string{"a"}); ExitCode(err) != ExitUsage {
		t.Errorf("minArgs(2) with 1 arg: err=%v, want usage error", err)
	}
	if err := minArgs(2)(cmd, []string{"a", "b"}); err != nil {
		t.Errorf("minArgs(2) with 2 args: %v", err)
	}
	if err := exactArgs(1)(cmd, nil); ExitCode(err) != ExitUsage {
		t.Errorf("exactArgs(1) with 0 args: err=%v, want usage error", err)
	}
}
