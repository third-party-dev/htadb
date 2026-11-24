// Package mirror runs mirroring sessions: it launches the mirroring server
// (the scrcpy server, speaking the scrcpy protocol) with tunnel_forward=true,
// connects its localabstract sockets through adb and multiplexes them onto
// one WebSocket.
package mirror

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"htadb/internal/adb"
	"htadb/internal/config"
	"htadb/internal/mirror/profile"
	"htadb/internal/mirror/servers"
)

// Error is a mirroring error with an HTTP status (used before the WebSocket
// is accepted).
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string     { return e.Msg }
func (e *Error) HTTPStatus() int   { return e.Status }
func (e *Error) ErrorCode() string { return e.Code }
func errf(status int, code, f string, a ...any) *Error {
	return &Error{Status: status, Code: code, Msg: fmt.Sprintf(f, a...)}
}

// Manager owns mirroring state shared across sessions.
type Manager struct {
	ADB            *adb.Client
	Registry       *servers.Registry
	Jars           *servers.Manager
	Log            *slog.Logger
	DefaultVersion string
	AllowUntested  bool
	Fallback       bool
	Devices        map[string]config.DeviceConfig

	// Timing knobs (tests shorten them).
	ConnectAttempts  int
	ConnectDelay     time.Duration
	FirstPacketWait  time.Duration
	HandshakeTimeout time.Duration

	mu       sync.Mutex
	fallback map[string]FallbackInfo
}

// FallbackInfo is the remembered per-device launch fallback.
type FallbackInfo struct {
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// New builds a manager with default timings.
func New(c *adb.Client, reg *servers.Registry, log *slog.Logger) *Manager {
	return &Manager{
		ADB: c, Registry: reg, Jars: &servers.Manager{ADB: c, Registry: reg}, Log: log,
		Fallback: true, ConnectAttempts: 100, ConnectDelay: 100 * time.Millisecond,
		FirstPacketWait: 20 * time.Second, HandshakeTimeout: 10 * time.Second,
		fallback: map[string]FallbackInfo{},
	}
}

// ---- api.Mirror ----

// HostServers lists the builds htadb holds and the known profiles.
func (m *Manager) HostServers() any {
	type prof struct {
		Name     string   `json:"name"`
		Versions []string `json:"versions"`
		Newest   string   `json:"newest"`
		MinSdk   int      `json:"minSdk"`
	}
	var ps []prof
	for _, p := range profile.Profiles {
		var vs []string
		for v := range p.Versions {
			vs = append(vs, v)
		}
		sort.Slice(vs, func(i, j int) bool { return profile.Compare(vs[i], vs[j]) > 0 })
		ps = append(ps, prof{p.Name, vs, p.Newest(), p.MinSdk})
	}
	builds := m.Registry.List()
	if builds == nil {
		builds = []*servers.Build{}
	}
	return map[string]any{
		"builds": builds, "profiles": ps, "defaultProfile": profile.DefaultProfile,
		"defaultVersion": m.DefaultVersion, "fallback": m.Fallback, "allowUntested": m.AllowUntested,
		"deviceDir": servers.DeviceDir,
	}
}

// DeviceServers lists jars on the device.
func (m *Manager) DeviceServers(ctx context.Context, serial string) (any, error) {
	jars, err := m.Jars.Inventory(ctx, serial)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"dir": servers.DeviceDir, "jars": jars}
	m.mu.Lock()
	if f, ok := m.fallback[serial]; ok {
		res["fallback"] = f
	}
	m.mu.Unlock()
	return res, nil
}

// ClearFallback forgets the remembered fallback for a device.
func (m *Manager) ClearFallback(serial string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.fallback[serial]
	delete(m.fallback, serial)
	return ok
}

// DeviceReconnected is called by the device tracker.
func (m *Manager) DeviceReconnected(serial string) {
	if m.ClearFallback(serial) {
		m.Log.Info("device reconnected; cleared remembered mirroring fallback", "serial", serial)
	}
}

// OnDeviceEvent adapts tracker events.
func (m *Manager) OnDeviceEvent(e adb.DeviceEvent) {
	switch {
	case e.Type == "remove":
		m.DeviceReconnected(e.Device.Serial)
	case e.Type == "add" && e.Device.State == "device":
		m.DeviceReconnected(e.Device.Serial)
	case e.Type == "state" && e.Device.State == "device":
		m.DeviceReconnected(e.Device.Serial)
	}
}

// RememberedFallback returns the fallback memory for a device.
func (m *Manager) RememberedFallback(serial string) (FallbackInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.fallback[serial]
	return f, ok
}

func (m *Manager) remember(serial string, f FallbackInfo) {
	m.mu.Lock()
	m.fallback[serial] = f
	m.mu.Unlock()
}

// ---- planning ----

