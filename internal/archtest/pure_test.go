package archtest

// Purity rules for the parser packages (plan 4A, Task 4).
//
// Threat model: parsers, decoders and record-type packages are trusted, in-repo,
// reviewed code; the hostile party is the input data. These are mechanical guards
// against mistakes and drift (a stray os.Create, a goroutine that outlives its
// job, a dependency that grows a side effect), made strong on purpose so that
// "a parser cannot write to the case" is a checked property of the tree. They are
// not a defence against a malicious parser author.
//
// The scanner lives in this one file, one function per rule, so each rule can be
// self-tested (the *SelfTest tests at the end of this file, over testdata/pure) and
// none can go vacuous.
//
//	P1 stdlib and module import ALLOWLIST      checkImports
//	P2 syntax-tree bans                        checkAstBans
//	P3 selectors of internal/records           checkRecordsSelectors
//	P4 no non-Go source                        checkNonGoSource
//	P5 package-level state                     checkMutableState
//	P6 no SQL                                  checkNoSQL
//
// Pure roots: internal/parse, internal/recordtypes/..., internal/decode/...,
// internal/sqlitefile (without sqlitetest) and every directory under
// internal/parsers except parsertest.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// pureClass is the kind of a pure package; the allowlist is per class.
type pureClass int

const (
	pcParse      pureClass = iota + 1 // internal/parse
	pcRTCommon                        // internal/recordtypes/common
	pcRTType                          // internal/recordtypes/<type>
	pcDecode                          // internal/decode/...
	pcSqlitefile                      // internal/sqlitefile
	pcParser                          // internal/parsers/<name>
)

func (c pureClass) String() string {
	switch c {
	case pcParse:
		return "parse"
	case pcRTCommon:
		return "recordtypes/common"
	case pcRTType:
		return "recordtypes/<type>"
	case pcDecode:
		return "decode/..."
	case pcSqlitefile:
		return "sqlitefile"
	case pcParser:
		return "parsers/<name>"
	}
	return "unknown"
}

// classSet is a set of classes.
type classSet uint8

func setOf(cs ...pureClass) classSet {
	var s classSet
	for _, c := range cs {
		s |= 1 << uint(c)
	}
	return s
}

func (s classSet) has(c pureClass) bool { return s&(1<<uint(c)) != 0 }

var allClasses = setOf(pcParse, pcRTCommon, pcRTType, pcDecode, pcSqlitefile, pcParser)

// Directories of the pure roots (module-relative, slash separated).
const (
	dirParse      = "internal/parse"
	dirRecordtype = "internal/recordtypes"
	dirRTCommon   = "internal/recordtypes/common"
	dirDecode     = "internal/decode"
	dirDecodePlst = "internal/decode/plist"
	dirDecodeSQL  = "internal/decode/sqlitedb"
	dirSqlitefile = "internal/sqlitefile"
	dirParsers    = "internal/parsers"
)

// pureRoots are the directories the scan starts from.
var pureRoots = []string{dirParse, dirRecordtype, dirDecode, dirSqlitefile, dirParsers}

