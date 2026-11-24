package mirror_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"htadb/internal/adb"
	"htadb/internal/adb/adbtest"
	"htadb/internal/config"
	"htadb/internal/mirror"
	"htadb/internal/mirror/mirrortest"
	"htadb/internal/mirror/profile"
	"htadb/internal/mirror/servers"
	"htadb/internal/ws"
)

type env struct {
	srv  *adbtest.Server
	dev  *adbtest.Device
	fake *mirrortest.Server
	m    *mirror.Manager
	http *httptest.Server
}

func newEnv(t *testing.T, jars ...string) *env {
	t.Helper()
	srv, err := adbtest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	d := adbtest.NewDevice("D1")
	srv.AddDevice(d)
	fake := &mirrortest.Server{FailVersions: map[string]string{}, FailAfterStart: map[string]int{}, Loop: true, FPS: 60}
	fake.Install(d)
	if jars == nil {
		jars = []string{"5.0.1", "4.1"}
	}
	mfs := fstest.MapFS{}
	for _, v := range jars {
		mfs["scrcpy-server-v"+v+".jar"] = &fstest.MapFile{Data: mirrortest.FakeJar(v)}
	}
	reg, err := servers.NewRegistry(mfs, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m := mirror.New(adb.New(srv.Addr()), reg, log)
	m.ConnectDelay = 10 * time.Millisecond
	m.FirstPacketWait = 3 * time.Second
	m.HandshakeTimeout = 3 * time.Second
	e := &env{srv: srv, dev: d, fake: fake, m: m}
	e.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plan, err := m.Prepare(r.Context(), "D1", r.URL.Query())
		if err != nil {
			status := 500
			var me interface{ HTTPStatus() int }
			if errors.As(err, &me) {
				status = me.HTTPStatus()
			}
			http.Error(w, err.Error(), status)
			return
		}
		c, err := ws.Accept(w, r, ws.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		m.Run(context.Background(), c, plan)
	}))
	t.Cleanup(e.http.Close)
	return e
}

type session struct {
	c      ws.Conn
	meta   map[string]any
	texts  []map[string]any
	video  [][]byte
	device [][]byte
	audio  int
	err    map[string]any
}

func (e *env) open(t *testing.T, q string) (*session, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(e.http.URL, "http")+"/?"+q, nil)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { c.Close(1000, "") })
	return &session{c: c}, nil
}

// readUntil reads frames until cond is true or the socket closes.
func (s *session) readUntil(t *testing.T, cond func(*session) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for !cond(s) {
		typ, b, err := s.c.Read(ctx)
		if err != nil {
			return
		}
		if typ == ws.Text {
			var m map[string]any
			json.Unmarshal(b, &m)
			s.texts = append(s.texts, m)
			switch m["type"] {
			case "meta":
				s.meta = m
			case "error":
				if m["fatal"] != false {
					s.err = m
				}
			}
			continue
		}
		switch b[0] {
		case mirror.TagVideo:
			s.video = append(s.video, b)
		case mirror.TagAudio:
			s.audio++
		case mirror.TagDevice:
			s.device = append(s.device, b[1:])
		}
	}
}

func streaming(s *session) bool { return len(s.video) >= 3 || s.err != nil }

