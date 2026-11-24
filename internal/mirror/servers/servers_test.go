package servers_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"htadb/internal/adb"
	"htadb/internal/adb/adbtest"
	"htadb/internal/mirror/mirrortest"
	"htadb/internal/mirror/servers"
)

func TestNames(t *testing.T) {
	ok := map[string]string{
		"scrcpy-server-v5.0.jar":       "5.0",
		"scrcpy-server-v5.0.1.jar":     "5.0.1",
		"scrcpy-server-v4.1-rc1.jar":   "4.1-rc1",
		"scrcpy-server-v4.1+local.jar": "4.1+local",
	}
	for n, v := range ok {
		if got, good := servers.ParseDeviceName(n); !good || got != v {
			t.Errorf("%s: %q %v", n, got, good)
		}
	}
	for _, n := range []string{"scrcpy-server.jar", "scrcpy-server-v5.jar", ".scrcpy-server-v5.0.jar.abc.tmp", "scrcpy-server-v5.0", "xscrcpy-server-v5.0.jar", ".session-0000abcd.jar"} {
		if _, good := servers.ParseDeviceName(n); good {
			t.Errorf("%s should not match", n)
		}
	}
	if v, ok := servers.ParseFileName("scrcpy-server-v5.0"); !ok || v != "5.0" {
		t.Error("upstream asset name")
	}
	if servers.DevicePath("5.0") != "/data/local/tmp/htadb/scrcpy-server-v5.0.jar" {
		t.Error(servers.DevicePath("5.0"))
	}
}

