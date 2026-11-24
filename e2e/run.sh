#!/bin/sh
# End-to-end smoke test: fakeadb (fake devices; the mirroring server streams
# an ffmpeg-generated H.264 clip) + htadb (embedded UI) + headless Chromium.
set -eu
cd "$(dirname "$0")/.."
out=testdata-gen
mkdir -p "$out"
[ -f "$out/clip.h264" ] || ffmpeg -hide_banner -loglevel error -y -f lavfi -i testsrc2=size=720x1600:rate=30 -t 4 \
	-c:v libx264 -profile:v baseline -pix_fmt yuv420p -g 30 -bf 0 -f h264 "$out/clip.h264"
go build -o bin/fakeadb ./internal/adb/adbtest/fakeadb
ADB_PORT=${ADB_PORT:-15037}
HTTP_PORT=${HTTP_PORT:-18443}
cfg=$(mktemp -d)
./bin/fakeadb -addr 127.0.0.1:$ADB_PORT -h264 "$out/clip.h264" -width 720 -height 1600 \
	-fail-versions 5.0.1 -fail-on emulator-5554 > "$out/fakeadb.log" 2>&1 &
fake=$!
./bin/htadb --adb-server 127.0.0.1:$ADB_PORT --listen http://127.0.0.1:$HTTP_PORT --config-dir "$cfg" > "$out/htadb.log" 2>&1 &
ht=$!
trap 'kill $fake $ht 2>/dev/null; rm -rf "$cfg"' EXIT
for i in $(seq 50); do curl -sf http://127.0.0.1:$HTTP_PORT/healthz >/dev/null && break; sleep 0.1; done
node e2e/smoke.mjs http://127.0.0.1:$HTTP_PORT "$out/screens"
