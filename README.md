# htadb

htadb exposes adb over REST and WebSocket. It is a single Go binary that
serves a browser terminal (`adb [serial]>` prompt, interactive device shells,
file transfer, installs) and device mirroring (WebCodecs, speaking the scrcpy
server protocol). A VS Code extension opens both in the integrated browser.

htadb is a peer client of the adb server on `:5037`, exactly like the `adb`
CLI: command-line adb keeps working, and htadb never touches USB. It has no
authentication — put a reverse proxy, firewall or socket permissions in front
of it when it listens beyond loopback.

```sh
make fetchdeps    # pinned xterm.js + mirroring server jars (sha256-verified)
make htadb        # → bin/htadb
bin/htadb         # http://127.0.0.1:10443/ui/
```

No Go toolchain? `docker build -t htadb-dev .devcontainer && .devcontainer/run make htadb`.

## Pages

| Page | URL |
| --- | --- |
| Devices | `/ui/` |
| Terminal | `/ui/terminal.html` (`?serial=…`, `?mode=shell&serial=…` opens a device shell) |
| Mirroring | `/ui/mirror.html` (`?serial=…&autoconnect=1`) |

## Documentation

- [docs/BUILD.md](docs/BUILD.md) — toolchain, make targets, dev container, tests
- [docs/RUNNING.md](docs/RUNNING.md) — listeners, configuration, systemd, examples
- [docs/TLS.md](docs/TLS.md) — https listeners and trusting the local CA
- [docs/API.md](docs/API.md) — `/api/v1` reference
- [docs/MIRRORING.md](docs/MIRRORING.md) — protocol profiles, versions, jars, fallback
- [docs/VSCODE.md](docs/VSCODE.md) — the extension
- [docs/DESIGN.md](docs/DESIGN.md) — architecture and decisions
- [PLAN.md](PLAN.md) — the implementation plan

## Third-party components

- [github.com/coder/websocket](https://github.com/coder/websocket) (ISC), the only Go dependency
- [xterm.js](https://xtermjs.org) 6.0.0 (MIT), served to browsers
- scrcpy server 4.1 and 5.0.1 (Apache-2.0), embedded and pushed to devices; see
  `htadb licenses` and `internal/mirror/servers/embedded/`
