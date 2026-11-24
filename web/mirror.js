// Mirroring viewer: decodes the device's H.264 stream with WebCodecs, draws
// it to a canvas, and turns pointer/keyboard/clipboard input into control
// messages for the mirroring server (scrcpy protocol).

import config from './config.js';
import * as api from './lib/api.js';
import { Hotkeys, display as comboDisplay } from './lib/hotkeys.js';
import { icon } from './lib/icons.js';
import { encoderFor, POINTER_ID_MOUSE, COPY_KEY, INJECT_TEXT_MAX } from './lib/mirror-control.js';
import {
  parseFrame, parseDeviceMessage, avcCodecString, TAG_VIDEO, TAG_AUDIO, TAG_DEVICE,
  ACTION, BUTTON, KEYCODE, CODE_TO_KEYCODE, metaState,
} from './lib/mirror-protocol.js';
import { checkSecureContext } from './lib/secure-context.js';
import { banner, clearBanners, pageLink } from './lib/ui.js';

const $ = (id) => document.getElementById(id);
const banners = $('banners');
const secure = checkSecureContext(banners, 'WebCodecs (video decoding) and clipboard sync');
$('home').href = pageLink('index.html');
$('nav-mirror').href = pageLink('mirror.html');

// ---------------------------------------------------------------------------
// Options (defaults from config.js, e.g. the VS Code extension)

const mcfg = config.mirror || {};
const defaults = { maxSize: '', bitrate: '', fps: '', audio: false, control: true, stayAwake: true, turnScreenOff: false, ...(mcfg.defaults || {}) };
function setSelect(sel, v) {
  v = v === undefined || v === null || v === 0 ? '' : String(v);
  if (![...sel.options].some((o) => o.value === v)) sel.append(new Option(v, v));
  sel.value = v;
}
setSelect($('max_size'), api.param('max_size', defaults.maxSize));
setSelect($('bitrate'), defaults.bitrate);
setSelect($('max_fps'), defaults.fps);
$('audio').checked = !!defaults.audio;
$('control').checked = defaults.control !== false;
$('stay_awake').checked = defaults.stayAwake !== false;
$('screen_off').checked = !!defaults.turnScreenOff;

const canvas = $('screen');
const ctx2d = canvas.getContext('2d', { alpha: false, desynchronized: true });
const stage = $('stage');
const phone = $('phone');

const state = {
  ws: null,
  meta: null,
  ctl: null,
  frameW: 0,
  frameH: 0,
  connected: false,
  native: false,
  clipSeq: 1n,
  pressedKeys: new Map(), // code → keycode sent with DOWN
  stats: { frames: 0, decoded: 0, dropped: 0, bytes: 0, lastFrames: 0, lastBytes: 0, fps: 0, kbps: 0 },
  logs: [],
  errors: [],
};
window.htadbMirror = state; // for tests and debugging

// ---------------------------------------------------------------------------
// UI helpers

function setStatus(text, cls = '') {
  $('status').textContent = text;
  $('status-dot').className = 'dot ' + cls;
}

function addLog(level, line) {
  state.logs.push({ level, line });
  if (state.logs.length > 1000) state.logs.shift();
  const p = $('logpanel');
  const div = document.createElement('div');
  div.className = level;
  div.textContent = line;
  p.append(div);
  while (p.childNodes.length > 1000) p.firstChild.remove();
  if (!p.hidden) p.scrollTop = p.scrollHeight;
}

function toggleLog(show) {
  const p = $('logpanel');
  p.hidden = show === undefined ? !p.hidden : !show;
  if (!p.hidden) { p.scrollTop = p.scrollHeight; p.scrollIntoView({ block: 'nearest' }); }
}

$('ph-icon').innerHTML = icon('screen', { size: 40, stroke: '#5E7183', width: 1.5 });
function placeholder(title, sub = '') {
  const p = $('placeholder');
  p.hidden = !title;
  $('ph-title').textContent = title || '';
  $('ph-sub').textContent = sub;
  layout();
}

// The mirroring server's last error line (its log lines start with
// "[server] LEVEL: ").
function lastServerLine() {
  const server = state.logs.filter((l) => l.line.startsWith('[server]'));
  const pick = [...server].reverse().find((l) => l.level === 'error') || server[server.length - 1];
  return pick ? pick.line.replace(/^\[server\] [A-Z]+: /, '') : '';
}

