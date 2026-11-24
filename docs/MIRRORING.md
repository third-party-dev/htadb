# Mirroring

htadb mirrors devices by running the scrcpy server (`scrcpy-server`,
Apache-2.0) on the device and speaking its protocol. It launches the server
with `tunnel_forward=true` and connects to its abstract sockets through adb,
so no `adb reverse` or host port is needed, and bridges the sockets to one
WebSocket. The browser decodes H.264 with WebCodecs.

## Protocol profiles

htadb's protocol tables come from diffing `doc/develop.md` and the server
sources (`Options`, `ControlMessage`, `ControlMessageReader`,
`DeviceMessageWriter`, `Streamer`, `DesktopConnection`, `VideoCodec`) across
the upstream tags v4.0 (2026-05-12), v4.1 (2026-07-12), v5.0 (2026-10-05)
and v5.0.1 (2026-10-08):

| Change | 4.0 | 4.1 | 5.0 / 5.0.1 |
| --- | --- | --- | --- |
| Control message 22 `SCAN_FILE` | — | ✓ | ✓ |
| Video codecs `vp8`, `vp9` | — | ✓ | ✓ |
| Launch option `ignore_video_encoder_constraints` | — | ✓ | ✓ |
| "Client resized" bit in the session packet | — | ✓ | ✓ |
| `minSdkVersion` | 21 | 21 | 21 |

4.1 → 5.0 → 5.0.1 changes nothing on the wire: 5.0's changes are host-side
(GPU decoding) plus server-internal audio-capture and camera fixes. So 4.1
and 5.0 share one implementation and stay separate allowlist entries; 4.0 is
the same implementation without the 4.1 additions.

| Profile | Allowlisted versions | Embedded |
| --- | --- | --- |
| `5.0` (default) | 5.0, 5.0.1 | 5.0.1 |
| `4.x` (fallback) | 4.0, 4.1 | 4.1 |

A profile defines the accepted launch options, the handshake, the packet
header, the control and device message tables and `minSdk`
(`internal/mirror/profile`). Golden-byte tests use scrcpy's own test vectors
(`internal/mirror/profile/testdata/golden.json`), for both the Go codec and
the browser's encoder (`web/lib/mirror-control.js`).

Versions are allowlisted exactly. A version outside the list whose major
matches a profile (e.g. 5.1) is refused with 409 unless
`--mirror-allow-untested`, in which case the major's newest profile is used
and the page shows a warning. Unknown majors are always refused. The server
itself refuses to start when its version differs from the version argument,
so the version picks both the jar and the argument; the profile decides
whether htadb can speak to it.

## Wire format

Handshake, for `tunnel_forward=true`:

1. The server listens on `localabstract:scrcpy_<scid>` (`scid` = 8 hex digits,
   31-bit random). htadb connects the video, audio and control sockets in
   that order, retrying while the server starts.
2. The first socket carries a dummy byte (it proves something is listening),
   then the device name (64 bytes, NUL-padded).
3. Video and audio sockets start with a codec id (u32; 0 = stream disabled,
   1 = configuration error).
4. Video then carries 12-byte *session packets* (MSB set; width, height; LSB
   of the first word = client resized, 4.1+) and media packets: u64 pts/flags
   (bit 62 config, bit 61 keyframe) + u32 size + payload.

The browser WebSocket multiplexes everything:

| Direction | Frame | Content |
| --- | --- | --- |
| server → browser | binary `0x01` + 8-byte pts/flags + payload | one video packet |
| server → browser | binary `0x02` + 8-byte pts/flags + payload | one audio packet |
| server → browser | binary `0x03` + bytes | one whole device message (clipboard, ack, uhid output) |
| server → browser | text `meta` | serial, deviceName, version, profile, tested, verified, jar, scid, controlTypes, handshake, options, videoCodec, width, height, audio/control flags, warnings, fallbackFrom/fallbackReason/fallbackDetail |
| server → browser | text `session` | new width/height (rotation, resize) |
| server → browser | text `log`, `error` | server output; failures (`fatal:false` for rejected control messages) |
| browser → server | binary | one control message per frame |

htadb validates each control message against the session's profile (type and
exact length) before writing it to the control socket: an unknown type
would make the server's controller throw. `meta` is sent when the first media
packet arrives, so a normal session sees exactly one.

Session query parameters: `version`, `server_path`, `launch=false&scid=…`
(attach), and launch options from the profile's allowlist (`max_size`,
`video_bit_rate`, `max_fps`, `audio`, `control`, `stay_awake`, `crop`,
`display_id`, `video_encoder`, …). htadb sets `scid`, `tunnel_forward` and
the `send_*`/`raw_stream` options itself and rejects them from clients.
Defaults: `video_codec=h264`, `audio=false`, `control=true`.

## Server builds

- **Embedded**: release builds embed the newest jar of each profile
  (5.0.1 and 4.1), fetched by `make fetchdeps` from the GitHub releases and
  verified against the sha256 published in each tag's `install_release.sh`.
  scrcpy's LICENSE and a NOTICE ship with htadb (`htadb licenses`, release
  archives). `-tags nomirrorjar` leaves the jars out.
- **Supplied**: `--mirror-server <path>` (repeatable). The version comes from
  the file name (`scrcpy-server-v5.0.jar` or the upstream asset name
  `scrcpy-server-v5.0`); `--mirror-server 5.0=<path>` sets it explicitly.
  Supplied files override embedded ones of the same version.