func TestSessionBasics(t *testing.T) {
	e := newEnv(t)
	s, err := e.open(t, "max_size=1200")
	if err != nil {
		t.Fatal(err)
	}
	s.readUntil(t, streaming)
	if s.err != nil || s.meta == nil {
		t.Fatalf("err=%v meta=%v", s.err, s.meta)
	}
	if s.meta["version"] != "5.0.1" || s.meta["profile"] != "5.0" || s.meta["verified"] != true || s.meta["deviceName"] != "Fake Phone" || s.meta["videoCodec"] != "h264" {
		t.Fatalf("meta %v", s.meta)
	}
	if s.meta["height"].(float64) != 1200 {
		t.Errorf("size %v x %v", s.meta["width"], s.meta["height"])
	}
	if _, ok := s.meta["fallbackFrom"]; ok {
		t.Error("unexpected fallback")
	}
	// First video packet is the config packet: 0x01 + pts/flags with CONFIG.
	v := s.video[0]
	if binary.BigEndian.Uint64(v[1:9])&profile.FlagConfig == 0 || !bytes.HasPrefix(v[9:], []byte{0, 0, 0, 1, 0x67}) {
		t.Fatalf("first video packet % x", v[:16])
	}
	if binary.BigEndian.Uint64(s.video[1][1:9])&profile.FlagKeyFrame == 0 {
		t.Error("second packet should be a keyframe")
	}
	l := e.fake.LastLaunch()
	if l.Args["tunnel_forward"] != "true" || l.Args["max_size"] != "1200" || l.Args["video_codec"] != "h264" || len(l.Args["scid"]) != 8 {
		t.Fatalf("launch args %v", l.Args)
	}
	// Launched from a per-session link; the named jar survives the
	// server's cleanup (which unlinks its classpath).
	if !strings.Contains(l.Classpath, "/.session-") {
		t.Errorf("classpath %s", l.Classpath)
	}
	if _, ok := e.dev.ReadFile(servers.DevicePath("5.0.1")); !ok {
		t.Fatal("named jar was deleted")
	}

	// Control: inject keycode goes through, clipboard round-trips.
	s.c.Write(context.Background(), ws.Binary, profile.EncodeInjectKeycode(0, 3, 0, 0))
	e.fake.DeviceClipboard = "from device"
	s.c.Write(context.Background(), ws.Binary, profile.EncodeGetClipboard(1))
	s.c.Write(context.Background(), ws.Binary, profile.EncodeSetClipboard(7, true, "to device"))
	s.readUntil(t, func(s *session) bool { return len(s.device) >= 2 })
	if !bytes.Equal(s.device[0], profile.EncodeDeviceClipboard("from device")) || !bytes.Equal(s.device[1], profile.EncodeDeviceAckClipboard(7)) {
		t.Fatalf("device messages % x", s.device)
	}
	// Malformed control message: rejected with a non-fatal error, session
	// continues.
	s.c.Write(context.Background(), ws.Binary, []byte{profile.InjectKeycode, 1})
	n := len(s.video)
	s.readUntil(t, func(s *session) bool { return len(s.video) > n+3 })
	found := false
	for _, m := range s.texts {
		if m["type"] == "error" && m["fatal"] == false {
			found = true
		}
	}
	if !found {
		t.Error("expected non-fatal error for malformed control")
	}
	if got := len(e.fake.ControlMessages()); got != 3 {
		t.Errorf("fake received %d control messages, want 3", got)
	}

	// A second session reuses the jar (no new push).
	pushes := e.dev.ServiceCount("shell,v2,raw:mv -f")
	s2, _ := e.open(t, "")
	s2.readUntil(t, streaming)
	if s2.err != nil || e.dev.ServiceCount("shell,v2,raw:mv -f") != pushes {
		t.Fatalf("jar not reused: %v", s2.err)
	}
}

func TestScanFileRejectedOn40(t *testing.T) {
	e := newEnv(t, "4.0")
	s, _ := e.open(t, "version=4.0")
	s.readUntil(t, streaming)
	if s.meta["profile"] != "4.x" {
		t.Fatal(s.meta, s.err)
	}
	s.c.Write(context.Background(), ws.Binary, profile.EncodeScanFile("/sdcard/x"))
	s.readUntil(t, func(s *session) bool {
		for _, m := range s.texts {
			if m["type"] == "error" {
				return true
			}
		}
		return false
	})
	if len(e.fake.ControlMessages()) != 0 {
		t.Fatal("SCAN_FILE reached a 4.0 server")
	}
}

