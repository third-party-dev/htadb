package adbtest

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ShellEnv is passed to fake shell commands.
type ShellEnv struct {
	Device *Device
	Args   []string          // argv, Args[0] is the command
	Env    map[string]string // VAR=value prefixes
	PTY    bool
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// CommandFunc implements one fake shell command and returns its exit code.
type CommandFunc func(e *ShellEnv) int

// File is a fake file.
type File struct {
	Data  []byte
	Mode  uint32 // permission bits
	Mtime time.Time
}

// Device is a fake device.
type Device struct {
	Serial     string
	State      string
	Product    string
	Model      string
	DeviceName string
	Features   []string
	Props      map[string]string

	// Commands extends or overrides the built-in fake shell commands.
	Commands map[string]CommandFunc
	// Resizes records PTY window-size packets.
	Resizes []string

	mu       sync.Mutex
	files    map[string]*File
	dirs     map[string]bool
	abstract map[string]*AbstractListener
	reverses []string
	srv      *Server
	// Services records every device service requested.
	Services []string
}

// NewDevice returns an online device with shell_v2 and cmd features.
func NewDevice(serial string) *Device {
	return &Device{
		Serial: serial, State: "device", Product: "fakeprod", Model: "Fake_Phone", DeviceName: "fake",
		Features: []string{"shell_v2", "cmd", "stat_v2", "ls_v2", "abb_exec"},
		Props: map[string]string{
			"ro.build.version.sdk":     "34",
			"ro.build.version.release": "14",
			"ro.product.model":         "Fake Phone",
			"ro.product.manufacturer":  "Fake",
			"ro.serialno":              serial,
		},
		Commands: map[string]CommandFunc{},
		files:    map[string]*File{},
		dirs:     map[string]bool{"/": true, "/data": true, "/data/local": true, "/data/local/tmp": true, "/sdcard": true},
		abstract: map[string]*AbstractListener{},
	}
}

// WriteFile stores a file (creating parent directories).
func (d *Device) WriteFile(p string, data []byte, mode uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writeFileLocked(p, data, mode)
}

func (d *Device) writeFileLocked(p string, data []byte, mode uint32) {
	p = path.Clean(p)
	for dir := path.Dir(p); dir != "/" && dir != "."; dir = path.Dir(dir) {
		d.dirs[dir] = true
	}
	d.files[p] = &File{Data: append([]byte(nil), data...), Mode: mode & 0o777, Mtime: time.Now()}
}

// RemoveFile deletes a file (like unlink: hard links keep the data).
func (d *Device) RemoveFile(p string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.files, path.Clean(p))
}

// ReadFile returns a file's content.
func (d *Device) ReadFile(p string) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[path.Clean(p)]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), f.Data...), true
}

