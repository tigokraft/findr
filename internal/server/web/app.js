'use strict';

/* ───────────── helpers ───────────── */
const $ = (id) => document.getElementById(id);
const esc = (s) => String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
// A value placed inside '...' in an inline handler attribute.
const jsq = (s) => esc(String(s == null ? '' : s).replace(/\\/g, '\\\\').replace(/'/g, "\\'"));
const ic = (name, cls) => '<svg class="i' + (cls ? ' ' + cls : '') + '" aria-hidden="true"><use href="#i-' + name + '"/></svg>';
const plural = (n, word, many) => n + ' ' + (n === 1 ? word : (many || word + 's'));

const PH_ART = 'data:image/svg+xml;utf8,' + encodeURIComponent(
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 200">' +
  '<defs><radialGradient id="g" cx=".25" cy=".2" r="1"><stop offset="0" stop-color="#3a3b42"/><stop offset="1" stop-color="#1c1d21"/></radialGradient></defs>' +
  '<rect width="200" height="200" fill="url(#g)"/><circle cx="100" cy="100" r="46" fill="#121316"/>' +
  '<circle cx="100" cy="100" r="30" fill="none" stroke="#26272c" stroke-width="1.5"/><circle cx="100" cy="100" r="9" fill="#3a3b42"/></svg>');
const art = (url) => esc(url || PH_ART);
function imgFail(el) { el.onerror = null; el.src = PH_ART; }
const imgTag = (url, cls, alt) => '<img class="' + cls + '" src="' + art(url) + '" alt="' + esc(alt || '') + '" loading="lazy" onerror="imgFail(this)">';

const STATUS = {
  completed: ['mint', 'Downloaded'],
  downloaded: ['sky', 'Downloaded'],
  missing: ['', 'Missing'],
  pending: ['peach', 'Queued'],
  'pending-download': ['peach', 'Queued'],
  downloading: ['sky', 'Downloading'],
  verifying: ['sky', 'Verifying'],
  importing: ['sky', 'Importing'],
  failed: ['rose', 'Failed']
};
function statusChip(status) {
  const s = status || 'missing';
  const [tone, label] = STATUS[s] || ['', s];
  return '<span class="chip' + (tone ? ' chip-' + tone : '') + '"><span class="dot"></span>' + esc(label) + '</span>';
}

function emptyState(title, sub, action, isError) {
  return '<div class="empty' + (isError ? ' error' : '') + '"><div class="empty-art" aria-hidden="true"></div>' +
    '<div class="empty-title">' + title + '</div>' +
    (sub ? '<div class="empty-sub">' + sub + '</div>' : '') +
    (action || '') + '</div>';
}

function skeletonFolders(n) {
  let h = '';
  for (let i = 0; i < n; i++) h += '<div class="folder skeleton" aria-hidden="true"><div class="folder-back"></div><div class="folder-front"></div></div>';
  return h;
}

/* ───────────── state ───────────── */
let currentConfig = null;
let allArtists = [];
let currentView = 'artists';
let currentSettingsTab = 'storage';
let searchProvider = 'deezer';

/* ───────────── toasts ───────────── */
function showToast(message, type) {
  type = type || 'info';
  const icon = type === 'success' ? 'check' : type === 'error' ? 'alert' : 'info';
  const t = document.createElement('div');
  t.className = 'toast ' + type;
  t.setAttribute('role', type === 'error' ? 'alert' : 'status');
  t.innerHTML = '<span class="toast-icon">' + ic(icon) + '</span><span></span>';
  t.lastChild.textContent = message;
  $('toastContainer').appendChild(t);
  setTimeout(() => { t.classList.add('out'); setTimeout(() => t.remove(), 260); }, 4200);
}

/* ───────────── shell: sidebar, menu, shortcuts ───────────── */
function toggleSidebar() {
  const collapsed = document.documentElement.classList.toggle('sb-collapsed');
  const b = $('sbToggle');
  b.setAttribute('aria-expanded', String(!collapsed));
  b.setAttribute('aria-label', collapsed ? 'Expand sidebar' : 'Collapse sidebar');
  try { localStorage.setItem('findr.sidebar', collapsed ? 'collapsed' : 'open'); } catch (e) {}
}

function toggleMenu(force) {
  const menu = $('mainMenu');
  const btn = $('menuBtn');
  const open = typeof force === 'boolean' ? force : menu.hidden;
  menu.hidden = !open;
  btn.setAttribute('aria-expanded', String(open));
  if (open) { const first = menu.querySelector('.menu-item'); if (first) first.focus(); }
}
function menuRun(fn) { toggleMenu(false); fn(); }

document.addEventListener('click', (e) => {
  const menu = $('mainMenu');
  if (!menu.hidden && !e.target.closest('.menu-wrap')) toggleMenu(false);
});
document.addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); focusCatalogSearch(); }
  if (e.key === 'Escape') {
    if (!$('mainMenu').hidden) { toggleMenu(false); $('menuBtn').focus(); }
  }
  if (!$('mainMenu').hidden && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
    const items = Array.from(document.querySelectorAll('#mainMenu .menu-item'));
    const i = items.indexOf(document.activeElement);
    const next = e.key === 'ArrowDown' ? (i + 1) % items.length : (i - 1 + items.length) % items.length;
    items[next].focus();
    e.preventDefault();
  }
});

function focusCatalogSearch() {
  go('search');
  setTimeout(() => { const inp = $('searchQueryInput'); if (inp) { inp.focus(); inp.select(); } }, 60);
}

/* ───────────── routing ───────────── */
const TITLES = {
  artists: 'Library',
  artist: 'Artist',
  search: 'Search & Add',
  activity: 'Queue & History',
  import: 'Manual Import',
  settings: 'Settings',
  system: 'System & Disks'
};

function go(hash) {
  if (window.location.hash === '#' + hash) { handleRoute(); return; }
  window.location.hash = hash;
}

function setCrumbs(parts) {
  $('crumbs').innerHTML = (parts || []).map((p, i) =>
    (i ? '<span aria-hidden="true">/</span>' : '') + (p.href ? '<a href="' + esc(p.href) + '">' + esc(p.label) + '</a>' : '<span>' + esc(p.label) + '</span>')
  ).join('');
}

function setTitle(text) {
  $('pageTitle').textContent = text;
  document.title = text === 'Library' ? 'findr' : text + ' · findr';
}

function navigateTo(viewName) {
  currentView = viewName;
  document.querySelectorAll('.view').forEach((el) => el.classList.toggle('active', el.id === 'view-' + viewName));

  const navKey = viewName === 'artist' ? (isArtistPreviewMode ? 'search' : 'artists') : viewName;
  document.querySelectorAll('.nav-item[data-view], .tab[data-view]').forEach((el) => {
    const on = el.dataset.view === navKey;
    el.classList.toggle('active', on);
    if (on) el.setAttribute('aria-current', 'page'); else el.removeAttribute('aria-current');
  });
  $('navSettingsGroup').classList.toggle('open', viewName === 'settings');

  setTitle(TITLES[viewName] || 'findr');
  setCrumbs([]);
  if (viewName !== 'artist') $('artistBatchToolbar').hidden = true;

  if (viewName === 'artists') loadLibraryArtists();
  if (viewName === 'activity') { loadLiveQueue(); loadDownloadHistory(); }
  if (viewName === 'import') initManualImport();
  if (viewName === 'settings') loadSettings();
  if (viewName === 'system') loadSystemOverview();
  if (viewName === 'search' && !$('searchResultsGrid').innerHTML) {
    $('searchResultsGrid').innerHTML = emptyState('Search to see artists', 'Type a name above. Open a result to preview its releases before you add it.');
  }
}

