// Inline SVG icons (24×24 strokes) used by the pages, from the UI designs.
const paths = {
  back: '<path d="M15 6 9 12l6 6"/>',
  home: '<circle cx="12" cy="12" r="7"/>',
  apps: '<rect x="6" y="6" width="12" height="12" rx="2"/>',
  volUp: '<path d="M4 10v4h4l5 4V6L8 10H4z"/><path d="M17 9v6M14 12h6"/>',
  volDown: '<path d="M4 10v4h4l5 4V6L8 10H4z"/><path d="M15 12h6"/>',
  power: '<path d="M12 3v8"/><path d="M6.5 7a7 7 0 1 0 11 0"/>',
  rotate: '<path d="M20 12a8 8 0 1 1-3-6.2"/><path d="M20 4v5h-5"/>',
  panels: '<path d="M4 5h16M4 10h16"/><path d="m9 15 3 3 3-3"/>',
  full: '<path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5"/>',
  warn: '<path d="M12 3 2 21h20L12 3z"/><path d="M12 10v5"/><path d="M12 18h.01"/>',
  close: '<path d="M6 6l12 12M18 6 6 18"/>',
  screen: '<rect x="3" y="4" width="18" height="14" rx="2"/><path d="m10 9 5 2.5-5 2.5z"/>',
};

/** SVG markup for an icon. */
export function icon(name, { size = 20, stroke = 'currentColor', width = 2 } = {}) {
  return `<svg aria-hidden="true" width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="${stroke}" stroke-width="${width}" stroke-linecap="round" stroke-linejoin="round">${paths[name] || ''}</svg>`;
}
