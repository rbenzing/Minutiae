package androidwhatsapp

import (
	"strings"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// Attachments (spec 6.7, D7, R5, R5a, R6). An attachment is made only for a message of a media label with a media row
// (modern and transition) or with non-empty media columns (legacy). Its file is linked only when exactly one file of the
// case, of the database's own source, app and Android user, is at the path the row names: a database never links a file
// of the other app or of another user, an ambiguous pair is counted and never chosen, and the stored hash is kept raw and
// never compared (R6).

// mediaFolders are the folders of each app: the legacy form is <root>/<legacy>/<rest>, the Android 11 form is
// <root>/Android/media/<package>/<legacy>/<rest> (research 2.1; the Business folder name is from memory).
func mediaFolders(app string) (pkg, folder string, ok bool) {
	switch app {
	case appConsumer:
		return pkgConsumer, "WhatsApp", true
	case appBusiness:
		return pkgBusiness, "WhatsApp Business", true
	}
	return "", "", false
}

// mediaRoot is the start of a media path: the root and the Android user it names. Named false: /sdcard,
// /storage/self/primary and any other prefix name no user (they are aliases of whichever user was current).
type mediaRoot struct {
	Root  string
	User  int
	Named bool
}

// Forms of a media path.
const (
	formLegacy    = "legacy"
	formAndroid11 = "android11"
)

// cleanPathElements splits an absolute path into its elements, refusing what a logical glob cannot express: an empty,
// "." or ".." element, a '*', a backslash or a NUL.
func cleanPathElements(p string) ([]string, bool) {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "*\\") || strings.ContainsRune(p, 0) {
		return nil, false
	}
	el := strings.Split(p[1:], "/")
	if len(el) > 64 {
		return nil, false
	}
	for _, e := range el {
		if e == "" || e == "." || e == ".." {
			return nil, false
		}
	}
	return el, true
}

// rootOf says what the leading elements of a media path name: /storage/emulated/<n>, /data/media/<n> and /media/<n> name
// user n; anything else names none.
func rootOf(el []string) mediaRoot {
	root := "/" + strings.Join(el, "/")
	switch {
	case len(el) == 3 && el[0] == "storage" && el[1] == "emulated", len(el) == 3 && el[0] == "data" && el[1] == "media":
		if n, ok := userOf(el[2]); ok {
			return mediaRoot{Root: root, User: n, Named: true}
		}
	case len(el) == 2 && el[0] == "media":
		if n, ok := userOf(el[1]); ok {
			return mediaRoot{Root: root, User: n, Named: true}
		}
	}
	return mediaRoot{Root: root}
}

// splitMediaPath splits the logical path of a file into the media root, the part after the app's folder and the form of
// the folder: <root>/<folder>/<rest> (legacy) or <root>/Android/media/<package>/<folder>/<rest> (android11), for the
// app's own package and folder only. A path of the other app, with an element that cannot be expressed or with nothing
// after the folder is not ok.
func splitMediaPath(logical, app string) (root mediaRoot, rest, form string, ok bool) {
	pkg, folder, known := mediaFolders(app)
	if !known {
		return mediaRoot{}, "", "", false
	}
	el, clean := cleanPathElements(strings.TrimPrefix(logical, "android:"))
	if !clean {
		return mediaRoot{}, "", "", false
	}
	for i := 0; i < len(el); i++ {
		switch {
		case el[i] == "Android" && i+4 < len(el) && el[i+1] == "media" && el[i+2] == pkg && el[i+3] == folder:
			return rootOf(el[:i]), strings.Join(el[i+4:], "/"), formAndroid11, true
		case el[i] == folder && i+1 < len(el):
			return rootOf(el[:i]), strings.Join(el[i+1:], "/"), formLegacy, true
		}
	}
	return mediaRoot{}, "", "", false
}

