// Hotkey registry for the mirroring page: action → key combo, with a
// configurable modifier ("Mod": Alt by default, or Ctrl+Alt, Super, Ctrl).
// Combos use KeyboardEvent.code so they are layout- and Option-key-
// independent, e.g. "Mod+KeyH", "Mod+Shift+KeyO", "Mod+ArrowUp".
//
// Bindings persist in localStorage (guarded: storage may be unavailable) or
// come from config.js ({hotkeys: {modifier, bindings}}).

import { icon } from './icons.js';

const STORAGE_KEY = 'htadb.mirror.hotkeys';
const MODS = ['Ctrl', 'Alt', 'Shift', 'Meta'];
export const MODIFIERS = [['Alt', 'Alt'], ['Ctrl+Alt', 'Ctrl+Alt'], ['Meta', 'Super'], ['Ctrl', 'Ctrl']];

/** Default actions; combos follow scrcpy's shortcuts. */
export const DEFAULT_ACTIONS = [
  { id: 'home', label: 'Home', combo: 'Mod+KeyH' },
  { id: 'back', label: 'Back', combo: 'Mod+KeyB' },
  { id: 'appSwitch', label: 'App switch', combo: 'Mod+KeyS' },
  { id: 'menu', label: 'Menu', combo: 'Mod+KeyM' },
  { id: 'volumeUp', label: 'Volume up', combo: 'Mod+ArrowUp' },
  { id: 'volumeDown', label: 'Volume down', combo: 'Mod+ArrowDown' },
  { id: 'power', label: 'Power', combo: 'Mod+KeyP' },
  { id: 'screenOff', label: 'Screen off', combo: 'Mod+KeyO' },
  { id: 'screenOn', label: 'Screen on', combo: 'Mod+Shift+KeyO' },
  { id: 'rotate', label: 'Rotate device', combo: 'Mod+KeyR' },
  { id: 'notifications', label: 'Expand notifications', combo: 'Mod+KeyN' },
  { id: 'collapsePanels', label: 'Collapse panels', combo: 'Mod+Shift+KeyN' },
  { id: 'copy', label: 'Copy from device', combo: 'Mod+KeyC' },
  { id: 'typeClipboard', label: 'Paste as keystrokes', combo: 'Mod+Shift+KeyV' },
  { id: 'fullscreen', label: 'Fullscreen', combo: 'Mod+KeyF' },
  { id: 'resize11', label: 'Resize to 1:1', combo: 'Mod+KeyG' },
  { id: 'fps', label: 'FPS overlay', combo: 'Mod+KeyI' },
  { id: 'hotkeys', label: 'Show hotkeys', combo: 'Mod+Slash' },
  { id: 'back2', label: 'Back (alternative)', combo: 'Mod+Backspace' },
  { id: 'cut', label: 'Cut from device', combo: 'Mod+KeyX' },
  { id: 'paste', label: 'Paste from this computer', combo: 'Mod+KeyV' },
  { id: 'settingsPanel', label: 'Expand settings panel', combo: '' },
  { id: 'keyboardSettings', label: 'Keyboard settings', combo: 'Mod+KeyK' },
  { id: 'resetVideo', label: 'Reset video stream', combo: 'Mod+Shift+KeyR' },
];

function storageGet() {
  try { return JSON.parse(localStorage.getItem(STORAGE_KEY) || 'null'); } catch { return null; }
}
function storageSet(v) {
  try { localStorage.setItem(STORAGE_KEY, JSON.stringify(v)); } catch { /* unavailable */ }
}

const ORDER = ['Mod', ...MODS];

/** Normalize a combo: Mod first, then modifiers in fixed order, then the code. */
export function normalize(combo) {
  if (!combo) return '';
  const parts = combo.split('+').map((p) => p.trim()).filter(Boolean);
  const code = parts.pop();
  const mods = ORDER.filter((m) => parts.some((p) => p.toLowerCase() === m.toLowerCase()));
  return [...mods, code].join('+');
}

