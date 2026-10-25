package evidence_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

func fileLine(off, length, img int64, path, id string, size int64, uniform, exported bool) string {
	return fmt.Sprintf(`{"offset":%d,"length":%d,"image_offset":%d,"fs_path":%q,"path_escaped":false,"fs_id":%q,"file_size":%d,"uniform":%t,"exported":%t}`+"\n",
		off, length, img, path, id, size, uniform, exported)
}

func volLine(off, length, img int64, uniform, exported bool) string {
	return fmt.Sprintf(`{"offset":%d,"length":%d,"image_offset":%d,"uniform":%t,"exported":%t}`+"\n", off, length, img, uniform, exported)
}

func readSlack(t *testing.T, in, method string) ([]evidence.SlackMapLine, error) {
	t.Helper()
	var got []evidence.SlackMapLine
	err := evidence.ReadSlackRunMap(strings.NewReader(in), method, func(l evidence.SlackMapLine) error {
		got = append(got, l)
		return nil
	})
	return got, err
}

func TestSlackRunMapRoundTrip(t *testing.T) {
	files := []evidence.SlackMapLine{
		{Offset: 0, Length: 324, ImageOffset: 4796, FSPath: "/a.txt", FSID: "dirent:2:3", FileSize: 700, Exported: true},
		{Offset: 324, Length: 700, ImageOffset: 9000, FSPath: `/z\x00.bin`, PathEscaped: true, FSID: "ino:7", FileSize: 5, Uniform: true, Exported: false},
		{Offset: 324, Length: 36, ImageOffset: 5084, FSPath: "/b.txt", FSID: "dirent:2:4", FileSize: 1500, Uniform: true, Exported: true},
	}
	var buf bytes.Buffer
	for _, l := range files {
		if err := evidence.WriteSlackMapLine(&buf, evidence.MethodSlackFile, l); err != nil {
			t.Fatal(err)
		}
	}
	wantFirst := `{"offset":0,"length":324,"image_offset":4796,"fs_path":"/a.txt","path_escaped":false,"fs_id":"dirent:2:3","file_size":700,"uniform":false,"exported":true}`
	if first, _, _ := strings.Cut(buf.String(), "\n"); first != wantFirst {
		t.Errorf("first file line\n got %s\nwant %s", first, wantFirst)
	}
	got, err := readSlack(t, buf.String(), evidence.MethodSlackFile)
	if err != nil || !slices.Equal(got, files) {
		t.Fatalf("file lines: got %+v, %v; want %+v", got, err, files)
	}

	vols := []evidence.SlackMapLine{
		{Offset: 0, Length: 4096, ImageOffset: 1000, Uniform: true, Exported: true},
		{Offset: 4096, Length: 100, ImageOffset: 5096, Exported: true},
	}
	buf.Reset()
	for _, l := range vols {
		if err := evidence.WriteSlackMapLine(&buf, evidence.MethodSlackVolume, l); err != nil {
			t.Fatal(err)
		}
	}
	if first, _, _ := strings.Cut(buf.String(), "\n"); first != `{"offset":0,"length":4096,"image_offset":1000,"uniform":true,"exported":true}` {
		t.Errorf("first volume line %s", first)
	}
	got, err = readSlack(t, buf.String(), evidence.MethodSlackVolume)
	if err != nil || !slices.Equal(got, vols) {
		t.Fatalf("volume lines: got %+v, %v; want %+v", got, err, vols)
	}

	runs, err := evidence.SlackMapRuns(strings.NewReader(fileLine(0, 324, 4796, "/a", "i", 1, false, true)+
		fileLine(324, 9, 77, "/b", "j", 1, true, false)+fileLine(324, 5, 100, "/c", "k", 1, false, true)), evidence.MethodSlackFile)
	want := []evidence.Run{{Offset: 4796, Length: 324}, {Offset: 100, Length: 5}}
	if err != nil || !slices.Equal(runs, want) {
		t.Errorf("SlackMapRuns = %v, %v; want %v (exported lines only)", runs, err, want)
	}
}

func TestSlackRunMapWriterRefusesWhatTheReaderRefuses(t *testing.T) {
	bad := []evidence.SlackMapLine{
		{Offset: 0, Length: 0, FSPath: "/a", FSID: "i", Exported: true},
		{Offset: 0, Length: 1, ImageOffset: -1, FSPath: "/a", FSID: "i", Exported: true},
		{Offset: 0, Length: 1, FSPath: "/a\xff", FSID: "i", Exported: true},
		{Offset: 0, Length: 1, FSPath: "/a\x00", FSID: "i", Exported: true},
		{Offset: 0, Length: 1, FSPath: "/a", FSID: "", Exported: true},
		{Offset: 0, Length: 1, FSPath: "/a", FSID: "i", FileSize: -1, Exported: true},
		{Offset: 0, Length: 1, FSPath: "/a", FSID: "i", Exported: false},
	}
	for i, l := range bad {
		if err := evidence.WriteSlackMapLine(&bytes.Buffer{}, evidence.MethodSlackFile, l); err == nil {
			t.Errorf("file line %d %+v: the writer accepted it", i, l)
		}
	}
	if err := evidence.WriteSlackMapLine(&bytes.Buffer{}, evidence.MethodSlackVolume, evidence.SlackMapLine{Length: 1, FSPath: "/a", Exported: true}); err == nil {
		t.Error("a volume line with fs_path: the writer accepted it")
	}
	if err := evidence.WriteSlackMapLine(&bytes.Buffer{}, "carve-sig", evidence.SlackMapLine{Length: 1, Exported: true}); err == nil {
		t.Error("unknown method: the writer accepted it")
	}
}

