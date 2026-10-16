// Package adbtest is an in-process fake ADB server implementing the subset of
// the host and sync protocols Minutiae uses. Test-only infrastructure.
package adbtest

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// File is a file on a fake device.
type File struct {
	Data       []byte
	Mode       uint32 // full st_mode; 0 means regular file 0644
	MTime      time.Time
	ListedSize uint64 // size LIST/STAT report when non-zero (data may be smaller)
}

// Device is a fake device.
type Device struct {
	Serial     string
	State      string                      // "device", "unauthorized", "offline"
	Props      string                      // devices-l tail, e.g. "product:p model:Pixel_7 device:panther"
	Commands   map[string][]byte           // exec/shell command -> stdout
	Files      map[string]File             // absolute path -> file; parent dirs implied
	Unreadable map[string]bool             // RECV fails with permission denied
	ExtraDents map[string][]Dent           // raw entries appended to LIST of a directory (hostile listings)
	NoExec     bool                        // exec: is rejected with FAIL "closed" (old adbd); shell: still works
	AfterChunk func(path string, sent int) // called after each RECV DATA chunk is written (tests use it to stall a transfer)
	Features   []string                    // advertised by host-serial:<serial>:features; nil = DefaultFeatures, empty non-nil = a legacy device
}

// Dent is a raw LIST entry, sent as-is (no validation of the name).
type Dent struct {
	Name  string
	Mode  uint32
	Size  uint64
	MTime int64
}

// Server is a fake ADB server on 127.0.0.1.
type Server struct {
	ln      net.Listener
	devices []*Device
	mu      sync.Mutex
	pushed  map[string]File
	done    chan struct{}
}

// Start listens on a random local port.
func Start(devs ...*Device) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, devices: devs, pushed: map[string]File{}, done: make(chan struct{})}
	go s.serve()
	return s, nil
}

// Addr is host:port of the server.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops accepting connections.
func (s *Server) Close() error {
	err := s.ln.Close()
	<-s.done
	return err
}

// Pushed returns a file received via SEND.
func (s *Server) Pushed(serial, path string) (File, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.pushed[serial+":"+path]
	return f, ok
}

func (s *Server) serve() {
	defer close(s.done)
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *Server) find(serial string) *Device {
	for _, d := range s.devices {
		if d.Serial == serial {
			return d
		}
	}
	return nil
}

func readRequest(r io.Reader) (string, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(l[:]), 16, 16)
	if err != nil {
		return "", err
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r, b)
	return string(b), err
}

func okay(w io.Writer)                { _, _ = w.Write([]byte("OKAY")) }
func lenString(w io.Writer, s string) { _, _ = fmt.Fprintf(w, "%04x%s", len(s), s) }
func fail(w io.Writer, msg string)    { _, _ = fmt.Fprintf(w, "FAIL%04x%s", len(msg), msg) }

func (s *Server) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	var dev *Device
	for {
		req, err := readRequest(c)
		if err != nil {
			return
		}
		switch {
		case req == "host:version":
			okay(c)
			lenString(c, fmt.Sprintf("%04x", 41))
			return
		case req == "host:devices-l":
			okay(c)
			var b strings.Builder
			for _, d := range s.devices {
				fmt.Fprintf(&b, "%-22s %s %s\n", d.Serial, d.State, d.Props)
			}
			lenString(c, b.String())
			return
		case strings.HasPrefix(req, "host-serial:") && strings.HasSuffix(req, ":features"):
			serial := strings.TrimSuffix(strings.TrimPrefix(req, "host-serial:"), ":features")
			d := s.find(serial)
			if d == nil {
				fail(c, "device '"+serial+"' not found")
				return
			}
			okay(c)
			lenString(c, strings.Join(d.advertised(), ","))
			return
		case strings.HasPrefix(req, "host:transport:"):
			serial := strings.TrimPrefix(req, "host:transport:")
			dev = s.find(serial)
			switch {
			case dev == nil:
				fail(c, "device '"+serial+"' not found")
				return
			case dev.State == "unauthorized":
				fail(c, "device unauthorized.\nPlease check the confirmation dialog on your device.")
				return
			case dev.State != "device":
				fail(c, "device offline")
				return
			}
			okay(c)
		case dev != nil && dev.NoExec && strings.HasPrefix(req, "exec:"):
			fail(c, "closed")
			return
		case dev != nil && (strings.HasPrefix(req, "exec:") || strings.HasPrefix(req, "shell:")):
			cmd := req[strings.IndexByte(req, ':')+1:]
			okay(c)
			if out, ok := dev.Commands[cmd]; ok {
				_, _ = c.Write(out)
			} else {
				name := cmd
				if f := strings.Fields(cmd); len(f) > 0 {
					name = f[0]
				}
				_, _ = fmt.Fprintf(c, "/system/bin/sh: %s: inaccessible or not found\n", name)
			}
			return
		case dev != nil && req == "sync:":
			okay(c)
			s.serveSync(c, dev)
			return
		default:
			fail(c, "unknown service "+req)
			return
		}
	}
}

