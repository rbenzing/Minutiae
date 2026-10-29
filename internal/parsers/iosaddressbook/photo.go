package iosaddressbook

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

// maxPhotoIDs caps the ids kept from the images database (ruling 4E-R25).
const maxPhotoIDs = 1_000_000

// photoIDCharge is what one kept id is charged to the budget (a map entry).
const photoIDCharge = 48

const (
	tableThumb = "ABThumbnailImage"
	tableFull  = "ABFullSizeImage"
)

// photoSet is what the images database says about who has a picture (rulings 4E-R25 and 4E-R41): the ids that
// hold a row in each table, and whether each table was decided, that is absent from the images schema or read
// completely (no reader warning, no damaged row, no unreadable id, no cap hit). A person is known to have a
// photo when either table holds a row; known not to have one only when both tables are decided and neither does.
type photoSet struct {
	thumb, full               map[int64]struct{}
	thumbDecided, fullDecided bool
	known                     bool  // the images database was read and holds at least one image table
	charged                   int64 // bytes charged to the budget for the ids
	count                     int   // ids kept over both tables (the cap counts them together)
}

// has reports what the set says about a person: present, and whether it says anything.
func (p photoSet) has(id int64) (present, decided bool) {
	_, inThumb := p.thumb[id]
	_, inFull := p.full[id]
	switch {
	case inThumb || inFull:
		return true, true
	case p.thumbDecided && p.fullDecided:
		return false, true
	}
	return false, false
}

// tableIDs is the read of one image table.
type tableIDs struct {
	ids     map[int64]struct{}
	present bool   // the table exists in the images database
	decided bool   // every row was read
	why     string // why it is undecided
	capHit  bool
}

// photoIndex reads the record ids of ABThumbnailImage and ABFullSizeImage from the images role (and its WAL)
// through sqlitedb, at most maxPhotoIDs ids, each charged to in.Budget; the caller frees set.charged. why is
// the note value: "absent" without an images database, "" when it was read and both tables are decided, else
// the reason something is undecided. A set that hit its cap, or an unreadable database, is discarded (known
// false): no person is then given a photo claim. Only a cancelled context is an error.
func photoIndex(ctx context.Context, in *parse.Input) (set photoSet, why string, err error) {
	return photoIndexLimit(ctx, in, maxPhotoIDs)
}

// photoIndexLimit is photoIndex with the cap as a parameter (Parser.photoCap is the test seam).
func photoIndexLimit(ctx context.Context, in *parse.Input, limit int) (set photoSet, why string, err error) {
	img, ok := in.Artifacts["images"]
	if !ok || img.R == nil {
		return photoSet{}, "absent", nil
	}
	files := sqlitedb.Files{DB: img.R, DBSize: img.Size}
	incomplete := img.Incomplete
	if w, ok := in.Artifacts["images_wal"]; ok && w.R != nil {
		files.WAL, files.WALSize = w.R, w.Size
		incomplete = incomplete || w.Incomplete
	}
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return photoSet{}, "", cerr
		}
		return photoSet{}, "images database unusable: " + openReason(err), nil
	}
	defer db.Release()
	discard := func(reason string) (photoSet, string, error) {
		in.Budget.Free(set.charged)
		return photoSet{}, reason, nil
	}
	var tables [2]tableIDs
	for i, name := range []string{tableThumb, tableFull} {
		t, err := readImageTable(ctx, in, db, name, &set, incomplete, limit)
		if err != nil {
			in.Budget.Free(set.charged)
			return photoSet{}, "", err
		}
		if t.capHit {
			return discard(fmt.Sprintf("images database unusable: %s hit the cap of %d ids", name, limit))
		}
		tables[i] = t
	}
	if !tables[0].present && !tables[1].present {
		return discard("images database unusable: it holds neither " + tableThumb + " nor " + tableFull)
	}
	set.thumb, set.full = tables[0].ids, tables[1].ids
	set.thumbDecided, set.fullDecided = tables[0].decided, tables[1].decided
	set.known = true
	var undecided []string
	for _, t := range tables {
		if !t.decided {
			undecided = append(undecided, t.why)
		}
	}
	return set, strings.Join(undecided, "; "), nil
}

func openReason(err error) string {
	switch {
	case errors.Is(err, sqlitedb.ErrNotSQLite):
		return "the file is not a database"
	case errors.Is(err, sqlitedb.ErrEncrypted):
		return "the database is encrypted"
	case errors.Is(err, sqlitedb.ErrCorrupt):
		return "the database structure is corrupt"
	}
	return "the database could not be read"
}

// readImageTable collects the record_id column of one image table. A table the schema lacks is decided and
// empty (and not present). Any damage leaves the table undecided but keeps the ids read: an id read from an
// intact row is a true fact.
func readImageTable(ctx context.Context, in *parse.Input, db *sqlitedb.DB, name string, set *photoSet, incomplete bool, limit int) (tableIDs, error) {
	res := tableIDs{ids: map[int64]struct{}{}}
	t, err := db.Table(ctx, name, []string{"record_id"}, nil)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		res.decided = true
		return res, nil
	case err != nil:
		if cerr := ctx.Err(); cerr != nil {
			return res, cerr
		}
		res.why = name + " is undecided: its definition is not readable"
		return res, nil
	}
	res.present = true
	col := t.Col("record_id")
	var damaged, unreadable, budget bool
	err = t.Scan(ctx, func(r sqlitedb.Row) error {
		if rowrun.DamageFlags(r) != nil {
			damaged = true
		}
		id, k := rowrun.IntCell(r, col)
		switch k {
		case rowrun.CellValue:
		case rowrun.CellNull: // a picture that belongs to nobody
			return nil
		default:
			unreadable = true
			return nil
		}
		if _, dup := res.ids[id]; dup {
			return nil
		}
		if set.count >= limit {
			res.capHit = true
			return sqlitedb.ErrStop
		}
		if err := in.Budget.Alloc(photoIDCharge); err != nil {
			budget = true
			return sqlitedb.ErrStop
		}
		set.charged += photoIDCharge
		set.count++
		res.ids[id] = struct{}{}
		return nil
	})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return res, cerr
		}
		if errors.Is(err, parse.ErrBudget) {
			budget = true
		} else {
			damaged = true
		}
	}
	if budget {
		res.capHit = true // the memory limit is a cap
	}
	if len(db.Warnings()) > 0 {
		damaged = true
	}
	switch {
	case res.capHit:
	case damaged:
		res.why = name + " is undecided: damaged pages or rows"
	case unreadable:
		res.why = name + " is undecided: a record_id could not be read"
	case incomplete:
		res.why = name + " is undecided: the images database is incomplete"
	default:
		res.decided = true
	}
	return res, nil
}

// photoEmitter adds payload.photo.present to every contact record from the person's source_id, when the images
// database was read (4E-R25). The mapper stays a pure function of the row and its context (spec 12.1): this
// decorator sits outside it.
type photoEmitter struct {
	parse.Emitter
	set   photoSet
	known bool
}

// Emit sets photo only when the set decides the person (4E-R41); otherwise the record passes unchanged.
func (e photoEmitter) Emit(ctx context.Context, r records.Record) error {
	if e.known && r.Type == contact.Type && r.Payload != nil {
		if s, ok := r.Payload["source_id"].(string); ok {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				if present, decided := e.set.has(id); decided {
					r.Payload["photo"] = map[string]any{"present": present}
				}
			}
		}
	}
	return e.Emitter.Emit(ctx, r)
}
