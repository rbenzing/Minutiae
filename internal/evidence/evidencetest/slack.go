package evidencetest

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// SlackSpec describes a slack artifact for AddSlack. Empty fields take the defaults noted beside them.
type SlackSpec struct {
	Method    string // default evidence.MethodSlackFile
	Partition int    // default 1
	FSType    string // default "mtfs"
	// Lines are the run map lines: ImageOffset and Length address the parent. Offset is filled in order: the
	// current .bin offset, which an Exported line advances by its Length and an omitted line leaves alone.
	Lines []evidence.SlackMapLine
	// Alloc defaults to {Allocated: sum} for a file artifact and {Unknown: sum} for a volume artifact, sum
	// being the bytes of the exported lines.
	Alloc      *evidence.AllocSummary
	Params     map[string]string
	Incomplete bool
	Error      string
	Mutate     func(*evidence.Recovery)
}

// AddSlack writes a slack artifact and its run map sidecar (written first, kind runs) through Capture, as a
// writer with a bug would: manifest, artifacts.db and the audit entry agree with each other, so only the
// rules under test can object. The artifact lands at slack/p<N>-<fstype>/slack.bin (file) or
// volume-slack.bin (volume) under RecoveredAcq. Its bytes are the parent's bytes at the exported lines.
func AddSlack(t testing.TB, c *evidence.Case, parent evidence.ManifestRecord, parentData []byte, s SlackSpec) (bin, sidecar evidence.ManifestRecord) {
	t.Helper()
	if s.Method == "" {
		s.Method = evidence.MethodSlackFile
	}
	if s.Partition == 0 {
		s.Partition = 1
	}
	if s.FSType == "" {
		s.FSType = "mtfs"
	}
	name := "slack"
	basis := []string{"allocated-units-of-live-files"}
	if s.Method == evidence.MethodSlackVolume {
		name = "volume-slack"
		basis = []string{"filesystem-size=1", "partition-length=2"}
	}
	rel := "slack/p" + strconv.Itoa(s.Partition) + "-" + s.FSType + "/" + name
	var side, data bytes.Buffer
	var cur, sum int64
	for _, l := range s.Lines {
		l.Offset = cur
		if err := evidence.WriteSlackMapLine(&side, s.Method, l); err != nil {
			t.Fatal(err)
		}
		if !l.Exported {
			continue
		}
		cur += l.Length
		sum += l.Length
		if l.ImageOffset >= 0 && l.Length >= 0 && l.ImageOffset+l.Length <= int64(len(parentData)) {
			data.Write(parentData[l.ImageOffset : l.ImageOffset+l.Length])
		}
	}
	alloc := evidence.AllocSummary{Allocated: sum}
	if s.Method == evidence.MethodSlackVolume {
		alloc = evidence.AllocSummary{Unknown: sum}
	}
	if s.Alloc != nil {
		alloc = *s.Alloc
	}
	conf := 10
	rv := &evidence.Recovery{
		Class: evidence.ClassSlack, Method: s.Method, Confidence: &conf, Basis: basis, Alloc: alloc,
		Content: "ok", Algorithm: "minutiae-slack/1", Params: s.Params,
	}
	if s.Mutate != nil {
		s.Mutate(rv)
	}
	dev := parent.Source.DeviceID
	sd := &evidence.Derivation{ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: s.Partition, FSType: s.FSType}
	sidecar, err := c.Capture(dev, RecoveredAcq, rel+".runs.jsonl", evidence.Source{Kind: "runs", DeviceID: dev, Derived: sd}, func(w io.Writer) error {
		_, err := w.Write(side.Bytes())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &evidence.Derivation{
		ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: s.Partition, FSType: s.FSType,
		RunsArtifact: sidecar.ID, Recovery: rv,
	}
	bin, err = c.Capture(dev, RecoveredAcq, rel+".bin", evidence.Source{Kind: evidence.KindSlack, DeviceID: dev, Derived: d}, func(w io.Writer) error {
		if _, err := w.Write(data.Bytes()); err != nil {
			return err
		}
		if s.Incomplete {
			msg := s.Error
			if msg == "" {
				msg = "evidencetest: the slack export is incomplete"
			}
			return errors.New(msg)
		}
		return nil
	})
	if s.Incomplete {
		if !bin.Incomplete {
			t.Fatalf("evidencetest: record %+v err %v is not incomplete", bin, err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	return bin, sidecar
}
