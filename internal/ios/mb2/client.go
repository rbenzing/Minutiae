package mb2

import (
	"context"
	"fmt"
	"io"
)

// DeviceError is a non-zero ErrorCode reported by the device.
type DeviceError struct {
	Code        int64
	Description string
}

func (e *DeviceError) Error() string {
	return fmt.Sprintf("mobilebackup2: device error %d: %s", e.Code, e.Description)
}

// BackupOptions configure Backup.
type BackupOptions struct {
	UDID      string
	Dir       string // working directory the device reads and writes
	FreeSpace func() (uint64, error)
	Progress  func(bytesReceived int64)
}

// Result summarizes a backup.
type Result struct {
	BytesReceived int64
	LocalErrors   []string // files the host could not store
	RemoteErrors  []string // per-file errors reported by the device
}

// Client is a mobilebackup2 session on an already-started service connection.
type Client struct {
	rw    io.ReadWriter
	codec Codec
}

// New wraps a mobilebackup2 service connection.
func New(rw io.ReadWriter) *Client { return &Client{rw: rw, codec: NewCodec(rw)} }

func (c *Client) processMessage(m map[string]any) error {
	return c.codec.Send([]any{"DLMessageProcessMessage", m})
}

func checkProcessMessage(msg []any) (map[string]any, error) {
	m, ok := arg(msg, 1).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mobilebackup2: malformed process message %v", msg)
	}
	if code, _ := ToInt(m["ErrorCode"]); code != 0 {
		desc, _ := m["ErrorDescription"].(string)
		return m, &DeviceError{Code: code, Description: desc}
	}
	return m, nil
}

// Handshake performs the DeviceLink version exchange and mobilebackup2 Hello.
func (c *Client) Handshake() error {
	msg, err := c.codec.Recv()
	if err != nil {
		return fmt.Errorf("mobilebackup2 handshake: %w", err)
	}
	if Name(msg) != "DLMessageVersionExchange" || len(msg) < 2 {
		return fmt.Errorf("mobilebackup2 handshake: expected version exchange, got %v", msg)
	}
	major, _ := ToInt(msg[1])
	if err := c.codec.Send([]any{"DLMessageVersionExchange", "DLVersionsOk", major}); err != nil {
		return err
	}
	if msg, err = c.codec.Recv(); err != nil {
		return err
	}
	if Name(msg) != "DLMessageDeviceReady" {
		return fmt.Errorf("mobilebackup2 handshake: expected device ready, got %v", msg)
	}
	if err := c.processMessage(map[string]any{
		"MessageName": "Hello", "SupportedProtocolVersions": []any{2.0, 2.1},
	}); err != nil {
		return err
	}
	if msg, err = c.codec.Recv(); err != nil {
		return err
	}
	_, err = checkProcessMessage(msg)
	return err
}

// Backup requests a full backup and serves the device's requests against
// o.Dir until the device reports completion.
func (c *Client) Backup(ctx context.Context, o BackupOptions) (Result, error) {
	if cl, ok := c.rw.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = cl.Close() })
		defer stop()
	}
	h := &handler{c: c, o: o}
	err := h.run()
	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return h.res, err
}
