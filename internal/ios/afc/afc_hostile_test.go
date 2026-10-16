package afc_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/ios/afc"
	"github.com/rbenzing/minutiae/internal/ios/afc/afctest"
)

const callBound = 5 * time.Second

func TestHostileRepliesAreProtocolErrors(t *testing.T) {
	list := func(c *afc.Client) error { _, err := c.List("/"); return err }
	stat := func(c *afc.Client) error { _, err := c.Stat("/f"); return err }
	open := func(c *afc.Client) error { _, err := c.Open("/f"); return err }
	read := func(c *afc.Client) error {
		f, err := c.Open("/f")
		if err != nil {
			return err
		}
		_, err = f.Read(make([]byte, 16))
		return err
	}
	badMagic := afctest.Frame(2, 1, nil, strs(".", ".."))
	badMagic[0] ^= 0xff
	for _, tc := range []struct {
		name  string
		only  uint64
		reply []byte
		call  func(*afc.Client) error
	}{
		{"bad magic", 0, badMagic, list},
		{"this_length below the header", 0, rawHeader(40, 39, 2), list},
		{"entire_length below this_length", 0, append(rawHeader(40, 48, 2), make([]byte, 8)...), list},
		{"entire_length over the cap", 0, rawHeader(afc.MaxPacket+1, 40, 2), list},
		{"header payload over its cap", 0, rawHeader(40+8192, 40+8192, 2), list},
		{"status packet without a code", 0, afctest.Frame(1, 1, []byte{1, 2, 3}, nil), list},
		{"listing not NUL terminated", 0x03, afctest.Frame(2, 1, nil, []byte("abc")), list},
		{"listing with a slash", 0x03, afctest.Frame(2, 1, nil, strs(".", "..", "a/b")), list},
		{"listing with a parent name", 0x03, afctest.Frame(2, 1, nil, strs("..", "../x")), list},
		{"listing with an empty name", 0x03, afctest.Frame(2, 1, nil, []byte("\x00\x00")), list},
		{"listing empty (not even dots)", 0x03, afctest.Frame(2, 1, nil, nil), list},
		{"listing over the entry cap", 0x03, afctest.Frame(2, 1, nil, bytes.Repeat([]byte("a\x00"), 1<<20+1)), list},
		{"listing with a duplicate name", 0x03, afctest.Frame(2, 1, nil, strs(".", "..", "a", "a")), list},
		{"listing with a duplicate dot", 0x03, afctest.Frame(2, 1, nil, strs(".", ".", "..")), list},
		{"listing answered by a success status", 0x03, afctest.Status(1, 0), list},
		{"stat without a size", 0x0A, afctest.Frame(2, 1, nil, strs("st_ifmt", "S_IFREG")), stat},
		{"stat without a type", 0x0A, afctest.Frame(2, 1, nil, strs("st_size", "4")), stat},
		{"stat odd string count", 0x0A, afctest.Frame(2, 1, nil, strs("st_size")), stat},
		{"stat duplicate key", 0x0A, afctest.Frame(2, 1, nil, strs("st_size", "1", "st_size", "2", "st_ifmt", "S_IFREG")), stat},
		{"stat size over int64", 0x0A, afctest.Frame(2, 1, nil, strs("st_size", "99999999999999999999", "st_ifmt", "S_IFREG")), stat},
		{"stat negative size", 0x0A, afctest.Frame(2, 1, nil, strs("st_size", "-5", "st_ifmt", "S_IFREG")), stat},
		{"stat answered by a success status", 0x0A, afctest.Status(1, 0), stat},
		{"open reply without a handle", 0x0D, afctest.Frame(0x0E, 1, []byte{1, 2, 3}, nil), open},
		{"open answered by a success status", 0x0D, afctest.Status(1, 0), open},
		{"read longer than requested", 0x0F, afctest.Frame(2, 1, nil, make([]byte, 17)), read},
		{"read answered by a success status", 0x0F, afctest.Status(1, 0), read},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := hostile(t, tc.only, tc.reply, false)
			var err error
			if !within(callBound, func() { err = tc.call(c) }) {
				t.Fatal("call hung")
			}
			if !errors.Is(err, afc.ErrProtocol) {
				t.Fatalf("err = %v, want ErrProtocol", err)
			}
		})
	}
}

