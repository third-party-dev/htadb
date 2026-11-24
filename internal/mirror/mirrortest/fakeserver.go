// Package mirrortest provides a fake mirroring server (speaking the scrcpy
// server protocol) for adbtest devices: it is installed as the device's
// app_process command, so htadb's real launch path is exercised.
package mirrortest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"htadb/internal/adb/adbtest"
	"htadb/internal/mirror/profile"
)

// FakeJar returns fake jar content for a version; the fake server refuses
// to start when the launch version differs from the jar's.
func FakeJar(version string) []byte {
	return []byte("PK\x03\x04 fake scrcpy-server jar version=" + version + "\n")
}

func jarVersion(b []byte) string {
	_, v, ok := bytes.Cut(b, []byte("version="))
	if !ok {
		return ""
	}
	return strings.TrimSpace(string(v))
}

// Frame is one encoded video access unit.
type Frame struct {
	Data []byte
	Key  bool
}

// Server configures the fake.
type Server struct {
	// FailVersions makes the server exit with an error before listening.
	FailVersions map[string]string
	// FailAfterStart makes the server exit after N video packets.
	FailAfterStart map[string]int
	// Config and Frames are the H.264 stream (Annex B). Defaults to a tiny
	// synthetic stream.
	Config []byte
	Frames []Frame
	FPS    int
	Loop   bool
	Width  uint32
	Height uint32
	// DeviceClipboard is returned on GET_CLIPBOARD.
	DeviceClipboard string
	// AudioUnavailable makes the server send codec id 0 on the audio socket.
	AudioUnavailable bool

	mu       sync.Mutex
	Launches []Launch
	Controls [][]byte
}

// Launch records one app_process invocation.
type Launch struct {
	Version   string
	Classpath string
	Args      map[string]string
}

// LaunchCount returns the number of launches so far.
func (s *Server) LaunchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Launches)
}

// LastLaunch returns the most recent launch.
func (s *Server) LastLaunch() Launch {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Launches) == 0 {
		return Launch{}
	}
	return s.Launches[len(s.Launches)-1]
}

// ControlMessages returns received control messages.
func (s *Server) ControlMessages() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.Controls...)
}

// Install hooks the fake into a device.
func (s *Server) Install(d *adbtest.Device) {
	d.Commands["app_process"] = s.run
}

var defaultConfig = []byte{0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x1f, 0xda, 0x01, 0x40, 0x16, 0xec, 0x04, 0x40, 0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80}

func (s *Server) stream() ([]byte, []Frame) {
	if s.Config != nil {
		return s.Config, s.Frames
	}
	return defaultConfig, []Frame{
		{Data: []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00, 0x33}, Key: true},
		{Data: []byte{0, 0, 0, 1, 0x41, 0x9a, 0x02}},
		{Data: []byte{0, 0, 0, 1, 0x41, 0x9a, 0x04}},
	}
}

