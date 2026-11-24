package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"htadb/internal/adb"
	"htadb/internal/adb/adbtest"
	"htadb/internal/api"
	"htadb/internal/cli"
	"htadb/internal/ws"
)

type env struct {
	adb  *adbtest.Server
	dev  *adbtest.Device
	http *httptest.Server
	srv  *api.Server
}

func newEnv(t *testing.T, withCLI bool) *env {
	t.Helper()
	a, err := adbtest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	d := adbtest.NewDevice("EMU1")
	a.AddDevice(d)
	s := &api.Server{ADB: adb.New(a.Addr()), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), CORSOrigins: []string{"*"}}
	if withCLI && runtime.GOOS != "windows" {
		dir := t.TempDir()
		script := filepath.Join(dir, "adb")
		os.WriteFile(script, []byte(`#!/bin/sh
case " $* " in *" logcat "*)
  trap 'echo interrupted; exit 130' INT
  echo started
  while true; do sleep 0.05; done;;
esac
case " $* " in *" cat "*) exec cat;; esac
echo "args:$*"
echo "stderr-line" >&2
exit 3
`), 0o755)
		s.CLI = &cli.Runner{Path: script, ServerAddr: a.Addr()}
	}
	h := s.Handler()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.Tracker.Run(ctx)
	e := &env{adb: a, dev: d, http: httptest.NewServer(h), srv: s}
	t.Cleanup(e.http.Close)
	return e
}

func (e *env) do(t *testing.T, method, path string, body io.Reader, ctype string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.http.URL+path, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func (e *env) json(t *testing.T, method, path string, in any, wantStatus int) map[string]any {
	t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	resp, b := e.do(t, method, path, body, "application/json")
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, wantStatus, b)
	}
	var out map[string]any
	if len(b) > 0 && b[0] == '{' {
		json.Unmarshal(b, &out)
	}
	return out
}

func TestVersionDevices(t *testing.T) {
	e := newEnv(t, false)
	v := e.json(t, "GET", "/api/v1/version", nil, 200)
	if v["adbServerVersion"].(float64) != 41 || v["api"] != "v1" {
		t.Fatal(v)
	}
	resp, b := e.do(t, "GET", "/api/v1/devices", nil, "")
	var ds []adb.Device
	json.Unmarshal(b, &ds)
	if resp.StatusCode != 200 || len(ds) != 1 || ds[0].Serial != "EMU1" || ds[0].Model != "Fake_Phone" {
		t.Fatalf("%s", b)
	}
	d := e.json(t, "GET", "/api/v1/devices/EMU1", nil, 200)
	if d["props"].(map[string]any)["ro.build.version.sdk"] != "34" {
		t.Fatal(d)
	}
	e.json(t, "GET", "/api/v1/devices/NOPE", nil, 404)
	p := e.json(t, "GET", "/api/v1/devices/EMU1/props", nil, 200)
	if p["ro.product.model"] != "Fake Phone" {
		t.Fatal(p)
	}
	h := e.json(t, "GET", "/healthz", nil, 200)
	if h["adb"] != true {
		t.Fatal(h)
	}
	e.json(t, "GET", "/api/v1/nonsense", nil, 404)
}

func TestErrorsJSON(t *testing.T) {
	e := newEnv(t, false)
	r := e.json(t, "POST", "/api/v1/devices/NOPE/shell", map[string]any{"command": "true"}, 404)
	if r["code"] != "device_not_found" || r["error"] == "" {
		t.Fatal(r)
	}
	// adb FAIL → 502.
	r = e.json(t, "DELETE", "/api/v1/devices/EMU1/forwards?local=tcp:1", nil, 502)
	if r["code"] != "adb_fail" {
		t.Fatal(r)
	}
	r = e.json(t, "POST", "/api/v1/devices/EMU1/shell", map[string]any{}, 400)
	if r["code"] != "bad_request" {
		t.Fatal(r)
	}
	// adb server down → 503.
	e.adb.Close()
	r = e.json(t, "GET", "/api/v1/devices", nil, 503)
	if r["code"] != "adb_unavailable" {
		t.Fatal(r)
	}
}