// W46: a reply carries the request's packet number and the operation that
// answers it. Anything else is ErrProtocol and never reaches file bytes, a
// listing, a size or a handle.
func TestMismatchedRepliesAreProtocolErrors(t *testing.T) {
	type outcome struct {
		leaked bool
		err    error
	}
	calls := map[string]func(*afc.Client) outcome{
		"list": func(c *afc.Client) outcome {
			names, err := c.List("/")
			return outcome{len(names) > 0, err}
		},
		"stat": func(c *afc.Client) outcome {
			st, err := c.Stat("/f")
			return outcome{st != afc.Stat{}, err}
		},
		"open": func(c *afc.Client) outcome {
			f, err := c.Open("/f")
			return outcome{f != nil, err}
		},
		"read": func(c *afc.Client) outcome {
			f, err := c.Open("/f")
			if err != nil {
				return outcome{false, err}
			}
			n, err := f.Read(make([]byte, 16))
			return outcome{n > 0, err}
		},
		"close": func(c *afc.Client) outcome {
			f, err := c.Open("/f")
			if err != nil {
				return outcome{false, err}
			}
			return outcome{false, f.Close()}
		},
	}
	onlyOp := map[string]uint64{"list": 0x03, "stat": 0x0A, "open": 0x0D, "read": 0x0F, "close": 0x14}
	// What a correct reply would look like, with packet number num.
	right := map[string]func(num uint64) []byte{
		"list":  func(n uint64) []byte { return afctest.Frame(0x02, n, nil, strs(".", "..", "a")) },
		"stat":  func(n uint64) []byte { return afctest.Frame(0x02, n, nil, strs("st_size", "4", "st_ifmt", "S_IFREG")) },
		"open":  func(n uint64) []byte { return afctest.Frame(0x0E, n, binary.LittleEndian.AppendUint64(nil, 7), nil) },
		"read":  func(n uint64) []byte { return afctest.Frame(0x02, n, nil, []byte("EVIL")) },
		"close": func(n uint64) []byte { return afctest.Status(n, 0) },
	}
	// The same payload under an operation that does not answer the request.
	wrongOp := map[string][]byte{
		"list":  afctest.Frame(0x0E, 1, nil, strs(".", "..", "a")),
		"stat":  afctest.Frame(0x03, 1, nil, strs("st_size", "4", "st_ifmt", "S_IFREG")),
		"open":  afctest.Frame(0x02, 1, binary.LittleEndian.AppendUint64(nil, 7), nil),
		"read":  afctest.Frame(0x03, 2, nil, []byte("EVIL")),
		"close": afctest.Frame(0x02, 2, nil, nil),
	}
	for op, call := range calls {
		t.Run(op+"/wrong opcode", func(t *testing.T) {
			c, _ := hostileSrv(t, onlyOp[op], wrongOp[op], false, true)
			var got outcome
			if !within(callBound, func() { got = call(c) }) {
				t.Fatal("call hung")
			}
			if !errors.Is(got.err, afc.ErrProtocol) || got.leaked {
				t.Fatalf("outcome = %+v, want ErrProtocol and nothing returned", got)
			}
		})
		nums := []uint64{0, 99}
		if op == "read" || op == "close" {
			nums = append(nums, 1) // the number of the open before it: a stale reply
		}
		for _, num := range nums {
			t.Run(op+"/wrong packet number", func(t *testing.T) {
				c, _ := hostileSrv(t, onlyOp[op], right[op](num), false, false)
				var got outcome
				if !within(callBound, func() { got = call(c) }) {
					t.Fatal("call hung")
				}
				if !errors.Is(got.err, afc.ErrProtocol) || got.leaked {
					t.Fatalf("num %d: outcome = %+v, want ErrProtocol and nothing returned", num, got)
				}
			})
		}
	}
}

// A device status for another packet is not this request's StatusError.
func TestStatusForAnotherPacketIsAProtocolError(t *testing.T) {
	c, _ := hostileSrv(t, 0x03, afctest.Status(5, 8), false, false)
	_, err := c.List("/")
	var se *afc.StatusError
	if !errors.Is(err, afc.ErrProtocol) || errors.As(err, &se) {
		t.Fatalf("err = %v, want ErrProtocol and no StatusError", err)
	}
}