// isKnownRoot says whether an absolute path starts at a media root the spec names: the three user roots and the two
// aliases /sdcard and /storage/self/primary.
func isKnownRoot(p string) bool {
	el, ok := cleanPathElements(p)
	if !ok {
		return false
	}
	switch {
	case el[0] == "sdcard", len(el) >= 3 && el[0] == "storage" && el[1] == "self" && el[2] == "primary":
		return true
	case len(el) >= 3 && (el[0] == "storage" && el[1] == "emulated" || el[0] == "data" && el[1] == "media"):
		return rootOf(el[:3]).Named
	case len(el) >= 2 && el[0] == "media":
		return rootOf(el[:2]).Named
	}
	return false
}

// sameSource reports whether artifact a comes from the same device, acquisition, partition and snapshot as the
// message's database (restated from android.mms: identity from structure, never from a path match alone). Device and
// acquisition must be present on both sides: an empty one is unknown and is never the same.
func sameSource(a, db parse.Artifact) bool {
	if a.Source.DeviceID == "" || a.Source.DeviceID != db.Source.DeviceID {
		return false
	}
	if a.Acquisition == "" || a.Acquisition != db.Acquisition || a.Source.Partition != db.Source.Partition {
		return false
	}
	x, y := a.Source.Snapshot, db.Source.Snapshot
	if (x == nil) != (y == nil) {
		return false
	}
	return x == nil || *x == *y
}

// expressible reports whether a device path can be written as an absolute glob that matches exactly itself.
func expressible(p string) bool {
	_, ok := cleanPathElements(p)
	return ok
}

type pairing int

const (
	pairSameUser  pairing = iota // the root names the database's own user
	pairOtherUser                // the root names another user
	pairUserless                 // the root names no user, and the database is the only live one of the app (R5a)
	pairUndecided                // the root names no user and the pairing cannot be shown, or the database user is unknown
)

// soleLive says whether the case holds exactly one live database of the app of db on the device: the artifacts at a
// LIVE path of the app's package, from the device of the primary (non-empty), not recovered, whatever their user or
// acquisition (a snapshot copy counts: the rule is conservative), are exactly the primary (4I-R12).
func soleLive(in *parse.Input, db dbPath) bool {
	if in == nil || in.Lookup == nil || db.Package == "" || in.Primary.Source.DeviceID == "" {
		return false
	}
	n, self := 0, false
	for _, a := range in.Lookup.Find("android:**/" + db.Package + "/databases/msgstore.db") {
		d := classifyDB(a.Logical)
		if d.Place != placeLive || d.Package != db.Package || a.Source.DeviceID != in.Primary.Source.DeviceID || a.Recovery != nil {
			continue
		}
		n++
		self = self || a.ID == in.Primary.ID
	}
	return n == 1 && self
}

// pair is the pairing of a media root with the database (R5, R5a). sole answers R5a once per parse; nil asks the case.
func pair(in *parse.Input, db dbPath, root mediaRoot, sole func() bool) pairing {
	if sole == nil {
		sole = func() bool { return soleLive(in, db) }
	}
	switch {
	case root.Named && db.UserKnown && root.User == db.User:
		return pairSameUser
	case root.Named && db.UserKnown:
		return pairOtherUser
	case root.Named:
		return pairUndecided
	case sole():
		return pairUserless
	}
	return pairUndecided
}

type lookup int

const (
	lookupNone          lookup = iota // no lookup was possible: no path, or one that cannot be expressed
	lookupFound                       // exactly one file of the database's own source, app and user
	lookupMissing                     // the case holds no file at that path
	lookupOtherSource                 // matches exist, but only from another device, acquisition, partition or snapshot
	lookupAmbiguous                   // several matches of the database's own source and user
	lookupOtherUser                   // matches exist, but only of another Android user
	lookupUserUndecided               // matches exist, but the user pairing cannot be decided
)

