# htadb — Implementation Plan

Nov 24, 2026 · @Vinnie

## Overview and architecture

htadb is a single Go binary that exposes adb over REST and WebSocket. It serves a browser terminal and device mirroring, and a VS Code extension opens both in the integrated browser.

&#91;embedded content: architecture · htadb sits beside the adb CLI as a client of the adb server\]

Browsers reach adb only through htadb; the adb CLI keeps working against the same adb server.

- **Peer client, not a replacement.** htadb talks to the existing adb server on :5037 exactly as the `adb` CLI does. Terminal adb users are unaffected, and htadb never touches USB.
- **No auth.** htadb has no authentication or authorization; that belongs to whatever sits in front of it (a reverse proxy, socat, a firewall).
- **Dependencies.** One static binary. Its only runtime dependency is `github.com/coder/websocket`, kept behind a small internal interface so it can be swapped.
- **Naming.** In code and UI the feature is always "mirroring". scrcpy is named only in comments and docs that describe its protocol and conventions.
- **Platforms.** Linux first (tests and CI). macOS and Windows get cross-compiled builds and a smoke test.
- **UI hosting.** htadb embeds and serves the web UI itself, so neither the extension nor the user needs a separate web server. `python -m http.server` remains an option for UI development.

## Repository layout

One repository holds the Go module, the web sources, the build tools and the extension.

```
htadb/
  cmd/htadb/                main: flags, config, listeners
  internal/adb/             smart-socket client (host:*, transport, shell v2, sync, exec)
  internal/api/             REST + WS handlers
  internal/mirror/          mirroring sessions (speaks the scrcpy protocol)
  internal/mirror/profile/  per-version protocol profiles (4.x, 5.0)
  internal/mirror/servers/  server-jar registry, embedding, device push/inventory, fallback
  internal/cli/             optional adb-binary passthrough
  internal/listen/          listener specs, TLS, CA/leaf generation
  internal/ui/              go:embed of built web assets
  web/                      readable ES-module sources
    index.html, terminal.html|js, mirror.html|js
    lib/{api,mirror-control,mirror-protocol,hotkeys,fit,secure-context}.js
  tools/webbuild/           Go program: esbuild API bundle+minify, or copy-through
  tools/fetchdeps/          Go program: pinned xterm.js + scrcpy-server jars (sha256-pinned)
  vscode-extension/         plain JS, zero npm dependencies
  docs/                     DESIGN, API, MIRRORING, TLS, VSCODE, BUILD
  Makefile
```

## htadb adapter

htadb listens on any mix of TCP and unix sockets, speaks the adb smart-socket protocol to the adb server, and exposes it as `/api/v1`.

### Listeners

`--listen` can be repeated; each value is a URL-style spec.

| Spec | Use |
| --- | --- |
| `http://127.0.0.1:10443` | Default. Loopback is a secure context, so every browser feature works |
| `https://0.0.0.0:10443` | Direct LAN or remote access with htadb's own TLS |
| `http://0.0.0.0:443` | Behind a TLS-terminating reverse proxy |
| `unix:///run/htadb.sock` | For socat or proxies; `--unix-mode 0660` |
| `systemd:` | Linux socket activation (optional) |

- **Non-loopback warning.** A plain `http://` listener on a non-loopback address logs a warning. Browsers reaching it directly lose WebCodecs and the async Clipboard API, so mirroring and clipboard sync need a TLS proxy in front.
- **Secure-context banner.** Every `/ui/` page checks `window.isSecureContext` and shows a banner instead of failing silently.

### TLS (only for https listeners)

