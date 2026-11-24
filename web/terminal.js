// htadb terminal: an `adb [serial]>` prompt in xterm.js.
//
// Built-in commands use the REST API (devices, use, push, pull, install,
// screencap, ls); `shell` opens an interactive PTY over WebSocket;
// everything else runs through the streamed CLI passthrough
// (`adb [-s serial] <args>`), with Ctrl+C sent as SIGINT.

import config from './config.js';
import * as api from './lib/api.js';
import { autoFit } from './lib/fit.js';
import { checkSecureContext } from './lib/secure-context.js';
import { deviceMeta, fmtSize, pageLink } from './lib/ui.js';

const $ = (id) => document.getElementById(id);
checkSecureContext($('banners'), 'the async Clipboard API');
$('home').href = pageLink('index.html');
$('nav-term').href = pageLink('terminal.html');

const C = {
  reset: '\x1b[0m', bold: '\x1b[1m', dim: '\x1b[2m', red: '\x1b[31m', green: '\x1b[32m',
  yellow: '\x1b[33m', blue: '\x1b[34m', cyan: '\x1b[36m', gray: '\x1b[90m',
};

// Colors from the UI design; config.js (e.g. from the VS Code extension) may
// override font and theme.
const THEME = {
  background: '#0A1016', foreground: '#C3CFDA', cursor: '#E6EDF3', cursorAccent: '#0A1016',
  selectionBackground: '#33475A', black: '#0A1016', brightBlack: '#93A4B4',
  red: '#FF8A80', brightRed: '#FFB3AD', green: '#5FD3A4', brightGreen: '#8BE3BF',
  yellow: '#F2B44B', brightYellow: '#F6DFAE', blue: '#7FB4FF', brightBlue: '#A9CCFF',
  magenta: '#C7A3FF', brightMagenta: '#DCC4FF', cyan: '#5FC8D3', brightCyan: '#8BDDE3',
  white: '#C3CFDA', brightWhite: '#E6EDF3',
};

function terminalOptions() {
  const t = config.terminal || {};
  const theme = { ...THEME, ...(t.theme || {}) };
  return {
    fontFamily: t.fontFamily || '"JetBrains Mono", ui-monospace, "SF Mono", Menlo, Consolas, "DejaVu Sans Mono", monospace',
    fontSize: t.fontSize || 14,
    lineHeight: t.lineHeight || 1.25,
    cursorBlink: true,
    scrollback: t.scrollback || 5000,
    allowProposedApi: true,
    theme,
  };
}

if (typeof window.Terminal !== 'function') {
  $('term').innerHTML =
    '<p class="hint" style="color:#FF8A80">xterm.js did not load (vendor/xterm/xterm.js). Run <code>make fetchdeps</code>, or check --ui-override.</p>';
  throw new Error('xterm.js missing');
}

let serial = api.param('serial') || '';
let mode = 'prompt'; // prompt | busy | shell
let busy = null; // {interrupt(), kill()} for running commands
let cliEnabled = true;

const term = new window.Terminal(terminalOptions());
term.open(document.getElementById('term'));
let shellResize = null;
autoFit(term, (cols, rows) => {
  if (shellResize) shellResize(rows, cols);
  updateTitle();
});
term.focus();

const write = (s) => term.write(s);
const writeln = (s = '') => term.write(s + '\r\n');
const err = (s) => writeln(C.red + s + C.reset);

// ---------------------------------------------------------------------------
// State


function prompt() {
  return `${C.green}adb${serial ? ` [${serial}]` : ''}>${C.reset} `;
}

function updateTitle() {
  $('term-title').textContent = `adb${serial ? ` [${serial}]` : ''} · xterm-256color · ${term.cols}×${term.rows}`;
  document.title = serial ? `adb ${serial}` : 'adb terminal';
  $('nav-mirror').href = pageLink('mirror.html', { serial });
}

// ---------------------------------------------------------------------------
// Line editor