// Files lists all file paths (sorted).
func (d *Device) Files() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for p := range d.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ServiceCount counts requested services with the given prefix.
func (d *Device) ServiceCount(prefix string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, s := range d.Services {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

func (d *Device) hasFeature(f string) bool {
	for _, x := range d.Features {
		if x == f {
			return true
		}
	}
	return false
}

// ---- abstract sockets ----

// AbstractListener accepts connections to a localabstract socket name.
type AbstractListener struct {
	name   string
	dev    *Device
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

// ListenAbstract starts listening on localabstract:name.
func (d *Device) ListenAbstract(name string) *AbstractListener {
	l := &AbstractListener{name: name, dev: d, ch: make(chan net.Conn), closed: make(chan struct{})}
	d.mu.Lock()
	d.abstract[name] = l
	d.mu.Unlock()
	return l
}

// Accept waits for a connection.
func (l *AbstractListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// AcceptTimeout is Accept with a deadline.
func (l *AbstractListener) AcceptTimeout(t time.Duration) (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-time.After(t):
		return nil, fmt.Errorf("accept timeout")
	}
}

// Close stops listening.
func (l *AbstractListener) Close() {
	l.once.Do(func() {
		close(l.closed)
		l.dev.mu.Lock()
		if l.dev.abstract[l.name] == l {
			delete(l.dev.abstract, l.name)
		}
		l.dev.mu.Unlock()
	})
}

// ---- device services ----

func (d *Device) serve(c stream, svc string) {
	d.mu.Lock()
	d.Services = append(d.Services, svc)
	d.mu.Unlock()
	switch {
	case strings.HasPrefix(svc, "shell,") || strings.HasPrefix(svc, "shell:"):
		d.serveShell(c, svc)
	case strings.HasPrefix(svc, "exec:"):
		okay(c)
		cmd := strings.TrimPrefix(svc, "exec:")
		var errb bytes.Buffer
		d.run(cmd, false, c, c, &errb)
	case svc == "sync:":
		okay(c)
		d.serveSync(c)
	case strings.HasPrefix(svc, "localabstract:"):
		name := strings.TrimPrefix(svc, "localabstract:")
		d.mu.Lock()
		l := d.abstract[name]
		d.mu.Unlock()
		if l == nil {
			fail(c, "closed")
			return
		}
		a, b := net.Pipe()
		select {
		case l.ch <- a:
		case <-l.closed:
			fail(c, "closed")
			return
		case <-time.After(2 * time.Second):
			fail(c, "closed")
			return
		}
		okay(c)
		done := make(chan struct{}, 2)
		go func() { io.Copy(b, c); b.Close(); done <- struct{}{} }()
		go func() { io.Copy(c, b); c.Close(); done <- struct{}{} }()
		<-done
	case strings.HasPrefix(svc, "reboot:"):
		okay(c)
		if d.srv != nil {
			go func() {
				d.srv.SetState(d.Serial, "offline")
				time.Sleep(50 * time.Millisecond)
				d.srv.SetState(d.Serial, "device")
			}()
		}
	case svc == "root:":
		okay(c)
		io.WriteString(c, "restarting adbd as root\n")
	case svc == "unroot:":
		okay(c)
		io.WriteString(c, "restarting adbd as non root\n")
	case svc == "remount:":
		okay(c)
		io.WriteString(c, "remount succeeded\n")
	case strings.HasPrefix(svc, "reverse:"):
		d.serveReverse(c, strings.TrimPrefix(svc, "reverse:"))
	default:
		fail(c, "unknown service "+svc)
	}
}

func (d *Device) serveReverse(c stream, cmd string) {
	switch {
	case cmd == "list-forward":
		okay(c)
		d.mu.Lock()
		var b strings.Builder
		for _, r := range d.reverses {
			b.WriteString("UsbFfs " + r + "\n")
		}
		d.mu.Unlock()
		hexStr(c, b.String())
	case strings.HasPrefix(cmd, "forward:"):
		spec := strings.TrimPrefix(strings.TrimPrefix(cmd, "forward:"), "norebind:")
		remote, local, ok := strings.Cut(spec, ";")
		if !ok {
			fail(c, "bad spec")
			return
		}
		okay(c)
		d.mu.Lock()
		d.reverses = append(d.reverses, remote+" "+local)
		d.mu.Unlock()
		okay(c)
	case cmd == "killforward-all" || strings.HasPrefix(cmd, "killforward:"):
		remote := strings.TrimPrefix(cmd, "killforward:")
		okay(c)
		d.mu.Lock()
		var keep []string
		for _, r := range d.reverses {
			if cmd == "killforward-all" || strings.Fields(r)[0] == remote {
				continue
			}
			keep = append(keep, r)
		}
		d.reverses = keep
		d.mu.Unlock()
		okay(c)
	default:
		fail(c, "unknown reverse service")
	}
}

func (d *Device) serveShell(c stream, svc string) {
	head, cmd, _ := strings.Cut(svc, ":")
	opts := strings.Split(head, ",")[1:]
	v2, pty := false, false
	for _, o := range opts {
		switch o {
		case "v2":
			v2 = true
		case "pty":
			pty = true
		}
	}
	if v2 && !d.hasFeature("shell_v2") {
		fail(c, "shell protocol not supported")
		return
	}
	if !v2 {
		pty = cmd == ""
	}
	okay(c)
	if !v2 {
		var errb bytes.Buffer
		if cmd == "" {
			d.interactive(c, c, c)
			return
		}
		d.run(cmd, pty, c, c, &errb)
		c.Write(errb.Bytes())
		return
	}
	var wmu sync.Mutex
	stdout := v2Writer{mu: &wmu, w: c, id: 1}
	stderr := v2Writer{mu: &wmu, w: c, id: 2}
	if pty {
		stderr = stdout
	}
	in := &v2Reader{r: c, onResize: func(s string) {
		d.mu.Lock()
		d.Resizes = append(d.Resizes, s)
		d.mu.Unlock()
	}}
	var code int
	if cmd == "" {
		code = d.interactive(in, stdout, stdout)
	} else {
		code = d.run(cmd, pty, in, stdout, stderr)
	}
	v2Writer{mu: &wmu, w: c, id: 3}.Write([]byte{byte(code)})
	waitClosed(c, 2*time.Second)
}

// readLine reads up to \r or \n (a PTY maps the terminal's Enter, \r).
func readLine(br *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		c, err := br.ReadByte()
		if err != nil {
			return b.String(), err
		}
		if c == '\r' || c == '\n' {
			return b.String(), nil
		}
		b.WriteByte(c)
	}
}

// interactive is a tiny line-oriented shell with prompt and echo.
func (d *Device) interactive(in io.Reader, out, errw io.Writer) int {
	br := bufio.NewReader(in)
	for {
		io.WriteString(out, d.DeviceName+":/ $ ")
		line, err := readLine(br)
		if line != "" || err == nil {
			io.WriteString(out, line+"\r\n")
		}
		if strings.TrimSpace(line) == "exit" {
			return 0
		}
		if strings.TrimSpace(line) != "" {
			d.run(line, true, strings.NewReader(""), out, errw)
		}
		if err != nil {
			return 0
		}
	}
}

// Tokenize splits a command line with sh-like single/double quoting.
func Tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	in := false
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			} else if ch == '\\' && quote == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(ch)
			}
		case ch == '\'' || ch == '"':
			quote = ch
			in = true
		case ch == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			in = true
		case ch == ' ' || ch == '\t' || ch == '\n':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(ch)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// splitList splits on top-level ';' and "&&" (quotes respected).