function handleRoute() {
  let hash = window.location.hash.replace(/^#/, '');
  if (!hash) {
    const path = window.location.pathname;
    if (path.startsWith('/artist/')) hash = 'artist/' + path.replace('/artist/', '');
    else if (path.startsWith('/search')) hash = 'search';
    else if (path.startsWith('/activity') || path.startsWith('/queue') || path.startsWith('/history')) hash = 'activity';
    else if (path.startsWith('/import')) hash = 'import';
    else if (path.startsWith('/settings')) hash = 'settings';
    else if (path.startsWith('/system')) hash = 'system';
  }

  if (hash.startsWith('artist/preview-')) {
    if (!isArtistPreviewMode || !currentArtistData) go('search');
    return;
  }
  if (hash.startsWith('artist/')) {
    const id = decodeURIComponent(hash.slice('artist/'.length));
    if (id) { openArtistPage(id, false); return; }
  }
  if (hash.startsWith('settings')) {
    const tab = hash.split('/')[1] || currentSettingsTab || 'storage';
    if (currentView !== 'settings') navigateTo('settings');
    openSettingsTab(tab, true);
    return;
  }
  if (['artists', 'search', 'activity', 'import', 'system'].includes(hash)) navigateTo(hash);
  else navigateTo('artists');
}

window.addEventListener('DOMContentLoaded', () => {
  if (document.documentElement.classList.contains('sb-collapsed')) {
    $('sbToggle').setAttribute('aria-expanded', 'false');
    $('sbToggle').setAttribute('aria-label', 'Expand sidebar');
  }
  handleRoute();
  refreshQueueBadge();
});
window.addEventListener('hashchange', handleRoute);

/* ───────────── Library ───────────── */
function setBusy(btn, busy, label) {
  if (!btn) return;
  if (busy) {
    btn.dataset.html = btn.innerHTML;
    btn.disabled = true;
    btn.innerHTML = '<svg class="i spin" aria-hidden="true" viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-6.22-8.56"/></svg><span>' + esc(label) + '</span>';
  } else {
    btn.disabled = false;
    if (btn.dataset.html) btn.innerHTML = btn.dataset.html;
  }
}

async function downloadAllArtistsReleases() {
  if (!confirm('Queue every missing release of every monitored artist?')) return;
  const btn = $('btn_download_all_artists');
  setBusy(btn, true, 'Queueing');
  showToast('Checking monitored artists for missing releases', 'info');
  try {
    const res = await fetch('/api/artists/download-all', { method: 'POST' });
    const data = await res.json();
    if (!res.ok) throw new Error(data.message || data.error || 'Could not queue downloads');
    showToast(data.message || 'Missing releases queued', 'success');
    await loadLibraryArtists();
    refreshQueueBadge();
  } catch (err) {
    showToast('Download all failed: ' + err.message, 'error');
  } finally {
    setBusy(btn, false);
  }
}

async function scanLibraryArtists() {
  const btn = $('btn_scan_library');
  setBusy(btn, true, 'Scanning');
  showToast('Reading the music folder for artists', 'info');
  try {
    const res = await fetch('/api/library/scan', { method: 'POST' });
    const data = await res.json();
    if (!res.ok) throw new Error(data.message || data.error || 'Scan failed');
    showToast(data.message || 'Library scan finished', 'success');
    await loadLibraryArtists();
  } catch (err) {
    showToast('Library scan failed: ' + err.message, 'error');
  } finally {
    setBusy(btn, false);
  }
}

let libraryLoaded = false;
async function loadLibraryArtists() {
  if (!libraryLoaded) $('libraryGrid').innerHTML = skeletonFolders(8);
  try {
    const res = await fetch('/api/artists');
    allArtists = (await res.json()) || [];
    libraryLoaded = true;
    $('navArtistCount').textContent = allArtists.length || '';
    renderFilteredArtists();
  } catch (err) {
    $('libraryGrid').innerHTML = emptyState('The library did not load', esc(err.message), '<button class="btn" type="button" onclick="loadLibraryArtists()">' + ic('refresh') + 'Try again</button>', true);
  }
}

function renderFilteredArtists() {
  const q = ($('libraryFilter').value || '').toLowerCase().trim();
  const filtered = allArtists.filter((a) => (a.name || '').toLowerCase().includes(q));
  const monitored = allArtists.filter((a) => a.monitored).length;
  $('artistCountSummary').textContent = q
    ? filtered.length + ' of ' + allArtists.length
    : plural(allArtists.length, 'artist') + ' · ' + monitored + ' monitored';

  const grid = $('libraryGrid');
  if (!filtered.length) {
    grid.innerHTML = allArtists.length === 0
      ? emptyState('No artists yet', 'Search the catalog to follow an artist, or import the ones already on disk.',
          '<div style="display:flex;gap:8px;flex-wrap:wrap;justify-content:center"><button class="btn btn-primary" type="button" onclick="focusCatalogSearch()">' + ic('search') + 'Search the catalog</button>' +
          '<button class="btn" type="button" onclick="scanLibraryArtists()">' + ic('layers') + 'Import from disk</button></div>')
      : emptyState('No artist matches “' + esc(q) + '”', 'Check the spelling or clear the filter.');
    return;
  }

  grid.innerHTML = filtered.map((a) => {
    const open = "openArtistPage('" + jsq(a.id) + "')";
    return '<div class="folder" onclick="' + open + '">' +
      '<div class="folder-back">' + imgTag(a.image_url, '', a.name) + '</div>' +
      '<div class="folder-front">' +
        '<div><h3 class="folder-name"><a href="#artist/' + esc(encodeURIComponent(a.id)) + '" onclick="event.stopPropagation()">' + esc(a.name) + '</a></h3>' +
        '<p class="folder-meta">' + esc((a.source || 'indexer')) + '</p></div>' +
        '<div class="folder-row">' +
          (a.monitored ? '<span class="chip chip-mint"><span class="dot"></span>Monitored</span>' : '<span class="chip"><span class="dot"></span>Paused</span>') +
          '<div class="folder-actions" onclick="event.stopPropagation()">' +
            '<button class="btn btn-icon btn-sm hide-sm" type="button" title="Remove ' + esc(a.name) + '" aria-label="Remove ' + esc(a.name) + '" onclick="deleteArtist(\'' + jsq(a.id) + '\', \'' + jsq(a.name) + '\')">' + ic('trash') + '</button>' +
            '<button class="btn btn-icon btn-sm btn-mint" type="button" title="Download every release" aria-label="Download every release by ' + esc(a.name) + '" onclick="downloadArtistAll(\'' + jsq(a.id) + '\', \'' + jsq(a.name) + '\')">' + ic('download') + '</button>' +
          '</div>' +
        '</div>' +
      '</div>' +
    '</div>';
  }).join('');
}

/* ───────────── Artist page ───────────── */
let currentArtistData = null;
let currentArtistAlbums = [];
let currentArtistFilterCat = 'all';
let currentArtistFilterText = '';
let currentArtistViewMode = 'list';
let isArtistPreviewMode = false;
let currentPreviewPayload = null;
let artistPreviousView = 'artists';
const selectedAlbumIDs = new Set();

function resetArtistView(name, loadingText) {
  $('artistHeroName').textContent = name;
  $('artistHeroOverview').hidden = true;
  $('artistHeroActions').innerHTML = '';
  $('artistCategoryTabs').innerHTML = '';
  $('artistHeroImg').src = PH_ART;
  $('artistHeroBackdrop').style.backgroundImage = 'none';
  $('artistReleasesList').innerHTML = '<div class="tracks-note"><span class="pulse" style="display:inline-block;vertical-align:middle;margin-right:10px"></span>' + esc(loadingText) + '</div>';
  ['artistStatTotal', 'artistStatDownloaded', 'artistStatMissing', 'artistStatMonitored'].forEach((id) => { $(id).textContent = '–'; });
  $('artistStatRing').style.setProperty('--p', 0);
  $('artistStatPct').textContent = '';
}

async function openArtistPage(artistID, pushHash) {
  if (pushHash !== false) { go('artist/' + encodeURIComponent(artistID)); return; }
  if (currentView !== 'artist') artistPreviousView = currentView || 'artists';
  isArtistPreviewMode = false;
  currentPreviewPayload = null;
  navigateTo('artist');
  resetArtistView('Loading', 'Loading the discography');
  window.scrollTo(0, 0);

  try {
    const res = await fetch('/api/artists/' + encodeURIComponent(artistID));
    if (!res.ok) throw new Error('This artist is not in the library');
    const data = await res.json();
    currentArtistData = data.artist;
    currentArtistAlbums = data.albums || [];
    resetArtistFilters();
    renderDedicatedArtistPage();
  } catch (err) {
    $('artistReleasesList').innerHTML = emptyState('The artist did not load', esc(err.message),
      '<button class="btn" type="button" onclick="navigateBackFromArtist()">' + ic('chev-left') + 'Back to library</button>', true);
  }
}

async function openArtistPreviewPage(sourceID, jsonArtistEncoded) {
  artistPreviousView = 'search';
  isArtistPreviewMode = true;
  currentPreviewPayload = jsonArtistEncoded;
  const artist = JSON.parse(decodeURIComponent(jsonArtistEncoded));
  currentArtistData = { id: sourceID, source_id: sourceID, source: artist.source || searchProvider, name: artist.name, image_url: artist.image_url, monitored: false, isPreview: true };
  currentArtistAlbums = [];

  window.location.hash = 'artist/preview-' + encodeURIComponent(sourceID);
  navigateTo('artist');
  resetArtistView(artist.name, 'Fetching the catalog preview');
  renderArtistChrome();
  window.scrollTo(0, 0);

  try {
    const res = await fetch('/api/search/preview?artist_id=' + encodeURIComponent(sourceID) + '&provider=' + encodeURIComponent(currentArtistData.source));
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const albums = (await res.json()) || [];
    currentArtistAlbums = albums.map((alb, idx) => Object.assign({}, alb, { id: alb.id || ('preview-' + idx), status: 'missing', monitored: false }));
    resetArtistFilters();
    renderDedicatedArtistPage();
  } catch (err) {
    $('artistReleasesList').innerHTML = emptyState('The preview did not load', esc(err.message),
      '<button class="btn" type="button" onclick="go(\'search\')">' + ic('chev-left') + 'Back to search</button>', true);
  }
}

function resetArtistFilters() {
  selectedAlbumIDs.clear();
  currentArtistFilterCat = 'all';
  currentArtistFilterText = '';
  $('artistReleaseFilterInput').value = '';
}

function navigateBackFromArtist() {
  go(isArtistPreviewMode || artistPreviousView === 'search' ? 'search' : 'artists');
}

function setArtistViewMode(mode) {
  currentArtistViewMode = mode;
  $('btnToggleList').classList.toggle('active', mode === 'list');
  $('btnToggleGrid').classList.toggle('active', mode === 'grid');
  renderArtistReleases();
}
function setArtistFilterCat(cat) { currentArtistFilterCat = cat; renderDedicatedArtistPage(); }
function onArtistReleaseFilter(val) { currentArtistFilterText = val; renderArtistReleases(); }

function renderArtistChrome() {
  const a = currentArtistData;
  const backTo = isArtistPreviewMode ? 'Search' : 'Library';
  $('artistBackButtonLabel').textContent = backTo;
  setTitle(isArtistPreviewMode ? 'Preview' : a.name);
  setCrumbs([{ label: backTo, href: isArtistPreviewMode ? '#search' : '#artists' }, { label: a.name }]);

  const img = a.image_url || PH_ART;
  $('artistHeroBackdrop').style.backgroundImage = 'url("' + img.replace(/"/g, '%22') + '")';
  const hi = $('artistHeroImg');
  hi.onerror = () => imgFail(hi);
  hi.src = img;
  hi.alt = a.name;
  $('artistHeroName').textContent = a.name;
  $('artistHeroSourceTag').textContent = a.source || 'indexer';

  const mon = $('artistHeroMonitoredPill');
  $('artistHeroPreviewBadge').hidden = !isArtistPreviewMode;
  mon.hidden = isArtistPreviewMode;
  mon.className = 'chip ' + (a.monitored ? 'chip-mint' : '');
  mon.innerHTML = '<span class="dot"></span>' + (a.monitored ? 'Monitored' : 'Paused');

  const bio = $('artistHeroOverview');
  bio.hidden = !a.overview;
  bio.textContent = a.overview || '';

  const actions = $('artistHeroActions');
  if (isArtistPreviewMode) {
    const p = jsq(currentPreviewPayload);
    actions.innerHTML =
      '<button class="btn btn-mint" type="button" onclick="addArtistFromSearch(\'' + p + '\', true)">' + ic('download') + 'Add and download all</button>' +
      '<button class="btn" type="button" onclick="addArtistFromSearch(\'' + p + '\', false)">' + ic('plus') + 'Add and monitor</button>';
  } else {
    actions.innerHTML =
      '<button class="btn btn-mint" type="button" onclick="downloadCurrentArtistAll()">' + ic('download') + 'Download discography</button>' +
      '<button class="btn" type="button" id="btnArtistToggleMonitored" onclick="toggleCurrentArtistMonitored()">' + ic(a.monitored ? 'eye-off' : 'eye') + (a.monitored ? 'Stop monitoring' : 'Monitor artist') + '</button>' +
      '<button class="btn btn-icon" type="button" onclick="reloadCurrentArtistPage()" aria-label="Refresh discography" title="Refresh discography">' + ic('refresh') + '</button>' +
      '<button class="btn btn-icon btn-danger" type="button" onclick="deleteCurrentArtist()" aria-label="Remove artist" title="Remove artist">' + ic('trash') + '</button>';
  }
}

function typeOf(a) { return (a.release_type || 'album').toLowerCase(); }

function renderDedicatedArtistPage() {
  if (!currentArtistData) return;
  renderArtistChrome();

  const total = currentArtistAlbums.length;
  const done = currentArtistAlbums.filter((a) => a.status === 'completed').length;
  const missing = currentArtistAlbums.filter((a) => !a.status || a.status === 'missing' || a.status === 'pending-download').length;
  const monitored = currentArtistAlbums.filter((a) => a.monitored).length;
  const pct = total ? Math.round((done / total) * 100) : 0;
  $('artistStatTotal').textContent = total;
  $('artistStatDownloaded').textContent = done;
  $('artistStatMissing').textContent = missing;
  $('artistStatMonitored').textContent = monitored;
  $('artistStatRing').style.setProperty('--p', pct);
  $('artistStatPct').textContent = pct + '%';

  const counts = { all: total, album: 0, ep: 0, single: 0, other: 0 };
  currentArtistAlbums.forEach((a) => { const t = typeOf(a); if (t in counts && t !== 'all') counts[t]++; else counts.other++; });
  const tabs = [['all', 'All'], ['album', 'Albums'], ['ep', 'EPs'], ['single', 'Singles']];
  if (counts.other) tabs.push(['other', 'Other']);
  $('artistCategoryTabs').innerHTML = tabs.map(([k, label]) =>
    '<button type="button" role="tab" aria-selected="' + (currentArtistFilterCat === k) + '" class="' + (currentArtistFilterCat === k ? 'active' : '') + '" onclick="setArtistFilterCat(\'' + k + '\')">' + label + '<span class="count">' + counts[k] + '</span></button>'
  ).join('');

  renderArtistReleases();
}

function getFilteredArtistAlbums() {
  let list = currentArtistAlbums;
  const cat = currentArtistFilterCat;
  if (cat === 'other') list = list.filter((a) => !['album', 'ep', 'single'].includes(typeOf(a)));
  else if (cat !== 'all') list = list.filter((a) => typeOf(a) === cat);
  const q = currentArtistFilterText.toLowerCase().trim();
  if (q) list = list.filter((a) => (a.title || '').toLowerCase().includes(q) || String(a.year || '').includes(q));
  return list;
}

function renderArtistBatchToolbar(displayed) {
  const bar = $('artistBatchToolbar');
  const wrap = $('selectAllWrap');
  if (isArtistPreviewMode || currentView !== 'artist') { bar.hidden = true; wrap.hidden = true; return; }
  wrap.hidden = displayed.length === 0;
  $('selectAllPageCb').checked = displayed.length > 0 && displayed.every((a) => selectedAlbumIDs.has(a.id));

  const n = selectedAlbumIDs.size;
  bar.hidden = n === 0;
  if (!n) return;
  bar.innerHTML =
    '<span class="batch-count">' + n + ' selected</span>' +
    '<button class="btn btn-sm btn-mint" type="button" onclick="executeBatchAlbumActionFromPage(\'download\')">' + ic('download') + '<span class="btn-text">Download</span></button>' +
    '<button class="btn btn-sm" type="button" onclick="executeBatchAlbumActionFromPage(\'monitor\')" title="Monitor">' + ic('eye') + '<span class="btn-text">Monitor</span></button>' +
    '<button class="btn btn-sm" type="button" onclick="executeBatchAlbumActionFromPage(\'unmonitor\')" title="Stop monitoring">' + ic('eye-off') + '<span class="btn-text">Unmonitor</span></button>' +
    '<button class="btn btn-sm btn-icon btn-ghost" type="button" onclick="clearAlbumSelection()" aria-label="Clear selection" title="Clear selection">' + ic('x') + '</button>';
}

function clearAlbumSelection() { selectedAlbumIDs.clear(); renderArtistReleases(); }

function releaseMeta(alb) {
  const tracks = (alb.tracks && alb.tracks.length) ? alb.tracks.length : (alb.total_tracks || 0);
  const parts = [alb.year || '—', typeOf(alb)];
  if (tracks) parts.push(plural(tracks, 'track'));
  return parts.map(esc).join(' · ');
}

function renderArtistReleases() {
  const displayed = getFilteredArtistAlbums();
  renderArtistBatchToolbar(displayed);
  const box = $('artistReleasesList');

  if (!displayed.length) {
    box.innerHTML = currentArtistAlbums.length === 0
      ? emptyState('No releases indexed', 'The indexer returned nothing for this artist yet. Refresh later or try the other catalog.')
      : emptyState('No release matches', 'Clear the filter or pick another type.');
    return;
  }

  const preview = isArtistPreviewMode;
  if (currentArtistViewMode === 'grid') {
    box.innerHTML = '<div class="albums">' + displayed.map((alb) => {
      const id = jsq(alb.id);
      const sel = selectedAlbumIDs.has(alb.id);
      return '<div class="album' + (sel ? ' selected' : '') + '" id="grid-card-' + esc(alb.id) + '">' +
        '<div class="album-art">' + imgTag(alb.cover_url, '', alb.title) +
          (preview ? '' : statusChip(alb.status)) +
          (preview ? '' : '<label class="album-cb" title="Select"><input type="checkbox" aria-label="Select ' + esc(alb.title) + '" ' + (sel ? 'checked' : '') + ' onchange="toggleSelectAlbumInPage(\'' + id + '\', this.checked)"></label>') +
        '</div>' +
        '<div class="album-body"><div class="album-title" title="' + esc(alb.title) + '">' + esc(alb.title) + '</div><div class="album-sub">' + releaseMeta(alb) + '</div>' +
          '<div class="album-actions">' +
            (preview ? '' : '<button class="btn btn-sm" type="button" onclick="downloadSingleAlbumFromPage(\'' + id + '\', \'' + jsq(alb.title) + '\')">' + ic('download') + (alb.status === 'completed' ? 'Again' : 'Get') + '</button>') +
            '<button class="btn btn-sm btn-icon" type="button" id="page-chevron-' + esc(alb.id) + '" aria-expanded="false" aria-label="Show tracks" title="Tracks" onclick="toggleAlbumTracksFromPage(\'' + id + '\')">' + ic('chev-down', 'chev') + '</button>' +
          '</div>' +
        '</div>' +
        '<div class="tracks" id="page-tracklist-' + esc(alb.id) + '"></div>' +
      '</div>';
    }).join('') + '</div>';
    return;
  }

  box.innerHTML = '<div class="releases">' + displayed.map((alb) => {
    const id = jsq(alb.id);
    const sel = selectedAlbumIDs.has(alb.id);
    return '<div class="release' + (sel ? ' selected' : '') + '" id="page-card-' + esc(alb.id) + '">' +
      '<div class="release-head' + (preview ? ' no-cb' : '') + '" onclick="toggleAlbumTracksFromPage(\'' + id + '\')">' +
        (preview ? '' : '<input type="checkbox" aria-label="Select ' + esc(alb.title) + '" ' + (sel ? 'checked' : '') + ' onclick="event.stopPropagation()" onchange="toggleSelectAlbumInPage(\'' + id + '\', this.checked)">') +
        imgTag(alb.cover_url, 'thumb thumb-lg', '') +
        '<div style="min-width:0"><div class="release-title"><span>' + esc(alb.title) + '</span></div>' +
          '<div class="release-sub">' + releaseMeta(alb) + (alb.release_date ? ' · ' + esc(alb.release_date) : '') + '</div></div>' +
        '<div class="release-ctl" onclick="event.stopPropagation()">' +
          (preview ? '' :
            '<label class="switch-label" title="Monitor this release"><input type="checkbox" ' + (alb.monitored ? 'checked' : '') + ' onchange="toggleAlbumMonitoredFromPage(\'' + id + '\', this.checked)"><span>Monitor</span></label>' +
            statusChip(alb.status) +
            '<button class="btn btn-sm grow" type="button" onclick="downloadSingleAlbumFromPage(\'' + id + '\', \'' + jsq(alb.title) + '\')">' + ic('download') + (alb.status === 'completed' ? 'Download again' : 'Download') + '</button>') +
          '<button class="btn btn-sm btn-icon' + (preview ? ' grow' : '') + '" type="button" id="page-chevron-' + esc(alb.id) + '" aria-expanded="false" aria-label="Show tracks" title="Tracks" onclick="toggleAlbumTracksFromPage(\'' + id + '\')">' + ic('chev-down', 'chev') + '</button>' +
        '</div>' +
      '</div>' +
      '<div class="tracks" id="page-tracklist-' + esc(alb.id) + '"></div>' +
    '</div>';
  }).join('') + '</div>';
}

function toggleSelectAllInPage(checked) {
  getFilteredArtistAlbums().forEach((a) => { if (checked) selectedAlbumIDs.add(a.id); else selectedAlbumIDs.delete(a.id); });
  renderArtistReleases();
}

function toggleSelectAlbumInPage(id, checked) {
  if (checked) selectedAlbumIDs.add(id); else selectedAlbumIDs.delete(id);
  const card = $('page-card-' + id) || $('grid-card-' + id);
  if (card) card.classList.toggle('selected', checked);
  renderArtistBatchToolbar(getFilteredArtistAlbums());
}

async function executeBatchAlbumActionFromPage(action) {
  if (!selectedAlbumIDs.size) { showToast('Select releases first', 'info'); return; }
  const ids = Array.from(selectedAlbumIDs);
  try {
    const res = await fetch('/api/albums/batch', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: action, album_ids: ids })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.message || 'Batch action failed');
    showToast(data.message || 'Done for ' + plural(ids.length, 'release'), 'success');
    currentArtistAlbums.forEach((a) => {
      if (!selectedAlbumIDs.has(a.id)) return;
      if (action === 'monitor') a.monitored = true;
      if (action === 'unmonitor') a.monitored = false;
      if (action === 'download') a.status = 'pending';
    });
    if (action === 'download') { selectedAlbumIDs.clear(); refreshQueueBadge(); }
    renderDedicatedArtistPage();
  } catch (err) {
    showToast(err.message, 'error');
  }
}

