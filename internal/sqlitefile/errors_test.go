package sqlitefile_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func TestErrorsIsAndAs(t *testing.T) {
	plain := &sqlitefile.NotSQLiteError{Reason: sqlitefile.ReasonBadMagic, Size: 12}
	if !errors.Is(plain, sqlitefile.ErrNotSQLite) {
		t.Error("NotSQLiteError must match ErrNotSQLite")
	}
	if errors.Is(plain, sqlitefile.ErrLooksEncrypted) {
		t.Error("NotSQLiteError without the hint must not match ErrLooksEncrypted")
	}
	if errors.Is(plain, sqlitefile.ErrCorrupt) {
		t.Error("NotSQLiteError must not match ErrCorrupt")
	}
	hinted := &sqlitefile.NotSQLiteError{Reason: sqlitefile.ReasonPage1Invalid, Size: 8192, Entropy: 7.91, LooksEncrypted: true}
	wrapped := fmt.Errorf("open x: %w", hinted)
	if !errors.Is(wrapped, sqlitefile.ErrNotSQLite) || !errors.Is(wrapped, sqlitefile.ErrLooksEncrypted) {
		t.Error("hinted NotSQLiteError must match both sentinels through a wrap")
	}
	var nse *sqlitefile.NotSQLiteError
	if !errors.As(wrapped, &nse) || nse.Reason != sqlitefile.ReasonPage1Invalid || nse.Size != 8192 {
		t.Errorf("errors.As lost the fields: %+v", nse)
	}
	for _, r := range []sqlitefile.NotSQLiteReason{sqlitefile.ReasonEmpty, sqlitefile.ReasonTooSmall, sqlitefile.ReasonBadMagic, sqlitefile.ReasonPage1Invalid} {
		if !strings.Contains((&sqlitefile.NotSQLiteError{Reason: r}).Error(), string(r)) {
			t.Errorf("message of %q does not name the reason", r)
		}
	}
	if !strings.Contains(hinted.Error(), "encrypted") {
		t.Errorf("hinted message must mention the encryption hint: %q", hinted.Error())
	}
	if strings.Contains(plain.Error(), "encrypted") {
		t.Errorf("unhinted message must not mention encryption: %q", plain.Error())
	}

	ce := &sqlitefile.CorruptError{File: sqlitefile.FileWAL, Page: 7, Reason: fmt.Sprintf("name %q", "a\x00b")}
	cw := fmt.Errorf("scan: %w", ce)
	if !errors.Is(cw, sqlitefile.ErrCorrupt) {
		t.Error("CorruptError must match ErrCorrupt")
	}
	if errors.Is(cw, sqlitefile.ErrNotSQLite) || errors.Is(cw, sqlitefile.ErrInternal) {
		t.Error("CorruptError matches an unrelated sentinel")
	}
	var cce *sqlitefile.CorruptError
	if !errors.As(cw, &cce) || cce.File != sqlitefile.FileWAL || cce.Page != 7 {
		t.Errorf("errors.As lost the fields: %+v", cce)
	}
	msg := ce.Error()
	if !strings.Contains(msg, "wal") || !strings.Contains(msg, "7") || !strings.Contains(msg, `a\x00b`) {
		t.Errorf("corrupt message lacks file, page or the quoted reason: %q", msg)
	}
	if strings.ContainsRune(msg, 0) {
		t.Errorf("a raw NUL reached the message: %q", msg)
	}

	pe := &sqlitefile.PanicError{Value: "boom", Stack: "frames"}
	if !errors.Is(pe, sqlitefile.ErrInternal) || errors.Is(pe, sqlitefile.ErrCorrupt) {
		t.Error("PanicError must match ErrInternal only")
	}
	if !strings.Contains(pe.Error(), "boom") || strings.Contains(pe.Error(), "frames") {
		t.Errorf("panic message must carry the value and not the stack: %q", pe.Error())
	}
}

func TestFileKindString(t *testing.T) {
	for k, want := range map[sqlitefile.FileKind]string{sqlitefile.FileDB: "db", sqlitefile.FileWAL: "wal", sqlitefile.FileJournal: "journal"} {
		if got := k.String(); got != want {
			t.Errorf("FileKind(%d).String() = %q, want %q", k, got, want)
		}
	}
	if sqlitefile.FileDB != 1 || sqlitefile.FileWAL != 2 || sqlitefile.FileJournal != 3 {
		t.Error("FileKind values are pinned: db 1, wal 2, journal 3")
	}
	if sqlitefile.FileKind(0).String() == "" || sqlitefile.FileKind(99).String() == "" {
		t.Error("an unknown FileKind must still print something")
	}
}

func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{
		sqlitefile.ErrNotSQLite, sqlitefile.ErrLooksEncrypted, sqlitefile.ErrCorrupt, sqlitefile.ErrBudget,
		sqlitefile.ErrLimit, sqlitefile.ErrPageUnavailable, sqlitefile.ErrNotFound, sqlitefile.ErrWithoutRowid,
		sqlitefile.ErrAlreadyAttached, sqlitefile.ErrInternal,
	}
	for i, a := range all {
		if !strings.HasPrefix(a.Error(), "sqlitefile: ") {
			t.Errorf("%q lacks the package prefix", a)
		}
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("%q matches %q", a, b)
			}
		}
	}
}
