package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"htadb/internal/adb"
	"htadb/internal/ws"
)

func (s *Server) routes(m *http.ServeMux) {
	p := "/api/v1"
	m.HandleFunc("GET "+p+"/version", s.h(s.version))
	m.HandleFunc("GET "+p+"/devices", s.h(s.devices))
	m.HandleFunc("GET "+p+"/devices/track", s.trackWS)
	m.HandleFunc("GET "+p+"/devices/{s}", s.h(s.device))
	m.HandleFunc("GET "+p+"/devices/{s}/props", s.h(s.props))
	m.HandleFunc("POST "+p+"/devices/{s}/shell", s.h(s.shellPost))
	m.HandleFunc("GET "+p+"/devices/{s}/shell", s.shellWS)
	m.HandleFunc("POST "+p+"/devices/{s}/exec", s.h(s.exec))
	m.HandleFunc("GET "+p+"/devices/{s}/screenshot", s.h(s.screenshot))
	m.HandleFunc("GET "+p+"/devices/{s}/files", s.h(s.files))
	m.HandleFunc("GET "+p+"/devices/{s}/files/content", s.h(s.pull))
	m.HandleFunc("PUT "+p+"/devices/{s}/files/content", s.h(s.push))
	m.HandleFunc("POST "+p+"/devices/{s}/install", s.h(s.install))
	m.HandleFunc("POST "+p+"/devices/{s}/uninstall", s.h(s.uninstall))
	m.HandleFunc("POST "+p+"/devices/{s}/reboot", s.h(s.reboot))
	m.HandleFunc("POST "+p+"/devices/{s}/root", s.h(s.admin("root")))
	m.HandleFunc("POST "+p+"/devices/{s}/unroot", s.h(s.admin("unroot")))
	m.HandleFunc("POST "+p+"/devices/{s}/remount", s.h(s.admin("remount")))
	m.HandleFunc("GET "+p+"/devices/{s}/forwards", s.h(s.listForwards))
	m.HandleFunc("POST "+p+"/devices/{s}/forwards", s.h(s.addForward))
	m.HandleFunc("DELETE "+p+"/devices/{s}/forwards", s.h(s.delForward))
	m.HandleFunc("GET "+p+"/devices/{s}/reverses", s.h(s.listReverses))
	m.HandleFunc("POST "+p+"/devices/{s}/reverses", s.h(s.addReverse))
	m.HandleFunc("DELETE "+p+"/devices/{s}/reverses", s.h(s.delReverse))
	m.HandleFunc("POST "+p+"/connect", s.h(s.connect))
	m.HandleFunc("POST "+p+"/disconnect", s.h(s.disconnect))
	m.HandleFunc("POST "+p+"/cli", s.h(s.cliPost))
	m.HandleFunc("GET "+p+"/cli", s.cliWS)
	m.HandleFunc("GET "+p+"/mirror/servers", s.h(s.mirrorServers))
	m.HandleFunc("GET "+p+"/devices/{s}/mirror/servers", s.h(s.deviceMirrorServers))
	m.HandleFunc("GET "+p+"/devices/{s}/mirror", s.mirrorWS)
	m.HandleFunc("DELETE "+p+"/devices/{s}/mirror/fallback", s.h(s.clearFallback))
	m.HandleFunc(p+"/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, Errf(http.StatusNotFound, "not_found", "no such endpoint: %s %s", r.Method, r.URL.Path))
	})
}

func serial(r *http.Request) string { return r.PathValue("s") }

func withTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()
	res := map[string]any{"version": Version, "api": "v1", "cli": s.CLI != nil, "mirror": s.Mirror != nil}
	if v, err := s.ADB.ServerVersion(ctx); err != nil {
		res["adbServerError"] = err.Error()
	} else {
		res["adbServerVersion"] = v
	}
	writeJSON(w, 200, res)
	return nil
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := withTimeout(r, 10*time.Second)
	defer cancel()
	ds, err := s.ADB.Devices(ctx)
	if err != nil {
		return err
	}
	if ds == nil {
		ds = []adb.Device{}
	}
	writeJSON(w, 200, ds)
	return nil
}

func (s *Server) findDevice(ctx context.Context, sr string) (*adb.Device, error) {
	ds, err := s.ADB.Devices(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		if d.Serial == sr {
			return &d, nil
		}
	}
	return nil, Errf(http.StatusNotFound, "device_not_found", "device %q not found", sr)
}

// keyProps are reported by GET /devices/{s}.
var keyProps = []string{
	"ro.product.manufacturer", "ro.product.model", "ro.product.name", "ro.product.device",
	"ro.build.version.release", "ro.build.version.sdk", "ro.build.fingerprint", "ro.build.type",
	"ro.product.cpu.abi", "ro.serialno", "ro.debuggable", "ro.secure",
}