async function downloadSingleAlbumFromPage(albumID, albumTitle) {
  try {
    const res = await fetch('/api/albums/' + encodeURIComponent(albumID) + '/download', { method: 'POST' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    showToast('Queued ' + albumTitle, 'success');
    const alb = currentArtistAlbums.find((a) => a.id === albumID);
    if (alb && currentView === 'artist') { alb.status = 'pending'; renderDedicatedArtistPage(); }
    if (currentView === 'activity') { loadLiveQueue(); loadDownloadHistory(); }
    refreshQueueBadge();
  } catch (err) {
    showToast('Could not queue ' + albumTitle + ': ' + err.message, 'error');
  }
}

async function toggleAlbumMonitoredFromPage(albumID, monitored) {
  try {
    const res = await fetch('/api/albums/' + encodeURIComponent(albumID), {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ monitored: monitored })
    });
    if (!res.ok) throw new Error('Could not update the release');
    const alb = currentArtistAlbums.find((a) => a.id === albumID);
    if (alb) alb.monitored = monitored;
    showToast(monitored ? 'Release monitored' : 'Release no longer monitored', 'success');
    renderDedicatedArtistPage();
  } catch (err) {
    showToast(err.message, 'error');
    renderDedicatedArtistPage();
  }
}

async function toggleCurrentArtistMonitored() {
  if (!currentArtistData || !currentArtistData.id) return;
  const next = !currentArtistData.monitored;
  try {
    const res = await fetch('/api/artists/' + encodeURIComponent(currentArtistData.id), {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ monitored: next })
    });
    if (!res.ok) throw new Error('Could not update the artist');
    currentArtistData.monitored = next;
    showToast(next ? 'Monitoring ' + currentArtistData.name : 'Stopped monitoring ' + currentArtistData.name, 'success');
    renderDedicatedArtistPage();
    libraryLoaded = false;
  } catch (err) {
    showToast(err.message, 'error');
  }
}

