// Small DOM helpers shared by the pages.
import { icon } from './icons.js';

/**
 * Show a banner in `container` (a .banners element). `text` may be a string
 * or a Node; `bold` is an optional leading strong part; `buttons` are
 * [{label, onClick}]. Banners with the same `key` replace each other.
 */
export function banner(container, { kind = 'warn', bold, text, buttons = [], key, dismiss = true }) {
  if (key) container.querySelectorAll(`[data-key="${CSS.escape(key)}"]`).forEach((el) => el.remove());
  const div = document.createElement('div');
  div.className = `banner banner-${kind}`;
  div.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  if (key) div.dataset.key = key;
  div.innerHTML = icon('warn', { size: 18, stroke: kind === 'err' ? '#FF8A80' : '#F2B44B' });
  const span = document.createElement('span');
  span.className = 'text';
  if (bold) {
    const b = document.createElement('b');
    b.textContent = bold + ' ';
    span.append(b);
  }
  span.append(typeof text === 'string' ? document.createTextNode(text) : text);
  div.append(span);
  for (const { label, onClick } of buttons) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'btn';
    btn.textContent = label;
    btn.onclick = onClick;
    div.append(btn);
  }
  if (dismiss) {
    const x = document.createElement('button');
    x.type = 'button';
    x.className = 'btn';
    x.setAttribute('aria-label', 'Dismiss');
    x.innerHTML = icon('close', { size: 14 });
    x.onclick = () => div.remove();
    div.append(x);
  }
  container.append(div);
  return div;
}

/** Remove banners (all, or those with the given keys). */
export function clearBanners(container, ...keys) {
  for (const el of container.querySelectorAll('.banner')) {
    if (!keys.length || keys.includes(el.dataset.key)) el.remove();
  }
}

/** Human-readable byte size. */
export function fmtSize(n) {
  if (n < 1024) return n + ' B';
  const u = ['KB', 'MB', 'GB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(n < 10 ? 1 : 0) + ' ' + u[i];
}

/** "model · USB · device" style description for a device. */
export function deviceMeta(d) {
  const model = (d.model || d.product || '').replaceAll('_', ' ');
  let conn = '';
  if (/^emulator-\d+$/.test(d.serial)) conn = 'emulator';
  else if (/:\d+$/.test(d.serial) || d.serial.includes('._adb-tls-')) conn = 'network';
  else if (d.usb) conn = 'USB';
  return [model, conn, d.state].filter(Boolean).join(' · ');
}

/** Keep ?api= (and other dev params) when linking between pages. */
export function pageLink(page, extra = {}) {
  const q = new URLSearchParams();
  const cur = new URLSearchParams(location.search);
  if (cur.get('api')) q.set('api', cur.get('api'));
  for (const [k, v] of Object.entries(extra)) if (v !== undefined && v !== null && v !== '') q.set(k, v);
  const s = q.toString();
  return page + (s ? '?' + s : '');
}
