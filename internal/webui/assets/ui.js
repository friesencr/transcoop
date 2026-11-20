'use strict';
const themes = ['tokyo-night', 'catppuccin', 'gruvbox', 'nord', 'everforest', 'kanagawa', 'matte-black', 'rose-pine', 'catppuccin-latte', 'flexoki-light'];
function setTheme(name) {
  document.documentElement.dataset.theme = name;
  try { localStorage.setItem('transcoop-theme', name); } catch (e) {}
  const button = document.querySelector('#theme-toggle');
  if (button) button.textContent = name + '/';
}
function cycleTheme(step) {
  const i = themes.indexOf(document.documentElement.dataset.theme);
  setTheme(themes[(i + step + themes.length) % themes.length]);
}
const toggle = document.querySelector('#theme-toggle');
if (toggle) {
  toggle.hidden = false;
  toggle.textContent = document.documentElement.dataset.theme + '/';
  toggle.addEventListener('click', e => cycleTheme(e.shiftKey ? -1 : 1));
}
document.addEventListener('keydown', e => {
  if (e.key.toLowerCase() !== 't' || e.ctrlKey || e.metaKey || e.altKey) return;
  if (e.target.closest('input, select, textarea, [contenteditable]')) return;
  cycleTheme(e.shiftKey ? -1 : 1);
});

// Show exact times in the browser's own time zone.
function localTimes(root) {
  root.querySelectorAll('time[datetime]').forEach(el => {
    const d = new Date(el.dateTime);
    if (!isNaN(d)) el.title = d.toLocaleString();
  });
}
localTimes(document);

// Confirmation can sit on the form or on the button that submitted it.
document.addEventListener('submit', e => {
  const message = e.submitter?.dataset.confirm || e.target.dataset.confirm;
  if (message && !window.confirm(message)) e.preventDefault();
});

function filterLogs() {
  const query = (document.querySelector('#log-search')?.value || '').toLowerCase();
  const follow = document.querySelector('#log-follow')?.checked;
  document.querySelectorAll('.log').forEach(el => {
    if (el._raw === undefined) el._raw = el.textContent;
    el.textContent = query ? el._raw.split('\n').filter(line => line.toLowerCase().includes(query)).join('\n') : el._raw;
    if (follow) el.scrollTop = el.scrollHeight;
  });
}
document.addEventListener('input', e => { if (e.target.id === 'log-search') filterLogs(); });
document.addEventListener('change', e => { if (e.target.id === 'log-follow') filterLogs(); });

// Live refresh: keep scroll and focus, and never swap away a control mid-use.
const scrolls = new Map();
let focused = '';
let caret = null;
document.addEventListener('htmx:beforeSwap', e => {
  if (e.detail.target.id !== 'live') return;
  const active = document.activeElement;
  const keep = active && (active.classList.contains('log') || active.closest('[hx-preserve]'));
  if (e.detail.target.contains(active) && !keep) { e.detail.shouldSwap = false; return; }
  focused = keep && active.id ? active.id : '';
  caret = active && 'selectionStart' in active ? [active.selectionStart, active.selectionEnd] : null;
  document.querySelectorAll('.log').forEach(el => scrolls.set(el.id, el.scrollTop));
});
document.addEventListener('htmx:afterSwap', e => {
  filterLogs();
  localTimes(document);
  if (!document.querySelector('#log-follow')?.checked) document.querySelectorAll('.log').forEach(el => { el.scrollTop = scrolls.get(el.id) || 0; });
  if (focused) {
    const el = document.getElementById(focused);
    el?.focus({ preventScroll: true });
    if (el && caret) try { el.setSelectionRange(caret[0], caret[1]); } catch (err) {}
  }
  const status = document.querySelector('#poll-status');
  if (status) status.textContent = '';
});
document.addEventListener('htmx:responseError', () => { const el = document.querySelector('#poll-status'); if (el) el.textContent = 'Refresh failed. Displayed information may be stale.'; });
document.addEventListener('htmx:sendError', () => { const el = document.querySelector('#poll-status'); if (el) el.textContent = 'Connection lost. Displayed information may be stale.'; });