func (s *Server) device(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := withTimeout(r, 15*time.Second)
	defer cancel()
	d, err := s.findDevice(ctx, serial(r))
	if err != nil {
		return err
	}
	res := map[string]any{"device": d}
	if d.State == "device" {
		f, err := s.ADB.Features(ctx, d.Serial)
		if err != nil {
			return err
		}
		var fl []string
		for k := range f {
			fl = append(fl, k)
		}
		sort.Strings(fl)
		res["features"] = fl
		props, err := s.ADB.GetProps(ctx, d.Serial)
		if err != nil {
			return err
		}
		kp := map[string]string{}
		for _, k := range keyProps {
			if v, ok := props[k]; ok {
				kp[k] = v
			}
		}
		res["props"] = kp
	}
	writeJSON(w, 200, res)
	return nil
}

func (s *Server) props(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := withTimeout(r, 15*time.Second)
	defer cancel()
	p, err := s.ADB.GetProps(ctx, serial(r))
	if err != nil {
		return err
	}
	writeJSON(w, 200, p)
	return nil
}

// textOrBase64 returns a JSON-friendly form of output bytes.
func textOrBase64(b []byte) (string, bool) {
	if utf8.Valid(b) {
		return string(b), false
	}
	return base64.StdEncoding.EncodeToString(b), true
}

func (s *Server) shellPost(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Command string  `json:"command"`
		Timeout float64 `json:"timeout"` // seconds
		Stdin   string  `json:"stdin"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.Command == "" {
		return badRequest("command is required")
	}
	to := 60 * time.Second
	if req.Timeout > 0 {
		to = time.Duration(req.Timeout * float64(time.Second))
	}
	ctx, cancel := withTimeout(r, to)
	defer cancel()
	var in io.Reader
	if req.Stdin != "" {
		in = strings.NewReader(req.Stdin)
	}
	res, err := s.ADB.RunShellInput(ctx, serial(r), req.Command, in)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Errf(http.StatusGatewayTimeout, "timeout", "command timed out after %s", to)
		}
		return err
	}
	out := map[string]any{"exitCode": res.ExitCode}
	so, b1 := textOrBase64(res.Stdout)
	se, b2 := textOrBase64(res.Stderr)
	out["stdout"], out["stderr"] = so, se
	if b1 || b2 {
		out["encoding"] = "base64"
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) error {
	cmd := r.URL.Query().Get("cmd")
	if cmd == "" && r.Header.Get("Content-Type") == "application/json" {
		var req struct {
			Command string `json:"command"`
		}
		if err := decodeJSON(r, &req); err != nil {
			return err
		}
		cmd = req.Command
	}
	if cmd == "" {
		return badRequest("command is required (?cmd= or JSON {command})")
	}
	conn, err := s.ADB.Exec(r.Context(), serial(r), cmd)
	if err != nil {
		return err
	}
	defer conn.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, err = io.Copy(w, conn)
	if err != nil {
		s.logger(r).Debug("exec stream ended", "err", err)
	}
	return nil
}

func (s *Server) screenshot(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := withTimeout(r, 30*time.Second)
	defer cancel()
	conn, err := s.ADB.Exec(ctx, serial(r), "screencap -p")
	if err != nil {
		return err
	}
	defer conn.Close()
	// Read the PNG signature first so failures still produce a JSON error.
	head := make([]byte, 8)
	n, err := io.ReadFull(conn, head)
	if n < 8 || string(head) != "\x89PNG\r\n\x1a\n" {
		rest, _ := io.ReadAll(io.LimitReader(conn, 4096))
		return Errf(http.StatusBadGateway, "screencap_failed", "screencap failed: %s", strings.TrimSpace(string(head[:n])+string(rest)))
	}
	_ = err
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="screenshot-%s-%s.png"`, sanitize(serial(r)), time.Now().Format("20060102-150405")))
	w.Write(head)
	io.Copy(w, conn)
	return nil
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

func reqPath(r *http.Request) (string, error) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "", badRequest("path is required")
	}
	if !strings.HasPrefix(p, "/") {
		return "", badRequest("path must be absolute")
	}
	return p, nil
}