func TestLaunchFallback(t *testing.T) {
	e := newEnv(t)
	e.fake.FailVersions["5.0.1"] = "Could not create encoder"
	s, _ := e.open(t, "")
	s.readUntil(t, streaming)
	if s.err != nil {
		t.Fatal(s.err)
	}
	if e.fake.LaunchCount() != 2 {
		t.Fatalf("launches %d, want 2", e.fake.LaunchCount())
	}
	if s.meta["version"] != "4.1" || s.meta["fallbackFrom"] != "5.0.1" || s.meta["fallbackReason"] != "launch" {
		t.Fatalf("meta %v", s.meta)
	}
	if !strings.Contains(s.meta["fallbackDetail"].(string), "Could not create encoder") {
		t.Errorf("detail should carry server log: %v", s.meta["fallbackDetail"])
	}
	if f, ok := e.m.RememberedFallback("D1"); !ok || f.From != "5.0.1" || f.To != "4.1" {
		t.Fatal("fallback not remembered")
	}

	// Memory is reused: next session goes straight to 4.1.
	s2, _ := e.open(t, "")
	s2.readUntil(t, streaming)
	if e.fake.LaunchCount() != 3 || s2.meta["version"] != "4.1" || s2.meta["fallbackFrom"] != "5.0.1" {
		t.Fatalf("remembered: launches=%d meta=%v", e.fake.LaunchCount(), s2.meta)
	}

	// Cleared on reconnect.
	e.m.OnDeviceEvent(adb.DeviceEvent{Type: "add", Device: adb.Device{Serial: "D1", State: "device"}})
	if _, ok := e.m.RememberedFallback("D1"); ok {
		t.Fatal("not cleared on reconnect")
	}
	delete(e.fake.FailVersions, "5.0.1")
	s3, _ := e.open(t, "")
	s3.readUntil(t, streaming)
	if s3.meta["version"] != "5.0.1" {
		t.Fatalf("after reconnect: %v", s3.meta)
	}

	// Explicit clear.
	e.fake.FailVersions["5.0.1"] = "boom"
	s4, _ := e.open(t, "")
	s4.readUntil(t, streaming)
	if !e.m.ClearFallback("D1") || e.m.ClearFallback("D1") {
		t.Fatal("ClearFallback")
	}
}

func TestExplicitVersionNeverFallsBack(t *testing.T) {
	e := newEnv(t)
	e.fake.FailVersions["5.0.1"] = "nope"
	s, _ := e.open(t, "version=5.0.1")
	s.readUntil(t, streaming)
	if s.err == nil || s.meta != nil {
		t.Fatalf("expected error, meta=%v", s.meta)
	}
	if e.fake.LaunchCount() != 1 || !strings.Contains(s.err["error"].(string), "server exited") || s.err["version"] != "5.0.1" {
		t.Fatalf("launches=%d err=%v", e.fake.LaunchCount(), s.err)
	}
	// Per-device config is explicit too.
	e.m.Devices = map[string]config.DeviceConfig{"D1": {Version: "5.0.1"}}
	s2, _ := e.open(t, "")
	s2.readUntil(t, streaming)
	if s2.err == nil || e.fake.LaunchCount() != 2 {
		t.Fatal("device-config version must not fall back")
	}
	if _, ok := e.m.RememberedFallback("D1"); ok {
		t.Fatal("explicit failure must not be remembered")
	}
}

func TestNoFallbackAfterStreaming(t *testing.T) {
	e := newEnv(t)
	e.fake.FailAfterStart["5.0.1"] = 2
	s, _ := e.open(t, "")
	s.readUntil(t, func(s *session) bool { return s.err != nil })
	if s.meta == nil || s.meta["version"] != "5.0.1" || s.err == nil {
		t.Fatalf("meta=%v err=%v", s.meta, s.err)
	}
	if e.fake.LaunchCount() != 1 {
		t.Fatalf("launches %d: must not fall back after streaming started", e.fake.LaunchCount())
	}
	if _, ok := e.m.RememberedFallback("D1"); ok {
		t.Fatal("must not remember")
	}
}

func TestFallbackRetriesOnce(t *testing.T) {
	e := newEnv(t)
	e.fake.FailVersions["5.0.1"] = "a"
	e.fake.FailVersions["4.1"] = "b"
	s, _ := e.open(t, "")
	s.readUntil(t, streaming)
	if s.err == nil || e.fake.LaunchCount() != 2 {
		t.Fatalf("launches %d err=%v", e.fake.LaunchCount(), s.err)
	}
}

