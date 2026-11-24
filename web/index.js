// htadb landing page: live device list with Terminal and Mirror links.
import { apiBase, get, trackDevices } from './lib/api.js';
import { checkSecureContext } from './lib/secure-context.js';
import { deviceMeta, pageLink } from './lib/ui.js';

const $ = (id) => document.getElementById(id);
checkSecureContext($('banners'));
$('home').href = pageLink('index.html');
$('nav-term').href = pageLink('terminal.html');
$('nav-mirror').href = pageLink('mirror.html');
$('f-api').textContent = apiBase;

get('/version').then((v) => {
  $('f-version').textContent = `htadb ${v.version}`;
  $('f-adb').textContent = `adb server ${v.adbServerVersion ?? 'unavailable'}`;
}).catch((e) => { $('f-version').textContent = 'htadb unreachable: ' + e.message; });

function status(text, cls) {
  $('adb-status').textContent = text;
  $('adb-dot').className = 'dot ' + (cls || '');
}

const list = $('devices');
trackDevices((devices) => {
  status('adb server connected', 'on');
  list.innerHTML = '';
  if (!devices.length) {
    list.innerHTML = '<p class="empty">No devices. Connect one over USB, or run <code>connect &lt;host:port&gt;</code> in the terminal.</p>';
    return;
  }
  for (const d of devices) {
    const row = document.createElement('div');
    row.className = 'devrow';
    row.innerHTML = '<div class="info"><p class="serial"></p><p class="meta"></p></div>';
    row.querySelector('.serial').textContent = d.serial;
    row.querySelector('.meta').textContent = deviceMeta(d);
    const t = document.createElement('a');
    t.className = 'btn';
    t.textContent = 'Terminal';
    t.href = pageLink('terminal.html', { mode: 'shell', serial: d.serial });
    const m = document.createElement('a');
    m.className = 'btn-accent';
    m.textContent = 'Mirror';
    m.href = pageLink('mirror.html', { serial: d.serial });
    row.append(t, m);
    list.append(row);
  }
}, (s) => {
  if (s === 'adb-unavailable') status('adb server unavailable', 'err');
  else if (s === 'disconnected') status('htadb disconnected', 'warn');
});