// classOf returns the class of the package in the module-relative directory rel,
// and false when rel is not a pure package (a test helper, the registry package,
// anything outside the roots).
func classOf(rel string) (pureClass, bool) {
	under := func(root string) bool { return rel == root || strings.HasPrefix(rel, root+"/") }
	switch {
	case under(dirParse):
		return pcParse, true
	case under(dirRTCommon):
		return pcRTCommon, true
	case under(dirRecordtype):
		return pcRTType, true
	case under(dirDecode):
		return pcDecode, true
	case under(dirSqlitefile + "/sqlitetest"):
		return 0, false // the test helper of the sqlite file reader
	case under(dirSqlitefile):
		return pcSqlitefile, true
	case strings.HasPrefix(rel, dirParsers+"/"):
		name, _, _ := strings.Cut(strings.TrimPrefix(rel, dirParsers+"/"), "/")
		if name == "parsertest" {
			return 0, false // the test harness: it is test-only (TestTestOnlyPackagesAreImportedFromTestsOnly)
		}
		return pcParser, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Sources and violations

// pureSrc is one parsed non-test Go file of a pure package.
type pureSrc struct {
	name string
	text string
	fset *token.FileSet
	f    *ast.File
}

type pureViolation struct {
	File string
	Line int
	Rule string
	Msg  string
}

func (v pureViolation) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", v.File, v.Line, v.Rule, v.Msg)
}

func violationAt(s pureSrc, pos token.Pos, rule, format string, args ...any) pureViolation {
	return pureViolation{File: s.name, Line: s.fset.Position(pos).Line, Rule: rule, Msg: fmt.Sprintf(format, args...)}
}

// dedupe drops a violation repeated for the same line and rule and message.
func dedupe(vs []pureViolation) []pureViolation {
	seen := map[pureViolation]bool{}
	var out []pureViolation
	for _, v := range vs {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// parsePureSources parses sources (file name to text). Build constraints are not
// evaluated: every file counts, whatever its tags.
func parsePureSources(t testing.TB, srcs map[string]string) []pureSrc {
	t.Helper()
	names := make([]string, 0, len(srcs))
	for n := range srcs {
		names = append(names, n)
	}
	sort.Strings(names)
	fset := token.NewFileSet()
	var out []pureSrc
	for _, n := range names {
		f, err := parser.ParseFile(fset, n, srcs[n], parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		out = append(out, pureSrc{name: n, text: srcs[n], fset: fset, f: f})
	}
	return out
}

// loadPureDir parses the non-test .go files directly in dir.
func loadPureDir(t testing.TB, dir string) []pureSrc {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n)) //nolint:gosec // reading the repository's own sources
		if err != nil {
			t.Fatal(err)
		}
		srcs[filepath.Join(dir, n)] = string(b)
	}
	return parsePureSources(t, srcs)
}

// ---------------------------------------------------------------------------
// Import helpers

func importPath(spec *ast.ImportSpec) string {
	p, err := strconv.Unquote(spec.Path.Value)
	if err != nil {
		return spec.Path.Value
	}
	return p
}

var majorVersionRE = regexp.MustCompile(`^v[0-9]+$`)

// localName is the name an import is known by in its file ("_" and "." as written).
func localName(spec *ast.ImportSpec) string {
	if spec.Name != nil {
		return spec.Name.Name
	}
	p := importPath(spec)
	base := path.Base(p)
	if majorVersionRE.MatchString(base) && strings.Contains(p, "/") {
		base = path.Base(path.Dir(p))
	}
	return base
}

// importNames maps the local name of every named import of f to its path.
func importNames(f *ast.File) map[string]string {
	m := map[string]string{}
	for _, spec := range f.Imports {
		if n := localName(spec); n != "_" && n != "." {
			m[n] = importPath(spec)
		}
	}
	return m
}

// pkgSelector reports the import path and selected name when e is `pkg.Name`
// where pkg is an import of the file (an identifier that a local declaration
// shadows does not resolve: the parser links it to its declaration).
func pkgSelector(e ast.Expr, names map[string]string) (pkgPath, sel string, ok bool) {
	s, isSel := e.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	id, isID := s.X.(*ast.Ident)
	if !isID || id.Obj != nil {
		return "", "", false
	}
	p, found := names[id.Name]
	if !found {
		return "", "", false
	}
	return p, s.Sel.Name, true
}

// ---------------------------------------------------------------------------
// P1: import allowlist

// stdAllow lists, for every package a pure package may import, the classes that
// may import it. Anything else, every stdlib package not listed included, is a
// violation: os, log, runtime, sync, reflect, net, unsafe, database/sql,
// math/rand, hash/maphash, go/*, bufio and the rest are refused without being
// named. A new import needs a deliberate, justified edit of this table.
var stdAllow = map[string]classSet{
	// pure data handling
	"bytes": allClasses, "strings": allClasses, "strconv": allClasses, "unicode": allClasses, "unicode/utf8": allClasses,
	"unicode/utf16": allClasses, "errors": allClasses, "math": allClasses, "math/bits": allClasses, "sort": allClasses,
	"slices": allClasses, "maps": allClasses,
	// time.Time and time.Duration values and conversions only; the clock, timers and the host zone are banned by P2
	"time": allClasses,
	// Sprintf, Errorf and Sprint for messages only; every printing and scanning function is banned by P2
	"fmt": allClasses,
	// binary formats and checksums
	"encoding/binary": setOf(pcDecode, pcSqlitefile, pcParser), "encoding/hex": setOf(pcDecode, pcSqlitefile, pcParser),
	"hash/crc32": setOf(pcDecode, pcSqlitefile, pcParser),
	// common.CleanText keeps the original bytes (base64) or their hash
	"encoding/base64": setOf(pcRTCommon), "crypto/sha256": setOf(pcRTCommon),
	// typed Decode of stored payloads
	"encoding/json": setOf(pcRTType),
	// name and token patterns (the regexp engine is linear time)
	"regexp": setOf(pcParse, pcRTCommon, pcRTType),
	// io.ReaderAt is the only way to read input; context carries cancellation (parse.Tick)
	"io": setOf(pcParse, pcDecode, pcSqlitefile, pcParser), "context": setOf(pcParse, pcDecode, pcSqlitefile, pcParser),
	// Budget and SealedReaderAt; no sync: nothing in a parser needs a lock
	"sync/atomic": setOf(pcParse),
	// embedded data lives only in the data packages (and is hashed by the parser identity, F1)
	"embed": setOf(pcRTCommon, pcRTType, pcDecode, pcParser),
}

// moduleImportAllowed reports whether a package of class (in directory dirRel)
// may import the module-relative package rel.
func moduleImportAllowed(rel string, class pureClass, dirRel string) bool {
	under := func(root string) bool { return rel == root || strings.HasPrefix(rel, root+"/") }
	switch {
	case rel == "internal/records": // data types only, bounded by P3
		return setOf(pcParse, pcRTType, pcDecode, pcSqlitefile, pcParser).has(class)
	case rel == dirParse: // the contract
		return setOf(pcRTCommon, pcRTType, pcDecode, pcSqlitefile, pcParser).has(class)
	case rel == dirRTCommon: // payload helpers
		return setOf(pcRTType, pcParser).has(class)
	case under(dirRecordtype): // payload builders
		return class == pcParser
	case under(dirDecode): // decoders
		return class == pcDecode || class == pcParser
	case rel == dirSqlitefile: // the sqlite file reader: its one decoder and the parsers
		return class == pcParser || dirRel == dirDecodeSQL
	}
	return false
}

// thirdPartyAllowed lists the non-stdlib, non-module packages and where each may be imported.
func thirdPartyAllowed(p, dirRel string) bool {
	return p == "howett.net/plist" && dirRel == dirDecodePlst
}

func isStdlib(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return !strings.Contains(first, ".") && p != "C"
}

// checkImports applies the allowlist to every import of the files of one package.
func checkImports(srcs []pureSrc, class pureClass, dirRel string) []pureViolation {
	var out []pureViolation
	for _, s := range srcs {
		for _, spec := range s.f.Imports {
			p := importPath(spec)
			ok := false
			switch {
			case strings.HasPrefix(p, module):
				ok = moduleImportAllowed(strings.TrimPrefix(p, module), class, dirRel)
			case isStdlib(p):
				ok = stdAllow[p].has(class)
			default:
				ok = thirdPartyAllowed(p, dirRel)
			}
			if !ok {
				out = append(out, violationAt(s, spec.Pos(), "P1", "import %q is not in the allowlist for %s (a new import needs a justified edit of the table in pure_test.go)", p, class))
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// P2: syntax-tree bans

// timeBans are the selectors of package time that read the clock, wait, start a
// timer or depend on the host time zone.
var timeBans = map[string]string{
	"Now": "reads the clock", "Since": "reads the clock", "Until": "reads the clock", "Sleep": "waits",
	"After": "starts a timer", "Tick": "starts a ticker", "NewTimer": "starts a timer", "NewTicker": "starts a ticker",
	"AfterFunc": "runs code on its own goroutine", "Local": "is the host time zone",
	"LoadLocation": "reads the host's zone database", "LoadLocationFromTZData": "loads a zone",
}

// unixBans are the time constructors that yield a Time in the host zone; internal/decode/ts, the one
// package that converts device timestamps, is exempt (it states the zone itself).
var unixBans = map[string]bool{"Unix": true, "UnixMilli": true, "UnixMicro": true}

// contextBans are the context constructors that read the clock and fire a timer on their own goroutine.
var contextBans = map[string]bool{"WithTimeout": true, "WithDeadline": true, "WithTimeoutCause": true, "WithDeadlineCause": true}

const dirDecodeTS = "internal/decode/ts"

// fmtBanPrefixes: every printing, scanning and appending function of fmt.
var fmtBanPrefixes = []string{"Print", "Fprint", "Scan", "Fscan", "Sscan", "Append"}

// embedAllowed are the classes that may carry //go:embed (data packages).
var embedAllowed = setOf(pcRTCommon, pcRTType, pcDecode, pcParser)

// topLevelNames returns the names the package declares at top level.
func topLevelNames(srcs []pureSrc) map[string]bool {
	m := map[string]bool{}
	for _, s := range srcs {
		for _, d := range s.f.Decls {
			switch x := d.(type) {
			case *ast.FuncDecl:
				if x.Recv == nil {
					m[x.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, sp := range x.Specs {
					switch y := sp.(type) {
					case *ast.ValueSpec:
						for _, n := range y.Names {
							m[n.Name] = true
						}
					case *ast.TypeSpec:
						m[y.Name.Name] = true
					}
				}
			}
		}
	}
	return m
}

// checkAstBans reports the constructs P2 forbids. Calls are resolved through the
// local name of each import, so an alias does not hide them.
func checkAstBans(srcs []pureSrc, class pureClass, dirRel string) []pureViolation {
	var out []pureViolation
	declared := topLevelNames(srcs)
	for _, s := range srcs {
		names := importNames(s.f)
		for _, spec := range s.f.Imports {
			p := importPath(spec)
			if p == "C" {
				out = append(out, violationAt(s, spec.Pos(), "P2", `import "C": cgo is banned (it would run code that is not hashed or reviewed as Go)`))
			}
			if spec.Name != nil && spec.Name.Name == "." {
				switch p {
				case "fmt", "log", "time", "context", "runtime":
					out = append(out, violationAt(s, spec.Pos(), "P2", "dot import of %s hides the calls the rules look for", p))
				}
			}
		}
		for _, cg := range s.f.Comments {
			for _, c := range cg.List {
				switch {
				case strings.HasPrefix(c.Text, "//go:linkname"):
					out = append(out, violationAt(s, c.Pos(), "P2", "//go:linkname reaches into other packages' internals"))
				case strings.HasPrefix(c.Text, "//go:embed") && !embedAllowed.has(class):
					out = append(out, violationAt(s, c.Pos(), "P2", "//go:embed is allowed only in the recordtypes, decode and parsers data packages (%s is not one)", class))
				}
			}
		}
		ast.Inspect(s.f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.GoStmt:
				out = append(out, violationAt(s, x.Pos(), "P2", "go statement: a parser runs on the host's goroutine and starts none"))
			case *ast.CallExpr:
				out = append(out, callBans(s, x, names, declared)...)
			case *ast.SelectorExpr:
				out = append(out, selectorBans(s, x, names, dirRel)...)
			}
			return true
		})
	}
	return dedupe(out)
}

// callBans checks the call-shaped bans: the print builtins and methods named Local or Go.
func callBans(s pureSrc, call *ast.CallExpr, names map[string]string, declared map[string]bool) []pureViolation {
	var out []pureViolation
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		if (fn.Name == "print" || fn.Name == "println") && fn.Obj == nil && !declared[fn.Name] {
			out = append(out, violationAt(s, fn.Pos(), "P2", "builtin %s writes to standard error", fn.Name))
		}
	case *ast.SelectorExpr:
		if _, _, isPkg := pkgSelector(fn, names); !isPkg {
			switch fn.Sel.Name {
			case "Local":
				out = append(out, violationAt(s, fn.Sel.Pos(), "P2", "call of a method named Local: results must not depend on the host time zone"))
			case "Go":
				out = append(out, violationAt(s, fn.Sel.Pos(), "P2", "call of a method named Go (the sync.WaitGroup.Go shape starts a goroutine)"))
			}
		}
	}
	return out
}

// selectorBans checks `pkg.Name` selectors against the banned lists.
func selectorBans(s pureSrc, sel *ast.SelectorExpr, names map[string]string, dirRel string) []pureViolation {
	p, name, ok := pkgSelector(sel, names)
	if !ok {
		return nil
	}
	switch {
	case p == "fmt":
		for _, pre := range fmtBanPrefixes {
			if strings.HasPrefix(name, pre) {
				return []pureViolation{violationAt(s, sel.Pos(), "P2", "fmt.%s prints, scans or appends to a writer (only Sprintf, Sprint and Errorf are allowed, for messages)", name)}
			}
		}
	case p == "log" || strings.HasPrefix(p, "log/"):
		return []pureViolation{violationAt(s, sel.Pos(), "P2", "%s.%s: a parser does not log", p, name)}
	case p == "time":
		if why, banned := timeBans[name]; banned {
			return []pureViolation{violationAt(s, sel.Pos(), "P2", "time.%s %s: results must not depend on the clock, a timer or the host zone", name, why)}
		}
		if unixBans[name] && dirRel != dirDecodeTS && !strings.HasPrefix(dirRel, dirDecodeTS+"/") {
			return []pureViolation{violationAt(s, sel.Pos(), "P2", "time.%s yields a Time in the host zone: build times in UTC with time.Date (only %s converts device timestamps)", name, dirDecodeTS)}
		}
	case p == "context" && name == "AfterFunc":
		return []pureViolation{violationAt(s, sel.Pos(), "P2", "context.AfterFunc runs code on its own goroutine")}
	case p == "context" && contextBans[name]:
		return []pureViolation{violationAt(s, sel.Pos(), "P2", "context.%s reads the clock and fires a timer: output must not depend on wall-clock timing, the host owns every deadline", name)}
	case p == "runtime" && (name == "SetFinalizer" || name == "AddCleanup"):
		return []pureViolation{violationAt(s, sel.Pos(), "P2", "runtime.%s runs code at a time the parser does not control", name)}
	}
	return nil
}

// ---------------------------------------------------------------------------
// P3: selectors of internal/records

const (
	recordsPkg  = module + "internal/records"
	evidencePkg = module + "internal/evidence"
)

// recordsData are the only selectors of records that every pure package may use:
// the data types of a record.
var recordsData = map[string]bool{
	"Record": true, "Range": true, "Time": true, "NamedTime": true, "Basis": true,
	"BasisUTC": true, "BasisLocalOffset": true, "BasisLocalUnknown": true,
}

// checkRecordsSelectors allows data types only. A recordtypes/<type> package may
// also call records.SetValidator, and only directly in the body of a function
// named init. No pure package imports internal/evidence.
func checkRecordsSelectors(srcs []pureSrc, class pureClass) []pureViolation {
	var out []pureViolation
	for _, s := range srcs {
		recNames := map[string]bool{} // every local name of records in this file
		for _, spec := range s.f.Imports {
			switch importPath(spec) {
			case evidencePkg:
				out = append(out, violationAt(s, spec.Pos(), "P3", "imports internal/evidence: a parser never touches the case, it only emits records through parse.Emitter"))
			case recordsPkg:
				switch n := localName(spec); n {
				case ".":
					out = append(out, violationAt(s, spec.Pos(), "P3", "dot import of internal/records hides which selectors are used"))
				case "_":
				default:
					recNames[n] = true
				}
			}
		}
		if len(recNames) == 0 {
			continue
		}
		check := func(sel *ast.SelectorExpr, inInit bool) {
			id, ok := sel.X.(*ast.Ident)
			if !ok || !recNames[id.Name] || id.Obj != nil {
				return
			}
			name := sel.Sel.Name
			switch {
			case recordsData[name]:
			case name == "SetValidator" && class == pcRTType && inInit:
			case name == "SetValidator" && class == pcRTType:
				out = append(out, violationAt(s, sel.Pos(), "P3", "records.SetValidator outside the body of a function named init"))
			default:
				out = append(out, violationAt(s, sel.Pos(), "P3", "records.%s: a pure package may use only the data types of records (%s)%s", name, sortedKeys(recordsData), setValidatorHint(name)))
			}
		}
		var walk func(n ast.Node, inInit bool)
		walk = func(n ast.Node, inInit bool) {
			ast.Inspect(n, func(m ast.Node) bool {
				switch x := m.(type) {
				case *ast.FuncLit:
					walk(x.Body, false) // a closure may run later: not the init body
					return false
				case *ast.SelectorExpr:
					check(x, inInit)
				}
				return true
			})
		}
		for _, d := range s.f.Decls {
			fd, isFn := d.(*ast.FuncDecl)
			walk(d, isFn && fd.Name.Name == "init" && fd.Recv == nil)
		}
	}
	return dedupe(out)
}

func setValidatorHint(name string) string {
	if name == "SetValidator" {
		return "; only a recordtypes/<type> package may call SetValidator, in init"
	}
	return ""
}

func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// ---------------------------------------------------------------------------
// P4: no non-Go source

// nonGoExts are the extensions of files go build compiles or links besides Go:
// assembly, C, C++, Objective-C, Fortran, SWIG and prebuilt objects. They would
// change behaviour without being part of the parser hash.
var nonGoExts = map[string]bool{
	".s": true, ".c": true, ".h": true, ".cc": true, ".cpp": true, ".cxx": true, ".hh": true, ".hpp": true, ".hxx": true,
	".m": true, ".mm": true, ".f": true, ".for": true, ".f90": true, ".syso": true, ".swig": true, ".swigcxx": true,
}

// checkNonGoSource reports the non-Go source files directly in dir.
func checkNonGoSource(dir string) []pureViolation {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []pureViolation{{File: dir, Rule: "P4", Msg: err.Error()}}
	}
	var out []pureViolation
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(e.Name())); nonGoExts[ext] {
			out = append(out, pureViolation{File: filepath.Join(dir, e.Name()), Rule: "P4", Msg: "non-Go source file (" + ext + "): it would change behaviour without being hashed or reviewed as Go"})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// P5: package-level state

type varKind int

// trackedVar is an allowed package-level variable and the spec that declares it.
type trackedVar struct {
	kind varKind
	spec *ast.ValueSpec
}

const (
	vkSentinel varKind = iota + 1 // error sentinel
	vkRegexp                      // regexp.MustCompile result
	vkTable                       // unexported read-only lookup table
	vkSchema                      // unexported common.Schema, receiver of Validate only
	vkEmbed                       // unexported string or embed.FS var filled by a go:embed directive: read-only
)

const commonPkg = module + dirRTCommon

// conversionTypes are the universe types whose conversion of a constant is a constant.
var conversionTypes = map[string]bool{
	"string": true, "byte": true, "rune": true, "bool": true, "int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true, "float32": true, "float64": true,
}

// constLike reports whether e is built only from literals, identifiers (named
// constants, true, false, nil, iota), package constants and operators, and holds
// no nested slice or map (a nested mutable value would escape the read-only check).
func constLike(e ast.Expr, names map[string]string) (ok bool, why string) {
	switch x := e.(type) {
	case *ast.BasicLit, *ast.Ident:
		return true, ""
	case *ast.ParenExpr:
		return constLike(x.X, names)
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return false, "an address is not a constant"
		}
		return constLike(x.X, names)
	case *ast.BinaryExpr:
		if ok, why := constLike(x.X, names); !ok {
			return false, why
		}
		return constLike(x.Y, names)
	case *ast.SelectorExpr:
		if _, _, isPkg := pkgSelector(x, names); isPkg {
			return true, ""
		}
		return false, "a selector that is not a package constant"
	case *ast.KeyValueExpr:
		if ok, why := constLike(x.Key, names); !ok {
			return false, why
		}
		return constLike(x.Value, names)
	case *ast.CallExpr:
		if id, isID := x.Fun.(*ast.Ident); isID && conversionTypes[id.Name] {
			for _, a := range x.Args {
				if ok, why := constLike(a, names); !ok {
					return false, why
				}
			}
			return true, ""
		}
		return false, "a call"
	case *ast.CompositeLit:
		switch t := x.Type.(type) {
		case *ast.MapType:
			return false, "a nested map"
		case *ast.ArrayType:
			if t.Len == nil {
				return false, "a nested slice"
			}
		}
		for _, el := range x.Elts {
			if ok, why := constLike(el, names); !ok {
				return false, why
			}
		}
		return true, ""
	}
	return false, fmt.Sprintf("%T is not a constant expression", e)
}

// mutableElementType reports whether the elements of a table literal of type t are
// slices, maps, pointers, channels, funcs or interfaces: state that could be
// modified through an alias even when the table itself is only read.
func mutableElementType(t ast.Expr) bool {
	var elem ast.Expr
	switch x := t.(type) {
	case *ast.ArrayType:
		elem = x.Elt
	case *ast.MapType:
		elem = x.Value
	}
	switch e := elem.(type) {
	case *ast.ArrayType:
		return e.Len == nil || mutableElementType(e)
	case *ast.MapType, *ast.StarExpr, *ast.ChanType, *ast.FuncType, *ast.InterfaceType:
		return true
	}
	return false
}

// isTableLiteral reports whether e is an array, slice or map literal.
func isTableLiteral(e ast.Expr) (*ast.CompositeLit, bool) {
	c, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	switch c.Type.(type) {
	case *ast.ArrayType, *ast.MapType:
		return c, true
	}
	return nil, false
}

func isCallToPkg(e ast.Expr, names map[string]string, pkg string, fns ...string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	p, name, ok := pkgSelector(call.Fun, names)
	if !ok || p != pkg {
		return false
	}
	for _, f := range fns {
		if name == f {
			return true
		}
	}
	return false
}

// isSchemaType reports whether t is the type common.Schema.
func isSchemaType(t ast.Expr, names map[string]string) bool {
	p, name, ok := pkgSelector(t, names)
	return ok && p == commonPkg && name == "Schema"
}

// isAssertionValue reports whether v is a form that runs no code: (*T)(nil), T(nil), T{} or nil.
func isAssertionValue(v ast.Expr) bool {
	switch x := v.(type) {
	case *ast.Ident:
		return x.Name == "nil"
	case *ast.CompositeLit:
		return len(x.Elts) == 0
	case *ast.CallExpr:
		if len(x.Args) != 1 {
			return false
		}
		arg, ok := x.Args[0].(*ast.Ident)
		if !ok || arg.Name != "nil" {
			return false
		}
		switch f := x.Fun.(type) {
		case *ast.ParenExpr:
			_, star := f.X.(*ast.StarExpr)
			return star
		case *ast.Ident, *ast.SelectorExpr:
			return true
		}
	}
	return false
}

// classifyVar decides whether the i-th name of a package-level var spec is one of
// the five allowed forms. It returns the kind to track, or a reason it is not allowed.
func classifyVar(spec *ast.ValueSpec, i int, names map[string]string) (varKind, string) {
	name := spec.Names[i].Name
	exported := ast.IsExported(name)
	var val ast.Expr
	switch {
	case len(spec.Values) == len(spec.Names):
		val = spec.Values[i]
	case len(spec.Values) == 0:
	default:
		return 0, "a var initialised from a multi-value expression"
	}
	if name == "_" {
		if val == nil || isAssertionValue(val) {
			return 0, ""
		}
		return 0, "a blank var whose initialiser runs code (only interface assertions are allowed)"
	}
	if val == nil {
		if spec.Type != nil && isSchemaType(spec.Type, names) && !exported {
			return vkSchema, ""
		}
		return 0, "package-level state: a var with no constant initialiser can be written by any function"
	}
	switch {
	case isCallToPkg(val, names, "errors", "New") || isCallToPkg(val, names, "fmt", "Errorf"):
		return vkSentinel, ""
	case isCallToPkg(val, names, "regexp", "MustCompile"):
		if exported {
			return 0, "an exported regexp var (only unexported ones are allowed)"
		}
		return vkRegexp, ""
	}
	if lit, ok := isTableLiteral(val); ok {
		if exported {
			return 0, "an exported table (only unexported lookup tables are allowed)"
		}
		if mutableElementType(lit.Type) {
			return 0, "a table of slices, maps or pointers (its elements could be modified through an alias)"
		}
		for _, el := range lit.Elts {
			if ok, why := constLike(el, names); !ok {
				return 0, "a table that is not made of constants: " + why
			}
		}
		return vkTable, ""
	}
	if lit, ok := val.(*ast.CompositeLit); ok && lit.Type != nil && isSchemaType(lit.Type, names) && !exported {
		return vkSchema, ""
	}
	if spec.Type != nil && isSchemaType(spec.Type, names) && !exported && isCallToPkg(val, names, commonPkg, funcNames(val)...) {
		return vkSchema, ""
	}
	return 0, "package-level state: only the blank identifier, error sentinels, regexp.MustCompile results, unexported constant tables and unexported common.Schema values are allowed"
}

// funcNames returns the called function's name of a call of a package function.
func funcNames(e ast.Expr) []string {
	if call, ok := e.(*ast.CallExpr); ok {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			return []string{sel.Sel.Name}
		}
	}
	return nil
}

// walkWithParents calls fn for every node with the chain of its ancestors
// (parents[len(parents)-1] is the direct parent).
func walkWithParents(root ast.Node, fn func(n ast.Node, parents []ast.Node)) {
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		fn(n, stack)
		stack = append(stack, n)
		return true
	})
}