const MOD_CODES = ['ShiftLeft', 'ShiftRight', 'ControlLeft', 'ControlRight', 'AltLeft', 'AltRight', 'MetaLeft', 'MetaRight', 'OSLeft', 'OSRight'];
const held = (e) => ({ Ctrl: !!e.ctrlKey, Alt: !!e.altKey, Shift: !!e.shiftKey, Meta: !!e.metaKey });

/** Combo for an event; `mod` is the configured modifier ("Alt", "Ctrl+Alt"…). */
export function eventCombo(e, mod) {
  if (MOD_CODES.includes(e.code)) return null;
  const h = held(e);
  const modSet = mod.split('+');
  const parts = [];
  if (modSet.every((m) => h[m])) {
    parts.push('Mod');
    for (const m of modSet) h[m] = false;
  }
  for (const m of MODS) if (h[m]) parts.push(m);
  parts.push(e.code);
  return parts.join('+');
}

/** Human-readable combo. */
export function display(combo, mod) {
  if (!combo) return '';
  const modLabel = (MODIFIERS.find(([v]) => v === mod) || [mod, mod])[1];
  return combo.split('+').map((p) => {
    if (p === 'Mod') return modLabel;
    if (p === 'Meta') return 'Super';
    if (p.startsWith('Key')) return p.slice(3);
    if (p.startsWith('Digit')) return p.slice(5);
    return { ArrowUp: '↑', ArrowDown: '↓', ArrowLeft: '←', ArrowRight: '→', Slash: '/', Backspace: '⌫', Period: '.', Comma: ',' }[p] || p;
  }).join('+');
}

export class Hotkeys {
  constructor(config = {}) {
    this.defaults = Object.fromEntries(DEFAULT_ACTIONS.map((a) => [a.id, a.combo]));
    this.labels = Object.fromEntries(DEFAULT_ACTIONS.map((a) => [a.id, a.label]));
    this.handlers = {};
    const saved = storageGet();
    const cfg = config.hotkeys || {};
    this.defaultModifier = cfg.modifier || 'Alt';
    this.modifier = (saved && saved.modifier) || this.defaultModifier;
    this.bindings = { ...this.defaults, ...(cfg.bindings || {}), ...((saved && saved.bindings) || {}) };
    this.listeners = new Set();
  }

  on(id, fn) { this.handlers[id] = fn; return this; }
  onChange(fn) { this.listeners.add(fn); }
  save() { storageSet({ modifier: this.modifier, bindings: this.bindings }); for (const f of this.listeners) f(); }

  /** Action id for an event, or null. */
  match(e) {
    const c = eventCombo(e, this.modifier);
    if (!c) return null;
    const n = normalize(c);
    for (const [id, combo] of Object.entries(this.bindings)) {
      if (combo && normalize(combo) === n) return id;
    }
    return null;
  }

  /** Run a matching handler; returns true if the event was consumed. */
  handle(e) {
    const id = this.match(e);
    if (!id || !this.handlers[id]) return false;
    e.preventDefault();
    e.stopPropagation();
    this.handlers[id](e);
    return true;
  }

  /** True when the configured modifier is held (those keys are reserved). */
  usesModifier(e) {
    const h = held(e);
    return this.modifier.split('+').every((m) => h[m]);
  }

  bind(id, combo) { this.bindings[id] = normalize(combo); this.save(); }
  reset() { this.bindings = { ...this.defaults }; this.modifier = this.defaultModifier; this.save(); }
  setModifier(m) { if (MODIFIERS.some(([v]) => v === m)) { this.modifier = m; this.save(); } }

  export() { return JSON.stringify({ modifier: this.modifier, bindings: this.bindings }, null, 2); }
  import(text) {
    const v = JSON.parse(text);
    if (!v || typeof v !== 'object' || typeof v.bindings !== 'object') throw new Error('expected {"modifier", "bindings"}');
    if (v.modifier) this.setModifier(v.modifier);
    for (const [id, combo] of Object.entries(v.bindings)) {
      if (id in this.defaults && typeof combo === 'string') this.bindings[id] = normalize(combo);
    }
    this.save();
  }