async function downloadCurrentArtistAll() {
  if (!currentArtistData || !currentArtistData.id) return;
  try {
    const res = await fetch('/api/artists/' + encodeURIComponent(currentArtistData.id) + '/download-all', { method: 'POST' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    showToast('Downloading the discography of ' + currentArtistData.name, 'success');
    currentArtistAlbums.forEach((a) => { if (a.status !== 'completed') a.status = 'pending'; });
    renderDedicatedArtistPage();
    refreshQueueBadge();
  } catch (err) {
    showToast('Download failed: ' + err.message, 'error');
  }
}

async function deleteCurrentArtist() {
  if (!currentArtistData || !currentArtistData.id) return;
  await deleteArtist(currentArtistData.id, currentArtistData.name);
}

async function reloadCurrentArtistPage() {
  if (currentArtistData && currentArtistData.id && !isArtistPreviewMode) {
    await openArtistPage(currentArtistData.id, false);
    showToast('Refreshed ' + currentArtistData.name, 'info');
  }
}

async function toggleAlbumTracksFromPage(albumID) {
  const el = $('page-tracklist-' + albumID);
  const btn = $('page-chevron-' + albumID);
  if (!el) return;
  const opening = !el.classList.contains('open');
  el.classList.toggle('open', opening);
  if (btn) { btn.classList.toggle('open-chev', opening); btn.setAttribute('aria-expanded', String(opening)); btn.setAttribute('aria-label', opening ? 'Hide tracks' : 'Show tracks'); }
  if (!opening) return;

  const alb = currentArtistAlbums.find((a) => a.id === albumID);
  if (!alb) return;
  if (!alb.tracks || !alb.tracks.length) {
    if (isArtistPreviewMode) { renderAlbumTracklist(albumID, [], el); return; }
    el.innerHTML = '<div class="tracks-note">Fetching tracks</div>';
    try {
      const res = await fetch('/api/albums/' + encodeURIComponent(albumID));
      const detailed = await res.json();
      alb.tracks = detailed.tracks || [];
      alb.total_tracks = detailed.total_tracks || alb.tracks.length;
    } catch (err) {
      el.innerHTML = '<div class="tracks-note" style="color:var(--rose)">Tracks did not load: ' + esc(err.message) + '</div>';
      return;
    }
  }
  renderAlbumTracklist(albumID, alb.tracks, el);
}

function renderAlbumTracklist(albumID, tracks, targetEl) {
  const el = targetEl || $('page-tracklist-' + albumID);
  if (!el) return;
  if (!tracks || !tracks.length) { el.innerHTML = '<div class="tracks-note">No track list in the catalog.</div>'; return; }
  el.innerHTML = '<ol class="tracklist">' + tracks.map((t, idx) => {
    const n = t.track_number || (idx + 1);
    const num = ((t.disc_number && t.disc_number > 1) ? t.disc_number + '-' : '') + (n < 10 ? '0' + n : n);
    return '<li class="track"><span class="t-no">' + esc(num) + '</span>' +
      '<span class="t-title"><span>' + esc(t.title) + '</span>' + (t.explicit ? '<span class="e-tag" title="Explicit">E</span>' : '') + '</span>' +
      '<span class="t-isrc">' + esc(t.isrc || '') + '</span>' +
      '<span class="t-dur">' + formatDuration(t.duration_sec || t.duration) + '</span></li>';
  }).join('') + '</ol>';
}

function formatDuration(sec) {
  if (!sec || sec <= 0) return '--:--';
  const m = Math.floor(sec / 60), s = Math.floor(sec % 60);
  return m + ':' + (s < 10 ? '0' + s : s);
}

async function downloadArtistAll(artistID, artistName) {
  try {
    const res = await fetch('/api/artists/' + encodeURIComponent(artistID) + '/download-all', { method: 'POST' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    showToast('Downloading everything by ' + artistName, 'success');
    refreshQueueBadge();
  } catch (err) {
    showToast('Download failed: ' + err.message, 'error');
  }
}

async function downloadSingleAlbum(albumID, albumTitle) { return downloadSingleAlbumFromPage(albumID, albumTitle); }
function viewArtistDetail(artistID) { openArtistPage(artistID); }
function previewArtistDiscography(sourceID, payload) { openArtistPreviewPage(sourceID, payload); }

async function deleteArtist(artistID, artistName) {
  if (!confirm('Remove ' + artistName + ' and every indexed release? Files on disk stay where they are.')) return;
  try {
    const res = await fetch('/api/artists/' + encodeURIComponent(artistID), { method: 'DELETE' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    showToast('Removed ' + artistName, 'success');
    if (currentView === 'artist') navigateBackFromArtist(); else loadLibraryArtists();
  } catch (err) {
    showToast('Could not remove ' + artistName + ': ' + err.message, 'error');
  }
}

/* ───────────── Search ───────────── */
function setSearchProvider(p) {
  searchProvider = p;
  document.querySelectorAll('#searchProviderSeg button').forEach((b) => {
    const on = b.dataset.provider === p;
    b.classList.toggle('active', on);
    b.setAttribute('aria-checked', String(on));
  });
  if ($('searchQueryInput').value.trim()) executeArtistSearch();
}

function quickSearch(name) { $('searchQueryInput').value = name; executeArtistSearch(); }

async function executeArtistSearch() {
  const q = $('searchQueryInput').value.trim();
  if (!q) { $('searchQueryInput').focus(); return; }
  const provider = searchProvider;
  const grid = $('searchResultsGrid');
  $('searchResultsHeader').textContent = 'Results for “' + q + '”';
  $('searchResultsCount').textContent = 'searching ' + provider;
  grid.innerHTML = skeletonFolders(6);

  try {
    const res = await fetch('/api/search?q=' + encodeURIComponent(q) + '&provider=' + provider);
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const artists = (await res.json()) || [];
    if (!libraryLoaded) { try { allArtists = (await (await fetch('/api/artists')).json()) || []; libraryLoaded = true; } catch (e) {} }
    $('searchResultsCount').textContent = plural(artists.length, 'match', 'matches') + ' on ' + provider;

    if (!artists.length) {
      grid.innerHTML = emptyState('Nothing found for “' + esc(q) + '”', 'Check the spelling or switch to the other catalog.');
      return;
    }

    grid.innerHTML = artists.map((a) => {
      const inLib = allArtists.find((x) => (x.source_id && x.source_id === a.source_id) || (x.name && a.name && x.name.toLowerCase() === a.name.toLowerCase()));
      const payload = jsq(encodeURIComponent(JSON.stringify(a)));
      const open = inLib ? "openArtistPage('" + jsq(inLib.id) + "')" : "openArtistPreviewPage('" + jsq(a.source_id) + "', '" + payload + "')";
      return '<div class="folder" onclick="' + open + '">' +
        '<div class="folder-back">' + imgTag(a.image_url, '', a.name) + '</div>' +
        '<div class="folder-front">' +
          '<div><h3 class="folder-name">' + esc(a.name) + '</h3><p class="folder-meta">' + esc(a.source || provider) + '</p></div>' +
          (inLib
            ? '<div class="folder-row"><span class="chip chip-mint"><span class="dot"></span>In library</span>' +
              '<div class="folder-actions"><button class="btn btn-icon btn-sm" type="button" aria-label="Open ' + esc(a.name) + '" onclick="event.stopPropagation();' + open + '">' + ic('chev-right') + '</button></div></div>'
            : '<div class="folder-buttons" onclick="event.stopPropagation()">' +
                '<button class="btn btn-sm" type="button" onclick="addArtistFromSearch(\'' + payload + '\', false)">' + ic('plus') + 'Add</button>' +
                '<button class="btn btn-sm btn-mint" type="button" onclick="addArtistFromSearch(\'' + payload + '\', true)">' + ic('download') + 'Get all</button>' +
              '</div>') +
        '</div>' +
      '</div>';
    }).join('');
  } catch (err) {
    $('searchResultsCount').textContent = '';
    grid.innerHTML = emptyState('Search failed', esc(err.message), '<button class="btn" type="button" onclick="executeArtistSearch()">' + ic('refresh') + 'Try again</button>', true);
  }
}

async function addArtistFromSearch(jsonArtistEncoded, downloadAll) {
  const artist = JSON.parse(decodeURIComponent(jsonArtistEncoded));
  try {
    const res = await fetch('/api/artists', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: artist.id, source_id: artist.source_id, source: artist.source, name: artist.name, image_url: artist.image_url, monitored: true, download_all: downloadAll })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.message || 'Could not add the artist');
    showToast(downloadAll ? 'Added ' + artist.name + ' and started the downloads' : 'Added ' + artist.name, 'success');
    libraryLoaded = false;
    try { allArtists = (await (await fetch('/api/artists')).json()) || []; libraryLoaded = true; } catch (e) {}
    if (downloadAll) refreshQueueBadge();
    if (currentView === 'artist' && isArtistPreviewMode) {
      const added = allArtists.find((x) => x.source_id === artist.source_id);
      if (added) { isArtistPreviewMode = false; openArtistPage(added.id); return; }
    }
    if ($('searchQueryInput').value.trim()) executeArtistSearch();
  } catch (err) {
    showToast(err.message, 'error');
  }
}

/* ───────────── Queue & history ───────────── */
let currentHistoryFilter = 'all';
let currentActivityStatus = 'all';

function setQueueBadges(n) {
  ['sidebarQueueCount', 'tabQueueCount'].forEach((id) => {
    const b = $(id);
    b.textContent = n;
    b.hidden = !n;
  });
}

async function refreshQueueBadge() {
  try {
    const res = await fetch('/api/queue');
    const jobs = (await res.json()) || [];
    setQueueBadges(jobs.length);
  } catch (e) {}
}
setInterval(() => { if (currentView !== 'activity' && !document.hidden) refreshQueueBadge(); }, 10000);

async function loadLiveQueue() {
  const box = $('liveQueueContainer');
  try {
    const [resJobs, resStatus] = await Promise.all([fetch('/api/queue'), fetch('/api/queue/status').catch(() => null)]);
    const jobs = (await resJobs.json()) || [];
    let q = { max_concurrent: 2, active_downloads: 0, pending_downloads: 0 };
    if (resStatus && resStatus.ok) { try { q = await resStatus.json(); } catch (e) {} }

    const max = q.max_concurrent || 2;
    const active = jobs.filter((j) => j.status !== 'pending');
    const waiting = jobs.filter((j) => j.status === 'pending');
    setQueueBadges(jobs.length);
    $('queueSlotRing').style.setProperty('--p', Math.min(100, Math.round((active.length / max) * 100)));
    $('queueSlotText').textContent = active.length + '/' + max;
    $('queueActiveCount').textContent = active.length + ' active';
    $('queueConcurrencyDisplay').textContent = plural(max, 'download slot');
    $('queuePendingCount').textContent = waiting.length;
    const badge = $('queueBadge');
    badge.className = 'chip' + (jobs.length ? ' chip-mint' : '');
    badge.innerHTML = '<span class="dot"></span>' + (jobs.length ? jobs.length + ' in pipeline' : 'Idle');

    if (!jobs.length) {
      box.innerHTML = emptyState('Nothing downloading', 'Queue a release from an artist page and it shows up here.');
      return;
    }

    let h = '';
    if (active.length) {
      h += '<div class="lane"><div class="lane-label live"><span class="pulse"></span>Downloading · ' + active.length + ' of ' + max + ' slots</div><div class="jobs">' +
        active.map((j) => {
          const pct = Math.min(100, Math.max(0, Math.round(j.progress || 0)));
          const started = j.created_at ? new Date(j.created_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '';
          const sub = [j.artist_name || 'Unknown artist', (j.downloader || 'qobuz').toUpperCase(), j.target_quality || 'flac', started].filter(Boolean).map(esc).join(' · ');
          return '<div class="job">' + imgTag(j.cover_url, 'thumb thumb-lg', '') +
            '<div style="min-width:0"><div class="job-title">' + esc(j.album_title || 'Album ' + j.album_id) + '</div>' +
              '<div class="job-sub">' + esc((j.status || 'downloading')) + ' · ' + pct + '% · ' + sub + '</div>' +
              '<div class="segbar" style="--p:' + pct + '" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="' + pct + '"></div></div>' +
            '<span class="ring" style="--p:' + pct + '"><span>' + pct + '%</span></span>' +
            '<button class="btn btn-icon btn-sm btn-ghost" type="button" aria-label="Cancel download" title="Cancel" onclick="cancelQueueDownload(\'' + jsq(j.album_id) + '\')">' + ic('x') + '</button>' +
          '</div>';
        }).join('') + '</div></div>';
    }
    if (waiting.length) {
      h += '<div class="lane"><div class="lane-label">Waiting · ' + waiting.length + '</div><div class="jobs">' +
        waiting.map((j, idx) =>
          '<div class="job waiting">' + imgTag(j.cover_url, 'thumb', '') +
            '<div style="min-width:0"><div class="job-title">' + esc(j.album_title || 'Album ' + j.album_id) + '</div>' +
              '<div class="job-sub">' + esc(j.artist_name || 'Unknown artist') + ' · ' + esc((j.downloader || 'qobuz').toUpperCase()) + '</div></div>' +
            '<span class="chip">#' + (idx + 1) + '</span>' +
            '<button class="btn btn-icon btn-sm btn-ghost" type="button" aria-label="Cancel" title="Cancel" onclick="cancelQueueDownload(\'' + jsq(j.album_id) + '\')">' + ic('x') + '</button>' +
          '</div>'
        ).join('') + '</div></div>';
    }
    box.innerHTML = h;
  } catch (err) {
    console.error('Queue error:', err);
  }
}

async function cancelQueueDownload(albumID) {
  if (!confirm('Cancel this download?')) return;
  try {
    const res = await fetch('/api/queue/cancel', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ album_id: String(albumID) })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'Could not cancel');
    showToast('Download cancelled', 'info');
    loadLiveQueue();
  } catch (err) {
    showToast(err.message, 'error');
  }
}

function filterHistory(filter, btn) {
  currentHistoryFilter = filter;
  btn.parentElement.querySelectorAll('button').forEach((b) => b.classList.toggle('active', b === btn));
  const releases = filter === 'releases';
  $('historyTableContainer').hidden = releases;
  $('releasesTableCard').hidden = !releases;
  $('btnClearHistory').hidden = releases;
  if (releases) loadActivityReleases(); else loadDownloadHistory();
}

async function loadDownloadHistory() {
  const box = $('historyTableContainer');
  try {
    const res = await fetch('/api/history?limit=100');
    if (!res.ok) throw new Error((await res.text()) || 'HTTP ' + res.status);
    const jobs = (await res.json()) || [];
    $('historyDoneCount').textContent = jobs.filter((j) => j.status === 'completed').length;
    if (currentHistoryFilter === 'releases') return;
    const list = jobs.filter((j) => currentHistoryFilter === 'all' || j.status === currentHistoryFilter);
    $('historyCount').textContent = plural(list.length, 'job') + (currentHistoryFilter === 'all' ? ', newest first' : '');

    if (!list.length) {
      box.innerHTML = emptyState(jobs.length ? 'No job matches' : 'No history yet', jobs.length ? 'Pick another filter.' : 'Finished and failed downloads are listed here.');
      return;
    }
    box.innerHTML = '<div class="rows">' + list.map((j) => {
      const title = j.album_title || 'Album ' + j.album_id;
      const when = j.created_at ? new Date(j.created_at).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }) : '';
      const sub = [j.artist_name || '—', (j.downloader || 'qobuz').toUpperCase() + (j.target_quality ? ' ' + j.target_quality : ''), when].filter(Boolean).map(esc).join(' · ');
      return '<div class="row">' + imgTag(j.cover_url, 'thumb', '') +
        '<div style="min-width:0"><div class="row-title">' + esc(title) + '</div><div class="row-sub">' + sub + '</div></div>' +
        statusChip(j.status) +
        '<button class="btn btn-icon btn-sm row-act" type="button" aria-label="Download ' + esc(title) + ' again" title="Retry" onclick="downloadSingleAlbum(\'' + jsq(j.album_id) + '\', \'' + jsq(title) + '\')">' + ic('refresh') + '</button>' +
        (j.status === 'failed' && j.error_message ? '<div class="row-err">' + esc(j.error_message) + '</div>' : '') +
      '</div>';
    }).join('') + '</div>';
  } catch (err) {
    box.innerHTML = emptyState('History did not load', esc(err.message), '', true);
  }
}

async function clearDownloadHistory() {
  if (!confirm('Clear all finished and failed jobs from the history?')) return;
  try {
    const res = await fetch('/api/history/clear', { method: 'POST' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    showToast('History cleared', 'success');
    loadDownloadHistory();
  } catch (err) {
    showToast('Could not clear history: ' + err.message, 'error');
  }
}

function filterActivity(status, btn) {
  currentActivityStatus = status;
  btn.parentElement.querySelectorAll('button').forEach((b) => b.classList.toggle('active', b === btn));
  loadActivityReleases();
}

async function loadActivityReleases() {
  const box = $('activityTableBody');
  try {
    const res = await fetch('/api/albums?status=' + currentActivityStatus);
    const albums = (await res.json()) || [];
    $('historyCount').textContent = plural(albums.length, 'release') + ' in the database';
    if (!albums.length) { box.innerHTML = emptyState('No release matches', 'Pick another filter.'); return; }
    box.innerHTML = albums.map((alb) =>
      '<div class="row">' + imgTag(alb.cover_url, 'thumb', '') +
        '<div style="min-width:0"><div class="row-title">' + esc(alb.title) + '</div><div class="row-sub">' + [alb.artist_name, typeOf(alb), alb.year || '—'].map(esc).join(' · ') + '</div></div>' +
        statusChip(alb.status) +
        '<button class="btn btn-icon btn-sm row-act" type="button" aria-label="Download ' + esc(alb.title) + '" title="' + (alb.status === 'completed' ? 'Download again' : 'Download') + '" onclick="downloadSingleAlbum(\'' + jsq(alb.id) + '\', \'' + jsq(alb.title) + '\')">' + ic('download') + '</button>' +
      '</div>'
    ).join('');
  } catch (err) {
    box.innerHTML = emptyState('Releases did not load', esc(err.message), '', true);
  }
}

setInterval(() => { if (currentView === 'activity' && !document.hidden) loadLiveQueue(); }, 2500);

/* ───────────── Settings ───────────── */
function openSettingsTab(tab, fromRoute) {
  if (!$('tab-' + tab)) tab = 'storage';
  currentSettingsTab = tab;
  if (!fromRoute) { go('settings/' + tab); return; }
  document.querySelectorAll('.settings-tab-btn').forEach((b) => {
    const on = b.dataset.tab === tab;
    b.classList.toggle('active', on);
    b.setAttribute('aria-selected', String(on));
  });
  document.querySelectorAll('.nav-sub').forEach((a) => a.classList.toggle('active', a.dataset.tab === tab));
  document.querySelectorAll('.settings-section').forEach((s) => s.classList.toggle('active', s.id === 'tab-' + tab));
  $('settingsSaveBar').hidden = tab === 'yaml';
  if (tab === 'yaml') loadRawYaml();
}
function switchSettingsTab(tabName) { openSettingsTab(tabName); }

async function loadSettings() {
  try {
    const res = await fetch('/api/config');
    currentConfig = await res.json();
    populateSettingsForm(currentConfig);
  } catch (err) {
    showToast('Settings did not load: ' + err.message, 'error');
  }
}

function populateSettingsForm(cfg) {
  if (!cfg) return;
  const v = (id, val) => { $(id).value = val; };
  const c = (id, val) => { $(id).checked = val; };
  const lib = cfg.library || {}, ver = cfg.verifier || {}, imp = cfg.import || {}, exp = cfg.export || {};
  const idx = cfg.indexer || {}, dl = cfg.downloader || {}, qb = dl.qobuz || {}, sl = dl.slskd || {}, nv = cfg.navidrome || {};

  v('cfg_music_path', lib.music_path || '');
  v('cfg_download_path', lib.download_path || '');
  v('cfg_folder_format', lib.folder_format || '{Artist}/{Album} ({Year})');
  v('cfg_file_format', lib.file_format || '{Track:02d} - {Title}');
  v('cfg_duration_tol', ver.duration_tolerance_sec || 4);
  v('cfg_min_score', ver.min_confidence_score || 0.85);
  c('cfg_allow_partial', !!ver.allow_partial_match);
  c('cfg_delete_after_import', imp.delete_after_import !== false);
  v('cfg_move_or_copy', imp.move_or_copy || 'move');
  c('cfg_clean_empty_folders', imp.clean_empty_folders !== false);
  c('cfg_download_cover_art', exp.download_cover_art !== false);
  v('cfg_cover_filename', exp.cover_filename || 'cover.jpg');
  c('cfg_write_nfo', !!exp.write_nfo);
  c('cfg_generate_m3u', !!exp.generate_m3u);
  v('cfg_indexer_provider', idx.default_provider || 'deezer');
  v('cfg_spotify_client_id', (idx.spotify || {}).client_id || '');
  v('cfg_spotify_client_secret', (idx.spotify || {}).client_secret || '');
  v('cfg_preferred_downloader', dl.preferred || 'slskd');
  v('cfg_max_concurrent_downloads', dl.max_concurrent_downloads || 2);
  c('cfg_qobuz_dl_enabled', !!qb.enabled);
  v('cfg_qobuz_target_quality', qb.target_quality || 'flac_16_44');
  v('cfg_qobuz_user_token', qb.user_token || '');
  v('cfg_qobuz_app_id', qb.app_id || '798273057');
  v('cfg_qobuz_app_secret', qb.app_secret || '');
  c('cfg_slskd_enabled', !!sl.enabled);
  v('cfg_slskd_url', sl.url || 'http://localhost:5030');
  v('cfg_slskd_api_key', sl.api_key || '');
  v('cfg_slskd_dir', sl.download_dir || '');
  c('cfg_navi_enabled', !!nv.enabled);
  v('cfg_navi_url', nv.url || 'http://localhost:4533');
  v('cfg_navi_username', nv.username || '');
  v('cfg_navi_password', nv.password || '');
  c('cfg_navi_token_auth', nv.token_auth !== false);
}

async function saveConfiguration(event) {
  if (event) event.preventDefault();
  const v = (id) => $(id).value;
  const c = (id) => $(id).checked;
  const prev = currentConfig || {};
  const prevQ = (prev.downloader && prev.downloader.qobuz) || {};
  const newCfg = {
    server: prev.server || { host: '0.0.0.0', port: 8686 },
    database: prev.database || { path: 'findr.db' },
    library: { music_path: v('cfg_music_path'), download_path: v('cfg_download_path'), folder_format: v('cfg_folder_format'), file_format: v('cfg_file_format') },
    import: { delete_after_import: c('cfg_delete_after_import'), move_or_copy: v('cfg_move_or_copy'), clean_empty_folders: c('cfg_clean_empty_folders') },
    export: { download_cover_art: c('cfg_download_cover_art'), cover_filename: v('cfg_cover_filename'), write_nfo: c('cfg_write_nfo'), generate_m3u: c('cfg_generate_m3u') },
    verifier: {
      duration_tolerance_sec: parseInt(v('cfg_duration_tol'), 10) || 4,
      min_confidence_score: parseFloat(v('cfg_min_score')) || 0.85,
      allow_partial_match: c('cfg_allow_partial')
    },
    indexer: {
      default_provider: v('cfg_indexer_provider'),
      spotify: { client_id: v('cfg_spotify_client_id'), client_secret: v('cfg_spotify_client_secret') },
      qobuz: { app_id: v('cfg_qobuz_app_id'), app_secret: v('cfg_qobuz_app_secret'), user_token: v('cfg_qobuz_user_token') }
    },
    downloader: {
      preferred: v('cfg_preferred_downloader') || 'slskd',
      max_concurrent_downloads: parseInt(v('cfg_max_concurrent_downloads'), 10) || 2,
      qobuz: {
        enabled: c('cfg_qobuz_dl_enabled'), target_quality: v('cfg_qobuz_target_quality'), user_token: v('cfg_qobuz_user_token'),
        app_id: v('cfg_qobuz_app_id'), app_secret: v('cfg_qobuz_app_secret'), username: prevQ.username || '', password: prevQ.password || ''
      },
      slskd: { enabled: c('cfg_slskd_enabled'), url: v('cfg_slskd_url'), api_key: v('cfg_slskd_api_key'), download_dir: v('cfg_slskd_dir') }
    },
    navidrome: { enabled: c('cfg_navi_enabled'), url: v('cfg_navi_url'), username: v('cfg_navi_username'), password: v('cfg_navi_password'), token_auth: c('cfg_navi_token_auth') }
  };

  const btn = event && event.submitter;
  setBusy(btn, true, 'Saving');
  try {
    const res = await fetch('/api/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(newCfg) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.message || 'Save failed');
    currentConfig = data.config;
    showToast('Settings saved and applied', 'success');
  } catch (err) {
    showToast('Save failed: ' + err.message, 'error');
  } finally {
    setBusy(btn, false);
  }
}

async function runConnectionTest(url, payload, el, okText) {
  el.innerHTML = '<span class="busy">Testing</span>';
  try {
    const res = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
    const data = await res.json();
    el.innerHTML = data.success
      ? '<span class="ok">✓ ' + okText + '</span>'
      : '<span class="bad">✕ ' + esc(data.error || 'Server error') + '</span>';
  } catch (err) {
    el.innerHTML = '<span class="bad">✕ ' + esc(err.message) + '</span>';
  }
}

function testNavidromeConnection() {
  runConnectionTest('/api/config/test-navidrome', {
    url: $('cfg_navi_url').value, username: $('cfg_navi_username').value,
    password: $('cfg_navi_password').value, token_auth: $('cfg_navi_token_auth').checked
  }, $('navidromeTestResult'), 'Navidrome answered the ping');
}

function testSlskdConnection() {
  runConnectionTest('/api/config/test-slskd', { url: $('cfg_slskd_url').value, api_key: $('cfg_slskd_api_key').value },
    $('slskdTestResult'), 'slskd is reachable');
}

async function loadDisksOverview() {
  try {
    const res = await fetch('/api/disks');
    renderDisksHtml('systemDisksList', (await res.json()) || []);
  } catch (err) {
    console.error(err);
  }
}

function renderDisksHtml(elementID, disks) {
  const el = $(elementID);
  if (!el) return;
  if (!disks.length) { el.innerHTML = '<p class="muted">No disks reported.</p>'; return; }
  el.innerHTML = disks.map((d) => {
    const pct = Math.round(d.percent_used || 0);
    const tone = pct > 90 ? ' bad' : pct > 75 ? ' warn' : '';
    return '<div class="disk"><div class="disk-top"><span class="disk-path">' + esc(d.path) + '</span>' +
      '<span class="disk-num">' + esc(d.free_human || '0 B') + ' free of ' + esc(d.total_human || '0 B') + ' · ' + pct + '% used</span></div>' +
      '<div class="bar' + tone + '" role="progressbar" aria-label="' + esc(d.path) + ' used" aria-valuemin="0" aria-valuemax="100" aria-valuenow="' + pct + '"><span style="width:' + pct + '%"></span></div></div>';
  }).join('');
}

async function loadRawYaml() {
  try {
    const res = await fetch('/api/config/raw');
    $('rawYamlEditor').value = await res.text();
  } catch (err) {
    showToast('YAML did not load: ' + err.message, 'error');
  }
}

async function saveRawYaml() {
  try {
    const res = await fetch('/api/config/raw', { method: 'POST', headers: { 'Content-Type': 'text/yaml' }, body: $('rawYamlEditor').value });
    const data = await res.json();
    if (!res.ok) throw new Error(data.message || 'Could not save the YAML');
    showToast('config.yaml saved', 'success');
    loadSettings();
  } catch (err) {
    showToast('YAML save failed: ' + err.message, 'error');
  }
}

function exportConfigFile() {
  const blob = new Blob([$('rawYamlEditor').value], { type: 'text/yaml' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url; a.download = 'config.yaml'; a.click();
  URL.revokeObjectURL(url);
}

function handleYamlFileUpload(event) {
  const file = event.target.files[0];
  if (!file) return;
  const reader = new FileReader();
  reader.onload = (e) => { $('rawYamlEditor').value = e.target.result; showToast('Loaded ' + file.name + '. Save to apply it.', 'info'); };
  reader.readAsText(file);
}

/* ───────────── System ───────────── */
async function loadSystemOverview() {
  try {
    if (!currentConfig) { try { currentConfig = await (await fetch('/api/config')).json(); } catch (e) {} }
    const res = await fetch('/api/status');
    const data = await res.json();
    $('sysUptime').textContent = formatUptime(data.uptime_seconds);
    $('sysArtistCount').textContent = data.monitored_artists || 0;
    $('sysAlbumCount').textContent = data.monitored_albums || 0;
    $('sysDownloader').textContent = data.downloader || 'qobuz';
    $('sysNavidrome').textContent = data.navidrome_enabled ? 'on' : 'off';
    $('sysIndexer').textContent = (currentConfig && currentConfig.indexer && currentConfig.indexer.default_provider) || 'deezer';
  } catch (err) {
    console.error(err);
  }
  loadDisksOverview();
}

function formatUptime(sec) {
  if (!sec) return '0s';
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d) return d + 'd ' + h + 'h';
  if (h) return h + 'h ' + m + 'm';
  if (m) return m + 'm ' + (sec % 60) + 's';
  return sec + 's';
}

async function triggerNavidromeScan() {
  const icon = $('scanIcon');
  icon.classList.add('spin');
  try {
    const res = await fetch('/api/scan', { method: 'POST' });
    let data = {};
    try { data = await res.json(); } catch (_) {}
    if (res.ok) showToast('Navidrome is rescanning the library', 'success');
    else showToast('Scan failed: ' + (data.message || data.error || 'Navidrome is not enabled'), 'error');
  } catch (err) {
    showToast('Scan failed: ' + err.message, 'error');
  } finally {
    setTimeout(() => icon.classList.remove('spin'), 900);
  }
}

/* ───────────── Manual import ───────────── */
let detectedImportFiles = [];
let importArtistsList = [];

async function initManualImport() {
  await populateImportArtistOptions();
}

function switchImportMode(mode) {
  const scan = mode === 'scan';
  $('btnModeUpload').classList.toggle('active', !scan);
  $('btnModeScan').classList.toggle('active', scan);
  $('paneUpload').hidden = scan;
  $('paneScan').hidden = !scan;
  if (scan) setTimeout(() => $('importScanPathInput').focus(), 40);
}

function handleDragOver(e) { e.preventDefault(); e.stopPropagation(); $('audioDropzone').classList.add('drag-over'); }
function handleDragLeave(e) { e.preventDefault(); e.stopPropagation(); $('audioDropzone').classList.remove('drag-over'); }
function handleFileDrop(e) {
  e.preventDefault(); e.stopPropagation();
  $('audioDropzone').classList.remove('drag-over');
  const files = e.dataTransfer && e.dataTransfer.files;
  if (files && files.length) uploadAudioFiles(files);
}
function handleAudioFileSelect(e) {
  const files = e.target.files;
  if (files && files.length) uploadAudioFiles(files);
  e.target.value = '';
}

function uploadAudioFiles(files) {
  const wrap = $('uploadProgressContainer'), status = $('uploadStatusText'), pctEl = $('uploadPercentText'), bar = $('uploadProgressBar');
  wrap.hidden = false;
  status.textContent = 'Uploading ' + plural(files.length, 'file');
  pctEl.textContent = '0%';
  bar.style.width = '0%';

  const form = new FormData();
  for (let i = 0; i < files.length; i++) form.append('files', files[i]);
  const xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/import/upload', true);
  xhr.upload.onprogress = (e) => {
    if (!e.lengthComputable) return;
    const p = Math.round((e.loaded / e.total) * 100);
    pctEl.textContent = p + '%';
    bar.style.width = p + '%';
  };
  xhr.onload = () => {
    if (xhr.status >= 200 && xhr.status < 300) {
      try {
        detectedImportFiles = JSON.parse(xhr.responseText).files || [];
        showToast('Read tags from ' + plural(detectedImportFiles.length, 'file'), 'success');
        renderDetectedFiles();
        autoFillImportDestination();
      } catch (err) {
        showToast('The upload answer could not be read', 'error');
      }
    } else {
      showToast('Upload failed (HTTP ' + xhr.status + ')', 'error');
    }
    setTimeout(() => { wrap.hidden = true; }, 1500);
  };
  xhr.onerror = () => { showToast('Network error during upload', 'error'); wrap.hidden = true; };
  xhr.send(form);
}

async function executeDirectoryScan() {
  const path = $('importScanPathInput').value.trim();
  if (!path) { showToast('Type a folder path first', 'info'); $('importScanPathInput').focus(); return; }
  try {
    const res = await fetch('/api/import/scan', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path: path }) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'Could not scan the folder');
    detectedImportFiles = data.files || [];
    showToast('Found ' + plural(detectedImportFiles.length, 'audio file'), 'success');
    renderDetectedFiles();
    autoFillImportDestination();
  } catch (err) {
    showToast(err.message, 'error');
  }
}

function renderDetectedFiles() {
  const card = $('importReviewCard'), box = $('detectedFilesTableContainer');
  if (!detectedImportFiles || !detectedImportFiles.length) { card.hidden = true; box.innerHTML = ''; return; }
  card.hidden = false;
  const totalMB = detectedImportFiles.reduce((s, f) => s + (f.size_bytes || 0), 0) / (1024 * 1024);
  $('importFileSummary').textContent = plural(detectedImportFiles.length, 'file') + ' · ' + totalMB.toFixed(1) + ' MB';
  box.innerHTML = detectedImportFiles.map((f, idx) => {
    const n = f.track_number ? (f.track_number < 10 ? '0' + f.track_number : f.track_number) : idx + 1;
    const title = f.title || f.file_name || 'Track ' + n;
    const sub = [f.artist || '—', f.album || '—', formatDuration(f.duration_sec), ((f.size_bytes || 0) / (1024 * 1024)).toFixed(1) + ' MB'].map(esc).join(' · ');
    return '<div class="row" style="grid-template-columns:48px minmax(0,1fr) auto">' +
      '<span class="thumb mono" style="display:grid;place-items:center;color:var(--muted);font-size:13px">' + esc(n) + '</span>' +
      '<div style="min-width:0"><div class="row-title">' + esc(title) + '</div><div class="row-sub">' + sub + '</div></div>' +
      '<span class="chip" title="' + esc(f.file_path || '') + '" style="max-width:220px;overflow:hidden;text-overflow:ellipsis">' + esc(f.file_name || '') + '</span>' +
    '</div>';
  }).join('');
}

async function populateImportArtistOptions() {
  try { importArtistsList = (await (await fetch('/api/artists')).json()) || []; } catch (e) { importArtistsList = []; }
  const select = $('importArtistSelect');
  const keep = select.value;
  select.innerHTML = '<option value="">Choose an artist</option>' +
    importArtistsList.map((a) => '<option value="' + esc(a.id) + '">' + esc(a.name) + '</option>').join('') +
    '<option value="__custom__">New artist…</option>';
  if (keep) select.value = keep;
  if (!$('importAlbumSelect').options.length) $('importAlbumSelect').innerHTML = '<option value="">Choose an artist first</option>';
}

async function handleImportArtistChange() {
  const val = $('importArtistSelect').value;
  const newArtist = $('importNewArtistInput'), albumSel = $('importAlbumSelect'), newAlbum = $('importNewAlbumInput');
  if (val === '__custom__') {
    newArtist.hidden = false;
    albumSel.innerHTML = '<option value="__custom__">New release…</option>';
    newAlbum.hidden = false;
    return;
  }
  newArtist.hidden = true;
  if (!val) { albumSel.innerHTML = '<option value="">Choose an artist first</option>'; newAlbum.hidden = true; return; }
  try {
    const data = await (await fetch('/api/artists/' + encodeURIComponent(val))).json();
    const releases = data.albums || data.releases || [];
    albumSel.innerHTML = '<option value="">Choose a release</option>' +
      releases.map((r) => {
        const type = r.release_type || r.type || 'album';
        return '<option value="' + esc(r.id) + '" data-type="' + esc(type) + '" data-year="' + esc(r.year || '') + '" data-title="' + esc(r.title) + '">' +
          esc(r.title) + (r.year ? ' (' + esc(r.year) + ')' : '') + ' · ' + esc(type) + '</option>';
      }).join('') +
      '<option value="__custom__">New release…</option>';
    newAlbum.hidden = true;
  } catch (err) {
    console.error('Releases did not load', err);
  }
}

function handleImportAlbumChange() {
  const sel = $('importAlbumSelect');
  const isNew = sel.value === '__custom__';
  $('importNewAlbumInput').hidden = !isNew;
  if (isNew) return;
  const opt = sel.selectedOptions[0];
  if (!opt) return;
  if (opt.dataset.type) $('importReleaseType').value = opt.dataset.type;
  if (opt.dataset.year) $('importYearInput').value = opt.dataset.year;
}

function autoFillImportDestination() {
  if (!detectedImportFiles.length) return;
  const first = detectedImportFiles[0];
  const artistName = (first.artist || '').trim();
  const albumTitle = (first.album || '').trim();
  if (!artistName) return;
  const artistSel = $('importArtistSelect'), albumSel = $('importAlbumSelect');
  const newArtist = $('importNewArtistInput'), newAlbum = $('importNewAlbumInput');
  const match = importArtistsList.find((a) => (a.name || '').toLowerCase() === artistName.toLowerCase());
  if (match) {
    artistSel.value = match.id;
    handleImportArtistChange().then(() => {
      if (!albumTitle) return;
      const opt = Array.from(albumSel.options).find((o) => (o.dataset.title || '').toLowerCase() === albumTitle.toLowerCase());
      if (opt) { albumSel.value = opt.value; handleImportAlbumChange(); }
      else { albumSel.value = '__custom__'; newAlbum.hidden = false; newAlbum.value = albumTitle; }
    });
  } else {
    artistSel.value = '__custom__';
    newArtist.hidden = false; newArtist.value = artistName;
    albumSel.innerHTML = '<option value="__custom__">New release…</option>';
    albumSel.value = '__custom__';
    newAlbum.hidden = false; newAlbum.value = albumTitle;
  }
}

async function executeManualImport() {
  if (!detectedImportFiles.length) { showToast('Upload or scan some files first', 'info'); return; }
  const artistSel = $('importArtistSelect'), albumSel = $('importAlbumSelect');

  let artistID = '', artistName = '';
  if (artistSel.value === '__custom__') artistName = $('importNewArtistInput').value.trim();
  else if (artistSel.value) {
    artistID = artistSel.value;
    const found = importArtistsList.find((a) => String(a.id) === artistID);
    if (found) artistName = found.name;
  }

  let albumID = '', albumTitle = '';
  if (albumSel.value === '__custom__') albumTitle = $('importNewAlbumInput').value.trim();
  else if (albumSel.value) {
    albumID = albumSel.value;
    const opt = albumSel.selectedOptions[0];
    if (opt) albumTitle = opt.dataset.title || opt.text;
  }

  if (!artistName) { showToast('Choose or type an artist', 'error'); return; }
  if (!albumTitle) { showToast('Choose or type a release', 'error'); return; }

  const payload = {
    artist_id: artistID, artist_name: artistName,
    album_id: albumID, album_title: albumTitle,
    release_type: $('importReleaseType').value || 'album',
    year: parseInt($('importYearInput').value, 10) || 0,
    move_or_copy: $('importMoveOrCopy').value || 'move',
    files: detectedImportFiles
  };

  showToast('Importing ' + plural(detectedImportFiles.length, 'track'), 'info');
  try {
    const res = await fetch('/api/import/execute', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || 'Import failed');
    showToast('Imported to ' + data.destination, 'success');
    detectedImportFiles = [];
    renderDetectedFiles();
    libraryLoaded = false;
    setTimeout(() => go('artists'), 1200);
  } catch (err) {
    showToast(err.message, 'error');
  }
}
