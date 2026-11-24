# htadb API (`/api/v1`)

All paths are under `/api/v1`. `{s}` is a device serial (URL-encode
`host:port` serials). Request and response bodies are JSON unless noted.
CORS is permissive by default (`--cors-origin`). There is no
authentication.

## Errors

Failures return `{"error": "…", "code": "…"}`:

| Status | Code | When |
| --- | --- | --- |
| 400 | `bad_request` | Invalid parameters |
| 404 | `device_not_found`, `not_found`, `server_not_found` | Unknown serial, missing file, no mirroring server for the version |
| 409 | `incompatible_version` | Mirroring server version outside the allowlist, or the device SDK is too old for it |
| 501 | `cli_disabled`, `mirror_disabled` | Feature off |
| 502 | `adb_fail`, `install_failed`, `uninstall_failed`, `screencap_failed` | adb (or adbd) answered `FAIL`, or the package manager refused |
| 503 | `adb_unavailable` | The adb server is unreachable |
| 504 | `timeout` | A command exceeded its timeout |

WebSocket endpoints report errors that happen before the upgrade as these
HTTP errors; afterwards as `{"type":"error",…}` text frames.

## Endpoints

| Method | Path | |
| --- | --- | --- |
| GET | `/version` | `{version, api, cli, mirror, adbServerVersion \| adbServerError}` |
| GET | `/devices` | `[{serial, state, product, model, device, transportId, usb}]` (`devices -l`) |
| WS | `/devices/track` | First `{"type":"snapshot","devices":[…],"adb":bool}`, then `{"type":"add"\|"remove"\|"state"\|"change","device":{…},"oldState"}` |
| GET | `/devices/{s}` | `{device, features[], props{key props}}` |
| GET | `/devices/{s}/props` | Full `getprop` map |
| POST | `/devices/{s}/shell` | `{command, timeout?, stdin?}` → `{stdout, stderr, exitCode, encoding?}`. `exitCode` is `-1` on devices without shell v2; `encoding: "base64"` when output is not UTF-8. Default timeout 60 s. |
| WS | `/devices/{s}/shell` | Interactive shell, see below. Query: `cmd`, `pty` (default on when `cmd` is empty), `term`, `rows`, `cols` |
| POST | `/devices/{s}/exec` | `?cmd=` or `{command}` → raw binary stdout (`exec:`) |
| GET | `/devices/{s}/screenshot` | `image/png` |
| GET | `/devices/{s}/files?path=` | `{path, stat{name, mode, size, mtime, type, perm}, entries[]?}` (entries for directories) |
| GET | `/devices/{s}/files/content?path=` | Streamed download (`Content-Length` for regular files) |
| PUT | `/devices/{s}/files/content?path=&mode=` | Streamed upload of the body (`mode` octal, default `0644`) → `{path, bytes, mode}` |
| POST | `/devices/{s}/install?args=` | APK body. `args` are pm flags (`-r -g …`). Streamed into `cmd package install -S <size>`; bodies without `Content-Length` are spooled to a temp file first. → `{output, success}` |
| POST | `/devices/{s}/uninstall` | `{package, keepData}` |
| POST | `/devices/{s}/reboot` | `{target}`: `""`, `bootloader`, `recovery`, `sideload`, `sideload-auto-reboot`, `fastboot`, `edl` |
| POST | `/devices/{s}/root`, `/unroot`, `/remount` | `{output}` |
| GET | `/devices/{s}/forwards` | `[{serial, local, remote}]` |
| POST | `/devices/{s}/forwards` | `{local, remote, noRebind}`; `local: "tcp:0"` returns the allocated `port` |
| DELETE | `/devices/{s}/forwards?local=` | One forward, or all for the device when `local` is omitted |
| GET/POST/DELETE | `/devices/{s}/reverses` | Same shape; `remote` is the device side, `local` the host side; DELETE takes `?remote=` |
| POST | `/connect`, `/disconnect` | `{address}` → `{output}` |
| POST | `/cli` | `{args[], serial?, timeout?}` → `{stdout, stderr, exitCode}` |
| WS | `/cli?arg=…&arg=…&serial=` | Streamed CLI, see below |
| GET | `/mirror/servers` | `{builds[{version, sha256, size, profile, tested, latest, source, upstream}], profiles[], defaultProfile, defaultVersion, fallback, allowUntested, deviceDir}` |
| GET | `/devices/{s}/mirror/servers` | `{dir, jars[{name, path, version, size, mtime, sha256, verified, known, profile, tested}], fallback?}` |
| WS | `/devices/{s}/mirror` | Mirroring session, see [MIRRORING.md](MIRRORING.md) |
| DELETE | `/devices/{s}/mirror/fallback` | Forget the remembered 4.x fallback → `{cleared}` |
| GET | `/healthz` | `{status, adb, adbServerVersion \| adbError}` |
| GET | `/metrics` | expvar JSON (`htadb_http_requests`, `htadb_ws_active`, `htadb_http_errors`, Go runtime) |

Every response carries `X-Request-Id` (taken from the request header when
present); logs use the same id.

## Shell WebSocket

- Client → server: **binary** frames are stdin. **Text** frames:
  `{"type":"resize","rows":R,"cols":C}`, `{"type":"eof"}` (close stdin).
- Server → client: **binary** frames are stdout (and stderr in PTY mode).
  **Text** frames: `{"type":"stderr","data":"…"}` (non-PTY only) and
  `{"type":"exit","code":N}` before a normal close.
- Uses shell protocol v2 (`shell,v2,pty|raw,TERM=…:<cmd>`); devices without
  `shell_v2` fall back to `shell:` (no exit code: `-1`, no resize).

## CLI WebSocket

Runs `exec.Command(adbPath, "-P", port, "-s", serial, args…)` without a shell,
pointed at the same adb server as htadb. Binary frames carry stdout and
stdin; `{"type":"stderr","data"}`, `{"type":"exit","code"}` and
`{"type":"error","error"}` arrive as text. Send `{"type":"signal","name":"SIGINT"}`
for Ctrl+C (Linux/macOS; Windows kills the process) and `{"type":"eof"}` to
close stdin. Closing the socket interrupts the process, then kills it after
3 seconds.