- **When.** Certificates are created or loaded only if at least one `https://` listener is configured. With plain http or unix listeners, htadb never touches TLS files.
- **Explicit certs.** `--tls-cert` and `--tls-key` take precedence.
- **Generated certs.** Otherwise htadb generates them in `os.UserConfigDir()/htadb/tls/`: `~/.config/htadb/tls/` on Linux (honours `XDG_CONFIG_HOME`), `~/Library/Application Support/htadb/tls/` on macOS, `%AppData%\htadb\tls\` on Windows. `--config-dir` overrides it.
- **Local CA plus leaf.** `ca.pem` and `ca-key.pem` are created once and kept. The leaf (`cert.pem`, `key.pem`) is signed by the CA, with SANs for `localhost`, `127.0.0.1`, `::1`, the hostname, the listener IPs and any `--tls-san`.
- **Renewal.** The leaf is regenerated when its SANs change or it is within 30 days of expiry; the CA is reused, so trust survives. Keys are ECDSA P-256, files `0600`, directory `0700`.
- **Commands.** `htadb tls print-ca` prints the CA path and fingerprint; `htadb tls regenerate` forces a new leaf. `docs/TLS.md` covers trusting the CA on Linux, macOS, Windows and Chrome.

### Configuration

- `--adb-server 127.0.0.1:5037` and `--adb-path` (used to auto-start the adb server and for CLI passthrough).
- `--cli=true|false`, `--cors-origin` (permissive by default), `--ui=true|false`, `--ui-dir`, `--ui-override`.
- Mirroring: `--mirror-server`, `--mirror-default-version`, `--mirror-allow-untested`, `--mirror-fallback`.
- Every flag can also come from an `HTADB_*` env var or `config.toml` in the config dir, which also holds per-device settings.

### adb smart-socket client (internal/adb)

- **Framing.** Each request is a 4-hex-digit length plus the service string; the reply is `OKAY`, or `FAIL` plus a message.
- **Host services.** `host:version`, `host:devices-l`, `host:track-devices-l`, `host-serial:<s>:features`, `host:connect:`, `host:disconnect:`, `forward`, `list-forward`, `killforward`.
- **Device services** via `host:transport:<serial>`:
  - Shell: `shell,v2,pty,TERM=…:<cmd>` and `shell,v2,raw:<cmd>`, framed as id byte + little-endian u32 length + data. Devices without `shell_v2` fall back to legacy `shell:` (no exit code, no resize).
  - `exec:` for binary-clean output, and `sync:` with STAT, LIST, RECV, SEND and DONE.
  - `reverse:forward…`, `reboot:<target>`, `root:`, `unroot:`, `remount:`.
  - `localabstract:<name>` raw streams, used by mirroring.
- **Install.** The APK is streamed into `exec:cmd package install -S <size> [args]`; uploads without Content-Length go to a temp file first.

### API (/api/v1)

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/version` | htadb version + adb server version |
| GET | `/devices` | List (`devices -l` parsed) |
| WS | `/devices/track` | JSON device add/remove/state events |
| GET | `/devices/{s}` | Features, key props |
| GET | `/devices/{s}/props` | Full getprop map |
| POST | `/devices/{s}/shell` | `{command, timeout}` → `{stdout, stderr, exitCode}` |
| WS | `/devices/{s}/shell` | Interactive; `?cmd=&pty=1&term=&rows=&cols=` |
| POST | `/devices/{s}/exec` | Raw binary stdout |
| GET | `/devices/{s}/screenshot` | `image/png` |
| GET | `/devices/{s}/files?path=` | Stat or directory listing |
| GET | `/devices/{s}/files/content?path=` | Pull (streamed download) |
| PUT | `/devices/{s}/files/content?path=&mode=` | Push (streamed body) |
| POST | `/devices/{s}/install?args=` | APK body |
| POST | `/devices/{s}/uninstall` | `{package, keepData}` |
| POST | `/devices/{s}/reboot` | `{target}` |
| POST | `/devices/{s}/root`, `/unroot`, `/remount` | Admin |
| GET/POST/DELETE | `/devices/{s}/forwards`, `/reverses` | Port forwarding |
| POST | `/connect`, `/disconnect` | `{address}` |
| POST / WS | `/cli` | Passthrough: `adb [-s s] <args…>`, buffered or streamed |
| GET | `/mirror/servers` | Server builds htadb holds (version, sha256, profile, latest) |
| GET | `/devices/{s}/mirror/servers` | Jars on the device, versions inferred from file names |
| WS | `/devices/{s}/mirror` | Mirroring session |
| DELETE | `/devices/{s}/mirror/fallback` | Clear the remembered 4.x fallback |
| GET | `/healthz`, `/metrics` | Health, expvar metrics |