func TestSlackRunMapStrict(t *testing.T) {
	ok := fileLine(0, 324, 4796, "/a.txt", "dirent:2:3", 700, false, true)
	longPad := ok[:len(ok)-1] + strings.Repeat(" ", evidence.MaxSlackMapLine+1-(len(ok)-1))
	type row struct {
		name, method, in string
		line             int // the line the message names; 0: none
	}
	f, v := evidence.MethodSlackFile, evidence.MethodSlackVolume
	rows := []row{
		{"unknown key", f, strings.Replace(ok, `"exported":true`, `"exported":true,"x":1`, 1), 1},
		{"missing key", f, strings.Replace(ok, `"file_size":700,`, ``, 1), 1},
		{"repeated key", f, strings.Replace(ok, `"uniform":false`, `"uniform":false,"uniform":false`, 1), 1},
		{"Offset-cased key", f, strings.Replace(ok, `"offset"`, `"Offset"`, 1), 1},
		{"offset gap", f, ok + fileLine(325, 1, 0, "/b", "i", 1, false, true), 2},
		{"offset overlap", f, ok + fileLine(323, 1, 0, "/b", "i", 1, false, true), 2},
		{"first offset not 0", f, fileLine(5, 1, 0, "/b", "i", 1, false, true), 1},
		{"length 0", f, fileLine(0, 0, 4796, "/a", "i", 1, false, true), 1},
		{"negative image_offset", f, fileLine(0, 1, -1, "/a", "i", 1, false, true), 1},
		{"offset+length overflow", f, ok + fileLine(324, math.MaxInt64, 0, "/b", "i", 1, false, true), 2},
		{"image_offset+length overflow", f, fileLine(0, 10, math.MaxInt64, "/a", "i", 1, false, true), 1},
		{"invalid UTF-8 fs_path", f, strings.Replace(ok, "/a.txt", "/a\xff.txt", 1), 1},
		{"NUL in fs_path", f, strings.Replace(ok, "/a.txt", `/a\u0000.txt`, 1), 1},
		{"fs_path of 32 KiB + 1", f, fileLine(0, 1, 0, "/"+strings.Repeat("a", 32<<10), "i", 1, false, true), 1},
		{"empty fs_id", f, fileLine(0, 1, 0, "/a", "", 1, false, true), 1},
		{"fs_id of 257 bytes", f, fileLine(0, 1, 0, "/a", strings.Repeat("i", 257), 1, false, true), 1},
		{"negative file_size", f, fileLine(0, 1, 0, "/a", "i", -1, false, true), 1},
		{"uniform not a bool", f, strings.Replace(ok, `"uniform":false`, `"uniform":"no"`, 1), 1},
		{"exported not a bool", f, strings.Replace(ok, `"exported":true`, `"exported":1`, 1), 1},
		{"volume line with fs_path", v, strings.Replace(volLine(0, 1, 0, false, true), `"uniform"`, `"fs_path":"/a","uniform"`, 1), 1},
		{"file line without fs_id", f, strings.Replace(ok, `"fs_id":"dirent:2:3",`, ``, 1), 1},
		{"file line read as volume", v, ok, 1},
		{"volume line read as file", f, volLine(0, 1, 0, false, true), 1},
		{"line of MaxSlackMapLine+1 bytes", f, longPad + "\n", 1},
		{"blank line", f, ok + "\n", 2},
		{"two values on one line", f, strings.TrimSuffix(ok, "\n") + ` {"a":1}` + "\n", 1},
		{"trailing garbage", f, strings.TrimSuffix(ok, "\n") + "x\n", 1},
		{"not an object", f, "[1]\n", 1},
		{"exported false with uniform false", f, fileLine(0, 1, 0, "/a", "i", 1, false, false), 1},
		{"exported false at a wrong offset", f, ok + fileLine(0, 9, 77, "/b", "j", 1, true, false), 2},
		{"exported false behind the offset", v, volLine(0, 8, 0, true, true) + volLine(0, 8, 8, true, false), 2},
		{"unknown method", "slack-bogus", volLine(0, 1, 0, false, true), 0},
		{"empty method", "", volLine(0, 1, 0, false, true), 0},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			_, err := readSlack(t, r.in, r.method)
			if !errors.Is(err, evidence.ErrIntegrity) {
				t.Fatalf("err = %v, want ErrIntegrity", err)
			}
			if want := fmt.Sprintf("line %d:", r.line); r.line > 0 && !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		})
	}

	accept := map[string]struct{ method, in string }{
		"escaped label, path_escaped false": {f, fileLine(0, 1, 0, `/a\x80`, "i", 1, false, true)},
		"no final newline":                  {f, strings.TrimSuffix(ok, "\n")},
		"empty map":                         {f, ""},
		"exported false then true":          {v, volLine(0, 8, 0, true, false) + volLine(0, 8, 8, false, true)},
		"fs_path of exactly 32 KiB":         {f, fileLine(0, 1, 0, strings.Repeat("a", 32<<10), "i", 1, false, true)},
		"fs_id of 256 bytes":                {f, fileLine(0, 1, 0, "/a", strings.Repeat("i", 256), 1, false, true)},
	}
	for name, a := range accept {
		if _, err := readSlack(t, a.in, a.method); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSlackRunMapLineCap(t *testing.T) {
	defer evidence.SetSlackMapLineCap(3)()
	three := volLine(0, 1, 0, false, true) + volLine(1, 1, 0, false, true) + volLine(2, 1, 0, false, true)
	if _, err := readSlack(t, three, evidence.MethodSlackVolume); err != nil {
		t.Fatalf("three exported lines at a cap of three: %v", err)
	}
	if _, err := readSlack(t, three+volLine(3, 1, 0, false, true), evidence.MethodSlackVolume); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("four exported lines at a cap of three: err %v, want ErrIntegrity", err)
	}
	omitted := strings.Repeat(volLine(0, 1, 0, true, false), 4)
	if _, err := readSlack(t, omitted, evidence.MethodSlackVolume); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("four omitted lines at a cap of three: err %v, want ErrIntegrity", err)
	}
}

