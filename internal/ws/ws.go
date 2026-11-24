// Package ws is htadb's small WebSocket interface. It is the only place that
// imports github.com/coder/websocket, so the implementation can be swapped.
package ws

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// MessageType is a WebSocket data frame type.
type MessageType int

const (
	Text   MessageType = 1
	Binary MessageType = 2
)

// Close status codes used by htadb.
const (
	StatusNormal        = 1000
	StatusGoingAway     = 1001
	StatusPolicy        = 1008
	StatusInternalError = 1011
)

// Conn is a WebSocket connection. Read must be called from one goroutine;
// Write is safe for concurrent use.
type Conn interface {
	Read(ctx context.Context) (MessageType, []byte, error)
	Write(ctx context.Context, t MessageType, p []byte) error
	Ping(ctx context.Context) error
	Close(code int, reason string) error
	SetReadLimit(n int64)
}

// AcceptOptions configures Accept.
type AcceptOptions struct {
	// OriginPatterns lists allowed cross-origin hosts ("*" allows any).
	OriginPatterns []string
}

type conn struct{ c *websocket.Conn }

// Accept upgrades an HTTP request.
func Accept(w http.ResponseWriter, r *http.Request, o AcceptOptions) (Conn, error) {
	opts := &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled}
	for _, p := range o.OriginPatterns {
		if p == "*" {
			opts.InsecureSkipVerify = true
		}
	}
	if !opts.InsecureSkipVerify {
		opts.OriginPatterns = o.OriginPatterns
	}
	c, err := websocket.Accept(w, r, opts)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(1 << 20)
	return &conn{c}, nil
}

// Dial connects to a WebSocket URL (used by tests and tools).
func Dial(ctx context.Context, url string, hdr http.Header) (Conn, error) {
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(64 << 20)
	return &conn{c}, nil
}

func (c *conn) Read(ctx context.Context) (MessageType, []byte, error) {
	t, b, err := c.c.Read(ctx)
	return MessageType(t), b, err
}

func (c *conn) Write(ctx context.Context, t MessageType, p []byte) error {
	return c.c.Write(ctx, websocket.MessageType(t), p)
}

func (c *conn) Ping(ctx context.Context) error { return c.c.Ping(ctx) }

func (c *conn) Close(code int, reason string) error {
	if len(reason) > 120 {
		reason = reason[:120]
	}
	return c.c.Close(websocket.StatusCode(code), reason)
}

func (c *conn) SetReadLimit(n int64) { c.c.SetReadLimit(n) }

// CloseStatus extracts the close code from an error (-1 if none).
func CloseStatus(err error) int {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return int(ce.Code)
	}
	return -1
}

// KeepAlive pings every interval until ctx ends; a failed ping cancels via
// onFail (typically the connection's context cancel).
func KeepAlive(ctx context.Context, c Conn, interval time.Duration, onFail func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, interval)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				if ctx.Err() == nil && onFail != nil {
					onFail()
				}
				return
			}
		}
	}
}
