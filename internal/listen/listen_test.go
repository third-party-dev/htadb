package listen

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in, scheme, addr string
		loop             bool
	}{
		{"http://127.0.0.1:10443", "http", "127.0.0.1:10443", true},
		{"https://0.0.0.0:10443", "https", "0.0.0.0:10443", false},
		{"http://0.0.0.0:443", "http", "0.0.0.0:443", false},
		{"http://[::1]:8080", "http", "[::1]:8080", true},
		{"http://localhost:1", "http", "localhost:1", true},
		{"unix:///run/htadb.sock", "unix", "/run/htadb.sock", false},
		{"unix://rel/htadb.sock", "unix", "rel/htadb.sock", false},
		{"systemd:", "systemd", "", false},
	}
	for _, c := range cases {
		s, err := Parse(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if s.Scheme != c.scheme || s.Addr != c.addr || s.IsLoopback() != c.loop {
			t.Errorf("%s: got %+v loop=%v", c.in, s, s.IsLoopback())
		}
	}
	for _, bad := range []string{"tcp://1.2.3.4:5", "http://nohost", "http://h:99999", "unix://", "http://h:1/path"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
}

func TestNoTLSWithoutHTTPS(t *testing.T) {
	dir := t.TempDir()
	tlsDir := filepath.Join(dir, "tls")
	s1, _ := Parse("http://127.0.0.1:0")
	s2, _ := Parse("unix://" + filepath.Join(dir, "h.sock"))
	ls, err := OpenAll([]Spec{s1, s2}, 0o660, TLSOptions{Dir: tlsDir}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		l.Close()
	}
	if _, err := os.Stat(tlsDir); !os.IsNotExist(err) {
		t.Fatalf("TLS dir touched without https listener: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "h.sock"))
	if err == nil && fi.Mode().Perm() != 0o660 {
		t.Errorf("unix mode %v", fi.Mode().Perm())
	}
}

func TestStaleUnixSocket(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.sock")
	s, _ := Parse("unix://" + p)
	ls, err := Open(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// In use: refuse.
	if _, err := Open(s, Options{}); err == nil {
		t.Fatal("expected in-use error")
	}
	ls[0].Listener.(*net.UnixListener).SetUnlinkOnClose(false)
	ls[0].Close()
	// Stale: replace.
	ls, err = Open(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ls[0].Close()
}

func TestHTTPSAndRenewal(t *testing.T) {
	dir := t.TempDir()
	s, _ := Parse("https://127.0.0.1:0")
	opts := TLSOptions{Dir: dir}
	ls, err := OpenAll([]Spec{s}, 0, opts, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer ls[0].Close()
	for _, f := range []string{CAFile, CAKeyFile, LeafFile, LeafKeyFile} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, fi.Mode(), err)
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode())
	}
	go http.Serve(ls[0], http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))

	caPEM, _ := os.ReadFile(filepath.Join(dir, CAFile))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := cl.Get(ls[0].URL)
	if err != nil {
		t.Fatalf("verified TLS request failed: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ok" {
		t.Fatal(string(b))
	}

	caBefore, _ := os.ReadFile(filepath.Join(dir, CAFile))
	leafBefore, _ := os.ReadFile(filepath.Join(dir, LeafFile))

	// Same SANs: no regeneration.
	opts.ListenSpecs = []Spec{s}
	if regen, err := EnsureLeaf(opts); err != nil || regen {
		t.Fatalf("unexpected regen=%v err=%v", regen, err)
	}
	// SAN change: new leaf, same CA.
	opts.ExtraSANs = []string{"htadb.example.test", "10.9.8.7"}
	if regen, err := EnsureLeaf(opts); err != nil || !regen {
		t.Fatalf("expected regen on SAN change: %v %v", regen, err)
	}
	leafAfter, _ := os.ReadFile(filepath.Join(dir, LeafFile))
	caAfter, _ := os.ReadFile(filepath.Join(dir, CAFile))
	if string(leafAfter) == string(leafBefore) {
		t.Fatal("leaf unchanged")
	}
	if string(caAfter) != string(caBefore) {
		t.Fatal("CA was not reused")
	}
	blk, _ := pem.Decode(leafAfter)
	leaf, _ := x509.ParseCertificate(blk.Bytes)
	found := false
	for _, d := range leaf.DNSNames {
		found = found || d == "htadb.example.test"
	}
	if !found || len(leaf.IPAddresses) < 3 {
		t.Fatalf("SANs %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	// Near expiry: regenerate.
	opts.Now = func() time.Time { return time.Now().AddDate(0, 0, 340) }
	if regen, _ := EnsureLeaf(opts); !regen {
		t.Fatal("expected renewal near expiry")
	}
	// Force.
	opts.Now = nil
	opts.Force = true
	if regen, _ := EnsureLeaf(opts); !regen {
		t.Fatal("force")
	}
	if _, fp, err := CAInfo(dir); err != nil || len(fp) != 95 {
		t.Fatal(fp, err)
	}
}

// TestSystemd passes a listening socket as fd 3 to a child process, like
// systemd socket activation, and checks the child serves on it.
func TestSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd activation is Linux-only")
	}
	if os.Getenv("HTADB_SYSTEMD_CHILD") == "1" {
		os.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
		s, _ := Parse("systemd:")
		ls, err := Open(s, Options{})
		if err != nil {
			fmt.Println("child error:", err)
			os.Exit(1)
		}
		c, err := ls[0].Accept()
		if err == nil {
			c.Write([]byte("served by fd 3"))
			c.Close()
		}
		os.Exit(0)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := ln.(*net.TCPListener).File()
	addr := ln.Addr().String()
	ln.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSystemd$")
	cmd.Env = append(os.Environ(), "HTADB_SYSTEMD_CHILD=1", "LISTEN_FDS=1", "LISTEN_FDNAMES=http")
	cmd.ExtraFiles = []*os.File{f}
	out := &strings.Builder{}
	cmd.Stdout = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(c)
	cmd.Wait()
	if string(b) != "served by fd 3" {
		t.Fatalf("got %q; child output %q", b, out.String())
	}
	// Without LISTEN_FDS the spec is an error.
	s, _ := Parse("systemd:")
	if _, err := Open(s, Options{}); err == nil {
		t.Fatal("expected error without LISTEN_FDS")
	}
}
