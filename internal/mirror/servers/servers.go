// Package servers manages mirroring server builds (scrcpy-server jars):
// the host-side registry (embedded and --mirror-server files), the
// on-device naming convention and inventory, and a push that tolerates jars
// left by earlier sessions.
package servers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"htadb/internal/adb"
	"htadb/internal/mirror/profile"
)

// DeviceDir is where htadb keeps jars on the device. The scrcpy CLI uses
// /data/local/tmp/scrcpy-server.jar; htadb never touches that file.
const DeviceDir = "/data/local/tmp/htadb"

// DevicePath is the on-device path for a version.
func DevicePath(version string) string {
	return DeviceDir + "/scrcpy-server-v" + version + ".jar"
}

// NameRE is the on-device naming pattern; the version sits at the end of the
// name, before the extension.
var NameRE = regexp.MustCompile(`^scrcpy-server-v(\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.]+)?)\.jar$`)

// upstreamRE also accepts the upstream release asset name (no extension).
var upstreamRE = regexp.MustCompile(`^scrcpy-server-v(\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.]+)?)(?:\.jar)?$`)

// ParseDeviceName returns the version encoded in an on-device file name.
func ParseDeviceName(name string) (string, bool) {
	m := NameRE.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ParseFileName accepts both the device naming convention and the upstream
// asset name ("scrcpy-server-v5.0").
func ParseFileName(name string) (string, bool) {
	m := upstreamRE.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Pinned are the sha256 sums of upstream release assets (from each tag's
// install_release.sh). They let htadb mark device jars as verified even when
// it does not hold that build itself.
var Pinned = map[string]string{
	"4.0":   "84924bd564a1eb6089c872c7521f968058977f91f5ff02514a8c74aff3210f3a",
	"4.1":   "deacb991ed2509715160ffdc7907e47b4160eb30d1566217e9047fd5b8850cae",
	"5.0":   "26cbc9ad0aced6c2282455bef4fb43462605c1f8758c74b4ab1dbf818c229daa",
	"5.0.1": "764eb6f79811d5211fe9df341120882ba9994c7a61b897d7bf3fb662e53bc536",
}

// Build is one server jar htadb holds.
type Build struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Profile string `json:"profile"`
	Tested  bool   `json:"tested"`
	Latest  bool   `json:"latest"` // newest build of its profile
	Source  string `json:"source"` // "embedded" or "file"
	Path    string `json:"path,omitempty"`
	// Upstream is true when SHA256 matches the pinned upstream release.
	Upstream bool `json:"upstream"`

	data []byte // embedded content
}

// Open returns the jar content.
func (b *Build) Open() (io.ReadCloser, error) {
	if b.data != nil {
		return io.NopCloser(bytes.NewReader(b.data)), nil
	}
	return os.Open(b.Path)
}

// Registry is the set of builds htadb can push.
type Registry struct {
	builds map[string]*Build
	Warn   []string // problems found while loading
}

// NewRegistry loads embedded jars and --mirror-server specs
// ("[version=]path"). Supplied files override embedded ones.
func NewRegistry(embedded fs.FS, supplied []string, allowUntested bool) (*Registry, error) {
	r := &Registry{builds: map[string]*Build{}}
	if embedded != nil {
		ents, _ := fs.ReadDir(embedded, ".")
		for _, e := range ents {
			v, ok := ParseFileName(e.Name())
			if !ok || e.IsDir() {
				continue
			}
			data, err := fs.ReadFile(embedded, e.Name())
			if err != nil {
				return nil, err
			}
			if err := r.add(&Build{Version: v, Source: "embedded", data: data, Size: int64(len(data)), SHA256: sum(data)}, allowUntested); err != nil {
				r.Warn = append(r.Warn, err.Error())
			}
		}
	}
	for _, s := range supplied {
		v, p, explicit := strings.Cut(s, "=")
		if !explicit || strings.ContainsAny(v, `/\`) {
			p = s
			var ok bool
			v, ok = ParseFileName(filepath.Base(p))
			if !ok {
				return nil, fmt.Errorf("--mirror-server %s: cannot infer the version from the file name; use --mirror-server <version>=<path>", s)
			}
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("--mirror-server: %w", err)
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return nil, err
		}
		if err := r.add(&Build{Version: v, Source: "file", Path: p, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, allowUntested); err != nil {
			return nil, fmt.Errorf("--mirror-server %s: %w", s, err)
		}
	}
	r.markLatest()
	return r, nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (r *Registry) add(b *Build, allowUntested bool) error {
	spec, err := profile.Lookup(b.Version, allowUntested)
	if err != nil {
		return err
	}
	b.Profile, b.Tested = spec.Profile, spec.Tested
	b.Upstream = Pinned[b.Version] == b.SHA256
	r.builds[b.Version] = b
	return nil
}

func (r *Registry) markLatest() {
	newest := map[string]*Build{}
	for _, b := range r.builds {
		b.Latest = false
		if n := newest[b.Profile]; n == nil || profile.Compare(b.Version, n.Version) > 0 {
			newest[b.Profile] = b
		}
	}
	for _, b := range newest {
		b.Latest = true
	}
}

// Get returns the build for an exact version.
func (r *Registry) Get(version string) *Build { return r.builds[version] }

// List returns builds, newest first.
func (r *Registry) List() []*Build {
	var out []*Build
	for _, b := range r.builds {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return profile.Compare(out[i].Version, out[j].Version) > 0 })
	return out
}

// Newest returns the newest build of a profile ("" = any profile).
func (r *Registry) Newest(profileName string) *Build {
	for _, b := range r.List() {
		if profileName == "" || b.Profile == profileName {
			return b
		}
	}
	return nil
}

// ---- device side ----

// DeviceJar is a jar found on the device.
type DeviceJar struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Version  string `json:"version"`
	Size     uint32 `json:"size"`
	Mtime    int64  `json:"mtime"`
	SHA256   string `json:"sha256,omitempty"`
	Verified bool   `json:"verified"`
	Profile  string `json:"profile,omitempty"`
	Tested   bool   `json:"tested"`
	// Known is true when htadb holds (or pins) a build of this version.
	Known bool `json:"known"`
}

// Manager pushes and inventories jars on devices.
type Manager struct {
	ADB      *adb.Client
	Registry *Registry

	locks sync.Map // serial/version → *sync.Mutex
}

func (m *Manager) lock(serial, version string) func() {
	v, _ := m.locks.LoadOrStore(serial+"\x00"+version, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// deviceSHA256 runs toybox sha256sum on the device for several paths.
func (m *Manager) deviceSHA256(ctx context.Context, serial string, paths []string) (map[string]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	var q []string
	for _, p := range paths {
		q = append(q, adb.ShellQuote(p))
	}
	r, err := m.ADB.RunShell(ctx, serial, "sha256sum "+strings.Join(q, " "))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(r.Stdout), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && len(f[0]) == 64 {
			out[f[1]] = strings.ToLower(f[0])
		}
	}
	return out, nil
}

// EnsureResult reports what Ensure did.
type EnsureResult struct {
	Path   string `json:"path"`
	Pushed bool   `json:"pushed"`
	Reason string `json:"reason"` // "reused", "missing", "size mismatch", "hash mismatch"
}

// Ensure makes sure the build is on the device at DevicePath(version):
//  1. a per-device, per-version mutex serializes check and push;
//  2. sync STAT; missing → push;
//  3. same size → sha256sum on the device; equal → reuse;
//  4. otherwise push to a hidden temp name and mv -f into place, so no
//     session ever launches a half-written jar.
func (m *Manager) Ensure(ctx context.Context, serial string, b *Build) (*EnsureResult, error) {
	unlock := m.lock(serial, b.Version)
	defer unlock()
	dst := DevicePath(b.Version)
	st, err := m.ADB.Stat(ctx, serial, dst)
	if err != nil {
		return nil, err
	}
	reason := "missing"
	if st.Exists() {
		reason = "size mismatch"
		if int64(st.Size) == b.Size {
			sums, err := m.deviceSHA256(ctx, serial, []string{dst})
			if err != nil {
				return nil, err
			}
			if sums[dst] == b.SHA256 {
				return &EnsureResult{Path: dst, Reason: "reused"}, nil
			}
			reason = "hash mismatch"
		}
	}
	var rb [6]byte
	rand.Read(rb[:])
	tmp := fmt.Sprintf("%s/.scrcpy-server-v%s.jar.%s.tmp", DeviceDir, b.Version, hex.EncodeToString(rb[:]))
	rc, err := b.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	sy, err := m.ADB.OpenSync(ctx, serial)
	if err != nil {
		return nil, err
	}
	_, err = sy.Send(tmp, 0o644, time.Now(), rc)
	sy.Close()
	if err != nil {
		m.ADB.RunShell(context.WithoutCancel(ctx), serial, "rm -f "+adb.ShellQuote(tmp))
		return nil, fmt.Errorf("pushing %s: %w", path.Base(dst), err)
	}
	r, err := m.ADB.RunShell(ctx, serial, "mv -f "+adb.ShellQuote(tmp)+" "+adb.ShellQuote(dst))
	if err != nil {
		return nil, err
	}
	if r.ExitCode > 0 {
		m.ADB.RunShell(context.WithoutCancel(ctx), serial, "rm -f "+adb.ShellQuote(tmp))
		return nil, fmt.Errorf("installing %s: %s", path.Base(dst), strings.TrimSpace(string(r.Stderr)+string(r.Stdout)))
	}
	return &EnsureResult{Path: dst, Pushed: true, Reason: reason}, nil
}

// Inventory lists jars in DeviceDir that follow the naming convention and
// marks each verified when its size and hash match a known build.
func (m *Manager) Inventory(ctx context.Context, serial string) ([]DeviceJar, error) {
	ents, err := m.ADB.List(ctx, serial, DeviceDir)
	if err != nil {
		return nil, err
	}
	var jars []DeviceJar
	var hashPaths []string
	for _, e := range ents {
		v, ok := ParseDeviceName(e.Name)
		if !ok || e.Type != "file" {
			continue
		}
		j := DeviceJar{Name: e.Name, Path: DeviceDir + "/" + e.Name, Version: v, Size: e.Size, Mtime: e.Mtime}
		if spec, err := profile.Lookup(v, true); err == nil {
			j.Profile, j.Tested = spec.Profile, spec.Tested
		}
		b := m.Registry.Get(v)
		_, pinned := Pinned[v]
		j.Known = b != nil || pinned
		if (b != nil && int64(e.Size) == b.Size) || (b == nil && pinned) {
			hashPaths = append(hashPaths, j.Path)
		}
		jars = append(jars, j)
	}
	sums, err := m.deviceSHA256(ctx, serial, hashPaths)
	if err != nil {
		return nil, err
	}
	for i := range jars {
		j := &jars[i]
		j.SHA256 = sums[j.Path]
		if j.SHA256 == "" {
			continue
		}
		if b := m.Registry.Get(j.Version); b != nil {
			j.Verified = b.SHA256 == j.SHA256
		} else {
			j.Verified = Pinned[j.Version] == j.SHA256
		}
	}
	sort.Slice(jars, func(a, b int) bool { return profile.Compare(jars[a].Version, jars[b].Version) > 0 })
	if jars == nil {
		jars = []DeviceJar{}
	}
	return jars, nil
}

// Find returns the device jar for an exact version, if present.
func Find(jars []DeviceJar, version string) *DeviceJar {
	for i := range jars {
		if jars[i].Version == version {
			return &jars[i]
		}
	}
	return nil
}
