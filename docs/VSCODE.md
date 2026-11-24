# VS Code extension

The extension (`vscode-extension/`) starts and stops an htadb process, shows
ADB devices in the activity bar, and opens htadb's terminal and mirroring
pages in VS Code's integrated browser. It is plain JavaScript using only the
`vscode` API and Node built-ins: no `node_modules`, no separate Node runtime
(extensions run in VS Code's extension host) and no web server (htadb serves
the pages).

## Install

```sh
make vsix                         # dist/htadb-<version>.vsix (all platforms)
make vsix-per-platform            # dist/htadb-<target>-<version>.vsix
code --install-extension dist/htadb-<version>.vsix
```

## Use

- **ADB Devices** view (activity bar): a status row (`htadb running ·
  http://127.0.0.1:<port>`; click to start when stopped) and one row per
  device, updated live from `/api/v1/devices/track`. Each device row has
  inline **Terminal** and **Mirror** buttons; each opens a new editor tab.
  The view title has Refresh and Stop Adapter.
- **Status bar**: `htadb running   N devices` (click: Open Terminal).
- **Commands**: `htadb: Open Terminal`, `htadb: Mirror Device`,
  `htadb: Start Adapter`, `htadb: Stop Adapter`, `htadb: Restart Adapter`,
  `htadb: Show Log`.

The managed htadb listens on `http://127.0.0.1:<random port>`. That is a
secure context, so WebCodecs and the Clipboard API work with no certificate
prompts. `htadb.managed.useTls` switches to `https://127.0.0.1` with htadb's
generated CA instead (trust it first; see [TLS.md](TLS.md)).

## Settings

| Setting | Default | |
| --- | --- | --- |
| `htadb.url` | `""` | Use an existing htadb; empty starts the bundled binary |
| `htadb.binaryPath` | `""` | Run this htadb instead of the bundled one |
| `htadb.args` | `[]` | Extra arguments, e.g. `["--adb-server","127.0.0.1:5037"]` |
| `htadb.managed.useTls` | `false` | https listener with the generated CA |
| `htadb.browser` | `integrated` | or `external` (system browser) |
| `htadb.mirror.defaults` | `{}` | `maxSize`, `bitrate`, `fps`, `audio`, `control`, `stayAwake`, `turnScreenOff` |
| `htadb.mirror.hotkeys` | `{}` | `{modifier, bindings}` (the page's Export JSON format) |
| `htadb.mirror.serverVersion` | `auto` | Passed as `--mirror-default-version` (keeps fallback) |
| `htadb.mirror.fallback` | `true` | `--mirror-fallback` |
| `htadb.mirror.devices` | `{}` | Per-device versions and options → a generated config.toml |
| `htadb.suppressIntegratedBrowserWarning` | `false` | |

The extension passes page settings (theme kind, terminal font, mirroring
defaults, hotkeys) through `--ui-config` (served as `/ui/config.js`) and
per-device settings through `--config-file` (a generated config.toml in the
extension's storage; the managed htadb therefore does not read your own
`~/.config/htadb/config.toml`). Changing an `htadb.*` setting restarts the
managed adapter.

## Integrated browser

Pages open with the built-in `simpleBrowser.show` command. What it renders
depends on the VS Code version (verified against the microsoft/vscode tags):

| VS Code | Behaviour | What the extension does |
| --- | --- | --- |
| 1.109 – 1.112 | `simpleBrowser.useIntegratedBrowser` (experimental, application scope, default `false`) selects the integrated browser | Inspects the setting at activation and before every open. If it is not `true`, warns that the classic Simple Browser is an iframe in a webview with less reliable keyboard capture, offering **Enable…** (modal confirmation, then `update(…, true, target)` at the most specific scope overriding it), **Open anyway** and **Don't ask again**. |
| 1.113 and later | The setting is gone; `simpleBrowser.show` uses the integrated browser whenever `workbench.action.browser.open` exists | Checks for that command; warns only when it is missing (for example VS Code for the Web), offering the external browser. |

`engines.vscode` is `^1.109.0`, the first release with the integrated
browser. Where neither mechanism exists, pages fall back to the classic
Simple Browser after the warning.

## xterm.js reuse

The extension looks for VS Code's own xterm.js under `vscode.env.appRoot`
(`node_modules/@xterm/xterm`, `node_modules.asar.unpacked/…`, or inside
`node_modules.asar`, which the extension host can read). Current VS Code
(1.147) ships `@xterm/xterm` 6.1 inside `node_modules.asar`, which htadb (a
separate process) cannot read, so the extension copies `lib/xterm.js` and
`css/xterm.css` into its storage and starts htadb with
`--ui-override /vendor/xterm=<copy>`. It requires version 5+ with the UMD
build (global `Terminal`). This layout is not a public API; whenever the
check fails, htadb serves its embedded xterm.js 6.0.0 and the log says why.

## Troubleshooting

- **htadb: Show Log** shows the adapter's output, the command line, the
  chosen xterm.js and the listening URL.
- "no bundled htadb for …": the vsix was built without your platform; use a
  per-platform vsix, set `htadb.binaryPath`, or point `htadb.url` at a
  running htadb.
- No devices: htadb uses the adb server at `127.0.0.1:5037` and starts it
  with `adb` from `PATH`; pass `--adb-path`/`--adb-server` via `htadb.args`.
- Mirroring shows "Not a secure context": the page was opened over plain
  http from a non-loopback address; use the managed adapter or https.
- Keys such as Alt+Tab reach the OS: use fullscreen (Keyboard Lock) or
  rebind the hotkey modifier.