// Plan is a resolved session request.
type Plan struct {
	Serial   string
	Version  string
	Spec     *profile.Spec
	Explicit bool   // never falls back
	Source   string // query, server_path, device-config, default, newest
	Build    *servers.Build
	JarPath  string // device path to launch from
	Verified bool
	Warnings []string
	Launch   bool
	SCID     uint32
	Options  map[string]string
	SDK      int

	FallbackFrom   string
	FallbackReason string
	FallbackDetail string

	userOptions map[string]string
	deviceJars  []servers.DeviceJar
}

var reservedQuery = map[string]bool{"version": true, "server_path": true, "launch": true, "scid": true, "api": true, "serial": true}

// Prepare resolves version, jar and options for a session.
func (m *Manager) Prepare(ctx context.Context, serial string, q url.Values) (any, error) {
	return m.prepare(ctx, serial, q)
}

func (m *Manager) prepare(ctx context.Context, serial string, q url.Values) (*Plan, error) {
	if _, err := m.ADB.Features(ctx, serial); err != nil {
		if adb.IsDeviceNotFound(err) {
			return nil, errf(404, "device_not_found", "device %q not found", serial)
		}
		return nil, err
	}
	p := &Plan{Serial: serial, Launch: q.Get("launch") != "false" && q.Get("launch") != "0", userOptions: map[string]string{}}
	for k, vs := range q {
		if reservedQuery[k] || len(vs) == 0 {
			continue
		}
		p.userOptions[k] = vs[len(vs)-1]
	}
	dc := m.Devices[serial]

	// Version selection, highest priority first.
	switch {
	case q.Get("version") != "":
		p.Version, p.Source, p.Explicit = q.Get("version"), "query", true
	case q.Get("server_path") != "":
		sp := q.Get("server_path")
		v, ok := servers.ParseFileName(path.Base(sp))
		if !ok {
			return nil, errf(400, "bad_request", "cannot infer the server version from %q; add ?version=", sp)
		}
		p.Version, p.Source, p.Explicit = v, "server_path", true
	case dc.Version != "":
		p.Version, p.Source, p.Explicit = dc.Version, "device-config", true
	case m.DefaultVersion != "":
		p.Version, p.Source = m.DefaultVersion, "default"
	default:
		p.Source = "newest"
	}
	if p.Launch && q.Get("scid") != "" {
		return nil, errf(400, "bad_request", "scid is only accepted with launch=false (attach mode)")
	}
	if !p.Launch {
		if q.Get("scid") == "" || q.Get("version") == "" {
			return nil, errf(400, "bad_request", "attach mode (launch=false) needs scid and version")
		}
		n, err := strconv.ParseUint(q.Get("scid"), 16, 32)
		if err != nil || n > 0x7fffffff {
			return nil, errf(400, "bad_request", "scid must be a 31-bit hex number")
		}
		p.SCID = uint32(n)
	}
	if p.Launch && p.Source != "query" && p.Source != "server_path" {
		jars, err := m.Jars.Inventory(ctx, serial)
		if err == nil {
			p.deviceJars = jars
		}
	}
	if p.Source == "newest" {
		p.Version = m.newestAvailable("", p.deviceJars)
		if p.Version == "" {
			return nil, errf(404, "server_not_found", "no mirroring server build is available: build htadb with embedded jars (make fetchdeps) or pass --mirror-server")
		}
	}

	// Remembered launch fallback (only when htadb chose the version).
	if !p.Explicit && m.Fallback && p.Launch {
		if f, ok := m.RememberedFallback(serial); ok && f.From == p.Version {
			if v := m.newestAvailable(profile.FallbackProfile, p.deviceJars); v != "" {
				p.FallbackFrom, p.FallbackReason, p.FallbackDetail = p.Version, "launch", "remembered: "+f.Reason
				p.Version = v
			}
		}
	}

	spec, err := profile.Lookup(p.Version, m.AllowUntested)
	if err != nil {
		return nil, err
	}
	p.Spec = spec
	if !spec.Tested {
		p.Warnings = append(p.Warnings, fmt.Sprintf("server %s is not in the tested allowlist; using the %s profile", p.Version, spec.Profile))
	}

	// SDK check against minSdk.
	if p.Launch {
		if sdkStr, err := m.ADB.GetProp(ctx, serial, "ro.build.version.sdk"); err == nil {
			p.SDK, _ = strconv.Atoi(sdkStr)
		}
		if p.SDK > 0 && p.SDK < spec.MinSdk {
			if p.Explicit || !m.Fallback || spec.Profile == profile.FallbackProfile {
				return nil, errf(409, "incompatible_version", "device SDK %d is below server %s's minimum (%d)", p.SDK, p.Version, spec.MinSdk)
			}
			v := m.newestAvailable(profile.FallbackProfile, p.deviceJars)
			fs, ferr := profile.Lookup(v, m.AllowUntested)
			if v == "" || ferr != nil || p.SDK < fs.MinSdk {
				return nil, errf(409, "incompatible_version", "device SDK %d is below every available server's minimum", p.SDK)
			}
			p.FallbackFrom, p.FallbackReason, p.FallbackDetail = p.Version, "sdk", fmt.Sprintf("SDK %d < %d", p.SDK, spec.MinSdk)
			p.Version, p.Spec = v, fs
		}
	}
	if err := m.resolveJar(ctx, p, q.Get("server_path")); err != nil {
		return nil, err
	}
	if err := m.resolveOptions(p, dc); err != nil {
		return nil, err
	}
	return p, nil
}

