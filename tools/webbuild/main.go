// Command webbuild prepares the web UI for embedding (internal/ui/dist).
//
//	-mode dev   copy web/ through unchanged (readable sources)
//	-mode min   bundle each page's entry module into one file, minify it and
//	            write sourcemaps; copy HTML, CSS and vendor files
//
// Uses esbuild's Go API, so no Node toolchain is needed.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Entry modules, one per page. config.js stays external: htadb generates it
// at runtime.
var entries = []string{"index.js", "terminal.js", "mirror.js"}

func main() {
	src := flag.String("src", "web", "web source directory")
	out := flag.String("out", "internal/ui/dist", "output directory")
	mode := flag.String("mode", "min", "dev or min")
	flag.Parse()
	if err := run(*src, *out, *mode); err != nil {
		fmt.Fprintln(os.Stderr, "webbuild:", err)
		os.Exit(1)
	}
}

func run(src, out, mode string) error {
	if mode != "dev" && mode != "min" {
		return fmt.Errorf("unknown mode %q", mode)
	}
	if _, err := os.Stat(filepath.Join(src, "vendor", "xterm", "xterm.js")); err != nil {
		return fmt.Errorf("web/vendor/xterm is missing; run `make fetchdeps` first")
	}
	// Clean the output but keep the committed placeholder.
	ents, _ := os.ReadDir(out)
	for _, e := range ents {
		if e.Name() != ".keep" {
			if err := os.RemoveAll(filepath.Join(out, e.Name())); err != nil {
				return err
			}
		}
	}
	skip := func(rel string) bool {
		return strings.HasPrefix(rel, "test/") || rel == "test"
	}
	bundled := map[string]bool{}
	if mode == "min" {
		var eps []string
		for _, e := range entries {
			eps = append(eps, filepath.Join(src, e))
			bundled[e] = true
		}
		r := api.Build(api.BuildOptions{
			EntryPoints:       eps,
			Bundle:            true,
			Format:            api.FormatESModule,
			Target:            api.ES2022,
			MinifyWhitespace:  true,
			MinifyIdentifiers: true,
			MinifySyntax:      true,
			Sourcemap:         api.SourceMapLinked,
			Outdir:            out,
			Outbase:           src,
			External:          []string{"./config.js"},
			Write:             true,
			LogLevel:          api.LogLevelWarning,
			Banner:            map[string]string{"js": "// htadb web UI (minified; readable sources in web/)"},
		})
		if len(r.Errors) > 0 {
			return fmt.Errorf("esbuild: %s", r.Errors[0].Text)
		}
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dst := filepath.Join(out, rel)
		if d.IsDir() {
			if mode == "min" && rel == "lib" {
				return filepath.SkipDir // bundled into the pages
			}
			return os.MkdirAll(dst, 0o755)
		}
		if bundled[rel] {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}
