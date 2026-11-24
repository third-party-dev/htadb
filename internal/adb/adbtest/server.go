// Package adbtest is a fake adb server speaking the smart-socket protocol,
// with fake devices that implement shell (v2 and legacy), exec, sync, port
// forwarding bookkeeping and abstract unix sockets. It is used by htadb's
// tests and by the fakeadb command for end-to-end runs.
package adbtest

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is a fake adb server.
type Server struct {
	ln net.Listener

	mu       sync.Mutex
	devices  map[string]*Device
	watchers map[chan struct{}]bool
	forwards []string // "serial local remote"
	Log      func(format string, args ...any)

	// ServerVersion is reported by host:version.
	ServerVersion int
}

// NewServer listens on addr ("127.0.0.1:0" for tests) and serves until Close.
func NewServer(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, devices: map[string]*Device{}, watchers: map[chan struct{}]bool{}, ServerVersion: 41}
	go s.serve()
	return s, nil
}

// Addr is the listening address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops the server.
func (s *Server) Close() error { return s.ln.Close() }

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// AddDevice registers (or replaces) a device and notifies trackers.
func (s *Server) AddDevice(d *Device) {
	s.mu.Lock()
	d.srv = s
	s.devices[d.Serial] = d
	s.mu.Unlock()
	s.notify()
}

// RemoveDevice unplugs a device.
func (s *Server) RemoveDevice(serial string) {
	s.mu.Lock()
	delete(s.devices, serial)
	s.mu.Unlock()
	s.notify()
}

// SetState changes a device's state (e.g. "offline").
func (s *Server) SetState(serial, state string) {
	s.mu.Lock()
	if d := s.devices[serial]; d != nil {
		d.State = state
	}
	s.mu.Unlock()
	s.notify()
}

// Device returns a registered device.
func (s *Server) Device(serial string) *Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devices[serial]
}

func (s *Server) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Server) devicesL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.devices {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		d := s.devices[k]
		fmt.Fprintf(&b, "%-22s %s product:%s model:%s device:%s transport_id:%d\n", d.Serial, d.State, d.Product, d.Model, d.DeviceName, i+1)
	}
	return b.String()
}

func (s *Server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func readRequest(r *bufio.Reader) (string, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(l[:]), 16, 16)
	if err != nil {
		return "", fmt.Errorf("bad length %q", l[:])
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func okay(w io.Writer)               { io.WriteString(w, "OKAY") }
func fail(w io.Writer, msg string)   { fmt.Fprintf(w, "FAIL%04x%s", len(msg), msg) }
func hexStr(w io.Writer, msg string) { fmt.Fprintf(w, "%04x%s", len(msg), msg) }

// stream is the raw device connection after the request header was read.
type stream struct {
	net.Conn
	r *bufio.Reader
}

func (s stream) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		req, err := readRequest(r)
		if err != nil {
			return
		}
		s.logf("adbtest: request %q", req)
		switch {
		case req == "host:version":
			okay(c)
			hexStr(c, fmt.Sprintf("%04x", s.ServerVersion))
			return
		case req == "host:devices-l" || req == "host:devices":
			okay(c)
			hexStr(c, s.devicesL())
			return
		case req == "host:track-devices-l" || req == "host:track-devices":
			s.track(c, r)
			return
		case strings.HasPrefix(req, "host-serial:") && strings.HasSuffix(req, ":features"):
			serial := strings.TrimSuffix(strings.TrimPrefix(req, "host-serial:"), ":features")
			d := s.Device(serial)
			if d == nil {
				fail(c, "device '"+serial+"' not found")
				return
			}
			okay(c)
			hexStr(c, strings.Join(d.Features, ","))
			return
		case strings.HasPrefix(req, "host:connect:"):
			addr := strings.TrimPrefix(req, "host:connect:")
			okay(c)
			if strings.HasPrefix(addr, "bad") {
				hexStr(c, "failed to connect to '"+addr+"': Connection refused")
				return
			}
			s.AddDevice(NewDevice(addr))
			hexStr(c, "connected to "+addr)
			return
		case strings.HasPrefix(req, "host:disconnect:"):
			addr := strings.TrimPrefix(req, "host:disconnect:")
			okay(c)
			s.RemoveDevice(addr)
			hexStr(c, "disconnected "+addr)
			return
		case req == "host:list-forward":
			s.mu.Lock()
			out := strings.Join(s.forwards, "\n")
			s.mu.Unlock()
			if out != "" {
				out += "\n"
			}
			okay(c)
			hexStr(c, out)
			return
		case strings.HasPrefix(req, "host-serial:"):
			s.hostSerial(c, req)
			return
		case req == "host:transport-any" || strings.HasPrefix(req, "host:transport:"):
			serial := strings.TrimPrefix(req, "host:transport:")
			var d *Device
			s.mu.Lock()
			if req == "host:transport-any" {
				if len(s.devices) == 1 {
					for _, x := range s.devices {
						d = x
					}
				}
			} else {
				d = s.devices[serial]
			}
			s.mu.Unlock()
			if d == nil {
				fail(c, "device '"+serial+"' not found")
				return
			}
			if d.State != "device" {
				fail(c, "device offline")
				return
			}
			okay(c)
			svc, err := readRequest(r)
			if err != nil {
				return
			}
			s.logf("adbtest: %s service %q", d.Serial, svc)
			d.serve(stream{c, r}, svc)
			return
		default:
			fail(c, "unknown host service")
			return
		}
	}
}