func splitList(s string) (parts []string, ops []string) {
	quote := byte(0)
	start := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch {
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == ';':
			parts = append(parts, s[start:i])
			ops = append(ops, ";")
			start = i + 1
		case ch == '&' && i+1 < len(s) && s[i+1] == '&':
			parts = append(parts, s[start:i])
			ops = append(ops, "&&")
			start = i + 2
			i++
		case ch == '|' && i+1 < len(s) && s[i+1] == '|':
			parts = append(parts, s[start:i])
			ops = append(ops, "||")
			start = i + 2
			i++
		}
	}
	parts = append(parts, s[start:])
	return
}

func (d *Device) run(cmdline string, pty bool, stdin io.Reader, stdout, stderr io.Writer) int {
	parts, ops := splitList(cmdline)
	code := 0
	for i, p := range parts {
		if i > 0 && ops[i-1] == "&&" && code != 0 {
			continue
		}
		if i > 0 && ops[i-1] == "||" && code == 0 {
			continue
		}
		var toks []string
		for _, t := range Tokenize(p) {
			if t == "2>/dev/null" || t == ">/dev/null" || t == "2>&1" {
				continue
			}
			toks = append(toks, t)
		}
		env := map[string]string{}
		for len(toks) > 0 && strings.Contains(toks[0], "=") && !strings.HasPrefix(toks[0], "=") {
			k, v, _ := strings.Cut(toks[0], "=")
			env[k] = v
			toks = toks[1:]
		}
		if len(toks) == 0 {
			continue
		}
		e := &ShellEnv{Device: d, Args: toks, Env: env, PTY: pty, Stdin: stdin, Stdout: stdout, Stderr: stderr}
		if f := d.Commands[toks[0]]; f != nil {
			code = f(e)
		} else if f := builtins[toks[0]]; f != nil {
			code = f(e)
		} else {
			fmt.Fprintf(stderr, "/system/bin/sh: %s: inaccessible or not found\n", toks[0])
			code = 127
		}
	}
	return code
}