const HISTORY_KEY = 'htadb.terminal.history';
function loadHistory() {
  try { return JSON.parse(localStorage.getItem(HISTORY_KEY) || '[]'); } catch { return []; }
}
function saveHistory(h) {
  try { localStorage.setItem(HISTORY_KEY, JSON.stringify(h.slice(-500))); } catch { /* unavailable */ }
}

const editor = {
  line: '',
  cursor: 0,
  history: loadHistory(),
  hpos: -1,
  saved: '',
  lastTab: 0,

  start() {
    this.line = '';
    this.cursor = 0;
    this.hpos = -1;
    write(prompt());
  },
  redraw() {
    write('\r\x1b[K' + prompt() + this.line);
    const back = this.line.length - this.cursor;
    if (back > 0) write(`\x1b[${back}D`);
  },
  insert(s) {
    this.line = this.line.slice(0, this.cursor) + s + this.line.slice(this.cursor);
    this.cursor += s.length;
    if (this.cursor === this.line.length) write(s);
    else this.redraw();
  },
  setLine(s) { this.line = s; this.cursor = s.length; this.redraw(); },
  historyMove(d) {
    if (!this.history.length) return;
    if (this.hpos === -1) { this.saved = this.line; this.hpos = this.history.length; }
    this.hpos = Math.max(0, Math.min(this.history.length, this.hpos + d));
    this.setLine(this.hpos === this.history.length ? this.saved : this.history[this.hpos]);
  },
  submit() {
    const line = this.line;
    write('\r\n');
    if (line.trim() && this.history[this.history.length - 1] !== line) {
      this.history.push(line);
      saveHistory(this.history);
    }
    this.hpos = -1;
    run(line).catch((e) => err(String(e && e.message || e))).finally(() => {
      if (mode === 'prompt') this.start();
    });
  },
  wordLeft() {
    let i = this.cursor;
    while (i > 0 && this.line[i - 1] === ' ') i--;
    while (i > 0 && this.line[i - 1] !== ' ') i--;
    return i;
  },
  handle(data) {
    // xterm delivers keys and pastes; split escape sequences from text.
    const re = /\x1b\[[0-9;]*[A-Za-z~]|\x1bO[A-Za-z]|\x1b.|[\s\S]/g;
    let m;
    let text = '';
    const flush = () => { if (text) { this.insert(text); text = ''; } };
    while ((m = re.exec(data))) {
      const k = m[0];
      if (k.length === 1 && k >= ' ' && k !== '\x7f') { text += k; continue; }
      flush();
      switch (k) {
        case '\r': case '\n': this.submit(); return;
        case '\x7f': case '\b':
          if (this.cursor > 0) { this.line = this.line.slice(0, this.cursor - 1) + this.line.slice(this.cursor); this.cursor--; this.redraw(); }
          break;
        case '\x1b[3~':
          if (this.cursor < this.line.length) { this.line = this.line.slice(0, this.cursor) + this.line.slice(this.cursor + 1); this.redraw(); }
          break;
        case '\x1b[D': if (this.cursor > 0) { this.cursor--; write(k); } break;
        case '\x1b[C': if (this.cursor < this.line.length) { this.cursor++; write(k); } break;
        case '\x1b[A': this.historyMove(-1); break;
        case '\x1b[B': this.historyMove(1); break;
        case '\x1b[H': case '\x1bOH': case '\x01': this.cursor = 0; this.redraw(); break;
        case '\x1b[F': case '\x1bOF': case '\x05': this.cursor = this.line.length; this.redraw(); break;
        case '\x1b[1;5D': case '\x1bb': this.cursor = this.wordLeft(); this.redraw(); break;
        case '\x1b[1;5C': case '\x1bf': {
          let i = this.cursor;
          while (i < this.line.length && this.line[i] === ' ') i++;
          while (i < this.line.length && this.line[i] !== ' ') i++;
          this.cursor = i; this.redraw(); break;
        }
        case '\x03': write(C.yellow + '^C' + C.reset + '\r\n'); this.start(); break;
        case '\x04': if (!this.line) { write('\r\n'); writeln(C.gray + '(Ctrl+D: use `exit` in a shell; the prompt stays open)' + C.reset); this.start(); } break;
        case '\x0c': term.clear(); this.redraw(); break;
        case '\x15': this.line = this.line.slice(this.cursor); this.cursor = 0; this.redraw(); break;
        case '\x0b': this.line = this.line.slice(0, this.cursor); this.redraw(); break;
        case '\x17': { const i = this.wordLeft(); this.line = this.line.slice(0, i) + this.line.slice(this.cursor); this.cursor = i; this.redraw(); break; }
        case '\t': complete(); break;
        default: break; // ignore other control sequences
      }
    }
    flush();
  },
};

// ---------------------------------------------------------------------------
// Tokenizing

function tokenize(s) {
  const out = [];
  let cur = '';
  let inTok = false;
  let q = null;
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    if (q) {
      if (ch === q) q = null;
      else if (ch === '\\' && q === '"' && i + 1 < s.length) cur += s[++i];
      else cur += ch;
    } else if (ch === '"' || ch === "'") { q = ch; inTok = true; }
    else if (ch === '\\' && i + 1 < s.length) { cur += s[++i]; inTok = true; }
    else if (ch === ' ' || ch === '\t') { if (inTok) { out.push(cur); cur = ''; inTok = false; } }
    else { cur += ch; inTok = true; }
  }
  if (inTok) out.push(cur);
  return out;
}