func TestSDKFallback(t *testing.T) {
	// Real profiles share minSdk 21; raise 5.0's for the test.
	p := profile.ByName("5.0")
	old := map[string]int{}
	for v, s := range p.Versions {
		old[v] = s.MinSdk
		s.MinSdk = 30
	}
	t.Cleanup(func() {
		for v, s := range p.Versions {
			s.MinSdk = old[v]
		}
	})
	e := newEnv(t)
	e.dev.Props["ro.build.version.sdk"] = "28"
	s, _ := e.open(t, "")
	s.readUntil(t, streaming)
	if s.err != nil || s.meta["version"] != "4.1" || s.meta["fallbackReason"] != "sdk" || e.fake.LaunchCount() != 1 {
		t.Fatalf("meta=%v err=%v launches=%d", s.meta, s.err, e.fake.LaunchCount())
	}
	// Explicit 5.0 on that device → 409.
	resp, err := http.Get(e.http.URL + "/?version=5.0.1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestVersionErrors(t *testing.T) {
	e := newEnv(t)
	for q, want := range map[string]int{
		"version=4.2":          409,
		"version=3.3.4":        409,
		"version=5.0":          404, // allowlisted, but no build anywhere
		"bogus_option=1":       400,
		"scid=1":               400, // reserved: htadb sets it
		"launch=false":         400,
		"server_path=/x/y.jar": 400,
		"max_size=notanumber":  400,
		"tunnel_forward=false": 400,
	} {
		resp, err := http.Get(e.http.URL + "/?" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", q, resp.StatusCode, want)
		}
	}
	e.m.AllowUntested = true
	plan, err := e.m.Prepare(context.Background(), "D1", url.Values{"version": {"4.2"}})
	if err == nil {
		t.Fatalf("4.2 has no jar: %v", plan)
	}
}

func TestDeviceJarWithoutHostBuild(t *testing.T) {
	e := newEnv(t, "5.0.1")
	e.dev.WriteFile(servers.DevicePath("4.0"), mirrortest.FakeJar("4.0"), 0o644)
	s, _ := e.open(t, "version=4.0")
	s.readUntil(t, streaming)
	if s.err != nil || s.meta["version"] != "4.0" || s.meta["verified"] != false {
		t.Fatalf("meta=%v err=%v", s.meta, s.err)
	}
	w := s.meta["warnings"].([]any)
	if len(w) == 0 || !strings.Contains(w[0].(string), "no build") {
		t.Errorf("warnings %v", w)
	}
}

func TestServerPathAndAttach(t *testing.T) {
	e := newEnv(t)
	e.dev.WriteFile("/data/local/tmp/custom/scrcpy-server-v4.1.jar", mirrortest.FakeJar("4.1"), 0o644)
	s, _ := e.open(t, "server_path=/data/local/tmp/custom/scrcpy-server-v4.1.jar")
	s.readUntil(t, streaming)
	if s.err != nil || s.meta["version"] != "4.1" || s.meta["source"] != "server_path" {
		t.Fatalf("meta=%v err=%v", s.meta, s.err)
	}
	if _, ok := e.dev.ReadFile("/data/local/tmp/custom/scrcpy-server-v4.1.jar"); !ok {
		t.Fatal("pre-installed jar deleted")
	}

	// Attach mode connects to an already-running server.
	l := e.fake.LaunchCount()
	e.dev.WriteFile(servers.DevicePath("5.0.1"), mirrortest.FakeJar("5.0.1"), 0o644)
	go func() {
		e.dev.Commands["app_process"](&adbtest.ShellEnv{
			Device: e.dev, Env: map[string]string{"CLASSPATH": servers.DevicePath("5.0.1")},
			Args:   []string{"app_process", "/", "com.genymobile.scrcpy.Server", "5.0.1", "scid=0000abcd", "tunnel_forward=true", "audio=false", "cleanup=false"},
			Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Stdin: strings.NewReader(""),
		})
	}()
	time.Sleep(100 * time.Millisecond)
	s2, err := e.open(t, "launch=false&scid=0000abcd&version=5.0.1")
	if err != nil {
		t.Fatal(err)
	}
	s2.readUntil(t, streaming)
	if s2.err != nil || s2.meta["scid"] != "0000abcd" || e.fake.LaunchCount() != l+1 {
		t.Fatalf("attach meta=%v err=%v", s2.meta, s2.err)
	}
}

func TestAudio(t *testing.T) {
	e := newEnv(t)
	s, _ := e.open(t, "audio=true")
	s.readUntil(t, func(s *session) bool { return (len(s.video) >= 3 && s.audio >= 3) || s.err != nil })
	if s.err != nil || s.meta["audioCodec"] != "opus" || s.audio < 3 {
		t.Fatalf("meta=%v err=%v audio=%d", s.meta, s.err, s.audio)
	}
	e.fake.AudioUnavailable = true
	s2, _ := e.open(t, "audio=true")
	s2.readUntil(t, streaming)
	if s2.err != nil || s2.meta["audio"] != false {
		t.Fatalf("audio unavailable: %v %v", s2.meta, s2.err)
	}
}
