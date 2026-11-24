package mirror

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"htadb/internal/adb"
	"htadb/internal/mirror/profile"
	"htadb/internal/mirror/servers"
	"htadb/internal/ws"
)

// WebSocket binary frame tags (server → browser).
const (
	TagVideo  = 0x01
	TagAudio  = 0x02
	TagDevice = 0x03
)

// launchError is a failure before the first media packet: eligible for the
// automatic fallback.
type launchError struct {
	err  error
	logs []string
}

func (e *launchError) Error() string { return e.err.Error() }
func (e *launchError) Unwrap() error { return e.err }

// Run executes the session, including the one-shot fallback to 4.x.
func (m *Manager) Run(ctx context.Context, c ws.Conn, planAny any) {
	p := planAny.(*Plan)
	log := m.Log.With("serial", p.Serial)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &sender{c: c, ctx: ctx}

	// The browser's reads run for the whole socket lifetime (also across
	// the fallback retry), feeding control messages to the active session.
	ctl := make(chan []byte, 64)
	go func() {
		defer cancel()
		for {
			t, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			if t == ws.Binary {
				select {
				case ctl <- b:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go ws.KeepAlive(ctx, c, 20*time.Second, cancel)

	err := m.attempt(ctx, p, out, ctl, log)
	var le *launchError
	if err != nil && errors.As(err, &le) && ctx.Err() == nil && !p.Explicit && m.Fallback && p.Launch && p.Spec.Profile != profile.FallbackProfile {
		detail := err.Error()
		if len(le.logs) > 0 {
			detail += " | " + strings.Join(lastN(le.logs, 5), " | ")
		}
		np, ferr := m.fallbackPlan(ctx, p, detail)
		if ferr == nil {
			log.Warn("mirroring server failed before streaming; falling back", "from", p.Version, "to", np.Version, "err", err)
			m.remember(p.Serial, FallbackInfo{From: p.Version, To: np.Version, Reason: detail, At: time.Now()})
			out.text(map[string]any{"type": "log", "level": "warn", "line": fmt.Sprintf("htadb: server %s failed (%v); retrying with %s", p.Version, err, np.Version)})
			err = m.attempt(ctx, np, out, ctl, log)
		} else {
			log.Warn("fallback unavailable", "err", ferr)
		}
	}
	if err != nil && ctx.Err() == nil {
		log.Warn("mirroring session failed", "err", err)
		msg := map[string]any{"type": "error", "error": err.Error(), "version": p.Version}
		if errors.As(err, &le) {
			msg["phase"] = "launch"
			msg["logs"] = lastN(le.logs, 20)
		}
		out.text(msg)
		c.Close(ws.StatusInternalError, "mirroring failed")
		return
	}
	c.Close(ws.StatusNormal, "")
}

func lastN(s []string, n int) []string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// sender serializes writes to the WebSocket.
type sender struct {
	c   ws.Conn
	ctx context.Context
	mu  sync.Mutex
}

func (s *sender) text(v any) error {
	b, _ := json.Marshal(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	wctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	return s.c.Write(wctx, ws.Text, b)
}

func (s *sender) binary(tag byte, head, payload []byte) error {
	b := make([]byte, 0, 1+len(head)+len(payload))
	b = append(append(append(b, tag), head...), payload...)
	s.mu.Lock()
	defer s.mu.Unlock()
	wctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	return s.c.Write(wctx, ws.Binary, b)
}

func randomSCID() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:]) & 0x7fffffff
}

// logRing keeps the server's recent output.
type logRing struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRing) add(l string) {
	r.mu.Lock()
	r.lines = append(r.lines, l)
	if len(r.lines) > 50 {
		r.lines = r.lines[len(r.lines)-50:]
	}
	r.mu.Unlock()
}

func (r *logRing) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func logLevel(line string) string {
	switch {
	case strings.Contains(line, "ERROR:"), strings.Contains(line, "Exception"):
		return "error"
	case strings.Contains(line, "WARN:"):
		return "warn"
	case strings.Contains(line, "DEBUG:"), strings.Contains(line, "VERBOSE:"):
		return "debug"
	}
	return "info"
}

// LaunchCommand builds the shell command that starts the server.
func LaunchCommand(classpath string, spec *profile.Spec, scid uint32, opts map[string]string) string {
	args := []string{
		"CLASSPATH=" + adb.ShellQuote(classpath), "app_process", "/", "com.genymobile.scrcpy.Server",
		adb.ShellQuote(spec.Version), fmt.Sprintf("scid=%08x", scid), "tunnel_forward=true",
	}
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, adb.ShellQuote(k+"="+opts[k]))
	}
	return strings.Join(args, " ")
}

