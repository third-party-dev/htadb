# htadb design

```
 browser / VS Code ──HTTP+WS──▶ htadb ──smart sockets──▶ adb server :5037 ──▶ devices
                                  │                          ▲
                                  └── adb CLI (passthrough) ─┘   adb CLI users (unaffected)
```

htadb is a peer client of the adb server, like the `adb` CLI. It never talks
to USB; every adb stream is its own TCP connection to the server.

## Packages

| Package | Role |
| --- | --- |
| `cmd/htadb` | Flags/config, listeners, wiring, `tls` and `licenses` subcommands |
| `internal/adb` | Smart-socket client: framing, host services, transport, shell v2/legacy, exec, sync, forwards/reverses, install |
| `internal/adb/adbtest` | Fake adb server and devices; `fakeadb` command |
| `internal/api` | `/api/v1` handlers, middleware (request ids, CORS, logging, recovery), device tracker |
| `internal/mirror` | Sessions: planning (version selection, jar resolution, options), launch, handshake, multiplexing, fallback |
| `internal/mirror/profile` | Per-version protocol specs, control/device message codec, golden tests |
| `internal/mirror/servers` | Jar registry (embedded/supplied), device naming, push, inventory |
| `internal/mirror/mirrortest` | Fake mirroring server (installed as the fake device's `app_process`), Annex B splitter |
| `internal/cli` | adb binary passthrough (no shell), signals per OS |
| `internal/listen` | Listener specs, unix/systemd listeners, local CA + leaf |
| `internal/config` | Flags + `HTADB_*` + config.toml (small TOML subset parser) |
| `internal/ui` | Embedded UI, `--ui-dir`, `--ui-override`, generated `config.js` |
| `internal/ws` | The WebSocket interface; the only importer of `github.com/coder/websocket` |
| `web/` | ES-module sources (no framework; xterm.js is the only library) |
| `tools/` | `fetchdeps`, `webbuild` (separate Go module, so esbuild is not a dependency of htadb) |
| `vscode-extension/` | Plain-JS extension |

## Decisions

- **One dependency.** The binary depends only on `coder/websocket`, behind
  `internal/ws`. TOML, TLS generation, the adb protocol and systemd
  activation are implemented with the standard library.
- **No auth.** htadb trusts whoever can reach it; access control belongs to
  the listener address, socket permissions or a proxy. Plain-http listeners
  on non-loopback addresses log a warning.
- **TLS only for https.** Certificates are created or read only when an
  `https://` listener is configured.
- **Errors before upgrades.** WebSocket endpoints validate and open their
  adb streams before accepting the upgrade, so failures are ordinary JSON
  errors with HTTP statuses (404 device, 409 version, 502 adb FAIL).
- **Contexts and lifetimes.** Each adb stream is bound to its request or
  WebSocket; WebSockets are pinged every 20 s; closing a socket closes its
  adb streams (and for mirroring, the launch shell, so the server exits).
- **adb CLI targeting.** Passthrough runs `adb -P <port>` (plus `-H` for
  remote servers) rather than `ADB_SERVER_SOCKET`, because adb only
  auto-starts a server it considers local.
- **Mirroring naming.** Code and UI say "mirroring"; scrcpy is named only
  where its protocol, jars and licence are concerned.
- **Mirroring safety.** Control messages from browsers are validated against
  the session's protocol profile before reaching the server; launch options
  are allowlisted per version and shell-quoted.
- **Jar lifetime.** The server unlinks its classpath (cleanup); sessions run
  from per-session hard links so the shared, verified jar survives. See
  [MIRRORING.md](MIRRORING.md).
- **UI hosting.** htadb embeds and serves the UI, so neither the extension
  nor users need a web server; `--ui-dir` and `?api=` support development.

## Verification log

Facts the plan asked to check during the build:

| Item | Finding |
| --- | --- |
| scrcpy protocol diff 4.0 / 4.1 / 5.0 | 4.0→4.1: SCAN_FILE (22), vp8/vp9, `ignore_video_encoder_constraints`, client-resized bit. 4.1→5.0→5.0.1: no wire changes. `minSdk` 21 everywhere. |
| VS Code's xterm.js | `@xterm/xterm` ^6.1.0-beta (1.147.0), packed in `node_modules.asar`; copied by the extension. |
| First VS Code with `simpleBrowser.useIntegratedBrowser` | 1.109.0 (removed in 1.113.0, where the integrated browser is used automatically). |
| Integrated browser and htadb's local CA | Not verifiable headless. The managed adapter uses loopback http by default, which needs no certificate. |
| Headless Chrome and H.264 | Alpine's Chromium 158 decodes H.264 via WebCodecs; the e2e test checks decoded frames. |