- **Errors.** `{error, code}` JSON: 404 for a missing device, 502 for an adb `FAIL`, 409 for an incompatible mirroring version.
- **Shell WebSocket.** Binary frames carry raw stdin and stdout. Client text frames: `resize {rows, cols}`, `eof`, `signal {name}` (CLI only). Server text frames: `exit {code}` and, in non-PTY mode, `stderr`.
- **CLI passthrough.** Runs `exec.Command(adbPath, args…)` with no shell, so there is no injection surface. Disabled automatically if adb is not found.

### Lifecycle and cross-platform

- Each adb stream gets its own connection to :5037; contexts are bound to the request or WebSocket, with per-stream deadlines, WebSocket pings and `slog` logging with request IDs.
- Unix-socket listeners work on Linux, macOS and Windows 10 or later (AF\_UNIX).
- Ctrl+C in CLI passthrough sends SIGINT on Linux and macOS, and kills the process on Windows.
- Linux extras: a systemd user-unit example and socket activation.

## Mirroring (speaks the scrcpy protocol)

Mirroring launches the scrcpy server with `tunnel_forward=true` and bridges its sockets to one WebSocket. It needs no `adb reverse`, supports 4.x and 5.0, and falls back to 4.x automatically.

### Versions and protocol profiles

- **Latest release.** scrcpy 5.0, released 5 October 2026 ([release coverage](https://ameridroid.com/blogs/ameriblogs/scrcpy-5-0-mirrors-android-phones-devices-with-hardware-decoding)). Its headline change, GPU decoding, is host-side.
- **Profiles.** 4.x (4.0, 4.1 and later 4.x patches) and 5.0; 5.0 is the default. A profile defines the accepted launch arguments, the handshake (dummy byte, device-meta length, codec-meta layout), the packet header, the control and device message tables, and `minSdk`.
- **Verification first.** Diff `doc/develop.md` and the server's message code across the 4.0, 4.1 and 5.0 tags. The diff becomes the profile table and golden-byte tests. If 4.x and 5.0 match on the wire, they share one implementation and remain separate allowlist entries.
- **Exact versions.** Profiles allowlist exact versions. A version outside the list that maps to a known major gets 409, unless `--mirror-allow-untested` is set (then the major's profile is used, with a warning).
- **Browser side.** The session `meta` names the profile, so the page loads the matching encoder from `lib/mirror-control.js`.
- **Why the version matters.** The scrcpy server refuses to start if the version argument differs from its own. The version selects the jar and the argument; the profile decides whether htadb can speak to it.

### Server builds on the host

- **Embedded.** Release builds embed the newest jar of each profile (latest 4.x and 5.0). scrcpy is Apache-2.0, so its licence and NOTICE ship with htadb. `-tags nomirrorjar` leaves the jars out.
- **Supplied.** `--mirror-server <path>` can be repeated. The version is inferred from the file name (`scrcpy-server-v5.0.jar` or the upstream asset name `scrcpy-server-v5.0`); `--mirror-server 5.0=<path>` sets it explicitly.
- **Fetching.** `tools/fetchdeps` downloads jars from GitHub releases and pins their sha256.

### Device naming convention

- **Path.** `/data/local/tmp/htadb/scrcpy-server-v<version>.jar`, for example `/data/local/tmp/htadb/scrcpy-server-v5.0.jar`.
- **Pattern.** `^scrcpy-server-v(\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.]+)?)\.jar$`: the version sits at the end of the name, before the extension.
- **Separate directory.** The scrcpy CLI pushes to `/data/local/tmp/scrcpy-server.jar`; ours never overwrites it, and several versions coexist.
- **Inventory.** `GET /devices/{s}/mirror/servers` lists the directory (sync LIST), parses versions from names, and marks `verified` when size and hash match a known build.

### Push, tolerant of earlier sessions

1. A per-device, per-version mutex serializes the check and the push.
2. sync STAT the target path; if missing, go to step 4.
3. If the size matches, run `sha256sum <path>` on the device (toybox) and compare. On a match, reuse the jar.
4. Otherwise push to `…/.scrcpy-server-v<version>.jar.<random>.tmp`, then `mv -f` into place, so no session ever launches a half-written jar.
5. If htadb has no build for the requested version but the device holds a jar for it, use that jar, trust the name's version, and mark the session `verified:false` with a warning.

### Version selection

Highest priority first:

1. The session query `?version=`.
2. A per-device entry in `config.toml`, e.g. `[mirror.devices."R58M12345"]` with `version = "4.1"`.
3. `--mirror-default-version`.
4. The newest version available.

The mirroring page has a version picker filled from the host builds plus the device's jars. `?server_path=` uses a pre-installed jar (version from its name, `?version=` overrides). Attach mode, `?launch=false&scid=…&version=…`, only connects to a server that is already running.

### Automatic fallback to 4.x

- **Scope.** Only when htadb chose the version itself (default or newest). An explicit `?version=`, `server_path=` or per-device entry never falls back; a failure there returns an error naming the version.
- **Before launch.** htadb reads `ro.build.version.sdk` and compares it with each profile's `minSdk`. Below 5.0's minimum, it goes straight to the newest 4.x.
- **At launch.** If 5.0 fails before the first video packet (server exits, socket connect retries run out, or the handshake fails), htadb tears down and retries once with the newest 4.x. A failure after streaming starts never falls back.
- **Reporting.** `meta` carries `fallbackFrom: "5.0"` and `fallbackReason` (`sdk`, or `launch` plus the server's last log lines). The toolbar shows e.g. "Running 4.1 (5.0 failed: …)".
- **Memory.** A launch fallback is remembered per device in memory, cleared on device reconnect, htadb restart, or `DELETE /devices/{s}/mirror/fallback`.
- **Control.** `--mirror-fallback=true|false`, on by default; `htadb.mirror.fallback` in the extension.

### Session flow

1. Generate a 31-bit `scid` and ensure the jar is on the device.
2. Launch over shell v2: `CLASSPATH=<path> app_process / com.genymobile.scrcpy.Server <version> scid=… tunnel_forward=true …`, options from the profile's allowlist. Server stdout and stderr go to the page as `log` messages.
3. Connect `localabstract:scrcpy_<scid>` in the order video, audio, control, retrying while the server starts, then read the handshake as the profile defines it.
4. Multiplex over one WebSocket:
   - Server → browser, binary: `0x01` + 8-byte pts/flags + payload = one video packet; `0x02` + same layout = one audio packet; `0x03` + bytes = device-message bytes.
   - Server → browser, text: `meta` (device name, codecs, size, version, profile, `verified`, fallback fields), `log`, `error`.
   - Browser → server: binary control messages, written straight to the control socket.
5. Closing the WebSocket closes every socket and the launch shell; the server exits once its sockets close.

## Browser clients

Two pages, a terminal and a mirroring viewer, written as readable ES modules with no framework and xterm.js as the only third-party library.

### Source and build

- **Build modes.** `tools/webbuild` (Make variable `WEB=dev|min`) uses esbuild's Go API, so no Node is needed. `dev` copies files through unchanged; `min` bundles each page into one file, minifies it and writes sourcemaps. Release builds embed `min`.
- **xterm.js.** Fetched by `tools/fetchdeps` with its hash pinned. Our own `lib/fit.js` replaces addon-fit, so it also works with VS Code's copy of xterm.

### Terminal (terminal.html)

- **Prompt.** xterm.js with an `adb [serial]>` prompt and a line editor (history, Ctrl+C, tab completion).
- **Device commands.** `help`, `clear`, `devices [-l]`, `use <serial>` (or `-s` per command).
- **Shell.** `shell [cmd]` opens an interactive PTY over WebSocket with live resize; `exit` returns to the prompt.
- **Files and install.** `push <remote>` (file picker, progress), `pull <remote>` (browser download), `install [args]` (file picker), `screencap` (PNG download).
- **Everything else.** logcat, reboot, forward, reverse, connect, bugreport and the rest go through streamed CLI passthrough, with Ctrl+C sent as a signal.
- **Direct shell.** `?mode=shell&serial=…` opens straight into a device shell. Theme and font come from `config.js`.

### Mirroring (mirror.html)

- **Video.** WebCodecs `VideoDecoder` decodes Annex B H.264, with the codec string taken from the SPS. Each config packet is merged into the next keyframe, and the decoder is reconfigured on new config (rotation, resize). Frames are drawn to a canvas; no WASM.
- **Audio (opt-in).** An Opus `AudioDecoder` feeds a scheduled `AudioContext` queue, started by a user gesture.
- **Pointer input.** Mouse uses the mouse pointer id; touch uses a per-finger id. Coordinates are scaled to the frame size. Wheel becomes scroll; right-click is back or screen-on, middle-click is home.
- **Keyboard input.** Printable characters are injected as text; special and modified keys become keycodes with metastate.
- **Clipboard.** Device clipboard goes to `navigator.clipboard`, with a textarea fallback. The browser `paste` event becomes set-clipboard with paste.
- **Hotkeys.** Action-to-combo registry; modifier configurable (default Alt). Defaults approximate scrcpy's shortcuts. An overlay panel lets you rebind, reset, export and import; bindings persist in localStorage (inside try/catch) or come from `config.js`. Keyboard Lock captures OS-reserved combos in fullscreen on Chromium.
- **Toolbar.** Connect and disconnect, device and version pickers, max-size, bitrate, fps, audio, control, stay-awake, turn-screen-off, navigation buttons, and notices for unverified jars and fallbacks.

### Running

- **Normal.** Run `htadb` and open `http://127.0.0.1:10443/ui/`.
- **Development.** `python3 -m http.server 8000 -d web`, then open `http://127.0.0.1:8000/terminal.html?api=http://127.0.0.1:10443` (CORS is on by default). Or `htadb --ui-dir ./web` to serve live sources.

## VS Code extension

The extension manages an htadb process, lists devices, and opens the terminal and mirroring pages in the integrated browser. It adds no runtime of its own.

### No additional runtime

- **No separate Node.** Extensions run in VS Code's own extension host, which already is Node.
- **No npm packages.** Only the `vscode` API and Node built-ins (`child_process`, `net`), so the vsix has no `node_modules`.
- **No web server.** htadb serves the pages; the extension only manages the process, shows the device view and opens URLs.

### Reusing VS Code's resources

- **xterm.js.** The extension looks for VS Code's copy under `vscode.env.appRoot` and checks its version. If it qualifies, htadb is launched with `--ui-override /vendor/xterm=<dir>`; otherwise htadb uses its embedded copy. That layout is not a public API (it may sit inside an `.asar` archive and changes between releases), so the fallback matters.
- **Theme.** The terminal font and theme kind are passed through `config.js`.

### Integrated browser

- **How pages open.** Through the built-in `simpleBrowser.show` command. The `simpleBrowser.useIntegratedBrowser` setting makes that command use the new integrated browser ([setting notes](https://israynotarray.com/en/misc/2026/05/18/vscode-disable-integrated-browser-auto-open/)), so no third-party extension is needed.
- **The check.** At activation and before every open, the extension calls `getConfiguration('simpleBrowser').inspect('useIntegratedBrowser')`. If the effective value is not `true`, it warns that the classic Simple Browser is an iframe in a webview, where keyboard capture and some APIs are less reliable. Buttons: **Enable…**, **Open anyway**, **Don't ask again**.
- **Enable…** shows a modal confirmation, then calls `update('useIntegratedBrowser', true, target)`, where `target` is the most specific scope currently overriding the value (folder, workspace or user).
- **Older VS Code.** If the setting does not exist, the extension says so and falls back to the classic Simple Browser.
- **Listener.** The managed htadb listens on `http://127.0.0.1:<random port>`, a secure context with no certificate prompts. `htadb.managed.useTls` switches it to https with the generated CA.

### UI and settings

- **Device view.** The activity-bar view **ADB Devices** updates live from `/devices/track`. Each row has inline **Terminal** and **Mirror** buttons, each opening a new editor tab.
- **Status bar and commands.** A status-bar item shows htadb's state. Commands: `htadb: Open Terminal`, `htadb: Mirror Device`, `htadb: Start Adapter`, `htadb: Stop Adapter`, `htadb: Show Log`.
- **Settings.**
  - `htadb.url`: an existing htadb to use; empty starts the bundled binary.
  - `htadb.binaryPath`, `htadb.args`, `htadb.managed.useTls`.
  - `htadb.browser`: `integrated` (default) or `external`.
  - `htadb.mirror.defaults`, `htadb.mirror.hotkeys`, `htadb.mirror.serverVersion`, `htadb.mirror.fallback`.
  - `htadb.mirror.devices`: per-device versions and options, passed to htadb as config.
  - `htadb.suppressIntegratedBrowserWarning`.

## Build and deliverables

Everything builds with Go alone; Node is needed only for `vsce` at packaging time.

| Target | Result |
| --- | --- |
| `make fetchdeps` | Pinned xterm.js + latest 4.x and 5.0 server jars, hash-verified |
| `make htadb [WEB=dev\|min]` | Host build (`min` by default) |
| `make htadb-all [TARGETS=…]` | `CGO_ENABLED=0` cross-builds; default `linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64 windows/amd64 windows/arm64` |
| `make vsix [TARGETS=…]` | One vsix with every target by default; restrict with `TARGETS` |
| `make vsix-per-platform` | One small vsix per platform via `vsce --target` |
| `make test` | Go tests + web smoke tests |
| `make dist` | Source archive, binaries, vsix(s), `SHA256SUMS` |

Docs to ship:

- **BUILD.** Toolchain and build options.
- **Running and TLS.** Each listener type, trusting the CA.
- **Examples.** Served by htadb or by python.
- **MIRRORING.** scrcpy protocol notes, profiles, version selection, the jar naming convention, push behaviour and fallback.
- **VSCODE.** `code --install-extension htadb-*.vsix`, settings, the integrated-browser check, troubleshooting.

## Testing

Go tests run against a fake adb server; one end-to-end path runs headless Chrome; real-device checks are left to you.

- **adb client.** Framing, device parsing, shell v2, sync and listener parsing.
- **TLS.** Generation only when an https listener exists; leaf regenerated on SAN change; CA reused.
- **Jars.** File-name version parsing; push logic for a missing jar, size mismatch, hash mismatch, reuse on match, concurrent sessions and the atomic rename.
- **Profiles.** Golden bytes for 4.x and 5.0.
- **Fallback.** Below 5.0's `minSdk` skips to 4.x; a pre-first-packet failure retries exactly once on 4.x; an explicit version never falls back; a failure after streaming starts does not fall back; the per-device memory is reused and cleared on reconnect.
- **End to end.** A fake device streams an ffmpeg-generated H.264 clip through mirroring; headless Chrome loads the embedded pages and checks for a clean console. If the sandbox's Chrome cannot decode H.264, this check moves to real-device testing.
- **Real-device checklist.** Several Android versions with 4.x and 5.0 servers, a jar left by a previous session, and the VS Code integrated browser on Linux.

## Milestones and verification tasks

Six milestones, each usable on its own; the PWA milestone was dropped.

1. **htadb core.** Listeners, TLS generation, the adb client, device tracking, the shell, and the embedded terminal page.
2. **Device operations.** Files, install, forwards and CLI passthrough, completing the terminal command set.
3. **Mirroring.** The protocol diff and profiles (4.x, 5.0) with `minSdk`; the jar registry, naming, push and inventory; fallback to 4.x; then video and control, clipboard, hotkeys and audio.
4. **Web build.** The `dev`/`min` pipeline and the secure-context banner.
5. **VS Code extension.** Process management, the device view, the integrated-browser check and setting update, xterm reuse.
6. **Release.** Cross-compile matrix, vsix target options, docs and `make dist`.

No design questions remain open. These facts still need checking during the build:

- [ ] scrcpy protocol diff across 4.0, 4.1 and 5.0, and each version's `minSdk`
- [ ] VS Code's internal xterm.js location and version
- [ ] First VS Code release with `simpleBrowser.useIntegratedBrowser` (sets `engines.vscode`)
- [ ] How the integrated browser treats a certificate from htadb's local CA
- [ ] Whether the sandbox's headless Chrome decodes H.264
