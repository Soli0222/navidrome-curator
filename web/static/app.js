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

// ---------- 全体の状態 ----------

let status = null;
const statusListeners = new Set();

async function loadStatus() {
  try {
    status = await api('GET', '/api/status');
    renderIndexStatus();
    for (const f of statusListeners) f(status);
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

// ---------- 共通の部品 ----------

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
    el.append(h('label', {}, b, name));
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

function selectAllBox(getBoxes) {
  return h('input', {
    type: 'checkbox', title: t('common.selectAll'),
    onchange: (ev) => {
      for (const b of getBoxes()) {
        if (!b.disabled) {
          b.checked = ev.target.checked;
          b.dispatchEvent(new Event('change'));
        }
      }
    },
  });
}

// ---------- 閲覧 ----------

function viewBrowse(root, params) {
  let stopped = false;
  const sources = status?.sources || [];
  const source = params.get('source') || sources[0]?.source;
  const ref = params.get('ref') || '';
  const srcInfo = sources.find((s) => s.source === source);
  const selected = new Map(); // ref -> path
  const differs = new Map(); // ref -> [library]
  const compared = new Set();
  let data = null;
  let picker = null;

  const header = h('div', { class: 'toolbar' });
  const albumPanel = h('div');
  const listPanel = h('div', { class: 'panel' });
  const filesPanel = h('div');
  root.replaceChildren(header, albumPanel, listPanel, filesPanel);

  const srcSelect = h('select', {
    onchange: (e) => { location.hash = `#/browse${qs({ source: e.target.value })}`; },
  }, sources.map((s) => h('option', { value: s.source, selected: s.source === source }, s.source)));

  function renderHeader() {
    const crumbs = h('div', { class: 'crumbs' });
    (data?.breadcrumbs || [{ name: source, ref: '' }]).forEach((c, i) => {
      if (i > 0) crumbs.append(h('span', { class: 'sep' }, '/'));
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

  async function compare(entryRef) {
    if (compared.has(entryRef)) return;
    compared.add(entryRef);
    try {
      const res = await api('GET', `/api/compare${qs({ source, ref: entryRef })}`);
      differs.set(entryRef, res.libraries.filter((l) => l.differs).map((l) => l.library));
      if (entryRef === ref) renderAlbum(res);
      if (!stopped) renderList();
    } catch {
      // 比べられなくても一覧は出す。
    }
  }

  function renderAlbum(cmp) {
    if (!data?.isAlbum || !ref) {
      albumPanel.replaceChildren();
      return;
    }
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
    albumPanel.replaceChildren(h('div', { class: 'panel' },
      h('h3', {}, `${t('browse.thisAlbum')}: ${data.path}`),
      stateBadges(data.state, true, differs.get(ref)),
      h('div', { class: 'small' }, lines),
      h('div', { class: 'toolbar' },
        albumPicker.el,
        h('button', {
          class: 'primary',
          onclick: () => submitJobs([{ source, ref }], albumPicker.value()).then(reload),
        }, data.state.libraries.length ? `${t('add.submitThis')} / ${t('add.resync')}` : t('add.submitThis')),
      ),
    ));
  }

  function renderList() {
    if (!data) {
      listPanel.replaceChildren(h('div', { class: 'muted' }, t('browse.loading')));
      return;
    }
    if (!picker) picker = libPicker({ libraries: srcInfo?.allowedLibraries || [], defaults: srcInfo?.defaultLibraries || [] });
    const boxes = [];
    const countEl = h('span', { class: 'muted' }, t('add.selected', { n: selected.size }));
    const rows = data.dirs.map((d) => {
      const box = h('input', {
        type: 'checkbox', checked: selected.has(d.ref),
        onchange: (e) => {
          if (e.target.checked) selected.set(d.ref, d.path); else selected.delete(d.ref);
          countEl.textContent = t('add.selected', { n: selected.size });
        },
      });
      boxes.push(box);
      if (d.isAlbum && d.state.libraries.length) compare(d.ref);
      return h('tr', {},
        h('td', { class: 'check' }, box),
        h('td', {}, h('a', { href: `#/browse${qs({ source, ref: d.ref })}` }, d.name)),
        h('td', { class: 'muted' }, d.isAlbum ? t('browse.album') : t('browse.folder')),
        h('td', {}, stateBadges(d.state, d.isAlbum, differs.get(d.ref))),
      );
    });
    listPanel.replaceChildren(
      h('div', { class: 'toolbar' },
        picker.el,
        h('span', { class: 'grow' }),
        countEl,
        h('button', {
          class: 'primary',
          onclick: async () => {
            const items = [...selected.keys()].map((r) => ({ source, ref: r }));
            const res = await submitJobs(items, picker.value());
            if (res) {
              selected.clear();
              reload();
            }
          },
        }, t('add.submit')),
      ),
      data.dirs.length
        ? h('table', {},
          h('thead', {}, h('tr', {},
            h('th', { class: 'check' }, selectAllBox(() => boxes)),
            h('th', {}, t('browse.name')), h('th', {}, t('browse.kind')), h('th', {}, t('browse.state')))),
          h('tbody', {}, rows))
        : h('div', { class: 'muted' }, t('browse.empty')),
    );
  }

  function renderFiles() {
    if (!data?.files.length) {
      filesPanel.replaceChildren();
      return;
    }
    filesPanel.replaceChildren(h('div', { class: 'panel' },
      h('h3', {}, t('browse.files')),
      h('table', {},
        h('thead', {}, h('tr', {}, h('th', {}, t('browse.name')), h('th', { class: 'num' }, t('browse.size')))),
        h('tbody', {}, data.files.map((f) => h('tr', {},
          h('td', { class: f.audio ? '' : 'muted' }, f.name),
          h('td', { class: 'num' }, fmtBytes(f.size))))))));
  }

  async function reload() {
    try {
      data = await api('GET', `/api/browse${qs({ source, ref })}`);
    } catch (e) {
      showError(e);
      listPanel.replaceChildren(h('div', { class: 'error-text' }, errorText(e.code, e.message)));
      return;
    }
    if (stopped) return;
    renderHeader();
    renderList();
    renderFiles();
    if (data.isAlbum && ref) {
      renderAlbum(null);
      compared.delete(ref);
      compare(ref);
    } else {
      renderAlbum(null);
    }
  }

  renderHeader();
  renderList();
  reload();
  // コピーが進んでいる間は状態を取り直す。
  const timer = setInterval(() => {
    if (activeJobs() > 0 || data?.dirs.some((d) => d.state.jobs.length)) {
      // 状態だけが変わるので、比べた結果は取り直さない。
      api('GET', `/api/browse${qs({ source, ref })}`).then((d) => {
        if (stopped) return;
        data = d;
        renderList();
        if (data.isAlbum && ref) renderAlbum(null);
      }).catch(() => {});
    }
  }, 3000);
  return () => {
    stopped = true;
    clearInterval(timer);
  };
}

// ---------- 検索 ----------

function viewSearch(root, params) {
  const q = params.get('q') || '';
  const input = h('input', { type: 'search', value: q, placeholder: t('search.placeholder') });
  const form = h('form', {
    class: 'toolbar',
    onsubmit: (e) => {
      e.preventDefault();
      location.hash = `#/search${qs({ q: input.value.trim() })}`;
    },
  }, input, h('button', { class: 'primary', type: 'submit' }, t('search.submit')));
  const results = h('div', { class: 'panel' });
  root.replaceChildren(form, results);
  input.focus();
  if (!q) {
    results.remove();
    return () => {};
  }
  const selected = new Map();
  const picker = libPicker({ libraries: status?.libraries || [], useDefaults: true });

  async function load() {
    let res;
    try {
      res = await api('GET', `/api/search${qs({ q })}`);
    } catch (e) {
      showError(e);
      return;
    }
    const boxes = [];
    const countEl = h('span', { class: 'muted' }, t('add.selected', { n: selected.size }));
    const key = (r) => `${r.source}\u0000${r.ref}`;
    const rows = res.results.map((r) => {
      const box = h('input', {
        type: 'checkbox', checked: selected.has(key(r)),
        onchange: (e) => {
          if (e.target.checked) selected.set(key(r), { source: r.source, ref: r.ref }); else selected.delete(key(r));
          countEl.textContent = t('add.selected', { n: selected.size });
        },
      });
      boxes.push(box);
      return h('tr', {},
        h('td', { class: 'check' }, box),
        h('td', {}, r.source),
        h('td', { class: 'path' },
          h('a', { href: `#/browse${qs({ source: r.source, ref: r.ref })}` }, r.path)),
        h('td', { class: 'muted' }, r.isAlbum ? t('browse.album') : t('browse.folder')),
        h('td', {}, stateBadges(r.state, r.isAlbum)),
      );
    });
    results.replaceChildren(
      res.indexReady ? '' : h('div', { class: 'notice' }, t('index.notReady')),
      res.truncated ? h('div', { class: 'notice' }, t('search.truncated')) : '',
      h('div', { class: 'toolbar' },
        picker.el, h('span', { class: 'grow' }), countEl,
        h('button', {
          class: 'primary',
          onclick: async () => {
            const r = await submitJobs([...selected.values()], picker.value());
            if (r) {
              selected.clear();
              load();
            }
          },
        }, t('add.submit'))),
      res.results.length
        ? h('table', {},
          h('thead', {}, h('tr', {},
            h('th', { class: 'check' }, selectAllBox(() => boxes)),
            h('th', {}, t('browse.source')), h('th', {}, t('search.path')),
            h('th', {}, t('browse.kind')), h('th', {}, t('browse.state')))),
          h('tbody', {}, rows))
        : h('div', { class: 'muted' }, t('search.none')),
    );
  }
  load();
  return () => {};
}

// ---------- ライブラリ ----------

function viewLibraries(root, params) {
  const name = params.get('name') || status?.libraries?.[0];
  const summary = h('div', { class: 'panel' });
  const cards = h('div', { class: 'cards' });
  const albums = h('div', { class: 'panel' });
  const trashPanel = h('div', { class: 'panel' });
  root.replaceChildren(summary, cards, albums, trashPanel);
  const selected = new Map();
  let filter = '';
  let onlyOrphan = false;
  let list = null;

  async function loadSummary() {
    try {
      const res = await api('GET', '/api/libraries');
      const u = res.usage;
      summary.replaceChildren(h('div', { class: 'toolbar' },
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
      h('div', { class: 'small muted' }, t('trash.hintSpace')));
      cards.replaceChildren(...res.libraries.map((l) => h('div', {
        class: l.name === name ? 'card active' : 'card',
        onclick: () => { location.hash = `#/libraries${qs({ name: l.name })}`; },
      },
      h('div', { class: 'title' }, l.name),
      h('div', {}, t('lib.albums', { n: l.albums, size: fmtBytes(l.bytes) })),
      h('div', { class: 'small muted' }, t('lib.scannedAt', { time: fmtTime(l.scannedAt) })),
      l.error ? h('div', { class: 'small error-text' }, l.error) : '')));
    } catch (e) { showError(e); }
  }

  function renderAlbums() {
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
    const boxes = [];
    const countEl = h('span', { class: 'muted' }, t('add.selected', { n: selected.size }));
    const filterInput = h('input', {
      type: 'search', value: filter, placeholder: t('lib.filter'),
      oninput: (e) => {
        filter = e.target.value;
        renderAlbums();
        const el = albums.querySelector('input[type="search"]');
        el.focus();
        el.setSelectionRange(filter.length, filter.length);
      },
    });
    albums.replaceChildren(
      h('div', { class: 'toolbar' },
        h('h2', {}, name),
        filterInput,
        h('label', {}, h('input', {
          type: 'checkbox', checked: onlyOrphan,
          onchange: (e) => { onlyOrphan = e.target.checked; renderAlbums(); },
        }), t('lib.onlyOrphan')),
        h('span', { class: 'grow' }),
        h('button', { onclick: () => loadAlbums(true) }, t('lib.rescan')),
        countEl,
        h('button', {
          class: 'danger',
          onclick: async () => {
            if (!selected.size) {
              toast(t('add.noSelection'), true);
              return;
            }
            if (!confirm(t('lib.deleteConfirm', { n: selected.size, lib: name }))) return;
            try {
              const res = await api('POST', `/api/libraries/${encodeURIComponent(name)}/delete`, {
                items: [...selected.keys()].map((ref) => ({ ref })),
              });
              const ok = res.results.filter((r) => r.result === 'deleted').length;
              const errs = res.results.filter((r) => r.result === 'error');
              const lines = [t('lib.deleted', { n: ok })];
              for (const e of errs.slice(0, 5)) lines.push(`${e.path}: ${errorText(e.error, e.message)}`);
              toast(lines.join('\n'), errs.length > 0);
              selected.clear();
              loadAll();
            } catch (e) { showError(e); }
          },
        }, t('lib.delete'))),
      shown.length
        ? h('table', {},
          h('thead', {}, h('tr', {},
            h('th', { class: 'check' }, selectAllBox(() => boxes)),
            h('th', {}, t('search.path')), h('th', { class: 'num' }, t('browse.files')),
            h('th', { class: 'num' }, t('browse.size')), h('th', {}, t('browse.state')))),
          h('tbody', {}, shown.map((a) => {
            const box = h('input', {
              type: 'checkbox', checked: selected.has(a.ref), disabled: !!a.job,
              onchange: (e) => {
                if (e.target.checked) selected.set(a.ref, a.path); else selected.delete(a.ref);
                countEl.textContent = t('add.selected', { n: selected.size });
              },
            });
            boxes.push(box);
            const badges = [];
            if (a.orphan) badges.push(h('span', { class: 'badge warn' }, t('lib.orphan')));
            else badges.push(h('span', { class: 'badge muted' }, t('lib.sources', { sources: a.sources.join(', ') })));
            if (a.job) badges.push(h('span', { class: 'badge info' }, t(`jobs.status.${a.job.status}`)));
            return h('tr', {},
              h('td', { class: 'check' }, box),
              h('td', { class: 'path' }, a.sources.length
                ? h('a', { href: `#/browse${qs({ source: a.sources[0], ref: a.ref })}` }, a.path)
                : a.path),
              h('td', { class: 'num' }, `${a.stats.files} (${t('lib.tracks', { n: a.stats.tracks })})`),
              h('td', { class: 'num' }, fmtBytes(a.stats.bytes)),
              h('td', {}, h('div', { class: 'badges' }, badges)));
          })))
        : h('div', { class: 'muted' }, t('lib.noAlbums')),
    );
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
          ? h('table', {},
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
              h('td', {},
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
                }, t('trash.remove')))))))
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
    onchange: (e) => { location.hash = `#/jobs${qs({ status: e.target.value })}`; },
  }, statuses.map((s) => h('option', { value: s, selected: s === filter }, s ? t(`jobs.status.${s}`) : t('jobs.filter.all'))));
  const countsEl = h('span', { class: 'muted' });
  const table = h('div', { class: 'panel' });
  root.replaceChildren(h('div', { class: 'toolbar' }, select, countsEl), table);

  // CSP で style 属性を禁じているので、幅は CSSOM で設定する。
  const bar = (pct) => {
    const el = h('div');
    el.style.width = `${pct.toFixed(1)}%`;
    return el;
  };
  const badgeClass = { queued: 'info', running: 'warn', completed: 'ok', failed: 'bad', canceled: 'muted' };

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
    table.replaceChildren(h('table', {},
      h('thead', {}, h('tr', {},
        h('th', {}, t('jobs.created')), h('th', {}, t('jobs.source')), h('th', {}, t('jobs.path')),
        h('th', {}, t('jobs.library')), h('th', {}, t('jobs.status')), h('th', {}, t('jobs.progress')),
        h('th', {}, t('jobs.actions')))),
      h('tbody', {}, jobs.map((j) => {
        const pct = j.bytesTotal > 0 ? Math.min(100, (100 * j.bytesDone) / j.bytesTotal) : (j.status === 'completed' ? 100 : 0);
        const progress = (j.status === 'running' || j.bytesTotal > 0)
          ? h('div', {},
            h('div', { class: 'progress' }, bar(pct)),
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
        return h('tr', {},
          h('td', { class: 'small' }, fmtTime(j.createdAt)),
          h('td', {}, j.source),
          h('td', { class: 'path' }, h('a', { href: `#/browse${qs({ source: j.source, ref: j.ref })}` }, j.path)),
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
          h('td', {}, actions));
      }))));
  }
  load();
  const timer = setInterval(load, 2000);
  return () => clearInterval(timer);
}