// resolveMedia finds the file a row names. A relative Media/... path is looked up under the app's own folder in both
// forms; an absolute path only when it is at a media root of the app. Exactly one candidate of the database's own
// source, whose root pairs with the database, is a link. The pairing returned is the one of the link, else of the first
// candidate considered.
func resolveMedia(in *parse.Input, db dbPath, filePath string, sole func() bool) (parse.Artifact, lookup, pairing) {
	if in == nil || in.Lookup == nil || filePath == "" {
		return parse.Artifact{}, lookupNone, pairUndecided
	}
	_, folder, known := mediaFolders(db.App)
	if !known {
		return parse.Artifact{}, lookupNone, pairUndecided
	}
	var found []parse.Artifact
	switch {
	case strings.HasPrefix(filePath, "/"):
		if !isKnownRoot(filePath) {
			return parse.Artifact{}, lookupNone, pairUndecided
		}
		if _, _, _, ok := splitMediaPath(filePath, db.App); !ok {
			return parse.Artifact{}, lookupNone, pairUndecided
		}
		for _, a := range in.Lookup.Find("android:" + filePath) {
			if a.Logical == "android:"+filePath { // the exact path, whatever the glob matched
				found = append(found, a)
			}
		}
	case strings.HasPrefix(filePath, "Media/") && expressible("/"+filePath):
		for _, a := range in.Lookup.Find("android:**/" + folder + "/" + filePath) {
			if _, rest, _, ok := splitMediaPath(a.Logical, db.App); ok && rest == filePath {
				found = append(found, a)
			}
		}
	default:
		return parse.Artifact{}, lookupNone, pairUndecided
	}
	if len(found) == 0 {
		return parse.Artifact{}, lookupMissing, pairUndecided
	}
	var same []parse.Artifact
	for _, a := range found {
		if sameSource(a, in.Primary) {
			same = append(same, a)
		}
	}
	if len(same) == 0 {
		return parse.Artifact{}, lookupOtherSource, pairUndecided
	}
	var link []parse.Artifact
	var otherUser, undecided int
	first := pairUndecided
	for i, a := range same {
		root, _, _, _ := splitMediaPath(a.Logical, db.App)
		p := pair(in, db, root, sole)
		if i == 0 {
			first = p
		}
		switch p {
		case pairSameUser, pairUserless:
			link = append(link, a)
		case pairOtherUser:
			otherUser++
		default:
			undecided++
		}
	}
	switch {
	case len(link) == 1:
		root, _, _, _ := splitMediaPath(link[0].Logical, db.App)
		return link[0], lookupFound, pair(in, db, root, sole)
	case len(link) > 1:
		return parse.Artifact{}, lookupAmbiguous, first
	case otherUser > 0:
		return parse.Artifact{}, lookupOtherUser, pairOtherUser
	}
	return parse.Artifact{}, lookupUserUndecided, pairUndecided
}

// mediaFacts is the media of a message as a reader found it: what an attachment is made of, and the raw values that
// state it. The lookups are made by the builder, only when it makes the attachment.
type mediaFacts struct {
	Name, Mime string
	Size       *int64 // file_size, only when above 0 (0 is not recorded: it stays in Raw)
	SourceRef  string // file_path; "" for a legacy row (no local path)
	Raw        map[string]any
}

// mediaLabels are the labels of the message type that hold a file (spec 6.7): a contact card, a location, a group
// invite, a system row and a revoke hold none.
func mediaLabel(l string) bool {
	switch l {
	case "image", "audio", "video", "gif", "document", "sticker":
		return true
	}
	return false
}

