package config

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTOML(t *testing.T) {
	src := `
# comment
listen = ["http://127.0.0.1:1", # trailing
  "unix:///tmp/x.sock"]
cli = false
adb_server = "10.0.0.1:5037"
'quoted key' = 'lit\eral'

[mirror]
default-version = "4.1"
fallback = false

[mirror.devices."R58M12345"]
version = "4.1"
max_size = 1024

[mirror.devices.'emulator-5554']
opts = { a = 1, b = "x" }
`
	m, err := ParseTOML(src)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m["listen"], []any{"http://127.0.0.1:1", "unix:///tmp/x.sock"}) {
		t.Errorf("listen %v", m["listen"])
	}
	if m["quoted key"] != `lit\eral` || m["cli"] != false {
		t.Errorf("%v", m)
	}
	dev := m["mirror"].(map[string]any)["devices"].(map[string]any)["R58M12345"].(map[string]any)
	if dev["version"] != "4.1" || dev["max_size"] != int64(1024) {
		t.Errorf("%v", dev)
	}
	for _, bad := range []string{"a = ", "a = \"x", "[a", "a = 1\na = 2", "a = [1, 2", "a.b = 1\n[a.b]\n"} {
		if _, err := ParseTOML(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestPrecedence(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
adb-server = "toml:1"
cors-origin = "https://toml"
cli = false
tls-san = ["a.test"]
[mirror]
default-version = "4.1"
[mirror.devices."S1"]
version = "4.0"
max_size = 800
`), 0o600)
	env := map[string]string{
		"HTADB_CONFIG_DIR": dir,
		"HTADB_ADB_SERVER": "env:2",
		"HTADB_LISTEN":     "http://127.0.0.1:1,unix:///tmp/h.sock",
	}
	c, err := Load("htadb", []string{"--adb-server", "flag:3", "--mirror-fallback=false"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.AdbServer != "flag:3" {
		t.Errorf("flag should win: %s", c.AdbServer)
	}
	if c.CORSOrigin != "https://toml" || c.CLI != false || c.MirrorDefaultVersion != "4.1" || c.MirrorFallback {
		t.Errorf("toml: %+v", c)
	}
	if len(c.Listen) != 2 || c.Listen[1] != "unix:///tmp/h.sock" {
		t.Errorf("env list: %v", c.Listen)
	}
	if len(c.TLSSAN) != 1 || c.TLSSAN[0] != "a.test" {
		t.Errorf("toml list: %v", c.TLSSAN)
	}
	d := c.Devices["S1"]
	if d.Version != "4.0" || d.Options["max_size"] != "800" {
		t.Errorf("devices: %+v", c.Devices)
	}
	delete(env, "HTADB_ADB_SERVER")
	c, _ = Load("htadb", nil, func(k string) string { return env[k] }, io.Discard)
	if c.AdbServer != "toml:1" {
		t.Errorf("toml: %s", c.AdbServer)
	}
	env["HTADB_ADB_SERVER"] = "env:2"
	c, _ = Load("htadb", nil, func(k string) string { return env[k] }, io.Discard)
	if c.AdbServer != "env:2" {
		t.Errorf("env over toml: %s", c.AdbServer)
	}
}

func TestUnknownSetting(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("nope = 1\n"), 0o600)
	if _, err := Load("htadb", []string{"--config-dir", dir}, func(string) string { return "" }, io.Discard); err == nil {
		t.Fatal("expected unknown setting error")
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load("htadb", []string{"--config-dir", t.TempDir()}, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen[0] != "http://127.0.0.1:10443" || !c.CLI || !c.UI || !c.MirrorFallback || c.CORSOrigin != "*" {
		t.Fatalf("%+v", c)
	}
}
