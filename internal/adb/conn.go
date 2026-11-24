// Package adb is a client for the adb server's smart-socket protocol.
//
// htadb is a peer of the adb CLI: every stream gets its own TCP connection to
// the adb server (normally 127.0.0.1:5037). Requests are framed as a 4-digit
// hex length followed by the service string; the reply is OKAY, or FAIL
// followed by a hex-length-prefixed message.
package adb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Error is returned when the adb server or adbd answers FAIL.
type Error struct {
	Service string
	Msg     string
}

func (e *Error) Error() string {
	if e.Service == "" {
		return "adb: " + e.Msg
	}
	return fmt.Sprintf("adb %s: %s", e.Service, e.Msg)
}

// IsFail reports whether err is an adb FAIL reply.
func IsFail(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

// IsDeviceNotFound reports whether err is a FAIL meaning the serial is
// unknown ("device 'X' not found", "no devices/emulators found").
func IsDeviceNotFound(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	m := e.Msg
	return (strings.HasPrefix(m, "device ") && strings.HasSuffix(m, "not found")) ||
		strings.Contains(m, "no devices") || strings.Contains(m, "no emulators")
}

// Client talks to one adb server.
type Client struct {
	// Addr is the adb server address, e.g. "127.0.0.1:5037".
	Addr string
	// DialTimeout bounds connecting to the adb server.
	DialTimeout time.Duration
	// StartServer, when non-nil, is called when a dial is refused (typically
	// runs "adb start-server"); attempts are at most one per 10 seconds.
	StartServer func(ctx context.Context) error

	startMu   sync.Mutex
	lastStart time.Time
}

func (c *Client) tryStart(ctx context.Context) bool {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if time.Since(c.lastStart) < 10*time.Second {
		return false
	}
	c.lastStart = time.Now()
	return c.StartServer(ctx) == nil
}

// New returns a client for addr.
func New(addr string) *Client {
	return &Client{Addr: addr, DialTimeout: 5 * time.Second}
}

// Conn is one smart-socket connection. After a successful service request it
// is a raw byte stream to that service.
type Conn struct {
	net.Conn
	stopCtx func() bool
}

// Close closes the connection and detaches it from its context.
func (c *Conn) Close() error {
	if c.stopCtx != nil {
		c.stopCtx()
	}
	return c.Conn.Close()
}

// Dial opens a new connection to the adb server. The connection is closed
// when ctx is done.
func (c *Client) Dial(ctx context.Context) (*Conn, error) {
	d := net.Dialer{Timeout: c.DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil && c.StartServer != nil && isRefused(err) && c.tryStart(ctx) {
		nc, err = d.DialContext(ctx, "tcp", c.Addr)
	}
	if err != nil {
		return nil, fmt.Errorf("adb server %s: %w", c.Addr, err)
	}
	conn := &Conn{Conn: nc}
	conn.stopCtx = context.AfterFunc(ctx, func() { nc.Close() })
	return conn, nil
}

func isRefused(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) {
		return strings.Contains(oe.Err.Error(), "refused")
	}
	return false
}

// Send writes one framed request.
func (c *Conn) Send(service string) error {
	if len(service) > 0xffff {
		return fmt.Errorf("adb: service string too long")
	}
	_, err := fmt.Fprintf(c.Conn, "%04x%s", len(service), service)
	return err
}

// Status reads OKAY or FAIL+message.
func (c *Conn) Status(service string) error {
	var st [4]byte
	if _, err := io.ReadFull(c.Conn, st[:]); err != nil {
		return fmt.Errorf("adb %s: reading status: %w", service, err)
	}
	switch string(st[:]) {
	case "OKAY":
		return nil
	case "FAIL":
		msg, err := c.ReadHexString()
		if err != nil {
			return &Error{Service: service, Msg: "FAIL (unreadable message)"}
		}
		return &Error{Service: service, Msg: msg}
	default:
		return fmt.Errorf("adb %s: unexpected status %q", service, st[:])
	}
}

// Request sends a service request and waits for its status.
func (c *Conn) Request(service string) error {
	if err := c.Send(service); err != nil {
		return err
	}
	return c.Status(service)
}

// ReadHexString reads a 4-hex-digit length followed by that many bytes.
func (c *Conn) ReadHexString() (string, error) {
	var l [4]byte
	if _, err := io.ReadFull(c.Conn, l[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(l[:]), 16, 32)
	if err != nil {
		return "", fmt.Errorf("adb: bad length %q", l[:])
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.Conn, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// HostQuery runs a host service and returns its length-prefixed reply.
func (c *Client) HostQuery(ctx context.Context, service string) (string, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.Request(service); err != nil {
		return "", err
	}
	return conn.ReadHexString()
}

// HostCommand runs a host service that replies with OKAY only.
func (c *Client) HostCommand(ctx context.Context, service string) error {
	conn, err := c.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Request(service)
}

// Transport opens a connection switched to the device with the given serial
// ("" means the only device) and requests service on it. The returned Conn is
// the raw service stream.
func (c *Client) Transport(ctx context.Context, serial, service string) (*Conn, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, err
	}
	tr := "host:transport-any"
	if serial != "" {
		tr = "host:transport:" + serial
	}
	if err := conn.Request(tr); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Request(service); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// DeviceOutput runs a device service and returns everything it writes until
// EOF (used for exec:, reboot:, root:, remount: ...).
func (c *Client) DeviceOutput(ctx context.Context, serial, service string) ([]byte, error) {
	conn, err := c.Transport(ctx, serial, service)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return io.ReadAll(conn)
}
