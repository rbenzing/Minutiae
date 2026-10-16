package dbrecover

import (
	"errors"
	"strings"
	"testing"
)

func TestRefusalErrorUnwraps(t *testing.T) {
	e := &RefusalError{Reason: ReasonNotSQLite, Detail: "app.db is not a database", Err: ErrNotSQLite}
	if !errors.Is(e, ErrNotSQLite) {
		t.Error("RefusalError does not unwrap to its cause")
	}
	if s := e.Error(); !strings.Contains(s, "not-sqlite") || !strings.Contains(s, "app.db is not a database") {
		t.Errorf("text %q", s)
	}
	if got := (&RefusalError{Reason: ReasonBadOption}).Error(); got != "bad-option" {
		t.Errorf("no detail: %q", got)
	}
	for _, r := range []string{ReasonNotSQLite, ReasonIneligibleKind, ReasonLiveUnavailable, ReasonEngineRefuses, ReasonSiblingUnusable, ReasonAmbiguousSibling, ReasonBadOption} {
		if r == "" || strings.ContainsAny(r, " _") {
			t.Errorf("reason %q is not a token", r)
		}
	}
}

func TestPanicErrorMatchesErrInternal(t *testing.T) {
	e := &PanicError{Value: "boom"}
	if !errors.Is(e, ErrInternal) {
		t.Error("PanicError is not ErrInternal")
	}
	if !strings.Contains(e.Error(), "boom") {
		t.Errorf("text %q", e.Error())
	}
}