// Fit the canvas (inside the phone frame) to the screen area.
function layout() {
  const w = state.frameW || canvas.width || 1080;
  const h = state.frameH || canvas.height || 2400;
  const cs = getComputedStyle(stage);
  const availW = stage.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight) - (state.native ? 0 : 24);
  const availH = stage.clientHeight - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom) - (state.native ? 0 : 24);
  const scale = state.native ? 1 / devicePixelRatio : Math.max(0.05, Math.min(availW / w, availH / h));
  canvas.style.width = Math.round(w * scale) + 'px';
  canvas.style.height = Math.round(h * scale) + 'px';
  phone.classList.toggle('native', state.native);
  stage.style.overflow = state.native ? 'auto' : 'hidden';
}
new ResizeObserver(() => layout()).observe(stage);

// ---------------------------------------------------------------------------
// Device and version pickers

const deviceSel = $('device');
const versionSel = $('version');
const wanted = api.param('serial') || '';
let lastVersionsFor = null;

api.trackDevices((ds) => {
  const cur = deviceSel.value || wanted;
  deviceSel.innerHTML = '';
  for (const d of ds) {
    const label = d.serial + (d.state !== 'device' ? ` (${d.state})` : '');
    deviceSel.append(new Option(label, d.serial, false, d.serial === cur));
  }
  if (cur && !ds.some((d) => d.serial === cur)) deviceSel.append(new Option(`${cur} (not attached)`, cur, false, true));
  if (!deviceSel.options.length) deviceSel.append(new Option('no devices', ''));
  if (deviceSel.value !== lastVersionsFor) loadVersions();
});

async function loadVersions() {
  const serial = deviceSel.value;
  lastVersionsFor = serial;
  $('nav-term').href = pageLink('terminal.html', serial ? { mode: 'shell', serial } : {});
  const keep = versionSel.value || api.param('version') || mcfg.serverVersion || '';
  const opts = [];
  let auto = 'Auto (5.0, falls back to 4.x)';
  try {
    const host = await api.get('/mirror/servers');
    const newest = host.builds.find((b) => b.profile === host.defaultProfile) || host.builds[0];
    if (newest) auto = `Auto (${newest.version}${host.fallback ? ', falls back to 4.x' : ''})`;
    for (const b of host.builds) opts.push([b.version, `${b.version} · ${b.source === 'embedded' ? 'embedded' : 'host file'}${b.tested ? '' : ', untested'}`]);
  } catch { /* mirroring may be disabled */ }
  if (serial) {
    try {
      const dev = await api.get(`/devices/${encodeURIComponent(serial)}/mirror/servers`);
      for (const j of dev.jars) {
        if (!opts.some(([v]) => v === j.version)) opts.push([j.version, `${j.version} · on device${j.verified ? '' : ', unverified'}`]);
      }
    } catch { /* device offline */ }
  }
  opts.sort((a, b) => cmpVer(b[0], a[0]));
  versionSel.innerHTML = '';
  versionSel.append(new Option(auto, ''));
  for (const [v, label] of opts) versionSel.append(new Option(label, v, false, v === keep));
  if (keep && !opts.some(([v]) => v === keep)) versionSel.append(new Option(`${keep} · not available`, keep, false, true));
}
deviceSel.onchange = () => loadVersions();

function cmpVer(a, b) {
  const pa = a.split(/[.-]/).map((x) => parseInt(x, 10) || 0);
  const pb = b.split(/[.-]/).map((x) => parseInt(x, 10) || 0);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    if ((pa[i] || 0) !== (pb[i] || 0)) return (pa[i] || 0) - (pb[i] || 0);
  }
  return 0;
}

// ---------------------------------------------------------------------------
// Video

class VideoPipe {
  constructor() {
    this.decoder = null;
    this.config = null; // last config packet bytes
    this.codec = null;
    this.pendingConfig = null; // merged into the next keyframe
    this.needKey = true;
    this.pending = null; // latest decoded frame awaiting draw
    this.raf = 0;
  }

  ensureDecoder() {
    if (this.decoder && this.decoder.state !== 'closed') return;
    this.decoder = new VideoDecoder({
      output: (frame) => this.onFrame(frame),
      error: (e) => {
        addLog('error', 'video decoder: ' + e.message);
        this.needKey = true;
        this.decoder = null;
        this.codec = null; // reconfigure on the next config packet
        if (state.ctl && state.ctl.supports('resetVideo')) sendCtl(state.ctl.resetVideo());
      },
    });
  }

