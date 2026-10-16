package cli

import (
	"strings"
	"testing"
)

func TestImageExtractLimitExits1(t *testing.T) {
	e := newImgEnv(t)
	code, out := e.image(t, "extract", e.ref, "-r", "/", "--max-files", "1")
	if code != 1 || !strings.Contains(out, "output limit reached: max-files") {
		t.Fatalf("extract --max-files 1: %d\n%s", code, out)
	}
	if code, out := run(t, e.d, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("case verify after a limited extract: %d %s", code, out)
	}
}

func TestImageExtractNegativeLimitIsUsage(t *testing.T) {
	e := newImgEnv(t)
	if code, out := e.image(t, "extract", e.ref, "-r", "/", "--max-bytes", "-5"); code != ExitUsage {
		t.Fatalf("negative --max-bytes: %d %s; want usage exit %d", code, out, ExitUsage)
	}
}