// SocketName is the server's abstract socket for a scid.
func SocketName(scid uint32) string { return fmt.Sprintf("scrcpy_%08x", scid) }

func (m *Manager) attempt(ctx context.Context, p *Plan, out *sender, ctl <-chan []byte, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	scid := p.SCID
	if p.Launch {
		scid = randomSCID()
	}
	log = log.With("version", p.Version, "scid", fmt.Sprintf("%08x", scid))
	logs := &logRing{}
	fail := func(err error) error { return &launchError{err: err, logs: logs.get()} }
	var cleanup []func()
	defer func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}()

	serverDone := make(chan struct{})
	var serverExit = -1
	if p.Launch {
		if p.Build != nil {
			res, err := m.Jars.Ensure(ctx, p.Serial, p.Build)
			if err != nil {
				return fail(fmt.Errorf("installing server %s: %w", p.Version, err))
			}
			log.Info("server jar ready", "path", res.Path, "pushed", res.Pushed, "reason", res.Reason)
			p.JarPath = res.Path
		}
		// The server deletes its own classpath when cleanup is on, so launch
		// from a per-session hard link (copy as fallback) and keep the
		// named jar for later sessions.
		sess := fmt.Sprintf("%s/.session-%08x.jar", servers.DeviceDir, scid)
		r, err := m.ADB.RunShell(ctx, p.Serial, fmt.Sprintf("mkdir -p %s; ln -f %s %s 2>/dev/null || cp %s %s",
			servers.DeviceDir, adb.ShellQuote(p.JarPath), sess, adb.ShellQuote(p.JarPath), sess))
		if err != nil {
			return fail(err)
		}
		if r.ExitCode > 0 {
			return fail(fmt.Errorf("preparing session jar: %s", strings.TrimSpace(string(r.Stderr))))
		}
		cleanup = append(cleanup, func() {
			go m.ADB.RunShell(context.Background(), p.Serial, "rm -f "+sess)
		})
		cmd := LaunchCommand(sess, p.Spec, scid, p.Options)
		log.Debug("launching mirroring server", "cmd", cmd)
		sh, err := m.ADB.OpenShell(ctx, p.Serial, adb.ShellOptions{Command: cmd})
		if err != nil {
			return fail(err)
		}
		cleanup = append(cleanup, func() { sh.Close() })
		go func() {
			defer close(serverDone)
			var bufs [3]bytes.Buffer
			for {
				id, data, err := sh.ReadPacket()
				if err != nil {
					return
				}
				switch id {
				case adb.ShellStdout, adb.ShellStderr:
					b := &bufs[id]
					b.Write(data)
					for {
						line, err := b.ReadString('\n')
						if err != nil {
							b.WriteString(line)
							break
						}
						line = strings.TrimRight(line, "\r\n")
						logs.add(line)
						out.text(map[string]any{"type": "log", "level": logLevel(line), "line": line})
					}
				case adb.ShellExit:
					if len(data) > 0 {
						serverExit = int(data[0])
					}
					return
				}
			}
		}()
	}

	// Connect video, audio, control — in that order — retrying while the
	// server starts.
	wantVideo := p.Options["video"] != "false"
	wantAudio := p.Options["audio"] == "true"
	wantControl := p.Options["control"] != "false"
	name := SocketName(scid)
	var socks []*adb.Conn
	cleanup = append(cleanup, func() {
		for _, s := range socks {
			s.Close()
		}
	})
	connect := func(first bool) (*adb.Conn, error) {
		var last error
		for i := 0; i < m.ConnectAttempts; i++ {
			select {
			case <-serverDone:
				return nil, fmt.Errorf("server exited (code %d) before accepting connections", serverExit)
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			conn, err := m.ADB.LocalAbstract(ctx, p.Serial, name)
			if err == nil {
				if !first || !p.Spec.Handshake.DummyByte {
					return conn, nil
				}
				// The dummy byte proves something is listening (an adb
				// forward accepts even when nothing is).
				conn.SetReadDeadline(time.Now().Add(m.HandshakeTimeout))
				var b [1]byte
				_, err = io.ReadFull(conn, b[:])
				conn.SetReadDeadline(time.Time{})
				if err == nil {
					return conn, nil
				}
				conn.Close()
			}
			last = err
			select {
			case <-time.After(m.ConnectDelay):
			case <-serverDone:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, fmt.Errorf("could not connect to %s: %v", name, last)
	}
	var video, audio, control *adb.Conn
	first := true
	for _, s := range []struct {
		want bool
		dst  **adb.Conn
	}{{wantVideo, &video}, {wantAudio, &audio}, {wantControl, &control}} {
		if !s.want {
			continue
		}
		conn, err := connect(first)
		if err != nil {
			return fail(err)
		}
		first = false
		socks = append(socks, conn)
		*s.dst = conn
	}
	if len(socks) == 0 {
		return fail(errors.New("video, audio and control are all disabled"))
	}

	// Handshake.
	hs := p.Spec.Handshake
	for _, s := range socks {
		s.SetReadDeadline(time.Now().Add(m.HandshakeTimeout))
	}
	nameBuf := make([]byte, hs.DeviceNameLen)
	if _, err := io.ReadFull(socks[0], nameBuf); err != nil {
		return fail(fmt.Errorf("reading device meta: %w", err))
	}
	deviceName := string(bytes.TrimRight(nameBuf, "\x00"))
	meta := map[string]any{
		"type": "meta", "serial": p.Serial, "deviceName": deviceName, "version": p.Version,
		"profile": p.Spec.Profile, "tested": p.Spec.Tested, "verified": p.Verified, "scid": fmt.Sprintf("%08x", scid),
		"controlTypes": p.Spec.ControlTypeList(), "handshake": hs, "options": p.Options, "sdk": p.SDK,
		"source": p.Source, "warnings": p.Warnings, "jar": p.JarPath,
		"video": wantVideo, "audio": wantAudio, "control": wantControl,
	}
	if p.FallbackFrom != "" {
		meta["fallbackFrom"], meta["fallbackReason"], meta["fallbackDetail"] = p.FallbackFrom, p.FallbackReason, p.FallbackDetail
	}
	readCodec := func(s *adb.Conn, kind string) (string, bool, error) {
		var b [4]byte
		if _, err := io.ReadFull(s, b[:]); err != nil {
			return "", false, fmt.Errorf("reading %s codec: %w", kind, err)
		}
		id := binary.BigEndian.Uint32(b[:])
		switch id {
		case 0:
			return "", false, nil // stream disabled by the device
		case 1:
			return "", false, fmt.Errorf("%s stream: configuration error on the device", kind)
		}
		n, ok := profile.CodecIDs[id]
		if !ok {
			return "", false, fmt.Errorf("%s stream: unknown codec id %08x", kind, id)
		}
		return n, true, nil
	}
	var firstVideo []byte
	if video != nil {
		codec, ok, err := readCodec(video, "video")
		if err != nil {
			return fail(err)
		}
		if !ok {
			return fail(errors.New("the device disabled the video stream"))
		}
		meta["videoCodec"] = codec
		if hs.SessionPacket {
			hdr := make([]byte, 12)
			if _, err := io.ReadFull(video, hdr); err != nil {
				return fail(fmt.Errorf("reading video session: %w", err))
			}
			if binary.BigEndian.Uint64(hdr)&profile.FlagSession == 0 {
				// No session packet: keep it as the first media header.
				firstVideo = hdr
			} else {
				meta["width"] = binary.BigEndian.Uint32(hdr[4:])
				meta["height"] = binary.BigEndian.Uint32(hdr[8:])
			}
		}
	}
	if audio != nil {
		codec, ok, err := readCodec(audio, "audio")
		if err != nil {
			return fail(err)
		}
		if ok {
			meta["audioCodec"] = codec
		} else {
			meta["audio"] = false
			p.Warnings = append(p.Warnings, "the device could not capture audio (Android 11+ is required); continuing without audio")
			meta["warnings"] = p.Warnings
			audio.Close()
			audio = nil
		}
	}
	for _, s := range socks {
		s.SetReadDeadline(time.Time{})
	}

	// Pumps. Errors before the first media packet are launch errors.
	started := make(chan struct{})
	var startOnce sync.Once
	markStarted := func() {
		startOnce.Do(func() {
			out.text(meta)
			log.Info("mirroring started", "device", deviceName, "codec", meta["videoCodec"], "fallbackFrom", p.FallbackFrom)
			close(started)
		})
	}
	errc := make(chan error, 4)
	pumpMedia := func(s *adb.Conn, tag byte, pre []byte) {
		r := bufio.NewReaderSize(s, 256*1024)
		hdr := make([]byte, 12)
		for {
			if pre != nil {
				copy(hdr, pre)
				pre = nil
			} else if _, err := io.ReadFull(r, hdr); err != nil {
				errc <- fmt.Errorf("stream %d: %w", tag, err)
				return
			}
			v := binary.BigEndian.Uint64(hdr)
			if v&profile.FlagSession != 0 {
				out.text(map[string]any{"type": "session", "width": binary.BigEndian.Uint32(hdr[4:]), "height": binary.BigEndian.Uint32(hdr[8:]),
					"clientResized": hs.ResizedFlag && hdr[3]&1 == 1})
				continue
			}
			n := binary.BigEndian.Uint32(hdr[8:])
			if n > 32<<20 {
				errc <- fmt.Errorf("stream %d: packet too large (%d)", tag, n)
				return
			}
			payload := make([]byte, n)
			if _, err := io.ReadFull(r, payload); err != nil {
				errc <- fmt.Errorf("stream %d: %w", tag, err)
				return
			}
			markStarted()
			if err := out.binary(tag, hdr[:8], payload); err != nil {
				errc <- err
				return
			}
		}
	}
	if video != nil {
		go pumpMedia(video, TagVideo, firstVideo)
	}
	if audio != nil {
		go pumpMedia(audio, TagAudio, nil)
	}
	if video == nil && audio == nil {
		markStarted()
	}
	if control != nil {
		go func() {
			dr := profile.NewDeviceReader(control, p.Spec)
			for {
				msg, err := dr.Next()
				if err != nil {
					errc <- fmt.Errorf("control: %w", err)
					return
				}
				if err := out.binary(TagDevice, nil, msg.Raw); err != nil {
					errc <- err
					return
				}
			}
		}()
	}

	firstTimer := time.NewTimer(m.FirstPacketWait)
	defer firstTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			select {
			case <-started:
				return fmt.Errorf("mirroring stopped: %w", err)
			default:
				return fail(err)
			}
		case <-serverDone:
			serverDone = nil // stop selecting on it
			select {
			case <-started:
				return fmt.Errorf("mirroring server exited (code %d)", serverExit)
			default:
				return fail(fmt.Errorf("server exited (code %d) before the first video packet", serverExit))
			}
		case <-firstTimer.C:
			select {
			case <-started:
			default:
				return fail(errors.New("timed out waiting for the first video packet"))
			}
		case b := <-ctl:
			if control == nil {
				continue
			}
			if err := p.Spec.Validate(b); err != nil {
				out.text(map[string]any{"type": "error", "fatal": false, "error": err.Error()})
				continue
			}
			control.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := control.Write(b); err != nil {
				return fmt.Errorf("control write: %w", err)
			}
		}
	}
}
