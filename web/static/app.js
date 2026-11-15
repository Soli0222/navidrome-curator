import { t, errorText, languages, lang, setLang } from './i18n.js';

// ---------- 小さな補助 ----------

function h(tag, props = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === 'checked' || k === 'disabled' || k === 'value' || k === 'selected' || k === 'indeterminate') el[k] = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === undefined || c === null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

class ApiError extends Error {
  constructor(code, message, status) {
    super(message || code);
    this.code = code;
    this.status = status;
  }
}

async function api(method, url, body) {
  const opt = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(url, opt);
  } catch (e) {
    throw new ApiError('network', String(e), 0);
  }
  let data = null;
  const ct = res.headers.get('Content-Type') || '';
  if (ct.includes('application/json')) data = await res.json();
  if (!res.ok) {
    throw new ApiError(data?.error || 'generic', data?.message || res.statusText, res.status);
  }
  return data;
}

function qs(params) {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '') u.set(k, v);
  }
  const s = u.toString();
  return s ? `?${s}` : '';
}

function fmtBytes(n) {
  if (n === undefined || n === null) return '';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let v = Number(n);
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${i === 0 ? v : v.toFixed(v < 10 ? 2 : 1)} ${units[i]}`;
}

function fmtTime(s) {
  if (!s || s.startsWith('0001-')) return t('common.never');
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return t('common.never');
  return d.toLocaleString(lang() === 'ja' ? 'ja-JP' : 'en-US');
}

let toastTimer;
function toast(msg, bad = false) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = bad ? 'toast bad' : 'toast';
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, bad ? 8000 : 4000);
}

function showError(e) {
  toast(errorText(e.code, e.message), true);
}

const baseName = (p) => p.split('/').pop();
const parentPath = (p) => p.split('/').slice(0, -1).join('/');

// 「Album (2019)」を、名前と年に分ける。
function splitYear(name) {
  const m = name.match(/^(.*?)\s*[([](\d{4})[)\]]\s*$/);
  return m ? { title: m[1], year: m[2] } : { title: name, year: '' };
}

// ---------- ジャケット画像 ----------

// 画像の URL。デモ用のページは window.curatorCoverURL で差し替えられる。
function coverURL(kind, name, ref, size) {
  if (typeof window.curatorCoverURL === 'function') return window.curatorCoverURL(kind, name, ref, size);
  return `/api/cover${qs({ [kind]: name, ref, size })}`;
}

function hue(s) {
  let x = 0;
  for (const c of s) x = (x * 31 + c.codePointAt(0)) >>> 0;
  return x % 360;
}

function initials(name) {
  const { title } = splitYear(name);
  const words = title.replace(/[^\p{L}\p{N}\s]/gu, ' ').trim().split(/\s+/).filter(Boolean);
  if (!words.length) return '♪';
  if (/^[\p{Script=Han}\p{Script=Hiragana}\p{Script=Katakana}]/u.test(words[0])) return [...words[0]].slice(0, 2).join('');
  return words.slice(0, 2).map((w) => [...w][0].toUpperCase()).join('');
}

function placeholder(name) {
  const el = h('div', { class: 'cover-ph', 'aria-hidden': 'true' }, h('span', {}, initials(name)));
  el.style.setProperty('--ph-hue', String(hue(name)));
  return el;
}

// cover は画像を読み込み、なければ名前から作った色面に置き換える。
function cover(kind, name, ref, title, size = 320, eager = false) {
  const box = h('div', { class: 'cover' });
  const img = h('img', { loading: eager ? 'eager' : 'lazy', decoding: 'async', alt: '', src: coverURL(kind, name, ref, size) });
  img.addEventListener('error', () => img.replaceWith(placeholder(title)));
  box.append(img);
  return box;
}

// mosaic はアーティストのアルバムのジャケットを並べる。
function mosaic(source, refs, name) {
  const box = h('div', { class: `cover mosaic n${Math.min(refs.length, 4)}` });
  if (!refs.length) {
    box.append(placeholder(name));
    return box;
  }
  const list = refs.length >= 4 ? refs.slice(0, 4) : refs.slice(0, 1);
  for (const r of list) {
    const img = h('img', { loading: 'lazy', decoding: 'async', alt: '', src: coverURL('source', source, r, 160) });
    img.addEventListener('error', () => img.replaceWith(placeholder(name)));
    box.append(img);
  }
  return box;
}

// ---------- 全体の状態 ----------

let status = null;

async function loadStatus() {
  try {
    status = await api('GET', '/api/status');
    renderIndexStatus();
  } catch (e) {
    if (e.code === 'unauthorized') location.reload();
  }
}

function renderIndexStatus() {
  const el = document.getElementById('index-status');
  el.replaceChildren();
  if (!status) return;
  for (const s of status.sources) {
    let text;
    if (s.building) text = t('index.building', { n: s.scanned });
    else if (s.ready) text = t('index.ready', { time: fmtTime(s.lastSuccess) });
    else text = t('index.never');
    const line = h('div', {}, `${s.source}: ${text}`);
    if (s.lastError) line.append(' ', h('span', { class: 'bad', title: s.lastError }, t('index.error', { msg: s.lastError })));
    else if (!s.up && s.ready) line.append(' ', h('span', { class: 'bad' }, t('source.down')));
    el.append(line);
  }
}

function activeJobs() {
  if (!status) return 0;
  return (status.jobs.queued || 0) + (status.jobs.running || 0);
}

// ---------- 状態の表示 ----------

// albumStatus はアルバムの状態を1つにまとめる。カードの帯と色に使う。
function albumStatus(state, differs) {
  const running = state.jobs.filter((j) => j.status === 'running').map((j) => j.library);
  const queued = state.jobs.filter((j) => j.status === 'queued').map((j) => j.library);
  const diff = (differs || []).filter((l) => state.libraries.includes(l));
  if (running.length) return { kind: 'run', text: t('ribbon.running'), libs: running };
  if (queued.length) return { kind: 'queue', text: t('ribbon.queued'), libs: queued };
  if (diff.length) return { kind: 'warn', text: t('ribbon.differs'), libs: diff };
  if (state.libraries.length) return { kind: 'ok', text: t('ribbon.added'), libs: state.libraries };
  return { kind: 'none', text: t('status.notAdded'), libs: [] };
}

function stateBadges(state, isAlbum, differs) {
  const out = [];
  const queued = state.jobs.filter((j) => j.status === 'queued').map((j) => j.library);
  const running = state.jobs.filter((j) => j.status === 'running').map((j) => j.library);
  if (running.length) out.push(h('span', { class: 'badge warn' }, t('status.running', { libs: running.join(', ') })));
  if (queued.length) out.push(h('span', { class: 'badge info' }, t('status.queued', { libs: queued.join(', ') })));
  if (isAlbum) {
    const diff = (differs || []).filter((l) => state.libraries.includes(l));
    const same = state.libraries.filter((l) => !diff.includes(l));
    if (same.length) out.push(h('span', { class: 'badge ok' }, t('status.added', { libs: same.join(', ') })));
    if (diff.length) out.push(h('span', { class: 'badge warn' }, t('status.differs', { libs: diff.join(', ') })));
    if (!state.libraries.length && !state.jobs.length) out.push(h('span', { class: 'badge muted' }, t('status.notAdded')));
  } else if (state.albumsTotal > 0) {
    const cls = state.albumsAdded > 0 ? 'badge ok' : 'badge muted';
    out.push(h('span', { class: cls }, t('status.albumsAdded', { added: state.albumsAdded, total: state.albumsTotal })));
  }
  if (state.sameIn.length) out.push(h('span', { class: 'badge info' }, t('status.sameIn', { sources: state.sameIn.join(', ') })));
  return h('div', { class: 'badges' }, out);
}

// ---------- 選択 ----------

// Selection は、カードで選んだものを持つ。キーは任意の文字列、値は依頼に使うもの。
class Selection {
  constructor(onChange) {
    this.items = new Map();
    this.boxes = new Map(); // key -> [checkbox]
    this.onChange = onChange;
  }

  has(key) { return this.items.has(key); }

  get size() { return this.items.size; }

  set(key, value, on) {
    if (on) this.items.set(key, value); else this.items.delete(key);
    for (const b of this.boxes.get(key) || []) {
      b.checked = on;
      b.closest('.card')?.classList.toggle('selected', on);
    }
    this.onChange();
  }

  clear() {
    for (const k of [...this.items.keys()]) this.set(k, null, false);
  }

  // checkbox はカードに付けるチェックボックスを作る。
  checkbox(key, value, label, disabled = false) {
    const box = h('input', {
      type: 'checkbox', checked: this.has(key), disabled, 'aria-label': label,
      onchange: (e) => this.set(key, value, e.target.checked),
    });
    if (!this.boxes.has(key)) this.boxes.set(key, []);
    this.boxes.get(key).push(box);
    return h('label', { class: 'pick', title: label }, box);
  }

  resetBoxes() { this.boxes.clear(); }
}

// actionBar は画面の下に固定する、選んだものへの操作。
function actionBar(sel, ...controls) {
  const count = h('span', { class: 'count' });
  const clearBtn = h('button', { class: 'link', onclick: () => sel.clear() }, t('common.clearSelection'));
  const bar = h('div', { class: 'actionbar' }, h('div', { class: 'actionbar-inner' }, count, clearBtn, h('span', { class: 'grow' }), ...controls));
  const update = () => {
    count.textContent = t('add.selected', { n: sel.size });
    bar.classList.toggle('empty', sel.size === 0);
  };
  update();
  return { el: bar, update };
}

// libPicker は追加先を選ぶ部品。useDefaults が真なら「source の既定」を選べる。
function libPicker({ libraries, defaults = [], useDefaults = false }) {
  const boxes = new Map();
  let defBox = null;
  const el = h('div', { class: 'libpicker' }, h('span', { class: 'muted' }, `${t('add.target')}:`));
  if (useDefaults) {
    defBox = h('input', {
      type: 'checkbox', checked: true,
      onchange: () => { for (const b of boxes.values()) b.disabled = defBox.checked; },
    });
    el.append(h('label', {}, defBox, t('add.defaults')));
  }
  for (const name of libraries) {
    const b = h('input', { type: 'checkbox', checked: defaults.includes(name), disabled: useDefaults });
    boxes.set(name, b);
    el.append(h('label', { class: 'chip' }, b, name));
  }
  return {
    el,
    // null は「source の既定」を表す。
    value() {
      if (defBox && defBox.checked) return null;
      return [...boxes.entries()].filter(([, b]) => b.checked).map(([n]) => n);
    },
  };
}

async function submitJobs(items, libraries) {
  if (!items.length) {
    toast(t('add.noSelection'), true);
    return null;
  }
  if (libraries !== null && libraries.length === 0) {
    toast(t('add.noLibrary'), true);
    return null;
  }
  try {
    const body = { items };
    if (libraries) body.libraries = libraries;
    const res = await api('POST', '/api/jobs', body);
    const dup = res.results.filter((r) => r.result === 'duplicate').length;
    const errs = res.results.filter((r) => r.result === 'error');
    const lines = [t('add.result', { created: res.created })];
    if (dup) lines.push(t('add.resultDup', { n: dup }));
    if (errs.length) {
      lines.push(t('add.resultErr', { n: errs.length }));
      for (const e of errs.slice(0, 5)) lines.push(`${e.path}: ${errorText(e.error, e.message)}`);
    }
    toast(lines.join('\n'), errs.length > 0);
    loadStatus();
    return res;
  } catch (e) {
    showError(e);
    return null;
  }
}

// ---------- カード ----------

// albumCard はジャケット付きのアルバムのカード。update で状態だけを差し替えられる。
function albumCard({ kind, name, path, ref, label, href, sel, selValue, state, differs, meta, extraRibbon }) {
  const { title, year } = splitYear(label);
  const ribbon = h('div', { class: 'ribbon' });
  const libsEl = h('div', { class: 'card-libs' });
  const card = h('div', { class: 'card album' });
  const coverLink = href
    ? h('a', { class: 'cover-link', href, title: path }, cover(kind, name, ref, label), ribbon)
    : h('div', { class: 'cover-link', title: path }, cover(kind, name, ref, label), ribbon);
  card.append(
    coverLink,
    sel ? sel.checkbox(`${name}\u0000${ref}`, selValue, label) : '',
    h('div', { class: 'card-body' },
      href ? h('a', { class: 'card-title', href, title: label }, title) : h('div', { class: 'card-title', title: label }, title),
      h('div', { class: 'card-sub' }, [year, meta].filter(Boolean).join(' · ')),
      libsEl),
  );
  if (sel?.has(`${name}\u0000${ref}`)) card.classList.add('selected');
  const update = (st, diff) => {
    if (extraRibbon) {
      ribbon.className = `ribbon ${extraRibbon.kind}`;
      ribbon.textContent = extraRibbon.text;
      libsEl.replaceChildren();
      return;
    }
    const s = albumStatus(st, diff);
    ribbon.className = `ribbon ${s.kind}`;
    ribbon.textContent = s.kind === 'none' ? '' : s.text;
    card.dataset.status = s.kind;
    libsEl.replaceChildren(...st.libraries.map((l) => h('span', {
      class: (diff || []).includes(l) ? 'lib-chip warn' : 'lib-chip',
    }, l)), ...(st.sameIn.length ? [h('span', { class: 'lib-chip info', title: t('status.sameIn', { sources: st.sameIn.join(', ') }) }, `≡ ${st.sameIn.join(', ')}`)] : []));
  };
  update(state || { libraries: [], jobs: [], sameIn: [] }, differs);
  return { el: card, update };
}

function intro(key) {
  return h('p', { class: 'intro' }, t(key));
}

// searchForm は置き場のアルバムを探す入力欄。結果は検索の画面に出す。
function searchForm(q = '') {
  const input = h('input', { type: 'search', id: 'search-q', value: q, placeholder: t('search.placeholder'), 'aria-label': t('search.submit') });
  const form = h('form', {
    class: 'search-form',
    role: 'search',
    onsubmit: (e) => {
      e.preventDefault();
      const v = input.value.trim();
      if (v) location.hash = `#/search${qs({ q: v })}`;
    },
  }, input, h('button', { type: 'submit' }, t('search.submit')));
  return { form, input };
}