const shq = (s) => (/^[\w@%+=:,./-]+$/.test(s) ? s : "'" + s.replaceAll("'", "'\\''") + "'");

// ---------------------------------------------------------------------------
// Commands

const HELP = [
  ['help', 'this help'],
  ['clear', 'clear the screen'],
  ['devices [-l]', 'list devices'],
  ['use <serial>', 'select the device for later commands (or pass -s <serial>)'],
  ['shell [cmd]', 'interactive shell (PTY); `exit` returns to the prompt'],
  ['ls [path]', 'list a device directory'],
  ['push <remote>', 'pick a local file and upload it to <remote> (file or directory)'],
  ['pull <remote>', 'download a device file'],
  ['install [args]', 'pick an APK and install it (args e.g. -r -g)'],
  ['uninstall [-k] <pkg>', 'uninstall a package'],
  ['screencap', 'download a PNG screenshot'],
  ['mirror', 'open the mirroring page for the device'],
  ['<anything else>', 'run `adb [-s serial] ...` on the host (logcat, reboot, forward, reverse, connect, bugreport, …); Ctrl+C interrupts'],
];

const BUILTINS = ['help', 'clear', 'devices', 'use', 'shell', 'ls', 'push', 'pull', 'install', 'uninstall', 'screencap', 'mirror', 'exit'];
const ADB_CMDS = ['logcat', 'reboot', 'forward', 'reverse', 'connect', 'disconnect', 'bugreport', 'root', 'unroot', 'remount',
  'get-state', 'get-serialno', 'get-devpath', 'wait-for-device', 'version', 'sideload', 'emu', 'jdwp', 'pair', 'mdns', 'features', 'host-features'];

function needSerial(s) {
  if (s) return s;
  throw new Error('no device selected: `use <serial>` or pass -s <serial>');
}

