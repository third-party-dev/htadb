package adb_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"htadb/internal/adb"
	"htadb/internal/adb/adbtest"
)

func setup(t *testing.T) (*adbtest.Server, *adb.Client, *adbtest.Device) {
	t.Helper()
	srv, err := adbtest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	d := adbtest.NewDevice("EMU123")
	srv.AddDevice(d)
	return srv, adb.New(srv.Addr()), d
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestFraming(t *testing.T) {
	_, c, _ := setup(t)
	v, err := c.ServerVersion(ctx(t))
	if err != nil || v != 41 {
		t.Fatalf("version = %d, %v", v, err)
	}
	_, err = c.HostQuery(ctx(t), "host:nonsense")
	if !adb.IsFail(err) || !strings.Contains(err.Error(), "unknown host service") {
		t.Fatalf("want FAIL, got %v", err)
	}
}

func TestParseDevices(t *testing.T) {
	in := "R58M12345              device usb:1-1 product:beyond1 model:SM_G973F device:beyond1 transport_id:3\n" +
		"emulator-5554          offline transport_id:4\n" +
		"0123456789ABCDEF       no permissions (missing udev rules?); see [http://developer.android.com/tools/device.html] usb:1-2 transport_id:5\n" +
		"192.168.1.5:5555       unauthorized transport_id:6\n"
	ds := adb.ParseDevices(in)
	if len(ds) != 4 {
		t.Fatalf("got %d devices: %+v", len(ds), ds)
	}
	byS := map[string]adb.Device{}
	for _, d := range ds {
		byS[d.Serial] = d
	}
	r := byS["R58M12345"]
	if r.State != "device" || r.Model != "SM_G973F" || r.Product != "beyond1" || r.DeviceName != "beyond1" || r.TransportID != "3" || r.USB != "1-1" {
		t.Errorf("bad parse: %+v", r)
	}
	if byS["emulator-5554"].State != "offline" {
		t.Errorf("offline: %+v", byS["emulator-5554"])
	}
	if s := byS["0123456789ABCDEF"].State; !strings.HasPrefix(s, "no permissions") {
		t.Errorf("state with spaces: %q", s)
	}
	if byS["192.168.1.5:5555"].State != "unauthorized" {
		t.Errorf("unauthorized: %+v", byS["192.168.1.5:5555"])
	}
}

func TestDiffDevices(t *testing.T) {
	a := []adb.Device{{Serial: "a", State: "device"}, {Serial: "b", State: "offline"}}
	b := []adb.Device{{Serial: "b", State: "device"}, {Serial: "c", State: "device"}}
	ev := adb.DiffDevices(a, b)
	got := map[string]string{}
	for _, e := range ev {
		got[e.Device.Serial] = e.Type
	}
	if got["a"] != "remove" || got["b"] != "state" || got["c"] != "add" || len(ev) != 3 {
		t.Fatalf("diff = %+v", ev)
	}
}

func TestTrackDevices(t *testing.T) {
	srv, c, _ := setup(t)
	cx, cancel := context.WithCancel(ctx(t))
	defer cancel()
	lists := make(chan []adb.Device, 10)
	go c.TrackDevices(cx, func(d []adb.Device) error { lists <- d; return nil })
	first := <-lists
	if len(first) != 1 || first[0].Serial != "EMU123" {
		t.Fatalf("first = %+v", first)
	}
	srv.AddDevice(adbtest.NewDevice("NEW1"))
	select {
	case l := <-lists:
		if len(l) != 2 {
			t.Fatalf("after add: %+v", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no update")
	}
}

func TestShellV2(t *testing.T) {
	_, c, _ := setup(t)
	r, err := c.RunShell(ctx(t), "EMU123", "echo hello; nosuchcmd")
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Stdout) != "hello\n" || !strings.Contains(string(r.Stderr), "not found") || r.ExitCode != 127 {
		t.Fatalf("result = %q %q %d", r.Stdout, r.Stderr, r.ExitCode)
	}
	r, err = c.RunShellInput(ctx(t), "EMU123", "cat", strings.NewReader("piped"))
	if err != nil || string(r.Stdout) != "piped" || r.ExitCode != 0 {
		t.Fatalf("stdin: %q %d %v", r.Stdout, r.ExitCode, err)
	}
}

func TestShellService(t *testing.T) {
	got := adb.ShellService(adb.ShellOptions{Command: "ls", PTY: true, Term: "xterm-256color"}, true)
	if got != "shell,v2,pty,TERM=xterm-256color:ls" {
		t.Error(got)
	}
	if got := adb.ShellService(adb.ShellOptions{Command: "ls"}, true); got != "shell,v2,raw:ls" {
		t.Error(got)
	}
	if got := adb.ShellService(adb.ShellOptions{Command: "ls", PTY: true}, false); got != "shell:ls" {
		t.Error(got)
	}
}

func TestShellPacketFraming(t *testing.T) {
	b := adb.EncodeShellPacket(adb.ShellStdin, []byte("ab"))
	if !bytes.Equal(b, []byte{0, 2, 0, 0, 0, 'a', 'b'}) {
		t.Fatalf("% x", b)
	}
}

func TestShellPTYResize(t *testing.T) {
	_, c, d := setup(t)
	s, err := c.OpenShell(ctx(t), "EMU123", adb.ShellOptions{PTY: true, Term: "xterm", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Resize(30, 100)
	s.Write([]byte("echo hi\n"))
	var out bytes.Buffer
	for !strings.Contains(out.String(), "hi\n") {
		id, data, err := s.ReadPacket()
		if err != nil {
			t.Fatal(err, out.String())
		}
		if id == adb.ShellStdout {
			out.Write(data)
		}
	}
	s.Write([]byte("exit\n"))
	for {
		id, data, err := s.ReadPacket()
		if err != nil {
			t.Fatal(err)
		}
		if id == adb.ShellExit {
			if data[0] != 0 {
				t.Fatalf("exit %d", data[0])
			}
			break
		}
	}
	if len(d.Resizes) != 2 || d.Resizes[0] != "24x80,0x0" || d.Resizes[1] != "30x100,0x0" {
		t.Fatalf("resizes %v", d.Resizes)
	}
}

func TestLegacyShell(t *testing.T) {
	srv, c, _ := setup(t)
	old := adbtest.NewDevice("OLD")
	old.Features = nil
	srv.AddDevice(old)
	r, err := c.RunShell(ctx(t), "OLD", "echo legacy")
	if err != nil || string(r.Stdout) != "legacy\n" || r.ExitCode != -1 {
		t.Fatalf("%q %d %v", r.Stdout, r.ExitCode, err)
	}
}

func TestSync(t *testing.T) {
	_, c, d := setup(t)
	data := bytes.Repeat([]byte("0123456789"), 20000) // > 64k, multiple chunks
	n, err := c.Push(ctx(t), "EMU123", "/sdcard/x/big.bin", 0o640, bytes.NewReader(data))
	if err != nil || n != int64(len(data)) {
		t.Fatal(n, err)
	}
	got, _ := d.ReadFile("/sdcard/x/big.bin")
	if !bytes.Equal(got, data) {
		t.Fatal("push content mismatch")
	}
	st, err := c.Stat(ctx(t), "EMU123", "/sdcard/x/big.bin")
	if err != nil || st.Size != uint32(len(data)) || st.Type != "file" || st.Mode&0o777 != 0o640 {
		t.Fatalf("stat %+v %v", st, err)
	}
	st, _ = c.Stat(ctx(t), "EMU123", "/nope")
	if st.Exists() {
		t.Fatal("missing file exists")
	}
	ls, err := c.List(ctx(t), "EMU123", "/sdcard")
	if err != nil || len(ls) != 1 || ls[0].Name != "x" || !ls[0].IsDir() {
		t.Fatalf("list %+v %v", ls, err)
	}
	var buf bytes.Buffer
	if _, err := c.Pull(ctx(t), "EMU123", "/sdcard/x/big.bin", &buf); err != nil || !bytes.Equal(buf.Bytes(), data) {
		t.Fatal("pull", err)
	}
	_, err = c.Pull(ctx(t), "EMU123", "/nope", io.Discard)
	if !adb.IsFail(err) {
		t.Fatalf("pull missing: %v", err)
	}
	_, err = c.Push(ctx(t), "EMU123", "/system/x", 0o644, strings.NewReader("x"))
	if !adb.IsFail(err) || !strings.Contains(err.Error(), "Read-only") {
		t.Fatalf("push ro: %v", err)
	}
	// The session survives several requests.
	s, _ := c.OpenSync(ctx(t), "EMU123")
	defer s.Close()
	for i := 0; i < 3; i++ {
		if _, err := s.Stat("/sdcard"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstall(t *testing.T) {
	_, c, d := setup(t)
	apk := append([]byte("PK"), bytes.Repeat([]byte{1}, 1000)...)
	r, err := c.Install(ctx(t), "EMU123", bytes.NewReader(apk), int64(len(apk)), []string{"-r"})
	if err != nil || !r.Success {
		t.Fatal(r, err)
	}
	if d.ServiceCount("exec:cmd package install -S 1002 -r") != 1 {
		t.Fatal(d.Services)
	}
	_, err = c.Install(ctx(t), "EMU123", strings.NewReader("notanapk"), 8, nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	r, err = c.Uninstall(ctx(t), "EMU123", "com.x", true)
	if err != nil || !r.Success {
		t.Fatal(r, err)
	}
}

func TestForwards(t *testing.T) {
	_, c, _ := setup(t)
	if _, err := c.AddForward(ctx(t), "EMU123", "tcp:6000", "localabstract:foo", false); err != nil {
		t.Fatal(err)
	}
	port, err := c.AddForward(ctx(t), "EMU123", "tcp:0", "tcp:7000", false)
	if err != nil || port != "34567" {
		t.Fatal(port, err)
	}
	fs, err := c.ListForwards(ctx(t), "EMU123")
	if err != nil || len(fs) != 2 || fs[0].Local != "tcp:6000" {
		t.Fatal(fs, err)
	}
	if err := c.RemoveForward(ctx(t), "EMU123", "tcp:6000"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveForward(ctx(t), "EMU123", "tcp:6000"); !adb.IsFail(err) {
		t.Fatal("double remove should fail", err)
	}
	if _, err := c.AddReverse(ctx(t), "EMU123", "tcp:8000", "tcp:9000", false); err != nil {
		t.Fatal(err)
	}
	rs, err := c.ListReverses(ctx(t), "EMU123")
	if err != nil || len(rs) != 1 || rs[0].Local != "tcp:8000" || rs[0].Remote != "tcp:9000" {
		t.Fatal(rs, err)
	}
	if err := c.RemoveReverse(ctx(t), "EMU123", ""); err != nil {
		t.Fatal(err)
	}
	rs, _ = c.ListReverses(ctx(t), "EMU123")
	if len(rs) != 0 {
		t.Fatal(rs)
	}
}

func TestDeviceNotFound(t *testing.T) {
	_, c, _ := setup(t)
	_, err := c.RunShell(ctx(t), "NOPE", "true")
	if !adb.IsDeviceNotFound(err) {
		t.Fatalf("%v", err)
	}
}

func TestLocalAbstract(t *testing.T) {
	_, c, d := setup(t)
	if _, err := c.LocalAbstract(ctx(t), "EMU123", "nobody"); !adb.IsFail(err) {
		t.Fatal("expected FAIL with no listener", err)
	}
	l := d.ListenAbstract("echo")
	defer l.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s, err := l.Accept()
		if err != nil {
			return
		}
		io.Copy(s, s)
		s.Close()
	}()
	conn, err := c.LocalAbstract(ctx(t), "EMU123", "echo")
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("ping"))
	b := make([]byte, 4)
	io.ReadFull(conn, b)
	if string(b) != "ping" {
		t.Fatal(string(b))
	}
	conn.Close()
	wg.Wait()
}

func TestGetprop(t *testing.T) {
	m := adb.ParseGetprop("[a]: [1]\n[multi]: [line1\nline2]\n[b]: []\n")
	if m["a"] != "1" || m["multi"] != "line1\nline2" || m["b"] != "" || len(m) != 3 {
		t.Fatalf("%q", m)
	}
	_, c, _ := setup(t)
	p, err := c.GetProps(ctx(t), "EMU123")
	if err != nil || p["ro.build.version.sdk"] != "34" {
		t.Fatal(p, err)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"abc": "abc", "a b": "'a b'", "it's": `'it'\''s'`, "": "''", "/data/x.jar": "/data/x.jar"} {
		if got := adb.ShellQuote(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}
