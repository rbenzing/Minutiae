package androidwhatsapp

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// Cleared chats awaiting purge (spec 6.3 as amended by G1, 4I-R4 and 4I-R9). The rule is implemented from the
// documented columns of deleted_chat_job (the chat row and the bound a job records), never from the SQL of the app's
// views: that text is recorded as a note and never evaluated. The jobs are read once per parse, before any mapping,
// and the mapping consults the immutable set.

const (
	// maxJobs is the most deleted_chat_job rows one parse reads; more is a counted warning and an incomplete job.
	maxJobs = 100_000
	// maxJobWarnings is the most per-job warnings written, the rest are counted in one more.
	maxJobWarnings = 1000
	// maxPurgeJobs is the most clearing job ids listed in raw.pending_purge_jobs.
	maxPurgeJobs = 1000
)

// The reasons a row is undecided (4I-R4 e) and the extra reasons the per-job warnings use (4I-R9).
const (
	reasonJobLists      = "categories-or-singular-list-not-interpreted"
	reasonJobBound      = "job-bound-not-integer"
	reasonRowKey        = "row-key-not-integer"
	reasonJobSet        = "job-set-incomplete"
	reasonRowChat       = "row-chat-undecided"
	reasonJobChatNotInt = "chat-row-id-not-integer"
)

// Where a cleared state or an undecided one is recorded.
const (
	sourceDeletedChatJob = "deleted_chat_job"
	reasonJobsWarn       = "whatsapp-deleted-chat-job-incomplete"
	reasonPurgeWarn      = "whatsapp-pending-purge-undecided"
)

// ErrPurgeJobsIncomplete is returned by Parse after every record was emitted, when deleted_chat_job could not be read
// whole: the job is then incomplete with its records kept (4I-R4 f).
var ErrPurgeJobsIncomplete = errors.New("android.whatsapp.msgstore: deleted_chat_job could not be read whole")

// jobBound is one usable deleted_chat_job row: the bounds its columns record. Msg and Starred are nil for NULL (a NULL
// bound covers nothing). BoundOK is false when a bound cell is neither NULL nor an INTEGER. Lists says the job holds a
// non-empty category or singular list, whose encoding is not documented.
type jobBound struct {
	ID           int64
	Msg, Starred *int64
	BoundOK      bool
	Lists        bool
}

// jobWarn is a job that can make rows undecided, with why.
type jobWarn struct {
	ID     int64
	Reason string
}

// jobSet is the deleted_chat_job table as one parse reads it. It is read-only after loadJobs.
type jobSet struct {
	ByChat             map[int64][]jobBound // jobs of a chat that exists, in rowid order
	Total              int64                // the census row count, -1 when it is unknown
	Read               int64                // jobs read (at most maxJobs)
	Unusable, Dangling int64
	Complete           bool
	Why                string    // why the set is incomplete ("" when complete)
	Undecided          []jobWarn // jobs that can make rows undecided, at most maxJobWarnings kept
	UndecidedDropped   int64     // the jobs past that cap
	minID              int64     // the smallest job id read, 0 when none
	solec              soleCache // the answer of R5a, computed on first use (one parse maps rows one at a time)
}

// soleCache holds the answer of soleLive for one parse.
type soleCache struct{ done, val bool }

// sole is the R5a question for db, asked at most once per parse (a nil set, as in the typed recovery pass, asks every time).
func (js *jobSet) sole(in *parse.Input, db dbPath) func() bool {
	if js == nil {
		return func() bool { return soleLive(in, db) }
	}
	return func() bool {
		if !js.solec.done {
			js.solec.val, js.solec.done = soleLive(in, db), true
		}
		return js.solec.val
	}
}

// anyJob says whether any usable or unusable job was read.
func (js *jobSet) anyJob() bool { return js != nil && js.Read > 0 }

// active says whether a row of any chat must be evaluated at all.
func (js *jobSet) active() bool {
	return js != nil && (len(js.ByChat) > 0 || !js.Complete || js.Unusable > 0)
}

