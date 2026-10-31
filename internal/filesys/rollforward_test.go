package filesys_test

import (
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

type rfStub struct{ filesys.FileSystem }

func (rfStub) RollForward(visit func(filesys.RollForwardNode) bool) (filesys.RollForwardChain, error) {
	visit(filesys.RollForwardNode{Index: 1})
	return filesys.RollForwardChain{Nodes: 1, Complete: true}, nil
}

func (rfStub) PostCheckpointFiles() ([]filesys.PostCheckpointFile, error) { return nil, nil }

type rfPlain struct{ filesys.FileSystem }

type rfWrap struct{ filesys.FileSystem }

func (w rfWrap) Underlying() filesys.FileSystem { return w.FileSystem }

func TestRollForwardTypesZeroValuesAreInert(t *testing.T) {
	var c filesys.RollForwardChain
	if c.Complete || c.Nodes != 0 || c.Stop != "" || c.Files != nil || c.Warnings != nil {
		t.Errorf("zero chain is not inert: %+v", c)
	}
	var n filesys.RollForwardNode
	if n.WouldReplay || n.LiveIno || n.Inode != nil || n.Addrs != nil || n.NIDs != nil {
		t.Errorf("zero node claims something: %+v", n)
	}
	var f filesys.PostCheckpointFile
	if f.Candidates != nil || f.Err != nil || f.ID != "" {
		t.Errorf("zero file claims something: %+v", f)
	}
	if _, ok := filesys.As[filesys.RollForwarder](rfPlain{}); ok {
		t.Error("a plain filesystem is not a RollForwarder")
	}
	if _, ok := filesys.As[filesys.RollForwarder](rfWrap{rfStub{}}); !ok {
		t.Error("As does not see through a wrapper to an implementing filesystem")
	}
	rf, _ := filesys.As[filesys.RollForwarder](rfStub{})
	var seen []int
	ch, err := rf.RollForward(func(n filesys.RollForwardNode) bool { seen = append(seen, n.Index); return true })
	if err != nil || !reflect.DeepEqual(seen, []int{1}) || ch.Nodes != 1 {
		t.Errorf("RollForward = %+v, %v, seen %v", ch, err, seen)
	}
}