func TestCORS(t *testing.T) {
	e := newEnv(t, false)
	req, _ := http.NewRequest("OPTIONS", e.http.URL+"/api/v1/devices", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8000")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:8000" || !strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), "PUT") {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	e.srv.CORSOrigins = []string{"https://only.example"}
	resp, _ = e.do(t, "GET", "/api/v1/version", nil, "")
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no Origin header → no CORS headers")
	}
}

func TestShellPost(t *testing.T) {
	e := newEnv(t, false)
	r := e.json(t, "POST", "/api/v1/devices/EMU1/shell", map[string]any{"command": "echo hi; nosuch; exit 4"}, 200)
	if r["stdout"] != "hi\n" || r["exitCode"].(float64) != 4 || !strings.Contains(r["stderr"].(string), "not found") {
		t.Fatal(r)
	}
	r = e.json(t, "POST", "/api/v1/devices/EMU1/shell", map[string]any{"command": "sleep 5", "timeout": 0.2}, 504)
	if r["code"] != "timeout" {
		t.Fatal(r)
	}
}

func wsURL(e *env, path string) string {
	return "ws" + strings.TrimPrefix(e.http.URL, "http") + path
}

func readAll(t *testing.T, c ws.Conn, until func(out string, texts []map[string]any) bool) (string, []map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	var texts []map[string]any
	for !until(out.String(), texts) {
		typ, b, err := c.Read(ctx)
		if err != nil {
			break
		}
		if typ == ws.Binary {
			out.Write(b)
		} else {
			var m map[string]any
			json.Unmarshal(b, &m)
			texts = append(texts, m)
		}
	}
	return out.String(), texts
}

func hasExit(texts []map[string]any) (float64, bool) {
	for _, m := range texts {
		if m["type"] == "exit" {
			return m["code"].(float64), true
		}
	}
	return 0, false
}