// isDeclaration reports whether ident (with its ancestors) declares a name instead of using one.
func isDeclaration(id *ast.Ident, parents []ast.Node) bool {
	if len(parents) == 0 {
		return false
	}
	switch p := parents[len(parents)-1].(type) {
	case *ast.ValueSpec:
		for _, n := range p.Names {
			if n == id {
				return true
			}
		}
	case *ast.Field:
		for _, n := range p.Names {
			if n == id {
				return true
			}
		}
	case *ast.FuncDecl:
		return p.Name == id
	case *ast.TypeSpec:
		return p.Name == id
	case *ast.ImportSpec:
		return p.Name == id
	case *ast.LabeledStmt:
		return p.Label == id
	case *ast.BranchStmt:
		return p.Label == id
	case *ast.AssignStmt:
		if p.Tok == token.DEFINE {
			for _, l := range p.Lhs {
				if l == id {
					return true
				}
			}
		}
	case *ast.RangeStmt:
		if p.Tok == token.DEFINE && (p.Key == id || p.Value == id) {
			return true
		}
	case *ast.SelectorExpr:
		return p.Sel == id // a field or method name, not the variable
	case *ast.KeyValueExpr:
		return p.Key == id // a struct field name (or a read-only map key)
	}
	return false
}

// useProblem classifies one use of a tracked variable ("" when it is allowed).
func useProblem(kind varKind, id *ast.Ident, parents []ast.Node) string {
	var node ast.Node = id
	i := len(parents) - 1
	path := 0
	sel := ""
climb:
	for i >= 0 {
		switch p := parents[i].(type) {
		case *ast.ParenExpr:
			node = p
		case *ast.IndexExpr:
			if p.X != node {
				break climb
			}
			path++
			node = p
		case *ast.SelectorExpr:
			if p.X != node {
				break climb
			}
			path++
			sel = p.Sel.Name
			node = p
		case *ast.SliceExpr:
			if p.X == node && kind == vkTable {
				return "sliced (the slice would alias the table)"
			}
			break climb
		default:
			break climb
		}
		i--
	}
	var parent ast.Node
	if i >= 0 {
		parent = parents[i]
	}
	// writes, for every kind
	switch p := parent.(type) {
	case *ast.AssignStmt:
		for _, l := range p.Lhs {
			if l == node {
				return "assigned"
			}
		}
	case *ast.IncDecStmt:
		if p.X == node {
			return "incremented or decremented"
		}
	case *ast.UnaryExpr:
		if p.Op == token.AND && p.X == node {
			return "address taken"
		}
	case *ast.RangeStmt:
		if p.Key == node || p.Value == node {
			return "assigned by a range clause"
		}
	}
	if kind == vkSchema {
		if call, ok := parent.(*ast.CallExpr); ok && call.Fun == node && path == 1 && sel == "Validate" {
			return ""
		}
		return "used other than as the receiver of Validate"
	}
	if kind != vkTable {
		return ""
	}
	if call, ok := parent.(*ast.CallExpr); ok && call.Fun == node && path > 0 {
		return "a method called on an element"
	}
	if path > 0 {
		return "" // an element read in rvalue position
	}
	switch p := parent.(type) {
	case *ast.CallExpr:
		if fn, ok := p.Fun.(*ast.Ident); ok && fn.Obj == nil && (fn.Name == "len" || fn.Name == "cap") {
			return ""
		}
		return "passed to a function (it could be modified or retained)"
	case *ast.RangeStmt:
		if p.X == node {
			return ""
		}
	case *ast.BinaryExpr:
		if p.Op == token.EQL || p.Op == token.NEQ {
			return ""
		}
	}
	return "used other than by index, len, cap, range or comparison (an alias or a copy escapes)"
}