var builtins map[string]CommandFunc

func init() {
	builtins = map[string]CommandFunc{
		"true":  func(*ShellEnv) int { return 0 },
		"false": func(*ShellEnv) int { return 1 },
		"exit": func(e *ShellEnv) int {
			if len(e.Args) > 1 {
				n, _ := strconv.Atoi(e.Args[1])
				return n
			}
			return 0
		},
		"echo": func(e *ShellEnv) int {
			fmt.Fprintln(e.Stdout, strings.Join(e.Args[1:], " "))
			return 0
		},
		"sleep": func(e *ShellEnv) int {
			if len(e.Args) > 1 {
				f, _ := strconv.ParseFloat(e.Args[1], 64)
				time.Sleep(time.Duration(f * float64(time.Second)))
			}
			return 0
		},
		"getprop": func(e *ShellEnv) int {
			d := e.Device
			if len(e.Args) > 1 {
				fmt.Fprintln(e.Stdout, d.Props[e.Args[1]])
				return 0
			}
			var keys []string
			for k := range d.Props {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(e.Stdout, "[%s]: [%s]\n", k, d.Props[k])
			}
			return 0
		},
		"cat": func(e *ShellEnv) int {
			if len(e.Args) == 1 {
				io.Copy(e.Stdout, e.Stdin)
				return 0
			}
			for _, p := range e.Args[1:] {
				b, ok := e.Device.ReadFile(p)
				if !ok {
					fmt.Fprintf(e.Stderr, "cat: %s: No such file or directory\n", p)
					return 1
				}
				e.Stdout.Write(b)
			}
			return 0
		},
		"sha256sum": func(e *ShellEnv) int {
			code := 0
			for _, p := range e.Args[1:] {
				b, ok := e.Device.ReadFile(p)
				if !ok {
					fmt.Fprintf(e.Stderr, "sha256sum: %s: No such file or directory\n", p)
					code = 1
					continue
				}
				h := sha256.Sum256(b)
				fmt.Fprintf(e.Stdout, "%s  %s\n", hex.EncodeToString(h[:]), p)
			}
			return code
		},
		"mkdir": func(e *ShellEnv) int {
			d := e.Device
			d.mu.Lock()
			defer d.mu.Unlock()
			for _, p := range e.Args[1:] {
				if strings.HasPrefix(p, "-") {
					continue
				}
				for dir := path.Clean(p); dir != "/" && dir != "."; dir = path.Dir(dir) {
					d.dirs[dir] = true
				}
			}
			return 0
		},
		"rm": func(e *ShellEnv) int {
			d := e.Device
			d.mu.Lock()
			defer d.mu.Unlock()
			for _, p := range e.Args[1:] {
				if !strings.HasPrefix(p, "-") {
					delete(d.files, path.Clean(p))
				}
			}
			return 0
		},
		"mv": func(e *ShellEnv) int {
			var args []string
			for _, a := range e.Args[1:] {
				if !strings.HasPrefix(a, "-") {
					args = append(args, a)
				}
			}
			if len(args) != 2 {
				fmt.Fprintln(e.Stderr, "mv: need 2 args")
				return 1
			}
			d := e.Device
			d.mu.Lock()
			defer d.mu.Unlock()
			f, ok := d.files[path.Clean(args[0])]
			if !ok {
				fmt.Fprintf(e.Stderr, "mv: bad '%s': No such file or directory\n", args[0])
				return 1
			}
			delete(d.files, path.Clean(args[0]))
			d.files[path.Clean(args[1])] = f
			return 0
		},
		"ln": func(e *ShellEnv) int { return copyFile(e, "ln") },
		"cp": func(e *ShellEnv) int { return copyFile(e, "cp") },
		"screencap": func(e *ShellEnv) int {
			e.Stdout.Write(FakePNG)
			return 0
		},
		"cmd": func(e *ShellEnv) int {
			// cmd package install -S <size> [args]
			if len(e.Args) >= 5 && e.Args[1] == "package" && e.Args[2] == "install" && e.Args[3] == "-S" {
				n, _ := strconv.ParseInt(e.Args[4], 10, 64)
				b, _ := io.ReadAll(io.LimitReader(e.Stdin, n))
				if int64(len(b)) != n {
					fmt.Fprintln(e.Stdout, "Failure [INSTALL_FAILED_INVALID_APK: short read]")
					return 1
				}
				if !bytes.HasPrefix(b, []byte("PK")) {
					fmt.Fprintln(e.Stdout, "Failure [INSTALL_PARSE_FAILED_NOT_APK]")
					return 1
				}
				e.Device.WriteFile("/data/app/installed-"+strconv.Itoa(len(b))+".apk", b, 0o644)
				fmt.Fprintln(e.Stdout, "Success")
				return 0
			}
			fmt.Fprintln(e.Stderr, "cmd: unsupported")
			return 1
		},
		"pm": func(e *ShellEnv) int {
			if len(e.Args) >= 3 && e.Args[1] == "uninstall" {
				pkg := e.Args[len(e.Args)-1]
				if pkg == "com.missing" {
					fmt.Fprintln(e.Stdout, "Failure [DELETE_FAILED_INTERNAL_ERROR]")
					return 1
				}
				fmt.Fprintln(e.Stdout, "Success")
				return 0
			}
			if len(e.Args) >= 3 && e.Args[1] == "install" {
				fmt.Fprintln(e.Stdout, "Success")
				return 0
			}
			return 1
		},
	}
}