func TestShellWS(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	c, err := ws.Dial(ctx, wsURL(e, "/api/v1/devices/EMU1/shell?pty=1&rows=40&cols=120&term=xterm"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(1000, "")
	c.Write(ctx, ws.Text, []byte(`{"type":"resize","rows":50,"cols":160}`))
	c.Write(ctx, ws.Binary, []byte("echo marker\n"))
	out, _ := readAll(t, c, func(o string, _ []map[string]any) bool {
		return strings.Contains(o, "marker\r\n") || strings.Contains(o, "marker\n")
	})
	if !strings.Contains(out, "fake:/ $ ") {
		t.Fatalf("no prompt: %q", out)
	}
	c.Write(ctx, ws.Binary, []byte("exit\n"))
	_, texts := readAll(t, c, func(_ string, tx []map[string]any) bool { _, ok := hasExit(tx); return ok })
	if code, ok := hasExit(texts); !ok || code != 0 {
		t.Fatalf("exit: %v", texts)
	}
	if len(e.dev.Resizes) < 2 || e.dev.Resizes[0] != "40x120,0x0" || e.dev.Resizes[1] != "50x160,0x0" {
		t.Fatalf("resizes %v", e.dev.Resizes)
	}

	// Non-PTY command: stderr arrives as text frames, exit code reported.
	c2, err := ws.Dial(ctx, wsURL(e, "/api/v1/devices/EMU1/shell?cmd="+urlq("cat; nosuch")), nil)
	if err != nil {
		t.Fatal(err)
	}
	c2.Write(ctx, ws.Binary, []byte("stdin-data"))
	c2.Write(ctx, ws.Text, []byte(`{"type":"eof"}`))
	out, texts = readAll(t, c2, func(_ string, tx []map[string]any) bool { _, ok := hasExit(tx); return ok })
	if out != "stdin-data" {
		t.Fatalf("out %q", out)
	}
	code, _ := hasExit(texts)
	stderr := ""
	for _, m := range texts {
		if m["type"] == "stderr" {
			stderr += m["data"].(string)
		}
	}
	if code != 127 || !strings.Contains(stderr, "not found") {
		t.Fatalf("code %v stderr %q", code, stderr)
	}

	// Unknown device: plain HTTP 404 before the upgrade.
	resp, _ := e.do(t, "GET", "/api/v1/devices/NOPE/shell", nil, "")
	if resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
}

func urlq(s string) string {
	return strings.NewReplacer(" ", "%20", ";", "%3B").Replace(s)
}

func TestFiles(t *testing.T) {
	e := newEnv(t, false)
	data := bytes.Repeat([]byte("abcdef"), 50000)
	resp, b := e.do(t, "PUT", "/api/v1/devices/EMU1/files/content?path=/sdcard/Download/f.bin&mode=0600", bytes.NewReader(data), "application/octet-stream")
	if resp.StatusCode != 200 {
		t.Fatal(string(b))
	}
	got, _ := e.dev.ReadFile("/sdcard/Download/f.bin")
	if !bytes.Equal(got, data) {
		t.Fatal("push content")
	}
	st := e.json(t, "GET", "/api/v1/devices/EMU1/files?path=/sdcard/Download/f.bin", nil, 200)
	if st["stat"].(map[string]any)["perm"] != "-rw-------" {
		t.Fatal(st)
	}
	ls := e.json(t, "GET", "/api/v1/devices/EMU1/files?path=/sdcard/Download", nil, 200)
	ents := ls["entries"].([]any)
	if len(ents) != 1 || ents[0].(map[string]any)["name"] != "f.bin" {
		t.Fatal(ls)
	}
	resp, b = e.do(t, "GET", "/api/v1/devices/EMU1/files/content?path=/sdcard/Download/f.bin", nil, "")
	if resp.StatusCode != 200 || !bytes.Equal(b, data) || resp.Header.Get("Content-Length") != "300000" || !strings.Contains(resp.Header.Get("Content-Disposition"), "f.bin") {
		t.Fatalf("pull %d %v", resp.StatusCode, resp.Header)
	}
	e.json(t, "GET", "/api/v1/devices/EMU1/files?path=/nope", nil, 404)
	e.json(t, "GET", "/api/v1/devices/EMU1/files/content?path=/nope", nil, 404)
	e.json(t, "GET", "/api/v1/devices/EMU1/files?path=relative", nil, 400)
	e.json(t, "PUT", "/api/v1/devices/EMU1/files/content?path=/sdcard/Download", nil, 400)
	r := e.json(t, "PUT", "/api/v1/devices/EMU1/files/content?path=/system/x", nil, 502)
	if !strings.Contains(r["error"].(string), "Read-only") {
		t.Fatal(r)
	}
}

func TestInstall(t *testing.T) {
	e := newEnv(t, false)
	apk := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{7}, 5000)...)
	resp, b := e.do(t, "POST", "/api/v1/devices/EMU1/install?args=-r+-g", bytes.NewReader(apk), "application/vnd.android.package-archive")
	if resp.StatusCode != 200 || !strings.Contains(string(b), "Success") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if e.dev.ServiceCount("exec:cmd package install -S 5004 -r -g") != 1 {
		t.Fatal(e.dev.Services)
	}
	// No Content-Length (chunked): spooled to a temp file first.
	pr, pw := io.Pipe()
	go func() { pw.Write(apk); pw.Close() }()
	req, _ := http.NewRequest("POST", e.http.URL+"/api/v1/devices/EMU1/install", pr)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || e.dev.ServiceCount("exec:cmd package install -S 5004") != 2 {
		t.Fatalf("chunked: %d %s", resp2.StatusCode, b)
	}
	resp, b = e.do(t, "POST", "/api/v1/devices/EMU1/install", strings.NewReader("not an apk"), "")
	if resp.StatusCode != 502 || !strings.Contains(string(b), "INSTALL_PARSE_FAILED") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	e.json(t, "POST", "/api/v1/devices/EMU1/install?args=evil", nil, 400)
	r := e.json(t, "POST", "/api/v1/devices/EMU1/uninstall", map[string]any{"package": "com.x", "keepData": true}, 200)
	if r["success"] != true || e.dev.ServiceCount("shell,v2,raw:pm uninstall -k com.x") != 1 {
		t.Fatal(r)
	}
	e.json(t, "POST", "/api/v1/devices/EMU1/uninstall", map[string]any{"package": "com.missing"}, 502)
}

