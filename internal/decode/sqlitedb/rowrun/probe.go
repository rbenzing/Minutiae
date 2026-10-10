package rowrun

import (
	"context"
	"errors"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Req is what a parser needs from one table.
type Req struct {
	Table    string
	Need     []string // columns the mapping cannot work without
	Alias    string   // a column that must be the rowid alias ("" = none): joins by rowid are only right then
	Optional bool     // the table may be absent; if it exists its Need columns must exist
	With     []string // tables that must exist whenever this one does
}

const deferred = "schema checked at parse time: a -wal or -journal companion is present"

// Probe decides applicability from the main database file only (the probe read
// limit makes attaching a WAL unsafe). A file that is not SQLite is
// not-applicable, a high-entropy one encrypted, a missing required table
// not-applicable and a missing column, rowid alias or companion table
// unsupported-schema, each naming tables and columns only. When a wal or journal
// artifact is in the bundle the main file's schema proves nothing (a freshly
// created database keeps its schema in the WAL), so every schema finding is
// deferred to Parse, which fails loudly instead of skipping (D4).
func Probe(ctx context.Context, in *parse.Input, reqs []Req) (parse.Applicability, error) {
	_, wal := in.Artifacts["wal"]
	_, jr := in.Artifacts["journal"]
	companion := wal || jr
	db, err := sqlitedb.Open(ctx, sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}, in.Budget)
	switch {
	case errors.Is(err, sqlitedb.ErrEncrypted):
		return parse.Applicability{Status: parse.Encrypted, Reason: "the file looks encrypted"}, nil
	case errors.Is(err, sqlitedb.ErrNotSQLite):
		if companion {
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: "the main file is not a database but a -wal or -journal companion exists"}, nil
		}
		return parse.Applicability{Status: parse.NotApplicable, Reason: "not an SQLite database"}, nil
	case err != nil:
		return parse.Applicability{}, err
	}
	defer db.Release()
	deferAll := func() (parse.Applicability, error) {
		return parse.Applicability{Status: parse.Applicable, Reason: deferred}, nil
	}
	for _, rq := range reqs {
		t, err := db.Table(ctx, rq.Table, rq.Need, nil)
		var us *sqlitedb.UnsupportedSchemaError
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable):
			if rq.Optional {
				continue
			}
			if companion {
				return deferAll()
			}
			return parse.Applicability{Status: parse.NotApplicable, Reason: "table " + rq.Table + " is absent"}, nil
		case errors.As(err, &us):
			if companion {
				return deferAll()
			}
			reason := "table " + us.Table + " could not be read"
			if len(us.Missing) > 0 {
				reason = "table " + us.Table + " lacks column(s) " + strings.Join(us.Missing, ", ")
			}
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reason}, nil
		case err != nil:
			return parse.Applicability{}, err
		}
		if rq.Alias != "" {
			i := t.Col(rq.Alias)
			if i < 0 || !t.Cols()[i].RowidAlias {
				if companion {
					return deferAll()
				}
				return parse.Applicability{Status: parse.UnsupportedSchema, Reason: "column " + rq.Alias + " of table " + t.Name() + " is not the rowid alias"}, nil
			}
		}
		for _, w := range rq.With {
			if _, err := db.Table(ctx, w, nil, nil); errors.Is(err, sqlitedb.ErrNoSuchTable) {
				if companion {
					return deferAll()
				}
				return parse.Applicability{Status: parse.UnsupportedSchema, Reason: "table " + t.Name() + " exists but table " + w + " is absent"}, nil
			} else if err != nil {
				return parse.Applicability{}, err
			}
		}
	}
	return parse.Applicability{Status: parse.Applicable, Reason: "required tables and columns present"}, nil
}