// ---------- 一括追加 ----------

function viewBulk(root) {
  const area = h('textarea', { spellcheck: 'false', placeholder: 'music\tArtist/Album\nmusic\tArtist/Other Album\tshared,alice' });
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
  root.replaceChildren(h('div', { class: 'panel' },
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
      h('table', {},
        h('thead', {}, h('tr', {},
          h('th', { class: 'num' }, t('bulk.line')), h('th', {}, t('jobs.source')), h('th', {}, t('jobs.path')),
          h('th', {}, t('jobs.library')), h('th', {}, t('jobs.status')))),
        h('tbody', {}, res.rows.map((r) => h('tr', {},
          h('td', { class: 'num' }, r.line),
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
            r.error ? h('div', { class: 'small error-text' }, errorText(r.error, r.message), r.message ? ` (${r.message})` : '') : '')))))));
  }
  return () => {};
}

// ---------- 画面の切り替え ----------

const routes = {
  browse: { label: 'nav.browse', view: viewBrowse },
  search: { label: 'nav.search', view: viewSearch },
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
  nav.replaceChildren(...Object.entries(routes).map(([name, r]) => h('a', {
    href: `#/${name}`, class: name === active ? 'active' : '',
  }, t(r.label))));
}

function route() {
  cleanup();
  const { name, params } = parseHash();
  renderNav(name);
  const view = document.getElementById('view');
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
  setInterval(loadStatus, 3000);
}

main();
