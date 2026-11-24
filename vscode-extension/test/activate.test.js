'use strict';
// Activates the real extension.js against a stubbed `vscode` module, with the
// real htadb binary (bin/htadb) and fakeadb (bin/fakeadb). Skipped when the
// binaries have not been built (make htadb && go build -o bin/fakeadb ...).
const { test } = require('node:test');
const assert = require('node:assert/strict');
const Module = require('node:module');
const path = require('node:path');
const fs = require('node:fs');
const os = require('node:os');
const cp = require('node:child_process');

const root = path.resolve(__dirname, '..', '..');
const htadb = path.join(root, 'bin', 'htadb');
const fakeadb = path.join(root, 'bin', 'fakeadb');
const have = fs.existsSync(htadb) && fs.existsSync(fakeadb);

function stubVSCode(settings, calls) {
  class EventEmitter { constructor() { this.l = []; this.event = (f) => { this.l.push(f); return { dispose() {} }; }; } fire(v) { for (const f of this.l) f(v); } }
  const cfg = (section) => ({
    get: (k) => settings[`${section}.${k}`],
    inspect: (k) => (section === 'simpleBrowser' && k === 'useIntegratedBrowser' ? undefined : undefined),
    update: async (k, v) => { calls.push(['update', `${section}.${k}`, v]); },
  });
  const registered = {};
  return {
    EventEmitter,
    version: '1.147.0',
    env: { appRoot: '/nonexistent', openExternal: async (u) => calls.push(['openExternal', String(u)]) },
    Uri: { parse: (s) => s },
    ThemeIcon: class { constructor(id) { this.id = id; } },
    ThemeColor: class { constructor(id) { this.id = id; } },
    TreeItem: class { constructor(label) { this.label = label; } },
    TreeItemCollapsibleState: { None: 0 },
    StatusBarAlignment: { Left: 1 },
    ConfigurationTarget: { Global: 1, Workspace: 2, WorkspaceFolder: 3 },
    ColorThemeKind: { Light: 1, Dark: 2, HighContrast: 3, HighContrastLight: 4 },
    workspace: { getConfiguration: cfg, onDidChangeConfiguration: () => ({ dispose() {} }) },
    window: {
      activeColorTheme: { kind: 2 },
      createOutputChannel: () => ({ appendLine: (l) => calls.push(['log', l]), append: () => {}, show() {}, dispose() {} }),
      createStatusBarItem: () => ({ show() {}, dispose() {} }),
      registerTreeDataProvider: (id, p) => { registered.tree = p; return { dispose() {} }; },
      showWarningMessage: async (...a) => { calls.push(['warn', a[0]]); return undefined; },
      showErrorMessage: async (m) => { calls.push(['error', m]); return undefined; },
      showInformationMessage: async (m) => { calls.push(['info', m]); },
      showQuickPick: async (items) => items[0],
    },
    commands: {
      registerCommand: (id, fn) => { registered[id] = fn; return { dispose() {} }; },
      getCommands: async () => ['simpleBrowser.show', 'workbench.action.browser.open'],
      executeCommand: async (id, ...a) => { calls.push(['exec', id, ...a]); },
    },
    registered,
  };
}

test('extension manages htadb and opens pages in the integrated browser', { skip: !have && 'bin/htadb or bin/fakeadb not built' }, async () => {
  const fake = cp.spawn(fakeadb, ['-addr', '127.0.0.1:0'], { stdio: ['ignore', 'pipe', 'ignore'] });
  const addr = await new Promise((res) => fake.stdout.on('data', (d) => { const m = /FAKEADB_LISTENING (\S+)/.exec(String(d)); if (m) res(m[1]); }));
  const storage = fs.mkdtempSync(path.join(os.tmpdir(), 'htadb-ext-'));
  const calls = [];
  const settings = {
    'htadb.url': '', 'htadb.binaryPath': htadb, 'htadb.args': ['--adb-server', addr, '--config-dir', storage],
    'htadb.managed.useTls': false, 'htadb.browser': 'integrated', 'htadb.mirror.fallback': true,
    'htadb.mirror.serverVersion': 'auto', 'htadb.mirror.devices': { R58M12345: { version: '4.1' } },
    'htadb.mirror.defaults': { maxSize: 1280 }, 'htadb.mirror.hotkeys': {},
  };
  const vscode = stubVSCode(settings, calls);
  const origLoad = Module._load;
  Module._load = function (req, ...rest) { return req === 'vscode' ? vscode : origLoad.call(this, req, ...rest); };
  const ext = require('../extension.js');
  const ctx = { subscriptions: [], extensionPath: path.join(root, 'vscode-extension'), globalStorageUri: { fsPath: storage } };
  try {
    ext.activate(ctx);
    const deadline = Date.now() + 15000;
    let items = [];
    while (Date.now() < deadline) {
      items = vscode.registered.tree ? vscode.registered.tree.getChildren() : [];
      if (items.length >= 3) break;
      await new Promise((r) => setTimeout(r, 100));
    }
    assert.match(items[0].label, /^htadb running · http:\/\/127\.0\.0\.1:\d+$/);
    const bySerial = Object.fromEntries(items.slice(1).map((i) => [i.label, i]));
    assert.deepEqual(Object.keys(bySerial).sort(), ['R58M12345', 'emulator-5554']);
    assert.equal(bySerial.R58M12345.contextValue, 'device');
    assert.equal(bySerial.R58M12345.description, 'SM G973F');
    const url = items[0].label.split(' · ')[1];
    // Pages open via simpleBrowser.show (VS Code 1.113+ routes it to the integrated browser).
    await vscode.registered['htadb.mirrorDevice'](bySerial.R58M12345);
    await vscode.registered['htadb.openTerminal'](bySerial['emulator-5554']);
    const opens = calls.filter((c) => c[0] === 'exec' && c[1] === 'simpleBrowser.show').map((c) => c[2]);
    assert.deepEqual(opens, [`${url}/ui/mirror.html?serial=R58M12345&autoconnect=1`, `${url}/ui/terminal.html?mode=shell&serial=emulator-5554`]);
    assert.equal(calls.filter((c) => c[0] === 'warn').length, 0, 'no integrated-browser warning on 1.147');
    // The generated config reaches the pages and htadb.
    const cfgJs = await (await fetch(url + '/ui/config.js')).text();
    assert.match(cfgJs, /"maxSize":1280/);
    assert.match(fs.readFileSync(path.join(storage, 'config.toml'), 'utf8'), /\[mirror\.devices\."R58M12345"\]\nversion = "4\.1"/);
  } finally {
    await ext.deactivate();
    Module._load = origLoad;
    fake.kill();
    fs.rmSync(storage, { recursive: true, force: true });
  }
});