async function run(line) {
  let args = tokenize(line);
  if (!args.length) return;
  let s = serial;
  if (args[0] === '-s' || args[0] === '-t') {
    if (args.length < 2) throw new Error('-s needs a serial');
    if (args[0] === '-t') throw new Error('-t (transport id) is not supported; use -s <serial>');
    s = args[1];
    args = args.slice(2);
    if (!args.length) return;
  }
  const [cmd, ...rest] = args;
  switch (cmd) {
    case 'help':
      for (const [c, d] of HELP) writeln(`  ${C.green}${c.padEnd(22)}${C.reset} ${C.gray}${d}${C.reset}`);
      writeln(`\r\n  ${C.gray}Keys: ↑/↓ history, Tab completion, Ctrl+A/E/U/K/W/L, Ctrl+C cancel.${C.reset}`);
      return;
    case 'clear': term.clear(); return;
    case 'exit': writeln(C.gray + 'Close the tab to leave; `exit` ends shells only.' + C.reset); return;
    case 'devices': return cmdDevices(rest.includes('-l'));
    case 'use': return cmdUse(rest[0]);
    case 'shell': return cmdShell(needSerial(s), rest);
    case 'ls': return cmdLs(needSerial(s), rest[0] || '/sdcard');
    case 'push': return cmdPush(needSerial(s), rest);
    case 'pull': return cmdPull(needSerial(s), rest);
    case 'install': return cmdInstall(needSerial(s), rest);
    case 'uninstall': {
      const keep = rest.includes('-k');
      const pkg = rest.filter((x) => x !== '-k')[0];
      if (!pkg) throw new Error('usage: uninstall [-k] <package>');
      const r = await api.post(`/devices/${enc(needSerial(s))}/uninstall`, { package: pkg, keepData: keep });
      writeln(r.output);
      return;
    }
    case 'screencap': case 'screenshot': return cmdScreencap(needSerial(s));
    case 'mirror': {
      const q = new URLSearchParams(location.search);
      q.set('serial', needSerial(s));
      q.delete('mode');
      window.open('mirror.html?' + q.toString(), '_blank');
      return;
    }
    default:
      return cmdCLI(s, args);
  }
}

const enc = encodeURIComponent;

async function cmdDevices(long) {
  const ds = await api.get('/devices');
  writeln('List of devices attached');
  for (const d of ds) {
    let l = `${d.serial.padEnd(22)} ${d.state}`;
    if (long) {
      for (const [k, v] of [['usb', d.usb], ['product', d.product], ['model', d.model], ['device', d.device], ['transport_id', d.transportId]]) {
        if (v) l += ` ${k}:${v}`;
      }
    }
    writeln((d.serial === serial ? C.cyan : '') + l + C.reset);
  }
  writeln();
}

async function cmdUse(s) {
  if (!s) {
    writeln(serial ? `using ${serial}` : 'no device selected');
    return;
  }
  const ds = await api.get('/devices');
  if (!ds.some((d) => d.serial === s)) writeln(C.yellow + `warning: ${s} is not currently attached` + C.reset);
  setSerial(s);
  writeln(C.gray + `Using ${s} for the following commands.` + C.reset);
}

function setSerial(s) {
  serial = s;
  const q = new URLSearchParams(location.search);
  if (s) q.set('serial', s); else q.delete('serial');
  q.delete('mode');
  history.replaceState(null, '', '?' + q.toString());
  if ($('device').value !== s) renderDevices();
  updateTitle();
}


async function cmdLs(s, p) {
  const r = await api.get(`/devices/${enc(s)}/files`, { path: p });
  const ents = r.entries || [r.stat];
  for (const e of ents) {
    const name = e.type === 'dir' ? C.blue + C.bold + e.name + '/' + C.reset : e.type === 'symlink' ? C.cyan + e.name + C.reset : e.name;
    const t = e.mtime ? new Date(e.mtime * 1000).toISOString().slice(0, 16).replace('T', ' ') : ''.padEnd(16);
    writeln(`${C.gray}${e.perm}${C.reset} ${String(e.size).padStart(10)} ${C.gray}${t}${C.reset} ${name}`);
  }
}

// Transfers card: one transfer at a time, mirrored in the sidebar.
function progress(label, verb = 'Uploading') {
  $('tx-empty').hidden = true;
  $('transfer').hidden = false;
  $('tx-name').textContent = label;
  const bar = $('tx-bar');
  bar.classList.remove('err');
  let last = 0;
  const fn = (done, total) => {
    const now = performance.now();
    if (now - last < 80 && done !== total) return;
    last = now;
    const pct = total ? Math.floor((done / total) * 100) : 0;
    bar.firstElementChild.style.width = (total ? pct : 100) + '%';
    bar.setAttribute('aria-valuenow', String(pct));
    $('tx-text').textContent = `${verb}… ${fmtSize(done)}${total ? ' of ' + fmtSize(total) : ''}`;
  };
  fn.done = (text) => { bar.firstElementChild.style.width = '100%'; $('tx-text').textContent = text; };
  fn.fail = (text) => { bar.classList.add('err'); $('tx-text').textContent = text; };
  return fn;
}

