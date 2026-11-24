// Command htadb exposes adb over REST and WebSocket, and serves a browser
// terminal and device mirroring.
//
//	htadb [flags]            run the adapter
//	htadb tls print-ca       print the local CA path and fingerprint
//	htadb tls regenerate     force a new TLS leaf certificate
//	htadb version            print the version
//	htadb licenses           print third-party notices
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"htadb/internal/adb"
	"htadb/internal/api"
	"htadb/internal/cli"
	"htadb/internal/config"
	"htadb/internal/listen"
	"htadb/internal/mirror"
	"htadb/internal/mirror/servers"
	"htadb/internal/ui"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	api.Version = version
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-version":
			fmt.Fprintln(stdout, "htadb", version)
			return 0
		case "tls":
			return tlsCmd(args[1:], stdout, stderr)
		case "licenses":
			return licenses(stdout)
		}
	}
	cfg, err := config.Load("htadb", args, nil, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "htadb:", err)
		return 2
	}
	log := newLogger(cfg, stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, cfg, log, stdout); err != nil {
		log.Error("fatal", "err", err)
		return 1
	}
	return 0
}

func newLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	o := &slog.HandlerOptions{Level: lvl}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, o))
	}
	return slog.New(slog.NewTextHandler(w, o))
}

func tlsDir(cfg *config.Config) (string, error) {
	return listen.DefaultTLSDir(cfg.ConfigDir)
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger, stdout io.Writer) error {
	slog.SetDefault(log)
	var specs []listen.Spec
	for _, l := range cfg.Listen {
		s, err := listen.Parse(l)
		if err != nil {
			return err
		}
		specs = append(specs, s)
	}
	unixMode, err := cfg.UnixModeBits()
	if err != nil {
		return err
	}
	td, err := tlsDir(cfg)
	if err != nil {
		return err
	}

	client := adb.New(cfg.AdbServer)
	var runner *cli.Runner
	if r, err := cli.New(cfg.AdbPath, cfg.AdbServer); err == nil {
		client.StartServer = func(ctx context.Context) error {
			log.Info("adb server not running; starting it", "adb", r.Path)
			sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			return r.StartServer(sctx)
		}
		if cfg.CLI {
			runner = r
		}
	} else if cfg.CLI {
		log.Warn("adb binary not found; CLI passthrough disabled and the adb server will not be auto-started", "adb-path", cfg.AdbPath)
	}

	reg, err := servers.NewRegistry(servers.Embedded(), cfg.MirrorServer, cfg.MirrorAllowUntested)
	if err != nil {
		return err
	}
	for _, w := range reg.Warn {
		log.Warn("mirroring server build skipped", "reason", w)
	}
	if len(reg.List()) == 0 {
		log.Info("no mirroring server builds embedded or supplied; mirroring will use jars already on devices (see --mirror-server)")
	}
	mm := mirror.New(client, reg, log.With("component", "mirror"))
	mm.DefaultVersion = cfg.MirrorDefaultVersion
	mm.AllowUntested = cfg.MirrorAllowUntested
	mm.Fallback = cfg.MirrorFallback
	mm.Devices = cfg.Devices

	srv := &api.Server{ADB: client, Log: log, CLI: runner, Mirror: mm}
	for _, o := range strings.Split(cfg.CORSOrigin, ",") {
		if o = strings.TrimSpace(o); o != "" {
			srv.CORSOrigins = append(srv.CORSOrigins, o)
		}
	}
	if cfg.UI {
		uo := ui.Options{Dir: cfg.UIDir, Overrides: map[string]string{}}
		for _, o := range cfg.UIOverride {
			p, d, err := ui.ParseOverride(o)
			if err != nil {
				return err
			}
			uo.Overrides[p] = d
		}
		if cfg.UIConfig != "" {
			b, err := os.ReadFile(cfg.UIConfig)
			if err != nil {
				return fmt.Errorf("--ui-config: %w", err)
			}
			if err := json.Unmarshal(b, &uo.Config); err != nil {
				return fmt.Errorf("--ui-config: %w", err)
			}
		}
		h, err := ui.Handler(uo)
		if err != nil {
			return err
		}
		srv.UI = h
		if cfg.UIDir == "" && !ui.Embedded() {
			log.Warn("this binary has no embedded web UI; run `make htadb` or pass --ui-dir ./web")
		}
	}
	handler := srv.Handler()
	srv.Tracker.OnEvent(mm.OnDeviceEvent)
	go srv.Tracker.Run(ctx)

	lns, err := listen.OpenAll(specs, unixMode, listen.TLSOptions{
		Dir: td, CertFile: cfg.TLSCert, KeyFile: cfg.TLSKey, ExtraSANs: cfg.TLSSAN, Logger: log,
	}, log)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: handler, ReadHeaderTimeout: 15 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug)}
	var wg sync.WaitGroup
	errc := make(chan error, len(lns))
	for _, l := range lns {
		log.Info("listening", "url", l.URL, "ui", cfg.UI)
		if cfg.Announce {
			fmt.Fprintf(stdout, "HTADB_LISTENING %s\n", l.URL)
		}
		wg.Add(1)
		go func(l *listen.Listener) {
			defer wg.Done()
			if err := hs.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(l)
	}
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		hs.Close()
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hs.Shutdown(sctx)
	hs.Close()
	wg.Wait()
	return nil
}

func tlsCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: htadb tls print-ca|regenerate [flags]")
		return 2
	}
	sub := args[0]
	cfg, err := config.Load("htadb tls "+sub, args[1:], nil, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "htadb:", err)
		return 2
	}
	td, err := tlsDir(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "htadb:", err)
		return 1
	}
	switch sub {
	case "print-ca":
		p, fp, err := listen.CAInfo(td)
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "htadb: no CA yet at %s (it is created when an https:// listener is first used, or by `htadb tls regenerate`)\n", p)
			return 1
		}
		if err != nil {
			fmt.Fprintln(stderr, "htadb:", err)
			return 1
		}
		fmt.Fprintf(stdout, "CA certificate: %s\nSHA-256 fingerprint: %s\n", p, fp)
		return 0
	case "regenerate":
		var specs []listen.Spec
		for _, l := range cfg.Listen {
			if s, err := listen.Parse(l); err == nil {
				specs = append(specs, s)
			}
		}
		if _, err := listen.EnsureLeaf(listen.TLSOptions{Dir: td, ExtraSANs: cfg.TLSSAN, ListenSpecs: specs, Force: true}); err != nil {
			fmt.Fprintln(stderr, "htadb:", err)
			return 1
		}
		p, fp, _ := listen.CAInfo(td)
		fmt.Fprintf(stdout, "regenerated %s/%s (CA %s, %s)\n", td, listen.LeafFile, p, fp)
		return 0
	}
	fmt.Fprintf(stderr, "htadb: unknown tls command %q\n", sub)
	return 2
}

func licenses(w io.Writer) int {
	fmt.Fprintln(w, "htadb", version)
	fmt.Fprintln(w, "\nThird-party components:\n  github.com/coder/websocket (ISC)\n  xterm.js (MIT), served to browsers")
	f := servers.Embedded()
	for _, n := range []string{"NOTICE-scrcpy", "LICENSE-scrcpy"} {
		if b, err := fs.ReadFile(f, n); err == nil {
			fmt.Fprintf(w, "\n==== %s ====\n%s", n, b)
		}
	}
	return 0
}