  onConfig(bytes) {
    const codec = avcCodecString(bytes);
    if (!codec) { addLog('warn', 'config packet without SPS'); return; }
    const changed = !this.config || codec !== this.codec || !sameBytes(bytes, this.config);
    this.config = bytes.slice();
    this.pendingConfig = this.config;
    if (!changed && this.decoder && this.decoder.state === 'configured') return;
    this.ensureDecoder();
    // Annex B: no description, so SPS/PPS travel in-band with keyframes.
    this.decoder.configure({ codec, optimizeForLatency: true, hardwareAcceleration: 'no-preference' });
    this.codec = codec;
    this.needKey = true;
    addLog('info', `video: ${codec}`);
  }

  onPacket(pkt) {
    if (!this.decoder || this.decoder.state !== 'configured') return;
    if (this.needKey && !pkt.key) { state.stats.dropped++; return; }
    let data = pkt.data;
    if (pkt.key && this.pendingConfig) {
      const merged = new Uint8Array(this.pendingConfig.length + data.length);
      merged.set(this.pendingConfig, 0);
      merged.set(data, this.pendingConfig.length);
      data = merged;
      this.pendingConfig = null;
    }
    if (pkt.key) this.needKey = false;
    if (this.decoder.decodeQueueSize > 30 && !pkt.key) { state.stats.dropped++; return; }
    try {
      this.decoder.decode(new EncodedVideoChunk({ type: pkt.key ? 'key' : 'delta', timestamp: pkt.pts, data }));
    } catch (e) {
      addLog('error', 'decode: ' + e.message);
      this.needKey = true;
    }
  }

  onFrame(frame) {
    state.stats.decoded++;
    if (this.pending) { this.pending.close(); state.stats.dropped++; }
    this.pending = frame;
    if (!this.raf) this.raf = requestAnimationFrame(() => this.draw());
  }

  draw() {
    this.raf = 0;
    const f = this.pending;
    if (!f) return;
    this.pending = null;
    if (canvas.width !== f.displayWidth || canvas.height !== f.displayHeight) {
      canvas.width = f.displayWidth;
      canvas.height = f.displayHeight;
      layout();
    }
    ctx2d.drawImage(f, 0, 0);
    f.close();
    state.stats.frames++;
    if (!$('placeholder').hidden) placeholder('');
  }

  close() {
    if (this.pending) this.pending.close();
    this.pending = null;
    if (this.decoder && this.decoder.state !== 'closed') this.decoder.close();
    this.decoder = null;
    this.config = null;
    this.codec = null;
  }
}