async function tracked(p, fn) {
  try { return await fn(); } catch (e) { p.fail('Failed: ' + e.message); throw e; }
}

async function cmdPush(s, rest) {
  const remote = rest[0];
  if (!remote) throw new Error('usage: push <remote path or directory>');
  writeln(C.gray + 'Choose a file…' + C.reset);
  const file = await api.pickFile();
  if (!file) { writeln('cancelled'); return; }
  let target = remote;
  if (remote.endsWith('/')) target = remote + file.name;
  else {
    try {
      const st = await api.get(`/devices/${enc(s)}/files`, { path: remote });
      if (st.stat && st.stat.type === 'dir') target = remote.replace(/\/+$/, '') + '/' + file.name;
    } catch { /* does not exist: it is the target file */ }
  }
  const p = progress(`${file.name} → ${target.slice(0, target.lastIndexOf('/') + 1)}`);
  const r = await tracked(p, () => api.upload('PUT', `/devices/${enc(s)}/files/content`, { path: target }, file, p));
  p.done(`Uploaded ${fmtSize(r.bytes)}`);
  writeln(`${file.name}: ${fmtSize(r.bytes)} pushed to ${r.path}`);
}

async function cmdPull(s, rest) {
  const remote = rest[0];
  if (!remote) throw new Error('usage: pull <remote file>');
  const ctrl = new AbortController();
  busy = { interrupt: () => ctrl.abort(), kill: () => ctrl.abort() };
  const name = remote.split('/').pop() || 'download';
  const p = progress(`${remote} → this computer`, 'Downloading');
  try {
    const resp = await tracked(p, () => api.fetchRaw(`/devices/${enc(s)}/files/content`, { path: remote }, { signal: ctrl.signal }));
    const total = Number(resp.headers.get('Content-Length')) || 0;
    const reader = resp.body.getReader();
    const chunks = [];
    let done = 0;
    for (;;) {
      const { value, done: end } = await reader.read();
      if (end) break;
      chunks.push(value);
      done += value.length;
      p(done, total);
    }
    api.saveBlob(new Blob(chunks), name);
    p.done(`Downloaded ${fmtSize(done)}`);
    writeln(`${name}: ${fmtSize(done)} downloaded`);
  } finally {
    busy = null;
  }
}

async function cmdInstall(s, rest) {
  write(C.gray + 'Choose an APK… ' + C.reset);
  const file = await api.pickFile('.apk,application/vnd.android.package-archive');
  if (!file) { writeln('cancelled'); return; }
  writeln(C.gray + file.name + C.reset);
  writeln('Performing Streamed Install');
  const p = progress(`${file.name} → install`);
  const r = await tracked(p, () => api.upload('POST', `/devices/${enc(s)}/install`, { args: rest.join(' ') }, file, p,
    'application/vnd.android.package-archive'));
  p.done('Installed');
  writeln(r.output || 'Success');
}

async function cmdScreencap(s) {
  const resp = await api.fetchRaw(`/devices/${enc(s)}/screenshot`);
  const cd = resp.headers.get('Content-Disposition') || '';
  const name = (cd.match(/filename="([^"]+)"/) || [])[1] || 'screenshot.png';
  const blob = await resp.blob();
  api.saveBlob(blob, name);
  writeln(`${name} (${fmtSize(blob.size)})`);
}

