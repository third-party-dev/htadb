// Browser smoke test: headless Chromium against htadb backed by fakeadb.
//   node e2e/smoke.mjs <htadb base URL> <screenshot dir>
// Checks: pages load with a clean console; the terminal talks to the device
// (built-ins, CLI passthrough, PTY shell); mirroring decodes H.264 frames;
// the 4.x fallback banner appears for a device whose 5.0 server fails.
import { writeFileSync, mkdirSync } from 'node:fs';
import { join } from 'node:path';
import { launch, sleep } from './cdp.mjs';

const base = process.argv[2] || 'http://127.0.0.1:18443';
const shots = process.argv[3] || 'testdata-gen/screens';
mkdirSync(shots, { recursive: true });
const chrome = process.env.CHROMIUM || 'chromium';

const results = [];
const check = (name, ok, detail = '') => { results.push({ name, ok, detail }); console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}${detail ? ' — ' + detail : ''}`); };

const browser = await launch(chrome);
console.log('browser:', browser.version);
const allErrors = [];
const save = async (page, name) => writeFileSync(join(shots, name + '.png'), await page.screenshot());

try {
  // Index.
  let p = await browser.page(`${base}/ui/`);
  check('index lists devices', await p.waitFor(`document.querySelectorAll('.devrow').length === 2`));
  await save(p, 'index');
  allErrors.push(...p.errors.map((e) => 'index: ' + e));
  await p.close();

  // Terminal.
  p = await browser.page(`${base}/ui/terminal.html?serial=R58M12345`);
  const buf = `(() => { const b = htadbTerminal.term.buffer.active; let s = ''; for (let i = 0; i < b.length; i++) s += b.getLine(i).translateToString(true) + '\\n'; return s; })()`;
  check('terminal prompt', await p.waitFor(`${buf}.includes('adb [R58M12345]>')`));
  await p.eval(`document.querySelector('.xterm-helper-textarea').focus()`);
  await p.type('devices -l\r');
  check('terminal devices -l', await p.waitFor(`${buf}.includes('model:SM_G973F')`));
  await p.type('ls /sdcard/Download\r');
  check('terminal ls', await p.waitFor(`${buf}.includes('notes.txt')`));
  await p.type('get-serialno\r');
  check('terminal CLI passthrough', await p.waitFor(`${buf}.includes('[exit') || /\\nR58M12345\\n/.test(${buf})`, 8000), 'adb get-serialno via /cli');
  await p.type('shell\r');
  check('terminal shell prompt', await p.waitFor(`${buf}.includes('beyond1:/ $')`));
  await p.type('getprop ro.build.version.sdk\r');
  check('terminal shell command', await p.waitFor(`/\\n34\\s*\\n/.test(${buf})`));
  await p.type('exit\r');
  check('terminal shell exit', await p.waitFor(`${buf}.trimEnd().endsWith('adb [R58M12345]>')`));
  check('terminal sidebar', await p.eval(`document.querySelector('.devrow.current .serial').textContent === 'R58M12345'`));
  await save(p, 'terminal');
  allErrors.push(...p.errors.map((e) => 'terminal: ' + e));
  await p.close();

  // Mirroring.
  p = await browser.page(`${base}/ui/mirror.html?serial=R58M12345&autoconnect=1`);
  const hasDecoder = await p.waitFor(`typeof VideoDecoder === 'function'`, 3000);
  check('WebCodecs available', hasDecoder);
  const support = hasDecoder ? await p.eval(`VideoDecoder.isConfigSupported({codec: 'avc1.42c01f'}).then(r => r.supported)`) : false;
  check('meta received', await p.waitFor(`htadbMirror.meta && htadbMirror.meta.version === '5.0.1'`, 15000),
    await p.eval(`JSON.stringify(htadbMirror.meta && {v: htadbMirror.meta.version, p: htadbMirror.meta.profile, verified: htadbMirror.meta.verified})`));
  if (support) {
    check('H.264 frames decoded and drawn', await p.waitFor(`htadbMirror.stats.frames > 20`, 15000), await p.eval(`JSON.stringify(htadbMirror.stats)`));
  } else {
    console.log('note: this Chromium cannot decode H.264; frame decoding moves to real-device testing');
    check('packets received (no H.264 decoder)', await p.waitFor(`htadbMirror.stats.bytes > 100000`, 15000));
  }
  await sleep(500);
  await save(p, 'mirror');
  // Hotkeys dialog via its shortcut (Alt+/).
  await p.eval(`document.getElementById('stage').focus()`);
  await p.key('/', 'Slash', 191, 1);
  check('hotkeys dialog', await p.waitFor(`!document.getElementById('hotkeys_panel').hidden && document.querySelectorAll('.hk-row').length > 15`));
  await save(p, 'hotkeys');
  await p.key('Escape', 'Escape', 27);
  // Device clipboard round trip (Alt+C → device sends its clipboard).
  await p.key('c', 'KeyC', 67, 1);
  check('device clipboard', await p.waitFor(`document.getElementById('clip').value === 'hello from the fake device'`, 5000));
  allErrors.push(...p.errors.map((e) => 'mirror: ' + e));
  await p.close();

  // Fallback: emulator-5554's 5.0.1 server fails at launch.
  p = await browser.page(`${base}/ui/mirror.html?serial=emulator-5554&autoconnect=1`);
  check('fallback to 4.1', await p.waitFor(`htadbMirror.meta && htadbMirror.meta.version === '4.1' && htadbMirror.meta.fallbackFrom === '5.0.1'`, 20000));
  check('fallback banner', await p.waitFor(`[...document.querySelectorAll('.banner')].some(b => b.textContent.includes('Running 4.1.') && b.textContent.includes('Retry with 5.0.1'))`));
  await sleep(500);
  await save(p, 'mirror-fallback');
  allErrors.push(...p.errors.map((e) => 'fallback: ' + e));
  await p.close();
} finally {
  browser.close();
}

check('clean console', allErrors.length === 0, allErrors.join(' | '));
const failed = results.filter((r) => !r.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed; screenshots in ${shots}`);
process.exit(failed.length ? 1 : 0);
