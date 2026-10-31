package ext4_test

import (
	"io"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// 3G-R22: no journal call changes what the live tree, the unallocated map or the filesystem info say.

type lvWalked struct {
	Path  string
	Entry filesys.Entry
	Err   string
}

type lvFile struct {
	Path string
	Data []byte
	Runs []filesys.Run
	Err  string
}

type lvView struct {
	Walk        []lvWalked
	Files       []lvFile
	Lookups     []filesys.Entry
	LookupErrs  []string
	Unallocated []filesys.Run
	Info        filesys.Info
}

// lvCapture records everything the live view says: a bounded Walk (deleted entries included), Lookup of every live
// path, Open plus a full read and the Runs of every live regular file, Unallocated and Info.
func lvCapture(t *testing.T, f *ext4.FS) lvView {
	t.Helper()
	var v lvView
	var live []filesys.Entry
	var livePaths []string
	count := 0
	err := filesys.Walk(f, f.Root(), "/", func(p string, e filesys.Entry, err error) error {
		count++
		if count > 1000 {
			t.Fatal("the walk is not bounded")
		}
		w := lvWalked{Path: p, Entry: e}
		if err != nil {
			w.Err = err.Error()
		}
		v.Walk = append(v.Walk, w)
		if !e.Deleted && e.Type == filesys.TypeFile {
			live = append(live, e)
			livePaths = append(livePaths, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for i, e := range live {
		lf := lvFile{Path: livePaths[i]}
		file, err := f.Open(e)
		if err != nil {
			lf.Err = err.Error()
		} else {
			data, rerr := io.ReadAll(io.NewSectionReader(file, 0, file.Size()))
			if rerr != nil {
				lf.Err = rerr.Error()
			}
			lf.Data, lf.Runs = data, file.Runs()
		}
		v.Files = append(v.Files, lf)
		le, lerr := f.Lookup(livePaths[i])
		v.Lookups = append(v.Lookups, le)
		if lerr != nil {
			v.LookupErrs = append(v.LookupErrs, lerr.Error())
		} else {
			v.LookupErrs = append(v.LookupErrs, "")
		}
	}
	v.Unallocated, err = f.Unallocated()
	if err != nil {
		t.Fatalf("Unallocated: %v", err)
	}
	v.Info = f.Info()
	return v
}

// lvImage: live, deleted and journalled files, a live journal region (needs_recovery) and a stale fragment.
func lvImage(t *testing.T) []byte {
	t.Helper()
	files := []ext4test.File{
		{Path: "/a.txt", Data: pattern(3000, 1)},
		{Path: "/d", Dir: true},
		{Path: "/d/b.bin", Data: pattern(5000, 2)},
		{Path: "/gone.txt", Data: pattern(2500, 3), Deleted: true},
		{Path: "/d/gone2.txt", Data: pattern(1500, 4), Deleted: true},
	}
	img, l := ext4test.BuildLayout(jtOpts(64), files)
	stale := liveTxn(5, 100, 101)
	stale.At = 1
	stale.Revokes = []uint64{100}
	l.WriteJournal(img, ext4test.JBD2{
		UUID: jtUUID, Bit64: true, CsumV3: true, Sequence: 20, Start: 10,
		Txns: []ext4test.JTxn{stale, {Seq: 20, At: 10, Tags: liveTxn(20, 110, 111).Tags}, liveTxn(21, 112)},
	})
	l.PatchSuper(img, func(sb []byte) { sb[0x60] |= 0x4 })
	return img
}

func TestJBD2NeverChangesLiveView(t *testing.T) {
	img := lvImage(t)
	touched, untouched := mustOpen(t, img), mustOpen(t, img)

	info, err := touched.Journal()
	if err != nil || !info.NeedsRecovery {
		t.Fatalf("Journal = %+v, %v; the image must carry a journal", info, err)
	}
	var txns []filesys.JournalTxn
	if err := touched.JournalTransactions(func(tx filesys.JournalTxn) bool { txns = append(txns, tx); return true }); err != nil {
		t.Fatal(err)
	}
	blocks := 0
	for _, tx := range txns {
		for i := range tx.Blocks {
			if _, _, err := touched.JournalBlock(tx, i); err == nil {
				blocks++
			}
		}
	}
	if len(txns) != 3 || blocks < 4 {
		t.Fatalf("%d transactions and %d readable blocks; the test must exercise live and stale transactions", len(txns), blocks)
	}

	a, b := lvCapture(t, touched), lvCapture(t, untouched)
	if len(a.Walk) < 5 || len(a.Files) < 2 {
		t.Fatalf("walk %d entries, %d files: the image is too small to prove anything", len(a.Walk), len(a.Files))
	}
	var deleted int
	for _, w := range a.Walk {
		if w.Entry.Deleted {
			deleted++
		}
	}
	if deleted < 2 {
		t.Fatalf("%d deleted entries seen; want the deleted files in the comparison", deleted)
	}
	if !reflect.DeepEqual(a, b) {
		switch {
		case !reflect.DeepEqual(a.Walk, b.Walk):
			t.Errorf("Walk differs after journal calls:\n%+v\n%+v", a.Walk, b.Walk)
		case !reflect.DeepEqual(a.Files, b.Files) || !reflect.DeepEqual(a.Lookups, b.Lookups):
			t.Errorf("Open/Lookup differ after journal calls")
		case !reflect.DeepEqual(a.Unallocated, b.Unallocated):
			t.Errorf("Unallocated differs after journal calls:\n%v\n%v", a.Unallocated, b.Unallocated)
		default:
			t.Errorf("Info differs after journal calls:\n%+v\n%+v", a.Info, b.Info)
		}
	}
	// The journal must not have been searched on behalf of the live view either: calling it first or last gives the same view.
	first := mustOpen(t, img)
	c := lvCapture(t, first)
	if _, err := first.Journal(); err != nil {
		t.Fatal(err)
	}
	if d := lvCapture(t, first); !reflect.DeepEqual(c, d) {
		t.Error("the live view changed between before and after Journal on the same FS")
	}
}
