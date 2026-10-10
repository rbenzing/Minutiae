package mb2_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/ios/mb2"
	"github.com/rbenzing/minutiae/internal/ios/mb2/mb2test"
)

func upload(d *mb2test.Device, ups ...mb2test.Upload) error {
	if code, err := d.UploadFiles(ups...); err != nil || code != 0 {
		return fmt.Errorf("upload: %d %v", code, err)
	}
	return nil
}

func start(d *mb2test.Device) error {
	if err := d.Handshake(); err != nil {
		return err
	}
	_, err := d.ExpectBackupRequest()
	return err
}

// W28: a move of a file onto itself is a no-op and never deletes the bytes.
func TestBackupMoveOntoItselfKeepsTheFile(t *testing.T) {
	dir, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := start(d); err != nil {
			return err
		}
		if err := upload(d, mb2test.Upload{DeviceName: "/a", Name: "U1/f", Data: []byte("abc"), EndRemote: true}); err != nil {
			return err
		}
		if code, _, err := d.Request("DLMessageMoveItems", map[string]any{"U1/f": "U1/f"}, 0.0); err != nil || code != 0 {
			return fmt.Errorf("move onto itself: %d %v", code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, rerr := os.ReadFile(filepath.Join(dir, "U1", "f")); rerr != nil || string(b) != "abc" {
		t.Fatalf("the received file was destroyed by a move onto itself: %q %v", b, rerr)
	}
	if res.RemovedFileCount != 0 || res.RemovedBytes != 0 || res.RemovedEndCount != 0 || len(res.RemoteEnded) != 1 {
		t.Fatalf("a no-op changed the result: %+v", res)
	}
}

func TestBackupMoveDifferingOnlyInCaseIsANoOpWhereTheFileSystemFolds(t *testing.T) {
	defer mb2.SetFoldCase(true)()
	dir, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := start(d); err != nil {
			return err
		}
		if err := upload(d, mb2test.Upload{DeviceName: "/a", Name: "U1/f", Data: []byte("abc")}); err != nil {
			return err
		}
		if code, _, err := d.Request("DLMessageMoveItems", map[string]any{"U1/f": "U1/F"}, 0.0); err != nil || code != 0 {
			return fmt.Errorf("case-only move: %d %v", code, err)
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, rerr := os.ReadFile(filepath.Join(dir, "U1", "f")); rerr != nil || string(b) != "abc" {
		t.Fatalf("the received file was destroyed by a case-only move: %q %v", b, rerr)
	}
	if res.RemovedFileCount != 0 {
		t.Fatalf("RemovedFileCount = %d", res.RemovedFileCount)
	}
}

func TestBackupMoveOntoAnAncestorOrDescendantIsRefused(t *testing.T) {
	dir, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := start(d); err != nil {
			return err
		}
		if err := upload(d, mb2test.Upload{DeviceName: "/a", Name: "U1/d/f", Data: []byte("abc")}); err != nil {
			return err
		}
		for _, mv := range []map[string]any{{"U1/d": "U1/d/sub"}, {"U1/d/f": "U1/d"}} {
			if code, _, err := d.Request("DLMessageMoveItems", mv, 0.0); err != nil || code == 0 {
				return fmt.Errorf("move %v must be refused: %d %v", mv, code, err)
			}
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, rerr := os.ReadFile(filepath.Join(dir, "U1", "d", "f")); rerr != nil || string(b) != "abc" {
		t.Fatalf("a refused move destroyed the file: %q %v", b, rerr)
	}
	if res.RefusedMoveCount != 2 || len(res.RefusedMoves) != 2 || res.RemovedFileCount != 0 {
		t.Fatalf("refused %d %v, removed %d", res.RefusedMoveCount, res.RefusedMoves, res.RemovedFileCount)
	}
}

// W29: every removal or replacement of received bytes is counted, marked or not.
func TestBackupCountsEveryDeviceRequestedRemovalOrReplacement(t *testing.T) {
	_, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := start(d); err != nil {
			return err
		}
		if err := upload(d,
			mb2test.Upload{DeviceName: "/a", Name: "U1/a", Data: []byte("aaa")},
			mb2test.Upload{DeviceName: "/b", Name: "U1/b", Data: []byte("bbbb")},
			mb2test.Upload{DeviceName: "/c", Name: "U1/c", Data: []byte("ccccc")},
			mb2test.Upload{DeviceName: "/x", Name: "U1/dir/x", Data: []byte("xx")},
			mb2test.Upload{DeviceName: "/y", Name: "U1/dir/y", Data: []byte("y")},
			mb2test.Upload{DeviceName: "/e", Name: "U1/e", Data: []byte("eeeeee")},
			mb2test.Upload{DeviceName: "/z", Name: "U1/z", Data: []byte("zz")},
		); err != nil {
			return err
		}
		if err := upload(d, mb2test.Upload{DeviceName: "/z", Name: "U1/z", Data: []byte("zzzz")}); err != nil { // replaces 2 bytes
			return err
		}
		for _, step := range []struct {
			name string
			msg  []any
		}{
			{"remove a", []any{"DLMessageRemoveItems", []any{"U1/a"}, 0.0}},                     // 1 file, 3 bytes
			{"move b over c", []any{"DLMessageMoveItems", map[string]any{"U1/b": "U1/c"}, 0.0}}, // 1 file, 5 bytes
			{"remove dir", []any{"DLMessageRemoveItems", []any{"U1/dir"}, 0.0}},                 // 2 files, 3 bytes
			{"copy c over e", []any{"DLMessageCopyItem", "U1/c", "U1/e"}},                       // 1 file, 6 bytes
			{"remove nothing", []any{"DLMessageRemoveItems", []any{"U1/never-existed"}, 0.0}},   // 0
		} {
			if code, _, err := d.Request(step.msg...); err != nil || code != 0 {
				return fmt.Errorf("%s: %d %v", step.name, code, err)
			}
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemovedFileCount != 6 || res.RemovedBytes != 2+3+5+3+6 {
		t.Fatalf("removed %d files, %d bytes; want 6 files, 19 bytes", res.RemovedFileCount, res.RemovedBytes)
	}
}

// W31: the message budget follows the live marks, so DetailOmitted is exact.
func TestBackupDetailOmittedFollowsTheLiveMarks(t *testing.T) {
	defer mb2.SetFoldCase(false)() // keys are compared as sent
	const n = mb2.MaxRecordedErrors + 5
	files := make([]mb2test.Upload, n)
	for i := range files {
		files[i] = mb2test.Upload{DeviceName: "/x", Name: fmt.Sprintf("U1/f%04d", i), Data: []byte("x"), EndRemote: true, RemoteMsg: []byte("m")}
	}
	_, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := start(d); err != nil {
			return err
		}
		if err := upload(d, files...); err != nil {
			return err
		}
		// Two of the five marks without a message are removed, one is replaced by a
		// complete re-send, one with a message is moved over another.
		if code, _, err := d.Request("DLMessageRemoveItems", []any{"U1/f1000", "U1/f1001"}, 0.0); err != nil || code != 0 {
			return fmt.Errorf("remove: %d %v", code, err)
		}
		if err := upload(d, mb2test.Upload{DeviceName: "/x", Name: "U1/f1002", Data: []byte("x")}); err != nil {
			return err
		}
		if code, _, err := d.Request("DLMessageMoveItems", map[string]any{"U1/f0000": "U1/f0001"}, 0.0); err != nil || code != 0 {
			return fmt.Errorf("move: %d %v", code, err)
		}
		// One slot is free now (the move overwrote a mark that kept its message): new1 keeps its message, new2 and new3 do not.
		if err := upload(d,
			mb2test.Upload{DeviceName: "/x", Name: "U1/new1", Data: []byte("x"), EndRemote: true, RemoteMsg: []byte("n1")},
			mb2test.Upload{DeviceName: "/x", Name: "U1/new2", Data: []byte("x"), EndRemote: true, RemoteMsg: []byte("n2")},
			mb2test.Upload{DeviceName: "/x", Name: "U1/new3", Data: []byte("x"), EndRemote: true, RemoteMsg: []byte("n3")},
		); err != nil {
			return err
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	without := 0
	for _, e := range res.RemoteEnded {
		if e.Message == "" {
			without++
		}
	}
	if without != res.RemoteEndedDetailOmitted || res.RemoteEndedDetailOmitted != 4 {
		t.Fatalf("detail omitted %d, marks without message %d, want both 4 (5 - 2 removed - 1 replaced + new2 + new3)", res.RemoteEndedDetailOmitted, without)
	}
	for _, k := range []string{"U1/new1"} {
		if res.RemoteEnded[k].Message == "" {
			t.Errorf("%s lost its message although slots were free", k)
		}
	}
}

// W33: where the file system folds case, the keys are folded, so a re-send in
// another case clears the mark.
func TestBackupRemoteEndKeysAreFoldedWhereTheFileSystemFolds(t *testing.T) {
	defer mb2.SetFoldCase(true)()
	_, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := start(d); err != nil {
			return err
		}
		if err := upload(d,
			mb2test.Upload{DeviceName: "/a", Name: "U1/File", Data: []byte("abc"), EndRemote: true},
			mb2test.Upload{DeviceName: "/b", Name: "U1/Keep", Data: []byte("abc"), EndRemote: true},
		); err != nil {
			return err
		}
		if err := upload(d, mb2test.Upload{DeviceName: "/a", Name: "U1/FILE", Data: []byte("abcdef")}); err != nil {
			return err
		}
		return d.Finish(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.RemoteEnded["u1/keep"]; !ok || len(res.RemoteEnded) != 1 {
		t.Fatalf("RemoteEnded = %+v, want only the folded key u1/keep", res.RemoteEnded)
	}
}

// W34: a re-send that fails part-way replaces the earlier mark of that path.
func TestBackupIncompleteResendReplacesTheEarlierMark(t *testing.T) {
	_, res, err := runBackup(t, func(d *mb2test.Device) error {
		if err := start(d); err != nil {
			return err
		}
		if err := upload(d, mb2test.Upload{DeviceName: "/a", Name: "U1/f", Data: []byte("abc"), EndRemote: true, RemoteMsg: []byte("m")}); err != nil {
			return err
		}
		if err := d.Send("DLMessageUploadFiles", map[string]any{}, 0.0); err != nil {
			return err
		}
		for _, step := range []error{mb2.WriteName(d, "/a"), mb2.WriteName(d, "U1/f"), mb2.WriteU32(d, 100)} {
			if step != nil {
				return step
			}
		}
		_, err := d.Write([]byte{mb2.CodeFileData, 'x'}) // the block is cut off
		return err
	})
	if err == nil {
		t.Fatal("a transfer cut off mid-block must fail the backup")
	}
	if len(res.Incomplete) != 1 || res.Incomplete[0] != "U1/f" {
		t.Fatalf("Incomplete = %v", res.Incomplete)
	}
	if len(res.RemoteEnded) != 0 {
		t.Fatalf("the stale mark of the replaced content survived: %+v", res.RemoteEnded)
	}
}
