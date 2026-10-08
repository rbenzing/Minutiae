package parse

import (
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

// graphWalker walks the static type graph reachable from a type: struct
// fields, element and key types, function signatures, and the method set of
// every concrete type and every interface. It records every type that must
// not be reachable from anything a parser receives.
type graphWalker struct {
	exportedOnly bool // follow exported fields only: what a parser can reach without reflect/unsafe
	seen         map[reflect.Type]bool
	problems     []string
}

var bannedMethodNames = map[string]bool{"Write": true, "WriteAt": true, "Seek": true, "Close": true, "Fd": true}

// recordsDataTypes are the only types of package records a parser may hold.
var recordsDataTypes = map[string]bool{"Record": true, "Range": true, "Time": true, "NamedTime": true, "Basis": true}

func newGraphWalker(exportedOnly bool) *graphWalker {
	return &graphWalker{exportedOnly: exportedOnly, seen: map[reflect.Type]bool{}}
}

func bannedPackage(p string) bool {
	switch {
	case p == "os", strings.HasPrefix(p, "os/"):
		return true
	case p == "net", strings.HasPrefix(p, "net/"):
		return true
	case p == "database/sql", strings.HasPrefix(p, "database/sql/"):
		return true
	case p == "syscall", p == "unsafe":
		return true
	case strings.HasSuffix(p, "/internal/evidence"), strings.Contains(p, "/internal/evidence/"):
		return true
	}
	return false
}

func (w *graphWalker) problem(format string, args ...any) {
	w.problems = append(w.problems, fmt.Sprintf(format, args...))
}

func (w *graphWalker) walk(t reflect.Type, path string) {
	if w.seen[t] {
		return
	}
	w.seen[t] = true
	if p := t.PkgPath(); p != "" {
		if bannedPackage(p) {
			w.problem("%s: type %s comes from banned package %s", path, t, p)
		}
		if strings.HasSuffix(p, "/internal/records") && !recordsDataTypes[t.Name()] {
			w.problem("%s: records.%s is not a data type a parser may hold", path, t.Name())
		}
	}
	switch t.Kind() {
	case reflect.Interface:
		for i := range t.NumMethod() {
			m := t.Method(i)
			if bannedMethodNames[m.Name] {
				w.problem("%s: interface %s has method %s", path, t, m.Name)
			}
			w.walk(m.Type, path+"."+m.Name)
		}
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		w.walk(t.Elem(), path)
	case reflect.Map:
		w.walk(t.Key(), path)
		w.walk(t.Elem(), path)
	case reflect.Struct:
		for i := range t.NumField() {
			f := t.Field(i)
			if w.exportedOnly && !f.IsExported() {
				continue
			}
			w.walk(f.Type, path+"."+f.Name)
		}
	case reflect.Func:
		for i := range t.NumIn() {
			w.walk(t.In(i), path+"(in)")
		}
		for i := range t.NumOut() {
			w.walk(t.Out(i), path+"(out)")
		}
	}
	if t.Kind() != reflect.Interface && t.Kind() != reflect.Pointer {
		pt := reflect.PointerTo(t)
		for i := range pt.NumMethod() {
			m := pt.Method(i)
			if bannedMethodNames[m.Name] {
				w.problem("%s: type %s has method %s", path, t, m.Name)
			}
			w.walk(m.Type, path+"."+m.Name)
		}
	}
}

// Deliberately bad shapes: the walker must report every one of them.
type (
	badFile     struct{ F *os.File }
	badWriter   struct{ W io.Writer }
	badCloser   struct{ C interface{ Close() error } }
	badFd       struct{ F interface{ Fd() uintptr } }
	badNet      struct{ C net.Conn }
	badSQL      struct{ D *sql.DB }
	badEvidence struct{ C *evidence.Case }
	badWriterDB struct{ W *records.Writer }
	badDeep     struct{ M map[string][]func() *os.File }
	badBudget   struct{ B *Budget }
	badFactory  struct{}
)

func (badFactory) Open() io.WriteSeeker { return nil }

type badViaMethod struct{ F badFactory }

type goodShape struct {
	R io.ReaderAt
	N int
	S []string
}

func TestContractWalkerCatchesBadShapes(t *testing.T) {
	bad := map[string]reflect.Type{
		"os.File":           reflect.TypeFor[badFile](),
		"io.Writer":         reflect.TypeFor[badWriter](),
		"Close interface":   reflect.TypeFor[badCloser](),
		"Fd interface":      reflect.TypeFor[badFd](),
		"net.Conn":          reflect.TypeFor[badNet](),
		"sql.DB":            reflect.TypeFor[badSQL](),
		"evidence.Case":     reflect.TypeFor[badEvidence](),
		"records.Writer":    reflect.TypeFor[badWriterDB](),
		"nested in a map":   reflect.TypeFor[badDeep](),
		"via a method":      reflect.TypeFor[badViaMethod](),
		"a bare os.File":    reflect.TypeFor[os.File](),
		"a func returning":  reflect.TypeFor[func() io.WriteCloser](),
		"a channel of file": reflect.TypeFor[chan *os.File](),
	}
	for name, typ := range bad {
		w := newGraphWalker(false)
		w.walk(typ, name)
		if len(w.problems) == 0 {
			t.Errorf("%s: the walker found nothing wrong: it is vacuous", name)
		}
	}
	// Unexported fields are skipped in exported-only mode but not in the full walk.
	type hidden struct{ _ *os.File }
	full := newGraphWalker(false)
	full.walk(reflect.TypeFor[hidden](), "hidden")
	if len(full.problems) == 0 {
		t.Error("the full walk does not follow unexported fields")
	}
	exp := newGraphWalker(true)
	exp.walk(reflect.TypeFor[hidden](), "hidden")
	if len(exp.problems) != 0 {
		t.Error("the exported-only walk followed an unexported field")
	}
	w := newGraphWalker(true)
	w.walk(reflect.TypeFor[badBudget](), "badBudget")
	if !w.seen[reflect.TypeFor[Budget]()] {
		t.Error("the exported-only walk does not reach the host Budget through an exported field: the Budget check below is vacuous")
	}
	good := newGraphWalker(false)
	good.walk(reflect.TypeFor[goodShape](), "goodShape")
	if len(good.problems) != 0 {
		t.Errorf("a harmless shape was reported: %v", good.problems)
	}
	if len(good.seen) < 3 {
		t.Errorf("the walker saw only %d types of a harmless shape", len(good.seen))
	}
}

// contractRoots are the types a parser receives or implements.
func contractRoots() map[string]reflect.Type {
	return map[string]reflect.Type{
		"Input":       reflect.TypeFor[Input](),
		"Artifact":    reflect.TypeFor[Artifact](),
		"SourceInfo":  reflect.TypeFor[SourceInfo](),
		"Limits":      reflect.TypeFor[Limits](),
		"BudgetView":  reflect.TypeFor[BudgetView](),
		"Lookuper":    reflect.TypeFor[Lookuper](),
		"Emitter":     reflect.TypeFor[Emitter](),
		"Parser":      reflect.TypeFor[Parser](),
		"Meta":        reflect.TypeFor[Meta](),
		"Row":         reflect.TypeFor[Row](),
		"MapContext":  reflect.TypeFor[MapContext](),
		"TableMapper": reflect.TypeFor[TableMapping](),
	}
}

func TestContractExposesNoWritableHandles(t *testing.T) {
	roots := contractRoots()
	names := make([]string, 0, len(roots))
	for n := range roots {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, exportedOnly := range []bool{false, true} {
		for _, n := range names {
			w := newGraphWalker(exportedOnly)
			w.walk(roots[n], n)
			for _, p := range w.problems {
				t.Errorf("exportedOnly=%v: %s", exportedOnly, p)
			}
			if exportedOnly && w.seen[reflect.TypeFor[Budget]()] {
				t.Errorf("%s reaches the host-owned Budget through its exported surface; a parser may only hold a BudgetView", n)
			}
			if len(w.seen) < 2 {
				t.Errorf("%s: the walker saw %d types", n, len(w.seen))
			}
		}
	}

	f, ok := reflect.TypeFor[Artifact]().FieldByName("R")
	if !ok || f.Type != reflect.TypeFor[io.ReaderAt]() {
		t.Errorf("Artifact.R must be exactly io.ReaderAt, got %v", f.Type)
	}
	b, ok := reflect.TypeFor[Input]().FieldByName("Budget")
	if !ok || b.Type != reflect.TypeFor[*BudgetView]() {
		t.Errorf("Input.Budget must be a *BudgetView, got %v", b.Type)
	}
}