function grid(cards) {
  return h('div', { class: 'grid' }, cards);
}

function section(titleEl, sub, body, actions) {
  return h('section', { class: 'group' },
    h('div', { class: 'group-head' }, h('h3', {}, titleEl), sub ? h('span', { class: 'muted small' }, sub) : '', h('span', { class: 'grow' }), actions || ''),
    body);
}

// ---------- 閲覧 ----------

function viewBrowse(root, params) {
  let stopped = false;
  const sources = status?.sources || [];
  const source = params.get('source') || sources[0]?.source;
  const ref = params.get('ref') || '';
  const srcInfo = sources.find((s) => s.source === source);
  const differs = new Map();
  const compared = new Set();
  const cards = new Map(); // ref -> [update]
  let data = null;
  let filter = '';

  const picker = libPicker({ libraries: srcInfo?.allowedLibraries || [], defaults: srcInfo?.defaultLibraries || [] });
  const sel = new Selection(() => bar.update());
  const bar = actionBar(sel, picker.el, h('button', {
    class: 'primary',
    onclick: async () => {
      const res = await submitJobs([...sel.items.values()], picker.value());
      if (res) {
        sel.clear();
        refresh();
      }
    },
  }, t('add.submit')));

  const header = h('div', { class: 'toolbar' });
  const body = h('div');
  const search = searchForm();
  root.replaceChildren(h('div', { class: 'view-head' }, intro('intro.browse'), search.form), header, body, bar.el);

  const srcSelect = h('select', {
    'aria-label': t('browse.source'),
    onchange: (e) => { location.hash = `#/browse${qs({ source: e.target.value })}`; },
  }, sources.map((s) => h('option', { value: s.source, selected: s.source === source }, s.source)));

  function renderHeader() {
    const crumbs = h('nav', { class: 'crumbs', 'aria-label': 'breadcrumb' });
    (data?.breadcrumbs || [{ name: source, ref: '' }]).forEach((c, i) => {
      if (i > 0) crumbs.append(h('span', { class: 'sep' }, '›'));
      crumbs.append(h('a', { href: `#/browse${qs({ source, ref: c.ref })}` }, c.name));
    });
    header.replaceChildren(
      h('label', {}, `${t('browse.source')}: `, srcSelect),
      crumbs,
      h('span', { class: 'grow' }),
      h('button', {
        onclick: async () => {
          try {
            await api('POST', '/api/index/refresh', { source });
            toast(t('index.refreshStarted'));
            loadStatus();
          } catch (e) { showError(e); }
        },
      }, t('index.refresh')),
    );
  }

  function register(r, update) {
    if (!cards.has(r)) cards.set(r, []);
    cards.get(r).push(update);
  }

  async function compare(entryRef, state) {
    if (compared.has(entryRef) || !state.libraries.length) return;
    compared.add(entryRef);
    try {
      const res = await api('GET', `/api/compare${qs({ source, ref: entryRef })}`);
      differs.set(entryRef, res.libraries.filter((l) => l.differs).map((l) => l.library));
      if (entryRef === ref && data?.isAlbum) renderAlbum(res);
      for (const u of cards.get(entryRef) || []) u(state, differs.get(entryRef));
    } catch {
      // 比べられなくても表示は続ける。
    }
  }

  function card(a, label) {
    const c = albumCard({
      kind: 'source', name: source, path: a.path, ref: a.ref, label,
      href: `#/browse${qs({ source, ref: a.ref })}`,
      sel, selValue: { source, ref: a.ref }, state: a.state, differs: differs.get(a.ref),
    });
    register(a.ref, (st, diff) => c.update(st, diff ?? differs.get(a.ref)));
    compare(a.ref, a.state);
    return c.el;
  }

  // source の直下: アーティストの一覧
  function renderArtists() {
    const albumsHere = data.dirs.filter((d) => d.isAlbum);
    const artists = data.dirs.filter((d) => !d.isAlbum);
    const f = filter.toLowerCase();
    const shown = artists.filter((d) => !f || d.name.toLowerCase().includes(f));
    const filterInput = h('input', {
      type: 'search', id: 'artist-filter', value: filter, placeholder: t('browse.filterArtists'),
      oninput: (e) => {
        filter = e.target.value;
        renderArtists();
        const el = document.getElementById('artist-filter');
        el.focus();
        el.setSelectionRange(filter.length, filter.length);
      },
    });
    const tiles = shown.map((d) => {
      const tile = h('div', { class: 'card artist' },
        h('a', { class: 'cover-link', href: `#/browse${qs({ source, ref: d.ref })}` }, mosaic(source, d.covers || [], d.name)),
        sel.checkbox(`${source}\u0000${d.ref}`, { source, ref: d.ref }, d.name),
        h('div', { class: 'card-body' },
          h('a', { class: 'card-title', href: `#/browse${qs({ source, ref: d.ref })}`, title: d.name }, d.name),
          h('div', { class: 'card-sub' }, d.state.albumsTotal >= 0
            ? t('browse.artistMeta', { n: d.state.albumsTotal, added: d.state.albumsAdded })
            : ''),
          d.state.albumsAdded > 0 ? h('div', { class: 'meter', title: t('status.albumsAdded', { added: d.state.albumsAdded, total: d.state.albumsTotal }) }, meterBar(d.state.albumsAdded / Math.max(1, d.state.albumsTotal))) : ''));
      if (sel.has(`${source}\u0000${d.ref}`)) tile.classList.add('selected');
      return tile;
    });
    body.replaceChildren(
      section(t('browse.artists'), t('browse.artistsCount', { n: artists.length }),
        shown.length ? grid(tiles) : h('div', { class: 'muted' }, t('browse.empty')),
        filterInput),
      albumsHere.length ? section(t('browse.albums'), '', grid(albumsHere.map((a) => card(a, a.name)))) : '',
    );
  }

  // アーティストなど、アルバムでないディレクトリ
  function renderGroups() {
    const direct = data.dirs.filter((d) => d.isAlbum);
    const groups = data.dirs.filter((d) => !d.isAlbum && d.albums?.length);
    const empty = data.dirs.filter((d) => !d.isAlbum && !d.albums?.length);
    const total = direct.length + groups.reduce((n, g) => n + g.albums.length, 0);
    const added = direct.filter((d) => d.state.libraries.length).length
      + groups.reduce((n, g) => n + g.albums.filter((a) => a.state.libraries.length).length, 0);
    const selectAll = (list) => h('button', {
      class: 'link',
      onclick: () => {
        const on = !list.every((a) => sel.has(`${source}\u0000${a.ref}`));
        for (const a of list) sel.set(`${source}\u0000${a.ref}`, { source, ref: a.ref }, on);
      },
    }, t('browse.selectSection'));
    const parts = [
      h('div', { class: 'page-head' },
        h('h2', {}, baseName(data.path)),
        h('div', { class: 'muted' }, t('browse.artistMeta', { n: total, added }))),
    ];
    if (direct.length) {
      parts.push(section(t('browse.albums'), t('browse.albumsCount', { n: direct.length }),
        grid(direct.map((a) => card(a, a.name))), selectAll(direct)));
    }
    for (const g of groups) {
      parts.push(section(
        h('a', { href: `#/browse${qs({ source, ref: g.ref })}` }, g.name),
        t('browse.albumsCount', { n: g.albums.length }),
        grid(g.albums.map((a) => card(a, a.name))),
        selectAll(g.albums)));
    }
    if (empty.length) {
      parts.push(section(t('browse.otherFolders'), '', h('div', { class: 'folders' },
        empty.map((d) => h('a', { class: 'folder-chip', href: `#/browse${qs({ source, ref: d.ref })}` }, d.name)))));
    }
    if (!direct.length && !groups.length && !empty.length) parts.push(h('div', { class: 'muted' }, t('browse.noAlbums')));
    body.replaceChildren(...parts);
  }

  // アルバムの詳細
  let albumInfo = null;
  function renderAlbum(cmp) {
    const { title, year } = splitYear(baseName(data.path));
    const parent = data.breadcrumbs[data.breadcrumbs.length - 2];
    const albumPicker = libPicker({ libraries: srcInfo?.allowedLibraries || [], defaults: srcInfo?.defaultLibraries || [] });
    const lines = [];
    if (cmp) {
      lines.push(h('div', {}, t('browse.compare', { files: cmp.stats.files, size: fmtBytes(cmp.stats.bytes) })));
      for (const l of cmp.libraries) {
        if (!l.stats) continue;
        lines.push(h('div', { class: l.differs ? 'error-text' : 'muted' },
          t('browse.compareLib', { lib: l.library, files: l.stats.files, size: fmtBytes(l.stats.bytes) })));
      }
    }
    const tracks = data.files.filter((f) => f.audio);
    const others = data.files.filter((f) => !f.audio);
    const subdirs = data.dirs;
    const info = h('div', { class: 'album-info' },
      parent ? h('a', { class: 'album-artist', href: `#/browse${qs({ source, ref: parent.ref })}` }, parent.name) : '',
      h('h2', { class: 'album-title' }, title),
      h('div', { class: 'muted' }, [year, t('lib.tracks', { n: tracks.length }), cmp ? fmtBytes(cmp.stats.bytes) : ''].filter(Boolean).join(' · ')),
      stateBadges(data.state, true, differs.get(ref)),
      h('div', { class: 'small' }, lines),
      h('div', { class: 'album-actions' },
        albumPicker.el,
        h('button', {
          class: 'primary',
          onclick: () => submitJobs([{ source, ref }], albumPicker.value()).then(refresh),
        }, data.state.libraries.length ? `${t('add.submitThis')} / ${t('add.resync')}` : t('add.submitThis'))));
    const detail = h('div', { class: 'album-detail' },
      h('div', { class: 'album-cover' }, cover('source', source, ref, baseName(data.path), 640, true)),
      info);
    albumInfo = info;
    const trackTable = h('div', { class: 'panel' },
      h('h3', {}, t('browse.tracks')),
      h('table', { class: 'tracks' },
        h('tbody', {}, tracks.map((f, i) => h('tr', {},
          h('td', { class: 'num muted' }, String(i + 1)),
          h('td', {}, f.name),
          h('td', { class: 'num muted' }, fmtBytes(f.size)))))),
      others.length || subdirs.length
        ? h('div', { class: 'small muted extra-files' },
          `${t('browse.otherFiles')}: `, [...subdirs.map((d) => `${d.name}/`), ...others.map((f) => f.name)].join(', '))
        : '');
    body.replaceChildren(detail, trackTable);
  }

  function render() {
    renderHeader();
    cards.clear();
    sel.resetBoxes();
    if (!data) {
      body.replaceChildren(h('div', { class: 'muted' }, t('browse.loading')));
      return;
    }
    if (data.isAlbum && ref) {
      bar.el.hidden = true;
      renderAlbum(null);
      compared.delete(ref);
      compare(ref, data.state);
      return;
    }
    bar.el.hidden = false;
    if (!ref) renderArtists(); else renderGroups();
  }

  async function load() {
    try {
      data = await api('GET', `/api/browse${qs({ source, ref })}`);
    } catch (e) {
      showError(e);
      body.replaceChildren(h('div', { class: 'error-text' }, errorText(e.code, e.message)));
      return;
    }
    if (!stopped) render();
  }

  // 状態だけを取り直し、カードを差し替える。画像は読み直さない。
  async function refresh() {
    let d;
    try {
      d = await api('GET', `/api/browse${qs({ source, ref })}`);
    } catch {
      return;
    }
    if (stopped) return;
    data = d;
    if (data.isAlbum && ref) {
      if (albumInfo) albumInfo.querySelector('.badges')?.replaceWith(stateBadges(data.state, true, differs.get(ref)));
      return;
    }
    if (!ref) {
      renderArtists();
      return;
    }
    for (const dir of data.dirs) {
      for (const a of dir.isAlbum ? [dir] : (dir.albums || [])) {
        for (const u of cards.get(a.ref) || []) u(a.state);
      }
    }
  }

  render();
  load();
  const timer = setInterval(() => {
    const busy = data && (data.state?.jobs?.length || data.dirs.some((d) => d.state.jobs.length || d.albums?.some((a) => a.state.jobs.length)));
    if (activeJobs() > 0 || busy) refresh();
  }, 3000);
  return () => {
    stopped = true;
    clearInterval(timer);
  };
}