  /** Render the hotkeys dialog into `root` (a .modal-backdrop element). */
  renderPanel(root, onClose) {
    root.innerHTML = `
<div class="hk" role="dialog" aria-modal="true" aria-labelledby="hk-title">
  <div class="hk-head">
    <h1 id="hk-title">Hotkeys</h1>
    <label for="hk-mod">Modifier</label>
    <select id="hk-mod"></select>
    <button type="button" class="hk-close" aria-label="Close hotkeys">${icon('close')}</button>
  </div>
  <p class="hk-desc">Defaults follow scrcpy’s shortcuts. Click a binding, then press the new combination (Esc cancels, Backspace clears).</p>
  <p class="hk-msg"></p>
  <div class="hk-rows"></div>
  <div class="hk-foot">
    <button type="button" class="btn" data-act="reset">Reset to defaults</button>
    <div class="grow"></div>
    <button type="button" class="btn" data-act="import">Import…</button>
    <button type="button" class="btn btn-accent" data-act="export">Export JSON</button>
  </div>
</div>`;
    const q = (s) => root.querySelector(s);
    const rerender = () => this.renderPanel(root, onClose);
    const sel = q('#hk-mod');
    for (const [v, label] of MODIFIERS) sel.append(new Option(label, v, false, v === this.modifier));
    sel.onchange = () => { this.setModifier(sel.value); rerender(); };
    q('.hk-close').onclick = onClose;
    const msg = q('.hk-msg');
    const rows = q('.hk-rows');
    let listening = null; // keydown handler while waiting for a combo
    let listeningBtn = null;
    const stopListening = () => {
      if (!listening) return;
      window.removeEventListener('keydown', listening, true);
      listening = null;
      if (listeningBtn) { listeningBtn.classList.remove('listening'); listeningBtn.textContent = listeningBtn.dataset.text; }
      listeningBtn = null;
    };
    for (const id of Object.keys(this.defaults)) {
      const row = document.createElement('div');
      row.className = 'hk-row';
      const label = document.createElement('span');
      label.textContent = this.labels[id];
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'hk-bind' + (this.bindings[id] ? '' : ' unbound');
      b.textContent = this.bindings[id] ? display(this.bindings[id], this.modifier) : 'unbound';
      b.setAttribute('aria-label', `${this.labels[id]}: ${b.textContent}. Click to change.`);
      b.dataset.text = b.textContent;
      b.onclick = () => {
        stopListening();
        listeningBtn = b;
        b.classList.add('listening');
        b.textContent = 'Press keys…';
        listening = (e) => {
          e.preventDefault();
          e.stopPropagation();
          if (e.code === 'Escape') { stopListening(); rerender(); return; }
          if ((e.code === 'Backspace' || e.code === 'Delete') && !e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey) {
            stopListening(); this.bind(id, ''); rerender(); return;
          }
          const combo = eventCombo(e, this.modifier);
          if (!combo) return; // modifier alone: keep waiting
          stopListening();
          const clash = Object.entries(this.bindings).find(([o, c]) => o !== id && c && normalize(c) === normalize(combo));
          if (clash) this.bindings[clash[0]] = '';
          this.bind(id, combo);
          rerender();
          if (clash) q('.hk-msg').textContent = `${display(combo, this.modifier)} was bound to “${this.labels[clash[0]]}”; that binding was cleared.`;
        };
        window.addEventListener('keydown', listening, true);
      };
      row.append(label, b);
      rows.append(row);
    }
    root.querySelector('[data-act=reset]').onclick = () => { this.reset(); rerender(); };
    root.querySelector('[data-act=export]').onclick = () => {
      const blob = new Blob([this.export() + '\n'], { type: 'application/json' });
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = 'htadb-hotkeys.json';
      a.click();
      setTimeout(() => URL.revokeObjectURL(a.href), 10000);
    };
    root.querySelector('[data-act=import]').onclick = () => {
      const input = document.createElement('input');
      input.type = 'file';
      input.accept = '.json,application/json';
      input.onchange = async () => {
        try { this.import(await input.files[0].text()); rerender(); } catch (err) { msg.textContent = 'Import failed: ' + err.message; }
      };
      input.click();
    };
    root.onkeydown = (e) => { if (e.code === 'Escape' && !listening) { e.preventDefault(); onClose(); } };
    sel.focus();
  }
}
