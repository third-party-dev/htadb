// htadb REST/WebSocket helpers.
//
// The API base defaults to the page's own origin (pages are served by htadb
// under /ui/). For UI development with another web server, pass
// ?api=http://127.0.0.1:10443 in the page URL.

const params = new URLSearchParams(location.search);

/** Base URL of the htadb server, without trailing slash. */
export const apiBase = (params.get('api') || location.origin).replace(/\/+$/, '');

/** Page query parameter helper. */
export function param(name, fallback = null) {
  const v = params.get(name);
  return v === null ? fallback : v;
}

/** Absolute URL for an /api/v1 path. */
export function apiURL(path, query) {
  const u = new URL(apiBase + '/api/v1' + path);
  if (query) {
    for (const [k, v] of Object.entries(query)) {
      if (v === undefined || v === null || v === '') continue;
      if (Array.isArray(v)) v.forEach((x) => u.searchParams.append(k, x));
      else u.searchParams.set(k, String(v));
    }
  }
  return u.toString();
}

/** WebSocket URL for an /api/v1 path. */
export function wsURL(path, query) {
  return apiURL(path, query).replace(/^http/, 'ws');
}

/** Error thrown for non-2xx responses; carries htadb's {error, code}. */
export class APIError extends Error {
  constructor(status, body) {
    super((body && body.error) || `HTTP ${status}`);
    this.status = status;
    this.code = body && body.code;
    this.body = body;
  }
}

async function check(resp) {
  if (resp.ok) return resp;
  let body = null;
  try { body = await resp.json(); } catch { /* not JSON */ }
  throw new APIError(resp.status, body);
}

/** GET/POST/... JSON helper. */
export async function call(method, path, { query, body, signal } = {}) {
  const init = { method, signal, headers: {} };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const resp = await check(await fetch(apiURL(path, query), init));
  const ct = resp.headers.get('Content-Type') || '';
  return ct.includes('application/json') ? resp.json() : resp;
}

export const get = (path, query) => call('GET', path, { query });
export const post = (path, body, query) => call('POST', path, { body, query });
export const del = (path, query) => call('DELETE', path, { query });

/** Raw fetch returning the Response (for downloads). */
export async function fetchRaw(path, query, init = {}) {
  return check(await fetch(apiURL(path, query), init));
}

/**
 * Upload a Blob with progress (fetch has no upload progress, so XHR).
 * Resolves to the parsed JSON response.
 */
export function upload(method, path, query, blob, onProgress, contentType = 'application/octet-stream') {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open(method, apiURL(path, query));
    xhr.setRequestHeader('Content-Type', contentType);
    xhr.upload.onprogress = (e) => { if (onProgress && e.lengthComputable) onProgress(e.loaded, e.total); };
    xhr.onerror = () => reject(new Error('network error'));
    xhr.onload = () => {
      let body = null;
      try { body = JSON.parse(xhr.responseText); } catch { /* ignore */ }
      if (xhr.status >= 200 && xhr.status < 300) resolve(body);
      else reject(new APIError(xhr.status, body));
    };
    xhr.send(blob);
  });
}

/** Save a Blob as a browser download. */
export function saveBlob(blob, name) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30000);
}

/** Ask the user for a file (must be called from a user gesture). */
export function pickFile(accept) {
  return new Promise((resolve) => {
    const input = document.createElement('input');
    input.type = 'file';
    if (accept) input.accept = accept;
    input.style.display = 'none';
    document.body.appendChild(input);
    input.addEventListener('change', () => { resolve(input.files[0] || null); input.remove(); });
    // "cancel" is supported by current browsers; older ones never resolve.
    input.addEventListener('cancel', () => { resolve(null); input.remove(); });
    input.click();
  });
}

/**
 * Live device list via /devices/track. Calls onChange(devices[]) on every
 * change and reconnects with backoff. Returns a stop function.
 */
export function trackDevices(onChange, onStatus = () => {}) {
  let ws = null;
  let stopped = false;
  let delay = 500;
  const devices = new Map();
  const emit = () => onChange([...devices.values()].sort((a, b) => a.serial.localeCompare(b.serial)));
  const connect = () => {
    if (stopped) return;
    ws = new WebSocket(wsURL('/devices/track'));
    ws.onopen = () => { delay = 500; onStatus('connected'); };
    ws.onmessage = (ev) => {
      const m = JSON.parse(ev.data);
      if (m.type === 'snapshot') {
        devices.clear();
        for (const d of m.devices) devices.set(d.serial, d);
        if (!m.adb) onStatus('adb-unavailable');
      } else if (m.type === 'remove') {
        devices.delete(m.device.serial);
      } else {
        devices.set(m.device.serial, m.device);
      }
      emit();
    };
    ws.onclose = () => {
      onStatus('disconnected');
      if (stopped) return;
      setTimeout(connect, delay);
      delay = Math.min(delay * 2, 8000);
    };
  };
  connect();
  return () => { stopped = true; if (ws) ws.close(); };
}
