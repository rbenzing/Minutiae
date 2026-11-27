package androidtelegram_test

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 13 (call 8, oracles): each of the twelve variants is parsed by a test with a stated outcome. A variant whose
// oracle states records is matched by TestAndroidTelegramMatchesOracle (and its oracle must state some); the three
// refused variants fail Parse at the app path with their typed error and no record: telegram-otherversion (182) and
// telegram-oldversion (63) with ErrUnsupportedVersion (B4, 4I4-P8), telegram-nomessages with ErrNoMessageTable.
// Every DDL snapshot of the telegram-android directory is used by a fixture, and every fixture names one.
func TestParserCoversEveryVariant(t *testing.T) {
	refused := map[string]error{
		"telegram-otherversion": androidtelegram.ErrUnsupportedVersion,
		"telegram-oldversion":   androidtelegram.ErrUnsupportedVersion,
		"telegram-nomessages":   androidtelegram.ErrNoMessageTable,
	}
	if len(variants) != 12 {
		t.Errorf("%d variants, want the twelve of spec 13", len(variants))
	}
	used := map[string]bool{}
	recorded := 0
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		used[fx.Expect.DDL] = true
		want, isRefused := refused[v]
		switch {
		case isRefused:
			if len(fx.Expect.Records) != 0 {
				t.Errorf("%s: a refused variant whose oracle states %d records", v, len(fx.Expect.Records))
			}
			res := parseAt(t, remoteApp, fx.Files)
			if !errors.Is(res.Err, want) || len(res.Records) != 0 || len(res.Violations) != 0 {
				t.Errorf("%s: err %v, %d records, violations %v; want %v and none", v, res.Err, len(res.Records), res.Violations, want)
			}
		case len(fx.Expect.Records) > 0:
			recorded++ // TestAndroidTelegramMatchesOracle parses it against its oracle
		default:
			t.Errorf("%s is parsed by no test with a stated outcome", v)
		}
	}
	if recorded != 9 {
		t.Errorf("%d variants with records, want the 9 TestAndroidTelegramMatchesOracle runs", recorded)
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "telegram-android", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DDL snapshots found: %v", err)
	}
	for _, f := range files {
		if !used[filepath.Base(f)] {
			t.Errorf("DDL snapshot %s is used by no fixture", filepath.Base(f))
		}
	}
	if used[""] {
		t.Error("a fixture names no DDL snapshot")
	}
}

// stageATypes are the types plan 4I4b adds to the table or grows (design D1): every full row of these is a 4I4b row
// the fixtures must read (P10). MessageMedia, Photo, Document, WebPage and MessageAction keep their 4I4 full rows,
// which are among them.
func stageATypes() []string {
	return []string{
		"PhotoSize", "VideoSize", "DocumentAttribute", "FileLocation", "InputStickerSet", "TL_maskCoords", "Video", "Audio",
		"MessageReplies", "MessageReactions", "Reaction", "ReactionCount", "MessagePeerReaction", "MessageReactor",
		"RestrictionReason", "TL_factCheck", "TL_textWithEntities", "SuggestedPost", "TL_stars.StarsAmount",
		"TL_messages_emojiGameOutcome", "WebPageAttribute", "TL_iv.RichText", "InputUser", "MessageMedia", "Photo",
		"Document", "WebPage", "MessageAction",
	}
}

// tlCells are the TL cells of a committed fixture database (its main file; a WAL frame is not committed to it), each
// with the type it is decoded as: messages and messages_v2 data and replydata as Message, replies_data as
// MessageReplies, users and chats data as User and Chat. A table or column the variant lacks gives nothing.
func tlCells(t *testing.T, db []byte) map[string][][]byte {
	t.Helper()
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(db), DBSize: int64(len(db))}, parse.NewBudget(1<<30).View())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	out := map[string][][]byte{}
	read := func(table, col, typ string) {
		tb, err := d.Table(t.Context(), table, nil, []string{col})
		if errors.Is(err, sqlitedb.ErrNoSuchTable) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		i := tb.Col(col)
		if i < 0 {
			return
		}
		if err := tb.Scan(t.Context(), func(row sqlitedb.Row) error {
			if b, ok := row.Blob(i); ok {
				out[typ] = append(out[typ], bytes.Clone(b))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"messages", "messages_v2"} {
		read(table, "data", androidtelegram.TypeMessage)
		read(table, "replydata", androidtelegram.TypeMessage)
		read(table, "replies_data", "MessageReplies")
	}
	read("users", "data", androidtelegram.TypeUser)
	read("chats", "data", "Chat")
	return out
}

// Plan 4I4b P10 (design 10, first bullet): the committed fixtures read every full row of the 4I4b types at least once.
// The union of SeenConstructors over every TL cell of every variant's database (messages and messages_v2 data,
// replydata and replies_data, users and chats data) holds every full row of stageATypes: the 33 full MessageMedia (32 in plan 4I4b; 0x4bd6e798 is full since 4I4c),
// every full Photo, Document and WebPage row, every full MessageAction row and every full row of the new types. A miss
// is listed by type, ID and class. An object counts only when read whole (the decode's stop neither at nor inside it,
// Task 4 review M1). Plan 4I4c (PL4): Poll is full, so TL_messageMediaPoll, exempt under 4I4b R1, is read whole too
// and no row is exempt.
func TestTelegramFixturesReadEveryStageAFullRow(t *testing.T) {
	seen := map[tl.Key]bool{}
	for _, v := range variants {
		for typ, cells := range tlCells(t, fixtureFiles(t, v)["db"]) {
			for _, b := range cells {
				for _, k := range androidtelegram.SeenConstructors(b, typ) {
					seen[k] = true
				}
			}
		}
	}
	var missing []string
	media, actions := 0, 0
	for k, c := range androidtelegram.Table() {
		if c.Body != tl.BodyFull || !slices.Contains(stageATypes(), c.Type) {
			continue
		}
		switch c.Type {
		case "MessageMedia":
			media++
		case "MessageAction":
			actions++
		}
		if !seen[k] {
			missing = append(missing, fmt.Sprintf("%s 0x%08x %s", c.Type, k.ID, c.Name))
		}
	}
	slices.Sort(missing)
	if media != 33 || actions != 13 {
		t.Errorf("%d full MessageMedia and %d full MessageAction rows, want the 33 and 13 of the emitted table (rulings T1d, PL4)", media, actions)
	}
	if len(missing) > 0 {
		t.Errorf("%d full 4I4b rows no committed fixture reads:\n%s", len(missing), strings.Join(missing, "\n"))
	}
}