func writeSync(w io.Writer, id string, vals ...uint32) {
	b := []byte(id)
	for _, v := range vals {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	_, _ = w.Write(b)
}

func syncFail(w io.Writer, msg string) {
	writeSync(w, "FAIL", uint32(len(msg)))
	_, _ = w.Write([]byte(msg))
}

func readSyncHeader(r io.Reader) (string, uint32, error) {
	var h [8]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return "", 0, err
	}
	return string(h[:4]), binary.LittleEndian.Uint32(h[4:]), nil
}

const (
	modeDir  = 0o040755
	modeFile = 0o100644
)

func fileMode(f File) uint32 {
	if f.Mode != 0 {
		return f.Mode
	}
	return modeFile
}

type dent struct {
	name  string
	mode  uint32
	size  uint64
	mtime int64
}

func listDir(d *Device, dir string) []dent {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	children := map[string]dent{}
	for p, f := range d.Files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			children[rest[:i]] = dent{name: rest[:i], mode: modeDir}
		} else {
			children[rest] = dent{name: rest, mode: fileMode(f), size: sizeOf(f), mtime: f.MTime.Unix()}
		}
	}
	if len(children) == 0 {
		return nil
	}
	names := make([]string, 0, len(children))
	for n := range children {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []dent{{name: ".", mode: modeDir}, {name: "..", mode: modeDir}}
	for _, n := range names {
		out = append(out, children[n])
	}
	return out
}