function sameBytes(a, b) {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

// ---------------------------------------------------------------------------
// Audio (opt-in; Opus via WebCodecs into a scheduled AudioContext queue)

class AudioPipe {
  constructor() {
    this.ctx = new AudioContext({ sampleRate: 48000, latencyHint: 'interactive' });
    this.decoder = null;
    this.next = 0;
  }
  resume() { return this.ctx.resume(); }
  onConfig(bytes) {
    if (typeof AudioDecoder === 'undefined') { addLog('warn', 'AudioDecoder unavailable; audio disabled'); return; }
    this.decoder = new AudioDecoder({
      output: (d) => this.play(d),
      error: (e) => addLog('error', 'audio decoder: ' + e.message),
    });
    this.decoder.configure({ codec: 'opus', sampleRate: 48000, numberOfChannels: 2, description: bytes.slice() });
  }
  onPacket(pkt) {
    if (!this.decoder || this.decoder.state !== 'configured') return;
    this.decoder.decode(new EncodedAudioChunk({ type: 'key', timestamp: pkt.pts, data: pkt.data }));
  }
  play(d) {
    const n = d.numberOfFrames;
    const ch = d.numberOfChannels;
    const buf = this.ctx.createBuffer(ch, n, d.sampleRate);
    for (let c = 0; c < ch; c++) {
      const plane = new Float32Array(n);
      d.copyTo(plane, { planeIndex: c, format: 'f32-planar' });
      buf.copyToChannel(plane, c);
    }
    d.close();
    const now = this.ctx.currentTime;
    if (this.next < now + 0.02 || this.next > now + 0.4) this.next = now + 0.06; // (re)sync
    const src = this.ctx.createBufferSource();
    src.buffer = buf;
    src.connect(this.ctx.destination);
    src.start(this.next);
    this.next += buf.duration;
  }
  close() {
    try { if (this.decoder && this.decoder.state !== 'closed') this.decoder.close(); } catch { /* ignore */ }
    this.ctx.close();
  }
}

// ---------------------------------------------------------------------------
// Session

let video = null;
let audio = null;

function sendCtl(bytes) {
  if (state.ws && state.ws.readyState === 1 && state.meta && state.meta.control) state.ws.send(bytes);
}

function connect(versionOverride) {
  const serial = deviceSel.value;
  if (!serial) { banner(banners, { kind: 'warn', key: 'session', text: 'Choose a device first.' }); return; }
  if (typeof VideoDecoder === 'undefined') {
    banner(banners, { kind: 'err', key: 'webcodecs', bold: 'No WebCodecs.', text: 'This browser has no VideoDecoder; mirroring needs a secure context and a current Chromium, Firefox or Safari.' });
    return;
  }
  disconnect();
  clearBanners(banners, 'session', 'fallback', 'verified', 'tested');
  $('logpanel').innerHTML = '';
  state.logs = [];
  const q = {
    version: versionOverride ?? (versionSel.value || undefined),
    max_size: $('max_size').value || undefined,
    video_bit_rate: $('bitrate').value || undefined,
    max_fps: $('max_fps').value || undefined,
    audio: $('audio').checked ? 'true' : 'false',
    control: $('control').checked ? 'true' : 'false',
    stay_awake: $('stay_awake').checked ? 'true' : undefined,
    clipboard_autosync: $('control').checked ? 'true' : undefined,
  };
  for (const [k, v] of Object.entries(mcfg.options || {})) if (q[k] === undefined) q[k] = v;
  video = new VideoPipe();
  if ($('audio').checked) { audio = new AudioPipe(); audio.resume(); } // user gesture: Connect click
  const ws = new WebSocket(api.wsURL(`/devices/${encodeURIComponent(serial)}/mirror`, q));
  ws.binaryType = 'arraybuffer';
  state.ws = ws;
  state.errors = [];
  setStatus(`Connecting to ${serial}…`, 'warn');
  placeholder('Starting the mirroring server…', serial);
  $('connect').textContent = 'Disconnect';
  $('connect').classList.remove('btn-accent');
  let opened = false;
  ws.onopen = () => { opened = true; };
  ws.onmessage = (ev) => (typeof ev.data === 'string' ? onText(JSON.parse(ev.data)) : onBinary(ev.data));
  ws.onclose = async (ev) => {
    if (state.ws !== ws) return;
    state.ws = null;
    state.connected = false;
    resetConnectButton();
    setStatus('Not connected');
    cleanupMedia();
    if (!opened) {
      // The upgrade was refused: fetch the JSON error with a plain request.
      try {
        await api.fetchRaw(`/devices/${encodeURIComponent(serial)}/mirror`, q);
      } catch (e) {
        banner(banners, { kind: 'err', key: 'session', bold: e.status === 409 ? 'Incompatible server version.' : 'Could not start mirroring.', text: e.message });
        placeholder('Could not start mirroring', e.message);
        return;
      }
    }
    if (!state.errors.length) placeholder(ev.code === 1000 ? 'Disconnected' : `Connection closed (${ev.code})`, ev.reason || 'Press Connect to start again.');
  };
}

function resetConnectButton() {
  $('connect').textContent = 'Connect';
  $('connect').classList.add('btn-accent');
}

function cleanupMedia() {
  if (video) video.close();
  if (audio) audio.close();
  video = audio = null;
  releaseAllKeys();
  $('fps').hidden = true;
}

function disconnect() {
  const ws = state.ws;
  state.ws = null;
  state.meta = null;
  state.ctl = null;
  state.connected = false;
  if (ws) ws.close(1000);
  cleanupMedia();
  resetConnectButton();
  setStatus('Not connected');
}

function onText(m) {
  switch (m.type) {
    case 'meta': {
      state.meta = m;
      state.ctl = encoderFor(m);
      state.frameW = m.width || 0;
      state.frameH = m.height || 0;
      state.connected = true;
      document.title = `${m.serial} · mirroring`;
      setStatus(`Mirroring ${m.serial}`, 'on');
      $('fps').hidden = !showFps;
      updateFooter();
      layout();
      if (m.fallbackFrom) {
        const detail = m.fallbackReason === 'sdk' ? `the device's SDK (${m.sdk}) is below its minimum` : `failed at launch: ${lastServerLine() || m.fallbackDetail || 'see the server log'}`;
        banner(banners, {
          kind: 'warn', key: 'fallback', bold: `Running ${m.version}.`,
          text: `${m.fallbackFrom} ${detail}. htadb will use ${m.version} for this device until it reconnects.`,
          buttons: [
            { label: 'Server log', onClick: () => toggleLog(true) },
            { label: `Retry with ${m.fallbackFrom}`, onClick: () => retryWith(m.serial, m.fallbackFrom) },
          ],
        });
      }
      if (!m.verified) banner(banners, { kind: 'warn', key: 'verified', bold: 'Unverified server.', text: `${m.jar} does not match a known build; its version (${m.version}) is taken from its name.` });
      if (!m.tested) banner(banners, { kind: 'warn', key: 'tested', bold: 'Untested version.', text: `Server ${m.version} is outside the tested allowlist; using the ${m.profile} protocol profile.` });
      for (const w of m.warnings || []) addLog('warn', w);
      if ($('screen_off').checked) sendCtl(state.ctl.setDisplayPower({ on: false }));
      stage.focus();
      break;
    }
    case 'session':
      state.frameW = m.width;
      state.frameH = m.height;
      updateFooter();
      layout();
      break;
    case 'log':
      addLog(m.level, m.line);
      break;
    case 'error':
      addLog('error', m.error);
      if (m.fatal === false) break;
      state.errors.push(m);
      for (const l of m.logs || []) addLog('error', l);
      banner(banners, {
        kind: 'err', key: 'session', bold: 'Mirroring failed.', text: m.error,
        buttons: [{ label: 'Server log', onClick: () => toggleLog(true) }],
      });
      placeholder('Mirroring failed', 'See the server log for details.');
      break;
  }
}

async function retryWith(serial, version) {
  try { await api.del(`/devices/${encodeURIComponent(serial)}/mirror/fallback`); } catch { /* best effort */ }
  clearBanners(banners, 'fallback');
  connect(version);
}

function onBinary(buf) {
  const f = parseFrame(buf);
  state.stats.bytes += buf.byteLength;
  if (f.tag === TAG_VIDEO) {
    if (!video) return;
    if (f.config) video.onConfig(f.data);
    else video.onPacket(f);
  } else if (f.tag === TAG_AUDIO) {
    if (!audio) return;
    if (f.config) audio.onConfig(f.data);
    else audio.onPacket(f);
  } else if (f.tag === TAG_DEVICE) {
    onDeviceMessage(parseDeviceMessage(f.data));
  }
}

function updateFooter() {
  const m = state.meta;
  if (!m) return;
  $('f-profile').textContent = `profile ${m.profile}`;
  $('f-jar').textContent = `${(m.jar || '').split('/').pop().replace(/^\.session-[0-9a-f]+\.jar$/, `scrcpy-server-v${m.version}.jar`) || 'server ' + m.version} · ${m.verified ? 'verified' : 'unverified'}`;
  $('f-video').textContent = `${(m.videoCodec || 'h264').toUpperCase().replace('H264', 'H.264')} · ${state.frameW}×${state.frameH}`;
  $('f-audio').textContent = m.audio ? `audio ${m.audioCodec || 'on'}` : 'audio off';
  $('f-control').textContent = m.control ? 'control on' : 'control off';
}

// ---------------------------------------------------------------------------
// Clipboard

async function writeHostClipboard(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    const ta = $('clip_fallback');
    ta.value = text;
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch { /* ignore */ }
    stage.focus();
    return ok;
  }
}