function meterBar(ratio) {
  const el = h('div');
  el.style.width = `${Math.round(Math.min(1, ratio) * 100)}%`;
  return el;
}

// ---------- 検索 ----------

function viewSearch(root, params) {
  const q = params.get('q') || '';
  const { form, input } = searchForm(q);
  const results = h('div');
  const picker = libPicker({ libraries: status?.libraries || [], useDefaults: true });
  const sel = new Selection(() => bar.update());
  const bar = actionBar(sel, picker.el, h('button', {
    class: 'primary',
    onclick: async () => {
      const r = await submitJobs([...sel.items.values()], picker.value());
      if (r) {
        sel.clear();
        load();
      }
    },
  }, t('add.submit')));
  root.replaceChildren(
    h('div', { class: 'view-head' }, intro('intro.search'), form),
    h('div', { class: 'toolbar' }, h('a', { href: '#/browse' }, `‹ ${t('search.back')}`)),
    results, bar.el);
  input.focus();
  if (!q) {
    bar.el.hidden = true;
    return () => {};
  }

  async function load() {
    let res;
    try {
      res = await api('GET', `/api/search${qs({ q })}`);
    } catch (e) {
      showError(e);
      return;
    }
    sel.resetBoxes();
    const multi = new Set(res.results.map((r) => r.source)).size > 1;
    const artists = res.results.filter((r) => !r.isAlbum);
    const albums = res.results.filter((r) => r.isAlbum);
    // アルバムを、親のディレクトリ（アーティスト）ごとにまとめる。
    const groups = new Map();
    for (const a of albums) {
      const key = `${a.source}\u0000${a.parent}`;
      if (!groups.has(key)) groups.set(key, { source: a.source, parent: a.parent, items: [] });
      groups.get(key).items.push(a);
    }
    const parts = [];
    if (!res.indexReady) parts.push(h('div', { class: 'notice' }, t('index.notReady')));
    if (res.truncated) parts.push(h('div', { class: 'notice' }, t('search.truncated')));
    if (artists.length) {
      parts.push(section(t('search.artists'), t('browse.artistsCount', { n: artists.length }), h('div', { class: 'folders' },
        artists.map((a) => h('a', { class: 'folder-chip', href: `#/browse${qs({ source: a.source, ref: a.ref })}` },
          a.path, multi ? h('span', { class: 'muted small' }, ` · ${a.source}`) : '',
          a.state.albumsTotal > 0 ? h('span', { class: 'muted small' }, ` · ${t('status.albumsAdded', { added: a.state.albumsAdded, total: a.state.albumsTotal })}`) : '')))));
    }
    for (const g of groups.values()) {
      parts.push(section(
        g.parent ? h('a', { href: `#/browse${qs({ source: g.source, ref: parentRef(g.items[0].ref) })}` }, g.parent) : g.source,
        multi ? g.source : '',
        grid(g.items.map((a) => albumCard({
          kind: 'source', name: a.source, path: a.path, ref: a.ref, label: a.name,
          href: `#/browse${qs({ source: a.source, ref: a.ref })}`,
          sel, selValue: { source: a.source, ref: a.ref }, state: a.state,
        }).el))));
    }
    if (!artists.length && !albums.length) parts.push(h('div', { class: 'muted' }, t('search.none')));
    results.replaceChildren(...parts);
  }
  load();
  return () => {};
}

