// htadb VS Code extension: manages an htadb process, shows ADB devices, and
// opens htadb's terminal and mirroring pages in VS Code's integrated
// browser. Only the `vscode` API and Node built-ins are used.
'use strict';

const vscode = require('vscode');
const cp = require('child_process');
const fs = require('fs');
const path = require('path');
const lib = require('./lib');

let output;
let adapter;
let tree;
let statusItem;

// ---------------------------------------------------------------------------
// Adapter (managed htadb process or an existing htadb at htadb.url)

class Adapter {
  constructor(context) {
    this.context = context;
    this.proc = null;
    this.url = null;
    this.state = 'stopped'; // stopped | starting | running | external | error
    this.error = '';
    this.devices = [];
    this.tracker = null;
    this.emitter = new vscode.EventEmitter();
    this.onDidChange = this.emitter.event;
  }

  cfg() { return vscode.workspace.getConfiguration('htadb'); }

  setState(s, err = '') {
    this.state = s;
    this.error = err;
    this.emitter.fire();
  }

  async start() {
    if (this.state === 'running' || this.state === 'starting' || this.state === 'external') return;
    const external = (this.cfg().get('url') || '').trim();
    if (external) {
      this.url = external.replace(/\/+$/, '');
      this.setState('external');
      output.appendLine(`Using existing htadb at ${this.url}`);
      this.track();
      return;
    }
    this.setState('starting');
    try {
      await this.spawn();
      this.track();
    } catch (e) {
      output.appendLine('htadb failed to start: ' + e.message);
      this.setState('error', e.message);
      vscode.window.showErrorMessage(`htadb failed to start: ${e.message}`, 'Show Log').then((b) => { if (b) output.show(); });
    }
  }

