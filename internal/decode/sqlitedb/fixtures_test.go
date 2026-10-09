package sqlitedb_test

// Fixture helpers of the decoder tests. The committed fixtures live with the
// library (internal/sqlitefile/testdata); the oracle structs below are a copy
// of that package's test types, so the decoder is compared with an oracle that
// shares no code with it. loadFixture checks every file's SHA-256 against the
// oracle first, so a stale image or a stale oracle fails before anything else.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

const fixtureDir = "../../sqlitefile/testdata"

type fxVal struct {
	T string `json:"t"` // null, int, float (bit pattern in hex), text, blob (base64)
	V string `json:"v,omitempty"`
}

type fxLoc struct {
	File    string `json:"file"` // db, wal, journal
	Page    uint32 `json:"page"`
	Offset  int64  `json:"offset"`
	Length  int64  `json:"length"`
	CellHex string `json:"cell_hex"`
}

type fxCellRow struct {
	Rowid int64 `json:"rowid"`
	fxLoc
}

type fxGenerator struct {
	Tool       string            `json:"tool"`
	Version    string            `json:"version"`
	Class      string            `json:"class"`
	Statements []string          `json:"statements"`
	FileSHA256 map[string]string `json:"file_sha256"`
}

type fxHeader struct {
	PageSize         int    `json:"page_size"`
	Reserved         int    `json:"reserved"`
	Encoding         string `json:"encoding"`
	HeaderPages      uint32 `json:"header_pages"`
	HeaderPagesValid bool   `json:"header_pages_valid"`
	FilePages        uint32 `json:"file_pages"`
	ChangeCounter    uint32 `json:"change_counter"`
	VersionValidFor  uint32 `json:"version_valid_for"`
	SQLiteVersion    uint32 `json:"sqlite_version"`
	SchemaCookie     uint32 `json:"schema_cookie"`
	SchemaFormat     uint32 `json:"schema_format"`
	FreelistTrunk    uint32 `json:"freelist_trunk"`
	FreelistCount    uint32 `json:"freelist_count"`
	AutoVacuum       int    `json:"auto_vacuum"`
	LargestRoot      uint32 `json:"largest_root"`
	UserVersion      int32  `json:"user_version"`
	ApplicationID    uint32 `json:"application_id"`
	WriteVersion     int    `json:"write_version"`
	ReadVersion      int    `json:"read_version"`
}

type fxSchemaRow struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	TblName  string `json:"tbl_name"`
	Rootpage uint32 `json:"rootpage"`
	SQL      string `json:"sql"`
}

type fxLiveTable struct {
	WithoutRowid bool      `json:"without_rowid"`
	Rows         [][]fxVal `json:"rows"` // rowid tables: [rowid, columns...]
}

type fxInfo struct {
	IntegrityCheck string `json:"integrity_check"`
	FreelistCount  int64  `json:"freelist_count"`
	PageCount      int64  `json:"page_count"`
}

type fxFreelist struct {
	Trunks []uint32 `json:"trunks"`
	Leaves []uint32 `json:"leaves"`
}

type fxFrame struct {
	Slot       uint32 `json:"slot"`
	Page       uint32 `json:"page"`
	DBSize     uint32 `json:"db_size"`
	Salt1      uint32 `json:"salt1"`
	Salt2      uint32 `json:"salt2"`
	Check1     uint32 `json:"check1"`
	Check2     uint32 `json:"check2"`
	State      string `json:"state"`
	Linked     bool   `json:"linked"`
	Generation int    `json:"generation"`
	Offset     int64  `json:"offset"`
}

type fxGeneration struct {
	Salt1     uint32 `json:"salt1"`
	Salt2     uint32 `json:"salt2"`
	FirstSlot uint32 `json:"first_slot"`
	Slots     uint32 `json:"slots"`
	Anchored  bool   `json:"anchored"`
	Age       uint32 `json:"age"`
	Commits   uint32 `json:"commits"`
}

type fxWAL struct {
	HeaderValid        bool           `json:"header_valid"`
	BigEndian          bool           `json:"big_endian"`
	PageSize           uint32         `json:"page_size"`
	CheckpointSeq      uint32         `json:"checkpoint_seq"`
	Salt1              uint32         `json:"salt1"`
	Salt2              uint32         `json:"salt2"`
	FrameSlots         uint32         `json:"frame_slots"`
	TrailingBytes      int64          `json:"trailing_bytes"`
	FramesValid        uint32         `json:"frames_valid"`
	FramesCommitted    uint32         `json:"frames_committed"`
	LastCommit         uint32         `json:"last_commit"`
	Commits            uint32         `json:"commits"`
	FramesUncommitted  uint32         `json:"frames_uncommitted"`
	FramesBroken       uint32         `json:"frames_broken"`
	FramesDetached     uint32         `json:"frames_detached"`
	FramesStale        uint32         `json:"frames_stale"`
	DBPagesAfterCommit uint32         `json:"db_pages_after_commit"`
	MaxPageNumber      uint32         `json:"max_page_number"`
	Frames             []fxFrame      `json:"frames"`
	Generations        []fxGeneration `json:"generations"`
}

type fxJournalSeg struct {
	Offset          int64  `json:"offset"`
	DeclaredRecords uint32 `json:"declared_records"`
	Records         uint32 `json:"records"`
}

