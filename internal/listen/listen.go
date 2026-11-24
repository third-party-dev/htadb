// Package listen parses htadb's --listen specs and opens the listeners.
//
//	http://127.0.0.1:10443   plain HTTP (default)
//	https://0.0.0.0:10443    HTTPS with htadb's own certificate
//	unix:///run/htadb.sock   unix socket (mode from --unix-mode)
//	systemd:                 sockets passed by systemd socket activation
package listen

import (
	"crypto/tls"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Spec is one parsed --listen value.
type Spec struct {
	Raw    string
	Scheme string // "http", "https", "unix", "systemd"
	Addr   string // host:port for http(s), path for unix
	Name   string // systemd: optional FileDescriptorName filter
}

// Parse parses a listener spec.
func Parse(s string) (Spec, error) {
	sp := Spec{Raw: s}
	if s == "systemd:" || strings.HasPrefix(s, "systemd:") {
		sp.Scheme = "systemd"
		sp.Name = strings.TrimPrefix(strings.TrimPrefix(s, "systemd:"), "//")
		return sp, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return sp, fmt.Errorf("listen %q: %w", s, err)
	}
	sp.Scheme = u.Scheme
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return sp, fmt.Errorf("listen %q: missing host:port", s)
		}
		if u.Path != "" && u.Path != "/" {
			return sp, fmt.Errorf("listen %q: paths are not supported", s)
		}
		host, port, err := net.SplitHostPort(u.Host)
		if err != nil {
			return sp, fmt.Errorf("listen %q: %w", s, err)
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return sp, fmt.Errorf("listen %q: bad port", s)
		}
		sp.Addr = net.JoinHostPort(host, port)
	case "unix":
		p := u.Path
		if u.Host != "" { // unix://relative/path
			p = u.Host + u.Path
		}
		if p == "" {
			p = u.Opaque
		}
		if p == "" {
			return sp, fmt.Errorf("listen %q: missing socket path", s)
		}
		sp.Addr = p
	default:
		return sp, fmt.Errorf("listen %q: unsupported scheme %q (want http, https, unix or systemd)", s, u.Scheme)
	}
	return sp, nil
}

// IsLoopback reports whether an http(s) spec binds only loopback.
func (s Spec) IsLoopback() bool {
	if s.Scheme != "http" && s.Scheme != "https" {
		return false
	}
	host, _, _ := net.SplitHostPort(s.Addr)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IP returns the bound IP for http(s) specs (nil for wildcard or names).
func (s Spec) IP() net.IP {
	host, _, _ := net.SplitHostPort(s.Addr)
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return nil
	}
	return ip
}

// IsUnspecified reports a 0.0.0.0 / :: bind.
func (s Spec) IsUnspecified() bool {
	host, _, _ := net.SplitHostPort(s.Addr)
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// Options controls Open.
type Options struct {
	UnixMode fs.FileMode
	TLS      *tls.Config // required for https specs
	Logger   *slog.Logger
}

// Listener pairs a net.Listener with its spec and public URL.
type Listener struct {
	net.Listener
	Spec Spec
	URL  string // e.g. http://127.0.0.1:10443 (actual port when 0 was asked)
}

// Open opens the listener(s) for one spec. systemd specs may yield several.
func Open(s Spec, o Options) ([]*Listener, error) {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	switch s.Scheme {
	case "http", "https":
		ln, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return nil, err
		}
		u := s.Scheme + "://" + ln.Addr().String()
		if s.Scheme == "http" && !s.IsLoopback() {
			log.Warn("plain http listener on a non-loopback address: browsers reaching it directly are not in a secure context, so mirroring (WebCodecs) and clipboard sync need a TLS proxy in front", "listen", s.Raw)
		}
		var l net.Listener = ln
		if s.Scheme == "https" {
			if o.TLS == nil {
				ln.Close()
				return nil, fmt.Errorf("listen %s: no TLS configuration", s.Raw)
			}
			l = tls.NewListener(ln, o.TLS)
		}
		return []*Listener{{Listener: l, Spec: s, URL: u}}, nil
	case "unix":
		if fi, err := os.Lstat(s.Addr); err == nil && fi.Mode()&fs.ModeSocket != 0 {
			// Stale socket from an earlier run: remove only if nobody answers.
			if c, err := net.Dial("unix", s.Addr); err == nil {
				c.Close()
				return nil, fmt.Errorf("listen %s: socket is in use", s.Raw)
			}
			os.Remove(s.Addr)
		}
		ln, err := net.Listen("unix", s.Addr)
		if err != nil {
			return nil, err
		}
		if o.UnixMode != 0 && runtime.GOOS != "windows" {
			if err := os.Chmod(s.Addr, o.UnixMode); err != nil {
				ln.Close()
				return nil, err
			}
		}
		return []*Listener{{Listener: ln, Spec: s, URL: "unix://" + s.Addr}}, nil
	case "systemd":
		return systemdListeners(s)
	}
	return nil, fmt.Errorf("listen %s: unsupported", s.Raw)
}

// systemdListeners implements sd_listen_fds(3): fds start at 3.
func systemdListeners(s Spec) ([]*Listener, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("listen systemd: only supported on Linux")
	}
	pid, _ := strconv.Atoi(os.Getenv("LISTEN_PID"))
	n, _ := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if pid != os.Getpid() || n <= 0 {
		return nil, fmt.Errorf("listen systemd: no sockets passed (LISTEN_PID/LISTEN_FDS unset)")
	}
	names := strings.Split(os.Getenv("LISTEN_FDNAMES"), ":")
	var out []*Listener
	for i := 0; i < n; i++ {
		name := ""
		if i < len(names) {
			name = names[i]
		}
		if s.Name != "" && name != s.Name {
			continue
		}
		f := os.NewFile(uintptr(3+i), "systemd-"+name)
		ln, err := net.FileListener(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("listen systemd fd %d: %w", 3+i, err)
		}
		out = append(out, &Listener{Listener: ln, Spec: s, URL: "systemd:" + ln.Addr().String()})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("listen systemd: no matching sockets")
	}
	return out, nil
}

// NeedTLS reports whether any spec is an https listener.
func NeedTLS(specs []Spec) bool {
	for _, s := range specs {
		if s.Scheme == "https" {
			return true
		}
	}
	return false
}

// OpenAll opens every spec. TLS material is created or loaded only when at
// least one https listener is configured; otherwise TLS files are never
// touched.
func OpenAll(specs []Spec, unixMode fs.FileMode, tlsOpts TLSOptions, log *slog.Logger) ([]*Listener, error) {
	o := Options{UnixMode: unixMode, Logger: log}
	if NeedTLS(specs) {
		tlsOpts.ListenSpecs = specs
		if tlsOpts.Logger == nil {
			tlsOpts.Logger = log
		}
		cfg, err := TLSConfig(tlsOpts)
		if err != nil {
			return nil, err
		}
		o.TLS = cfg
	}
	var all []*Listener
	for _, s := range specs {
		ls, err := Open(s, o)
		if err != nil {
			for _, l := range all {
				l.Close()
			}
			return nil, err
		}
		all = append(all, ls...)
	}
	return all, nil
}
