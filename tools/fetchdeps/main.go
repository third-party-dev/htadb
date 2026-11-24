// Command fetchdeps downloads htadb's pinned third-party assets and verifies
// their sha256:
//
//   - xterm.js (npm @xterm/xterm tarball) → web/vendor/xterm/
//   - the newest mirroring server jar of each profile (scrcpy releases)
//     → internal/mirror/servers/embedded/scrcpy-server-v<version>.jar
//
// Usage: go run ./fetchdeps [-root ..] [-all-jars] [-jars-dir dir]
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type asset struct {
	name, url, sha256 string
}

var xterm = asset{
	name:   "xterm-6.0.0.tgz",
	url:    "https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz",
	sha256: "908e66e04af6c8dc6b00dd3b54de088e2e81e5ed866284fd6c2fb3c2d1c7a3f6",
}

// Server jars: sha256 from each scrcpy tag's install_release.sh (also pinned
// in internal/mirror/servers.Pinned).
var jars = []struct {
	version, sha256 string
	embed           bool // newest of its profile
}{
	{"5.0.1", "764eb6f79811d5211fe9df341120882ba9994c7a61b897d7bf3fb662e53bc536", true},
	{"5.0", "26cbc9ad0aced6c2282455bef4fb43462605c1f8758c74b4ab1dbf818c229daa", false},
	{"4.1", "deacb991ed2509715160ffdc7907e47b4160eb30d1566217e9047fd5b8850cae", true},
	{"4.0", "84924bd564a1eb6089c872c7521f968058977f91f5ff02514a8c74aff3210f3a", false},
}

func main() {
	root := flag.String("root", ".", "repository root")
	allJars := flag.Bool("all-jars", false, "also fetch non-embedded jars (into -jars-dir)")
	jarsDir := flag.String("jars-dir", "", "directory for non-embedded jars (default <root>/testdata-gen/jars)")
	cache := flag.String("cache", "", "download cache directory (default <root>/.cache/fetchdeps)")
	flag.Parse()
	if *cache == "" {
		*cache = filepath.Join(*root, ".cache", "fetchdeps")
	}
	if *jarsDir == "" {
		*jarsDir = filepath.Join(*root, "testdata-gen", "jars")
	}
	if err := run(*root, *cache, *jarsDir, *allJars); err != nil {
		fmt.Fprintln(os.Stderr, "fetchdeps:", err)
		os.Exit(1)
	}
}

func run(root, cache, jarsDir string, allJars bool) error {
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	tgz, err := fetch(cache, xterm)
	if err != nil {
		return err
	}
	if err := extractXterm(tgz, filepath.Join(root, "web", "vendor", "xterm")); err != nil {
		return err
	}
	fmt.Println("xterm.js 6.0.0 → web/vendor/xterm/")
	embedDir := filepath.Join(root, "internal", "mirror", "servers", "embedded")
	for _, j := range jars {
		if !j.embed && !allJars {
			continue
		}
		a := asset{
			name:   "scrcpy-server-v" + j.version,
			url:    fmt.Sprintf("https://github.com/Genymobile/scrcpy/releases/download/v%s/scrcpy-server-v%s", j.version, j.version),
			sha256: j.sha256,
		}
		p, err := fetch(cache, a)
		if err != nil {
			return err
		}
		dst := filepath.Join(jarsDir, "scrcpy-server-v"+j.version+".jar")
		if j.embed {
			dst = filepath.Join(embedDir, "scrcpy-server-v"+j.version+".jar")
		}
		if err := copyFile(p, dst); err != nil {
			return err
		}
		fmt.Printf("scrcpy-server %s → %s\n", j.version, dst)
	}
	return nil
}

func fetch(cache string, a asset) (string, error) {
	p := filepath.Join(cache, a.name)
	if sum, err := fileSHA(p); err == nil && sum == a.sha256 {
		return p, nil
	}
	cl := &http.Client{Timeout: 5 * time.Minute}
	resp, err := cl.Get(a.url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s: HTTP %d", a.url, resp.StatusCode)
	}
	tmp := p + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != a.sha256 {
		os.Remove(tmp)
		return "", fmt.Errorf("%s: sha256 mismatch: got %s, want %s", a.name, got, a.sha256)
	}
	return p, os.Rename(tmp, p)
}

func fileSHA(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func extractXterm(tgz, dst string) error {
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	want := map[string]string{
		"package/lib/xterm.js":     "xterm.js",
		"package/lib/xterm.js.map": "xterm.js.map",
		"package/css/xterm.css":    "xterm.css",
		"package/LICENSE":          "LICENSE",
		"package/package.json":     "package.json",
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	found := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, ok := want[h.Name]
		if !ok || strings.Contains(name, "..") {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, name), b, 0o644); err != nil {
			return err
		}
		found++
	}
	if found != len(want) {
		return fmt.Errorf("xterm tarball: found %d of %d files", found, len(want))
	}
	return nil
}
