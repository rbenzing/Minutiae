package cli

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

func TestImageInfoMaxScanBytes(t *testing.T) {
	e := newEWFEnv(t, ewftest.Options{})
	before := len(e.audit(t))
	code, out, errs := e.info(t, "--verify", "--max-scan-bytes", "5000")
	if code != 1 {
		t.Fatalf("budget stop: exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if !strings.Contains(out, "scan-budget") || strings.Contains(out, "result  match") {
		t.Fatalf("output must name the budget stop:\n%s", out)
	}
	entries := e.verifyEntries(t)
	if len(entries) != 1 || entries[0].Details["result"] != "scan-budget" {
		t.Fatalf("audit = %+v", entries)
	}
	if len(newEntries(e.audit(t), before)) != 1 {
		t.Fatalf("a budget stop must append exactly one entry, got %+v", newEntries(e.audit(t), before))
	}
	// 0 means no limit.
	if code, out, errs := e.info(t, "--verify", "--max-scan-bytes", "0"); code != 0 {
		t.Fatalf("--max-scan-bytes 0: %d\n%s\n%s", code, out, errs)
	}
	if code, _, _ := e.info(t, "--verify", "--max-scan-bytes", "-1"); code != ExitUsage {
		t.Fatalf("negative --max-scan-bytes: exit %d, want usage", code)
	}
}

func TestImageInfoShowsImplausibleSize(t *testing.T) {
	e := newEWFEnvMutated(t, ewftest.Options{}, func(segs [][]byte) {
		v := ewftest.Sections(segs[0])
		for _, s := range v {
			if s.Type != "volume" {
				continue
			}
			p := int(s.Offset) + 76
			binary.LittleEndian.PutUint32(segs[0][p+4:], 1000)        // number_of_chunks
			binary.LittleEndian.PutUint32(segs[0][p+8:], 32768)       // sectors_per_chunk
			binary.LittleEndian.PutUint64(segs[0][p+16:], 1000*32768) // number_of_sectors
			ewftest.FixAdler(segs[0], p, p+1048, p+1048)
		}
	})
	// The partition table cannot be read from such an image, but the container
	// view (--verify) still prints, so the claim is visible; the exit code is non-zero.
	code, out, errs := e.info(t, "--verify")
	if code == 0 || !strings.Contains(out, "Size claim:") || !strings.Contains(out, "implausible") {
		t.Fatalf("info --verify: %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	_, jout, _ := e.info(t, "--verify", "--json")
	if !strings.Contains(jout, `"implausible_size": true`) && !strings.Contains(jout, `"implausible_size":true`) {
		t.Fatalf("JSON lacks implausible_size:\n%s", jout)
	}
}

// X42: a bound the user gave is never silently dropped. --max-scan-bytes only bounds the
// verification, so without --verify it is a usage error that names the requirement.
func TestImageInfoMaxScanBytesRequiresVerify(t *testing.T) {
	e := newEWFEnv(t, ewftest.Options{})
	before := len(e.audit(t))
	for _, v := range []string{"5000", "0"} {
		code, out, errs := e.info(t, "--max-scan-bytes", v)
		if code != ExitUsage || !strings.Contains(errs, "--verify") {
			t.Fatalf("--max-scan-bytes %s without --verify: exit %d, want usage naming --verify\nstdout:\n%s\nstderr:\n%s", v, code, out, errs)
		}
	}
	if n := len(newEntries(e.audit(t), before)); n != 0 {
		t.Fatalf("a usage error audited %d entries", n)
	}
}
