package chromium_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

const (
	remoteChrome  = "/data/data/com.android.chrome/app_chrome/Default/History"
	remoteEdge    = "/data/data/com.microsoft.emmx/app_chrome/Default/History"
	remoteBrave   = "/data/user/10/com.brave.browser/app_chrome/Profile 1/History"
	remoteWebView = "/data/data/com.example.reader/app_webview/Default/History"
)

var variants = []string{"current", "current-wal", "mid-upgraded", "old", "minimal", "chromium-deleted"}

// remoteFor is where the variant's database sits on the device.
func remoteFor(variant string) string {
	switch variant {
	case "mid-upgraded":
		return remoteEdge
	case "old":
		return remoteBrave
	case "minimal":
		return remoteWebView
	}
	return remoteChrome
}

func TestFixturesArePinnedByHash(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		for role, data := range fx.Files {
			sum := sha256.Sum256(data)
			key := v + "." + role
			seen[key] = true
			if want, ok := fixturePins[key]; !ok || want != hex.EncodeToString(sum[:]) {
				t.Errorf("%s: sha256 %x is not the pinned value %q (regenerate image, oracle and pin together)", key, sum, want)
			}
		}
	}
	for key := range fixturePins {
		if !seen[key] {
			t.Errorf("pin %s has no fixture file", key)
		}
	}
}

func TestEveryCommittedFixtureIsInTheVariantList(t *testing.T) {
	all, err := filepath.Glob("testdata/*.expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var m []string
	for _, p := range all {
		if !strings.HasSuffix(p, ".recover.expect.json") { // the typed recovery oracles are checked by TestEveryRecoverOracleHasAFixture
			m = append(m, p)
		}
	}
	if len(m) != len(variants) {
		t.Errorf("%d fixtures on disk, %d in the variant list", len(m), len(variants))
	}
	for _, p := range m {
		name := strings.TrimSuffix(filepath.Base(p), ".expect.json")
		known := false
		for _, v := range variants {
			known = known || v == name
		}
		if !known {
			t.Errorf("testdata/%s is not in the variant list", name)
		}
	}
}

var (
	verdictLine = regexp.MustCompile(`^-- verdict (verified|historical)$`)
	sourceLine  = regexp.MustCompile(`^-- source chromium \d+(\.\d+){3} \S+\.cc:\d+(, \S+\.cc:\d+)*$`)
	versionLine = regexp.MustCompile(`^-- meta\.version (\d+(,\d+)*)$`)
	phaseLine   = regexp.MustCompile(`(?m)^-- @phase \d+$`)
)

// Every snapshot names its verdict, its source and, per phase, the meta.version it stands for (a comma list when the
// file has several phases: the generator reads it the same way).
func TestDDLSnapshotsNameTheirVerdictAndSource(t *testing.T) {
	verdict := map[string]string{
		"chromium-current.sql": "verified", "chromium-minimal.sql": "verified",
		"chromium-mid.sql": "historical", "chromium-old.sql": "historical",
	}
	files, err := filepath.Glob("../../../tools/fixtures/schemas/chromium/*.sql")
	if err != nil || len(files) != len(verdict) {
		t.Fatalf("snapshots %v (%v), want %d", files, err, len(verdict))
	}
	for _, p := range files {
		name := filepath.Base(p)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(raw), "\n")
		if len(lines) < 3 {
			t.Fatalf("%s: fewer than three header lines", name)
		}
		if m := verdictLine.FindStringSubmatch(lines[0]); m == nil || m[1] != verdict[name] {
			t.Errorf("%s: line 1 %q, want verdict %s", name, lines[0], verdict[name])
		}
		if !sourceLine.MatchString(lines[1]) {
			t.Errorf("%s: line 2 %q is not a chromium source line", name, lines[1])
		}
		m := versionLine.FindStringSubmatch(lines[2])
		if m == nil {
			t.Errorf("%s: line 3 %q is not a meta.version line", name, lines[2])
			continue
		}
		phases := max(1, len(phaseLine.FindAllString(string(raw), -1)))
		if got := len(strings.Split(m[1], ",")); got != phases {
			t.Errorf("%s: %d meta.version values for %d phases", name, got, phases)
		}
	}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if _, ok := verdict[fx.Expect.DDL]; !ok {
			t.Errorf("%s: ddl %q is not one of the snapshots", v, fx.Expect.DDL)
		}
	}
}
