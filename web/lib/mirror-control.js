// Control message encoders for the mirroring protocol (the scrcpy server's
// ControlMessageReader format: big-endian, one message per WebSocket binary
// frame). Byte layouts are checked against upstream golden vectors in
// internal/mirror/profile/testdata/golden.json (see web/test/).
//
// Profiles: 4.1, 5.0 and 5.0.1 share this implementation; 4.0 lacks
// scanFile. The session `meta` names the profile and lists the accepted
// control type ids, and encoderFor() refuses anything else.

export const TYPE = Object.freeze({
  INJECT_KEYCODE: 0, INJECT_TEXT: 1, INJECT_TOUCH_EVENT: 2, INJECT_SCROLL_EVENT: 3,
  BACK_OR_SCREEN_ON: 4, EXPAND_NOTIFICATION_PANEL: 5, EXPAND_SETTINGS_PANEL: 6,
  COLLAPSE_PANELS: 7, GET_CLIPBOARD: 8, SET_CLIPBOARD: 9, SET_DISPLAY_POWER: 10,
  ROTATE_DEVICE: 11, UHID_CREATE: 12, UHID_INPUT: 13, UHID_DESTROY: 14,
  OPEN_HARD_KEYBOARD_SETTINGS: 15, START_APP: 16, RESET_VIDEO: 17, CAMERA_SET_TORCH: 18,
  CAMERA_ZOOM_IN: 19, CAMERA_ZOOM_OUT: 20, RESIZE_DISPLAY: 21, SCAN_FILE: 22,
});

export const POINTER_ID_MOUSE = 0xffffffffffffffffn; // -1
export const POINTER_ID_GENERIC_FINGER = 0xfffffffffffffffen; // -2
export const COPY_KEY = Object.freeze({ NONE: 0, COPY: 1, CUT: 2 });
export const INJECT_TEXT_MAX = 300;
export const CLIPBOARD_TEXT_MAX = (1 << 18) - 14;

const enc = new TextEncoder();

class Writer {
  constructor(size) { this.buf = new Uint8Array(size); this.view = new DataView(this.buf.buffer); this.o = 0; }
  u8(v) { this.view.setUint8(this.o, v); this.o += 1; return this; }
  u16(v) { this.view.setUint16(this.o, v); this.o += 2; return this; }
  i16(v) { this.view.setInt16(this.o, v); this.o += 2; return this; }
  u32(v) { this.view.setUint32(this.o, v >>> 0); this.o += 4; return this; }
  i32(v) { this.view.setInt32(this.o, v | 0); this.o += 4; return this; }
  u64(v) { this.view.setBigUint64(this.o, BigInt.asUintN(64, toBigInt(v))); this.o += 8; return this; }
  bytes(b) { this.buf.set(b, this.o); this.o += b.length; return this; }
  done() { return this.buf.subarray(0, this.o); }
}

function toBigInt(v) {
  if (typeof v === 'bigint') return v;
  if (typeof v === 'string') return BigInt(v);
  return BigInt(Math.trunc(v));
}

/** UTF-8 encode, truncated to max bytes without splitting a code point. */
function utf8(s, max) {
  let b = enc.encode(s);
  if (max !== undefined && b.length > max) {
    let n = max;
    while (n > 0 && (b[n] & 0xc0) === 0x80) n--; // continuation byte
    b = b.subarray(0, n);
  }
  return b;
}

/** Float in [0, 1] → u16 fixed point (sc_float_to_u16fp). */
export function u16fp(f) {
  let u = Math.trunc(f * 65536);
  if (u >= 0xffff) u = 0xffff;
  return Math.max(0, u);
}

/** Float in [-1, 1] → i16 fixed point (sc_float_to_i16fp). */
export function i16fp(f) {
  let i = Math.trunc(f * 32768);
  if (i >= 0x7fff) i = 0x7fff;
  if (i < -0x8000) i = -0x8000;
  return i;
}

function pos(w, a) {
  w.i32(a.x).i32(a.y).u16(a.width).u16(a.height);
}

const empty = (type) => () => Uint8Array.of(type);