`GET /api/v1/mirror/servers` lists them (version, sha256, profile, latest,
whether the hash matches the upstream release).

## On the device

- **Path**: `/data/local/tmp/htadb/scrcpy-server-v<version>.jar`. The scrcpy
  CLI uses `/data/local/tmp/scrcpy-server.jar`; htadb never touches it, and
  several versions coexist.
- **Pattern**: `^scrcpy-server-v(\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.]+)?)\.jar$`.
- **Inventory**: `GET /api/v1/devices/{s}/mirror/servers` lists the directory
  (sync LIST), parses versions from names, and marks a jar `verified` when its
  size and sha256 (toybox `sha256sum` on the device) match a build htadb
  holds, or its sha256 matches the pinned upstream release.

Push, tolerant of earlier sessions:

1. A per-device, per-version mutex serializes the check and the push.
2. sync STAT the target; missing → push.
3. Same size → `sha256sum` on the device; equal → reuse.
4. Otherwise push to `…/.scrcpy-server-v<version>.jar.<random>.tmp` and
   `mv -f` it into place, so no session launches a half-written jar.
5. If htadb has no build of the requested version but the device holds a jar
   for it, that jar is used with the version from its name; the session is
   marked unverified unless its hash matches the upstream release.

**Launching from a session link.** With the default `cleanup=true`, the
server deletes its own classpath file at startup (`CleanUp.unlinkSelf()`),
which would remove the shared jar after every session. htadb therefore
launches each session from a hard link
`/data/local/tmp/htadb/.session-<scid>.jar` (falling back to a copy); the
server removes the link, the named jar stays for the next session, and
cleanup (restoring stay-awake, show-touches, screen-off timeout) keeps
working. htadb also removes the link when the session ends.

## Version selection

Highest priority first:

1. `?version=`
2. `?server_path=` (pre-installed jar; version from its name)
3. The device's entry in config.toml: `[mirror.devices."R58M12345"] version = "4.1"`
4. `--mirror-default-version`
5. The newest version available (host builds and device jars)

The page's version picker lists host builds and the device's jars. Attach
mode (`?launch=false&scid=…&version=…`) only connects to a server that is
already running.

## Automatic fallback to 4.x

- **Scope**: only when htadb chose the version itself (4 or 5 above). An
  explicit `?version=`, `server_path` or per-device entry never falls back; a
  failure names the version.
- **Before launch**: htadb reads `ro.build.version.sdk` and compares it with
  the profile's `minSdk`; below 5.0's minimum it goes straight to the newest
  4.x. (Today every profile's `minSdk` is 21, so this path only matters for
  future profiles.)
- **At launch**: if the 5.0 server fails before the first video packet
  (server exits, socket connection retries run out, handshake fails, or no
  packet within 20 s), htadb tears down and retries once with the newest 4.x.
  A failure after streaming started never falls back.
- **Reporting**: `meta` carries `fallbackFrom`, `fallbackReason` (`sdk` or
  `launch`) and `fallbackDetail` (the error plus the server's last log lines).
  The page shows "Running 4.1. 5.0.1 failed at launch: … htadb will use 4.1
  for this device until it reconnects." with *Server log* and *Retry with
  5.0.1* buttons.
- **Memory**: a launch fallback is remembered per device in memory and
  cleared when the device reconnects (device tracking), when htadb restarts,
  or by `DELETE /api/v1/devices/{s}/mirror/fallback` (the Retry button).
- **Control**: `--mirror-fallback=false`; `htadb.mirror.fallback` in VS Code.

## Browser side

- **Video**: `VideoDecoder` with Annex B H.264; the codec string
  (`avc1.PPCCLL`) comes from the SPS in the config packet. Config packets are
  merged into the next keyframe, and the decoder is reconfigured when the
  config changes (rotation, resize). Frames are drawn to a canvas (latest
  frame wins; no WASM).
- **Audio** (opt-in; Android 11+): Opus `AudioDecoder` → scheduled
  `AudioContext` buffers, started by the Connect click.
- **Input**: mouse uses the mouse pointer id (−1), touch uses per-finger ids;
  coordinates are scaled to the frame size. Wheel → scroll; right click → back
  or screen on; middle click → home. Printable characters are injected as
  text; other keys as keycodes with metastate.
- **Clipboard**: the device clipboard goes to `navigator.clipboard` (textarea
  fallback, and always shown in the side panel); the browser `paste` event
  (Ctrl/Cmd+V) becomes set-clipboard with paste.
- **Hotkeys**: action → combo registry with a configurable modifier (Alt,
  Ctrl+Alt, Super, Ctrl); defaults follow scrcpy's shortcuts. The dialog
  rebinds, resets, exports and imports JSON; bindings persist in localStorage
  or come from `config.js`. Keyboard Lock captures OS-reserved combos in
  fullscreen on Chromium.
- **Secure context**: WebCodecs and the async Clipboard API need https or
  loopback; pages show a banner otherwise.

## Testing on real devices

Automated tests use a fake device. Before a release, check on hardware:

- several Android versions (including < 11 for no-audio) with the 4.x and 5.0 servers;
- a jar left by a previous session (reuse) and a tampered one (re-push);
- the fallback with a device where 5.0 fails;
- the VS Code integrated browser on Linux, macOS and Windows.
