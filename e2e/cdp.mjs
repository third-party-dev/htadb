// Minimal Chrome DevTools Protocol client (Node's built-in WebSocket and
// fetch; no npm packages).
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

export async function launch(binary, port = 9333) {
  const profile = mkdtempSync(join(tmpdir(), 'htadb-chrome-'));
  const proc = spawn(binary, [
    '--headless=new', `--remote-debugging-port=${port}`, `--user-data-dir=${profile}`,
    '--no-sandbox', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--autoplay-policy=no-user-gesture-required', '--window-size=1440,900', 'about:blank',
  ], { stdio: ['ignore', 'ignore', 'pipe'] });
  let stderr = '';
  proc.stderr.on('data', (d) => { stderr += d; });
  let ver;
  for (let i = 0; i < 100; i++) {
    try { ver = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json(); break; } catch { await sleep(100); }
  }
  if (!ver) { proc.kill(); throw new Error('chromium did not start: ' + stderr.slice(-2000)); }
  const browser = await connect(ver.webSocketDebuggerUrl);
  browser.close = () => { try { proc.kill('SIGKILL'); } catch { /* gone */ } rmSync(profile, { recursive: true, force: true }); };
  browser.version = ver.Browser;
  return browser;
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function connect(url) {
  const ws = new WebSocket(url);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0;
  const pending = new Map();
  const listeners = new Set();
  ws.onmessage = (ev) => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) {
      const { res, rej } = pending.get(m.id);
      pending.delete(m.id);
      if (m.error) rej(new Error(m.error.message)); else res(m.result);
    } else {
      for (const l of listeners) l(m);
    }
  };
  const send = (method, params = {}, sessionId) => new Promise((res, rej) => {
    const msg = { id: ++id, method, params };
    if (sessionId) msg.sessionId = sessionId;
    pending.set(msg.id, { res, rej });
    ws.send(JSON.stringify(msg));
  });
  return {
    send,
    on: (fn) => listeners.add(fn),
    /** Open a page; returns helpers bound to its session. */
    async page(url) {
      const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
      const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
      const s = (m, p) => send(m, p, sessionId);
      const errors = [];
      const logs = [];
      listeners.add((m) => {
        if (m.sessionId !== sessionId) return;
        if (m.method === 'Runtime.consoleAPICalled') {
          const text = m.params.args.map((a) => a.value ?? a.description ?? '').join(' ');
          logs.push(`${m.params.type}: ${text}`);
          if (m.params.type === 'error') errors.push(text);
        } else if (m.method === 'Runtime.exceptionThrown') {
          const d = m.params.exceptionDetails;
          errors.push((d.exception && d.exception.description) || d.text);
        } else if (m.method === 'Log.entryAdded' && m.params.entry.level === 'error') {
          errors.push(`${m.params.entry.source}: ${m.params.entry.text} ${m.params.entry.url || ''}`);
        }
      });
      await s('Runtime.enable');
      await s('Log.enable');
      await s('Page.enable');
      await s('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false });
      await s('Page.navigate', { url });
      const page = {
        errors, logs,
        async eval(expr) {
          const r = await s('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
          if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
          return r.result.value;
        },
        async waitFor(expr, timeout = 10000) {
          const t0 = Date.now();
          for (;;) {
            try { if (await page.eval(expr)) return true; } catch { /* not ready */ }
            if (Date.now() - t0 > timeout) return false;
            await sleep(100);
          }
        },
        async type(text) {
          // insertText for the text; real key events for Enter.
          for (const part of text.split(/(\r)/)) {
            if (part === '\r') await page.key('Enter', 'Enter', 13);
            else if (part) await s('Input.insertText', { text: part });
          }
        },
        async key(key, code, keyCode, modifiers = 0) {
          await s('Input.dispatchKeyEvent', { type: 'keyDown', key, code, windowsVirtualKeyCode: keyCode, modifiers });
          await s('Input.dispatchKeyEvent', { type: 'keyUp', key, code, windowsVirtualKeyCode: keyCode, modifiers });
        },
        async screenshot() { return Buffer.from((await s('Page.captureScreenshot', { format: 'png' })).data, 'base64'); },
        close: () => send('Target.closeTarget', { targetId }),
      };
      return page;
    },
  };
}
