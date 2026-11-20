'use strict';
// Runs before first paint so the chosen theme never flashes.
(function () {
  let theme = '';
  try { theme = localStorage.getItem('transcoop-theme') || ''; } catch (e) {}
  if (!theme) theme = matchMedia('(prefers-color-scheme: light)').matches ? 'catppuccin-latte' : 'tokyo-night';
  document.documentElement.dataset.theme = theme;
})();