// Interactive shell over /devices/{s}/shell.
function cmdShell(s, rest) {
  return new Promise((resolve) => {
    const query = { pty: 1, term: 'xterm-256color', rows: term.rows, cols: term.cols };
    if (rest.length) query.cmd = rest.map(shq).join(' ');
    const ws = new WebSocket(api.wsURL(`/devices/${enc(s)}/shell`, query));
    ws.binaryType = 'arraybuffer';
    const te = new TextEncoder();
    let opened = false;
    mode = 'shell';
    term.options.convertEol = false;
    const sub = term.onData((d) => { if (ws.readyState === 1) ws.send(te.encode(d)); });
    const sub2 = term.onBinary((d) => {
      if (ws.readyState === 1) ws.send(Uint8Array.from(d, (c) => c.charCodeAt(0)));
    });
    shellResize = (rows, cols) => { if (ws.readyState === 1) ws.send(JSON.stringify({ type: 'resize', rows, cols })); };
    let exitCode = null;
    ws.onopen = () => { opened = true; };
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        const m = JSON.parse(ev.data);
        if (m.type === 'exit') exitCode = m.code;
        else if (m.type === 'stderr') write(C.red + m.data + C.reset);
        return;
      }
      term.write(new Uint8Array(ev.data));
    };
    ws.onclose = async (ev) => {
      sub.dispose();
      sub2.dispose();
      shellResize = null;
      mode = 'prompt';
      term.options.convertEol = true;
      if (!opened) {
        // The upgrade failed: fetch the JSON error for a useful message.
        try { await api.get(`/devices/${enc(s)}`); err('shell: connection failed'); } catch (e) { err('shell: ' + e.message); }
      } else if (exitCode !== null && exitCode !== 0) {
        writeln(C.gray + `[exit ${exitCode}]` + C.reset);
      } else if (exitCode === null && ev.code !== 1000) {
        writeln(C.yellow + '[connection closed]' + C.reset);
      }
      resolve();
    };
  });
}

const STREAMING = new Set(['logcat', 'bugreport', 'jdwp', 'track-devices', 'track-jdwp', 'wait-for-device', 'wait-for-recovery', 'wait-for-bootloader']);

const REST_FALLBACK = {
  async connect(s, a) { const r = await api.post('/connect', { address: a[1] }); writeln(r.output); },
  async disconnect(s, a) { const r = await api.post('/disconnect', { address: a[1] || '' }); writeln(r.output); },
  async reboot(s, a) { await api.post(`/devices/${enc(needSerial(s))}/reboot`, { target: a[1] || '' }); },
  async root(s) { writeln((await api.post(`/devices/${enc(needSerial(s))}/root`)).output); },
  async unroot(s) { writeln((await api.post(`/devices/${enc(needSerial(s))}/unroot`)).output); },
  async remount(s) { writeln((await api.post(`/devices/${enc(needSerial(s))}/remount`)).output); },
};

// Streamed adb CLI passthrough.
function cmdCLI(s, args) {
  if (!cliEnabled) {
    const f = REST_FALLBACK[args[0]];
    if (f) return f(s, args);
    return Promise.reject(new Error('CLI passthrough is disabled on this htadb; only built-in commands are available (try `help`)'));
  }
  if (STREAMING.has(args[0])) writeln(C.gray + 'Streaming through adb passthrough. Ctrl+C to stop.' + C.reset);
  return new Promise((resolve) => {
    const ws = new WebSocket(api.wsURL('/cli', { arg: args, serial: s }));
    ws.binaryType = 'arraybuffer';
    mode = 'busy';
    term.options.convertEol = true;
    let sigints = 0;
    let exitCode = null;
    let opened = false;
    busy = {
      interrupt() {
        sigints++;
        write(C.yellow + '^C' + C.reset);
        if (sigints > 1 || ws.readyState !== 1) ws.close();
        else ws.send(JSON.stringify({ type: 'signal', name: 'SIGINT' }));
      },
    };
    ws.onopen = () => { opened = true; };
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        const m = JSON.parse(ev.data);
        if (m.type === 'stderr') write(C.yellow + m.data + C.reset);
        else if (m.type === 'exit') exitCode = m.code;
        else if (m.type === 'error') err(m.error);
        return;
      }
      term.write(new Uint8Array(ev.data));
    };
    ws.onclose = async () => {
      busy = null;
      mode = 'prompt';
      if (!opened) {
        try { await api.post('/cli', { args: ['version'] }); err('adb: connection failed'); } catch (e) {
          if (e.code === 'cli_disabled') {
            cliEnabled = false;
            const f = REST_FALLBACK[args[0]];
            if (f) { try { await f(s, args); } catch (e2) { err(e2.message); } resolve(); return; }
          }
          err(e.message);
        }
      } else if (exitCode) {
        writeln(C.gray + `[exit ${exitCode}]` + C.reset);
      }
      resolve();
    };
  });
}