func (s *Server) run(e *adbtest.ShellEnv) int {
	logf := func(level, f string, a ...any) {
		w := e.Stdout
		if level == "ERROR" || level == "WARN" {
			w = e.Stderr
		}
		fmt.Fprintf(w, "[server] %s: %s\n", level, fmt.Sprintf(f, a...))
	}
	// app_process / com.genymobile.scrcpy.Server <version> k=v...
	if len(e.Args) < 4 || e.Args[2] != "com.genymobile.scrcpy.Server" {
		fmt.Fprintln(e.Stderr, "app_process: bad arguments")
		return 1
	}
	cp := e.Env["CLASSPATH"]
	jar, ok := e.Device.ReadFile(cp)
	if !ok {
		fmt.Fprintf(e.Stderr, "Error: Could not find or load main class com.genymobile.scrcpy.Server (classpath %s)\n", cp)
		return 1
	}
	server := jarVersion(jar)
	client := e.Args[3]
	if server == "" && len(jar) > 100000 {
		server = client // a real scrcpy-server jar (e.g. embedded build): trust it
	}
	args := map[string]string{}
	for _, a := range e.Args[4:] {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			logf("ERROR", "Invalid key=value pair: \"%s\"", a)
			return 1
		}
		args[k] = v
	}
	s.mu.Lock()
	s.Launches = append(s.Launches, Launch{Version: client, Classpath: cp, Args: args})
	s.mu.Unlock()
	if client != server {
		logf("ERROR", "The server version (%s) does not match the client (%s)", server, client)
		return 1
	}
	spec, err := profile.Lookup(server, true)
	if err != nil {
		logf("ERROR", "%v", err)
		return 1
	}
	for k := range args {
		switch k {
		case "scid", "tunnel_forward", "cleanup", "send_device_meta", "send_frame_meta", "send_dummy_byte", "send_stream_meta", "raw_stream":
			continue // server-side options htadb controls
		}
		if _, ok := spec.Options[k]; !ok {
			logf("ERROR", "Unknown server option: %s", k)
			return 1
		}
	}
	if msg, ok := s.FailVersions[server]; ok {
		logf("ERROR", "%s", msg)
		return 1
	}
	logf("INFO", "Device: [Fake] Fake Phone (Android 14)")
	if args["cleanup"] != "false" {
		// CleanUp.unlinkSelf(): the server deletes its classpath.
		e.Device.RemoveFile(cp)
	}
	scid, err := strconv.ParseUint(args["scid"], 16, 32)
	if err != nil {
		logf("ERROR", "bad scid")
		return 1
	}
	if args["tunnel_forward"] != "true" {
		logf("ERROR", "fake only supports tunnel_forward=true")
		return 1
	}
	video := args["video"] != "false"
	audio := args["audio"] != "false"
	control := args["control"] != "false"
	l := e.Device.ListenAbstract(fmt.Sprintf("scrcpy_%08x", scid))
	defer l.Close()
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	accept := func() (net.Conn, error) {
		c, err := l.AcceptTimeout(10 * time.Second)
		if err != nil {
			return nil, err
		}
		if len(conns) == 0 {
			c.Write([]byte{0}) // dummy byte
		}
		conns = append(conns, c)
		return c, nil
	}
	var vc, ac, cc net.Conn
	if video {
		if vc, err = accept(); err != nil {
			logf("ERROR", "accept video: %v", err)
			return 1
		}
	}
	if audio {
		if ac, err = accept(); err != nil {
			logf("ERROR", "accept audio: %v", err)
			return 1
		}
	}
	if control {
		if cc, err = accept(); err != nil {
			logf("ERROR", "accept control: %v", err)
			return 1
		}
	}
	name := make([]byte, 64)
	copy(name, "Fake Phone")
	conns[0].Write(name)
	l.Close()

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	if cc != nil {
		go func() {
			defer stop()
			buf := make([]byte, 0, 4096)
			tmp := make([]byte, 4096)
			for {
				n, err := cc.Read(tmp)
				if err != nil {
					return
				}
				buf = append(buf, tmp[:n]...)
				for len(buf) > 0 {
					ml, err := spec.MessageLen(buf)
					if err == profile.ErrIncomplete || (err == nil && ml > len(buf)) {
						break
					}
					if err != nil {
						logf("ERROR", "Controller error: %v", err)
						return
					}
					msg := append([]byte(nil), buf[:ml]...)
					buf = buf[ml:]
					s.mu.Lock()
					s.Controls = append(s.Controls, msg)
					s.mu.Unlock()
					switch msg[0] {
					case profile.GetClipboard:
						cc.Write(profile.EncodeDeviceClipboard(s.DeviceClipboard))
					case profile.SetClipboard:
						seq := binary.BigEndian.Uint64(msg[1:9])
						if seq != 0 {
							cc.Write(profile.EncodeDeviceAckClipboard(seq))
						}
					}
				}
			}
		}()
	}
	if ac != nil {
		go func() {
			if s.AudioUnavailable {
				ac.Write([]byte{0, 0, 0, 0})
				return
			}
			ac.Write(binary.BigEndian.AppendUint32(nil, profile.CodecID("opus")))
			ac.Write(profile.EncodeMediaHeader(0, true, false, 19))
			ac.Write([]byte("OpusHead\x01\x02\x38\x01\x80\xbb\x00\x00\x00\x00\x00"))
			for i := 0; ; i++ {
				pkt := []byte{0xfc, 0xff, 0xfe}
				if _, err := ac.Write(append(profile.EncodeMediaHeader(uint64(i)*20000, false, false, uint32(len(pkt))), pkt...)); err != nil {
					return
				}
				select {
				case <-done:
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}()
	}
	if vc == nil {
		<-done
		return 0
	}
	w, h := s.Width, s.Height
	if w == 0 {
		w, h = 1080, 2400
	}
	if ms, ok := args["max_size"]; ok {
		if n, _ := strconv.Atoi(ms); n > 0 && uint32(n) < h {
			w, h = w*uint32(n)/h&^7, uint32(n)
		}
	}
	vc.Write(binary.BigEndian.AppendUint32(nil, profile.CodecID("h264")))
	vc.Write(spec.EncodeSession(profile.SessionPacket{Width: w, Height: h}))
	cfg, frames := s.stream()
	fps := s.FPS
	if fps <= 0 {
		fps = 30
	}
	failAfter, hasFail := s.FailAfterStart[server]
	sent := 0
	write := func(b []byte) bool {
		vc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := vc.Write(b)
		return err == nil
	}
	if !write(append(profile.EncodeMediaHeader(0, true, false, uint32(len(cfg))), cfg...)) {
		return 0
	}
	start := time.Now()
	for i := 0; ; i++ {
		if len(frames) == 0 {
			break
		}
		if i >= len(frames) && !s.Loop {
			break
		}
		f := frames[i%len(frames)]
		if i > 0 && i%len(frames) == 0 && s.Loop {
			// New GOP: repeat config so decoders can (re)start anywhere.
			if !write(append(profile.EncodeMediaHeader(0, true, false, uint32(len(cfg))), cfg...)) {
				return 0
			}
		}
		pts := uint64(time.Since(start).Microseconds())
		if !write(append(profile.EncodeMediaHeader(pts, false, f.Key, uint32(len(f.Data))), f.Data...)) {
			return 0
		}
		sent++
		if hasFail && sent >= failAfter {
			logf("ERROR", "Encoding error: simulated crash")
			return 1
		}
		select {
		case <-done:
			return 0
		case <-time.After(time.Second / time.Duration(fps)):
		}
	}
	// Stream finished: keep the session open until the client leaves.
	io.Copy(io.Discard, vc)
	return 0
}
