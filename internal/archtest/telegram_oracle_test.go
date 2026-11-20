package archtest

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// telegramOracleModules are the only modules tools/fixtures/telegram_tl_oracle.py may import (plan 4I4 Task 5,
// 4I4-P2, spec §13 "Independent oracle"): the standard library pieces a TL reader needs to read constructors.tsv and
// the blobs. Nothing of the encoder, the generator, telephony_common or anything derived from the Go decoder.
var telegramOracleModules = map[string]bool{
	"struct": true, "json": true, "hashlib": true, "sys": true, "os": true, "argparse": true, "math": true, "csv": true,
}

var pyImportRE = regexp.MustCompile(`^\s*(?:import\s+(.+)|from\s+(\S+)\s+import\b)`)

// TestTLOracleIsIndependent pins that the independent TL oracle imports only the allowed standard modules and names
// neither the encoder, the generator, telephony_common nor the Go decoder (spec §13: the oracle is written from the
// recon facts and constructors.tsv by a different task than the Go decoder; 4I4-P2).
func TestTLOracleIsIndependent(t *testing.T) {
	src := calendarReadFile(t, filepath.Join(repoRoot(t), "tools", "fixtures", "telegram_tl_oracle.py"))
	imports := 0
	for _, line := range strings.Split(src, "\n") {
		m := pyImportRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var mods []string
		if m[1] != "" {
			for _, part := range strings.Split(m[1], ",") {
				f := strings.Fields(part)
				if len(f) > 0 {
					mods = append(mods, f[0])
				}
			}
		} else {
			mods = append(mods, m[2])
		}
		for _, mod := range mods {
			imports++
			top, _, _ := strings.Cut(mod, ".")
			if !telegramOracleModules[top] || top != mod {
				t.Errorf("telegram_tl_oracle.py imports %q: only %v are allowed", mod, telegramOracleModules)
			}
		}
	}
	if imports == 0 {
		t.Error("telegram_tl_oracle.py imports nothing: the import scan read no line")
	}
	for _, banned := range []string{"telegram_tl_encode", "telegram_android", "telephony_common", "internal/decode/tl", "__import__", "importlib"} {
		if strings.Contains(src, banned) {
			t.Errorf("telegram_tl_oracle.py names %q", banned)
		}
	}
}