// incomplete says whether a job that was not read, or not usable, could have covered a row.
func (js *jobSet) incomplete() bool { return !js.Complete || js.Unusable > 0 }

type purgeVerdict int

const (
	purgeNone purgeVerdict = iota
	purgeCleared
	purgeUndecided
)

// purgeResult is the outcome for one row: Cleared lists every job that covers it, Undecided names the smallest job
// involved (0 for none) and why.
type purgeResult struct {
	Verdict purgeVerdict
	Jobs    []int64
	Job     int64
	Reason  string
}

// rowKey is what a row offers the evaluation: its chat, whether the chat is known, whether the chat could be decided
// (ChatUndecided), its starred flag and its order key (modern sort_id, legacy messages._id).
type rowKey struct {
	Chat                     int64
	ChatKnown, ChatUndecided bool
	Starred, Order           int64
	StarredOK, OrderOK       bool
}

// covers says whether the job's bound for the row's starred flag reaches the row's order key.
func (j jobBound) covers(k rowKey) bool {
	b := j.Msg
	if k.Starred == 1 {
		b = j.Starred
	}
	return b != nil && k.Order <= *b
}

// eval is the pure decision of 4I-R4 (c) to (e); a nil set gives purgeNone.
func (js *jobSet) eval(k rowKey) purgeResult {
	if js == nil {
		return purgeResult{}
	}
	if k.ChatUndecided {
		switch {
		case js.anyJob():
			return purgeResult{Verdict: purgeUndecided, Job: js.minID, Reason: reasonRowChat}
		case js.incomplete():
			return purgeResult{Verdict: purgeUndecided, Reason: reasonJobSet}
		}
		return purgeResult{}
	}
	if !k.ChatKnown {
		return purgeResult{} // a row with no chat is not in any job's chat
	}
	jobs := js.ByChat[k.Chat]
	if k.StarredOK && k.OrderOK {
		var ids []int64
		for _, j := range jobs {
			if j.covers(k) {
				ids = append(ids, j.ID)
			}
		}
		if len(ids) > 0 {
			return purgeResult{Verdict: purgeCleared, Jobs: ids, Job: ids[0]}
		}
	}
	for _, j := range jobs {
		if j.Lists {
			return purgeResult{Verdict: purgeUndecided, Job: j.ID, Reason: reasonJobLists}
		}
	}
	for _, j := range jobs {
		if !j.BoundOK {
			return purgeResult{Verdict: purgeUndecided, Job: j.ID, Reason: reasonJobBound}
		}
	}
	if len(jobs) > 0 && (!k.StarredOK || !k.OrderOK) {
		return purgeResult{Verdict: purgeUndecided, Job: jobs[0].ID, Reason: reasonRowKey}
	}
	if js.incomplete() {
		r := purgeResult{Verdict: purgeUndecided, Reason: reasonJobSet}
		if len(jobs) > 0 {
			r.Job = jobs[0].ID
		}
		return r
	}
	return purgeResult{}
}

// dataDamage says an error is the database's damage, which makes the job set incomplete, not the job fail.
func dataDamage(err error) bool {
	var us *sqlitedb.UnsupportedSchemaError
	return errors.As(err, &us) || errors.Is(err, sqlitedb.ErrCorrupt) || errors.Is(err, sqlitedb.ErrEngineRefuses) ||
		errors.Is(err, sqlitedb.ErrLiveUnavailable) || errors.Is(err, sqlitedb.ErrKeyUndecidable) || errors.Is(err, sqlitedb.ErrWithoutRowid)
}