function onDeviceMessage(m) {
  if (m.type === 'clipboard') {
    $('clip').value = m.text;
    writeHostClipboard(m.text).then((ok) => {
      addLog('info', ok ? `device clipboard copied to this computer (${m.text.length} chars)` : 'device clipboard received; the browser blocked writing it — use "Copy to this computer"');
    });
  }
}

$('clip_copy').onclick = async () => {
  const ok = await writeHostClipboard($('clip').value);
  $('clip_copy').textContent = ok ? 'Copied' : 'Copy failed';
  setTimeout(() => { $('clip_copy').textContent = 'Copy to this computer'; }, 1200);
};
$('clip_send').onclick = () => { setDeviceClipboard($('clip').value, false); stage.focus(); };

function setDeviceClipboard(text, paste) {
  if (!state.ctl) return;
  state.clipSeq += 1n;
  sendCtl(state.ctl.setClipboard({ sequence: state.clipSeq, paste, text }));
}

document.addEventListener('paste', (e) => {
  if (!state.connected || document.activeElement !== stage) return;
  const text = e.clipboardData && e.clipboardData.getData('text/plain');
  if (text) { e.preventDefault(); setDeviceClipboard(text, true); }
});

async function readHostClipboard() {
  try { return await navigator.clipboard.readText(); } catch (e) {
    banner(banners, { kind: 'warn', key: 'clip', text: `Reading this computer's clipboard was refused (${e.message}). Use Ctrl/Cmd+V instead.` });
    return null;
  }
}