func (s *Server) hostSerial(c net.Conn, req string) {
	rest := strings.TrimPrefix(req, "host-serial:")
	serial, cmd, _ := strings.Cut(rest, ":")
	if s.Device(serial) == nil {
		fail(c, "device '"+serial+"' not found")
		return
	}
	switch {
	case cmd == "get-serialno":
		okay(c)
		hexStr(c, serial)
	case cmd == "get-state":
		okay(c)
		hexStr(c, s.Device(serial).State)
	case strings.HasPrefix(cmd, "forward:"):
		spec := strings.TrimPrefix(cmd, "forward:")
		spec = strings.TrimPrefix(spec, "norebind:")
		local, remote, ok := strings.Cut(spec, ";")
		if !ok {
			fail(c, "bad forward spec")
			return
		}
		okay(c)
		port := ""
		if local == "tcp:0" {
			port = "34567"
			local = "tcp:" + port
		}
		s.mu.Lock()
		s.forwards = append(s.forwards, serial+" "+local+" "+remote)
		s.mu.Unlock()
		okay(c)
		if port != "" {
			hexStr(c, port)
		}
	case cmd == "killforward-all" || strings.HasPrefix(cmd, "killforward:"):
		local := strings.TrimPrefix(cmd, "killforward:")
		okay(c)
		s.mu.Lock()
		var keep []string
		found := false
		for _, f := range s.forwards {
			p := strings.Fields(f)
			if p[0] == serial && (cmd == "killforward-all" || p[1] == local) {
				found = true
				continue
			}
			keep = append(keep, f)
		}
		s.forwards = keep
		s.mu.Unlock()
		if !found && cmd != "killforward-all" {
			fail(c, "listener '"+local+"' not found")
			return
		}
		okay(c)
	default:
		fail(c, "unknown host-serial service")
	}
}

func (s *Server) track(c net.Conn, r *bufio.Reader) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.watchers[ch] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watchers, ch)
		s.mu.Unlock()
	}()
	okay(c)
	closed := make(chan struct{})
	go func() { io.Copy(io.Discard, r); close(closed) }()
	last := "\x00"
	for {
		cur := s.devicesL()
		if cur != last {
			if _, err := fmt.Fprintf(c, "%04x%s", len(cur), cur); err != nil {
				return
			}
			last = cur
		}
		select {
		case <-ch:
		case <-closed:
			return
		}
	}
}

// ---- shell v2 framing helpers for device handlers ----

type v2Writer struct {
	mu *sync.Mutex
	w  io.Writer
	id byte
}

func (w v2Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	h := make([]byte, 5, 5+len(p))
	h[0] = w.id
	binary.LittleEndian.PutUint32(h[1:], uint32(len(p)))
	if _, err := w.w.Write(append(h, p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// v2Reader decodes client→device packets: stdin data is returned by Read,
// close-stdin yields EOF and window-size packets go to onResize.
type v2Reader struct {
	r        io.Reader
	buf      []byte
	eof      bool
	onResize func(string)
}

func (v *v2Reader) Read(p []byte) (int, error) {
	for len(v.buf) == 0 {
		if v.eof {
			return 0, io.EOF
		}
		var h [5]byte
		if _, err := io.ReadFull(v.r, h[:]); err != nil {
			return 0, io.EOF
		}
		n := binary.LittleEndian.Uint32(h[1:])
		data := make([]byte, n)
		if _, err := io.ReadFull(v.r, data); err != nil {
			return 0, io.EOF
		}
		switch h[0] {
		case 0:
			v.buf = data
		case 4:
			v.eof = true
		case 5:
			if v.onResize != nil {
				v.onResize(string(data))
			}
		}
	}
	n := copy(p, v.buf)
	v.buf = v.buf[n:]
	return n, nil
}

// ---- small helpers ----

func waitClosed(c io.Reader, d time.Duration) {
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, c); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}