func (s *Server) files(w http.ResponseWriter, r *http.Request) error {
	p, err := reqPath(r)
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout(r, 30*time.Second)
	defer cancel()
	sy, err := s.ADB.OpenSync(ctx, serial(r))
	if err != nil {
		return err
	}
	defer sy.Close()
	st, err := sy.Stat(p)
	if err != nil {
		return err
	}
	if !st.Exists() {
		return Errf(http.StatusNotFound, "not_found", "%s: no such file or directory", p)
	}
	res := map[string]any{"path": p, "stat": st}
	if st.IsDir() {
		ents, err := sy.List(p)
		if err != nil {
			return err
		}
		if ents == nil {
			ents = []adb.FileInfo{}
		}
		res["entries"] = ents
	}
	writeJSON(w, 200, res)
	return nil
}

func (s *Server) pull(w http.ResponseWriter, r *http.Request) error {
	p, err := reqPath(r)
	if err != nil {
		return err
	}
	sy, err := s.ADB.OpenSync(r.Context(), serial(r))
	if err != nil {
		return err
	}
	defer sy.Close()
	st, err := sy.Stat(p)
	if err != nil {
		return err
	}
	if !st.Exists() {
		return Errf(http.StatusNotFound, "not_found", "%s: no such file or directory", p)
	}
	if st.IsDir() {
		return badRequest("%s is a directory", p)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, sanitize(path.Base(p))))
	if st.Type == "file" {
		w.Header().Set("Content-Length", strconv.FormatUint(uint64(st.Size), 10))
	}
	if _, err := sy.Recv(p, w); err != nil {
		// Headers are gone; abort the response so the client sees a
		// truncated transfer rather than a silently short file.
		s.logger(r).Warn("pull failed mid-stream", "path", p, "err", err)
		panic(http.ErrAbortHandler)
	}
	return nil
}

func (s *Server) push(w http.ResponseWriter, r *http.Request) error {
	p, err := reqPath(r)
	if err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if m := r.URL.Query().Get("mode"); m != "" {
		n, err := strconv.ParseUint(m, 8, 32)
		if err != nil || n > 0o7777 {
			return badRequest("bad mode %q (octal, e.g. 0644)", m)
		}
		mode = fs.FileMode(n)
	}
	sy, err := s.ADB.OpenSync(r.Context(), serial(r))
	if err != nil {
		return err
	}
	defer sy.Close()
	if st, err := sy.Stat(p); err == nil && st.IsDir() {
		return badRequest("%s is a directory; give the full target file path", p)
	}
	n, err := sy.Send(p, mode, time.Now(), r.Body)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"path": p, "bytes": n, "mode": fmt.Sprintf("%04o", mode)})
	return nil
}

func installArgs(r *http.Request) []string {
	var out []string
	for _, a := range r.URL.Query()["args"] {
		out = append(out, strings.Fields(a)...)
	}
	return out
}

func (s *Server) install(w http.ResponseWriter, r *http.Request) error {
	args := installArgs(r)
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return badRequest("install args must be flags, got %q", a)
		}
	}
	body := io.Reader(r.Body)
	size := r.ContentLength
	if size < 0 {
		// Unknown length: spool to a temp file first.
		f, err := os.CreateTemp("", "htadb-install-*.apk")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		n, err := io.Copy(f, r.Body)
		if err != nil {
			return badRequest("reading upload: %v", err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		body, size = f, n
	}
	if size == 0 {
		return badRequest("empty APK body")
	}
	ctx, cancel := withTimeout(r, 10*time.Minute)
	defer cancel()
	res, err := s.ADB.Install(ctx, serial(r), body, size, args)
	if err != nil {
		if res != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": res.Output, "code": "install_failed", "output": res.Output})
			return nil
		}
		return err
	}
	writeJSON(w, 200, res)
	return nil
}

func (s *Server) uninstall(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Package  string `json:"package"`
		KeepData bool   `json:"keepData"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.Package == "" {
		return badRequest("package is required")
	}
	ctx, cancel := withTimeout(r, 60*time.Second)
	defer cancel()
	res, err := s.ADB.Uninstall(ctx, serial(r), req.Package, req.KeepData)
	if err != nil {
		if res != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": res.Output, "code": "uninstall_failed", "output": res.Output})
			return nil
		}
		return err
	}
	writeJSON(w, 200, res)
	return nil
}

var rebootTargets = map[string]bool{"": true, "bootloader": true, "recovery": true, "sideload": true, "sideload-auto-reboot": true, "fastboot": true, "edl": true}

func (s *Server) reboot(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Target string `json:"target"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if !rebootTargets[req.Target] {
		return badRequest("unknown reboot target %q", req.Target)
	}
	ctx, cancel := withTimeout(r, 30*time.Second)
	defer cancel()
	if err := s.ADB.Reboot(ctx, serial(r), req.Target); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "target": req.Target})
	return nil
}

