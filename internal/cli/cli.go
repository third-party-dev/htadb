// Package cli runs the adb binary for /api/v1/cli passthrough. Commands are
// started with exec.Command(adbPath, args...) — never through a shell — and
// pointed at the same adb server htadb uses (ADB_SERVER_SOCKET).
package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
)

// Runner runs adb.
type Runner struct {
	Path       string // resolved adb binary
	ServerAddr string // host:port of the adb server
}

// New resolves path (a name on PATH or a file path). It fails when adb cannot
// be found; callers then disable passthrough.
func New(path, serverAddr string) (*Runner, error) {
	p, err := exec.LookPath(path)
	if err != nil {
		return nil, err
	}
	return &Runner{Path: p, ServerAddr: serverAddr}, nil
}

// serverArgs points adb at the configured server. adb only auto-starts a
// server it considers local, so loopback addresses use -P alone.
func (r *Runner) serverArgs() []string {
	host, port, err := net.SplitHostPort(r.ServerAddr)
	if err != nil {
		return nil
	}
	ip := net.ParseIP(host)
	local := host == "localhost" || (ip != nil && ip.IsLoopback())
	switch {
	case local && port == "5037":
		return nil
	case local:
		return []string{"-P", port}
	}
	return []string{"-H", host, "-P", port}
}

// Command builds `adb [-H host] [-P port] [-s serial] args...`.
func (r *Runner) Command(ctx context.Context, serial string, args []string) *exec.Cmd {
	a := r.serverArgs()
	if serial != "" {
		a = append(a, "-s", serial)
	}
	a = append(a, args...)
	cmd := exec.CommandContext(ctx, r.Path, a...)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "ADB_SERVER_SOCKET=") && !strings.HasPrefix(e, "ANDROID_ADB_SERVER_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Cancel = func() error { return Interrupt(cmd) }
	setProcAttr(cmd)
	return cmd
}

// Result is a buffered run.
type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// Run runs adb to completion.
func (r *Runner) Run(ctx context.Context, serial string, args []string, stdin io.Reader) (*Result, error) {
	cmd := r.Command(ctx, serial, args)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &out, &errb, stdin
	err := cmd.Run()
	res := &Result{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		return nil, err
	}
	return res, nil
}

// StartServer runs `adb start-server` (used when the adb server is down).
func (r *Runner) StartServer(ctx context.Context) error {
	_, err := r.Run(ctx, "", []string{"start-server"}, nil)
	return err
}
