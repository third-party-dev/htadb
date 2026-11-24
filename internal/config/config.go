// Package config resolves htadb settings from flags, HTADB_* environment
// variables and config.toml (in that order of precedence).
package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Config is htadb's resolved configuration.
type Config struct {
	Listen   []string
	UnixMode string

	AdbServer string
	AdbPath   string

	CLI        bool
	CORSOrigin string
	UI         bool
	UIDir      string
	UIOverride []string // "/vendor/xterm=<dir>"
	UIConfig   string   // JSON file served as config.js

	TLSCert string
	TLSKey  string
	TLSSAN  []string

	ConfigDir  string
	ConfigFile string

	MirrorServer         []string
	MirrorDefaultVersion string
	MirrorAllowUntested  bool
	MirrorFallback       bool

	LogLevel  string
	LogFormat string
	Announce  bool

	// Devices holds per-device settings from config.toml
	// ([mirror.devices."<serial>"]).
	Devices map[string]DeviceConfig
}

// DeviceConfig is a per-device entry.
type DeviceConfig struct {
	Version string            `json:"version,omitempty"`
	Options map[string]string `json:"options,omitempty"`
}

type strList struct{ p *[]string }

func (s strList) String() string {
	if s.p == nil {
		return ""
	}
	return strings.Join(*s.p, ",")
}
func (s strList) Set(v string) error { *s.p = append(*s.p, v); return nil }

// FlagSet builds the flag set bound to c.
func (c *Config) FlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Var(strList{&c.Listen}, "listen", "listener spec, repeatable: http://host:port, https://host:port, unix:///path, systemd: (default http://127.0.0.1:10443)")
	fs.StringVar(&c.UnixMode, "unix-mode", "0660", "permission bits for unix socket listeners")
	fs.StringVar(&c.AdbServer, "adb-server", "127.0.0.1:5037", "adb server address")
	fs.StringVar(&c.AdbPath, "adb-path", "adb", "adb binary (starts the adb server, CLI passthrough)")
	fs.BoolVar(&c.CLI, "cli", true, "enable /api/v1/cli adb passthrough")
	fs.StringVar(&c.CORSOrigin, "cors-origin", "*", "allowed CORS origin(s), comma-separated; empty disables CORS")
	fs.BoolVar(&c.UI, "ui", true, "serve the web UI under /ui/")
	fs.StringVar(&c.UIDir, "ui-dir", "", "serve the UI from this directory instead of the embedded copy")
	fs.Var(strList{&c.UIOverride}, "ui-override", "serve a UI sub-path from a directory, e.g. /vendor/xterm=/path (repeatable)")
	fs.StringVar(&c.UIConfig, "ui-config", "", "JSON file exposed to pages as config.js")
	fs.StringVar(&c.TLSCert, "tls-cert", "", "TLS certificate (PEM) for https listeners")
	fs.StringVar(&c.TLSKey, "tls-key", "", "TLS key (PEM) for https listeners")
	fs.Var(strList{&c.TLSSAN}, "tls-san", "extra SAN for the generated certificate (repeatable)")
	fs.StringVar(&c.ConfigDir, "config-dir", "", "config directory (default <user config dir>/htadb)")
	fs.StringVar(&c.ConfigFile, "config-file", "", "config.toml path (default <config dir>/config.toml)")
	fs.Var(strList{&c.MirrorServer}, "mirror-server", "mirroring server jar, [version=]path (repeatable)")
	fs.StringVar(&c.MirrorDefaultVersion, "mirror-default-version", "", "default mirroring server version")
	fs.BoolVar(&c.MirrorAllowUntested, "mirror-allow-untested", false, "allow server versions outside the tested allowlist")
	fs.BoolVar(&c.MirrorFallback, "mirror-fallback", true, "fall back to the newest 4.x server when 5.0 cannot run")
	fs.StringVar(&c.LogLevel, "log-level", "info", "debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", "text", "text or json")
	fs.BoolVar(&c.Announce, "announce", false, "print \"HTADB_LISTENING <url>\" lines on stdout once listening")
	return fs
}