// A reply that is right for the previous request, replayed, is refused.
func TestReplayedReplyIsRefused(t *testing.T) {
	c, _ := newPeer(t, func(int, []byte) ([]byte, bool) {
		return afctest.Frame(0x02, 1, nil, strs(".", "..", "a")), false
	})
	if names, err := c.List("/"); err != nil || len(names) != 1 {
		t.Fatalf("first List = %v, %v", names, err)
	}
	names, err := c.List("/")
	if !errors.Is(err, afc.ErrProtocol) || names != nil {
		t.Fatalf("replayed List = %v, %v; want ErrProtocol", names, err)
	}
}

func TestCutShortRepliesFailWithoutHanging(t *testing.T) {
	for name, reply := range map[string][]byte{
		"header cut":  rawHeader(40, 40, 2)[:20],
		"body cut":    append(rawHeader(40+100, 40, 2), make([]byte, 10)...),
		"nothing":     nil,
		"magic only":  []byte("CFA6LPAA"),
		"huge claim":  rawHeader(afc.MaxPacket, 40, 2),
		"status only": rawHeader(48, 48, 1),
	} {
		c := hostile(t, 0, reply, true)
		var err error
		if !within(callBound, func() { _, err = c.List("/") }) {
			t.Errorf("%s: hung", name)
			continue
		}
		if err == nil {
			t.Errorf("%s: a cut-short reply was accepted", name)
		}
	}
}

// W48: a connection that ends before any reply byte is an unexpected end, not
// the clean io.EOF a caller reads as the end of a file.
func TestReplyCutAtZeroBytesIsNotEOF(t *testing.T) {
	c, _ := newPeer(t, func(n int, _ []byte) ([]byte, bool) {
		if n == 1 {
			return afctest.Frame(0x0E, 1, binary.LittleEndian.AppendUint64(nil, 7), nil), false
		}
		return nil, true
	})
	f, err := c.Open("/f")
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.Read(make([]byte, 8))
	if n != 0 || errors.Is(err, io.EOF) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Read = %d, %v; want 0 and io.ErrUnexpectedEOF, never io.EOF", n, err)
	}
}

