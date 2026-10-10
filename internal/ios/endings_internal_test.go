package ios

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/ios/mb2"
)

func TestJudgeEndings(t *testing.T) {
	ends := map[string]mb2.RemoteEnd{
		"U1/aa/aaa": {Code: mb2.CodeErrorRemote, Bytes: 10, Message: "m"},
		"U1/bb/bbb": {Code: mb2.CodeErrorRemote, Bytes: 10},
		"U1/cc/ccc": {Code: mb2.CodeErrorRemote, Bytes: 10},
		"U1/dd/ddd": {Code: mb2.CodeErrorRemote, Bytes: 10},
	}
	oracle := func(rel string) (int64, bool, error) {
		switch rel {
		case "U1/aa/aaa":
			return 10, true, nil
		case "U1/bb/bbb":
			return 99, true, nil
		case "U1/cc/ccc":
			return 0, false, nil
		}
		return 0, false, errors.New("manifest unreadable")
	}
	cut, rep := judgeEndings(ends, oracle)
	if rep.Confirmed != 1 || rep.Mismatch != 1 || rep.Unconfirmed != 2 || len(cut) != 3 {
		t.Fatalf("report = %+v, cut = %v", rep, cut)
	}
	if _, ok := cut["U1/aa/aaa"]; ok {
		t.Error("a confirmed file must not be cut")
	}
	if !errors.Is(cut["U1/bb/bbb"], errEndSizeMismatch) || !errors.Is(cut["U1/cc/ccc"], errEndUnconfirmed) || !errors.Is(cut["U1/dd/ddd"], errEndUnconfirmed) {
		t.Fatalf("causes = %v", cut)
	}
	if len(rep.Samples) != 3 || !strings.HasPrefix(rep.Samples[0], `"U1/bb/bbb": `) {
		t.Fatalf("samples must list the three flagged files in path order: %q", rep.Samples)
	}
}

func TestJudgeEndingsWithoutOracleIsUnconfirmed(t *testing.T) {
	cut, rep := judgeEndings(map[string]mb2.RemoteEnd{"U1/x": {Code: mb2.CodeErrorRemote, Bytes: 1}}, nil)
	if rep.Unconfirmed != 1 || !errors.Is(cut["U1/x"], errEndUnconfirmed) {
		t.Fatalf("report = %+v, cut = %v", rep, cut)
	}
}

func TestJudgeEndingsBoundsSamples(t *testing.T) {
	ends := map[string]mb2.RemoteEnd{}
	for i := range maxEndingSamples + 50 {
		ends["U1/f"+string(rune('a'+i%26))+string(rune('a'+i/26))] = mb2.RemoteEnd{Code: mb2.CodeErrorRemote}
	}
	cut, rep := judgeEndings(ends, nil)
	if len(rep.Samples) != maxEndingSamples || rep.Unconfirmed != len(ends) || len(cut) != len(ends) {
		t.Fatalf("samples %d, unconfirmed %d, cut %d", len(rep.Samples), rep.Unconfirmed, len(cut))
	}
}

// W41: a device path in a sample is quoted, like the message.
func TestJudgeEndingsQuotesTheDevicePath(t *testing.T) {
	_, rep := judgeEndings(map[string]mb2.RemoteEnd{
		"u1/x": {Code: mb2.CodeErrorRemote, Bytes: 1, Path: "U1/\nX", Message: "m"},
	}, nil)
	if len(rep.Samples) != 1 || rep.Samples[0] != `"U1/\nX": code 0x0b, 1 bytes, "m"` {
		t.Fatalf("samples = %q", rep.Samples)
	}
}

// W39: when a file is both cut off and ended by an error block, the recorded
// cause is the transfer interruption, whatever the order of the entries.
func TestFindCutPrefersTheInterruptedTransfer(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	end, cut := cutFile{fi: fi, cause: errEndUnconfirmed}, cutFile{fi: fi, cause: errTransferInterrupted}
	for _, cuts := range [][]cutFile{{end, cut}, {cut, end}} {
		if got := findCut(p, cuts); got == nil || got.cause != errTransferInterrupted {
			t.Fatalf("findCut = %+v, want the interrupted transfer", got)
		}
	}
}