// EnvName maps a flag name to its environment variable.
func EnvName(flagName string) string {
	return "HTADB_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

var listFlags = map[string]bool{"listen": true, "ui-override": true, "tls-san": true, "mirror-server": true}

// Load parses args and fills unset flags from the environment and config.toml.
func Load(name string, args []string, getenv func(string) string, stderr io.Writer) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := &Config{}
	fs := c.FlagSet(name)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	apply := func(name, val string) error {
		f := fs.Lookup(name)
		if listFlags[name] {
			for _, v := range strings.Split(val, ",") {
				if v = strings.TrimSpace(v); v != "" {
					f.Value.Set(v)
				}
			}
			return nil
		}
		return f.Value.Set(val)
	}
	// Environment first (it may set the config dir/file).
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] || envErr != nil {
			return
		}
		if v := getenv(EnvName(f.Name)); v != "" {
			if err := apply(f.Name, v); err != nil {
				envErr = fmt.Errorf("%s: %w", EnvName(f.Name), err)
			}
			set[f.Name] = true
		}
	})
	if envErr != nil {
		return nil, envErr
	}

	if c.ConfigDir == "" {
		if d, err := os.UserConfigDir(); err == nil {
			c.ConfigDir = filepath.Join(d, "htadb")
		}
	}
	path := c.ConfigFile
	if path == "" && c.ConfigDir != "" {
		path = filepath.Join(c.ConfigDir, "config.toml")
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && (!os.IsNotExist(err) || c.ConfigFile != "") {
			return nil, err
		}
		if err == nil {
			tree, err := ParseTOML(string(b))
			if err != nil {
				return nil, err
			}
			if err := c.applyTOML(fs, tree, set, apply); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	if len(c.Listen) == 0 {
		c.Listen = []string{"http://127.0.0.1:10443"}
	}
	return c, nil
}

func scalarString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	}
	return "", false
}

func (c *Config) applyTOML(fs *flag.FlagSet, tree map[string]any, set map[string]bool, apply func(string, string) error) error {
	// Top-level keys and [section] keys map to flags: listen, adb-server or
	// adb_server; [mirror] default-version → mirror-default-version, etc.
	var walk func(prefix string, m map[string]any) error
	walk = func(prefix string, m map[string]any) error {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := m[k]
			name := strings.ReplaceAll(k, "_", "-")
			if prefix != "" {
				name = prefix + "-" + name
			}
			if prefix == "mirror" && k == "devices" {
				dm, ok := v.(map[string]any)
				if !ok {
					return fmt.Errorf("mirror.devices must be a table")
				}
				if err := c.applyDevices(dm); err != nil {
					return err
				}
				continue
			}
			if sub, ok := v.(map[string]any); ok {
				if err := walk(name, sub); err != nil {
					return err
				}
				continue
			}
			f := fs.Lookup(name)
			if f == nil {
				return fmt.Errorf("unknown setting %q", name)
			}
			if set[name] {
				continue
			}
			if arr, ok := v.([]any); ok {
				if !listFlags[name] {
					return fmt.Errorf("%s: expected a single value", name)
				}
				for _, e := range arr {
					s, ok := scalarString(e)
					if !ok {
						return fmt.Errorf("%s: bad list element", name)
					}
					f.Value.Set(s)
				}
				continue
			}
			s, ok := scalarString(v)
			if !ok {
				return fmt.Errorf("%s: unsupported value", name)
			}
			if err := apply(name, s); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		return nil
	}
	return walk("", tree)
}

func (c *Config) applyDevices(dm map[string]any) error {
	if c.Devices == nil {
		c.Devices = map[string]DeviceConfig{}
	}
	for serial, v := range dm {
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("mirror.devices.%q must be a table", serial)
		}
		dc := DeviceConfig{Options: map[string]string{}}
		for k, x := range m {
			s, ok := scalarString(x)
			if !ok {
				return fmt.Errorf("mirror.devices.%q.%s: unsupported value", serial, k)
			}
			if k == "version" {
				dc.Version = s
			} else {
				dc.Options[k] = s
			}
		}
		c.Devices[serial] = dc
	}
	return nil
}

// UnixModeBits parses UnixMode as octal.
func (c *Config) UnixModeBits() (os.FileMode, error) {
	n, err := strconv.ParseUint(c.UnixMode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("bad --unix-mode %q", c.UnixMode)
	}
	return os.FileMode(n), nil
}