func TestAdminAndForwards(t *testing.T) {
	e := newEnv(t, false)
	if r := e.json(t, "POST", "/api/v1/devices/EMU1/root", nil, 200); !strings.Contains(r["output"].(string), "root") {
		t.Fatal(r)
	}
	e.json(t, "POST", "/api/v1/devices/EMU1/unroot", nil, 200)
	e.json(t, "POST", "/api/v1/devices/EMU1/remount", nil, 200)
	e.json(t, "POST", "/api/v1/devices/EMU1/reboot", map[string]any{"target": "bogus"}, 400)
	e.json(t, "POST", "/api/v1/devices/EMU1/reboot", map[string]any{"target": "bootloader"}, 200)
	if e.dev.ServiceCount("reboot:bootloader") != 1 {
		t.Fatal(e.dev.Services)
	}
	time.Sleep(200 * time.Millisecond) // the fake goes offline briefly

	r := e.json(t, "POST", "/api/v1/devices/EMU1/forwards", map[string]any{"local": "tcp:0", "remote": "localabstract:x"}, 200)
	if r["port"] != "34567" {
		t.Fatal(r)
	}
	e.json(t, "POST", "/api/v1/devices/EMU1/forwards", map[string]any{"local": "tcp:1;tcp:2", "remote": "tcp:3"}, 400)
	_, b := e.do(t, "GET", "/api/v1/devices/EMU1/forwards", nil, "")
	if !strings.Contains(string(b), "tcp:34567") {
		t.Fatal(string(b))
	}
	e.json(t, "DELETE", "/api/v1/devices/EMU1/forwards", nil, 200)
	_, b = e.do(t, "GET", "/api/v1/devices/EMU1/forwards", nil, "")
	if strings.TrimSpace(string(b)) != "[]" {
		t.Fatal(string(b))
	}
	e.json(t, "POST", "/api/v1/devices/EMU1/reverses", map[string]any{"remote": "tcp:8081", "local": "tcp:8081"}, 200)
	_, b = e.do(t, "GET", "/api/v1/devices/EMU1/reverses", nil, "")
	if !strings.Contains(string(b), `"local":"tcp:8081"`) {
		t.Fatal(string(b))
	}
	e.json(t, "DELETE", "/api/v1/devices/EMU1/reverses?remote=tcp:8081", nil, 200)

	r = e.json(t, "POST", "/api/v1/connect", map[string]any{"address": "10.0.0.9:5555"}, 200)
	if !strings.Contains(r["output"].(string), "connected") {
		t.Fatal(r)
	}
	e.json(t, "POST", "/api/v1/connect", map[string]any{"address": "bad:1"}, 502)
	e.json(t, "POST", "/api/v1/disconnect", map[string]any{"address": "10.0.0.9:5555"}, 200)
}