function typeText(text) {
  const step = Math.floor(INJECT_TEXT_MAX / 3);
  for (let i = 0; i < text.length; i += step) sendCtl(state.ctl.injectText({ text: text.slice(i, i + step) }));
}

// ---------------------------------------------------------------------------
// Pointer input

function framePos(e) {
  const r = canvas.getBoundingClientRect();
  const w = state.frameW || canvas.width;
  const h = state.frameH || canvas.height;
  const x = Math.round(((e.clientX - r.left) / r.width) * w);
  const y = Math.round(((e.clientY - r.top) / r.height) * h);
  return { x: Math.max(0, Math.min(w - 1, x)), y: Math.max(0, Math.min(h - 1, y)), width: w, height: h };
}

const buttonBits = (b) => (b & 1 ? BUTTON.PRIMARY : 0) | (b & 2 ? BUTTON.SECONDARY : 0) | (b & 4 ? BUTTON.TERTIARY : 0) | (b & 8 ? BUTTON.BACK : 0) | (b & 16 ? BUTTON.FORWARD : 0);
const buttonOf = (n) => [BUTTON.PRIMARY, BUTTON.TERTIARY, BUTTON.SECONDARY, BUTTON.BACK, BUTTON.FORWARD][n] || 0;
const pointerId = (e) => (e.pointerType === 'mouse' ? POINTER_ID_MOUSE : BigInt(e.pointerId));

function touch(e, action) {
  if (!state.ctl || !state.frameW) return;
  const mouse = e.pointerType === 'mouse';
  sendCtl(state.ctl.injectTouch({
    action, pointerId: pointerId(e), ...framePos(e),
    pressure: action === ACTION.UP ? 0 : (e.pressure || 1),
    actionButton: mouse && (action === ACTION.DOWN || action === ACTION.UP) ? buttonOf(e.button) : 0,
    buttons: mouse ? buttonBits(e.buttons) : 0,
  }));
}

function key(keycode, action, meta = 0, repeat = 0) {
  if (state.ctl) sendCtl(state.ctl.injectKeycode({ action, keycode, repeat, metastate: meta }));
}
function tap(keycode, meta = 0) { key(keycode, 0, meta); key(keycode, 1, meta); }

canvas.addEventListener('contextmenu', (e) => e.preventDefault());
canvas.addEventListener('pointerdown', (e) => {
  stage.focus();
  if (!state.connected) return;
  e.preventDefault();
  if (e.pointerType === 'mouse' && e.button === 2) { sendCtl(state.ctl.backOrScreenOn({ action: 0 })); return; } // back / screen on
  if (e.pointerType === 'mouse' && e.button === 1) { tap(KEYCODE.HOME); return; }
  canvas.setPointerCapture(e.pointerId);
  touch(e, ACTION.DOWN);
});
let hoverPending = null;
canvas.addEventListener('pointermove', (e) => {
  if (!state.connected) return;
  if (canvas.hasPointerCapture(e.pointerId)) { touch(e, ACTION.MOVE); return; }
  if (e.pointerType === 'mouse' && !e.buttons) {
    if (!hoverPending) requestAnimationFrame(() => { if (hoverPending) touch(hoverPending, ACTION.HOVER_MOVE); hoverPending = null; });
    hoverPending = e;
  }
});
const up = (e) => {
  if (!state.connected) return;
  if (e.pointerType === 'mouse' && e.button === 2) { sendCtl(state.ctl.backOrScreenOn({ action: 1 })); return; }
  if (!canvas.hasPointerCapture(e.pointerId)) return;
  canvas.releasePointerCapture(e.pointerId);
  touch(e, e.type === 'pointercancel' ? ACTION.CANCEL : ACTION.UP);
};
canvas.addEventListener('pointerup', up);
canvas.addEventListener('pointercancel', up);
canvas.addEventListener('wheel', (e) => {
  if (!state.connected || !state.ctl) return;
  e.preventDefault();
  const unit = e.deltaMode === 1 ? 1 / 3 : e.deltaMode === 2 ? 1 : 1 / 100;
  sendCtl(state.ctl.injectScroll({ ...framePos(e), hscroll: e.deltaX * unit, vscroll: -e.deltaY * unit, buttons: buttonBits(e.buttons) }));
}, { passive: false });

