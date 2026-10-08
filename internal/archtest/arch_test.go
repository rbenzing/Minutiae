// Package archtest enforces the package dependency rule from the spec §4.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
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
	"internal/examine":                     {"internal/evidence", "internal/version", "internal/device", "internal/image", "internal/volume", "internal/filesys", "internal/filesys/detect"},
	"internal/device":                      {"internal/evidence", "internal/version"},
	"internal/transport/serial":            {"internal/device", "internal/evidence", "internal/version"},
	"internal/protocol":                    {"internal/device", "internal/evidence", "internal/version"},
	"internal/android/adb":                 {},
	"internal/android/adb/adbtest":         {},
	"internal/android":                     {"internal/android/adb", "internal/device", "internal/evidence", "internal/version"},
	"internal/ios/mb2":                     {},
	"internal/ios/mb2/mb2test":             {"internal/ios/mb2"},
	"internal/ios":                         {"internal/ios/mb2", "internal/device", "internal/evidence", "internal/version"},
	"internal/ios/iostest":                 {"internal/ios", "internal/ios/mb2/mb2test"},
	"internal/archtest":                    {},
	"internal/sqlitefile":                  {},
	"internal/sqlitefile/sqlitetest":       {"internal/sqlitefile"},
	"internal/records":                     {"internal/evidence", "internal/version"},
	"internal/decode/ts":                   {},
	"internal/decode/plist":                {},
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

func TestArchitectureDependencyRule(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	imports := map[string]map[string]bool{}
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