func (s *Server) admin(op string) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		ctx, cancel := withTimeout(r, 60*time.Second)
		defer cancel()
		var out string
		var err error
		switch op {
		case "root":
			out, err = s.ADB.Root(ctx, serial(r))
		case "unroot":
			out, err = s.ADB.Unroot(ctx, serial(r))
		case "remount":
			out, err = s.ADB.Remount(ctx, serial(r))
		}
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"output": out})
		return nil
	}
}

type fwdReq struct {
	Local    string `json:"local"`
	Remote   string `json:"remote"`
	NoRebind bool   `json:"noRebind"`
}

func validSpec(s string) bool {
	k, v, ok := strings.Cut(s, ":")
	return ok && k != "" && v != "" && !strings.ContainsAny(s, ";\n")
}

func (s *Server) listForwards(w http.ResponseWriter, r *http.Request) error {
	fs, err := s.ADB.ListForwards(r.Context(), serial(r))
	if err != nil {
		return err
	}
	if fs == nil {
		fs = []adb.Forward{}
	}
	writeJSON(w, 200, fs)
	return nil
}

func (s *Server) addForward(w http.ResponseWriter, r *http.Request) error {
	var req fwdReq
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if !validSpec(req.Local) || !validSpec(req.Remote) {
		return badRequest("local and remote must be specs like tcp:8080 or localabstract:name")
	}
	port, err := s.ADB.AddForward(r.Context(), serial(r), req.Local, req.Remote, req.NoRebind)
	if err != nil {
		return err
	}
	res := map[string]any{"local": req.Local, "remote": req.Remote}
	if port != "" {
		res["port"] = port
		res["local"] = "tcp:" + port
	}
	writeJSON(w, 200, res)
	return nil
}

func (s *Server) delForward(w http.ResponseWriter, r *http.Request) error {
	local := r.URL.Query().Get("local")
	if local != "" && !validSpec(local) {
		return badRequest("bad local spec")
	}
	if err := s.ADB.RemoveForward(r.Context(), serial(r), local); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true})
	return nil
}

func (s *Server) listReverses(w http.ResponseWriter, r *http.Request) error {
	fs, err := s.ADB.ListReverses(r.Context(), serial(r))
	if err != nil {
		return err
	}
	if fs == nil {
		fs = []adb.Forward{}
	}
	writeJSON(w, 200, fs)
	return nil
}

func (s *Server) addReverse(w http.ResponseWriter, r *http.Request) error {
	var req fwdReq
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	// For reverses, "remote" is the device side and "local" the host side.
	if !validSpec(req.Local) || !validSpec(req.Remote) {
		return badRequest("local and remote must be specs like tcp:8080")
	}
	port, err := s.ADB.AddReverse(r.Context(), serial(r), req.Remote, req.Local, req.NoRebind)
	if err != nil {
		return err
	}
	res := map[string]any{"local": req.Local, "remote": req.Remote}
	if port != "" {
		res["port"] = port
	}
	writeJSON(w, 200, res)
	return nil
}

func (s *Server) delReverse(w http.ResponseWriter, r *http.Request) error {
	remote := r.URL.Query().Get("remote")
	if remote != "" && !validSpec(remote) {
		return badRequest("bad remote spec")
	}
	if err := s.ADB.RemoveReverse(r.Context(), serial(r), remote); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true})
	return nil
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Address string `json:"address"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.Address == "" {
		return badRequest("address is required")
	}
	ctx, cancel := withTimeout(r, 30*time.Second)
	defer cancel()
	out, err := s.ADB.Connect(ctx, req.Address)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"output": out})
	return nil
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Address string `json:"address"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	out, err := s.ADB.Disconnect(r.Context(), req.Address)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"output": out})
	return nil
}

// ---- device tracking WebSocket ----

func (s *Server) trackWS(w http.ResponseWriter, r *http.Request) {
	c, err := s.acceptWS(w, r)
	if err != nil {
		return
	}
	metricWS.Add(1)
	defer metricWS.Add(-1)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() { // reader: detects close, answers pings
		for {
			if _, _, err := c.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()
	go ws.KeepAlive(ctx, c, 20*time.Second, cancel)
	ch, unsub := s.Tracker.Subscribe()
	defer unsub()
	snap, ok := s.Tracker.Snapshot()
	if snap == nil {
		snap = []adb.Device{}
	}
	send := func(v any) error {
		b, _ := json.Marshal(v)
		wctx, wc := context.WithTimeout(ctx, 10*time.Second)
		defer wc()
		return c.Write(wctx, ws.Text, b)
	}
	if err := send(map[string]any{"type": "snapshot", "devices": snap, "adb": ok}); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			c.Close(ws.StatusNormal, "")
			return
		case evs := <-ch:
			for _, e := range evs {
				if err := send(e); err != nil {
					return
				}
			}
		}
	}
}
