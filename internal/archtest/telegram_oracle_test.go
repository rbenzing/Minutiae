package archtest

import (
	"fmt"
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

// pyImportRE finds an import statement anywhere in the source, not only at the start of a line (Task 5 review M2): after
// a ";", in a one-line compound statement ("if x: import re") or behind a backslash continuation (joined first). The
// word must stand alone, so "imports" in a docstring is not one; a docstring that names "import x" fails the scan, which
// errs on the safe side.
var (
	pyImportRE     = regexp.MustCompile(`\bimport[ \t]+([\w.]+(?:[ \t]+as[ \t]+\w+)?(?:[ \t]*,[ \t]*[\w.]+(?:[ \t]+as[ \t]+\w+)?)*)`)
	pyFromImportRE = regexp.MustCompile(`\bfrom[ \t]+(\S+)[ \t]+import\b[^\n;]*`)
	pyOpenPyRE     = regexp.MustCompile(`\.py[co]?["']`)
)

// telegramOracleBanned are the names and calls the oracle never holds: the modules it must be independent of, and every
// route that loads code other than an import statement (exec, eval, compile, __import__, importlib, runpy, the module
// table or path).
func telegramOracleBanned() []string {
	return []string{
		"telegram_tl_encode", "telegram_android", "telegram_tl_facts", "telephony_common", "internal/decode/tl",
		"__import__", "importlib", "runpy", "exec(", "eval(", "compile(", "sys.modules", "sys.path",
	}
}

// TestTLOracleIsIndependent pins that the independent TL oracle imports only the allowed standard modules, names
// neither the encoder, the generator, the facts tool, telephony_common nor the Go decoder, and loads no code by another
// route (spec §13: the oracle is written from the recon facts and constructors.tsv by a different task than the Go
// decoder; 4I4-P2; Task 5 review M2).
func TestTLOracleIsIndependent(t *testing.T) {
	src := calendarReadFile(t, filepath.Join(repoRoot(t), "tools", "fixtures", "telegram_tl_oracle.py"))
	for _, b := range oracleIndependenceFindings(src) {
		t.Error("telegram_tl_oracle.py: " + b)
	}
	if len(pyImportRE.FindAllString(src, -1)) == 0 {
		t.Error("telegram_tl_oracle.py imports nothing: the import scan read no statement")
	}
}

// oracleIndependenceFindings lists what breaks the oracle's independence in src: an import of a module outside
// telegramOracleModules wherever it stands, a banned name or call, a .py path.
func oracleIndependenceFindings(src string) []string {
	var out []string
	joined := strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\\\n", " ")
	var mods []string
	for _, m := range pyFromImportRE.FindAllStringSubmatch(joined, -1) {
		mods = append(mods, m[1])
	}
	// the names a from-import brings in are not modules: the from-statements are read above and removed here
	for _, m := range pyImportRE.FindAllStringSubmatch(pyFromImportRE.ReplaceAllString(joined, ""), -1) {
		for _, part := range strings.Split(m[1], ",") {
			if f := strings.Fields(part); len(f) > 0 {
				mods = append(mods, f[0])
			}
		}
	}
	for _, mod := range mods {
		top, _, _ := strings.Cut(mod, ".")
		if !telegramOracleModules[top] || top != mod {
			out = append(out, fmt.Sprintf("imports %q: only the modules of telegramOracleModules are allowed", mod))
		}
	}
	for _, banned := range telegramOracleBanned() {
		if strings.Contains(src, banned) {
			out = append(out, fmt.Sprintf("names %q", banned))
		}
	}
	if loc := pyOpenPyRE.FindStringIndex(src); loc != nil {
		out = append(out, fmt.Sprintf("names a .py path (%q)", src[max(0, loc[0]-20):loc[1]]))
	}
	return out
}

// TestTLOracleIndependenceScanCatchesEvasions is the scan's own test (Task 5 review M2): each route around a line-start
// import is found, and a clean source passes.
func TestTLOracleIndependenceScanCatchesEvasions(t *testing.T) {
	clean := "import struct\nimport json, os\nfrom csv import reader\nx = 1  # it imports nothing else\n"
	if bad := oracleIndependenceFindings(clean); len(bad) > 0 {
		t.Fatalf("a clean source gives %v", bad)
	}
	for name, src := range map[string]string{
		"after a semicolon":   "x = 1; import re\n",
		"compound statement":  "if True: import re\n",
		"continuation":        "import struct, \\\n    re\n",
		"aliased second":      "import json as j, re as r\n",
		"from a submodule":    "from os.path import join\n",
		"after a from-import": "from csv import reader; import re\n",
		"dunder import":       "m = __import__('re')\n",
		"importlib":           "import struct\nm = importlib\n",
		"exec of a file":      "exec(open('x').read())\n",
		"open of a .py path":  "src = open(\"helper.py\").read()\n",
		"the facts tool":      "# see telegram_tl_facts for the rows\n",
		"runpy":               "runpy.run_path(p)\n",
		"compile":             "c = compile(s, 'x', 'exec')\n",
		"sys.modules":         "m = sys.modules['re']\n",
		"generator by name":   "# from the telegram_android generator\n",
		"encoder by name":     "E = 'telegram_tl_encode'\n",
		"telephony by name":   "# telephony_common\n",
		"go decoder by name":  "# internal/decode/tl\n",
		"windows line ending": "import struct\r\nimport re\r\n",
	} {
		if len(oracleIndependenceFindings(src)) == 0 {
			t.Errorf("%s: %q passes the scan", name, src)
		}
	}
}