// W52: the per-call maximum is applied before the body is read. The device
// declares a larger packet, sends nothing more and keeps the connection open:
// a client that waited for the body would hang.
func TestReplyOverThePerCallMaximumIsRefusedUnread(t *testing.T) {
	for _, tc := range []struct {
		name  string
		only  uint64
		reply []byte
		call  func(*afc.Client) error
	}{
		{"read", 0x0F, rawHeader(40+afc.MaxRead+1, 40, 2), func(c *afc.Client) error {
			f, err := c.Open("/f")
			if err != nil {
				return err
			}
			_, err = f.Read(make([]byte, 16))
			return err
		}},
		{"stat", 0x0A, rawHeader(1<<17, 40, 2), func(c *afc.Client) error { _, err := c.Stat("/f"); return err }},
		{"open", 0x0D, rawHeader(40+5000, 40, 0x0E), func(c *afc.Client) error { _, err := c.Open("/f"); return err }},
		{"list", 0x03, rawHeader(afc.MaxPacket+1, 40, 2), func(c *afc.Client) error { _, err := c.List("/"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := hostile(t, tc.only, tc.reply, false)
			var err error
			if !within(callBound, func() { err = tc.call(c) }) {
				t.Fatal("the client waited for the body of an oversized packet")
			}
			if !errors.Is(err, afc.ErrProtocol) {
				t.Fatalf("err = %v, want ErrProtocol", err)
			}
		})
	}
}

// A data reply of exactly the largest allowed size is accepted (G6 too: a
// bigger buffer still asks for at most MaxRead).
func TestReadAtTheMaximumIsAccepted(t *testing.T) {
	c, p := newPeer(t, func(n int, _ []byte) ([]byte, bool) {
		if n == 1 {
			return afctest.Frame(0x0E, 1, binary.LittleEndian.AppendUint64(nil, 7), nil), false
		}
		return afctest.Frame(0x02, 2, nil, make([]byte, afc.MaxRead)), false
	})
	f, err := c.Open("/f")
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.Read(make([]byte, 3*afc.MaxRead))
	if err != nil || n != afc.MaxRead {
		t.Fatalf("Read = %d, %v", n, err)
	}
	req := p.requests()[1]
	if asked := binary.LittleEndian.Uint64(req[48:]); asked != afc.MaxRead {
		t.Fatalf("the read request asked for %d bytes, want %d", asked, afc.MaxRead)
	}
}

// W47: after a framing, I/O or protocol failure the connection is closed and
// every later call returns that failure; a forged packet that follows is
// never parsed as the next reply.
func TestFramingErrorPoisonsTheClient(t *testing.T) {
	c, p := newPeer(t, func(n int, _ []byte) ([]byte, bool) {
		if n == 1 {
			return rawHeader(afc.MaxPacket+1, 40, 2), false
		}
		return afctest.Frame(0x02, uint64(n), nil, strs(".", "..", "forged")), false
	})
	if _, err := c.List("/"); !errors.Is(err, afc.ErrProtocol) {
		t.Fatalf("first err = %v", err)
	}
	names, err := c.List("/")
	if !errors.Is(err, afc.ErrProtocol) || names != nil {
		t.Fatalf("second List = %v, %v; want the sticky ErrProtocol and no names", names, err)
	}
	if _, err := c.Stat("/f"); !errors.Is(err, afc.ErrProtocol) {
		t.Fatalf("Stat after the failure = %v", err)
	}
	if _, err := c.Open("/f"); !errors.Is(err, afc.ErrProtocol) {
		t.Fatalf("Open after the failure = %v", err)
	}
	if got := len(p.requests()); got != 1 {
		t.Fatalf("%d requests reached the device; the failed client must send nothing more", got)
	}
}

func TestIOErrorPoisonsTheClient(t *testing.T) {
	c, p := newPeer(t, func(int, []byte) ([]byte, bool) { return rawHeader(40, 40, 2)[:20], true })
	_, err1 := c.List("/")
	if !errors.Is(err1, io.ErrUnexpectedEOF) {
		t.Fatalf("first err = %v", err1)
	}
	if _, err := c.List("/"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("second err = %v, want the first failure", err)
	}
	if got := len(p.requests()); got != 1 {
		t.Fatalf("%d requests reached the device", got)
	}
}

func TestProtocolErrorFoundByTheCallerPoisonsTheClient(t *testing.T) {
	c, p := newPeer(t, func(n int, _ []byte) ([]byte, bool) {
		return afctest.Frame(0x02, uint64(n), nil, strs(".", "..", "a/b")), false
	})
	if _, err := c.List("/"); !errors.Is(err, afc.ErrProtocol) {
		t.Fatalf("first err = %v", err)
	}
	if _, err := c.Stat("/f"); !errors.Is(err, afc.ErrProtocol) {
		t.Fatalf("second err = %v", err)
	}
	if got := len(p.requests()); got != 1 {
		t.Fatalf("%d requests reached the device", got)
	}
}

func TestFileOperationsAfterAFailureReturnIt(t *testing.T) {
	c, p := newPeer(t, func(n int, _ []byte) ([]byte, bool) {
		if n == 1 {
			return afctest.Frame(0x0E, 1, binary.LittleEndian.AppendUint64(nil, 7), nil), false
		}
		return afctest.Frame(0x02, 99, nil, []byte("forged")), false // wrong packet number
	})
	f, err := c.Open("/f")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(make([]byte, 8)); !errors.Is(err, afc.ErrProtocol) {
		t.Fatalf("Read err = %v", err)
	}
	if n, err := f.Read(make([]byte, 8)); n != 0 || !errors.Is(err, afc.ErrProtocol) {
		t.Fatalf("second Read = %d, %v", n, err)
	}
	if err := f.Close(); !errors.Is(err, afc.ErrProtocol) {
		t.Fatalf("Close err = %v", err)
	}
	if got := len(p.requests()); got != 2 {
		t.Fatalf("%d requests reached the device, want 2 (open, one read)", got)
	}
}

// A device status is an answer, not a broken connection: the client goes on.
func TestStatusErrorDoesNotPoisonTheClient(t *testing.T) {
	c, _ := newPeer(t, func(n int, _ []byte) ([]byte, bool) {
		if n == 1 {
			return afctest.Status(1, 8), false
		}
		return afctest.Frame(0x02, uint64(n), nil, strs(".", "..", "a")), false
	})
	if _, err := c.List("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("first err = %v", err)
	}
	if names, err := c.List("/"); err != nil || len(names) != 1 {
		t.Fatalf("second List = %v, %v", names, err)
	}
}

// A path the client refuses locally never reached the wire and does not end
// the session.
func TestRefusedPathDoesNotPoisonTheClient(t *testing.T) {
	c, p := newPeer(t, func(n int, _ []byte) ([]byte, bool) {
		return afctest.Frame(0x02, uint64(n), nil, strs(".", "..", "a")), false
	})
	if _, err := c.List("/a\x00b"); err == nil {
		t.Fatal("a path with a NUL byte was accepted")
	}
	if names, err := c.List("/"); err != nil || len(names) != 1 {
		t.Fatalf("List after the refusal = %v, %v", names, err)
	}
	if got := len(p.requests()); got != 1 {
		t.Fatalf("%d requests reached the device", got)
	}
}

// A device that declares the largest packet and sends nothing makes the client
// allocate for what arrives, not for the claim.
func TestDeclaredSizeIsNotAllocated(t *testing.T) {
	c := hostile(t, 0, rawHeader(afc.MaxPacket, 40, 2), true)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := c.List("/")
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("accepted")
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4<<20 {
		t.Fatalf("allocated %d bytes for a packet that never arrived", grew)
	}
}

func TestStatusCodesMapToErrors(t *testing.T) {
	srv := &afctest.Server{Files: map[string][]byte{"/f": []byte("data")}}
	rw := afctest.Pipe(srv)
	watchdog(t, rw)
	c := afc.New(rw)
	defer func() { _ = c.Close() }()
	var se *afc.StatusError
	if _, err := c.List("/missing"); !errors.As(err, &se) || se.Code != 8 || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing dir err = %v", err)
	}
	if errors.Is(se, fs.ErrPermission) {
		t.Fatal("not-found is also permission-denied")
	}
	denied := hostile(t, 0x0D, afctest.Status(1, 10), false)
	_, err := denied.Open("/f")
	if !errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("denied open err = %v", err)
	}
	// G3: a code the client does not know stays a plain StatusError.
	odd := hostile(t, 0x0A, afctest.Status(1, 99), false)
	_, err = odd.Stat("/f")
	if !errors.As(err, &se) || se.Code != 99 || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		t.Fatalf("unknown code err = %v", err)
	}
}

