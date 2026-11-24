# Building htadb

Everything builds with Go alone (Go 1.24+; the dev container ships 1.27).
Node is needed only to package the VS Code extension (`vsce`, fetched by
`npx`) and to run the JavaScript tests.

## Make targets

| Target | Result |
| --- | --- |
| `make fetchdeps` | xterm.js 6.0.0 → `web/vendor/xterm/`; scrcpy-server 5.0.1 and 4.1 → `internal/mirror/servers/embedded/`. Every download is sha256-pinned in `tools/fetchdeps`. |
| `make web [WEB=dev\|min]` | Web assets → `internal/ui/dist/` (embedded by `go:embed`) |
| `make htadb [WEB=dev\|min]` | Host binary `bin/htadb` (`min` by default) |
| `make htadb-all [TARGETS=…]` | `CGO_ENABLED=0` cross builds → `bin/<os>-<arch>/`. Default targets: `linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64 windows/amd64 windows/arm64` |
| `make vsix [TARGETS=…]` | One vsix carrying every target's binary → `dist/` (~37 MB for all seven) |
| `make vsix-per-platform` | One vsix per platform via `vsce --target` (~5.5 MB each) |
| `make test` | `go vet`, Go tests (also with `-tags nomirrorjar`), JS unit tests, extension tests, browser smoke test |
| `make dist` | Source archive, per-platform archives, vsix and `SHA256SUMS` → `dist/` |

Build options:

- `WEB=dev` embeds the readable sources unchanged; `WEB=min` (release) bundles
  each page with esbuild's Go API (`tools/webbuild`), minifies it and writes
  linked sourcemaps. `config.js` stays external: htadb generates it.
- `GOTAGS=nomirrorjar` leaves the server jars out of the binary. Mirroring then
  uses `--mirror-server` files or jars already on devices.
- `VERSION=…` overrides the version stamped into the binary (default:
  `git describe`).

## Dev container

The repository includes an Alpine-based image with Go, adb, Chromium, fonts,
ffmpeg and Node:

```sh
docker build -t htadb-dev .devcontainer
.devcontainer/run make test
.devcontainer/run go test ./internal/mirror/...
```

`.devcontainer/run` mounts the repository at `/src`, runs as your user and
keeps the Go module/build cache in the `htadb-go` volume.

## Tests

- **Go** (`go test ./...`): the adb client, listeners/TLS, configuration, the
  API and mirroring run against `internal/adb/adbtest`, a fake adb server that
  speaks the smart-socket protocol (shell v2 and legacy, exec, sync, forwards,
  abstract sockets). `internal/mirror/mirrortest` installs a fake mirroring
  server as the fake device's `app_process`, so the real launch, handshake,
  push, fallback and multiplexing code is exercised.
- **Protocol golden bytes**: `internal/mirror/profile/testdata/golden.json`
  holds vectors from scrcpy v5.0.1's own unit tests; both the Go codec and
  `web/lib/mirror-control.js` (`node --test web/test/`) are checked against it.
- **Extension** (`node --test vscode-extension/test/`): pure logic, plus an
  activation test that runs the real `extension.js` against a stubbed
  `vscode` module, the real `bin/htadb` and `bin/fakeadb`.
- **End to end** (`make test-e2e`, `e2e/run.sh`): `bin/fakeadb` serves two fake
  devices; the mirroring server streams an ffmpeg-generated H.264 clip;
  headless Chromium loads the embedded pages through a small CDP client
  (`e2e/cdp.mjs`, no npm packages) and checks the terminal, PTY shell, CLI
  passthrough, H.264 decoding, hotkeys, clipboard and the 4.x fallback banner,
  plus a clean console. Screenshots land in `testdata-gen/screens/`.

`bin/fakeadb` is also handy for UI work without hardware:

```sh
go build -o bin/fakeadb ./internal/adb/adbtest/fakeadb
ffmpeg -f lavfi -i testsrc2=size=720x1600:rate=30 -t 4 -c:v libx264 -profile:v baseline -bf 0 -f h264 clip.h264
bin/fakeadb -addr 127.0.0.1:15037 -h264 clip.h264 -width 720 -height 1600 &
bin/htadb --adb-server 127.0.0.1:15037 --ui-dir web
```

## UI development

`htadb --ui-dir ./web` serves the live sources. Alternatively serve `web/`
with any static server and point the pages at htadb (CORS is permissive by
default):

```sh
python3 -m http.server 8000 -d web
# http://127.0.0.1:8000/terminal.html?api=http://127.0.0.1:10443
```