// ---------------------------------------------------------------------------
// Keyboard input and hotkeys

let showFps = true;
const hotkeys = new Hotkeys(config);
const ifCtl = (fn) => () => { if (state.ctl) fn(state.ctl); };
hotkeys
  .on('home', () => tap(KEYCODE.HOME))
  .on('back', () => tap(KEYCODE.BACK))
  .on('back2', () => tap(KEYCODE.BACK))
  .on('appSwitch', () => tap(KEYCODE.APP_SWITCH))
  .on('menu', () => tap(KEYCODE.MENU))
  .on('volumeUp', () => tap(KEYCODE.VOLUME_UP))
  .on('volumeDown', () => tap(KEYCODE.VOLUME_DOWN))
  .on('power', () => tap(KEYCODE.POWER))
  .on('screenOff', ifCtl((c) => sendCtl(c.setDisplayPower({ on: false }))))
  .on('screenOn', ifCtl((c) => sendCtl(c.setDisplayPower({ on: true }))))
  .on('rotate', ifCtl((c) => sendCtl(c.rotateDevice())))
  .on('notifications', ifCtl((c) => sendCtl(c.expandNotificationPanel())))
  .on('settingsPanel', ifCtl((c) => sendCtl(c.expandSettingsPanel())))
  .on('collapsePanels', ifCtl((c) => sendCtl(c.collapsePanels())))
  .on('copy', ifCtl((c) => sendCtl(c.getClipboard({ copyKey: COPY_KEY.COPY }))))
  .on('cut', ifCtl((c) => sendCtl(c.getClipboard({ copyKey: COPY_KEY.CUT }))))
  .on('paste', async () => { const t = await readHostClipboard(); if (t) setDeviceClipboard(t, true); })
  .on('typeClipboard', async () => { const t = await readHostClipboard(); if (t && state.ctl) typeText(t); })
  .on('fullscreen', () => toggleFullscreen())
  .on('resize11', () => { state.native = !state.native; layout(); })
  .on('fps', () => { showFps = !showFps; $('fps').hidden = !showFps || !state.connected; })
  .on('keyboardSettings', ifCtl((c) => sendCtl(c.openHardKeyboardSettings())))
  .on('resetVideo', ifCtl((c) => c.supports('resetVideo') && sendCtl(c.resetVideo())))
  .on('hotkeys', () => toggleHotkeysPanel());

function releaseAllKeys() {
  for (const kc of state.pressedKeys.values()) key(kc, 1);
  state.pressedKeys.clear();
}

// Keys whose e.key is one character but that are better sent as keycodes.
const NONTEXT_CODES = new Set(['Space', 'Enter', 'NumpadEnter', 'Tab']);

stage.addEventListener('keydown', (e) => {
  if (hotkeys.handle(e)) return;
  if (!state.connected || !state.ctl) return;
  if (hotkeys.usesModifier(e)) { e.preventDefault(); return; } // reserved for hotkeys
  // Ctrl/Cmd+V: let the browser fire `paste`, which syncs and pastes.
  if ((e.ctrlKey || e.metaKey) && e.code === 'KeyV') return;
  if (e.isComposing || e.key === 'Dead' || e.key === 'Process') return;
  e.preventDefault();
  const printable = e.key.length === 1 && !e.ctrlKey && !e.metaKey && (!e.altKey || e.getModifierState('AltGraph'));
  if (printable && !NONTEXT_CODES.has(e.code)) {
    sendCtl(state.ctl.injectText({ text: e.key }));
    return;
  }
  const kc = CODE_TO_KEYCODE[e.code];
  if (kc === undefined) return;
  state.repeats = e.repeat ? (state.repeats || 0) + 1 : 0;
  state.pressedKeys.set(e.code, kc);
  key(kc, 0, metaState(e), state.repeats);
});
stage.addEventListener('keyup', (e) => {
  const kc = state.pressedKeys.get(e.code);
  if (kc === undefined) return;
  e.preventDefault();
  state.pressedKeys.delete(e.code);
  key(kc, 1, metaState(e));
});
stage.addEventListener('blur', releaseAllKeys);
stage.addEventListener('compositionend', (e) => { if (e.data && state.ctl) typeText(e.data); });

