package iossms

import (
	"errors"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

const sourceBackup = "backup"

// attachmentRel is the path of an attachment relative to the Library of the device, from attachment.filename
// (4E-R19): the prefixes ~/, /var/mobile/ and /private/var/mobile/ are the only ones read. A name that cannot be
// written as a glob matching exactly itself ('*', backslash, NUL, an empty, "." or ".." element, more than 64
// elements) is not looked up: it stays unresolved with its source_ref.
func attachmentRel(filename string) (string, bool) {
	rel := ""
	for _, p := range []string{"/private/var/mobile/", "/var/mobile/", "~/"} {
		if r, ok := strings.CutPrefix(filename, p); ok {
			rel = r
			break
		}
	}
	if rel == "" || strings.ContainsAny(rel, "*\\") || strings.ContainsRune(rel, 0) {
		return "", false
	}
	elems := 0
	for rest := rel; ; {
		el, tail, more := strings.Cut(rest, "/")
		if el == "" || el == "." || el == ".." {
			return "", false
		}
		if elems++; elems > 64 {
			return "", false
		}
		if !more {
			return rel, true
		}
		rest = tail
	}
}

// backupDir is the first element of the remote path of a backup artifact: the directory of the backup
// (<udid>/<aa>/<fileID>). Empty when unknown.
func backupDir(a parse.Artifact) string {
	d, _, _ := strings.Cut(a.Source.RemotePath, "/")
	return d
}

// sameSource reports whether artifact a comes from the same device, acquisition, partition and snapshot as the
// message database db (rulings T19, T25 and T46: identity from structure, never from a path match alone; the
// 4D rule, restated because parsers cannot import each other). Device and acquisition must be present on both
// sides: an empty one is unknown and is never "the same", so it never links. Partition and snapshot are compared
// as recorded (a live file has no snapshot). 4E-R34: when either artifact is a backup file, both must be, from
// the same backup directory.
func sameSource(a, db parse.Artifact) bool {
	if a.Source.DeviceID == "" || a.Source.DeviceID != db.Source.DeviceID {
		return false
	}
	if a.Acquisition == "" || a.Acquisition != db.Acquisition || a.Source.Partition != db.Source.Partition {
		return false
	}
	x, y := a.Source.Snapshot, db.Source.Snapshot
	if (x == nil) != (y == nil) || (x != nil && *x != *y) {
		return false
	}
	if a.Source.Kind == sourceBackup || db.Source.Kind == sourceBackup {
		if a.Source.Kind != sourceBackup || db.Source.Kind != sourceBackup {
			return false
		}
		if d := backupDir(db); d == "" || d != backupDir(a) {
			return false
		}
	}
	return true
}

type lookup int

const (
	lookupNone        lookup = iota // not looked up: no case to look in, or a name that is not expressible
	lookupFound                     // exactly one artifact of the message's own source
	lookupMissing                   // the case holds no file at that logical path
	lookupOtherSource               // a path match exists, but only on another device, backup, partition or snapshot
	lookupAmbiguous                 // several matches of the message's own source
)

// resolveAttachment finds the artifact of the case for an attachment filename: ios:MediaDomain/<rel> (backups) or
// ios:HomeDomain/<rel> (extractions), whose source is the message database's. Exactly one over both is a link.
// None, a name that is not expressible or only matches from another source leave the attachment unlinked (an
// unlinked attachment means "not found for this source", not "no file"); several of the same source are counted,
// because choosing one would be a guess.
func resolveAttachment(in *parse.Input, filename string) (parse.Artifact, lookup) {
	if in == nil || in.Lookup == nil {
		return parse.Artifact{}, lookupNone
	}
	rel, ok := attachmentRel(filename)
	if !ok {
		return parse.Artifact{}, lookupNone
	}
	var found, same []parse.Artifact
	for _, dom := range []string{"MediaDomain", "HomeDomain"} {
		for _, a := range in.Lookup.Find("ios:" + dom + "/" + rel) {
			found = append(found, a)
			if sameSource(a, in.Primary) {
				same = append(same, a)
			}
		}
	}
	switch {
	case len(found) == 0:
		return parse.Artifact{}, lookupMissing
	case len(same) == 1:
		return same[0], lookupFound
	case len(same) == 0:
		return parse.Artifact{}, lookupOtherSource
	}
	return parse.Artifact{}, lookupAmbiguous
}

// attachments lists the attachments of message id (4E-R19): one per message_attachment_join row whose attachment
// row exists, in join order, at most 1000. Nothing is invented: an undecided join lists the table in
// raw.join_undecided and gives no attachments; a missing join table is raw.attachments_state "no-join-table", a
// missing attachment table "no-attachment-table", and a join row whose attachment row does not exist is counted in
// raw.attachments_no_row. The counters resolved and unresolved are stated whenever there is an attachment;
// other_source and ambiguous only when they are not zero.
func attachments(m parse.MapContext, id int64, raw, unknown map[string]any) ([]message.Attachment, error) {
	rows, undecided, err := rowrun.Match(m, "message_attachment_join", "message_id", id)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		raw["attachments_state"] = "no-join-table"
		return nil, nil
	case err != nil:
		return nil, err
	}
	ac := m.Column("message_attachment_join", "attachment_id")
	var ids []int64
	for _, r := range rows {
		v, k := rowrun.IntCell(r, ac)
		if k != rowrun.CellValue {
			undecided = true
			continue
		}
		ids = append(ids, v)
	}
	if undecided {
		markUndecided(raw, "message_attachment_join")
		return nil, nil
	}
	total := len(ids)
	if total > maxList {
		ids = ids[:maxList]
	}
	var out []message.Attachment
	var resolved, unresolved, ambiguous, otherSource, noRow int64
	cols := rowrun.Resolve(m, "attachment", "transfer_name", "mime_type", "total_bytes", "filename")
	for _, aid := range ids {
		row, found, err := m.Get("attachment", aid)
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable):
			raw["attachments_state"] = "no-attachment-table"
			return nil, nil
		case err != nil:
			return nil, err
		case !found:
			noRow++
			continue
		}
		var a message.Attachment
		a.Name, _ = attachText(row, cols, "transfer_name", "attachments.name", raw, unknown)
		a.Mime, _ = attachText(row, cols, "mime_type", "attachments.mime", raw, unknown)
		if n, k := rowrun.IntCell(row, cols.At("total_bytes")); k == rowrun.CellValue {
			if n >= 0 {
				a.Size = &n
			}
		} else {
			rowrun.Flag(unknown, row, "attachments.size", cols.At("total_bytes"), k)
		}
		ref, _ := attachText(row, cols, "filename", "attachments.source_ref", raw, unknown)
		if ref == "" {
			unresolved++
			out = append(out, a)
			continue
		}
		a.SourceRef = ref
		art, st := resolveAttachment(m.Input(), ref)
		switch st {
		case lookupFound:
			a.ArtifactID, a.ArtifactSHA256 = art.ID, art.SHA256
			resolved++
		case lookupAmbiguous:
			ambiguous++
		case lookupOtherSource:
			otherSource++
		default:
			unresolved++
		}
		out = append(out, a)
	}
	if len(out) > 0 {
		raw["attachments_resolved"], raw["attachments_unresolved"] = resolved, unresolved
	}
	if ambiguous > 0 {
		raw["attachments_ambiguous"] = ambiguous
	}
	if otherSource > 0 {
		raw["attachments_other_source"] = otherSource
	}
	if noRow > 0 {
		raw["attachments_no_row"] = noRow
	}
	if total > maxList {
		raw["attachments_total"], raw["attachments_truncated"] = int64(total), true
	}
	return out, nil
}

// attachText is textCell for a column of the attachment row: another storage class is named in unknown_columns
// under the field's name.
func attachText(row parse.Row, c rowrun.Cols, col, field string, raw, unknown map[string]any) (string, bool) {
	b, k := rowrun.TextCell(row, c.At(col))
	if k != rowrun.CellValue {
		rowrun.Flag(unknown, row, field, c.At(col), k)
		return "", false
	}
	return cleanField(raw, field, b), true
}
