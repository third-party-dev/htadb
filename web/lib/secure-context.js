// Shows a banner when the page is not in a secure context. Browsers only
// expose WebCodecs (mirroring) and the async Clipboard API to secure
// contexts: https, or http on loopback (127.0.0.1, ::1, localhost).
import { banner } from './ui.js';

/**
 * Check the context and, if insecure, add a banner to `container`
 * (a .banners element). Returns true when the context is secure.
 */
export function checkSecureContext(container, needs = 'mirroring and clipboard sync') {
  if (window.isSecureContext) return true;
  banner(container, {
    kind: 'warn',
    key: 'secure-context',
    bold: 'Not a secure context.',
    text: `This page was loaded over plain HTTP from ${location.host}, so the browser disables ${needs}. ` +
      'Open htadb via http://127.0.0.1 on this machine, use an https:// listener, or put a TLS-terminating proxy in front of it.',
  });
  return false;
}