  async spawn() {
    const c = this.cfg();
    let bin = (c.get('binaryPath') || '').trim();
    if (!bin) {
      bin = lib.bundledBinary(this.context.extensionPath);
      if (!bin || !fs.existsSync(bin)) throw new Error(`no bundled htadb for ${process.platform}/${process.arch}; set htadb.binaryPath or htadb.url`);
      if (process.platform !== 'win32') { try { fs.chmodSync(bin, 0o755); } catch { /* read-only install */ } }
    }
    const storage = this.context.globalStorageUri.fsPath;
    fs.mkdirSync(storage, { recursive: true });
    const uiConfigPath = path.join(storage, 'ui-config.json');
    const configTomlPath = path.join(storage, 'config.toml');
    fs.writeFileSync(uiConfigPath, JSON.stringify(uiConfig(), null, 2));
    fs.writeFileSync(configTomlPath, lib.devicesToml(c.get('mirror.devices')));
    const args = lib.adapterArgs({
      useTls: c.get('managed.useTls'),
      uiConfigPath, configTomlPath,
      fallback: c.get('mirror.fallback'),
      serverVersion: c.get('mirror.serverVersion'),
      xtermDir: prepareXterm(storage),
      extraArgs: c.get('args') || [],
    });
    output.appendLine(`Starting ${bin} ${args.join(' ')}`);
    const proc = cp.spawn(bin, args, { stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
    this.proc = proc;
    await new Promise((resolve, reject) => {
      let buf = '';
      const timer = setTimeout(() => reject(new Error('timed out waiting for htadb to listen')), 20000);
      proc.stdout.on('data', (d) => {
        buf += d;
        let i;
        while ((i = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, i);
          buf = buf.slice(i + 1);
          const url = lib.parseAnnounce(line);
          if (url && !this.url) {
            this.url = url;
            clearTimeout(timer);
            this.setState('running');
            resolve();
          } else if (line.trim()) {
            output.appendLine(line);
          }
        }
      });
      proc.stderr.on('data', (d) => output.append(String(d)));
      proc.on('error', (e) => { clearTimeout(timer); reject(e); });
      proc.on('exit', (code, sig) => {
        clearTimeout(timer);
        output.appendLine(`htadb exited (${sig || code})`);
        const wasRunning = this.state === 'running';
        this.proc = null;
        this.url = null;
        this.stopTracking();
        this.devices = [];
        this.setState(wasRunning && code ? 'error' : 'stopped', code ? `exited with code ${code}` : '');
        reject(new Error(`htadb exited (${sig || code}) before listening; see the htadb log`));
      });
    });
    output.appendLine(`htadb listening at ${this.url}`);
  }

  async stop() {
    this.stopTracking();
    if (this.proc) {
      const p = this.proc;
      await new Promise((resolve) => {
        const t = setTimeout(() => { try { p.kill('SIGKILL'); } catch { /* gone */ } resolve(); }, 3000);
        p.once('exit', () => { clearTimeout(t); resolve(); });
        try { p.kill(); } catch { resolve(); }
      });
    }
    this.proc = null;
    this.url = null;
    this.devices = [];
    this.setState('stopped');
  }

  async restart() { await this.stop(); await this.start(); }

  /** Track devices via /api/v1/devices/track, or poll if WebSocket is missing. */
  track() {
    this.stopTracking();
    if (!this.url) return;
    let stopped = false;
    let ws = null;
    let poll = null;
    let delay = 500;
    const devices = new Map();
    const emit = () => { this.devices = [...devices.values()].sort((a, b) => a.serial.localeCompare(b.serial)); this.emitter.fire(); };
    const pollOnce = async () => {
      try {
        const r = await fetch(this.url + '/api/v1/devices');
        if (r.ok) { devices.clear(); for (const d of await r.json()) devices.set(d.serial, d); emit(); }
      } catch { /* adapter not reachable yet */ }
    };
    const connect = () => {
      if (stopped) return;
      if (typeof WebSocket !== 'function') { pollOnce(); poll = setInterval(pollOnce, 2000); return; }
      ws = new WebSocket(this.url.replace(/^http/, 'ws') + '/api/v1/devices/track');
      ws.onopen = () => { delay = 500; };
      ws.onmessage = (ev) => {
        const m = JSON.parse(ev.data);
        if (m.type === 'snapshot') { devices.clear(); for (const d of m.devices) devices.set(d.serial, d); }
        else if (m.type === 'remove') devices.delete(m.device.serial);
        else devices.set(m.device.serial, m.device);
        emit();
      };
      ws.onclose = () => { if (!stopped) { setTimeout(connect, delay); delay = Math.min(delay * 2, 8000); } };
      ws.onerror = () => { /* onclose follows */ };
    };
    connect();
    this.tracker = () => { stopped = true; if (ws) ws.close(); if (poll) clearInterval(poll); };
  }

  stopTracking() { if (this.tracker) this.tracker(); this.tracker = null; }
}

// ---------------------------------------------------------------------------
// UI config for htadb's pages (served as /ui/config.js)

function uiConfig() {
  const c = vscode.workspace.getConfiguration('htadb');
  const editor = vscode.workspace.getConfiguration('editor');
  const termCfg = vscode.workspace.getConfiguration('terminal.integrated');
  const kind = vscode.window.activeColorTheme.kind;
  const theme = kind === vscode.ColorThemeKind.Light || kind === vscode.ColorThemeKind.HighContrastLight ? 'light'
    : kind === vscode.ColorThemeKind.HighContrast ? 'high-contrast' : 'dark';
  return {
    theme,
    terminal: {
      fontFamily: termCfg.get('fontFamily') || editor.get('fontFamily') || undefined,
      fontSize: termCfg.get('fontSize') || editor.get('fontSize') || undefined,
    },
    mirror: {
      defaults: c.get('mirror.defaults') || {},
      serverVersion: c.get('mirror.serverVersion') === 'auto' ? '' : c.get('mirror.serverVersion'),
    },
    hotkeys: c.get('mirror.hotkeys') || {},
    host: 'vscode',
  };
}

/** Copy VS Code's own xterm.js (possibly inside node_modules.asar) to storage. */
function prepareXterm(storage) {
  const found = lib.findVSCodeXterm(vscode.env.appRoot);
  if (!found.dir) {
    output.appendLine(`Using htadb's embedded xterm.js (${found.reason}).`);
    return null;
  }
  try {
    const dst = path.join(storage, 'xterm-' + found.version);
    if (!fs.existsSync(path.join(dst, 'xterm.js'))) {
      fs.mkdirSync(dst, { recursive: true });
      fs.writeFileSync(path.join(dst, 'xterm.js'), fs.readFileSync(path.join(found.dir, 'lib', 'xterm.js')));
      fs.writeFileSync(path.join(dst, 'xterm.css'), fs.readFileSync(path.join(found.dir, 'css', 'xterm.css')));
    }
    output.appendLine(`Using VS Code's xterm.js ${found.version}.`);
    return dst;
  } catch (e) {
    output.appendLine(`Using htadb's embedded xterm.js (copying VS Code's failed: ${e.message}).`);
    return null;
  }
}

// ---------------------------------------------------------------------------
// Integrated browser check

const SUPPRESS_KEY = 'htadb.suppressIntegratedBrowserWarning';

async function checkBrowser(interactive) {
  const htadbCfg = vscode.workspace.getConfiguration('htadb');
  if (htadbCfg.get('browser') === 'external') return 'external';
  const inspect = vscode.workspace.getConfiguration('simpleBrowser').inspect('useIntegratedBrowser');
  const commands = await vscode.commands.getCommands(true);
  const d = lib.browserDecision(vscode.version, inspect, commands);
  if (d.mode === 'integrated') return 'open';
  if (htadbCfg.get('suppressIntegratedBrowserWarning')) return 'open';
  if (d.mode === 'classic') {
    if (interactive) {
      const b = await vscode.window.showWarningMessage(
        `The integrated browser is not available (${d.reason}). htadb pages will open in the classic Simple Browser, an iframe in a webview where keyboard capture and some browser APIs are less reliable.`,
        'Open anyway', 'Open in external browser', "Don't ask again");
      if (b === "Don't ask again") await htadbCfg.update('suppressIntegratedBrowserWarning', true, vscode.ConfigurationTarget.Global);
      if (b === 'Open in external browser') return 'external';
      return b ? 'open' : 'cancel';
    }
    return 'open';
  }
  // Setting exists but is off (VS Code 1.109–1.112).
  if (!interactive) return 'open';
  const b = await vscode.window.showWarningMessage(
    'The integrated browser is off (simpleBrowser.useIntegratedBrowser). The classic Simple Browser is less reliable for keyboard capture and some browser APIs.',
    'Enable…', 'Open anyway', "Don't ask again");
  if (b === "Don't ask again") { await htadbCfg.update('suppressIntegratedBrowserWarning', true, vscode.ConfigurationTarget.Global); return 'open'; }
  if (b === 'Enable…') {
    const scope = { global: 'your user settings', workspace: 'this workspace', workspaceFolder: 'this folder' }[d.target];
    const ok = await vscode.window.showWarningMessage(
      `Set simpleBrowser.useIntegratedBrowser to true in ${scope}? It affects every extension that opens the Simple Browser.`,
      { modal: true }, 'Enable');
    if (ok !== 'Enable') return 'cancel';
    const target = { global: vscode.ConfigurationTarget.Global, workspace: vscode.ConfigurationTarget.Workspace, workspaceFolder: vscode.ConfigurationTarget.WorkspaceFolder }[d.target];
    await vscode.workspace.getConfiguration('simpleBrowser').update('useIntegratedBrowser', true, target);
    return 'open';
  }
  return b === 'Open anyway' ? 'open' : 'cancel';
}

async function openPage(page, serial) {
  if (!adapter.url) {
    await adapter.start();
    if (!adapter.url) return;
  }
  const q = new URLSearchParams();
  if (page === 'terminal') { if (serial) { q.set('mode', 'shell'); q.set('serial', serial); } }
  else if (serial) { q.set('serial', serial); q.set('autoconnect', '1'); }
  const url = `${adapter.url}/ui/${page === 'terminal' ? 'terminal' : 'mirror'}.html${q.toString() ? '?' + q : ''}`;
  const how = await checkBrowser(true);
  if (how === 'cancel') return;
  if (how === 'external') { await vscode.env.openExternal(vscode.Uri.parse(url)); return; }
  await vscode.commands.executeCommand('simpleBrowser.show', url);
}

async function pickDevice() {
  const ds = adapter.devices.filter((d) => d.state === 'device');
  if (ds.length === 1) return ds[0].serial;
  if (!ds.length) { vscode.window.showInformationMessage('No ADB devices are attached.'); return undefined; }
  const pick = await vscode.window.showQuickPick(ds.map((d) => ({ label: d.serial, description: lib.deviceDescription(d) })), { placeHolder: 'Choose a device' });
  return pick && pick.label;
}

// ---------------------------------------------------------------------------
// Device view and status bar

class DeviceTree {
  constructor() {
    this.emitter = new vscode.EventEmitter();
    this.onDidChangeTreeData = this.emitter.event;
  }
  refresh() { this.emitter.fire(); }
  getTreeItem(el) { return el; }
  getChildren() {
    const items = [];
    const st = new vscode.TreeItem(statusText(), vscode.TreeItemCollapsibleState.None);
    st.iconPath = new vscode.ThemeIcon('circle-filled', new vscode.ThemeColor(adapter.state === 'running' || adapter.state === 'external' ? 'testing.iconPassed' : adapter.state === 'error' ? 'testing.iconFailed' : 'disabledForeground'));
    st.contextValue = 'status';
    if (adapter.state === 'stopped' || adapter.state === 'error') st.command = { command: 'htadb.startAdapter', title: 'Start Adapter' };
    if (adapter.error) st.tooltip = adapter.error;
    items.push(st);
    for (const d of adapter.devices) {
      const it = new vscode.TreeItem(d.serial, vscode.TreeItemCollapsibleState.None);
      it.description = lib.deviceDescription(d);
      it.iconPath = new vscode.ThemeIcon('device-mobile');
      it.contextValue = d.state === 'device' ? 'device' : 'device-offline';
      it.tooltip = `${d.serial}\n${d.model || ''} ${d.product ? '(' + d.product + ')' : ''}\nstate: ${d.state}`;
      it.serial = d.serial;
      items.push(it);
    }
    return items;
  }
}

function statusText() {
  switch (adapter.state) {
    case 'running': return `htadb running · ${adapter.url}`;
    case 'external': return `htadb at ${adapter.url}`;
    case 'starting': return 'htadb starting…';
    case 'error': return 'htadb failed — click to retry';
    default: return 'htadb stopped — click to start';
  }
}

function updateStatusBar() {
  const n = adapter.devices.filter((d) => d.state === 'device').length;
  const up = adapter.state === 'running' || adapter.state === 'external';
  statusItem.text = up ? `$(circle-filled) htadb running   ${n} device${n === 1 ? '' : 's'}` : adapter.state === 'starting' ? '$(sync~spin) htadb starting' : '$(circle-outline) htadb stopped';
  statusItem.tooltip = up ? `htadb at ${adapter.url}` : adapter.error || 'htadb is not running';
  statusItem.command = up ? 'htadb.openTerminal' : 'htadb.startAdapter';
  statusItem.show();
}

// ---------------------------------------------------------------------------

function activate(context) {
  output = vscode.window.createOutputChannel('htadb');
  adapter = new Adapter(context);
  tree = new DeviceTree();
  statusItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 50);
  adapter.onDidChange(() => { tree.refresh(); updateStatusBar(); });
  context.subscriptions.push(
    output, statusItem,
    vscode.window.registerTreeDataProvider('htadb.devices', tree),
    vscode.commands.registerCommand('htadb.openTerminal', async (item) => openPage('terminal', item && item.serial ? item.serial : await pickDevice())),
    vscode.commands.registerCommand('htadb.mirrorDevice', async (item) => {
      const s = item && item.serial ? item.serial : await pickDevice();
      if (s) openPage('mirror', s);
    }),
    vscode.commands.registerCommand('htadb.startAdapter', () => adapter.start()),
    vscode.commands.registerCommand('htadb.stopAdapter', () => adapter.stop()),
    vscode.commands.registerCommand('htadb.restartAdapter', () => adapter.restart()),
    vscode.commands.registerCommand('htadb.refresh', () => { adapter.track(); tree.refresh(); }),
    vscode.commands.registerCommand('htadb.showLog', () => output.show()),
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration('htadb') && !e.affectsConfiguration('htadb.suppressIntegratedBrowserWarning') && !e.affectsConfiguration('htadb.browser')) {
        if (adapter.state !== 'stopped') adapter.restart();
      }
    }),
    { dispose: () => { adapter.stop(); } },
  );
  updateStatusBar();
  adapter.start();
  // Check the integrated browser once at activation (non-blocking; the
  // warning offers Enable…, Open anyway and Don't ask again).
  checkBrowser(true).catch(() => {});
}

function deactivate() {
  return adapter ? adapter.stop() : undefined;
}

module.exports = { activate, deactivate };