func testRegistry(t *testing.T) *servers.Registry {
	r, err := servers.NewRegistry(fstest.MapFS{
		"scrcpy-server-v5.0.1.jar": {Data: mirrortest.FakeJar("5.0.1")},
		"scrcpy-server-v4.1.jar":   {Data: mirrortest.FakeJar("4.1")},
		"scrcpy-server-v4.0.jar":   {Data: mirrortest.FakeJar("4.0")},
		"README.md":                {Data: []byte("x")},
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegistry(t *testing.T) {
	r := testRegistry(t)
	l := r.List()
	if len(l) != 3 || l[0].Version != "5.0.1" || l[0].Profile != "5.0" || !l[0].Latest {
		t.Fatalf("%+v", l[0])
	}
	if r.Get("4.1").Latest != true || r.Get("4.0").Latest {
		t.Error("latest per profile")
	}
	if r.Newest("4.x").Version != "4.1" {
		t.Error("newest 4.x")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "scrcpy-server-v5.0")
	os.WriteFile(p, mirrortest.FakeJar("5.0"), 0o644)
	p2 := filepath.Join(dir, "custom.jar")
	os.WriteFile(p2, mirrortest.FakeJar("4.1"), 0o644)
	r2, err := servers.NewRegistry(nil, []string{p, "4.1=" + p2}, false)
	if err != nil {
		t.Fatal(err)
	}
	if b := r2.Get("5.0"); b == nil || b.Source != "file" || b.Upstream {
		t.Fatalf("%+v", b)
	}
	if r2.Get("4.1").Path != p2 {
		t.Error("explicit version")
	}
	if _, err := servers.NewRegistry(nil, []string{p2}, false); err == nil {
		t.Error("expected error: version not inferable")
	}
	p3 := filepath.Join(dir, "scrcpy-server-v4.7.jar")
	os.WriteFile(p3, []byte("x"), 0o644)
	if _, err := servers.NewRegistry(nil, []string{p3}, false); err == nil {
		t.Error("untested version must be refused")
	}
	if _, err := servers.NewRegistry(nil, []string{p3}, true); err != nil {
		t.Error(err)
	}
}

func setup(t *testing.T) (*servers.Manager, *adbtest.Device) {
	srv, err := adbtest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	d := adbtest.NewDevice("D1")
	srv.AddDevice(d)
	return &servers.Manager{ADB: adb.New(srv.Addr()), Registry: testRegistry(t)}, d
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func noTmp(t *testing.T, d *adbtest.Device) {
	for _, f := range d.Files() {
		if strings.HasSuffix(f, ".tmp") {
			t.Errorf("temp file left behind: %s", f)
		}
	}
}

func TestEnsure(t *testing.T) {
	m, d := setup(t)
	b := m.Registry.Get("5.0.1")
	dst := servers.DevicePath("5.0.1")

	r, err := m.Ensure(ctx(t), "D1", b)
	if err != nil || !r.Pushed || r.Reason != "missing" {
		t.Fatalf("missing: %+v %v", r, err)
	}
	got, _ := d.ReadFile(dst)
	if string(got) != string(mirrortest.FakeJar("5.0.1")) {
		t.Fatal("content")
	}
	// The push went through a hidden temp name and mv -f.
	if d.ServiceCount("shell,v2,raw:mv -f /data/local/tmp/htadb/.scrcpy-server-v5.0.1.jar.") != 1 {
		t.Fatalf("no atomic rename: %v", d.Services)
	}
	noTmp(t, d)

	r, err = m.Ensure(ctx(t), "D1", b)
	if err != nil || r.Pushed || r.Reason != "reused" {
		t.Fatalf("reuse: %+v %v", r, err)
	}

	// Same size, different content → hash mismatch → re-push.
	bad := []byte(strings.Repeat("x", len(mirrortest.FakeJar("5.0.1"))))
	d.WriteFile(dst, bad, 0o644)
	r, err = m.Ensure(ctx(t), "D1", b)
	if err != nil || !r.Pushed || r.Reason != "hash mismatch" {
		t.Fatalf("hash: %+v %v", r, err)
	}

	// Different size → size mismatch → re-push without hashing.
	d.WriteFile(dst, []byte("short"), 0o644)
	before := d.ServiceCount("shell,v2,raw:sha256sum")
	r, err = m.Ensure(ctx(t), "D1", b)
	if err != nil || !r.Pushed || r.Reason != "size mismatch" {
		t.Fatalf("size: %+v %v", r, err)
	}
	if d.ServiceCount("shell,v2,raw:sha256sum") != before {
		t.Error("size mismatch should not hash")
	}
	noTmp(t, d)
}

func TestEnsureConcurrent(t *testing.T) {
	m, d := setup(t)
	b := m.Registry.Get("4.1")
	var wg sync.WaitGroup
	results := make([]*servers.EnsureResult, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := m.Ensure(ctx(t), "D1", b)
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	pushed := 0
	for _, r := range results {
		if r != nil && r.Pushed {
			pushed++
		}
	}
	if pushed != 1 {
		t.Fatalf("%d pushes for concurrent sessions, want 1", pushed)
	}
	if d.ServiceCount("shell,v2,raw:mv -f") != 1 {
		t.Fatal("expected exactly one rename")
	}
	noTmp(t, d)
}

func TestInventory(t *testing.T) {
	m, d := setup(t)
	d.WriteFile(servers.DevicePath("5.0.1"), mirrortest.FakeJar("5.0.1"), 0o644) // matches registry
	d.WriteFile(servers.DevicePath("4.1"), []byte("tampered"), 0o644)            // size differs
	d.WriteFile(servers.DevicePath("4.2"), []byte("unknown"), 0o644)             // not known
	d.WriteFile(servers.DeviceDir+"/notes.txt", []byte("x"), 0o644)
	d.WriteFile(servers.DeviceDir+"/.scrcpy-server-v5.0.jar.abcd.tmp", []byte("x"), 0o644)
	d.WriteFile("/data/local/tmp/scrcpy-server.jar", []byte("cli"), 0o644)
	jars, err := m.Inventory(ctx(t), "D1")
	if err != nil {
		t.Fatal(err)
	}
	if len(jars) != 3 {
		t.Fatalf("%+v", jars)
	}
	by := map[string]servers.DeviceJar{}
	for _, j := range jars {
		by[j.Version] = j
	}
	if !by["5.0.1"].Verified || !by["5.0.1"].Known {
		t.Errorf("5.0.1 %+v", by["5.0.1"])
	}
	if by["4.1"].Verified {
		t.Error("tampered 4.1 verified")
	}
	if by["4.2"].Verified || by["4.2"].Known || by["4.2"].Tested || by["4.2"].Profile != "4.x" {
		t.Errorf("4.2 %+v", by["4.2"])
	}
	if jars[0].Version != "5.0.1" {
		t.Error("sort order")
	}
}