func TestDerivedRunsReadsSlackMap(t *testing.T) {
	c, parent, data := recoverParent(t)
	bin, _ := evidencetest.AddSlack(t, c, parent, data, evidencetest.SlackSpec{Lines: []evidence.SlackMapLine{
		{ImageOffset: 4796, Length: 324, FSPath: "/a.txt", FSID: "dirent:2:3", FileSize: 700, Exported: true},
		{ImageOffset: 9000, Length: 100, FSPath: "/u", FSID: "dirent:2:4", FileSize: 5, Uniform: true},
		{ImageOffset: 100, Length: 36, FSPath: "/b.txt", FSID: "dirent:2:5", FileSize: 1500, Exported: true},
	}})
	got, err := c.DerivedRuns(bin, nil)
	want := []evidence.Run{{Offset: 4796, Length: 324}, {Offset: 100, Length: 36}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("DerivedRuns = %v, %v; want %v", got, err, want)
	}

	c2, parent2, data2 := recoverParent(t)
	bin2, _ := evidencetest.AddSlack(t, c2, parent2, data2, evidencetest.SlackSpec{
		Lines:  []evidence.SlackMapLine{{ImageOffset: 4796, Length: 324, FSPath: "/a.txt", FSID: "dirent:2:3", FileSize: 700, Exported: true}},
		Mutate: func(r *evidence.Recovery) { r.Method = "carve-sig" },
	})
	if runs, err := c2.DerivedRuns(bin2, nil); err == nil {
		t.Errorf("a slack map under a method that is not slack-*: DerivedRuns = %v, nil error", runs)
	}
}

func TestCheckedRunsReadsSlackMap(t *testing.T) {
	c, parent, data := recoverParent(t)
	bin, _ := evidencetest.AddSlack(t, c, parent, data, evidencetest.SlackSpec{
		Method: evidence.MethodSlackVolume,
		Lines: []evidence.SlackMapLine{
			{ImageOffset: 8192, Length: 4096, Uniform: true, Exported: true},
			{ImageOffset: 12288, Length: 100, Exported: true},
		},
	})
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.CheckedRuns(evidence.NewManifestIndex(recs), bin)
	want := []evidence.Run{{Offset: 8192, Length: 4096}, {Offset: 12288, Length: 100}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("CheckedRuns = %v, %v; want %v", got, err, want)
	}

	c2, parent2, data2 := recoverParent(t)
	bin2, _ := evidencetest.AddSlack(t, c2, parent2, data2, evidencetest.SlackSpec{
		Lines:  []evidence.SlackMapLine{{ImageOffset: 4796, Length: 324, FSPath: "/a.txt", FSID: "dirent:2:3", FileSize: 700, Exported: true}},
		Mutate: func(r *evidence.Recovery) { r.Method = "carve-sig" },
	})
	recs2, err := c2.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if runs, err := c2.CheckedRuns(evidence.NewManifestIndex(recs2), bin2); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("a slack map under a method that is not slack-*: CheckedRuns = %v, %v; want ErrIntegrity", runs, err)
	}
}
