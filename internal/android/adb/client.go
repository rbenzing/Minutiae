// Package adb is a minimal pure-Go client for the ADB server's host and sync
// protocols. It imports nothing from Minutiae so it can be reused and tested
// in isolation.
package adb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

// ErrServerUnavailable means nothing is listening on the ADB server port.
var ErrServerUnavailable = errors.New("adb server not reachable (install Android platform-tools and run `adb start-server`)")

// FailError is a FAIL response from the server or device.
type FailError struct{ Msg string }

func (e *FailError) Error() string { return "adb: " + e.Msg }

// DeviceEntry is one line of host:devices-l.
type DeviceEntry struct {
	Serial string
	State  string
	Props  map[string]string
}

// Client talks to one ADB server.
type Client struct {
	Addr   string
	dialer net.Dialer
}

// DefaultAddr honours ANDROID_ADB_SERVER_PORT like the adb tool does.
func DefaultAddr() string {
	port := os.Getenv("ANDROID_ADB_SERVER_PORT")
	if port == "" {
		port = "5037"
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// New returns a client for addr (DefaultAddr when empty).
func New(addr string) *Client {
	if addr == "" {
		addr = DefaultAddr()
	}
	return &Client{Addr: addr}
}

// conn ties a connection to a context: cancellation closes it and turns the
// resulting I/O error into ctx.Err().
type conn struct {
	net.Conn
	ctx  context.Context
	stop func() bool
}

func (c *conn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil && c.ctx.Err() != nil {
		return n, c.ctx.Err()
	}
	return n, err
}

func (c *conn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil && c.ctx.Err() != nil {
		return n, c.ctx.Err()
	}
	return n, err
}

func (c *conn) Close() error {
	c.stop()
	return c.Conn.Close()
}

func (c *Client) dial(ctx context.Context) (*conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nc, err := c.dialer.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrServerUnavailable, err)
	}
	return &conn{Conn: nc, ctx: ctx, stop: context.AfterFunc(ctx, func() { _ = nc.Close() })}, nil
}

func sendRequest(w io.Writer, payload string) error {
	if len(payload) > 0xffff {
		return fmt.Errorf("adb request too long (%d bytes)", len(payload))
	}
	_, err := fmt.Fprintf(w, "%04x%s", len(payload), payload)
	return err
}

func readLenString(r io.Reader) (string, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(l[:]), 16, 16)
	if err != nil {
		return "", fmt.Errorf("adb: bad length %q", l[:])
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r, b)
	return string(b), err
}

func readStatus(r io.Reader) error {
	var st [4]byte
	if _, err := io.ReadFull(r, st[:]); err != nil {
		return err
	}
	switch string(st[:]) {
	case "OKAY":
		return nil
	case "FAIL":
		msg, err := readLenString(r)
		if err != nil {
			return err
		}
		return &FailError{Msg: msg}
	default:
		return fmt.Errorf("adb: unexpected status %q", st[:])
	}
}

func (c *Client) hostRequest(ctx context.Context, payload string) (*conn, error) {
	cn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	if err := sendRequest(cn, payload); err != nil {
		_ = cn.Close()
		return nil, err
	}
	if err := readStatus(cn); err != nil {
		_ = cn.Close()
		return nil, err
	}
	return cn, nil
}

// Version returns the ADB server protocol version.
func (c *Client) Version(ctx context.Context) (int, error) {
	cn, err := c.hostRequest(ctx, "host:version")
	if err != nil {
		return 0, err
	}
	defer func() { _ = cn.Close() }()
	s, err := readLenString(cn)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(s, 16, 32)
	return int(v), err
}

// Devices lists devices known to the server, in any state.
func (c *Client) Devices(ctx context.Context) ([]DeviceEntry, error) {
	cn, err := c.hostRequest(ctx, "host:devices-l")
	if err != nil {
		return nil, err
	}
	defer func() { _ = cn.Close() }()
	body, err := readLenString(cn)
	if err != nil {
		return nil, err
	}
	return parseDevices(body), nil
}

func parseDevices(body string) []DeviceEntry {
	var out []DeviceEntry
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		e := DeviceEntry{Serial: f[0], State: f[1], Props: map[string]string{}}
		if f[1] == "no" && len(f) > 2 && f[2] == "permissions" {
			e.State = "no permissions"
		}
		for _, kv := range f[2:] {
			if k, v, ok := strings.Cut(kv, ":"); ok && k != "" && !strings.ContainsAny(k, "()[];") {
				e.Props[k] = v
			}
		}
		out = append(out, e)
	}
	return out
}

func (c *Client) openService(ctx context.Context, serial, service string) (*conn, error) {
	cn, err := c.hostRequest(ctx, "host:transport:"+serial)
	if err != nil {
		return nil, err
	}
	if err := sendRequest(cn, service); err != nil {
		_ = cn.Close()
		return nil, err
	}
	if err := readStatus(cn); err != nil {
		_ = cn.Close()
		return nil, err
	}
	return cn, nil
}

// Exec runs cmd on the device via exec: (raw, binary-safe stdout).
func (c *Client) Exec(ctx context.Context, serial, cmd string) (io.ReadCloser, error) {
	return c.openService(ctx, serial, "exec:"+cmd)
}

// Output runs cmd and returns all of its output, falling back to shell: when
// the device rejects exec:.
func (c *Client) Output(ctx context.Context, serial, cmd string) ([]byte, error) {
	rc, err := c.Exec(ctx, serial, cmd)
	var fe *FailError
	if errors.As(err, &fe) && strings.Contains(fe.Msg, "unknown service") {
		rc, err = c.openService(ctx, serial, "shell:"+cmd)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}
