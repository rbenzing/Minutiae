package ioswhatsapp

import (
	"context"
	"errors"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
)

// The Core Data model is read, never assumed (4I2-R3): the entity number of a table's rows is the Z_ENT that
// Z_PRIMARYKEY gives its entity name, and a store whose Z_PRIMARYKEY does not name the entities the parser needs is
// refused with a stated reason. The entity names and this rule are from memory (docs/limitations.md).

// entity is one row of Z_PRIMARYKEY: the entity's number and its super-entity's (0 when the column is absent or not an
// integer; the parser reads no number to map a row, so Super is context only).
type entity struct{ Ent, Super int64 }

// model holds the entities that were asked for, by name.
type model struct{ ByName map[string]entity }

// modelError says the Core Data model cannot be used; Reason is the whole message.
type modelError struct{ Reason string }

func (e *modelError) Error() string { return e.Reason }

// maxModelRows is the most Z_PRIMARYKEY rows one read scans: a Core Data model names a few dozen entities.
const maxModelRows = 4096

const (
	zPrimaryKey = "Z_PRIMARYKEY"
	reasonWhole = "Z_PRIMARYKEY could not be read whole"
)

// readModel reads Z_PRIMARYKEY of the database in f and returns the entities called names. Z_NAME compares byte-exact.
// A table that is absent, over the cap, unreadable, that does not name a wanted entity exactly once, or whose Z_ENT for
// it is not an INTEGER is a *modelError; budget, context and I/O errors are returned unchanged (never a model error).
func readModel(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget, names ...string) (model, error) {
	db, err := sqlitedb.Open(ctx, f, b)
	if err != nil {
		return model{}, err
	}
	defer db.Release()
	return readModelDB(ctx, db, names...)
}

// readModelDB is readModel on a database the caller opened (and releases).
func readModelDB(ctx context.Context, db *sqlitedb.DB, names ...string) (model, error) {
	t, err := db.Table(ctx, zPrimaryKey, []string{"Z_ENT", "Z_NAME"}, nil)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return model{}, &modelError{"no Z_PRIMARYKEY table: the Core Data model cannot be read"}
	case err != nil && dataDamage(err):
		return model{}, &modelError{reasonWhole}
	case err != nil:
		return model{}, err
	}
	cEnt, cName, cSuper := t.Col("Z_ENT"), t.Col("Z_NAME"), t.Col("Z_SUPER")
	type found struct {
		n          int
		ent        int64
		entOK      bool
		entUnknown bool
		super      int64
	}
	seen := make(map[string]*found, len(names))
	for _, n := range names {
		seen[n] = &found{}
	}
	var rows int
	var unreadName, overCap bool
	scanErr := t.Scan(ctx, func(row sqlitedb.Row) error {
		if rows++; rows > maxModelRows {
			overCap = true
			return sqlitedb.ErrStop
		}
		name, k := rowrun.TextCell(row, cName)
		if k == rowrun.CellUnknown {
			unreadName = true // the row could have been a wanted entity: no count of the others is decided
			return nil
		}
		if k != rowrun.CellValue {
			return nil // NULL or not text: it names no entity
		}
		fd, ok := seen[string(name)]
		if !ok {
			return nil
		}
		if fd.n++; fd.n == 1 {
			v, kind := rowrun.IntCell(row, cEnt)
			fd.ent, fd.entOK, fd.entUnknown = v, kind == rowrun.CellValue, kind == rowrun.CellUnknown
			if v, kind := rowrun.IntCell(row, cSuper); kind == rowrun.CellValue {
				fd.super = v
			}
		}
		return nil
	})
	if scanErr != nil && !errors.Is(scanErr, sqlitedb.ErrStop) {
		if dataDamage(scanErr) {
			return model{}, &modelError{reasonWhole}
		}
		return model{}, scanErr
	}
	if overCap {
		return model{}, &modelError{"Z_PRIMARYKEY has more than 4096 rows"}
	}
	// A page or cell the reader skipped is a loss, not an absence: rows of the model may be missing (unknown is never absent).
	ix, err := t.Index(ctx, "Z_NAME", maxModelRows+1)
	var uc *sqlitedb.UnsupportedCollationError
	switch {
	// the row cap was decided by the scan: an index with more entries than rows (a numeric-looking text is keyed twice) is no row count
	case errors.Is(err, sqlitedb.ErrIndexLimit) || errors.As(err, &uc) || err != nil && dataDamage(err):
		return model{}, &modelError{reasonWhole}
	case err != nil:
		return model{}, err
	}
	lossy := ix.Lossy()
	ix.Release()
	if lossy || unreadName {
		return model{}, &modelError{reasonWhole}
	}
	out := model{ByName: make(map[string]entity, len(names))}
	for _, n := range names {
		fd := seen[n]
		switch {
		case fd.n == 0:
			return model{}, &modelError{"Z_PRIMARYKEY does not name the entity " + n}
		case fd.n > 1:
			return model{}, &modelError{"Z_PRIMARYKEY names the entity " + n + " more than once"}
		case fd.entUnknown:
			return model{}, &modelError{reasonWhole}
		case !fd.entOK:
			return model{}, &modelError{"Z_PRIMARYKEY entity " + n + " has no integer Z_ENT"}
		}
		out.ByName[n] = entity{Ent: fd.ent, Super: fd.super}
	}
	return out, nil
}

// dataDamage says an error is the database's damage (a model that cannot be read), not a budget, context or I/O fact.
func dataDamage(err error) bool {
	var us *sqlitedb.UnsupportedSchemaError
	return errors.As(err, &us) || errors.Is(err, sqlitedb.ErrCorrupt) || errors.Is(err, sqlitedb.ErrEngineRefuses) ||
		errors.Is(err, sqlitedb.ErrLiveUnavailable) || errors.Is(err, sqlitedb.ErrKeyUndecidable) || errors.Is(err, sqlitedb.ErrWithoutRowid)
}
