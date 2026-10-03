// Package mb2test is a scripted fake iOS device for mobilebackup2 tests.
package mb2test

import (
	"bytes"
	"fmt"
	"io"
	"net"

	"github.com/rbenzing/minutiae/internal/ios/mb2"
)

// Device is the device end of an in-memory connection.
type Device struct {
	conn  net.Conn
	codec mb2.Codec
}

// Pipe returns the host end and a scripted device end.
func Pipe() (io.ReadWriteCloser, *Device) {
	host, dev := net.Pipe()
	return host, &Device{conn: dev, codec: mb2.NewCodec(dev)}
}

// Close closes the device end.
func (d *Device) Close() error { return d.conn.Close() }

// Send sends one DeviceLink message.
func (d *Device) Send(msg ...any) error { return d.codec.Send(msg) }

func (d *Device) expect(name string) ([]any, error) {
	msg, err := d.codec.Recv()
	if err != nil {
		return nil, err
	}
	if mb2.Name(msg) != name {
		return nil, fmt.Errorf("expected %s, got %v", name, msg)
	}
	return msg, nil
}

// Handshake plays the device side of version exchange and Hello.
func (d *Device) Handshake() error {
	if err := d.Send("DLMessageVersionExchange", 300, 0); err != nil {
		return err
	}
	msg, err := d.expect("DLMessageVersionExchange")
	if err != nil {
		return err
	}
	if len(msg) < 2 || msg[1] != "DLVersionsOk" {
		return fmt.Errorf("version reply = %v", msg)
	}
	if err := d.Send("DLMessageDeviceReady"); err != nil {
		return err
	}
	msg, err = d.expect("DLMessageProcessMessage")
	if err != nil {
		return err
	}
	if m, _ := msg[1].(map[string]any); m["MessageName"] != "Hello" {
		return fmt.Errorf("hello = %v", msg)
	}
	return d.Send("DLMessageProcessMessage", map[string]any{"ErrorCode": 0, "MessageName": "Response", "ProtocolVersion": 2.1})
}

// ExpectBackupRequest reads the host's Backup request.
func (d *Device) ExpectBackupRequest() (map[string]any, error) {
	msg, err := d.expect("DLMessageProcessMessage")
	if err != nil {
		return nil, err
	}
	m, _ := msg[1].(map[string]any)
	if m["MessageName"] != "Backup" {
		return nil, fmt.Errorf("request = %v", m)
	}
	return m, nil
}

func (d *Device) status() (int64, any, error) {
	msg, err := d.expect("DLMessageStatusResponse")
	if err != nil {
		return 0, nil, err
	}
	if len(msg) < 4 {
		return 0, nil, fmt.Errorf("short status %v", msg)
	}
	code, _ := mb2.ToInt(msg[1])
	return code, msg[3], nil
}

// Request sends msg and returns the host's status response.
func (d *Device) Request(msg ...any) (int64, any, error) {
	if err := d.Send(msg...); err != nil {
		return 0, nil, err
	}
	return d.status()
}

// Upload is one file the device sends to the host.
type Upload struct {
	DeviceName string
	Name       string
	Data       []byte
	EndRemote  bool // end with a 0x0b remote marker instead of 0x00
}

// UploadFiles sends files and returns the host's status code.
func (d *Device) UploadFiles(files ...Upload) (int64, error) {
	if err := d.Send("DLMessageUploadFiles", map[string]any{}, 0.0); err != nil {
		return 0, err
	}
	for _, f := range files {
		if err := mb2.WriteName(d.conn, f.DeviceName); err != nil {
			return 0, err
		}
		if err := mb2.WriteName(d.conn, f.Name); err != nil {
			return 0, err
		}
		for off := 0; off < len(f.Data); off += 64 << 10 {
			end := min(off+64<<10, len(f.Data))
			if err := mb2.WriteBlock(d.conn, mb2.CodeFileData, f.Data[off:end]); err != nil {
				return 0, err
			}
		}
		var err error
		if f.EndRemote {
			err = mb2.WriteBlock(d.conn, mb2.CodeErrorRemote, []byte("end"))
		} else {
			err = mb2.WriteBlock(d.conn, mb2.CodeSuccess, nil)
		}
		if err != nil {
			return 0, err
		}
	}
	if err := mb2.WriteU32(d.conn, 0); err != nil {
		return 0, err
	}
	code, _, err := d.status()
	return code, err
}

// DownloadFiles asks the host for files; missing ones are absent from the map.
func (d *Device) DownloadFiles(paths ...string) (map[string][]byte, int64, error) {
	list := make([]any, len(paths))
	for i, p := range paths {
		list[i] = p
	}
	if err := d.Send("DLMessageDownloadFiles", list, 0.0); err != nil {
		return nil, 0, err
	}
	got := map[string][]byte{}
	for {
		name, err := mb2.ReadName(d.conn)
		if err != nil {
			return nil, 0, err
		}
		if name == "" {
			break
		}
		var buf bytes.Buffer
	blocks:
		for {
			n, err := mb2.ReadU32(d.conn)
			if err != nil {
				return nil, 0, err
			}
			code, err := mb2.ReadByte(d.conn)
			if err != nil {
				return nil, 0, err
			}
			payload := make([]byte, n-1)
			if _, err := io.ReadFull(d.conn, payload); err != nil {
				return nil, 0, err
			}
			switch code {
			case mb2.CodeFileData:
				buf.Write(payload)
			case mb2.CodeSuccess:
				got[name] = buf.Bytes()
				break blocks
			default:
				break blocks
			}
		}
	}
	code, _, err := d.status()
	return got, code, err
}

// Finish ends the backup with errorCode and expects the host to disconnect.
func (d *Device) Finish(errorCode int) error {
	if err := d.Send("DLMessageProcessMessage", map[string]any{"ErrorCode": errorCode, "ErrorDescription": "fake"}); err != nil {
		return err
	}
	_, err := d.expect("DLMessageDisconnect")
	return err
}
