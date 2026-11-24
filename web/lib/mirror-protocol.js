// Decoding side of htadb's mirroring WebSocket, plus H.264 and Android key
// helpers.
//
// Server → browser binary frames:
//   0x01 + u64 pts/flags + payload   one video packet
//   0x02 + u64 pts/flags + payload   one audio packet
//   0x03 + bytes                     one device message
// Text frames are JSON: meta, session, log, error.

export const TAG_VIDEO = 1;
export const TAG_AUDIO = 2;
export const TAG_DEVICE = 3;

const FLAG_CONFIG = 1n << 62n;
const FLAG_KEY = 1n << 61n;
const PTS_MASK = FLAG_KEY - 1n;

/** Parse one binary frame. */
export function parseFrame(buf) {
  const u8 = new Uint8Array(buf);
  const tag = u8[0];
  if (tag === TAG_DEVICE) return { tag, data: u8.subarray(1) };
  const v = new DataView(u8.buffer, u8.byteOffset + 1, 8).getBigUint64(0);
  const config = (v & FLAG_CONFIG) !== 0n;
  return {
    tag,
    config,
    key: !config && (v & FLAG_KEY) !== 0n,
    pts: config ? 0 : Number(v & PTS_MASK),
    data: u8.subarray(9),
  };
}

const dec = new TextDecoder();

/** Parse a device message (clipboard, ack_clipboard, uhid_output). */
export function parseDeviceMessage(b) {
  const v = new DataView(b.buffer, b.byteOffset, b.byteLength);
  switch (b[0]) {
    case 0: {
      const n = v.getUint32(1);
      return { type: 'clipboard', text: dec.decode(b.subarray(5, 5 + n)) };
    }
    case 1:
      return { type: 'ack_clipboard', sequence: v.getBigUint64(1) };
    case 2: {
      const n = v.getUint16(3);
      return { type: 'uhid_output', id: v.getUint16(1), data: b.subarray(5, 5 + n) };
    }
  }
  return { type: 'unknown', raw: b };
}

// ---- H.264 (Annex B) ----

/** Split an Annex B buffer into NAL units (without start codes). */
export function splitNALs(b) {
  const out = [];
  let i = 0;
  let start = -1;
  while (i + 3 <= b.length) {
    if (b[i] === 0 && b[i + 1] === 0 && (b[i + 2] === 1 || (b[i + 2] === 0 && b[i + 3] === 1))) {
      const sc = b[i + 2] === 1 ? 3 : 4;
      if (start >= 0) out.push(b.subarray(start, i));
      i += sc;
      start = i;
    } else {
      i++;
    }
  }
  if (start >= 0) out.push(b.subarray(start));
  return out;
}

/** "avc1.PPCCLL" from the SPS in a config packet (null if none). */
export function avcCodecString(config) {
  for (const nal of splitNALs(config)) {
    if ((nal[0] & 0x1f) === 7 && nal.length >= 4) {
      const hex = (x) => x.toString(16).padStart(2, '0');
      return `avc1.${hex(nal[1])}${hex(nal[2])}${hex(nal[3])}`;
    }
  }
  return null;
}

// ---- Android input constants ----

export const ACTION = Object.freeze({ DOWN: 0, UP: 1, MOVE: 2, CANCEL: 3, HOVER_MOVE: 7 });
export const BUTTON = Object.freeze({ PRIMARY: 1, SECONDARY: 2, TERTIARY: 4, BACK: 8, FORWARD: 16 });

export const KEYCODE = Object.freeze({
  HOME: 3, BACK: 4, DPAD_UP: 19, DPAD_DOWN: 20, DPAD_LEFT: 21, DPAD_RIGHT: 22,
  VOLUME_UP: 24, VOLUME_DOWN: 25, POWER: 26, ENTER: 66, DEL: 67, MENU: 82,
  APP_SWITCH: 187, ESCAPE: 111, FORWARD_DEL: 112, TAB: 61, SPACE: 62,
  MOVE_HOME: 122, MOVE_END: 123, PAGE_UP: 92, PAGE_DOWN: 93, INSERT: 124,
  VOLUME_MUTE: 164, MEDIA_PLAY_PAUSE: 85, CUT: 277, COPY: 278, PASTE: 279,
});

/** KeyboardEvent.code → Android keycode. */
export const CODE_TO_KEYCODE = (() => {
  const m = {
    Enter: 66, NumpadEnter: 160, Backspace: 67, Delete: 112, Tab: 61, Space: 62, Escape: 111,
    ArrowUp: 19, ArrowDown: 20, ArrowLeft: 21, ArrowRight: 22, Home: 122, End: 123,
    PageUp: 92, PageDown: 93, Insert: 124, CapsLock: 115, NumLock: 143, ScrollLock: 116,
    ShiftLeft: 59, ShiftRight: 60, ControlLeft: 113, ControlRight: 114, AltLeft: 57, AltRight: 58,
    MetaLeft: 117, MetaRight: 118, ContextMenu: 82,
    Comma: 55, Period: 56, Minus: 69, Equal: 70, BracketLeft: 71, BracketRight: 72,
    Backslash: 73, Semicolon: 74, Quote: 75, Slash: 76, Backquote: 68,
    NumpadAdd: 157, NumpadSubtract: 156, NumpadMultiply: 155, NumpadDivide: 154, NumpadDecimal: 158,
    AudioVolumeUp: 24, AudioVolumeDown: 25, AudioVolumeMute: 164, MediaPlayPause: 85,
    MediaTrackNext: 87, MediaTrackPrevious: 88, MediaStop: 86,
  };
  for (let i = 0; i < 26; i++) m['Key' + String.fromCharCode(65 + i)] = 29 + i;
  for (let i = 0; i < 10; i++) { m['Digit' + i] = 7 + i; m['Numpad' + i] = 144 + i; }
  for (let i = 1; i <= 12; i++) m['F' + i] = 130 + i;
  return Object.freeze(m);
})();

export const META = Object.freeze({
  SHIFT_ON: 0x1, ALT_ON: 0x2, ALT_LEFT_ON: 0x10, ALT_RIGHT_ON: 0x20, SHIFT_LEFT_ON: 0x40, SHIFT_RIGHT_ON: 0x80,
  CTRL_ON: 0x1000, CTRL_LEFT_ON: 0x2000, CTRL_RIGHT_ON: 0x4000,
  META_ON: 0x10000, META_LEFT_ON: 0x20000, META_RIGHT_ON: 0x40000,
  CAPS_LOCK_ON: 0x100000, NUM_LOCK_ON: 0x200000,
});

/** Android metastate from a KeyboardEvent. */
export function metaState(e) {
  let m = 0;
  if (e.shiftKey) m |= META.SHIFT_ON | META.SHIFT_LEFT_ON;
  if (e.altKey) m |= META.ALT_ON | META.ALT_LEFT_ON;
  if (e.ctrlKey) m |= META.CTRL_ON | META.CTRL_LEFT_ON;
  if (e.metaKey) m |= META.META_ON | META.META_LEFT_ON;
  if (e.getModifierState && e.getModifierState('CapsLock')) m |= META.CAPS_LOCK_ON;
  if (e.getModifierState && e.getModifierState('NumLock')) m |= META.NUM_LOCK_ON;
  return m;
}
