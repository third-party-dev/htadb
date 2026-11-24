package adb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// InstallResult is the package manager's verdict.
type InstallResult struct {
	Output  string `json:"output"`
	Success bool   `json:"success"`
}

func installVerdict(out string) (*InstallResult, error) {
	out = strings.TrimSpace(out)
	r := &InstallResult{Output: out, Success: strings.Contains(out, "Success")}
	if !r.Success {
		return r, &Error{Service: "install", Msg: out}
	}
	return r, nil
}

// Install streams an APK of the given size to the package manager. args are
// extra pm flags (e.g. "-r", "-g"); each is shell-quoted.
func (c *Client) Install(ctx context.Context, serial string, apk io.Reader, size int64, args []string) (*InstallResult, error) {
	f, err := c.CachedFeatures(ctx, serial)
	if err != nil {
		return nil, err
	}
	var qa []string
	for _, a := range args {
		qa = append(qa, ShellQuote(a))
	}
	if f["cmd"] || f["abb_exec"] {
		cmd := fmt.Sprintf("cmd package install -S %d", size)
		if len(qa) > 0 {
			cmd += " " + strings.Join(qa, " ")
		}
		conn, err := c.Exec(ctx, serial, cmd)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		if _, err := io.CopyN(conn, apk, size); err != nil {
			return nil, fmt.Errorf("install: streaming apk: %w", err)
		}
		out, err := io.ReadAll(conn)
		if err != nil {
			return nil, err
		}
		return installVerdict(string(out))
	}
	// Old devices: push to a temp path and run pm install.
	var rb [6]byte
	rand.Read(rb[:])
	tmp := "/data/local/tmp/htadb-install-" + hex.EncodeToString(rb[:]) + ".apk"
	if _, err := c.Push(ctx, serial, tmp, 0o644, io.LimitReader(apk, size)); err != nil {
		return nil, err
	}
	defer c.RunShell(context.WithoutCancel(ctx), serial, "rm -f "+tmp)
	cmd := "pm install"
	if len(qa) > 0 {
		cmd += " " + strings.Join(qa, " ")
	}
	r, err := c.RunShell(ctx, serial, cmd+" "+tmp)
	if err != nil {
		return nil, err
	}
	return installVerdict(string(r.Stdout) + string(r.Stderr))
}

// Uninstall removes a package; keepData maps to -k.
func (c *Client) Uninstall(ctx context.Context, serial, pkg string, keepData bool) (*InstallResult, error) {
	cmd := "pm uninstall "
	if keepData {
		cmd += "-k "
	}
	r, err := c.RunShell(ctx, serial, cmd+ShellQuote(pkg))
	if err != nil {
		return nil, err
	}
	return installVerdict(string(r.Stdout) + string(r.Stderr))
}