func copyFile(e *ShellEnv, name string) int {
	var args []string
	for _, a := range e.Args[1:] {
		if !strings.HasPrefix(a, "-") {
			args = append(args, a)
		}
	}
	if len(args) != 2 {
		fmt.Fprintf(e.Stderr, "%s: need 2 args\n", name)
		return 1
	}
	d := e.Device
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[path.Clean(args[0])]
	if !ok {
		fmt.Fprintf(e.Stderr, "%s: %s: No such file or directory\n", name, args[0])
		return 1
	}
	cp := *f
	if name == "ln" {
		d.files[path.Clean(args[1])] = f // hard link: same inode
	} else {
		d.files[path.Clean(args[1])] = &cp
	}
	return 0
}

// FakePNG is a 1x1 PNG returned by screencap -p.
var FakePNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x60, 0x00, 0x02, 0x00,
	0x00, 0x05, 0x00, 0x01, 0xe9, 0xfa, 0xdc, 0xd8, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44,
	0xae, 0x42, 0x60, 0x82,
}

// ---- sync ----

func (d *Device) stat(p string) (mode, size, mtime uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p = path.Clean(p)
	if f, ok := d.files[p]; ok {
		return 0o100000 | f.Mode, uint32(len(f.Data)), uint32(f.Mtime.Unix())
	}
	if d.dirs[p] {
		return 0o040755, 4096, 0
	}
	pre := strings.TrimSuffix(p, "/") + "/"
	for f := range d.files {
		if strings.HasPrefix(f, pre) {
			return 0o040755, 4096, 0
		}
	}
	return 0, 0, 0
}

