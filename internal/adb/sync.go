package adb

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"time"
)

// SyncMaxChunk is the largest DATA payload adbd accepts.
const SyncMaxChunk = 64 * 1024

// FileInfo is a sync STAT or LIST entry.
type FileInfo struct {
	Name  string      `json:"name"`
	Mode  uint32      `json:"mode"`
	Size  uint32      `json:"size"`
	Mtime int64       `json:"mtime"`
	Type  string      `json:"type"` // "file", "dir", "symlink", "other"
	Perm  string      `json:"perm"`
	FMode fs.FileMode `json:"-"`
}

// Exists reports whether STAT found anything (mode 0 means "no such file").
func (f *FileInfo) Exists() bool { return f.Mode != 0 }

// IsDir reports a directory.
func (f *FileInfo) IsDir() bool { return f.Mode&0o170000 == 0o040000 }

func newFileInfo(name string, mode, size, mtime uint32) FileInfo {
	fi := FileInfo{Name: name, Mode: mode, Size: size, Mtime: int64(mtime)}
	switch mode & 0o170000 {
	case 0o100000:
		fi.Type = "file"
	case 0o040000:
		fi.Type = "dir"
		fi.FMode |= fs.ModeDir
	case 0o120000:
		fi.Type = "symlink"
		fi.FMode |= fs.ModeSymlink
	default:
		fi.Type = "other"
	}
	fi.FMode |= fs.FileMode(mode & 0o777)
	fi.Perm = fi.FMode.String()
	return fi
}

// Sync is an open sync: session. Requests on one session are sequential.
type Sync struct {
	conn *Conn
}

// OpenSync starts a sync: session on the device.
func (c *Client) OpenSync(ctx context.Context, serial string) (*Sync, error) {
	conn, err := c.Transport(ctx, serial, "sync:")
	if err != nil {
		return nil, err
	}
	return &Sync{conn: conn}, nil
}

// Close sends QUIT and closes the session.
func (s *Sync) Close() error {
	_ = s.send("QUIT", nil)
	return s.conn.Close()
}

func (s *Sync) send(id string, data []byte) error {
	b := make([]byte, 8+len(data))
	copy(b, id)
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(data)))
	copy(b[8:], data)
	_, err := s.conn.Write(b)
	return err
}

func (s *Sync) readHeader() (string, uint32, error) {
	var h [8]byte
	if _, err := io.ReadFull(s.conn, h[:]); err != nil {
		return "", 0, err
	}
	return string(h[:4]), binary.LittleEndian.Uint32(h[4:]), nil
}

