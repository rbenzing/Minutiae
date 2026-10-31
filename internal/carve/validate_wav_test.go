package carve

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

func waveVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateRIFFWave(riffWindow(buf, newBudget(1<<30)))
}

// TestReadFailReason: the one mapping of a window read failure to a typed
// rejection reason (ruling 3E-C1): past the window is truncated, anything else
// (media, budget, cancel) is unreadable.
func TestReadFailReason(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{ErrOutsideWindow, "truncated"},
		{errors.Join(errors.New("x"), ErrOutsideWindow), "truncated"},
		{errMedia, "unreadable"},
		{ErrValidateBudget, "unreadable"},
		{context.Canceled, "unreadable"},
	} {
		if got := readFailReason(c.err); got != c.want {
			t.Errorf("readFailReason(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestWAVValidatesBuilderVariants(t *testing.T) {
	for name, o := range map[string]carvetest.WAVOpts{
		"mono 16-bit":        {},
		"mono 8-bit":         {Bits: 8, DataLen: 100},
		"stereo 16-bit 44.1": {Channels: 2, Rate: 44100, DataLen: 400},
		"list before data":   {ListBefore: true},
		"odd chunk with pad": {OddChunk: true},
		"odd data size":      {Bits: 8, DataLen: 101},
	} {
		t.Run(name, func(t *testing.T) {
			obj := carvetest.WAV(o)
			v := waveVerdict(obj, 0)
			ch, rate := max(o.Channels, 1), o.Rate
			if rate == 0 {
				rate = 8000
			}
			if v.Outcome != Validated || v.Length != int64(len(obj)) {
				t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
			}
			if want := []string{"channels=" + strconv.Itoa(ch), "rate=" + strconv.Itoa(rate)}; !slices.Equal(v.Basis, want) {
				t.Errorf("basis = %v, want %v", v.Basis, want)
			}
			if v := waveVerdict(obj, 4096); v.Outcome != Validated || v.Length != int64(len(obj)) {
				t.Errorf("with trailing fill: verdict = %+v", v)
			}
		})
	}
}

func TestWAVRejects(t *testing.T) {
	ch := carvetest.RIFFChunk
	fmtChunk := func(mod func(f []byte)) []byte {
		f := make([]byte, 16)
		binary.LittleEndian.PutUint16(f, 1)
		binary.LittleEndian.PutUint16(f[2:], 1)
		binary.LittleEndian.PutUint32(f[4:], 8000)
		binary.LittleEndian.PutUint32(f[8:], 16000)
		binary.LittleEndian.PutUint16(f[12:], 2)
		binary.LittleEndian.PutUint16(f[14:], 16)
		if mod != nil {
			mod(f)
		}
		return ch("fmt ", f)
	}
	data := ch("data", make([]byte, 20))
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"BadFmt", carvetest.WAV(carvetest.WAVOpts{BadFmt: true}), "bad-fmt"},
		{"NoData", carvetest.WAV(carvetest.WAVOpts{NoData: true}), "no-data"},
		{"size +2 with data after", slices.Concat(carvetest.WAV(carvetest.WAVOpts{SizeDelta: 2}), make([]byte, 64)), "bad-length"},
		{"size +2 exact buffer", carvetest.WAV(carvetest.WAVOpts{SizeDelta: 2}), "truncated"},
		{"size -2", carvetest.WAV(carvetest.WAVOpts{SizeDelta: -2}), "bad-length"},
		{"no chunks (S = 4)", carvetest.RIFF("WAVE", 0), "bad-fmt"},
		{"first chunk is data", carvetest.RIFF("WAVE", 0, data, fmtChunk(nil)), "bad-fmt"},
		{"fmt payload under another type", carvetest.RIFF("WAVE", 0, ch("data", fmtChunk(nil)[8:]), data), "bad-fmt"},
		{"fmt of 15 bytes", carvetest.RIFF("WAVE", 0, ch("fmt ", make([]byte, 15)), data), "bad-fmt"},
		{"format 0", carvetest.RIFF("WAVE", 0, fmtChunk(func(f []byte) { f[0] = 0 }), data), "bad-fmt"},
		{"rate 0", carvetest.RIFF("WAVE", 0, fmtChunk(func(f []byte) { clear(f[4:8]) }), data), "bad-fmt"},
		{"block align 0", carvetest.RIFF("WAVE", 0, fmtChunk(func(f []byte) { clear(f[12:14]) }), data), "bad-fmt"},
		{"form WEBP", slices.Concat([]byte("RIFF"), []byte{4, 0, 0, 0}, []byte("WEBP")), "bad-form"},
		{"not RIFF", slices.Concat([]byte("RIFX"), carvetest.WAV(carvetest.WAVOpts{})[4:]), "bad-signature"},
		{"cut short", carvetest.WAV(carvetest.WAVOpts{})[:50], "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validateRIFFWave(riffWindow(c.obj, newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
}

func TestWAVReadErrorIsUnreadable(t *testing.T) {
	obj := carvetest.WAV(carvetest.WAVOpts{ListBefore: true})
	for _, at := range []int64{0, 14, 40} {
		w := newWindow(context.Background(), failAt{obj, at}, 0, int64(len(obj)), newBudget(1<<30))
		if v := validateRIFFWave(w); v.Outcome != Rejected || v.Reason != "unreadable" || !errors.Is(w.readErr, errMedia) {
			t.Fatalf("at %d: verdict = %+v read error = %v", at, v, w.readErr)
		}
	}
}

func TestWAVRealEncoderObjects(t *testing.T) {
	dir := filepath.Join("testdata", "objects")
	raw, err := os.ReadFile(filepath.Join(dir, "objects.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Objects []struct {
			File, Family string
			Size         int64
			Facts        struct{ Nchannels, Framerate int }
		}
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range exp.Objects {
		if o.Family != "wav" {
			continue
		}
		n++
		obj, err := os.ReadFile(filepath.Join(dir, o.File))
		if err != nil {
			t.Fatal(err)
		}
		v := waveVerdict(obj, 4096)
		if v.Outcome != Validated || v.Length != o.Size || int64(len(obj)) != o.Size {
			t.Fatalf("%s: verdict = %+v, want validated length %d", o.File, v, o.Size)
		}
		want := []string{"channels=" + strconv.Itoa(o.Facts.Nchannels), "rate=" + strconv.Itoa(o.Facts.Framerate)}
		if o.Facts.Nchannels == 0 || !slices.Equal(v.Basis, want) {
			t.Errorf("%s: basis = %v, want %v", o.File, v.Basis, want)
		}
	}
	if n < 1 {
		t.Fatal("no wav object in the expectations")
	}
}
