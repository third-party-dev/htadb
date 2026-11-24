package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"htadb/internal/adb"
	"htadb/internal/cli"
	"htadb/internal/ws"
)

// ctlMsg is a client→server text frame on shell and CLI sockets.
type ctlMsg struct {
	Type string `json:"type"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
	Name string `json:"name"`
}

func sendText(ctx context.Context, c ws.Conn, v any) error {
	b, _ := json.Marshal(v)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Write(wctx, ws.Text, b)
}

func atoiDefault(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}

// shellWS: interactive shell. Binary frames carry stdin/stdout; text frames
// carry control messages (resize, eof) and server events (exit, stderr).
func (s *Server) shellWS(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pty := q.Get("pty") != "0" && q.Get("pty") != "false"
	if q.Get("pty") == "" {
		pty = q.Get("cmd") == ""
	}
	opts := adb.ShellOptions{
		Command: q.Get("cmd"),
		PTY:     pty,
		Term:    q.Get("term"),
		Rows:    atoiDefault(q.Get("rows"), 24),
		Cols:    atoiDefault(q.Get("cols"), 80),
	}
	if opts.PTY && opts.Term == "" {
		opts.Term = "xterm-256color"
	}
	// Open the shell before upgrading so failures are plain HTTP errors.
	sctx, scancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer scancel()
	sh, err := s.ADB.OpenShell(sctx, serial(r), opts)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer sh.Close()
	c, err := s.acceptWS(w, r)
	if err != nil {
		return
	}
	metricWS.Add(1)
	defer metricWS.Add(-1)
	log := s.logger(r).With("serial", serial(r), "v2", sh.V2, "pty", opts.PTY)
	log.Info("shell session opened")
	ctx, cancel := context.WithCancel(sctx)
	defer cancel()
	go ws.KeepAlive(ctx, c, 20*time.Second, cancel)

	// browser → device
	go func() {
		defer cancel()
		for {
			t, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			if t == ws.Binary {
				if _, err := sh.Write(b); err != nil {
					return
				}
				continue
			}
			var m ctlMsg
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			switch m.Type {
			case "resize":
				if m.Rows > 0 && m.Cols > 0 {
					sh.Resize(m.Rows, m.Cols)
				}
			case "eof":
				sh.CloseStdin()
			case "input": // text fallback for clients that cannot send binary
				sh.Write([]byte(m.Name))
			}
		}
	}()

	// device → browser
	code := -1
	for {
		id, data, err := sh.ReadPacket()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				log.Debug("shell read ended", "err", err)
			}
			break
		}
		wctx, wc := context.WithTimeout(ctx, 30*time.Second)
		switch id {
		case adb.ShellStdout:
			err = c.Write(wctx, ws.Binary, data)
		case adb.ShellStderr:
			if opts.PTY {
				err = c.Write(wctx, ws.Binary, data)
			} else {
				err = sendText(ctx, c, map[string]any{"type": "stderr", "data": string(data)})
			}
		case adb.ShellExit:
			if len(data) > 0 && sh.V2 {
				code = int(data[0])
			}
		}
		wc()
		if err != nil {
			break
		}
		if id == adb.ShellExit {
			break
		}
	}
	if ctx.Err() == nil {
		sendText(ctx, c, map[string]any{"type": "exit", "code": code})
		c.Close(ws.StatusNormal, "exit")
	}
	log.Info("shell session closed", "exit", code)
}

// ---- CLI passthrough ----

func (s *Server) cliPost(w http.ResponseWriter, r *http.Request) error {
	if s.CLI == nil {
		return Errf(http.StatusNotImplemented, "cli_disabled", "CLI passthrough is disabled (adb binary not found or --cli=false)")
	}
	var req struct {
		Args    []string `json:"args"`
		Serial  string   `json:"serial"`
		Timeout float64  `json:"timeout"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if len(req.Args) == 0 {
		return badRequest("args is required")
	}
	to := 5 * time.Minute
	if req.Timeout > 0 {
		to = time.Duration(req.Timeout * float64(time.Second))
	}
	ctx, cancel := withTimeout(r, to)
	defer cancel()
	res, err := s.CLI.Run(ctx, req.Serial, req.Args, nil)
	if err != nil {
		return err
	}
	writeJSON(w, 200, res)
	return nil
}