// ---------------------------------------------------------------------------
// Completion

let deviceCache = { at: 0, list: [] };
async function deviceSerials() {
  if (performance.now() - deviceCache.at > 3000) {
    try { deviceCache = { at: performance.now(), list: (await api.get('/devices')).map((d) => d.serial) }; } catch { /* keep */ }
  }
  return deviceCache.list;
}

function commonPrefix(xs) {
  if (!xs.length) return '';
  let p = xs[0];
  for (const x of xs) while (!x.startsWith(p)) p = p.slice(0, -1);
  return p;
}

async function complete() {
  const before = editor.line.slice(0, editor.cursor);
  const toks = before.split(/\s+/);
  const word = toks[toks.length - 1];
  const idx = toks.length - 1;
  const first = toks[0] === '-s' ? toks[2] : toks[0];
  const argIdx = toks[0] === '-s' ? idx - 2 : idx;
  let candidates = [];
  let suffixFor = () => ' ';
  if (idx === 1 && toks[0] === '-s' || (first === 'use' && argIdx === 1)) {
    candidates = (await deviceSerials()).filter((x) => x.startsWith(word));
  } else if (argIdx === 0) {
    candidates = [...BUILTINS, ...ADB_CMDS].filter((x) => x.startsWith(word));
  } else if (['ls', 'pull', 'push', 'cd'].includes(first) && (serial || toks[0] === '-s')) {
    const s = toks[0] === '-s' ? toks[1] : serial;
    const dir = word.includes('/') ? word.slice(0, word.lastIndexOf('/') + 1) : '/';
    const base = word.slice(dir.length);
    try {
      const r = await api.get(`/devices/${enc(s)}/files`, { path: dir });
      const ents = (r.entries || []).filter((e) => e.name.startsWith(base));
      const types = Object.fromEntries(ents.map((e) => [dir + e.name, e.type]));
      candidates = ents.map((e) => dir + e.name);
      suffixFor = (c) => (types[c] === 'dir' ? '/' : ' ');
    } catch { /* no completion */ }
  }
  if (!candidates.length) return;
  if (candidates.length === 1) {
    editor.insert(candidates[0].slice(word.length) + suffixFor(candidates[0]));
    return;
  }
  const p = commonPrefix(candidates);
  if (p.length > word.length) { editor.insert(p.slice(word.length)); return; }
  const now = performance.now();
  if (now - editor.lastTab < 600) {
    write('\r\n' + candidates.map((c) => c.slice(c.lastIndexOf('/', c.length - 2) + 1)).join('  ') + '\r\n');
    editor.redraw();
  }
  editor.lastTab = now;
}

// ---------------------------------------------------------------------------
// Input routing and startup

term.onData((d) => {
  if (mode === 'prompt') editor.handle(d);
  else if (mode === 'busy' && d.includes('\x03') && busy) busy.interrupt();
});

term.options.convertEol = true;
updateTitle();

// ---------------------------------------------------------------------------
// Page chrome: device picker, device list, quick actions, footer

