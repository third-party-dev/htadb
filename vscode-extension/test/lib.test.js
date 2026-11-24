'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const lib = require('../lib');

const cmds = ['simpleBrowser.show', lib.INTEGRATED_BROWSER_COMMAND];

test('browser decision by VS Code version', () => {
  assert.equal(lib.browserDecision('1.147.0', undefined, cmds).mode, 'integrated');
  assert.equal(lib.browserDecision('1.113.0', undefined, ['simpleBrowser.show']).mode, 'classic');
  assert.equal(lib.browserDecision('1.110.1', { defaultValue: false }, cmds).mode, 'setting-off');
  assert.equal(lib.browserDecision('1.110.1', { defaultValue: false, globalValue: true }, cmds).mode, 'integrated');
  const d = lib.browserDecision('1.112.0', { defaultValue: false, globalValue: true, workspaceValue: false }, cmds);
  assert.equal(d.mode, 'setting-off');
  assert.equal(d.target, 'workspace');
  assert.equal(lib.browserDecision('1.108.0', { }, []).mode, 'classic');
});

test('setting target is the most specific override', () => {
  assert.equal(lib.settingTarget({ defaultValue: false }), 'global');
  assert.equal(lib.settingTarget({ globalValue: false, workspaceValue: false }), 'workspace');
  assert.equal(lib.settingTarget({ workspaceValue: false, workspaceFolderValue: false }), 'workspaceFolder');
});

test('bundled binary path', () => {
  assert.equal(lib.bundledBinary('/x', 'linux', 'x64'), path.join('/x', 'bin', 'linux-amd64', 'htadb'));
  assert.equal(lib.bundledBinary('/x', 'win32', 'arm64'), path.join('/x', 'bin', 'windows-arm64', 'htadb.exe'));
  assert.equal(lib.bundledBinary('/x', 'darwin', 'arm64'), path.join('/x', 'bin', 'darwin-arm64', 'htadb'));
  assert.equal(lib.bundledBinary('/x', 'aix', 'ppc64'), null);
});

test('adapter args', () => {
  const a = lib.adapterArgs({ uiConfigPath: '/s/u.json', configTomlPath: '/s/c.toml', fallback: false, serverVersion: '4.1', xtermDir: '/s/xt', extraArgs: ['--log-level', 'debug'] });
  assert.deepEqual(a, ['--listen', 'http://127.0.0.1:0', '--announce', '--ui-config', '/s/u.json', '--config-file', '/s/c.toml',
    '--mirror-fallback=false', '--mirror-default-version', '4.1', '--ui-override', '/vendor/xterm=/s/xt', '--log-level', 'debug']);
  assert.equal(lib.adapterArgs({ useTls: true, serverVersion: 'auto' })[1], 'https://127.0.0.1:0');
  assert.equal(lib.parseAnnounce('HTADB_LISTENING http://127.0.0.1:41234\n'), 'http://127.0.0.1:41234');
  assert.equal(lib.parseAnnounce('time=… level=INFO'), null);
});

test('devices toml', () => {
  const t = lib.devicesToml({ 'R58M"1': { version: '4.1', max_size: 1280, audio: false }, bad: 3 });
  assert.match(t, /\[mirror\.devices\."R58M\\"1"\]\nversion = "4\.1"\nmax_size = 1280\naudio = false\n/);
  assert.doesNotMatch(t, /bad/);
});

test('VS Code xterm discovery', () => {
  const files = {
    [path.join('/app', 'node_modules.asar', '@xterm', 'xterm', 'package.json')]: '{"version":"6.1.0-beta.337"}',
    [path.join('/app', 'node_modules.asar', '@xterm', 'xterm', 'lib', 'xterm.js')]: '!function(e,t){if("object"==typeof exports',
  };
  const read = (p) => { if (p in files) return files[p]; throw new Error('ENOENT'); };
  assert.deepEqual(lib.findVSCodeXterm('/app', read), { dir: path.join('/app', 'node_modules.asar', '@xterm', 'xterm'), version: '6.1.0-beta.337' });
  assert.match(lib.findVSCodeXterm('/none', read).reason, /not found/);
  files[path.join('/app', 'node_modules.asar', '@xterm', 'xterm', 'package.json')] = '{"version":"4.19.0"}';
  assert.match(lib.findVSCodeXterm('/app', read).reason, /too old/);
});

test('device description', () => {
  assert.equal(lib.deviceDescription({ serial: 'R58', model: 'SM_G973F', usb: '1-1', state: 'device' }), 'SM G973F · USB');
  assert.equal(lib.deviceDescription({ serial: 'emulator-5554', model: 'sdk', state: 'offline' }), 'sdk · emulator · offline');
});