type wsWriter struct {
	ctx context.Context
	c   ws.Conn
	mu  *sync.Mutex
	err bool // stderr → text frame
}

func (w wsWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var err error
	if w.err {
		err = sendText(w.ctx, w.c, map[string]any{"type": "stderr", "data": string(p)})
	} else {
		wctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
		err = w.c.Write(wctx, ws.Binary, append([]byte(nil), p...))
		cancel()
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// cliWS streams `adb [-s serial] arg...` (?arg=..&arg=..&serial=). stdout is
// sent as binary frames, stderr as {"type":"stderr"} text frames; the client
// may send binary stdin, {"type":"signal","name":"SIGINT"} and {"type":"eof"}.
func (s *Server) cliWS(w http.ResponseWriter, r *http.Request) {
	if s.CLI == nil {
		s.writeError(w, r, Errf(http.StatusNotImplemented, "cli_disabled", "CLI passthrough is disabled"))
		return
	}
	q := r.URL.Query()
	args := q["arg"]
	if len(args) == 0 {
		s.writeError(w, r, badRequest("at least one arg is required"))
		return
	}
	c, err := s.acceptWS(w, r)
	if err != nil {
		return
	}
	metricWS.Add(1)
	defer metricWS.Add(-1)
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	log := s.logger(r).With("args", args)
	cmd := s.CLI.Command(ctx, q.Get("serial"), args)
	var mu sync.Mutex
	cmd.Stdout = wsWriter{ctx, c, &mu, false}
	cmd.Stderr = wsWriter{ctx, c, &mu, true}
	stdin, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		sendText(ctx, c, map[string]any{"type": "error", "error": err.Error()})
		c.Close(ws.StatusInternalError, "start failed")
		return
	}
	log.Info("cli started")
	go ws.KeepAlive(ctx, c, 20*time.Second, nil)
	go func() {
		for {
			t, b, err := c.Read(ctx)
			if err != nil {
				// Browser went away: stop the process.
				cli.Interrupt(cmd)
				time.AfterFunc(3*time.Second, func() {
					if cmd.ProcessState == nil && cmd.Process != nil {
						cmd.Process.Kill()
					}
				})
				return
			}
			if t == ws.Binary {
				stdin.Write(b)
				continue
			}
			var m ctlMsg
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			switch m.Type {
			case "signal":
				cli.Signal(cmd, m.Name)
			case "eof":
				stdin.Close()
			}
		}
	}()
	err = cmd.Wait()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	log.Info("cli exited", "code", code)
	if ctx.Err() == nil {
		mu.Lock()
		sendText(ctx, c, map[string]any{"type": "exit", "code": code})
		mu.Unlock()
		c.Close(ws.StatusNormal, "exit")
	}
}

// ---- mirroring ----

func (s *Server) mirrorDisabled() error {
	return Errf(http.StatusNotImplemented, "mirror_disabled", "mirroring is not available")
}

func (s *Server) mirrorServers(w http.ResponseWriter, r *http.Request) error {
	if s.Mirror == nil {
		return s.mirrorDisabled()
	}
	writeJSON(w, 200, s.Mirror.HostServers())
	return nil
}

func (s *Server) deviceMirrorServers(w http.ResponseWriter, r *http.Request) error {
	if s.Mirror == nil {
		return s.mirrorDisabled()
	}
	ctx, cancel := withTimeout(r, 60*time.Second)
	defer cancel()
	v, err := s.Mirror.DeviceServers(ctx, serial(r))
	if err != nil {
		return err
	}
	writeJSON(w, 200, v)
	return nil
}

func (s *Server) clearFallback(w http.ResponseWriter, r *http.Request) error {
	if s.Mirror == nil {
		return s.mirrorDisabled()
	}
	writeJSON(w, 200, map[string]any{"cleared": s.Mirror.ClearFallback(serial(r))})
	return nil
}

func (s *Server) mirrorWS(w http.ResponseWriter, r *http.Request) {
	if s.Mirror == nil {
		s.writeError(w, r, s.mirrorDisabled())
		return
	}
	ctx, cancel := withTimeout(r, 30*time.Second)
	plan, err := s.Mirror.Prepare(ctx, serial(r), r.URL.Query())
	cancel()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	c, err := s.acceptWS(w, r)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	metricWS.Add(1)
	defer metricWS.Add(-1)
	s.Mirror.Run(context.WithoutCancel(r.Context()), c, plan)
}