// newestAvailable picks the newest version (optionally within a profile)
// among host builds and device jars.
func (m *Manager) newestAvailable(profileName string, jars []servers.DeviceJar) string {
	best := ""
	consider := func(v string) {
		spec, err := profile.Lookup(v, m.AllowUntested)
		if err != nil || (profileName != "" && spec.Profile != profileName) {
			return
		}
		if best == "" || profile.Compare(v, best) > 0 {
			best = v
		}
	}
	for _, b := range m.Registry.List() {
		consider(b.Version)
	}
	for _, j := range jars {
		consider(j.Version)
	}
	return best
}

func (m *Manager) resolveJar(ctx context.Context, p *Plan, serverPath string) error {
	if !p.Launch {
		return nil
	}
	if serverPath != "" {
		if !strings.HasPrefix(serverPath, "/") {
			return errf(400, "bad_request", "server_path must be absolute")
		}
		st, err := m.ADB.Stat(ctx, p.Serial, serverPath)
		if err != nil {
			return err
		}
		if !st.Exists() {
			return errf(404, "server_not_found", "%s does not exist on the device", serverPath)
		}
		p.JarPath = serverPath
		p.Warnings = append(p.Warnings, "using pre-installed server "+serverPath+"; its version is taken from its name and not verified")
		return nil
	}
	if b := m.Registry.Get(p.Version); b != nil {
		p.Build, p.Verified = b, true
		p.JarPath = servers.DevicePath(p.Version)
		return nil
	}
	if p.deviceJars == nil {
		p.deviceJars, _ = m.Jars.Inventory(ctx, p.Serial)
	}
	if j := servers.Find(p.deviceJars, p.Version); j != nil {
		p.JarPath, p.Verified = j.Path, j.Verified
		if !j.Verified {
			p.Warnings = append(p.Warnings, fmt.Sprintf("htadb has no build of %s; using the jar already on the device (%s), version trusted from its name", p.Version, j.Name))
		}
		return nil
	}
	return errf(404, "server_not_found", "no mirroring server %s on the host or the device", p.Version)
}

// Defaults applied to every session (the browser decodes H.264 only).
var defaultOptions = map[string]string{"video_codec": "h264", "audio": "false", "control": "true"}

func (m *Manager) resolveOptions(p *Plan, dc config.DeviceConfig) error {
	opts := map[string]string{}
	for k, v := range defaultOptions {
		opts[k] = v
	}
	for k, v := range dc.Options {
		opts[k] = v
	}
	for k, v := range p.userOptions {
		opts[k] = v
	}
	for k, v := range opts {
		if err := p.Spec.CheckOption(k, v); err != nil {
			return errf(400, "bad_request", "%v", err)
		}
	}
	p.Options = opts
	return nil
}

// fallbackPlan derives the 4.x retry plan.
func (m *Manager) fallbackPlan(ctx context.Context, p *Plan, detail string) (*Plan, error) {
	v := m.newestAvailable(profile.FallbackProfile, p.deviceJars)
	if v == "" {
		return nil, fmt.Errorf("no %s server available for fallback", profile.FallbackProfile)
	}
	spec, err := profile.Lookup(v, m.AllowUntested)
	if err != nil {
		return nil, err
	}
	np := &Plan{
		Serial: p.Serial, Version: v, Spec: spec, Source: p.Source, Launch: true, SDK: p.SDK,
		FallbackFrom: p.Version, FallbackReason: "launch", FallbackDetail: detail,
		userOptions: p.userOptions, deviceJars: p.deviceJars,
	}
	if err := m.resolveJar(ctx, np, ""); err != nil {
		return nil, err
	}
	// Options valid for 5.0 may not exist in 4.x: drop those with a warning.
	opts := map[string]string{}
	for k, val := range p.Options {
		if spec.CheckOption(k, val) == nil {
			opts[k] = val
		} else {
			np.Warnings = append(np.Warnings, "option "+k+" is not supported by server "+v+"; dropped")
		}
	}
	np.Options = opts
	return np, nil
}