func TestReadEndsOnEmptyDataOrEndOfDataStatus(t *testing.T) {
	for name, reply := range map[string][]byte{
		"empty data":       afctest.Frame(2, 1, nil, nil),
		"end of data (14)": afctest.Status(1, 14),
	} {
		c := hostile(t, 0x0F, reply, false)
		f, err := c.Open("/f")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n, err := f.Read(make([]byte, 8)); n != 0 || err != io.EOF {
			t.Errorf("%s: Read = %d, %v; want 0, EOF", name, n, err)
		}
		// End of data is an answer, not a failure.
		if err := f.Close(); err != nil {
			t.Errorf("%s: Close after EOF = %v", name, err)
		}
	}
}

func TestPathWithNULIsRefusedBeforeSending(t *testing.T) {
	cl, srv := net.Pipe()
	watchdog(t, cl, srv)
	defer func() { _ = srv.Close() }()
	c := afc.New(cl)
	if _, err := c.List("/a\x00b"); err == nil {
		t.Fatal("a path with a NUL byte was sent")
	}
	if _, err := c.Stat("/a\x00b"); err == nil {
		t.Fatal("a path with a NUL byte was sent")
	}
	if _, err := c.Open("/a\x00b"); err == nil {
		t.Fatal("a path with a NUL byte was sent")
	}
}

