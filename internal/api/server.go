// Package api implements htadb's /api/v1 REST and WebSocket endpoints.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"htadb/internal/adb"
	"htadb/internal/cli"
	"htadb/internal/ws"
)

// Version is htadb's version (set by main via -ldflags).
var Version = "dev"

// Mirror is implemented by internal/mirror.
type Mirror interface {
	HostServers() any
	DeviceServers(ctx context.Context, serial string) (any, error)
	// Prepare validates a session request; errors map to HTTP statuses
	// before the WebSocket is accepted.
	Prepare(ctx context.Context, serial string, q url.Values) (any, error)
	Run(ctx context.Context, c ws.Conn, plan any)
	ClearFallback(serial string) bool
	DeviceReconnected(serial string)
}

// Server holds the API's dependencies.
type Server struct {
	ADB         *adb.Client
	Log         *slog.Logger
	CLI         *cli.Runner // nil disables /cli
	Mirror      Mirror      // nil disables mirroring endpoints
	UI          http.Handler
	CORSOrigins []string // "*" = any; empty disables CORS headers

	Tracker *Tracker
	mux     *http.ServeMux
}

var (
	metricRequests = expvar.NewInt("htadb_http_requests")
	metricWS       = expvar.NewInt("htadb_ws_active")
	metricErrors   = expvar.NewInt("htadb_http_errors")
	reqCounter     atomic.Uint64
)

// HTTPError carries a status and machine-readable code.
type HTTPError struct {
	Status int
	Code   string
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }

// Errf builds an HTTPError.
func Errf(status int, code, format string, args ...any) *HTTPError {
	return &HTTPError{Status: status, Code: code, Msg: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) *HTTPError {
	return Errf(http.StatusBadRequest, "bad_request", format, args...)
}

// statusError lets other packages (mirror) choose the HTTP status.
type statusError interface {
	error
	HTTPStatus() int
	ErrorCode() string
}

// classify maps an error to status + code.
func classify(err error) (int, string) {
	var he *HTTPError
	var se statusError
	switch {
	case errors.As(err, &he):
		return he.Status, he.Code
	case errors.As(err, &se):
		return se.HTTPStatus(), se.ErrorCode()
	case adb.IsDeviceNotFound(err):
		return http.StatusNotFound, "device_not_found"
	case adb.IsFail(err):
		return http.StatusBadGateway, "adb_fail"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout"
	}
	var ne *net.OpError
	if errors.As(err, &ne) && ne.Op == "dial" {
		return http.StatusServiceUnavailable, "adb_unavailable"
	}
	return http.StatusInternalServerError, "internal"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := classify(err)
	metricErrors.Add(1)
	s.logger(r).Warn("request failed", "status", status, "code", code, "err", err)
	writeJSON(w, status, map[string]string{"error": err.Error(), "code": code})
}

type ctxKey int

const logKey ctxKey = 0

func (s *Server) logger(r *http.Request) *slog.Logger {
	if l, ok := r.Context().Value(logKey).(*slog.Logger); ok {
		return l
	}
	return s.Log
}

// handler adapts func(w, r) error.
type handler func(w http.ResponseWriter, r *http.Request) error

func (s *Server) h(f handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := f(w, r); err != nil {
			s.writeError(w, r, err)
		}
	}
}

// Handler builds the full HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.Tracker == nil {
		s.Tracker = NewTracker(s.ADB, s.Log)
	}
	m := http.NewServeMux()
	s.mux = m
	s.routes(m)
	if s.UI != nil {
		m.Handle("/ui/", s.UI)
		m.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusFound))
	}
	m.HandleFunc("GET /healthz", s.healthz)
	m.Handle("GET /metrics", expvar.Handler())
	return s.middleware(m)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metricRequests.Add(1)
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			var b [6]byte
			rand.Read(b[:])
			id = fmt.Sprintf("%d-%s", reqCounter.Add(1), hex.EncodeToString(b[:]))
		}
		w.Header().Set("X-Request-Id", id)
		l := s.Log.With("req", id)
		r = r.WithContext(context.WithValue(r.Context(), logKey, l))
		if s.cors(w, r) {
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		defer func() {
			if p := recover(); p != nil {
				l.Error("panic", "panic", p, "path", r.URL.Path)
				if !sw.wrote {
					writeJSON(w, 500, map[string]string{"error": "internal error", "code": "internal"})
				}
			}
			lvl := slog.LevelDebug
			if sw.status >= 500 {
				lvl = slog.LevelWarn
			}
			l.Log(r.Context(), lvl, "http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start).Round(time.Millisecond))
		}()
		next.ServeHTTP(sw, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(c int) {
	if !w.wrote {
		w.status, w.wrote = c, true
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController and the WebSocket upgrade reach the
// underlying writer (Hijacker).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) allowedOrigin(origin string) bool {
	for _, o := range s.CORSOrigins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// cors sets CORS headers; returns true when the request was a preflight.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || len(s.CORSOrigins) == 0 || !s.allowedOrigin(origin) {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Expose-Headers", "Content-Disposition, Content-Length, X-Request-Id, X-Exit-Code")
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, X-Request-Id")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// wsOrigins returns the WebSocket origin patterns matching CORS settings.
func (s *Server) wsOrigins() []string {
	var out []string
	for _, o := range s.CORSOrigins {
		if o == "*" {
			return []string{"*"}
		}
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			out = append(out, u.Host)
		}
	}
	return out
}

func (s *Server) acceptWS(w http.ResponseWriter, r *http.Request) (ws.Conn, error) {
	c, err := ws.Accept(w, r, ws.AcceptOptions{OriginPatterns: s.wsOrigins()})
	if err != nil {
		s.logger(r).Warn("websocket upgrade failed", "err", err)
		return nil, err
	}
	return c, nil
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	res := map[string]any{"status": "ok"}
	if v, err := s.ADB.ServerVersion(ctx); err != nil {
		res["adb"] = false
		res["adbError"] = err.Error()
	} else {
		res["adb"] = true
		res["adbServerVersion"] = v
	}
	writeJSON(w, 200, res)
}

// decodeJSON reads a JSON request body (empty body leaves v unchanged).
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil && err.Error() != "EOF" {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}