// readMediaRow reads the message_media row of a modern or transition message (nil when the table is absent).
func readMediaRow(m parse.MapContext, f *msgFacts) error {
	const tbl = "message_media"
	if badTable(m, tbl) {
		undecidedJoin(f.Raw, tbl)
		return nil
	}
	if m.Column(tbl, "message_row_id") < 0 {
		return nil
	}
	row, st, err := getRow(m, tbl, f.Rowid)
	if err != nil {
		return err
	}
	switch st {
	case jNoRow:
		f.Raw["media_state"] = "no-message_media-row"
		return nil
	case jNoTable:
		return nil
	case jUndecided:
		undecidedJoin(f.Raw, tbl)
		return nil
	}
	r := sideReader(m, f, row, tbl, "file_path", "file_size", "mime_type", "media_name", "file_length", "message_url", "direct_path",
		"file_hash", "transferred", "media_key")
	mf := &mediaFacts{Raw: map[string]any{}}
	if b, ok := r.text("media_name"); ok && len(b) > 0 {
		mf.Name = cleanField(f.Raw, "attachments.name", b)
	}
	if b, ok := r.text("mime_type"); ok && len(b) > 0 {
		mf.Mime = cleanField(f.Raw, "attachments.mime", b)
	}
	if b, ok := r.text("file_path"); ok && len(b) > 0 {
		mf.SourceRef = cleanField(f.Raw, "attachments.source_ref", b)
	}
	if v, ok := r.int("file_size"); ok {
		mf.Raw["file_size"] = v
		if v > 0 {
			mf.Size = &v
		}
	}
	for _, n := range []string{"file_length", "transferred"} {
		if v, ok := r.int(n); ok {
			mf.Raw[n] = v
		}
	}
	for _, n := range []string{"message_url", "direct_path", "file_hash"} {
		if b, ok := r.text(n); ok && len(b) > 0 {
			mf.Raw[n] = cleanField(f.Raw, n, b)
		}
	}
	if col := r.cols.At("media_key"); col >= 0 && !r.row.IsNull(col) {
		_, isBlob := r.row.Blob(col)
		mf.Raw["media_key_present"] = isBlob
	}
	f.Media = mf
	return nil
}

// readLegacyMedia reads the media columns of a legacy messages row: no local path, so the attachment is never linked.
func readLegacyMedia(r cellReader, f *msgFacts) {
	mf := &mediaFacts{Raw: map[string]any{}}
	if b, ok := r.text("media_name"); ok && len(b) > 0 {
		mf.Name = cleanField(f.Raw, "attachments.name", b)
	}
	if b, ok := r.text("media_mime_type"); ok && len(b) > 0 {
		mf.Mime = cleanField(f.Raw, "attachments.mime", b)
	}
	if v, ok := r.int("media_size"); ok {
		mf.Raw["file_size"] = v
		if v > 0 {
			mf.Size = &v
		}
	}
	for _, n := range []struct{ col, key string }{{"media_url", "media_url"}, {"media_hash", "file_hash"}} {
		if b, ok := r.text(n.col); ok && len(b) > 0 {
			mf.Raw[n.key] = cleanField(f.Raw, n.key, b)
		}
	}
	if mf.Name != "" || mf.Mime != "" || mf.Size != nil || len(mf.Raw) > 0 {
		f.Media = mf
	}
}

// applyMedia makes the attachment of a media message and states how its file was looked up. msg.Kind is media and the
// message is not a revoke.
func applyMedia(m parse.MapContext, msg *message.Message, f *msgFacts) {
	mf := f.Media
	if mf == nil {
		return
	}
	for k, v := range mf.Raw {
		f.Raw[k] = v
	}
	if mf.Name == "" && mf.Mime == "" && mf.Size == nil && mf.SourceRef == "" {
		return // a media row that holds nothing to link or name: its raw values only
	}
	a := message.Attachment{Name: mf.Name, Mime: mf.Mime, SourceRef: mf.SourceRef, Size: mf.Size}
	var resolved, unresolved, ambiguous, otherSource, otherUser, undecided int64
	art, st, _ := resolveMedia(m.Input(), classifyDB(m.Input().Primary.Logical), mf.SourceRef, f.Sole)
	switch st {
	case lookupFound:
		a.ArtifactID, a.ArtifactSHA256 = art.ID, art.SHA256
		resolved++
	case lookupAmbiguous:
		ambiguous++
	case lookupOtherSource:
		otherSource++
	case lookupOtherUser:
		otherUser++
	case lookupUserUndecided:
		undecided++
	default:
		unresolved++
	}
	msg.Attachments = append(msg.Attachments, a)
	f.Raw["attachments_resolved"], f.Raw["attachments_unresolved"] = resolved, unresolved
	for k, v := range map[string]int64{
		"attachments_ambiguous": ambiguous, "attachments_other_source": otherSource,
		"attachments_other_user": otherUser, "attachments_user_undecided": undecided,
	} {
		if v > 0 {
			f.Raw[k] = v
		}
	}
}