func (s *Sync) readFail(n uint32, path string) error {
	if n > 64*1024 {
		return fmt.Errorf("adb sync: bad FAIL length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(s.conn, b); err != nil {
		return err
	}
	return &Error{Service: "sync " + path, Msg: string(b)}
}

// Stat returns file metadata; FileInfo.Exists() is false when missing.
func (s *Sync) Stat(path string) (*FileInfo, error) {
	if err := s.send("STAT", []byte(path)); err != nil {
		return nil, err
	}
	var b [16]byte
	if _, err := io.ReadFull(s.conn, b[:]); err != nil {
		return nil, err
	}
	if string(b[:4]) != "STAT" {
		return nil, fmt.Errorf("adb sync: unexpected %q for STAT", b[:4])
	}
	fi := newFileInfo(path, binary.LittleEndian.Uint32(b[4:]), binary.LittleEndian.Uint32(b[8:]), binary.LittleEndian.Uint32(b[12:]))
	return &fi, nil
}

// List returns directory entries (excluding . and ..).
func (s *Sync) List(path string) ([]FileInfo, error) {
	if err := s.send("LIST", []byte(path)); err != nil {
		return nil, err
	}
	var out []FileInfo
	for {
		var b [20]byte
		if _, err := io.ReadFull(s.conn, b[:]); err != nil {
			return nil, err
		}
		id := string(b[:4])
		switch id {
		case "DONE":
			return out, nil
		case "FAIL":
			// FAIL has only a length after the id; we've over-read 12 bytes.
			n := binary.LittleEndian.Uint32(b[4:8])
			rest := make([]byte, 0, n)
			rest = append(rest, b[8:]...)
			if n < 12 {
				return nil, &Error{Service: "sync LIST " + path, Msg: string(rest[:n])}
			}
			more := make([]byte, n-12)
			if _, err := io.ReadFull(s.conn, more); err != nil {
				return nil, err
			}
			return nil, &Error{Service: "sync LIST " + path, Msg: string(append(rest, more...))}
		case "DENT":
		default:
			return nil, fmt.Errorf("adb sync: unexpected %q in LIST", id)
		}
		mode := binary.LittleEndian.Uint32(b[4:])
		size := binary.LittleEndian.Uint32(b[8:])
		mtime := binary.LittleEndian.Uint32(b[12:])
		nl := binary.LittleEndian.Uint32(b[16:])
		if nl > 4096 {
			return nil, fmt.Errorf("adb sync: name too long")
		}
		name := make([]byte, nl)
		if _, err := io.ReadFull(s.conn, name); err != nil {
			return nil, err
		}
		if n := string(name); n != "." && n != ".." {
			out = append(out, newFileInfo(n, mode, size, mtime))
		}
	}
}

// Recv streams the file at path into w.
func (s *Sync) Recv(path string, w io.Writer) (int64, error) {
	if err := s.send("RECV", []byte(path)); err != nil {
		return 0, err
	}
	var total int64
	buf := make([]byte, SyncMaxChunk)
	for {
		id, n, err := s.readHeader()
		if err != nil {
			return total, err
		}
		switch id {
		case "DATA":
			if n > SyncMaxChunk {
				return total, fmt.Errorf("adb sync: chunk too large")
			}
			if _, err := io.ReadFull(s.conn, buf[:n]); err != nil {
				return total, err
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return total, err
			}
			total += int64(n)
		case "DONE":
			return total, nil
		case "FAIL":
			return total, s.readFail(n, path)
		default:
			return total, fmt.Errorf("adb sync: unexpected %q in RECV", id)
		}
	}
}

// Send uploads r to path with the given permission bits (e.g. 0644).
func (s *Sync) Send(path string, mode fs.FileMode, mtime time.Time, r io.Reader) (int64, error) {
	m := uint32(mode.Perm()) | 0o100000
	if err := s.send("SEND", []byte(fmt.Sprintf("%s,%d", path, m))); err != nil {
		return 0, err
	}
	buf := make([]byte, SyncMaxChunk)
	var total int64
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			if err := s.send("DATA", buf[:n]); err != nil {
				return total, s.sendError(err, path)
			}
			total += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return total, rerr
		}
	}
	var done [8]byte
	copy(done[:], "DONE")
	binary.LittleEndian.PutUint32(done[4:], uint32(mtime.Unix()))
	if _, err := s.conn.Write(done[:]); err != nil {
		return total, s.sendError(err, path)
	}
	id, n, err := s.readHeader()
	if err != nil {
		return total, err
	}
	switch id {
	case "OKAY":
		return total, nil
	case "FAIL":
		return total, s.readFail(n, path)
	}
	return total, fmt.Errorf("adb sync: unexpected %q after SEND", id)
}

// sendError turns a write failure into adbd's FAIL message when one is
// waiting (adbd answers FAIL and closes on e.g. permission denied).
func (s *Sync) sendError(werr error, path string) error {
	_ = s.conn.SetReadDeadline(time.Now().Add(time.Second))
	id, n, err := s.readHeader()
	if err == nil && id == "FAIL" {
		return s.readFail(n, path)
	}
	return werr
}

// Stat, List, Pull and Push are one-shot conveniences on Client.

func (c *Client) Stat(ctx context.Context, serial, path string) (*FileInfo, error) {
	s, err := c.OpenSync(ctx, serial)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Stat(path)
}

func (c *Client) List(ctx context.Context, serial, path string) ([]FileInfo, error) {
	s, err := c.OpenSync(ctx, serial)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.List(path)
}

func (c *Client) Pull(ctx context.Context, serial, path string, w io.Writer) (int64, error) {
	s, err := c.OpenSync(ctx, serial)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	return s.Recv(path, w)
}

func (c *Client) Push(ctx context.Context, serial, path string, mode fs.FileMode, r io.Reader) (int64, error) {
	s, err := c.OpenSync(ctx, serial)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	return s.Send(path, mode, time.Now(), r)
}
