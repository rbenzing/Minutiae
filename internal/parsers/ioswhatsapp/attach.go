package ioswhatsapp

import (
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// Attachments (spec 6.7 iOS paragraph, D7, 4I2-R6, 4I2-P6). An attachment is made for a file-type message (image, video,
// audio, document, gif, sticker) whose media row is decided (joinMedia says mediaRow). Its file is linked only when exactly
// one file of the case, of the database's own source, is at <container>/Message/<ZMEDIALOCALPATH>: a database never links a
// file of another container, an ambiguous pair is counted and never chosen, and the stored hash is never compared (R6).

const (
	stateNoLocalPath    = "no-local-path"
	stateUnreadable     = "local-path-unreadable"
	stateNotExpressible = "lookup-path-not-expressible"
	stateRefused        = "lookup-path-refused"
	stateNoMediaRow     = "no-ZWAMEDIAITEM-row"

	maxGlobElems = 64 // the most elements of a case glob (parse.CompileGlob)
	sourceBackup = "backup"
)

// containerElems is the number of path elements a container takes in a glob: "ios:<domain>" is one, an app group
// container is five.
func containerElems(container string) int {
	return strings.Count(strings.TrimPrefix(container, "ios:"), "/") + 1
}

// mediaRel reads the stored ZMEDIALOCALPATH as a path relative to <container>/Message. rel is the cleaned text (what the
// attachment's source_ref states). state is "" when the path can be looked up, else why it cannot be: no-local-path (empty),
// lookup-path-not-expressible (cleaning changed the stored bytes, or the path holds '*', the case glob's only
// metacharacter: no glob matches exactly it) and lookup-path-refused (an absolute path, a backslash, an empty, "." or ".."
// element, or more elements than the glob allows under a container of containerElems elements). The characters [ ] ? { }
// are literal to the glob and are never refused.
func mediaRel(stored []byte, containerElems int) (rel, state string) {
	if len(stored) == 0 {
		return "", stateNoLocalPath
	}
	cl := common.CleanText(stored, maxText)
	rel = cl.Text
	if len(cl.Flags) > 0 || strings.Contains(rel, "*") {
		return rel, stateNotExpressible
	}
	if rel == "" {
		return rel, stateNoLocalPath
	}
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
		return rel, stateRefused
	}
	n := 0
	for _, e := range strings.Split(rel, "/") {
		if e == "" || e == "." || e == ".." {
			return rel, stateRefused
		}
		n++
	}
	if n+1+containerElems > maxGlobElems {
		return rel, stateRefused
	}
	return rel, ""
}

// backupDir is the first element of the remote path of a backup artifact: the directory of the backup
// (<udid>/<aa>/<fileID>). Empty when unknown.
func backupDir(a parse.Artifact) string {
	d, _, _ := strings.Cut(a.Source.RemotePath, "/")
	return d
}

// sameSource reports whether artifact a comes from the same device, acquisition, partition and snapshot as the message
// database db (restated from iossms: identity from structure, never from a path match alone). Device and acquisition must be
// present on both sides: an empty one is unknown and is never the same. When either artifact is a backup file, both must be,
// from the same backup directory (4E-R34).
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
	lookupNone        lookup = iota // no lookup was possible: no case to look in, or no container
	lookupFound                     // exactly one file of the database's own source
	lookupMissing                   // the case holds no file at that logical path
	lookupOtherSource               // matches exist, but only from another device, acquisition, partition or snapshot
	lookupAmbiguous                 // several matches of the database's own source
	lookupRefused                   // the glob cannot be compiled, so no lookup was made (see resolveMedia)
)