func TestReadAfterCloseIsAnError(t *testing.T) {
	rw := afctest.Pipe(&afctest.Server{Files: map[string][]byte{"/f": []byte("data")}})
	watchdog(t, rw)
	c := afc.New(rw)
	f, err := c.Open("/f")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(make([]byte, 4)); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("Read after Close err = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

// G9: FILE_CLOSE carries the handle the device returned, for each file, and a
// double close sends nothing.
func TestCloseSendsTheHandleOfThatFile(t *testing.T) {
	srv := &afctest.Server{Files: map[string][]byte{"/a": []byte("AAAA"), "/b": []byte("BBBB")}}
	rw := afctest.Pipe(srv)
	watchdog(t, rw)
	c := afc.New(rw)
	defer func() { _ = c.Close() }()
	a, err := c.Open("/a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Open("/b")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(b); err != nil || string(got) != "BBBB" {
		t.Fatalf("read b = %q, %v", got, err)
	}
	if got, err := io.ReadAll(a); err != nil || string(got) != "AAAA" {
		t.Fatalf("read a = %q, %v", got, err)
	}
	_ = b.Close()
	_ = a.Close()
	_ = a.Close()
	if got := srv.Closed(); len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("closed handles = %v, want [2 1] (b then a, each once)", got)
	}
}

// G4: the packet number counts up with each request on the wire.
func TestPacketNumberIncrements(t *testing.T) {
	c, p := newPeer(t, func(n int, _ []byte) ([]byte, bool) {
		if n == 3 {
			return afctest.Frame(0x02, 3, nil, strs("st_size", "4", "st_ifmt", "S_IFREG")), false
		}
		return afctest.Frame(0x02, uint64(n), nil, strs(".", "..")), false
	})
	_, _ = c.List("/a")
	_, _ = c.List("/b")
	_, _ = c.Stat("/c")
	reqs := p.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	for i, tc := range []struct {
		op   uint64
		path string
	}{{0x03, "/a"}, {0x03, "/b"}, {0x0A, "/c"}} {
		body := append([]byte(tc.path), 0)
		want := append([]byte("CFA6LPAA"), le(uint64(40+len(body)), uint64(40+len(body)), uint64(i+1), tc.op)...)
		want = append(want, body...)
		if !bytes.Equal(reqs[i], want) {
			t.Errorf("request %d = % x, want % x", i+1, reqs[i], want)
		}
	}
}

// G5, W51: the type is exposed as read; only the three known types answer
// IsRegular, IsDir and IsLink, and an unknown one is none of them.
func TestStatTypes(t *testing.T) {
	for _, tc := range []struct {
		ifmt           string
		reg, dir, link bool
	}{
		{"S_IFREG", true, false, false},
		{"S_IFDIR", false, true, false},
		{"S_IFLNK", false, false, true},
		{"S_IFSOCK", false, false, false},
		{"S_IFBLK", false, false, false},
		{"", false, false, false},
		{"s_ifreg", false, false, false},
	} {
		c, _ := hostileSrv(t, 0x0A, afctest.Frame(2, 1, nil, strs("st_size", "4", "st_ifmt", tc.ifmt)), false, true)
		st, err := c.Stat("/f")
		if err != nil {
			t.Fatalf("%q: %v", tc.ifmt, err)
		}
		if st.Type != tc.ifmt || st.IsRegular() != tc.reg || st.IsDir != tc.dir || st.IsLink != tc.link {
			t.Errorf("%q: %+v regular=%v; want regular=%v dir=%v link=%v", tc.ifmt, st, st.IsRegular(), tc.reg, tc.dir, tc.link)
		}
	}
}

// W50: names are device evidence and come back as sent, whatever they hold.
func TestListReturnsNamesRaw(t *testing.T) {
	c, _ := hostileSrv(t, 0x03, afctest.Frame(2, 1, nil, strs(".", "..", `a\b`, "c:d", "e\x01f", " g ", "CON")), false, true)
	names, err := c.List("/")
	want := []string{`a\b`, "c:d", "e\x01f", " g ", "CON"}
	if err != nil || len(names) != len(want) {
		t.Fatalf("List = %q, %v", names, err)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("name %d = %q, want %q", i, names[i], want[i])
		}
	}
}