func (s *Server) serveSync(c net.Conn, d *Device) {
	for {
		id, n, err := readSyncHeader(c)
		if err != nil {
			return
		}
		arg := make([]byte, n)
		if _, err := io.ReadFull(c, arg); err != nil {
			return
		}
		p := string(arg)
		switch id {
		case "LIST":
			for _, e := range listDir(d, p) {
				writeSync(c, "DENT", e.mode, uint32(e.size), uint32(e.mtime), uint32(len(e.name)))
				_, _ = c.Write([]byte(e.name))
			}
			for _, e := range d.ExtraDents[p] {
				writeSync(c, "DENT", e.Mode, uint32(e.Size), uint32(e.MTime), uint32(len(e.Name)))
				_, _ = c.Write([]byte(e.Name))
			}
			writeSync(c, "DONE", 0, 0, 0, 0)
		case "LIS2":
			if !d.has("ls_v2") {
				syncFail(c, "unknown sync id "+id)
				return
			}
			for _, e := range listDir(d, p) {
				_, _ = c.Write(EncodeDNT2(0, e.mode, e.size, e.mtime, e.name))
			}
			for _, e := range d.ExtraDents[p] {
				_, _ = c.Write(EncodeDNT2(0, e.Mode, e.Size, e.MTime, e.Name))
			}
			_, _ = c.Write(append([]byte("DONE"), make([]byte, dnt2Rest)...))
		case "STAT":
			if f, ok := d.Files[p]; ok {
				writeSync(c, "STAT", fileMode(f), uint32(sizeOf(f)), uint32(f.MTime.Unix()))
			} else if len(listDir(d, p)) > 0 {
				writeSync(c, "STAT", modeDir, 0, 0)
			} else {
				writeSync(c, "STAT", 0, 0, 0)
			}
		case "STA2":
			if !d.has("stat_v2") {
				syncFail(c, "unknown sync id "+id)
				return
			}
			switch f, ok := d.Files[p]; {
			case ok:
				_, _ = c.Write(encodeSTA2(0, fileMode(f), sizeOf(f), f.MTime.Unix()))
			case len(listDir(d, p)) > 0:
				_, _ = c.Write(encodeSTA2(0, modeDir, 0, 0))
			default:
				_, _ = c.Write(encodeSTA2(2, 0, 0, 0)) // ENOENT
			}
		case "RECV":
			if d.Unreadable[p] {
				syncFail(c, "open failed: Permission denied")
				return // adbd ends the sync session after a failed RECV
			}
			f, ok := d.Files[p]
			if !ok {
				syncFail(c, "open failed: No such file or directory")
				return
			}
			for off := 0; off < len(f.Data); off += 65536 {
				end := min(off+65536, len(f.Data))
				writeSync(c, "DATA", uint32(end-off))
				_, _ = c.Write(f.Data[off:end])
				if d.AfterChunk != nil {
					d.AfterChunk(p, end)
				}
			}
			writeSync(c, "DONE", 0)
		case "SEND":
			i := strings.LastIndexByte(p, ',')
			if i < 0 {
				syncFail(c, "bad SEND spec")
				return
			}
			mode, _ := strconv.ParseUint(p[i+1:], 10, 32)
			var data []byte
			for {
				cid, cn, err := readSyncHeader(c)
				if err != nil {
					return
				}
				if cid == "DONE" {
					s.mu.Lock()
					s.pushed[d.Serial+":"+p[:i]] = File{Data: data, Mode: uint32(mode), MTime: time.Unix(int64(cn), 0)}
					s.mu.Unlock()
					writeSync(c, "OKAY", 0)
					break
				}
				if cid != "DATA" {
					return
				}
				chunk := make([]byte, cn)
				if _, err := io.ReadFull(c, chunk); err != nil {
					return
				}
				data = append(data, chunk...)
			}
		case "QUIT":
			return
		default:
			syncFail(c, "unknown sync id "+id)
			return
		}
	}
}

// DefaultFeatures is what a modern device advertises when Device.Features is nil.
var DefaultFeatures = []string{"shell_v2", "cmd", "stat_v2", "ls_v2"}

func sizeOf(f File) uint64 {
	if f.ListedSize != 0 {
		return f.ListedSize
	}
	return uint64(len(f.Data))
}

func (d *Device) advertised() []string {
	if d.Features == nil {
		return DefaultFeatures
	}
	return d.Features
}

func (d *Device) has(feature string) bool {
	return slices.Contains(d.advertised(), feature)
}

const dnt2Rest = 72 // bytes after the id of a DNT2 or DONE frame

func appendV2Fields(b []byte, errno, mode uint32, size uint64, mtime int64) []byte {
	b = binary.LittleEndian.AppendUint32(b, errno)
	b = binary.LittleEndian.AppendUint64(b, 0) // dev
	b = binary.LittleEndian.AppendUint64(b, 0) // ino
	b = binary.LittleEndian.AppendUint32(b, mode)
	b = binary.LittleEndian.AppendUint32(b, 1) // nlink
	b = binary.LittleEndian.AppendUint32(b, 0) // uid
	b = binary.LittleEndian.AppendUint32(b, 0) // gid
	b = binary.LittleEndian.AppendUint64(b, size)
	b = binary.LittleEndian.AppendUint64(b, uint64(mtime))    // atime
	b = binary.LittleEndian.AppendUint64(b, uint64(mtime))    // mtime
	return binary.LittleEndian.AppendUint64(b, uint64(mtime)) // ctime
}

// EncodeDNT2 is one sync v2 directory entry frame.
func EncodeDNT2(errno, mode uint32, size uint64, mtime int64, name string) []byte {
	b := appendV2Fields([]byte("DNT2"), errno, mode, size, mtime)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(name)))
	return append(b, name...)
}

func encodeSTA2(errno, mode uint32, size uint64, mtime int64) []byte {
	return appendV2Fields([]byte("STA2"), errno, mode, size, mtime)
}