// parentRef はアルバムの ref から、親のディレクトリの ref をバイト列のまま作る。
function parentRef(ref) {
  const bin = atob(ref.replace(/-/g, '+').replace(/_/g, '/'));
  const i = bin.lastIndexOf('/');
  if (i <= 0) return '';
  return btoa(bin.slice(0, i)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// ---------- ライブラリ ----------

function viewLibraries(root, params) {
  const name = params.get('name') || status?.libraries?.[0];
  const summary = h('div', { class: 'panel' });
  const tabs = h('div', { class: 'cards' });
  const albums = h('div');
  const trashPanel = h('div', { class: 'panel' });
  const sel = new Selection(() => bar.update());
  const bar = actionBar(sel, h('button', {
    class: 'danger',
    onclick: async () => {
      if (!sel.size) {
        toast(t('add.noSelection'), true);
        return;
      }
      if (!confirm(t('lib.deleteConfirm', { n: sel.size, lib: name }))) return;
      try {
        const res = await api('POST', `/api/libraries/${encodeURIComponent(name)}/delete`, {
          items: [...sel.items.values()].map((ref) => ({ ref })),
        });
        const ok = res.results.filter((r) => r.result === 'deleted').length;
        const errs = res.results.filter((r) => r.result === 'error');
        const lines = [t('lib.deleted', { n: ok })];
        for (const e of errs.slice(0, 5)) lines.push(`${e.path}: ${errorText(e.error, e.message)}`);
        toast(lines.join('\n'), errs.length > 0);
        sel.clear();
        loadAll();
      } catch (e) { showError(e); }
    },
  }, t('lib.delete')));
  root.replaceChildren(intro('intro.libraries'), summary, tabs, albums, trashPanel, bar.el);
  let filter = '';
  let onlyOrphan = false;
  let list = null;

  async function loadSummary() {
    try {
      const res = await api('GET', '/api/libraries');
      const u = res.usage;
      const usedPct = u ? (u.used / Math.max(1, u.total)) : 0;
      summary.replaceChildren(
        h('div', { class: 'toolbar' },
          u ? h('span', {}, t('lib.usage', { used: fmtBytes(u.used), total: fmtBytes(u.total), free: fmtBytes(u.free) })) : '',
          h('span', {}, t('lib.trash', { size: fmtBytes(res.trashBytes) })),
          h('span', { class: 'grow' }),
          h('button', {
            class: 'danger', disabled: !res.trashBytes,
            onclick: async () => {
              if (!confirm(t('lib.emptyTrashConfirm'))) return;
              try {
                await api('POST', '/api/trash/empty');
                loadAll();
              } catch (e) { showError(e); }
            },
          }, t('lib.emptyTrash'))),
        u ? h('div', { class: 'usage' }, meterBar(usedPct)) : '',
        h('div', { class: 'small muted' }, t('trash.hintSpace')));
      tabs.replaceChildren(...res.libraries.map((l) => h('a', {
        class: l.name === name ? 'card tab active' : 'card tab',
        href: `#/libraries${qs({ name: l.name })}`,
      },
      h('div', { class: 'card-title' }, l.name),
      h('div', {}, t('lib.albums', { n: l.albums, size: fmtBytes(l.bytes) })),
      h('div', { class: 'small muted' }, t('lib.scannedAt', { time: fmtTime(l.scannedAt) })),
      l.error ? h('div', { class: 'small error-text' }, l.error) : '')));
    } catch (e) { showError(e); }
  }

  function renderAlbums() {
    sel.resetBoxes();
    if (!name) {
      albums.replaceChildren(h('div', { class: 'muted' }, t('lib.select')));
      return;
    }
    if (!list) {
      albums.replaceChildren(h('div', { class: 'muted' }, t('browse.loading')));
      return;
    }
    const f = filter.toLowerCase();
    const shown = list.albums.filter((a) => (!onlyOrphan || a.orphan) && (!f || a.path.toLowerCase().includes(f)));
    const orphanCount = list.albums.filter((a) => a.orphan).length;
    const groups = new Map();
    for (const a of shown) {
      const p = parentPath(a.path);
      if (!groups.has(p)) groups.set(p, []);
      groups.get(p).push(a);
    }
    const filterInput = h('input', {
      type: 'search', id: 'lib-filter', value: filter, placeholder: t('lib.filter'),
      oninput: (e) => {
        filter = e.target.value;
        renderAlbums();
        const el = document.getElementById('lib-filter');
        el.focus();
        el.setSelectionRange(filter.length, filter.length);
      },
    });
    const parts = [h('div', { class: 'toolbar' },
      h('h2', {}, name),
      filterInput,
      h('label', { class: 'chip' }, h('input', {
        type: 'checkbox', checked: onlyOrphan,
        onchange: (e) => { onlyOrphan = e.target.checked; renderAlbums(); },
      }), t('lib.onlyOrphan'), orphanCount ? h('span', { class: 'count-pill' }, String(orphanCount)) : ''),
      h('span', { class: 'grow' }),
      h('button', { onclick: () => loadAlbums(true) }, t('lib.rescan')))];
    if (!shown.length) parts.push(h('div', { class: 'muted' }, t('lib.noAlbums')));
    for (const [artist, items] of groups) {
      parts.push(section(artist || name, t('browse.albumsCount', { n: items.length }), grid(items.map((a) => {
        const ribbon = a.job
          ? { kind: a.job.status === 'running' ? 'run' : 'queue', text: t(`jobs.status.${a.job.status}`) }
          : (a.orphan ? { kind: 'warn', text: t('lib.orphan') } : null);
        const href = a.sources.length ? `#/browse${qs({ source: a.sources[0], ref: a.ref })}` : '';
        const c = albumCard({
          kind: 'library', name, path: a.path, ref: a.ref, label: baseName(a.path), href,
          sel: a.job ? null : sel, selValue: a.ref,
          meta: `${t('lib.tracks', { n: a.stats.tracks })} · ${fmtBytes(a.stats.bytes)}`,
          extraRibbon: ribbon || { kind: 'none', text: '' },
        });
        return c.el;
      }))));
    }
    albums.replaceChildren(...parts);
  }

  async function loadAlbums(refresh = false) {
    if (!name) {
      renderAlbums();
      return;
    }
    try {
      list = await api('GET', `/api/libraries/${encodeURIComponent(name)}/albums${qs({ refresh: refresh ? '1' : '' })}`);
    } catch (e) {
      showError(e);
      return;
    }
    renderAlbums();
  }

  async function loadTrash() {
    try {
      const res = await api('GET', '/api/trash');
      trashPanel.replaceChildren(
        h('h2', {}, `${t('trash.title')} (${fmtBytes(res.bytes)})`),
        res.entries.length
          ? h('div', { class: 'table-wrap' }, h('table', {},
            h('thead', {}, h('tr', {},
              h('th', {}, t('jobs.library')), h('th', {}, t('search.path')), h('th', {}, t('browse.state')),
              h('th', { class: 'num' }, t('browse.size')), h('th', {}, t('trash.deletedAt')),
              h('th', {}, t('trash.expiresAt')), h('th', {}, t('jobs.actions')))),
            h('tbody', {}, res.entries.map((e) => h('tr', {},
              h('td', {}, e.library),
              h('td', { class: 'path' }, e.path),
              h('td', {}, h('span', { class: 'badge muted' }, t(`trash.reason.${e.reason}`))),
              h('td', { class: 'num' }, fmtBytes(e.bytes)),
              h('td', {}, fmtTime(e.deletedAt)),
              h('td', {}, fmtTime(e.expiresAt)),
              h('td', { class: 'nowrap' },
                h('button', {
                  onclick: async () => {
                    try {
                      await api('POST', `/api/trash/${encodeURIComponent(e.id)}/restore`);
                      toast(t('trash.restored'));
                      loadAll();
                    } catch (err) { showError(err); }
                  },
                }, t('trash.restore')), ' ',
                h('button', {
                  class: 'danger',
                  onclick: async () => {
                    if (!confirm(t('trash.removeConfirm'))) return;
                    try {
                      await api('POST', `/api/trash/${encodeURIComponent(e.id)}/delete`);
                      loadAll();
                    } catch (err) { showError(err); }
                  },
                }, t('trash.remove'))))))))
          : h('div', { class: 'muted' }, t('trash.empty')),
      );
    } catch (e) { showError(e); }
  }

  function loadAll() {
    loadSummary();
    loadAlbums(true);
    loadTrash();
  }

  renderAlbums();
  loadSummary();
  loadAlbums(false);
  loadTrash();
  return () => {};
}

// ---------- ジョブ ----------

function viewJobs(root, params) {
  const filter = params.get('status') || '';
  const statuses = ['', 'queued', 'running', 'completed', 'failed', 'canceled'];
  const select = h('select', {
    'aria-label': t('jobs.status'),
    onchange: (e) => { location.hash = `#/jobs${qs({ status: e.target.value })}`; },
  }, statuses.map((s) => h('option', { value: s, selected: s === filter }, s ? t(`jobs.status.${s}`) : t('jobs.filter.all'))));
  const countsEl = h('span', { class: 'muted' });
  const table = h('div', { class: 'panel' });
  root.replaceChildren(intro('intro.jobs'), h('div', { class: 'toolbar' }, select, countsEl), table);

  const badgeClass = { queued: 'info', running: 'warn', completed: 'ok', failed: 'bad', canceled: 'muted' };
  // 画像は行を作り直しても読み直さないよう、使い回す。
  const thumbs = new Map();
  const thumb = (j) => {
    const key = `${j.source}\u0000${j.ref}`;
    if (!thumbs.has(key)) thumbs.set(key, cover('source', j.source, j.ref, baseName(j.path), 160));
    return thumbs.get(key);
  };

  async function load() {
    let res;
    try {
      res = await api('GET', '/api/jobs');
    } catch (e) {
      showError(e);
      return;
    }
    countsEl.textContent = statuses.slice(1).map((s) => `${t(`jobs.status.${s}`)} ${res.counts[s] || 0}`).join(' / ');
    const jobs = res.jobs.filter((j) => !filter || j.status === filter);
    if (!jobs.length) {
      table.replaceChildren(h('div', { class: 'muted' }, t('jobs.none')));
      return;
    }
    table.replaceChildren(h('div', { class: 'table-wrap' }, h('table', { class: 'jobs' },
      h('thead', {}, h('tr', {},
        h('th', {}), h('th', {}, t('jobs.path')),
        h('th', {}, t('jobs.library')), h('th', {}, t('jobs.status')), h('th', {}, t('jobs.progress')),
        h('th', {}, t('jobs.created')), h('th', {}, t('jobs.actions')))),
      h('tbody', {}, jobs.map((j) => {
        const pct = j.bytesTotal > 0 ? Math.min(100, (100 * j.bytesDone) / j.bytesTotal) : (j.status === 'completed' ? 100 : 0);
        const progress = (j.status === 'running' || j.bytesTotal > 0)
          ? h('div', {},
            h('div', { class: 'progress' }, meterBar(pct / 100)),
            h('div', { class: 'small muted' }, t('jobs.progressText', {
              doneSize: fmtBytes(j.bytesDone), totalSize: fmtBytes(j.bytesTotal),
              doneFiles: j.filesDone, totalFiles: j.filesTotal,
            })))
          : '';
        const actions = [];
        if (j.status === 'queued' || j.status === 'running') {
          actions.push(h('button', {
            onclick: async () => {
              try { await api('POST', `/api/jobs/${encodeURIComponent(j.id)}/cancel`); load(); } catch (e) { showError(e); }
            },
          }, t('jobs.cancel')));
        }
        if (j.status === 'failed' || j.status === 'canceled') {
          actions.push(h('button', {
            onclick: async () => {
              try { await api('POST', `/api/jobs/${encodeURIComponent(j.id)}/retry`); load(); loadStatus(); } catch (e) { showError(e); }
            },
          }, t('jobs.retry')));
        }
        const { title } = splitYear(baseName(j.path));
        return h('tr', {},
          h('td', { class: 'thumb' }, thumb(j)),
          h('td', { class: 'path' },
            h('a', { class: 'job-title', href: `#/browse${qs({ source: j.source, ref: j.ref })}` }, title),
            h('div', { class: 'small muted' }, `${j.source} · ${parentPath(j.path)}`)),
          h('td', {}, j.library),
          h('td', {},
            h('div', { class: 'badges' },
              h('span', { class: `badge ${badgeClass[j.status] || 'muted'}` }, t(`jobs.status.${j.status}`)),
              j.resync ? h('span', { class: 'badge muted' }, t('jobs.resync')) : ''),
            j.status === 'failed'
              ? h('div', { class: 'small error-text', title: j.errorMessage || '' },
                errorText(j.errorCode, j.errorMessage), j.errorMessage ? h('div', { class: 'muted' }, j.errorMessage) : '',
                j.errorCode === 'insufficient_space' ? h('div', {}, h('a', { href: '#/libraries' }, t('lib.emptyTrash'))) : '')
              : ''),
          h('td', {}, progress),
          h('td', { class: 'small muted' }, fmtTime(j.createdAt)),
          h('td', { class: 'nowrap' }, actions));
      })))));
  }
  load();
  const timer = setInterval(load, 2000);
  return () => clearInterval(timer);
}

// ---------- 一括追加 ----------

function viewBulk(root) {
  const area = h('textarea', { id: 'bulk-text', spellcheck: 'false', placeholder: 'music\tArtist/Album\nmusic\tArtist/Other Album\tshared,alice' });
  try {
    area.value = sessionStorage.getItem('bulk') || '';
  } catch {
    // 保存できなくても使える。
  }
  area.addEventListener('input', () => {
    try { sessionStorage.setItem('bulk', area.value); } catch { /* 無視する */ }
  });
  // テキストエリアで Tab を押したら、移動せずにタブ文字を入れる。
  area.addEventListener('keydown', (e) => {
    if (e.key === 'Tab' && !e.shiftKey) {
      e.preventDefault();
      const { selectionStart: s, selectionEnd: en } = area;
      area.setRangeText('\t', s, en, 'end');
    }
  });
  const result = h('div');
  root.replaceChildren(intro('intro.bulk'), h('div', { class: 'panel' },
    h('p', { class: 'muted' }, t('bulk.help')),
    area,
    h('div', { class: 'toolbar' }, h('button', { class: 'primary', onclick: check }, t('bulk.check')))),
  result);

  async function check() {
    let res;
    try {
      res = await api('POST', '/api/bulk/check', { text: area.value });
    } catch (e) {
      showError(e);
      return;
    }
    const found = res.rows.filter((r) => r.status === 'found');
    const cls = { found: 'ok', not_found: 'warn', invalid: 'bad' };
    const label = { found: t('bulk.found'), not_found: t('bulk.notFound'), invalid: t('bulk.invalid') };
    result.replaceChildren(h('div', { class: 'panel' },
      h('div', { class: 'toolbar' },
        h('span', {}, t('bulk.summary', {
          found: res.counts.found || 0, notFound: res.counts.not_found || 0, invalid: res.counts.invalid || 0,
        })),
        h('span', { class: 'grow' }),
        h('button', {
          class: 'primary', disabled: !found.length,
          onclick: async () => {
            const r = await submitJobs(found.map((row) => ({ source: row.source, ref: row.ref, libraries: row.libraries })), null);
            if (r) location.hash = '#/jobs';
          },
        }, t('bulk.submit', { n: found.length }))),
      h('div', { class: 'table-wrap' }, h('table', {},
        h('thead', {}, h('tr', {},
          h('th', { class: 'num' }, t('bulk.line')), h('th', {}), h('th', {}, t('jobs.source')), h('th', {}, t('jobs.path')),
          h('th', {}, t('jobs.library')), h('th', {}, t('jobs.status')))),
        h('tbody', {}, res.rows.map((r) => h('tr', {},
          h('td', { class: 'num' }, r.line),
          h('td', { class: 'thumb' }, r.status === 'found' && r.albums === 1 ? cover('source', r.source, r.ref, baseName(r.path), 160) : ''),
          h('td', {}, r.source),
          h('td', { class: 'path' }, r.status === 'found'
            ? h('a', { href: `#/browse${qs({ source: r.source, ref: r.ref })}` }, r.path)
            : (r.path || r.text)),
          h('td', {}, r.libraries.join(', ')),
          h('td', {},
            h('div', { class: 'badges' },
              h('span', { class: `badge ${cls[r.status]}` }, label[r.status]),
              r.albums > 1 ? h('span', { class: 'badge muted' }, t('bulk.albums', { n: r.albums })) : '',
              r.present.length ? h('span', { class: 'badge info' }, t('bulk.present', { libs: r.present.join(', ') })) : ''),
            r.error ? h('div', { class: 'small error-text' }, errorText(r.error, r.message), r.message ? ` (${r.message})` : '') : ''))))))));
  }
  return () => {};
}

// ---------- 画面の切り替え ----------

const routes = {
  browse: { label: 'nav.browse', view: viewBrowse },
  search: { label: 'nav.search', view: viewSearch, navAs: 'browse' },
  libraries: { label: 'nav.libraries', view: viewLibraries },
  jobs: { label: 'nav.jobs', view: viewJobs },
  bulk: { label: 'nav.bulk', view: viewBulk },
};

let cleanup = () => {};

function parseHash() {
  const raw = location.hash.replace(/^#\/?/, '');
  const [name, query] = raw.split('?');
  return { name: routes[name] ? name : 'browse', params: new URLSearchParams(query || '') };
}

function renderNav(active) {
  const nav = document.getElementById('nav');
  const current = routes[active]?.navAs || active;
  nav.replaceChildren(...Object.entries(routes).filter(([, r]) => !r.navAs).map(([name, r]) => {
    const a = h('a', { href: `#/${name}`, class: name === current ? 'active' : '' }, t(r.label));
    if (name === 'jobs' && activeJobs() > 0) a.append(h('span', { class: 'nav-pill' }, String(activeJobs())));
    return a;
  }));
}

function route() {
  cleanup();
  const { name, params } = parseHash();
  renderNav(name);
  const view = document.getElementById('view');
  window.scrollTo(0, 0);
  cleanup = routes[name].view(view, params) || (() => {});
}

function initLang() {
  const sel = document.getElementById('lang');
  sel.replaceChildren(...languages.map((l) => h('option', { value: l.code, selected: l.code === lang() }, l.label)));
  sel.addEventListener('change', () => {
    setLang(sel.value);
    document.documentElement.lang = sel.value;
    renderIndexStatus();
    route();
  });
  document.documentElement.lang = lang();
}

async function main() {
  initLang();
  await loadStatus();
  window.addEventListener('hashchange', route);
  route();
  setInterval(async () => {
    await loadStatus();
    renderNav(parseHash().name);
  }, 3000);
}

main();