/** Raw encoders keyed by name; args objects match golden.json. */
export const encoders = {
  injectKeycode: (a) => new Writer(14).u8(TYPE.INJECT_KEYCODE).u8(a.action).u32(a.keycode).u32(a.repeat || 0).u32(a.metastate || 0).done(),
  injectText: (a) => { const t = utf8(a.text, INJECT_TEXT_MAX); return new Writer(5 + t.length).u8(TYPE.INJECT_TEXT).u32(t.length).bytes(t).done(); },
  injectTouch: (a) => {
    const w = new Writer(32).u8(TYPE.INJECT_TOUCH_EVENT).u8(a.action).u64(a.pointerId);
    pos(w, a);
    return w.u16(u16fp(a.pressure)).u32(a.actionButton || 0).u32(a.buttons || 0).done();
  },
  injectScroll: (a) => {
    const w = new Writer(21).u8(TYPE.INJECT_SCROLL_EVENT);
    pos(w, a);
    const clamp = (v) => Math.max(-16, Math.min(16, v)) / 16;
    return w.i16(i16fp(clamp(a.hscroll))).i16(i16fp(clamp(a.vscroll))).u32(a.buttons || 0).done();
  },
  backOrScreenOn: (a) => Uint8Array.of(TYPE.BACK_OR_SCREEN_ON, a.action),
  expandNotificationPanel: empty(TYPE.EXPAND_NOTIFICATION_PANEL),
  expandSettingsPanel: empty(TYPE.EXPAND_SETTINGS_PANEL),
  collapsePanels: empty(TYPE.COLLAPSE_PANELS),
  getClipboard: (a) => Uint8Array.of(TYPE.GET_CLIPBOARD, a.copyKey),
  setClipboard: (a) => {
    const t = utf8(a.text, CLIPBOARD_TEXT_MAX);
    return new Writer(14 + t.length).u8(TYPE.SET_CLIPBOARD).u64(a.sequence).u8(a.paste ? 1 : 0).u32(t.length).bytes(t).done();
  },
  setDisplayPower: (a) => Uint8Array.of(TYPE.SET_DISPLAY_POWER, a.on ? 1 : 0),
  rotateDevice: empty(TYPE.ROTATE_DEVICE),
  uhidCreate: (a) => {
    const n = utf8(a.name || '', 255);
    const d = Uint8Array.from(a.reportDesc);
    return new Writer(10 + n.length + d.length).u8(TYPE.UHID_CREATE).u16(a.id).u16(a.vendorId).u16(a.productId)
      .u8(n.length).bytes(n).u16(d.length).bytes(d).done();
  },
  uhidInput: (a) => { const d = Uint8Array.from(a.data); return new Writer(5 + d.length).u8(TYPE.UHID_INPUT).u16(a.id).u16(d.length).bytes(d).done(); },
  uhidDestroy: (a) => new Writer(3).u8(TYPE.UHID_DESTROY).u16(a.id).done(),
  openHardKeyboardSettings: empty(TYPE.OPEN_HARD_KEYBOARD_SETTINGS),
  startApp: (a) => { const n = utf8(a.name, 255); return new Writer(2 + n.length).u8(TYPE.START_APP).u8(n.length).bytes(n).done(); },
  resetVideo: empty(TYPE.RESET_VIDEO),
  cameraSetTorch: (a) => Uint8Array.of(TYPE.CAMERA_SET_TORCH, a.on ? 1 : 0),
  cameraZoomIn: empty(TYPE.CAMERA_ZOOM_IN),
  cameraZoomOut: empty(TYPE.CAMERA_ZOOM_OUT),
  resizeDisplay: (a) => new Writer(5).u8(TYPE.RESIZE_DISPLAY).u16(a.width).u16(a.height).done(),
  scanFile: (a) => { const p = utf8(a.path, 256); return new Writer(5 + p.length).u8(TYPE.SCAN_FILE).u32(p.length).bytes(p).done(); },
};

const typeOf = {
  injectKeycode: TYPE.INJECT_KEYCODE, injectText: TYPE.INJECT_TEXT, injectTouch: TYPE.INJECT_TOUCH_EVENT,
  injectScroll: TYPE.INJECT_SCROLL_EVENT, backOrScreenOn: TYPE.BACK_OR_SCREEN_ON,
  expandNotificationPanel: TYPE.EXPAND_NOTIFICATION_PANEL, expandSettingsPanel: TYPE.EXPAND_SETTINGS_PANEL,
  collapsePanels: TYPE.COLLAPSE_PANELS, getClipboard: TYPE.GET_CLIPBOARD, setClipboard: TYPE.SET_CLIPBOARD,
  setDisplayPower: TYPE.SET_DISPLAY_POWER, rotateDevice: TYPE.ROTATE_DEVICE, uhidCreate: TYPE.UHID_CREATE,
  uhidInput: TYPE.UHID_INPUT, uhidDestroy: TYPE.UHID_DESTROY, openHardKeyboardSettings: TYPE.OPEN_HARD_KEYBOARD_SETTINGS,
  startApp: TYPE.START_APP, resetVideo: TYPE.RESET_VIDEO, cameraSetTorch: TYPE.CAMERA_SET_TORCH,
  cameraZoomIn: TYPE.CAMERA_ZOOM_IN, cameraZoomOut: TYPE.CAMERA_ZOOM_OUT, resizeDisplay: TYPE.RESIZE_DISPLAY,
  scanFile: TYPE.SCAN_FILE,
};

/** Known profiles → accepted types (used when meta omits controlTypes). */
const PROFILE_TYPES = {
  '4.0': [...Array(22).keys()],
  '4.x': [...Array(23).keys()],
  '5.0': [...Array(23).keys()],
};

/**
 * Returns an encoder object for a session's profile. Methods have the same
 * names as `encoders`; calling one the profile does not support throws.
 * `supports(name)` checks first.
 */
export function encoderFor(meta) {
  const ids = new Set(meta.controlTypes || PROFILE_TYPES[meta.version] || PROFILE_TYPES[meta.profile] || []);
  const out = { profile: meta.profile, version: meta.version, supports: (name) => ids.has(typeOf[name]) };
  for (const [name, fn] of Object.entries(encoders)) {
    out[name] = (args = {}) => {
      if (!ids.has(typeOf[name])) throw new Error(`${name} is not supported by mirroring server ${meta.version}`);
      return fn(args);
    };
  }
  return out;
}
