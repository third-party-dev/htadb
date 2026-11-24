# Running htadb

```sh
htadb                                   # http://127.0.0.1:10443/ui/
htadb --listen https://0.0.0.0:10443    # LAN, with htadb's own TLS
htadb --listen unix:///run/htadb.sock --unix-mode 0660
```

htadb connects to the adb server at `--adb-server` (default
`127.0.0.1:5037`) and starts it with `adb start-server` when it is not
running (`--adb-path`, default `adb` on `PATH`).

## Listeners

`--listen` can be repeated. Each value is a URL-style spec:

| Spec | Use |
| --- | --- |
| `http://127.0.0.1:10443` | Default. Loopback is a secure context, so every browser feature works. |
| `https://0.0.0.0:10443` | Direct LAN/remote access with htadb's TLS (see [TLS.md](TLS.md)). |
| `http://0.0.0.0:443` | Behind a TLS-terminating reverse proxy. |
| `unix:///run/htadb.sock` | For socat or proxies; permissions from `--unix-mode` (default `0660`). A stale socket file is replaced; one in use is an error. Works on Linux, macOS and Windows 10+. |
| `systemd:` | Linux socket activation (`LISTEN_FDS`); `systemd:name` picks sockets by `FileDescriptorName`. |

Port `0` picks a free port; `--announce` prints `HTADB_LISTENING <url>` on
stdout for each listener (the VS Code extension uses this).

A plain `http://` listener on a non-loopback address logs a warning:
browsers reaching it directly are not in a secure context, so they disable
WebCodecs (mirroring) and the async Clipboard API. Every page shows a banner
in that case. Use loopback, an `https://` listener, or a TLS proxy.

There is no authentication. Restrict access with the listener address,
socket permissions, a firewall or an authenticating reverse proxy.

## Configuration

Each flag can also come from an `HTADB_*` environment variable (upper case,
dashes → underscores, lists comma-separated) or from `config.toml` in the
config directory (`--config-dir`, default `<user config dir>/htadb`, i.e.
`~/.config/htadb` on Linux, `~/Library/Application Support/htadb` on macOS,
`%AppData%\htadb` on Windows; `--config-file` names the file directly).
Precedence: flag > environment > config.toml > default. Unknown keys in
config.toml are errors. Example: [examples/config.toml](examples/config.toml).

| Flag | Default | |
| --- | --- | --- |
| `--listen` | `http://127.0.0.1:10443` | repeatable |
| `--unix-mode` | `0660` | |
| `--adb-server` | `127.0.0.1:5037` | |
| `--adb-path` | `adb` | starting the adb server; CLI passthrough |
| `--cli` | `true` | `/api/v1/cli`; off automatically when adb is not found |
| `--cors-origin` | `*` | comma-separated origins; empty disables CORS |
| `--ui` / `--ui-dir` | `true` / embedded | serve the UI / serve it from a directory |
| `--ui-override` | | `/vendor/xterm=/path`, repeatable |
| `--ui-config` | | JSON file served to pages as `config.js` |
| `--tls-cert`, `--tls-key`, `--tls-san` | | see [TLS.md](TLS.md) |
| `--mirror-server` | embedded jars | `[version=]path`, repeatable |
| `--mirror-default-version` | newest | |
| `--mirror-allow-untested` | `false` | |
| `--mirror-fallback` | `true` | |
| `--log-level`, `--log-format` | `info`, `text` | `json` for structured logs |

Subcommands: `htadb version`, `htadb licenses`, `htadb tls print-ca`,
`htadb tls regenerate`.

## systemd (Linux)

[examples/htadb.service](examples/htadb.service) is a user unit;
[examples/htadb.socket](examples/htadb.socket) adds socket activation
(start the service with `--listen systemd:`).

## Examples

Served by htadb:

```sh
curl -s localhost:10443/api/v1/devices
curl -s -X POST localhost:10443/api/v1/devices/R58M12345/shell -d '{"command":"getprop ro.product.model"}'
curl -s -o shot.png localhost:10443/api/v1/devices/R58M12345/screenshot
curl -s --data-binary @app.apk "localhost:10443/api/v1/devices/R58M12345/install?args=-r"
curl -s -T notes.txt "localhost:10443/api/v1/devices/R58M12345/files/content?path=/sdcard/Download/notes.txt"
```

Served by Python (UI development), pointing at htadb:

```sh
python3 -m http.server 8000 -d web
open 'http://127.0.0.1:8000/mirror.html?api=http://127.0.0.1:10443&serial=R58M12345'
```

## Terminal page

The prompt is `adb [serial]>`. Built-ins: `help`, `clear`, `devices [-l]`,
`use <serial>` (or `-s <serial>` per command), `shell [cmd]` (interactive
PTY with live resize; `exit` returns to the prompt), `ls`, `push <remote>`
(file picker), `pull <remote>` (browser download), `install [args]`,
`uninstall [-k] <pkg>`, `screencap`, `mirror`. Everything else runs as
`adb [-s serial] …` on the host through the streamed CLI passthrough; Ctrl+C
sends SIGINT (Windows: the process is killed). History, Tab completion of
commands, serials and remote paths, and the usual line-editing keys work.

## Mirroring page

Choose a device and server version (Auto = newest with fallback to 4.x),
size, bitrate and frame rate, then Connect. Mouse and touch become touches
(right click = back / screen on, middle click = home, wheel = scroll);
printable keys are injected as text, other keys as Android keycodes. The
device clipboard is copied to this computer when the browser allows it (and
is always shown in the Device clipboard panel); Ctrl/Cmd+V pastes this
computer's clipboard on the device. Hotkeys use a modifier (Alt by default;
`Alt+/` shows and rebinds them; bindings can be exported/imported as JSON).
Audio (Opus, Android 11+) is opt-in.
