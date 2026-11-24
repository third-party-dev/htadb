// Checks web/lib/mirror-control.js against the upstream golden vectors shared
// with the Go tests (internal/mirror/profile/testdata/golden.json).
// Run: node --test web/test/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { encoders, encoderFor } from '../lib/mirror-control.js';
import { parseDeviceMessage, parseFrame, avcCodecString, splitNALs } from '../lib/mirror-protocol.js';
import { normalize, eventCombo } from '../lib/hotkeys.js';

const golden = JSON.parse(readFileSync(fileURLToPath(new URL('../../internal/mirror/profile/testdata/golden.json', import.meta.url)), 'utf8'));
const hex = (b) => Buffer.from(b).toString('hex');

test('control messages match upstream golden bytes', () => {
  assert.equal(golden.control.length, 23);
  for (const c of golden.control) {
    assert.equal(hex(encoders[c.fn](c.args)), c.hex, c.name);
  }
});

test('encoderFor enforces the profile', () => {
  const v40 = encoderFor({ profile: '4.x', version: '4.0', controlTypes: [...Array(22).keys()] });
  const v50 = encoderFor({ profile: '5.0', version: '5.0.1', controlTypes: [...Array(23).keys()] });
  assert.throws(() => v40.scanFile({ path: '/x' }), /not supported/);
  assert.equal(v40.supports('scanFile'), false);
  assert.equal(hex(v50.scanFile({ path: '/sdcard/Download' })), golden.control.find((c) => c.fn === 'scanFile').hex);
});

test('long text is truncated on a UTF-8 boundary', () => {
  const b = encoders.injectText({ text: 'é'.repeat(200) }); // 400 bytes
  const n = new DataView(b.buffer, b.byteOffset).getUint32(1);
  assert.equal(n, 300);
  const b2 = encoders.injectText({ text: 'a' + 'é'.repeat(200) });
  assert.equal(new DataView(b2.buffer, b2.byteOffset).getUint32(1), 299);
});

test('device messages parse', () => {
  const [clip, ack, uhid] = golden.device.map((d) => parseDeviceMessage(Buffer.from(d.hex, 'hex')));
  assert.deepEqual(clip, { type: 'clipboard', text: 'ABC' });
  assert.equal(ack.sequence, 0x0102030405060708n);
  assert.equal(uhid.id, 42);
  assert.deepEqual([...uhid.data], [1, 2, 3, 4, 5]);
});

test('frames parse', () => {
  for (const m of golden.media) {
    const buf = Buffer.concat([Buffer.from([1]), Buffer.from(m.hex.slice(0, 16), 'hex'), Buffer.from('xy')]);
    const f = parseFrame(buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.length));
    assert.equal(f.config, m.config, m.name);
    assert.equal(f.key, m.key, m.name);
    assert.equal(f.pts, m.config ? 0 : m.pts, m.name);
    assert.equal(Buffer.from(f.data).toString(), 'xy');
  }
});

test('H.264 codec string from SPS', () => {
  const cfg = Uint8Array.from([0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x1f, 0xda, 0, 0, 1, 0x68, 0xce]);
  assert.equal(splitNALs(cfg).length, 2);
  assert.equal(avcCodecString(cfg), 'avc1.42c01f');
});

test('hotkey combos', () => {
  assert.equal(normalize('Shift+Mod+KeyO'), 'Mod+Shift+KeyO');
  assert.equal(eventCombo({ code: 'KeyH', ctrlKey: true, altKey: true }, 'Ctrl+Alt'), 'Mod+KeyH');
  assert.equal(eventCombo({ code: 'KeyH', altKey: true }, 'Ctrl+Alt'), 'Alt+KeyH');
  const e = { code: 'KeyO', altKey: true, shiftKey: true, ctrlKey: false, metaKey: false };
  assert.equal(eventCombo(e, 'Alt'), 'Mod+Shift+KeyO');
  assert.equal(normalize(eventCombo(e, 'Alt')), normalize('Mod+Shift+KeyO'));
  assert.equal(eventCombo({ code: 'AltLeft', altKey: true }, 'Alt'), null);
});