// checkMutableState applies P5 to one package: every package-level var is one of
// the five allowed forms, and every use of a tracked var is read-only (a lookup
// table) or a plain reference (sentinels, regexps).
func checkMutableState(srcs []pureSrc, class pureClass) []pureViolation {
	var out []pureViolation
	tracked := map[string]trackedVar{}
	for _, s := range srcs {
		names := importNames(s.f)
		for di, d := range s.f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for si, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i := range vs.Names {
					if embedVar(s.f, di, si, vs, names, class) {
						tracked[vs.Names[i].Name] = trackedVar{kind: vkEmbed, spec: vs}
						continue
					}
					kind, why := classifyVar(vs, i, names)
					if why != "" {
						out = append(out, violationAt(s, vs.Names[i].Pos(), "P5", "var %s: %s", vs.Names[i].Name, why))
						continue
					}
					if kind != 0 {
						tracked[vs.Names[i].Name] = trackedVar{kind: kind, spec: vs}
					}
				}
			}
		}
	}
	for _, s := range srcs {
		walkWithParents(s.f, func(n ast.Node, parents []ast.Node) {
			id, ok := n.(*ast.Ident)
			if !ok {
				return
			}
			tv, isTracked := tracked[id.Name]
			// an identifier the parser resolved to another declaration is a local variable that shadows the name
			if !isTracked || isDeclaration(id, parents) || (id.Obj != nil && id.Obj.Decl != tv.spec) {
				return
			}
			if why := useProblem(tv.kind, id, parents); why != "" {
				out = append(out, violationAt(s, id.Pos(), "P5", "package-level var %s is %s: it must stay read-only", id.Name, why))
			}
		})
	}
	return dedupe(out)
}

// ---------------------------------------------------------------------------
// P6: no SQL

// sqlStatementRE has the shapes of a SQL statement over text that normalizeSQL
// (records_writes_test.go) prepared: placeholders are \x01, quoting and comments
// are gone, whitespace is one space. The shapes are structural so that English
// ("select an option from the menu", "failed to delete from the cache") is not flagged.
var sqlStatementRE = regexp.MustCompile(`(?is)\b(?:` +
	// select <columns> from <table>, also a builder piece that ends right after from
	`select\s+(?:(?:distinct|all)\s+)?(?:\*|[\w.\x01]+(?:\s*\([^)]*\))?(?:\s+as\s+\w+)?(?:\s*,\s*[\w.\x01*]+(?:\s*\([^)]*\))?(?:\s+as\s+\w+)?)*)\s+from(?:\s+[\w.\x01(]|\s*$)|` +
	`insert\s+(?:or\s+\w+\s+)?into(?:\s+[\w.\x01]+\s*(?:\(|values\b|select\b|default\b|$)|\s*$)|` +
	`replace\s+into\s+[\w.\x01]+|` +
	`update\s+(?:or\s+\w+\s+)?[\w.\x01]+\s+set\s+[\w.\x01]+\s*=|` +
	`delete\s+from(?:\s+[\w.\x01]+(?:\s*;|\s*$|\s+(?:where|returning|order|limit|indexed|not|as)\b)|\s*$)|` +
	`create\s+(?:(?:temp|temporary|virtual|unique)\s+)*(?:table|index|trigger|view)\s+(?:if\s+not\s+exists\s+)?[\w.\x01]+|` +
	`drop\s+(?:table|index|trigger|view)\s+(?:if\s+exists\s+)?[\w.\x01]+\s*(?:;|$)|` +
	`alter\s+table\s+[\w.\x01]+\s+(?:add|rename|drop)\b|` +
	`pragma\s+[\w.]+\s*(?:=|\(|;|$)|` +
	`attach\s+(?:database\s+)?(?:'[^']*'|[\w./\\\x01-]+)\s+as\s+\w)`)