func (d *Device) list(p string) []struct {
	name              string
	mode, size, mtime uint32
} {
	p = path.Clean(p)
	pre := strings.TrimSuffix(p, "/") + "/"
	seen := map[string]bool{}
	d.mu.Lock()
	var names []string
	for f := range d.files {
		if strings.HasPrefix(f, pre) {
			n, _, _ := strings.Cut(f[len(pre):], "/")
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	for dir := range d.dirs {
		if strings.HasPrefix(dir, pre) {
			n, _, _ := strings.Cut(dir[len(pre):], "/")
			if n != "" && !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	d.mu.Unlock()
	sort.Strings(names)
	var out []struct {
		name              string
		mode, size, mtime uint32
	}
	for _, n := range append([]string{".", ".."}, names...) {
		m, s, t := uint32(0o040755), uint32(4096), uint32(0)
		if n != "." && n != ".." {
			m, s, t = d.stat(pre + n)
		}
		out = append(out, struct {
			name              string
			mode, size, mtime uint32
		}{n, m, s, t})
	}
	return out
}

func syncHdr(id string, n uint32) []byte {
	b := make([]byte, 8)
	copy(b, id)
	binary.LittleEndian.PutUint32(b[4:], n)
	return b
}

func syncFail(w io.Writer, msg string) {
	w.Write(syncHdr("FAIL", uint32(len(msg))))
	io.WriteString(w, msg)
}

func (d *Device) serveSync(c stream) {
	for {
		var h [8]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return
		}
		id := string(h[:4])
		n := binary.LittleEndian.Uint32(h[4:])
		if n > 1024 {
			return
		}
		arg := make([]byte, n)
		if _, err := io.ReadFull(c, arg); err != nil {
			return
		}
		switch id {
		case "QUIT":
			return
		case "STAT":
			m, s, t := d.stat(string(arg))
			b := make([]byte, 16)
			copy(b, "STAT")
			binary.LittleEndian.PutUint32(b[4:], m)
			binary.LittleEndian.PutUint32(b[8:], s)
			binary.LittleEndian.PutUint32(b[12:], t)
			c.Write(b)
		case "LIST":
			for _, e := range d.list(string(arg)) {
				b := make([]byte, 20)
				copy(b, "DENT")
				binary.LittleEndian.PutUint32(b[4:], e.mode)
				binary.LittleEndian.PutUint32(b[8:], e.size)
				binary.LittleEndian.PutUint32(b[12:], e.mtime)
				binary.LittleEndian.PutUint32(b[16:], uint32(len(e.name)))
				c.Write(append(b, e.name...))
			}
			b := make([]byte, 20)
			copy(b, "DONE")
			c.Write(b)
		case "RECV":
			data, ok := d.ReadFile(string(arg))
			if !ok {
				syncFail(c, "No such file or directory")
				continue
			}
			for len(data) > 0 {
				k := min(len(data), 64*1024)
				c.Write(append(syncHdr("DATA", uint32(k)), data[:k]...))
				data = data[k:]
			}
			c.Write(syncHdr("DONE", 0))
		case "SEND":
			p, ms, _ := strings.Cut(string(arg), ",")
			mode, _ := strconv.ParseUint(ms, 10, 32)
			var buf bytes.Buffer
			for {
				var hh [8]byte
				if _, err := io.ReadFull(c, hh[:]); err != nil {
					return
				}
				k := binary.LittleEndian.Uint32(hh[4:])
				if string(hh[:4]) == "DONE" {
					break
				}
				if string(hh[:4]) != "DATA" || k > 64*1024 {
					syncFail(c, "bad packet")
					return
				}
				if _, err := io.CopyN(&buf, c, int64(k)); err != nil {
					return
				}
			}
			if strings.HasPrefix(p, "/system/") {
				syncFail(c, "Read-only file system")
				continue
			}
			d.WriteFile(p, buf.Bytes(), uint32(mode))
			c.Write(syncHdr("OKAY", 0))
		default:
			syncFail(c, "unknown sync command")
			return
		}
	}
}