// G10: whatever the device answers, including hostile and error replies, the
// client sends only read-dir, file-info, open, read and close.
func TestOnlyReadOperationsAcrossErrorPaths(t *testing.T) {
	allowed := map[uint64]bool{0x03: true, 0x0A: true, 0x0D: true, 0x0F: true, 0x14: true}
	replies := map[string][]byte{
		"not found": afctest.Status(1, 8),
		"denied":    afctest.Status(1, 10),
		"unknown":   afctest.Status(1, 99),
		"garbage":   rawHeader(40, 39, 2),
		"cut":       rawHeader(80, 40, 2),
		"wrong op":  afctest.Frame(0x0E, 1, nil, strs("x")),
	}
	for name, reply := range replies {
		c, srv := hostileSrv(t, 0, reply, name == "cut", true)
		_, _ = c.List("/")
		_, _ = c.Stat("/f")
		if f, err := c.Open("/f"); err == nil {
			_, _ = f.Read(make([]byte, 4))
			_ = f.Close()
		}
		_ = c.Close()
		if len(srv.Ops()) == 0 {
			t.Errorf("%s: the device saw nothing", name)
		}
		for _, op := range srv.Ops() {
			if !allowed[op] {
				t.Errorf("%s: the client sent operation 0x%02x, which is not a read operation", name, op)
			}
		}
	}
	// A normal session that meets errors on every path.
	srv := &afctest.Server{Files: map[string][]byte{"/f": []byte("data")}}
	rw := afctest.Pipe(srv)
	watchdog(t, rw)
	c := afc.New(rw)
	_, _ = c.List("/nope")
	_, _ = c.Stat("/nope")
	_, _ = c.Open("/nope")
	f, err := c.Open("/f")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	_, _ = f.Read(make([]byte, 4))
	_ = c.Close()
	for _, op := range srv.Ops() {
		if !allowed[op] {
			t.Errorf("error session: operation 0x%02x is not a read operation", op)
		}
	}
}

// The watchdog closes the connection and names the failure.
func TestWatchdogClosesAndNamesTheProbe(t *testing.T) {
	old := watchdogAfter
	watchdogAfter = 20 * time.Millisecond
	defer func() { watchdogAfter = old }()
	cl, srv := net.Pipe()
	defer func() { _ = srv.Close() }()
	rec := &recorder{}
	watchdog(rec, cl)
	done := make(chan error, 1)
	go func() { _, err := cl.Read(make([]byte, 1)); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read returned no error after the watchdog closed the connection")
		}
	case <-time.After(callBound):
		t.Fatal("the watchdog did not unblock the read")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.msgs) != 1 || rec.msgs[0] != "probe blocked: fake and go-ios disagree on the wire" {
		t.Fatalf("watchdog messages = %q", rec.msgs)
	}
}

// FuzzClientReplies feeds arbitrary bytes as the device's reply to each
// operation: the client may fail but must not panic, hang or allocate wildly.
func FuzzClientReplies(f *testing.F) {
	f.Add(byte(0), afctest.Frame(2, 1, nil, strs(".", "..", "a")))
	f.Add(byte(1), afctest.Frame(2, 1, nil, strs("st_size", "4", "st_ifmt", "S_IFREG")))
	f.Add(byte(2), afctest.Frame(0x0E, 1, binary.LittleEndian.AppendUint64(nil, 7), nil))
	f.Add(byte(3), afctest.Status(1, 14))
	f.Add(byte(0), rawHeader(afc.MaxPacket+1, 40, 2))
	f.Add(byte(1), []byte{})
	f.Fuzz(func(t *testing.T, which byte, reply []byte) {
		c := hostile(t, 0, reply, true)
		if !within(callBound, func() {
			switch which % 4 {
			case 0:
				_, _ = c.List("/")
			case 1:
				_, _ = c.Stat("/f")
			case 2:
				if f, err := c.Open("/f"); err == nil {
					_ = f.Close()
				}
			default:
				if f, err := c.Open("/f"); err == nil {
					_, _ = f.Read(make([]byte, 32))
				}
			}
		}) {
			t.Fatal("client hung")
		}
	})
}

// A reply that runs on past one packet leaves bytes unread in the device's
// socket; the client's next request must not block on them (the fake buffers its
// replies the way a socket does). Found by FuzzClientReplies.
func TestUnreadTrailingBytesDoNotBlockTheNextRequest(t *testing.T) {
	reply := append(afctest.Frame(0x0E, 1, binary.LittleEndian.AppendUint64(nil, 7), nil), bytes.Repeat([]byte{0xAB}, 40)...)
	c := hostile(t, 0, reply, true)
	var err error
	if !within(callBound, func() {
		f, e := c.Open("/f")
		if e != nil {
			err = e
			return
		}
		_, err = f.Read(make([]byte, 8))
	}) {
		t.Fatal("the client blocked writing its second request")
	}
	if err == nil {
		t.Fatal("a read after a closed device succeeded")
	}
}
