package adb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Shell protocol v2 packet ids.
const (
	ShellStdin      = 0
	ShellStdout     = 1
	ShellStderr     = 2
	ShellExit       = 3
	ShellCloseStdin = 4
	ShellWindowSize = 5
)

// ShellOptions configures OpenShell.
type ShellOptions struct {
	Command string // empty = interactive login shell
	PTY     bool
	Term    string // TERM for PTY sessions
	Rows    int
	Cols    int
	// Legacy forces the old shell: protocol even when shell_v2 is available.
	Legacy bool
}

// Shell is an open shell session. For v2 sessions stdout, stderr and the exit
// status arrive as separate packets; legacy sessions only produce stdout and
// end with exit code -1.
type Shell struct {
	conn *Conn
	V2   bool
	wmu  sync.Mutex
	done bool
}

// EncodeShellPacket frames one v2 packet: id byte + LE u32 length + data.
func EncodeShellPacket(id byte, data []byte) []byte {
	b := make([]byte, 5+len(data))
	b[0] = id
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(data)))
	copy(b[5:], data)
	return b
}

// ShellService builds the adbd service string.
func ShellService(o ShellOptions, v2 bool) string {
	if !v2 {
		return "shell:" + o.Command
	}
	var sb strings.Builder
	sb.WriteString("shell,v2")
	if o.PTY {
		sb.WriteString(",pty")
		if o.Term != "" {
			sb.WriteString(",TERM=" + o.Term)
		}
	} else {
		sb.WriteString(",raw")
	}
	sb.WriteString(":" + o.Command)
	return sb.String()
}

type featureCache struct {
	mu sync.Mutex
	m  map[string]featureEntry
}

type featureEntry struct {
	f  map[string]bool
	at time.Time
}

var features featureCache

// CachedFeatures returns the device features, cached for a few seconds.
func (c *Client) CachedFeatures(ctx context.Context, serial string) (map[string]bool, error) {
	key := c.Addr + "/" + serial
	features.mu.Lock()
	if e, ok := features.m[key]; ok && time.Since(e.at) < 10*time.Second {
		features.mu.Unlock()
		return e.f, nil
	}
	features.mu.Unlock()
	f, err := c.Features(ctx, serial)
	if err != nil {
		return nil, err
	}
	features.mu.Lock()
	if features.m == nil {
		features.m = map[string]featureEntry{}
	}
	features.m[key] = featureEntry{f, time.Now()}
	features.mu.Unlock()
	return f, nil
}