// ---------------------------------------------------------------------------
// Side panel, toolbar

const NAV = [
  ['back', 'Back', () => tap(KEYCODE.BACK)],
  ['home', 'Home', () => tap(KEYCODE.HOME)],
  ['apps', 'Apps', () => tap(KEYCODE.APP_SWITCH)],
  ['volUp', 'Vol +', () => tap(KEYCODE.VOLUME_UP)],
  ['volDown', 'Vol −', () => tap(KEYCODE.VOLUME_DOWN)],
  ['power', 'Power', () => tap(KEYCODE.POWER)],
  ['rotate', 'Rotate', ifCtl((c) => sendCtl(c.rotateDevice()))],
  ['panels', 'Panels', ifCtl((c) => sendCtl(c.expandNotificationPanel()))],
  ['full', 'Full', () => toggleFullscreen()],
];
for (const [ic, label, fn] of NAV) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'navbtn';
  b.innerHTML = icon(ic) + `<span>${label}</span>`;
  b.onclick = () => { fn(); if (ic !== 'full') stage.focus(); };
  $('navgrid').append(b);
}

$('connect').onclick = () => (state.ws ? disconnect() : connect());
$('screen_off').onchange = () => { if (state.ctl) sendCtl(state.ctl.setDisplayPower({ on: !$('screen_off').checked })); stage.focus(); };

const panel = $('hotkeys_panel');
function toggleHotkeysPanel() {
  if (!panel.hidden) { panel.hidden = true; stage.focus(); return; }
  hotkeys.renderPanel(panel, () => { panel.hidden = true; stage.focus(); });
  panel.hidden = false;
}
panel.addEventListener('mousedown', (e) => { if (e.target === panel) { panel.hidden = true; stage.focus(); } });
$('hotkeys_btn').onclick = toggleHotkeysPanel;
const showHotkeyCombo = () => { $('hotkeys_combo').textContent = hotkeys.bindings.hotkeys ? comboDisplay(hotkeys.bindings.hotkeys, hotkeys.modifier) : ''; };
hotkeys.onChange(showHotkeyCombo);
showHotkeyCombo();

async function toggleFullscreen() {
  if (document.fullscreenElement) { await document.exitFullscreen(); return; }
  await stage.requestFullscreen();
  stage.focus();
  // Chromium: capture OS-reserved combos (Alt+Tab, Esc…) in fullscreen.
  if (navigator.keyboard && navigator.keyboard.lock) navigator.keyboard.lock().catch(() => {});
}
document.addEventListener('fullscreenchange', () => {
  if (!document.fullscreenElement && navigator.keyboard && navigator.keyboard.unlock) navigator.keyboard.unlock();
  layout();
});

// Stats.
setInterval(() => {
  const s = state.stats;
  s.fps = s.frames - s.lastFrames;
  s.kbps = Math.round(((s.bytes - s.lastBytes) * 8) / 1000);
  s.lastFrames = s.frames;
  s.lastBytes = s.bytes;
  $('fps').textContent = `${s.fps} fps`;
  $('f-stats').textContent = state.connected ? `${s.fps} fps · ${s.kbps} kbit/s` : '';
}, 1000);

window.addEventListener('beforeunload', () => disconnect());
placeholder('Device screen', 'H.264 decoded with WebCodecs. Choose a device and press Connect.');

if (api.param('autoconnect') === '1' || (wanted && api.param('autoconnect') !== '0' && secure)) {
  // Give the device list a moment, then connect (no gesture: audio stays off).
  setTimeout(() => { if (!state.ws) { deviceSel.value = wanted || deviceSel.value; connect(); } }, 300);
}
