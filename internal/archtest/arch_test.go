// Package archtest enforces the package dependency rule from the spec §4.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

const module = "github.com/rbenzing/minutiae/"

// allowed lists, per package (relative to module root), the Minutiae packages
// it may import from non-test files. A package missing here may import nothing
// from Minutiae — add it deliberately.
var allowed = map[string][]string{
	"cmd/minutiae":                         {"internal/cli"},
	"internal/version":                     {},
	"internal/image":                       {"internal/image/ewf"},
	"internal/image/ewf":                   {},
	"internal/image/ewf/ewftest":           {},
	"internal/volume":                      {},
	"internal/volume/volumetest":           {"internal/volume"},
	"internal/filesys":                     {},
	"internal/filesys/fstest":              {"internal/filesys"},
	"internal/filesys/detect":              {"internal/filesys", "internal/filesys/apfs", "internal/filesys/ext4", "internal/filesys/f2fs", "internal/filesys/exfat", "internal/filesys/hfsplus", "internal/filesys/fat"},
	"internal/filesys/apfs":                {"internal/filesys"},
	"internal/filesys/apfs/apfstest":       {"internal/filesys", "internal/filesys/apfs"},
	"internal/filesys/ext4":                {"internal/filesys"},
	"internal/filesys/ext4/ext4test":       {"internal/filesys", "internal/filesys/ext4"},
	"internal/filesys/f2fs":                {"internal/filesys"},
	"internal/filesys/f2fs/f2fstest":       {"internal/filesys", "internal/filesys/f2fs"},
	"internal/filesys/fat":                 {"internal/filesys"},
	"internal/filesys/fat/fattest":         {"internal/filesys", "internal/filesys/fat"},
	"internal/filesys/exfat":               {"internal/filesys"},
	"internal/filesys/exfat/exfattest":     {"internal/filesys", "internal/filesys/exfat"},
	"internal/filesys/hfsplus":             {"internal/filesys"},
	"internal/filesys/hfsplus/hfsplustest": {"internal/filesys", "internal/filesys/hfsplus"},
	"internal/evidence":                    {"internal/version"},
	"internal/evidence/evidencetest":       {"internal/evidence"},
	"internal/examine":                     {"internal/evidence", "internal/version", "internal/device", "internal/image", "internal/volume", "internal/filesys", "internal/filesys/detect"},
	"internal/device":                      {"internal/evidence", "internal/version"},
	"internal/transport/serial":            {"internal/device", "internal/evidence", "internal/version"},
	"internal/protocol":                    {"internal/device", "internal/evidence", "internal/version"},
	"internal/android/adb":                 {},
	"internal/android/adb/adbtest":         {},
	"internal/android":                     {"internal/android/adb", "internal/device", "internal/evidence", "internal/version"},
	"internal/ios/mb2":                     {"internal/decode/plist"},
	"internal/ios/mb2/mb2test":             {"internal/ios/mb2"},
	"internal/ios":                         {"internal/ios/mb2", "internal/device", "internal/evidence", "internal/version"},
	"internal/ios/iostest":                 {"internal/ios", "internal/ios/mb2/mb2test"},
	"internal/archtest":                    {},
	"internal/sqlitefile":                  {},
	"internal/sqlitefile/sqlitetest":       {"internal/sqlitefile"},
	"internal/records":                     {"internal/evidence", "internal/version"},
	"internal/decode/ts":                   {},
	"internal/decode/plist":                {},
	"internal/decode/typedstream":          {},
	"internal/records/recordstest":         {"internal/records", "internal/evidence"},
	"internal/parse":                       {"internal/records"},
	"internal/parsers":                     {"internal/parse", "internal/records"},
	"internal/recordtypes/common":          {},
	"internal/recordtypes/call":            {"internal/records", "internal/recordtypes/common"},
	"internal/recordtypes/contact":         {"internal/records", "internal/recordtypes/common"},
	"internal/recordtypes/message":         {"internal/records", "internal/recordtypes/common"},
	"internal/recordtypes/web":             {"internal/records", "internal/recordtypes/common"},
	"internal/recordtypes/all":             {"internal/recordtypes/message", "internal/recordtypes/call", "internal/recordtypes/contact", "internal/recordtypes/web"},
	"internal/parsers/parsertest":          {"internal/parse", "internal/records", "internal/evidence", "internal/recordtypes/common", "internal/recordtypes/message", "internal/recordtypes/call", "internal/recordtypes/contact", "internal/recordtypes/web"},
	"internal/artparse":                    {"internal/evidence", "internal/version", "internal/records", "internal/parse", "internal/recordtypes/all"},
	"tools/check":                          {},
	"tools/parserhash":                     {"internal/parse", "internal/parsers"},
}

// "internal/cli" may import anything.
const unrestricted = "internal/cli"

// moduleImports parses the import clauses of every non-test Go file under the module root and
// returns, per package directory (slash form, relative to the root), the Minutiae packages it
// imports: the real import graph the architecture tests are judged against.
func moduleImports(t *testing.T) (root string, imports map[string]map[string]bool) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	imports = map[string]map[string]bool{}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); strings.HasPrefix(name, ".") || name == "docs" || name == "bin" || name == "cases" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		pkg := filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		if imports[pkg] == nil {
			imports[pkg] = map[string]bool{}
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(path, module) {
				imports[pkg][strings.TrimPrefix(path, module)] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return root, imports
}

func TestArchitectureDependencyRule(t *testing.T) {
	root, imports := moduleImports(t)
	pkgs := make([]string, 0, len(imports))
	for p := range imports {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, pkg := range pkgs {
		if pkg == unrestricted {
			continue
		}
		allow, known := allowed[pkg]
		if !known {
			t.Errorf("package %s is not listed in archtest.allowed; add it with its permitted imports", pkg)
			continue
		}
		ok := map[string]bool{}
		for _, a := range allow {
			ok[a] = true
		}
		for imp := range imports[pkg] {
			if !ok[imp] {
				t.Errorf("%s must not import %s", pkg, imp)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("root detection failed: %v", err)
	}
}

// TestBackendsImportDecodeOnlyThroughMb2 pins the one backend -> decode edge the architecture
// rule allows (CLAUDE.md section 5): internal/ios/mb2 imports internal/decode/plist and nothing
// else under internal/decode; no other backend package has any edge into internal/decode.
func TestBackendsImportDecodeOnlyThroughMb2(t *testing.T) {
	_, imports := moduleImports(t)
	backendPrefixes := []string{
		"internal/transport", "internal/protocol", "internal/android", "internal/ios", "internal/device",
	}
	isBackend := func(pkg string) bool {
		for _, p := range backendPrefixes {
			if pkg == p || strings.HasPrefix(pkg, p+"/") {
				return true
			}
		}
		return false
	}
	var edges []string
	for pkg, imps := range imports {
		if !isBackend(pkg) {
			continue
		}
		for imp := range imps {
			if imp == "internal/decode" || strings.HasPrefix(imp, "internal/decode/") {
				edges = append(edges, pkg+" -> "+imp)
			}
		}
	}
	sort.Strings(edges)
	want := []string{"internal/ios/mb2 -> internal/decode/plist"}
	if !slices.Equal(edges, want) {
		t.Fatalf("backend -> decode edges = %v, want exactly %v", edges, want)
	}
}