// OpenShell starts a shell on the device.
func (c *Client) OpenShell(ctx context.Context, serial string, o ShellOptions) (*Shell, error) {
	v2 := false
	if !o.Legacy {
		f, err := c.CachedFeatures(ctx, serial)
		if err != nil {
			return nil, err
		}
		v2 = f["shell_v2"]
	}
	conn, err := c.Transport(ctx, serial, ShellService(o, v2))
	if err != nil {
		return nil, err
	}
	s := &Shell{conn: conn, V2: v2}
	if v2 && o.PTY && o.Rows > 0 && o.Cols > 0 {
		if err := s.Resize(o.Rows, o.Cols); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return s, nil
}

// Write sends stdin data.
func (s *Shell) Write(p []byte) (int, error) {
	if !s.V2 {
		s.wmu.Lock()
		defer s.wmu.Unlock()
		return s.conn.Write(p)
	}
	if err := s.writePacket(ShellStdin, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *Shell) writePacket(id byte, data []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.conn.Write(EncodeShellPacket(id, data))
	return err
}

// CloseStdin signals EOF on stdin (v2 only; legacy half-closes the socket).
func (s *Shell) CloseStdin() error {
	if !s.V2 {
		if tc, ok := s.conn.Conn.(interface{ CloseWrite() error }); ok {
			return tc.CloseWrite()
		}
		return nil
	}
	return s.writePacket(ShellCloseStdin, nil)
}

// Resize changes the PTY window size (v2 only; ignored for legacy).
func (s *Shell) Resize(rows, cols int) error {
	if !s.V2 {
		return nil
	}
	return s.writePacket(ShellWindowSize, []byte(fmt.Sprintf("%dx%d,0x0", rows, cols)))
}

// ErrNoV2 is returned by operations that need shell v2.
var ErrNoV2 = errors.New("adb: device lacks shell_v2")

// ReadPacket returns the next packet. Legacy sessions report stdout chunks
// and, at EOF, a synthetic ShellExit with code 255 (unknown).
func (s *Shell) ReadPacket() (id byte, data []byte, err error) {
	if s.done {
		return 0, nil, io.EOF
	}
	if !s.V2 {
		buf := make([]byte, 32*1024)
		n, err := s.conn.Read(buf)
		if n > 0 {
			return ShellStdout, buf[:n], nil
		}
		if err == io.EOF {
			s.done = true
			return ShellExit, []byte{255}, nil
		}
		return 0, nil, err
	}
	var h [5]byte
	if _, err := io.ReadFull(s.conn, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(h[1:])
	if n > 16<<20 {
		return 0, nil, fmt.Errorf("adb shell: packet too large (%d)", n)
	}
	data = make([]byte, n)
	if _, err := io.ReadFull(s.conn, data); err != nil {
		return 0, nil, err
	}
	if h[0] == ShellExit {
		s.done = true
	}
	return h[0], data, nil
}

// Close ends the session.
func (s *Shell) Close() error { return s.conn.Close() }

// SetDeadline bounds reads and writes on the session.
func (s *Shell) SetDeadline(t time.Time) error { return s.conn.SetDeadline(t) }

// ShellResult is the outcome of RunShell.
type ShellResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int // -1 when unknown (legacy shell)
}

// RunShell runs a non-interactive command and collects its output.
func (c *Client) RunShell(ctx context.Context, serial, cmd string) (*ShellResult, error) {
	return c.RunShellInput(ctx, serial, cmd, nil)
}

// RunShellInput is RunShell with stdin data (nil = none).
func (c *Client) RunShellInput(ctx context.Context, serial, cmd string, stdin io.Reader) (*ShellResult, error) {
	s, err := c.OpenShell(ctx, serial, ShellOptions{Command: cmd})
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if s.V2 {
		go func() {
			if stdin != nil {
				buf := make([]byte, 32*1024)
				for {
					n, err := stdin.Read(buf)
					if n > 0 {
						if _, werr := s.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						break
					}
				}
			}
			s.CloseStdin()
		}()
	}
	var out, errb bytes.Buffer
	res := &ShellResult{ExitCode: -1}
	for {
		id, data, err := s.ReadPacket()
		if err != nil {
			if err == io.EOF {
				break
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		switch id {
		case ShellStdout:
			out.Write(data)
		case ShellStderr:
			errb.Write(data)
		case ShellExit:
			if s.V2 && len(data) > 0 {
				res.ExitCode = int(data[0])
			}
			res.Stdout, res.Stderr = out.Bytes(), errb.Bytes()
			return res, nil
		}
	}
	res.Stdout, res.Stderr = out.Bytes(), errb.Bytes()
	return res, nil
}

// Exec runs exec:<cmd> (binary-clean stdout, no pty mangling) and returns the
// stream; the caller reads until EOF and closes it.
func (c *Client) Exec(ctx context.Context, serial, cmd string) (*Conn, error) {
	return c.Transport(ctx, serial, "exec:"+cmd)
}

// ShellQuote quotes s for the device's /system/bin/sh.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// GetProps returns the device's getprop map.
func (c *Client) GetProps(ctx context.Context, serial string) (map[string]string, error) {
	r, err := c.RunShell(ctx, serial, "getprop")
	if err != nil {
		return nil, err
	}
	return ParseGetprop(string(r.Stdout)), nil
}

// ParseGetprop parses "[key]: [value]" lines (values may span lines).
func ParseGetprop(s string) map[string]string {
	m := map[string]string{}
	var key string
	var val strings.Builder
	inVal := false
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if !inVal {
			if !strings.HasPrefix(line, "[") {
				continue
			}
			k, rest, ok := strings.Cut(line[1:], "]: [")
			if !ok {
				continue
			}
			key = k
			val.Reset()
			if strings.HasSuffix(rest, "]") {
				m[key] = rest[:len(rest)-1]
				continue
			}
			val.WriteString(rest)
			inVal = true
			continue
		}
		val.WriteString("\n")
		if strings.HasSuffix(line, "]") {
			val.WriteString(line[:len(line)-1])
			m[key] = val.String()
			inVal = false
			continue
		}
		val.WriteString(line)
	}
	return m
}

// GetProp reads one property.
func (c *Client) GetProp(ctx context.Context, serial, name string) (string, error) {
	r, err := c.RunShell(ctx, serial, "getprop "+ShellQuote(name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(r.Stdout)), nil
}
