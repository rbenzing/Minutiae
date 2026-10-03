package ios

import (
	"io"
	"strings"
	"testing"

	"github.com/danielpaulus/go-ios/ios/afc"
)

// A go-ios AFC client without a connection panics on every call.
func TestAFCClientConvertsGoIOSPanicsToErrors(t *testing.T) {
	a := afcClient{c: &afc.Client{}}
	check := func(op string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "panic") {
			t.Errorf("%s: err = %v", op, err)
		}
	}
	_, err := a.List("/")
	check("List", err)
	_, err = a.Stat("/x")
	check("Stat", err)
	_, err = a.Open("/x")
	check("Open", err)
	check("Close", a.Close())
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("bad AFC packet") }
func (panicReader) Close() error             { panic("bad AFC packet") }

func TestAFCFileConvertsPanicsToErrors(t *testing.T) {
	f := safeFile{panicReader{}}
	if _, err := f.Read(make([]byte, 1)); err == nil || err == io.EOF || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("Read err = %v", err)
	}
	if err := f.Close(); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("Close err = %v", err)
	}
}
