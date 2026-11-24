// Fits an xterm.js Terminal to its container. A small replacement for
// @xterm/addon-fit that also works with VS Code's bundled xterm.js (which
// ships without addons). Only public API plus the render service's cell
// size, with a measured fallback.

function cellSize(term) {
  // xterm 5: term._core._renderService.dimensions.css.cell
  try {
    const d = term._core._renderService.dimensions;
    const c = (d.css && d.css.cell) || d.actualCell || null;
    if (c && c.width > 0 && c.height > 0) return { width: c.width, height: c.height };
  } catch { /* private API moved; measure instead */ }
  const span = document.createElement('span');
  span.textContent = 'W'.repeat(32);
  span.style.cssText = `position:absolute;visibility:hidden;white-space:pre;font-family:${term.options.fontFamily};font-size:${term.options.fontSize}px;line-height:${term.options.lineHeight || 1}`;
  document.body.appendChild(span);
  const r = span.getBoundingClientRect();
  span.remove();
  return { width: r.width / 32, height: r.height };
}

/** Proposed {cols, rows} for the terminal's parent element. */
export function proposeDimensions(term) {
  const el = term.element && term.element.parentElement;
  if (!el) return null;
  const cell = cellSize(term);
  const style = getComputedStyle(el);
  const pad = (a, b) => parseFloat(style[a]) + parseFloat(style[b]);
  const width = el.clientWidth - pad('paddingLeft', 'paddingRight') - 14; // scrollbar
  const height = el.clientHeight - pad('paddingTop', 'paddingBottom');
  const cols = Math.max(2, Math.floor(width / cell.width));
  const rows = Math.max(1, Math.floor(height / cell.height));
  return { cols, rows };
}

/** Resize the terminal to fit; returns the new size. */
export function fit(term) {
  const d = proposeDimensions(term);
  if (d && (d.cols !== term.cols || d.rows !== term.rows)) term.resize(d.cols, d.rows);
  return d;
}

/** Keep the terminal fitted on window/container resize. */
export function autoFit(term, onResize) {
  let pending = 0;
  const run = () => {
    pending = 0;
    fit(term);
    if (onResize) onResize(term.cols, term.rows);
  };
  const schedule = () => { if (!pending) pending = requestAnimationFrame(run); };
  const ro = new ResizeObserver(schedule);
  ro.observe(term.element.parentElement);
  window.addEventListener('resize', schedule);
  run();
  return () => { ro.disconnect(); window.removeEventListener('resize', schedule); };
}