func TestTrackWS(t *testing.T) {
	e := newEnv(t, false)
	time.Sleep(100 * time.Millisecond) // let the tracker connect
	ctx := context.Background()
	c, err := ws.Dial(ctx, wsURL(e, "/api/v1/devices/track"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(1000, "")
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, b, err := c.Read(rctx)
	if err != nil {
		t.Fatal(err)
	}
	var snap map[string]any
	json.Unmarshal(b, &snap)
	if snap["type"] != "snapshot" || len(snap["devices"].([]any)) != 1 {
		t.Fatal(string(b))
	}
	e.adb.AddDevice(adbtest.NewDevice("NEW"))
	_, b, err = c.Read(rctx)
	if err != nil || !strings.Contains(string(b), `"type":"add"`) || !strings.Contains(string(b), "NEW") {
		t.Fatal(string(b), err)
	}
	e.adb.SetState("NEW", "offline")
	_, b, _ = c.Read(rctx)
	if !strings.Contains(string(b), `"type":"state"`) || !strings.Contains(string(b), `"oldState":"device"`) {
		t.Fatal(string(b))
	}
	e.adb.RemoveDevice("NEW")
	_, b, _ = c.Read(rctx)
	if !strings.Contains(string(b), `"type":"remove"`) {
		t.Fatal(string(b))
	}
}

func TestScreenshotExec(t *testing.T) {
	e := newEnv(t, false)
	resp, b := e.do(t, "GET", "/api/v1/devices/EMU1/screenshot", nil, "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(b, adbtest.FakePNG) {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	resp, b = e.do(t, "POST", "/api/v1/devices/EMU1/exec?cmd=cat+/sdcard/none%3B+echo+x", nil, "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatal(resp.StatusCode)
	}
	e.dev.WriteFile("/sdcard/bin", []byte{0, 1, 2, 0xff, '\n', '\r'}, 0o644)
	_, b = e.do(t, "POST", "/api/v1/devices/EMU1/exec", strings.NewReader(`{"command":"cat /sdcard/bin"}`), "application/json")
	if !bytes.Equal(b, []byte{0, 1, 2, 0xff, '\n', '\r'}) {
		t.Fatalf("exec not binary-clean: % x", b)
	}
}

func TestCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	e := newEnv(t, true)
	r := e.json(t, "POST", "/api/v1/cli", map[string]any{"args": []string{"devices", "-l", "; rm -rf /"}, "serial": "EMU1"}, 200)
	if !strings.HasSuffix(r["stdout"].(string), "-s EMU1 devices -l ; rm -rf /\n") || !strings.HasPrefix(r["stdout"].(string), "args:-P ") || r["exitCode"].(float64) != 3 || r["stderr"] != "stderr-line\n" {
		t.Fatal(r)
	}
	// Streamed, with Ctrl+C as SIGINT.
	ctx := context.Background()
	c, err := ws.Dial(ctx, wsURL(e, "/api/v1/cli?arg=logcat&serial=EMU1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(1000, "")
	readAll(t, c, func(o string, _ []map[string]any) bool { return strings.Contains(o, "started") })
	c.Write(ctx, ws.Text, []byte(`{"type":"signal","name":"SIGINT"}`))
	out, texts := readAll(t, c, func(_ string, tx []map[string]any) bool { _, ok := hasExit(tx); return ok })
	code, _ := hasExit(texts)
	if !strings.Contains(out, "interrupted") || code != 130 {
		t.Fatalf("out %q code %v", out, code)
	}
	// stdin + eof.
	c2, _ := ws.Dial(ctx, wsURL(e, "/api/v1/cli?arg=cat"), nil)
	c2.Write(ctx, ws.Binary, []byte("hello"))
	c2.Write(ctx, ws.Text, []byte(`{"type":"eof"}`))
	out, _ = readAll(t, c2, func(_ string, tx []map[string]any) bool { _, ok := hasExit(tx); return ok })
	if out != "hello" {
		t.Fatalf("%q", out)
	}
	// Disabled.
	e.srv.CLI = nil
	e.json(t, "POST", "/api/v1/cli", map[string]any{"args": []string{"devices"}}, 501)
}