// looksLikeSQL reports whether a string constant has the shape of a SQL statement.
func looksLikeSQL(text string) bool {
	return sqlStatementRE.MatchString(normalizeSQL(text))
}

// checkNoSQL reports every string expression (constants and concatenations folded
// as records_writes_test.go does, through the same scanner) with the shape of a
// SQL statement. Comments are never inspected.
func checkNoSQL(t testing.TB, srcs []pureSrc) []pureViolation {
	t.Helper()
	text := map[string]string{}
	for _, s := range srcs {
		text[s.name] = s.text
	}
	var out []pureViolation
	for _, v := range stringViolations(t, text, looksLikeSQL) {
		out = append(out, pureViolation{File: v.File, Line: v.Line, Rule: "P6", Msg: fmt.Sprintf("string with the shape of a SQL statement: %q (parsers never talk to a database; sqlitefile decodes the file format directly)", v.Text)})
	}
	return out
}

// ---------------------------------------------------------------------------
// The scan of the real tree

// pureDirs returns every pure package directory (module-relative, slash separated)
// that exists under root, with its class. Directories named testdata and dot
// directories are skipped.
func pureDirs(root string) (map[string]pureClass, error) {
	out := map[string]pureClass{}
	for _, r := range pureRoots {
		base := filepath.Join(root, filepath.FromSlash(r))
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			continue // a root that does not exist yet is skipped
		}
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				return nil
			}
			if name := d.Name(); p != base && (name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if class, ok := classOf(rel); ok {
				out[rel] = class
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func repoRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func sortedDirs(m map[string]pureClass) []string {
	out := make([]string, 0, len(m))
	for d := range m {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// scanPure runs rule over every pure package of the tree and reports the violations.
func scanPure(t *testing.T, rule func(t *testing.T, dirRel string, class pureClass, srcs []pureSrc) []pureViolation) {
	t.Helper()
	root := repoRoot(t)
	dirs, err := pureDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range sortedDirs(dirs) {
		srcs := loadPureDir(t, filepath.Join(root, filepath.FromSlash(rel)))
		for _, v := range rule(t, rel, dirs[rel], srcs) {
			f, err := filepath.Rel(root, v.File)
			if err != nil {
				f = v.File
			}
			t.Errorf("%s:%d: %s [%s] (package %s)", filepath.ToSlash(f), v.Line, v.Msg, v.Rule, rel)
		}
	}
}

func TestParserPackagesImportOnlyAllowlisted(t *testing.T) {
	scanPure(t, func(_ *testing.T, rel string, class pureClass, srcs []pureSrc) []pureViolation {
		return checkImports(srcs, class, rel)
	})
}

func TestParserPackagesAstBans(t *testing.T) {
	scanPure(t, func(_ *testing.T, rel string, class pureClass, srcs []pureSrc) []pureViolation {
		return checkAstBans(srcs, class, rel)
	})
}

func TestParserPackagesRecordsSelectors(t *testing.T) {
	scanPure(t, func(_ *testing.T, _ string, class pureClass, srcs []pureSrc) []pureViolation {
		return checkRecordsSelectors(srcs, class)
	})
}

func TestParserDirsHaveNoNonGoSource(t *testing.T) {
	root := repoRoot(t)
	dirs, err := pureDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range sortedDirs(dirs) {
		for _, v := range checkNonGoSource(filepath.Join(root, filepath.FromSlash(rel))) {
			f, _ := filepath.Rel(root, v.File)
			t.Errorf("%s: %s [%s]", filepath.ToSlash(f), v.Msg, v.Rule)
		}
	}
}

func TestParserPackagesHaveNoMutableState(t *testing.T) {
	scanPure(t, func(_ *testing.T, _ string, class pureClass, srcs []pureSrc) []pureViolation {
		return checkMutableState(srcs, class)
	})
}

func TestParserPackagesHaveNoSQL(t *testing.T) {
	scanPure(t, func(t *testing.T, _ string, _ pureClass, srcs []pureSrc) []pureViolation {
		return checkNoSQL(t, srcs)
	})
}

// purityRequired are the pure packages that must be scanned: a rename or a
// change of the roots that drops one fails TestPurityRulesCoverKnownPackages.
// Every plan that creates a pure package appends it here in the same commit
// (internal/recordtypes/common is listed; the six type packages and the others follow with their plans).
var purityRequired = []string{"internal/parse", "internal/recordtypes/common"}

// purePackageRE is a second, independent description of the pure roots: any
// directory under internal/ that matches it and holds a non-test Go file must
// have been scanned.
var purePackageRE = regexp.MustCompile(`^internal/(?:parse|recordtypes|decode|sqlitefile|parsers/[^/]+)(?:/|$)`)

// TestPurityRulesCoverKnownPackages: the scan visited every required package and
// every directory the independent walk finds, so a root that exists but is not
// scanned (a typo, a rename, a new directory layout) fails instead of passing vacuously.
func TestPurityRulesCoverKnownPackages(t *testing.T) {
	root := repoRoot(t)
	scanned, err := pureDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range purityRequired {
		if _, ok := scanned[rel]; !ok {
			t.Errorf("required pure package %s was not scanned (renamed, or the roots changed)", rel)
		}
	}
	err = filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		dir, _ := filepath.Rel(root, filepath.Dir(p))
		dir = filepath.ToSlash(dir)
		if !purePackageRE.MatchString(dir) {
			return nil
		}
		// the two helper packages the roots exclude on purpose
		if strings.HasPrefix(dir, dirSqlitefile+"/sqlitetest") || strings.HasPrefix(dir, dirParsers+"/parsertest") {
			return nil
		}
		if _, ok := scanned[dir]; !ok {
			t.Errorf("%s holds Go source of a pure root but was not scanned", dir)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scanned) == 0 {
		t.Fatal("no pure package was scanned")
	}
}

// TestParserPackagesArePure runs every rule over the pure packages of the tree.
// The rules are also separate tests so a failure names its rule; this is the
// single entry point the plan and the spec cite.
func TestParserPackagesArePure(t *testing.T) {
	t.Run("coverage", TestPurityRulesCoverKnownPackages)
	t.Run("P1 imports", TestParserPackagesImportOnlyAllowlisted)
	t.Run("P2 syntax-tree bans", TestParserPackagesAstBans)
	t.Run("P3 records selectors", TestParserPackagesRecordsSelectors)
	t.Run("P4 non-Go source", TestParserDirsHaveNoNonGoSource)
	t.Run("P5 package state", TestParserPackagesHaveNoMutableState)
	t.Run("P6 no SQL", TestParserPackagesHaveNoSQL)
}

// ---------------------------------------------------------------------------
// Self-tests: one per rule, so no rule can go vacuous. The violating and clean
// snippets live in testdata/pure/*.go.txt (not compiled, not scanned by the
// rules), so this package holds no violation itself.

func readPureFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "pure", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// markedLines returns the numbers of the lines of data that carry marker and no
// longer marker of the given exceptions (so "// want" does not match "// want:x").
func markedLines(data, marker string, not ...string) map[int]bool {
	out := map[int]bool{}
	for i, line := range strings.Split(data, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		skip := false
		for _, n := range not {
			skip = skip || strings.Contains(line, n)
		}
		if !skip {
			out[i+1] = true
		}
	}
	return out
}

// expectLines requires the violations to be on exactly the wanted lines: every
// marked line flagged, no other line flagged. min guards against damaged data.
func expectLines(t *testing.T, data string, want map[int]bool, vs []pureViolation, minWant int) {
	t.Helper()
	if len(want) < minWant {
		t.Fatalf("only %d lines are marked as violations (want at least %d): the test data was damaged", len(want), minWant)
	}
	lines := strings.Split(data, "\n")
	got := map[int]bool{}
	for _, v := range vs {
		got[v.Line] = true
		if !want[v.Line] {
			t.Errorf("line %d was flagged but is harmless: %s [%s: %s]", v.Line, strings.TrimSpace(lines[v.Line-1]), v.Rule, v.Msg)
		}
	}
	for l := range want {
		if !got[l] {
			t.Errorf("line %d was not flagged: %s", l, strings.TrimSpace(lines[l-1]))
		}
	}
}

const fixtureParserDir = "internal/parsers/fixture"

func TestPurityAllowlistSelfTest(t *testing.T) {
	data := readPureFixture(t, "imports.go.txt")
	srcs := parsePureSources(t, map[string]string{"imports.go": data})
	expectLines(t, data, markedLines(data, "// want"), checkImports(srcs, pcParser, fixtureParserDir), 50)

	// the class matrix: who may import what (each row names a class and an import)
	const m = module
	type row struct {
		class pureClass
		dir   string
		path  string
		ok    bool
	}
	rows := []row{
		// parse
		{pcParse, dirParse, "sync/atomic", true},
		{pcParse, dirParse, "regexp", true},
		{pcParse, dirParse, "io", true},
		{pcParse, dirParse, "context", true},
		{pcParse, dirParse, "fmt", true},
		{pcParse, dirParse, "time", true},
		{pcParse, dirParse, m + "internal/records", true},
		{pcParse, dirParse, "encoding/binary", false},
		{pcParse, dirParse, "embed", false},
		{pcParse, dirParse, "encoding/json", false},
		{pcParse, dirParse, "sync", false},
		{pcParse, dirParse, m + "internal/recordtypes/common", false},
		{pcParse, dirParse, m + "internal/evidence", false},
		// recordtypes/common
		{pcRTCommon, dirRTCommon, "encoding/base64", true},
		{pcRTCommon, dirRTCommon, "crypto/sha256", true},
		{pcRTCommon, dirRTCommon, "regexp", true},
		{pcRTCommon, dirRTCommon, "embed", true},
		{pcRTCommon, dirRTCommon, m + "internal/parse", true},
		{pcRTCommon, dirRTCommon, "encoding/json", false},
		{pcRTCommon, dirRTCommon, "io", false},
		{pcRTCommon, dirRTCommon, "context", false},
		{pcRTCommon, dirRTCommon, "sync/atomic", false},
		{pcRTCommon, dirRTCommon, m + "internal/records", false},
		{pcRTCommon, dirRTCommon, m + "internal/recordtypes/message", false},
		// recordtypes/<type>
		{pcRTType, dirRecordtype + "/message", "encoding/json", true},
		{pcRTType, dirRecordtype + "/message", "regexp", true},
		{pcRTType, dirRecordtype + "/message", m + "internal/records", true},
		{pcRTType, dirRecordtype + "/message", m + "internal/parse", true},
		{pcRTType, dirRecordtype + "/message", m + "internal/recordtypes/common", true},
		{pcRTType, dirRecordtype + "/message", m + "internal/recordtypes/call", false},
		{pcRTType, dirRecordtype + "/message", "encoding/base64", false},
		{pcRTType, dirRecordtype + "/message", "io", false},
		{pcRTType, dirRecordtype + "/message", m + "internal/decode/plist", false},
		// decode
		{pcDecode, dirDecodePlst, "howett.net/plist", true},
		{pcDecode, dirDecode + "/other", "howett.net/plist", false},
		{pcDecode, dirDecodeSQL, m + "internal/sqlitefile", true},
		{pcDecode, dirDecodePlst, m + "internal/sqlitefile", false},
		{pcDecode, dirDecodePlst, "encoding/binary", true},
		{pcDecode, dirDecodePlst, "io", true},
		{pcDecode, dirDecodePlst, "hash/crc32", true},
		{pcDecode, dirDecodeSQL, m + "internal/decode/plist", true},
		{pcDecode, dirDecodePlst, m + "internal/records", true},
		{pcDecode, dirDecodePlst, "regexp", false},
		{pcDecode, dirDecodePlst, "encoding/json", false},
		{pcDecode, dirDecodePlst, m + "internal/recordtypes/common", false},
		{pcDecode, dirDecodePlst, m + "internal/sqlitefile/sqlitetest", false},
		// sqlitefile
		{pcSqlitefile, dirSqlitefile, "encoding/binary", true},
		{pcSqlitefile, dirSqlitefile, "io", true},
		{pcSqlitefile, dirSqlitefile, m + "internal/records", true},
		{pcSqlitefile, dirSqlitefile, "regexp", false},
		{pcSqlitefile, dirSqlitefile, "embed", false},
		{pcSqlitefile, dirSqlitefile, m + "internal/decode/plist", false},
		// parsers
		{pcParser, fixtureParserDir, m + "internal/recordtypes/message", true},
		{pcParser, fixtureParserDir, m + "internal/decode/sqlitedb", true},
		{pcParser, fixtureParserDir, m + "internal/sqlitefile", true},
		{pcParser, fixtureParserDir, m + "internal/parse", true},
		{pcParser, fixtureParserDir, "embed", true},
		{pcParser, fixtureParserDir, "encoding/hex", true},
		{pcParser, fixtureParserDir, m + "internal/sqlitefile/sqlitetest", false},
		{pcParser, fixtureParserDir, m + "internal/parsers/other", false},
		{pcParser, fixtureParserDir, m + "internal/parsers/parsertest", false},
		{pcParser, fixtureParserDir, "regexp", false},
		{pcParser, fixtureParserDir, "encoding/json", false},
		{pcParser, fixtureParserDir, "sync/atomic", false},
		{pcParser, fixtureParserDir, "golang.org/x/text/encoding/charmap", false},
		{pcParser, fixtureParserDir, "howett.net/plist", false},
		// nobody imports the evidence package or the CLI
		{pcParse, dirParse, m + "internal/evidence", false},
		{pcDecode, dirDecodePlst, m + "internal/evidence", false},
		{pcSqlitefile, dirSqlitefile, m + "internal/evidence", false},
		{pcRTType, dirRecordtype + "/message", m + "internal/evidence", false},
		{pcRTCommon, dirRTCommon, m + "internal/evidence", false},
		{pcParser, fixtureParserDir, m + "internal/evidence", false},
		{pcParser, fixtureParserDir, m + "internal/cli", false},
	}
	for _, r := range rows {
		src := "package x\n\nimport _ " + strconv.Quote(r.path) + "\n"
		got := checkImports(parsePureSources(t, map[string]string{"x.go": src}), r.class, r.dir)
		if (len(got) == 0) != r.ok {
			t.Errorf("%s (%s) importing %s: refused=%v, want refused=%v", r.class, r.dir, r.path, len(got) != 0, !r.ok)
		}
	}
	// a test file is not scanned, and the class decides, not the file name
	if got := checkImports(parsePureSources(t, map[string]string{"x.go": "package x\n\nimport \"os\"\n"}), pcParser, fixtureParserDir); len(got) != 1 {
		t.Errorf("os in a parser: %v", got)
	}
}

func TestPurityAstBansSelfTest(t *testing.T) {
	data := readPureFixture(t, "astbans.go.txt")
	srcs := parsePureSources(t, map[string]string{"astbans.go": data})
	expectLines(t, data, markedLines(data, "// want"), checkAstBans(srcs, pcParser, fixtureParserDir), 40)

	// a package that declares its own print is not calling the builtin
	own := "package x\n\nfunc print(s string) {}\n\nfunc f() { print(\"x\") }\n"
	if got := checkAstBans(parsePureSources(t, map[string]string{"x.go": own}), pcParser, fixtureParserDir); len(got) != 0 {
		t.Errorf("a package's own print function was flagged: %v", got)
	}
	// dot imports of the packages the rules resolve would hide the calls
	for _, p := range []string{"fmt", "log", "time", "context", "runtime"} {
		src := "package x\n\nimport . " + strconv.Quote(p) + "\n"
		if got := checkAstBans(parsePureSources(t, map[string]string{"x.go": src}), pcParser, fixtureParserDir); len(got) != 1 {
			t.Errorf("dot import of %s: %v, want one violation", p, got)
		}
	}
	// //go:embed is allowed only in the data packages
	embed := "package x\n\nimport _ \"embed\"\n\n//go:embed data.bin\nvar data []byte\n"
	for class, want := range map[pureClass]int{pcParse: 1, pcSqlitefile: 1, pcRTCommon: 0, pcRTType: 0, pcDecode: 0, pcParser: 0} {
		got := checkAstBans(parsePureSources(t, map[string]string{"x.go": embed}), class, fixtureParserDir)
		if len(got) != want {
			t.Errorf("//go:embed in %s: %d violations, want %d (%v)", class, len(got), want, got)
		}
	}
	// time.Unix, UnixMilli and UnixMicro yield host-zone times: banned everywhere except internal/decode/ts
	for _, fn := range []string{"Unix(1, 0)", "UnixMilli(1)", "UnixMicro(1)"} {
		src := "package x\n\nimport \"time\"\n\nvar _ = time." + fn + ".UTC()\n"
		for _, tc := range []struct {
			class pureClass
			dir   string
			want  int
		}{
			{pcParser, fixtureParserDir, 1},
			{pcParse, dirParse, 1},
			{pcRTType, "internal/recordtypes/message", 1},
			{pcDecode, "internal/decode/plist", 1},
			{pcDecode, "internal/decode/ts", 0},
			{pcDecode, "internal/decode/tsx", 1},
		} {
			if got := checkAstBans(parsePureSources(t, map[string]string{"x.go": src}), tc.class, tc.dir); len(got) != tc.want {
				t.Errorf("time.%s in %s: %d violations, want %d (%v)", fn, tc.dir, len(got), tc.want, got)
			}
		}
	}
	// the exemption of decode/ts covers the Unix constructors only
	for _, banned := range []string{"time.Now()", "time.Local", "context.WithTimeout"} {
		src := "package x\n\nimport (\n\t\"context\"\n\t\"time\"\n)\n\nvar _ = " + banned + "\nvar _ = context.Background\nvar _ = time.UTC\n"
		if got := checkAstBans(parsePureSources(t, map[string]string{"x.go": src}), pcDecode, "internal/decode/ts"); len(got) != 1 {
			t.Errorf("%s in decode/ts: %v, want one violation", banned, got)
		}
	}
	// the violation of one ban is reported once per construct, with its line
	one := "package x\n\nimport \"time\"\n\nvar _ = time.Now()\n"
	got := checkAstBans(parsePureSources(t, map[string]string{"x.go": one}), pcParser, fixtureParserDir)
	if len(got) != 1 || got[0].Line != 5 || !strings.Contains(got[0].Msg, "time.Now") {
		t.Errorf("time.Now violation = %v", got)
	}
}

func TestPurityRecordsSelectorsSelfTest(t *testing.T) {
	data := readPureFixture(t, "records.go.txt")
	srcs := parsePureSources(t, map[string]string{"records.go": data})
	// recordtypes/<type> may call SetValidator in init; every other class may not
	expectLines(t, data, markedLines(data, "// want", "// want:non-rttype"), checkRecordsSelectors(srcs, pcRTType), 10)
	every := markedLines(data, "// want")
	for _, class := range []pureClass{pcParse, pcRTCommon, pcDecode, pcSqlitefile, pcParser} {
		expectLines(t, data, every, checkRecordsSelectors(srcs, class), 10)
	}

	// importing the evidence package is refused with its own message, in every class
	ev := "package x\n\nimport \"" + evidencePkg + "\"\n"
	for _, class := range []pureClass{pcParse, pcRTCommon, pcRTType, pcDecode, pcSqlitefile, pcParser} {
		got := checkRecordsSelectors(parsePureSources(t, map[string]string{"x.go": ev}), class)
		if len(got) != 1 || !strings.Contains(got[0].Msg, "imports internal/evidence") {
			t.Errorf("%s importing the evidence package: %v", class, got)
		}
	}
	// a dot import hides the selectors, so it is refused; an unrelated package called records is not
	dot := "package x\n\nimport . \"" + recordsPkg + "\"\n"
	if got := checkRecordsSelectors(parsePureSources(t, map[string]string{"x.go": dot}), pcParser); len(got) != 1 {
		t.Errorf("dot import of records: %v", got)
	}
	other := "package x\n\nimport records \"example.org/records\"\n\nvar _ = records.NewWriter\n"
	if got := checkRecordsSelectors(parsePureSources(t, map[string]string{"x.go": other}), pcParser); len(got) != 0 {
		t.Errorf("another package named records was flagged: %v", got)
	}
	// importing records twice under two names: every local name is checked, in either order
	imp := func(names ...string) string {
		s := "package x\n\nimport (\n"
		for _, n := range names {
			s += "\t" + n + " \"" + recordsPkg + "\"\n"
		}
		return s + ")\n"
	}
	for name, tc := range map[string]struct {
		src  string
		want int
	}{
		"two aliases":              {imp("r1", "r2") + "\nvar _ = r1.NewWriter\nvar _ = r2.RegisterType\n", 2},
		"two aliases, other order": {imp("r2", "r1") + "\nvar _ = r1.NewWriter\nvar _ = r2.RegisterType\n", 2},
		"plain then alias":         {imp("", "r1") + "\nvar _ = r1.NewWriter\nvar _ = records.RegisterType\n", 2},
		"alias then plain":         {imp("r1", "") + "\nvar _ = r1.NewWriter\nvar _ = records.RegisterType\n", 2},
		"three names":              {imp("a", "b", "c") + "\nvar _ = a.NewWriter\nvar _ = b.NewWriter\nvar _ = c.NewWriter\n", 3},
		"data types under both":    {imp("r1", "r2") + "\nvar _ r1.Record\nvar _ r2.Range\n", 0},
	} {
		if got := checkRecordsSelectors(parsePureSources(t, map[string]string{"x.go": tc.src}), pcParser); len(got) != tc.want {
			t.Errorf("%s: %d violations, want %d: %v", name, len(got), tc.want, got)
		}
	}
	// the local name decides: an alias does not hide the call
	alias := "package x\n\nimport r \"" + recordsPkg + "\"\n\nvar _ = r.NewWriter\n"
	if got := checkRecordsSelectors(parsePureSources(t, map[string]string{"x.go": alias}), pcParser); len(got) != 1 {
		t.Errorf("aliased records.NewWriter: %v", got)
	}
}

func TestPurityNonGoSourceSelfTest(t *testing.T) {
	dir := t.TempDir()
	bad := []string{"x.s", "y.S", "z.c", "w.h", "a.cc", "b.cpp", "c.m", "d.f", "e.syso", "f.swig", "g.cxx", "h.mm"}
	good := []string{"ok.go", "ok_test.go", "README.md", "data.json", "notes.txt", "gen.sh"}
	for _, n := range append(append([]string{}, bad...), good...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// a subdirectory (testdata) is not looked into by this directory's check
	if err := os.MkdirAll(filepath.Join(dir, "testdata"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testdata", "fixture.c"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range checkNonGoSource(dir) {
		got[filepath.Base(v.File)] = true
	}
	for _, n := range bad {
		if !got[n] {
			t.Errorf("%s was not flagged", n)
		}
	}
	for _, n := range good {
		if got[n] {
			t.Errorf("%s was flagged", n)
		}
	}
	if got["fixture.c"] {
		t.Error("testdata/fixture.c was flagged: testdata is exempt")
	}
	if vs := checkNonGoSource(filepath.Join(dir, "missing")); len(vs) != 1 {
		t.Errorf("an unreadable directory must be reported, got %v", vs)
	}
}

func TestPurityStateSelfTest(t *testing.T) {
	data := readPureFixture(t, "state.go.txt")
	srcs := parsePureSources(t, map[string]string{"state.go": data})
	expectLines(t, data, markedLines(data, "// want"), checkMutableState(srcs, pcParser), 30)

	// uses in another file of the package count, and a local variable of the same name is not the table
	a := "package x\n\nvar table = [...]int{1, 2}\n\nfunc read() int { return table[0] }\n"
	b := "package x\n\nfunc f() {\n\ttable[0] = 9\n}\n\nfunc g() {\n\ttable := []int{1}\n\ttable[0] = 9\n}\n"
	got := checkMutableState(parsePureSources(t, map[string]string{"a.go": a, "b.go": b}), pcParser)
	if len(got) != 1 || got[0].File != "b.go" || got[0].Line != 4 {
		t.Errorf("violations = %v, want only b.go:4", got)
	}
}

// TestPurityEmbedVarSelfTest: a //go:embed variable is the one package-level var that
// carries data without a constant initialiser. P5 exempts exactly an unexported
// string or embed.FS var with the directive, alone in its spec, in a data class;
// writing to it is still flagged, and nothing else with a directive is exempt.
func TestPurityEmbedVarSelfTest(t *testing.T) {
	const head = "package x\n\nimport \"embed\"\n\nvar _ embed.FS\n\n"
	dataClasses := []pureClass{pcRTCommon, pcRTType, pcDecode, pcParser}
	otherClasses := []pureClass{pcParse, pcSqlitefile}
	cases := []struct {
		name string
		decl string
		ok   bool // exempt in a data class
	}{
		{"string", "//go:embed a.txt\nvar text string\n", true},
		{"embed.FS", "//go:embed assets/*\nvar assets embed.FS\n", true},
		{"directive in a group", "var (\n\t//go:embed a.txt\n\ttext string\n)\n", true},
		{"directive with a blank line after", "//go:embed a.txt\n\nvar text string\n", true},
		{"no directive", "var text string\n", false},
		{"byte slice", "//go:embed a.bin\nvar data []byte\n", false},
		{"exported", "//go:embed a.txt\nvar Text string\n", false},
		{"two names", "//go:embed a.txt\nvar a, b string\n", false},
		{"with an initialiser", "//go:embed a.txt\nvar text string = \"x\"\n", false},
		{"another type", "//go:embed a.txt\nvar n int\n", false},
		{"other directive", "//go:generate echo\nvar text string\n", false},
		{"directive on a different var", "//go:embed a.txt\nvar text string\n\nvar other string\n", false},
	}
	for _, tc := range cases {
		for _, class := range dataClasses {
			got := checkMutableState(parsePureSources(t, map[string]string{"x.go": head + tc.decl}), class)
			want := 0
			switch tc.name {
			case "directive on a different var":
				want = 1 // only the plain `other` is state
			case "two names":
				want = 2
			default:
				if !tc.ok {
					want = 1
				}
			}
			if len(got) != want {
				t.Errorf("%s in %s: %d violations, want %d: %v", tc.name, class, len(got), want, got)
			}
		}
	}
	// outside the data classes the directive exempts nothing (P2 flags the directive itself)
	for _, class := range otherClasses {
		got := checkMutableState(parsePureSources(t, map[string]string{"x.go": head + "//go:embed a.txt\nvar text string\n"}), class)
		if len(got) != 1 {
			t.Errorf("embed var in %s: %d violations, want 1: %v", class, len(got), got)
		}
	}
	// the exempt var stays read-only: assigning, incrementing or taking its address is flagged, reading is not
	use := head + "//go:embed a.txt\nvar text string\n\nfunc f() string {\n\tx := text + \"!\"\n\treturn x + text[:1]\n}\n"
	if got := checkMutableState(parsePureSources(t, map[string]string{"x.go": use}), pcParser); len(got) != 0 {
		t.Errorf("reading an embed var was flagged: %v", got)
	}
	for _, write := range []string{"text = \"y\"", "text += \"y\"", "_ = &text"} {
		src := head + "//go:embed a.txt\nvar text string\n\nfunc f() {\n\t" + write + "\n}\n"
		if got := checkMutableState(parsePureSources(t, map[string]string{"x.go": src}), pcParser); len(got) != 1 {
			t.Errorf("%q: %d violations, want 1: %v", write, len(got), got)
		}
	}
	// the P1 table and P2 allow the embed import and the directive in the same classes
	src := head + "//go:embed a.txt\nvar text string\n"
	for _, class := range dataClasses {
		if got := checkAstBans(parsePureSources(t, map[string]string{"x.go": src}), class, fixtureParserDir); len(got) != 0 {
			t.Errorf("P2 flagged the embed directive in %s: %v", class, got)
		}
	}
}

func TestNoSQLScannerSelfTest(t *testing.T) {
	data := readPureFixture(t, "sql.go.txt")
	srcs := parsePureSources(t, map[string]string{"sql.go": data})
	expectLines(t, data, markedLines(data, "// want"), checkNoSQL(t, srcs), 30)

	// every statement shape of the rule, in odd spacing and case, as one string each
	for _, sql := range []string{
		"SELECT x FROM t", "select\tx\r\nfrom\r\nt", "SELECT  *  FROM  t", "INSERT   INTO t(a) VALUES(1)", "UPDATE t SET a=1", "DELETE FROM t",
		"CREATE TABLE t(a)", "CREATE INDEX i ON t(a)", "CREATE TRIGGER x AFTER INSERT ON t BEGIN SELECT 1; END", "CREATE VIEW v AS SELECT 1",
		"ALTER TABLE t RENAME TO u", "PRAGMA user_version",
		"PRAGMA cache_size = 10", "ATTACH DATABASE 'f' AS d", "attach 'f.db' as d",
	} {
		src := "package x\n\nvar q = " + strconv.Quote(sql) + "\n"
		if len(checkNoSQL(t, parsePureSources(t, map[string]string{"x.go": src}))) != 1 {
			t.Errorf("%q is not flagged", sql)
		}
	}
	// the single-writer scanner and this one share normalizeSQL: quoting and comments cannot hide a statement
	for _, sql := range []string{`SELECT "a" FROM "t"`, "SELECT [a] FROM [t]", "SELECT a /* x */ FROM t", "SELECT a -- x\nFROM t"} {
		if !looksLikeSQL(sql) {
			t.Errorf("%q is not recognised", sql)
		}
	}
	for _, text := range []string{"", "select", "from", "select the best option from the list", "Please update the settings", "failed to delete from the cache"} {
		if looksLikeSQL(text) {
			t.Errorf("%q was flagged", text)
		}
	}
}

// TestPurityCleanSnippet: typical parser code passes every rule, so the rules do
// not fire on ordinary code.
func TestPurityCleanSnippet(t *testing.T) {
	data := readPureFixture(t, "clean.go.txt")
	srcs := parsePureSources(t, map[string]string{"clean.go": data})
	var all []pureViolation
	all = append(all, checkImports(srcs, pcParser, fixtureParserDir)...)
	all = append(all, checkAstBans(srcs, pcParser, fixtureParserDir)...)
	all = append(all, checkRecordsSelectors(srcs, pcParser)...)
	all = append(all, checkMutableState(srcs, pcParser)...)
	all = append(all, checkNoSQL(t, srcs)...)
	for _, v := range all {
		t.Errorf("clean snippet flagged: %s", v)
	}
}

// TestPurityClassOf: which directories are pure roots and with which class.
func TestPurityClassOf(t *testing.T) {
	for rel, want := range map[string]pureClass{
		"internal/parse":                     pcParse,
		"internal/recordtypes/common":        pcRTCommon,
		"internal/recordtypes/message":       pcRTType,
		"internal/recordtypes":               pcRTType,
		"internal/decode/plist":              pcDecode,
		"internal/decode/sqlitedb":           pcDecode,
		"internal/sqlitefile":                pcSqlitefile,
		"internal/parsers/androidmms":        pcParser,
		"internal/parsers/androidmms/helper": pcParser,
		"internal/parsers/ios/sms":           pcParser,
	} {
		if got, ok := classOf(rel); !ok || got != want {
			t.Errorf("classOf(%s) = %v, %v; want %v", rel, got, ok, want)
		}
	}
	for _, rel := range []string{
		"internal/parsers", "internal/parsers/parsertest", "internal/parsers/parsertest/x", "internal/sqlitefile/sqlitetest",
		"internal/cli", "internal/records", "internal/parser", "internal/parsex", "internal/artparse", "internal/evidence", "cmd/minutiae",
	} {
		if got, ok := classOf(rel); ok {
			t.Errorf("classOf(%s) = %v, want it to be outside the pure roots", rel, got)
		}
	}
}

// TestPureDirsDiscovery: the scan finds every pure directory of a synthetic tree,
// including directories that hold no Go file (P4 needs them) and skipping testdata
// and the test helper packages.
func TestPureDirsDiscovery(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{
		"internal/parse", "internal/parse/testdata", "internal/recordtypes/common", "internal/recordtypes/message",
		"internal/decode/plist", "internal/decode/plist/testdata/x", "internal/sqlitefile", "internal/sqlitefile/sqlitetest",
		"internal/parsers", "internal/parsers/parsertest", "internal/parsers/androidmms", "internal/parsers/androidmms/testdata",
		"internal/cli", "internal/records",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	got, err := pureDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]pureClass{
		"internal/parse": pcParse, "internal/recordtypes": pcRTType, "internal/recordtypes/common": pcRTCommon,
		"internal/recordtypes/message": pcRTType, "internal/decode": pcDecode, "internal/decode/plist": pcDecode,
		"internal/sqlitefile": pcSqlitefile, "internal/parsers/androidmms": pcParser,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("pure dirs = %v, want %v", got, want)
	}
	// a root that does not exist is skipped
	if got, err := pureDirs(t.TempDir()); err != nil || len(got) != 0 {
		t.Errorf("empty tree: %v, %v", got, err)
	}
}

// TestPurityScannerSelfTest runs the self-test of every rule.
func TestPurityScannerSelfTest(t *testing.T) {
	t.Run("P1 allowlist", TestPurityAllowlistSelfTest)
	t.Run("P2 syntax-tree bans", TestPurityAstBansSelfTest)
	t.Run("P3 records selectors", TestPurityRecordsSelectorsSelfTest)
	t.Run("P4 non-Go source", TestPurityNonGoSourceSelfTest)
	t.Run("P5 package state", TestPurityStateSelfTest)
	t.Run("P5 embed vars", TestPurityEmbedVarSelfTest)
	t.Run("P6 no SQL", TestNoSQLScannerSelfTest)
	t.Run("clean snippet", TestPurityCleanSnippet)
	t.Run("class of a directory", TestPurityClassOf)
	t.Run("discovery", TestPureDirsDiscovery)
}

// embedVar reports whether the si-th spec of the di-th declaration of f is the one
// package-level var form a //go:embed directive allows: a single unexported name of
// type string or embed.FS, with no initialiser, preceded by the directive (only blank
// lines and // comments may stand between them), in a class that may embed. The
// directive is looked for by position, so blank lines between it and the var, which
// the go tool allows, do not hide it. A []byte var is refused: it is mutable and
// aliasable. Writes to the var are still flagged (kind vkEmbed is read-only).
func embedVar(f *ast.File, di, si int, vs *ast.ValueSpec, names map[string]string, class pureClass) bool {
	if !embedAllowed.has(class) || len(vs.Names) != 1 || len(vs.Values) != 0 || ast.IsExported(vs.Names[0].Name) || vs.Names[0].Name == "_" {
		return false
	}
	switch t := vs.Type.(type) {
	case *ast.Ident:
		if t.Name != "string" || t.Obj != nil {
			return false
		}
	case *ast.SelectorExpr:
		if p, name, ok := pkgSelector(t, names); !ok || p != "embed" || name != "FS" {
			return false
		}
	default:
		return false
	}
	gd := f.Decls[di].(*ast.GenDecl)
	var lo, hi token.Pos
	switch {
	case gd.Lparen.IsValid() && si == 0:
		lo, hi = gd.Lparen, vs.Pos()
	case gd.Lparen.IsValid():
		lo, hi = gd.Specs[si-1].End(), vs.Pos()
	case di == 0:
		lo, hi = f.Name.End(), gd.Pos()
	default:
		lo, hi = f.Decls[di-1].End(), gd.Pos()
	}
	for _, cg := range f.Comments {
		if cg.Pos() < lo || cg.End() > hi {
			continue
		}
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:embed ") {
				return true
			}
		}
	}
	return false
}