// loadJobs reads deleted_chat_job once, in rowid order, at most maxRows rows. A table that is absent is an empty complete
// set. A table that cannot be read whole (damage, or more than max rows) leaves the jobs read and Complete false.
// Context, budget and internal errors are returned.
func loadJobs(ctx context.Context, in *parse.Input, maxRows int) (*jobSet, error) {
	js := &jobSet{ByChat: map[int64][]jobBound{}, Complete: true, Total: -1}
	files, err := filesOf(in)
	if err != nil {
		return nil, err
	}
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		if dataDamage(err) {
			js.Complete, js.Why = false, "the database could not be opened for the jobs"
			return js, nil
		}
		return nil, err
	}
	defer db.Release()
	t, err := db.Table(ctx, tblDeletedJobs, []string{"chat_row_id"}, nil)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return js, nil
	case err != nil && dataDamage(err):
		js.Complete, js.Why = false, "the table could not be opened"
		return js, nil
	case err != nil:
		return nil, err
	}
	cChat, cMsg, cStar := t.Col("chat_row_id"), t.Col("deleted_message_row_id"), t.Col("deleted_starred_message_row_id")
	cCats, cSing := t.Col("deleted_message_categories"), t.Col("singular_message_delete_rows_id")
	var rows []jobRow
	scanErr := t.Scan(ctx, func(row sqlitedb.Row) error {
		if js.Read >= int64(maxRows) {
			js.Complete, js.Why = false, "more than the job cap"
			return sqlitedb.ErrStop
		}
		id, ok := row.Rowid()
		if !ok {
			js.Complete, js.Why = false, "a job row has no rowid"
			return nil
		}
		js.Read++
		if js.minID == 0 || id < js.minID {
			js.minID = id
		}
		jr := jobRow{ID: id}
		if v, k := rowrun.IntCell(row, cChat); k == rowrun.CellValue {
			jr.Chat, jr.ChatOK = v, true
		}
		jr.Msg, jr.HasMsg, jr.MsgOK = boundOf(row, cMsg)
		jr.Starred, jr.HasStar, jr.StarOK = boundOf(row, cStar)
		jr.Lists = nonEmpty(row, cCats) || nonEmpty(row, cSing)
		rows = append(rows, jr)
		return nil
	})
	if scanErr != nil {
		if !dataDamage(scanErr) {
			return nil, scanErr
		}
		js.Complete, js.Why = false, "a data error ended the scan"
	}
	// a job whose chat does not exist is counted and never applied: one Get per distinct chat
	exists := map[int64]bool{}
	var ct *sqlitedb.Table
	if len(rows) > 0 {
		if ct, err = db.Table(ctx, "chat", nil, nil); err != nil && !dataDamage(err) && !errors.Is(err, sqlitedb.ErrNoSuchTable) {
			return nil, err
		}
	}
	for _, jr := range rows {
		if !jr.ChatOK {
			js.Unusable++
			js.warn(jr.ID, reasonJobChatNotInt)
			continue
		}
		known, seen := exists[jr.Chat]
		if !seen {
			known = true // a chat table that cannot answer proves no absence
			if ct != nil {
				r, found, gerr := ct.Get(ctx, jr.Chat)
				switch {
				case gerr == nil:
					known = found
					if found {
						r.Release()
					}
				case !dataDamage(gerr):
					return nil, gerr
				}
			}
			exists[jr.Chat] = known
		}
		if !known {
			js.Dangling++
			continue
		}
		b := jobBound{ID: jr.ID, BoundOK: jr.MsgOK && jr.StarOK, Lists: jr.Lists}
		if jr.MsgOK && jr.HasMsg {
			v := jr.Msg
			b.Msg = &v
		}
		if jr.StarOK && jr.HasStar {
			v := jr.Starred
			b.Starred = &v
		}
		js.ByChat[jr.Chat] = append(js.ByChat[jr.Chat], b)
		switch {
		case b.Lists:
			js.warn(b.ID, reasonJobLists)
		case !b.BoundOK:
			js.warn(b.ID, reasonJobBound)
		}
	}
	return js, nil
}

// jobRow is a deleted_chat_job row as scanned.
type jobRow struct {
	ID              int64
	Chat            int64
	ChatOK          bool
	Msg, Starred    int64
	HasMsg, HasStar bool
	MsgOK, StarOK   bool // the bound cell is NULL or an INTEGER
	Lists           bool
}

// boundOf reads a bound cell: its value, whether one is stored, and whether the cell is usable (NULL, absent or an
// INTEGER). Any other content is not usable and covers nothing.
func boundOf(row parse.Row, col int) (v int64, has, ok bool) {
	v, k := rowrun.IntCell(row, col)
	switch k {
	case rowrun.CellMissing, rowrun.CellNull:
		return 0, false, true
	case rowrun.CellValue:
		return v, true, true
	}
	return 0, false, false
}