// resolveMedia finds the file of the case at <container>/Message/<rel> (rel is expressible, see mediaRel). One Find is made;
// only a file whose logical path is exactly that path counts (the container's own text is never a pattern), and exactly one
// of the database's own source is a link. lookupRefused is reachable with a hostile backup: an app-group id such as "x**y"
// is one path element that the primary glob accepts, but "**" inside an element is no glob (parse.CompileGlob), so the
// container itself cannot be searched and no Find is made.
func resolveMedia(in *parse.Input, container, rel string) (parse.Artifact, lookup) {
	if in == nil || in.Lookup == nil || container == "" {
		return parse.Artifact{}, lookupNone
	}
	want := container + "/Message/" + rel
	if _, err := parse.CompileGlob(want); err != nil {
		return parse.Artifact{}, lookupRefused
	}
	var found, same []parse.Artifact
	for _, a := range in.Lookup.Find(want) {
		if a.Logical != want {
			continue
		}
		found = append(found, a)
		if sameSource(a, in.Primary) {
			same = append(same, a)
		}
	}
	switch {
	case len(found) == 0:
		return parse.Artifact{}, lookupMissing
	case len(same) == 0:
		return parse.Artifact{}, lookupOtherSource
	case len(same) > 1:
		return parse.Artifact{}, lookupAmbiguous
	}
	return same[0], lookupFound
}

// attachmentOf makes the attachment of a file-type message from its decided media row and states how its file was looked up
// (raw.media_state and the attachments_* counters). A row holding no MIME type, no size and no path makes none.
func attachmentOf(m parse.MapContext, ir cellReader) (message.Attachment, bool) {
	raw := ir.raw
	var a message.Attachment
	if b, ok := ir.text("ZVCARDSTRING"); ok && len(b) > 0 {
		a.Mime = cleanField(raw, "attachments.mime", b)
	}
	if v, ok := ir.int("ZFILESIZE"); ok && v > 0 {
		a.Size = &v
	}
	stored, readable := localPath(ir)
	container := classifyPlace(m.Input().Primary.Logical).Container
	rel, state := mediaRel(stored, containerElems(container))
	if !readable {
		rel, state = "", stateUnreadable
	}
	if len(stored) > 0 {
		a.SourceRef = cleanField(raw, "attachments.source_ref", stored)
	}
	if a.Mime == "" && a.Size == nil && a.SourceRef == "" {
		return message.Attachment{}, false
	}
	// every attachment is counted once: resolved, unresolved (no path, a refused path, no file) or one of the three that
	// never pass for "no file": ambiguous, other source, and a path no lookup could express
	counter := "attachments_unresolved"
	// lookupNone (no Lookuper, or no container) cannot happen for a primary the host classified: Probe accepted its logical
	// path and the host always gives a Lookuper. It is counted unresolved, as "no file was found", and never a link.
	switch state {
	case "":
		art, st := resolveMedia(m.Input(), container, rel)
		switch st {
		case lookupFound:
			a.ArtifactID, a.ArtifactSHA256 = art.ID, art.SHA256
			counter = "attachments_resolved"
		case lookupAmbiguous:
			counter = "attachments_ambiguous"
		case lookupOtherSource:
			counter = "attachments_other_source"
		case lookupRefused:
			raw["media_state"] = stateRefused
		}
	case stateNotExpressible, stateUnreadable:
		raw["media_state"] = state
		counter = "attachments_lookup_undecided" // no lookup was possible
	default:
		raw["media_state"] = state
	}
	raw["attachments_resolved"], raw["attachments_unresolved"] = int64(0), int64(0)
	raw[counter] = int64(1)
	return a, true
}

// localPath reads ZMEDIALOCALPATH: the stored bytes (empty for NULL), and whether the cell is readable. A cell that is NULL
// or text is readable (NULL and empty text are "no local path"); a BLOB, an INTEGER, a damaged cell or a missing column is
// not, and is never read as "no path" (PF15).
func localPath(ir cellReader) ([]byte, bool) {
	col := ir.cols.At("ZMEDIALOCALPATH")
	b, k := rowrun.TextCell(ir.row, col)
	rowrun.Flag(ir.unknown, ir.row, ir.prefix+"ZMEDIALOCALPATH", col, k)
	switch k {
	case rowrun.CellValue:
		return b, true
	case rowrun.CellNull:
		return nil, true
	}
	return nil, false
}