let devices = [];
function renderDevices() {
  const sel = $('device');
  sel.innerHTML = '';
  sel.append(new Option('no device', ''));
  for (const d of devices) sel.append(new Option(`${d.serial}${d.model ? ' · ' + d.model.replaceAll('_', ' ') : ''}`, d.serial));
  if (serial && !devices.some((d) => d.serial === serial)) sel.append(new Option(`${serial} · not attached`, serial));
  sel.value = serial;
  const list = $('devices');
  list.innerHTML = '';
  if (!devices.length) list.innerHTML = '<p class="empty">No devices attached.</p>';
  const ordered = [...devices].sort((a, b) => (b.serial === serial) - (a.serial === serial));
  for (const d of ordered) {
    const row = document.createElement('div');
    row.className = 'devrow' + (d.serial === serial ? ' current' : '');
    row.innerHTML = '<div class="info"><p class="serial"></p><p class="meta"></p></div>';
    row.querySelector('.serial').textContent = d.serial;
    row.querySelector('.meta').textContent = deviceMeta(d);
    if (d.serial === serial) {
      const a = document.createElement('a');
      a.className = 'btn-accent';
      a.textContent = 'Mirror';
      a.href = pageLink('mirror.html', { serial: d.serial });
      row.append(a);
    } else {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'btn';
      b.textContent = 'Use';
      b.onclick = () => typeCommand(`use ${d.serial}`);
      row.append(b);
    }
    list.append(row);
  }
}

function adbStatus(text, cls) {
  $('adb-status').textContent = text;
  $('adb-dot').className = 'dot ' + (cls || '');
}

let adbPort = '5037';
api.trackDevices((ds) => {
  devices = ds;
  adbStatus(`adb server :${adbPort} connected`, 'on');
  renderDevices();
}, (st) => {
  if (st === 'adb-unavailable') adbStatus('adb server unavailable', 'err');
  else if (st === 'disconnected') adbStatus('htadb disconnected', 'warn');
});

$('device').onchange = () => typeCommand(`use ${$('device').value}`.trim());

/** Run a command as if typed at the prompt (only when idle). */
function typeCommand(line) {
  if (mode !== 'prompt') { term.focus(); return; }
  if (line === 'use') { setSerial(''); editor.redraw(); term.focus(); return; }
  editor.setLine(line);
  editor.submit();
  term.focus();
}

$('clear').onclick = () => { term.clear(); term.focus(); };
$('copy').onclick = async () => {
  let text = term.getSelection();
  if (!text) {
    const b = term.buffer.active;
    const lines = [];
    for (let i = 0; i < b.length; i++) lines.push(b.getLine(i).translateToString(true));
    text = lines.join('\n').replace(/\n+$/, '\n');
  }
  try { await navigator.clipboard.writeText(text); $('copy').textContent = 'Copied'; } catch { $('copy').textContent = 'Copy failed'; }
  setTimeout(() => { $('copy').textContent = 'Copy'; }, 1200);
  term.focus();
};
$('qa-push').onclick = () => typeCommand('push /sdcard/Download/');
$('qa-pull').onclick = () => {
  if (mode !== 'prompt') return;
  editor.setLine('pull /sdcard/');
  term.focus();
};
$('qa-install').onclick = () => typeCommand('install -r');
$('qa-screencap').onclick = () => typeCommand('screencap');

$('f-api').textContent = api.apiBase;

(async () => {
  try {
    const v = await api.get('/version');
    cliEnabled = v.cli !== false;
    $('f-version').textContent = `htadb ${v.version}`;
    $('f-adb').textContent = `adb server ${v.adbServerVersion ?? 'unavailable'}`;
    writeln(C.gray + 'htadb terminal. Type help for commands; anything else runs as adb.' + (cliEnabled ? '' : ' (adb passthrough is disabled.)') + C.reset);
  } catch (e) {
    err('htadb unreachable at ' + api.apiBase + ': ' + e.message);
  }
  if (api.param('mode') === 'shell' && serial) {
    await cmdShell(serial, api.param('cmd') ? tokenize(api.param('cmd')) : []);
  }
  if (mode === 'prompt') editor.start();
})();

window.htadbTerminal = { term, run: (l) => run(l) }; // for tests and debugging