// nonEmpty says whether a cell holds a non-NULL, non-empty value of any class. A cell that cannot be read counts as
// holding one: it cannot be shown to be empty.
func nonEmpty(row parse.Row, col int) bool {
	b, k := rowrun.TextCell(row, col)
	switch k {
	case rowrun.CellMissing, rowrun.CellNull:
		return false
	case rowrun.CellValue:
		return len(b) > 0
	case rowrun.CellOther:
		if bl, ok := row.Blob(col); ok {
			return len(bl) > 0
		}
	}
	return true
}

// warn lists a job that can make rows undecided, at most maxJobWarnings.
func (js *jobSet) warn(id int64, reason string) {
	if len(js.Undecided) >= maxJobWarnings {
		js.UndecidedDropped++
		return
	}
	js.Undecided = append(js.Undecided, jobWarn{ID: id, Reason: reason})
}

// emitWarnings writes the per-job warnings of 4I-R9 (before rowrun.Run) and the cap or damage warning of 4I-R4 f.
func (js *jobSet) emitWarnings(ctx context.Context, out parse.Emitter) error {
	sort.SliceStable(js.Undecided, func(i, j int) bool { return js.Undecided[i].ID < js.Undecided[j].ID })
	for _, w := range js.Undecided {
		loc := fmt.Sprintf("sqlite:table=%s;rowid=%d", tblDeletedJobs, w.ID)
		if err := out.Warn(ctx, loc, fmt.Sprintf("%s: %s %d: %s", reasonPurgeWarn, tblDeletedJobs, w.ID, w.Reason)); err != nil {
			return err
		}
	}
	if js.UndecidedDropped > 0 {
		if err := out.Warn(ctx, "sqlite:table="+tblDeletedJobs, fmt.Sprintf("%s: %d further jobs are not listed", reasonPurgeWarn, js.UndecidedDropped)); err != nil {
			return err
		}
	}
	if js.Complete {
		return nil
	}
	n := "unknown"
	if js.Total >= 0 {
		n = fmt.Sprint(js.Total)
	}
	return out.Warn(ctx, "sqlite:table="+tblDeletedJobs, fmt.Sprintf("%s: %s rows, %d read (%s)", reasonJobsWarn, n, js.Read, js.Why))
}

// judge adds what the census knows to the job set: the row count, and whether the table is whole. A table the census
// could not read, a schema entry the damage hides, or a count that differs from the rows read leaves the set incomplete.
func (d detection) judge(js *jobSet) {
	c, ok := d.count(tblDeletedJobs)
	if !ok {
		return
	}
	switch {
	case c.Present && c.Unreadable == "":
		js.Total = c.Rows
		if c.Rows != js.Read && js.Complete {
			js.Complete, js.Why = false, "the census counted a different number of rows"
		}
	case c.Present:
		js.Complete, js.Why = false, "table "+tblDeletedJobs+" is unreadable: "+c.Unreadable
	case c.Unreadable != "":
		js.Complete, js.Why = false, "table "+tblDeletedJobs+": "+c.Unreadable
	}
}

// emit writes the notes and the warnings of the job set (before rowrun.Run).
func (js *jobSet) emit(ctx context.Context, out parse.Emitter) error {
	if js.Unusable > 0 {
		out.Note("deleted_chat_jobs_unusable", fmt.Sprint(js.Unusable))
	}
	if js.Dangling > 0 {
		out.Note("deleted_chat_jobs_dangling", fmt.Sprint(js.Dangling))
	}
	return js.emitWarnings(ctx, out)
}

// starredOf reads the starred flag of a row: NULL or absent is 0; only 0 and 1 are usable.
func starredOf(row parse.Row, col int) (int64, bool) {
	v, k := rowrun.IntCell(row, col)
	switch k {
	case rowrun.CellMissing, rowrun.CellNull:
		return 0, true
	case rowrun.CellValue:
		return v, v == 0 || v == 1
	}
	return 0, false
}