type fxJournalRec struct {
	Index      int    `json:"index"`
	Segment    int    `json:"segment"`
	Page       uint32 `json:"page"`
	Offset     int64  `json:"offset"`
	ChecksumOK bool   `json:"checksum_ok"`
	Applied    bool   `json:"applied"`
}

type fxJournal struct {
	Hot              bool           `json:"hot"`
	HeaderValid      bool           `json:"header_valid"`
	ZeroedHeader     bool           `json:"zeroed_header"`
	PageSize         uint32         `json:"page_size"`
	SectorSize       uint32         `json:"sector_size"`
	InitialPages     uint32         `json:"initial_pages"`
	Nonce            uint32         `json:"nonce"`
	Applied          bool           `json:"applied"`
	NotAppliedReason string         `json:"not_applied_reason"`
	RecordsTotal     uint32         `json:"records_total"`
	RecordsValid     uint32         `json:"records_valid"`
	AppliedRecords   uint32         `json:"applied_records"`
	Segments         []fxJournalSeg `json:"segments"`
	Records          []fxJournalRec `json:"records"` // empty when the header is zeroed
}

type fxHistory struct {
	Table      string   `json:"table"`
	Rowid      *int64   `json:"rowid"`
	Values     []fxVal  `json:"values"`
	Method     string   `json:"method"`
	Relation   string   `json:"relation"`
	Origin     string   `json:"origin"`
	Basis      string   `json:"basis"`
	Confidence int      `json:"confidence"`
	Notes      []string `json:"notes"`
	Cell       fxLoc    `json:"cell"`
}

type fxUnreachable struct {
	Table  string  `json:"table"`
	Rowid  *int64  `json:"rowid"`
	Values []fxVal `json:"values"`
	Marker string  `json:"marker"`
}

type fxWarnings struct {
	Live []string `json:"live"`
}

type fxExpect struct {
	Generator   fxGenerator                `json:"generator"`
	NotSQLite   bool                       `json:"not_sqlite,omitempty"`
	Header      *fxHeader                  `json:"header,omitempty"`
	Schema      []fxSchemaRow              `json:"schema,omitempty"`
	Live        map[string]fxLiveTable     `json:"live,omitempty"`
	Cells       map[string][]fxCellRow     `json:"cells,omitempty"`
	Info        *fxInfo                    `json:"info,omitempty"`
	Freelist    *fxFreelist                `json:"freelist,omitempty"`
	WAL         *fxWAL                     `json:"wal,omitempty"`
	Journal     *fxJournal                 `json:"journal,omitempty"`
	AsFound     map[string][][]fxVal       `json:"as_found,omitempty"` // raw rows: [rowid, record values...]
	History     []fxHistory                `json:"history,omitempty"`
	Unreachable []fxUnreachable            `json:"unreachable,omitempty"`
	Warnings    fxWarnings                 `json:"warnings"`
	Extra       map[string]json.RawMessage `json:"extra,omitempty"`
}

// fixtureExpect is the oracle of one fixture.
type fixtureExpect = fxExpect

func gunzipFixtureFile(p string) ([]byte, bool, error) {
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", p, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", p, err)
	}
	return b, true, nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// checkFixtureHashes compares the SHA-256 of every file with the oracle's
// generator.file_sha256 and says what is stale.
func checkFixtureHashes(name string, files map[string][]byte, want map[string]string) error {
	if len(files) != len(want) {
		return fmt.Errorf("fixture %s: the oracle lists %d files, the fixture has %d", name, len(want), len(files))
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if got := sha256Hex(files[k]); want[k] != got {
			return fmt.Errorf("fixture %s: %s has sha256 %s, the oracle describes %q (regenerate image and oracle together)", name, k, got, want[k])
		}
	}
	return nil
}

// readFixture reads the files and the oracle of a fixture and verifies them.
func readFixture(dir, name string) (db, wal, journal []byte, exp *fixtureExpect, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, name+".expect.json"))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("fixture %s: no oracle committed: %w", name, err)
	}
	exp = new(fixtureExpect)
	if err := json.Unmarshal(raw, exp); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("fixture %s: oracle: %w", name, err)
	}
	files := map[string][]byte{}
	for _, f := range []struct {
		suffix string
		dst    *[]byte
	}{{".db", &db}, {".db-wal", &wal}, {".db-journal", &journal}} {
		b, ok, err := gunzipFixtureFile(filepath.Join(dir, name+f.suffix+".gz"))
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if ok {
			files[name+f.suffix] = b
			*f.dst = b
		}
	}
	if db == nil {
		return nil, nil, nil, nil, fmt.Errorf("fixture %s: no database file", name)
	}
	if err := checkFixtureHashes(name, files, exp.Generator.FileSHA256); err != nil {
		return nil, nil, nil, nil, err
	}
	return db, wal, journal, exp, nil
}

// loadFixture returns the committed files of a fixture; a missing companion is
// nil.
func loadFixture(t testing.TB, name string) (db, wal, journal []byte) {
	t.Helper()
	db, wal, journal, _, err := readFixture(fixtureDir, name)
	if err != nil {
		t.Fatal(err)
	}
	return db, wal, journal
}

// loadExpect returns the oracle of a fixture, after the same hash check.
func loadExpect(t testing.TB, name string) *fixtureExpect {
	t.Helper()
	_, _, _, exp, err := readFixture(fixtureDir, name)
	if err != nil {
		t.Fatal(err)
	}
	return exp
}