// modernKeyOf is the key of a modern message row: its chat, its starred flag and its sort_id (4I-R4 b).
func modernKeyOf(m parse.MapContext, row parse.Row) rowKey {
	c := rowrun.Resolve(m, tblMessage, "chat_row_id", "starred", "sort_id")
	var k rowKey
	if v, kd := rowrun.IntCell(row, c.At("chat_row_id")); kd == rowrun.CellValue {
		k.Chat, k.ChatKnown = v, true
	}
	k.Starred, k.StarredOK = starredOf(row, c.At("starred"))
	if v, kd := rowrun.IntCell(row, c.At("sort_id")); kd == rowrun.CellValue {
		k.Order, k.OrderOK = v, true
	}
	return k
}

// legacyKeyOf is the key of a legacy messages row: the chat is reached through jid.raw_string = key_remote_jid and
// chat.jid_row_id = jid._id (research 3.3), the order key is messages._id. More than one jid or chat row, or a join
// that cannot be decided, leaves the chat undecided.
func legacyKeyOf(m parse.MapContext, row parse.Row, c rowrun.Cols, id int64) (rowKey, error) {
	k := rowKey{Order: id, OrderOK: true}
	k.Starred, k.StarredOK = starredOf(row, c.At("starred"))
	kb, kd := rowrun.TextCell(row, c.At("key_remote_jid"))
	switch kd {
	case rowrun.CellMissing, rowrun.CellNull:
		return k, nil
	case rowrun.CellValue:
		if len(kb) == 0 {
			return k, nil
		}
	default:
		k.ChatUndecided = true
		return k, nil
	}
	jrows, st, err := matchText(m, "jid", "raw_string", kb)
	if err != nil {
		return k, err
	}
	switch {
	case st == jUndecided, len(jrows) > 1:
		k.ChatUndecided = true
		return k, nil
	case st == jNoTable, len(jrows) == 0:
		return k, nil
	}
	jid, ok := jrows[0].Rowid()
	if !ok {
		k.ChatUndecided = true
		return k, nil
	}
	crows, st, err := matchInt(m, "chat", "jid_row_id", jid)
	if err != nil {
		return k, err
	}
	switch {
	case st == jUndecided, len(crows) > 1:
		k.ChatUndecided = true
		return k, nil
	case st == jNoTable, len(crows) == 0:
		return k, nil
	}
	chat, ok := crows[0].Rowid()
	if !ok {
		k.ChatUndecided = true
		return k, nil
	}
	k.Chat, k.ChatKnown = chat, true
	return k, nil
}

// applyPurge writes the outcome of the job set on the record (4I-R4 g). A cleared row is Deleted by the app's own
// structure: no Confidence, no Recovery, no deleted time (the job stores none). A row that is already a revoke keeps
// its own source and only lists the clearing jobs.
func applyPurge(msg *message.Message, rec *records.Record, f *msgFacts) {
	raw := f.Raw
	switch f.Purge.Verdict {
	case purgeCleared:
		jobs := f.Purge.Jobs
		if len(jobs) > maxPurgeJobs {
			raw["pending_purge_jobs_total"], raw["pending_purge_jobs_truncated"] = int64(len(jobs)), true
			jobs = jobs[:maxPurgeJobs]
		}
		list := make([]any, len(jobs))
		for i, j := range jobs {
			list[i] = j
		}
		raw["pending_purge_jobs"] = list
		if f.PurgeVia != "" {
			raw["deleted_via"] = f.PurgeVia
		}
		if msg.Deleted == nil {
			msg.Deleted = map[string]any{"source": sourceDeletedChatJob, "job": f.Purge.Job}
			rec.Deleted = true
		}
	case purgeUndecided:
		raw["pending_purge"] = "undecided"
		if f.Purge.Job > 0 {
			raw["pending_purge_job"] = f.Purge.Job
		}
		raw["pending_purge_reason"] = f.Purge.Reason
		if f.PurgeVia != "" {
			raw["pending_purge_via"] = f.PurgeVia
		}
	}
}
