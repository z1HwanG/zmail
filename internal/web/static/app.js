/* zMail 前端交互（原生 JS + fetch，无框架）
 * 依赖：window.__ZMAIL__ = {accountId, folderId, messageId, csrfToken, baseUrl}
 * 约定：DOM 结构与 docs/CONTRACT.md「前端 DOM 契约」一致。
 */
(function () {
  'use strict';

  var cfg = window.__ZMAIL__ || {};
  // baseUrl 是 PUBLIC_URL 的完整地址；前端只取其中的路径前缀，请求保持同源相对路径。
  var base = (function () {
    var raw = String(cfg.baseUrl || '');
    try {
      var p = new URL(raw).pathname.replace(/\/+$/, '');
      return p === '/' ? '' : p;
    } catch (e) {
      return raw.replace(/^https?:\/\/[^/]+/i, '').replace(/\/+$/, '');
    }
  })();
  var THEME_KEY = 'zmail-theme';
  var PAGE_TITLE = document.title || 'zMail';
  var API = '/api';

  var state = {
    accounts: [],
    accountById: {},
    folders: [],
    folderById: {},
    folderEls: {},
    badgeEls: {},
    allInboxBadge: null,
    nodes: {},
    messages: [],
    byId: {},
    cursor: null,
    hasMore: false,
    loading: false,
    scope: 'inbox',
    folderId: cfg.folderId || null,
    folderName: '全部收件箱',
    selectedId: null,
    filter: '',
    mode: 'list',
    searchQuery: '',
    unreadTotal: 0,
    detailCache: {},
    inflight: {},
    paletteItems: [],
    paletteIndex: 0,
    paletteSeq: 0,
    theme: 'auto',
    collapsedAccounts: null,
    backfilling: false,
    syncByAccount: {},
    lastLastGroup: '',
    booted: false
  };

  var el = {};
  var es = null;
  var sseDown = false;
  var toastTimer = 0;
  var searchTimer = 0;
  var prefetchTimer = 0;
  var live = null;

  /* ---------- 基础工具 ---------- */

  function $(id) { return document.getElementById(id); }

  function div(cls, text) {
    var n = document.createElement('div');
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }

  function span(cls, text) {
    var n = document.createElement('span');
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function pad(n) { return n < 10 ? '0' + n : String(n); }

  function parseDate(v) {
    if (!v) return null;
    var d = v instanceof Date ? v : new Date(v);
    return isNaN(d.getTime()) ? null : d;
  }

  function startOfDay(d) {
    var x = new Date(d.getTime());
    x.setHours(0, 0, 0, 0);
    return x;
  }

  function startOfWeek(d) {
    var x = startOfDay(d);
    var wd = (x.getDay() + 6) % 7; // 周一为 0
    x.setDate(x.getDate() - wd);
    return x;
  }

  var WEEK_CN = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];

  function groupOf(v) {
    var d = parseDate(v);
    if (!d) return '更早';
    var today = startOfDay(new Date());
    var day = startOfDay(d);
    if (day.getTime() === today.getTime()) return '今天';
    var yesterday = new Date(today.getTime() - 86400000);
    if (day.getTime() === yesterday.getTime()) return '昨天';
    if (d.getTime() >= startOfWeek(new Date()).getTime()) return '本周';
    return '更早';
  }

  function fmtTime(v) {
    var d = parseDate(v);
    if (!d) return '';
    var now = new Date();
    var today = startOfDay(now);
    var day = startOfDay(d);
    if (day.getTime() === today.getTime()) return pad(d.getHours()) + ':' + pad(d.getMinutes());
    var yesterday = new Date(today.getTime() - 86400000);
    if (day.getTime() === yesterday.getTime()) return '昨天';
    if (d.getTime() >= startOfWeek(now).getTime()) return WEEK_CN[d.getDay()];
    if (d.getFullYear() === now.getFullYear()) return (d.getMonth() + 1) + '月' + d.getDate() + '日';
    return d.getFullYear() + '/' + (d.getMonth() + 1) + '/' + d.getDate();
  }

  function fmtFullTime(v) {
    var d = parseDate(v);
    if (!d) return '';
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
      pad(d.getHours()) + ':' + pad(d.getMinutes());
  }

  function fmtSize(n) {
    n = Number(n) || 0;
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
    return (n / 1073741824).toFixed(1) + ' GB';
  }

  var COLORS = ['#1c4f8a', '#cf4436', '#3f7258', '#8a5a44', '#5d6f87', '#7a5c74', '#4e7a7c', '#9a6a22'];

  function accountColor(id) {
    var s = String(id || '');
    var h = 0;
    for (var i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
    return COLORS[h % COLORS.length];
  }

  function accountEmailOf(id) {
    var a = state.accountById[id];
    return (a && (a.email || a.displayName)) || id || '';
  }

  function folderById(id) { return state.folderById[id] || null; }

  function folderRole(folderId) {
    var f = folderById(folderId);
    return f ? (f.role || '') : '';
  }

  var ROLE_NAMES = { inbox: '收件箱', sent: '已发送', drafts: '草稿', trash: '已删除', spam: '垃圾邮件', archive: '归档' };

  function folderLabel(f) {
    if (!f) return '';
    var name = String(f.name || '').trim();
    var role = String(f.role || '');
    if (ROLE_NAMES[role]) return ROLE_NAMES[role] + (name && name !== ROLE_NAMES[role] && role === 'custom' ? ' · ' + name : '');
    return name || role || '文件夹';
  }

  var FOLDER_ICONS = {
    all: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16v13H4z"/><path d="M4 10h16"/><path d="M9 14h6"/></svg>',
    inbox: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 5h16v14H4z"/><path d="M4 13h4l2 2h4l2-2h4"/></svg>',
    sent: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m3 11 18-8-7 18-3-7-8-3Z"/><path d="M14 10 8 16"/></svg>',
    drafts: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 20h4L19 9l-4-4L4 16v4Z"/><path d="m13 7 4 4"/></svg>',
    trash: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16"/><path d="M9 7V4h6v3"/><path d="m6 7 1 13h10l1-13"/></svg>',
    spam: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="8"/><path d="m8 8 8 8"/></svg>',
    archive: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 8h16v12H4z"/><path d="M3 4h18v4H3z"/><path d="M9 12h6"/></svg>',
    custom: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6h7l2 2h9v11H3z"/></svg>'
  };

  function folderIcon(role) {
    return FOLDER_ICONS[role] || FOLDER_ICONS.custom;
  }

  /* ---------- 网络 ---------- */

  async function api(path, opts) {
    opts = opts || {};
    var headers = Object.assign({ Accept: 'application/json' }, opts.headers || {});
    var body = opts.body;
    var isFormData = typeof FormData !== 'undefined' && body instanceof FormData;
    if (body != null && typeof body !== 'string' && !isFormData) {
      headers['Content-Type'] = 'application/json';
      body = JSON.stringify(body);
    }
    if (opts.method && opts.method !== 'GET' && cfg.csrfToken) {
      headers['X-CSRF-Token'] = cfg.csrfToken;
    }
    var res;
    try {
      res = await fetch(base + path, {
        method: opts.method || 'GET',
        credentials: 'same-origin',
        headers: headers,
        body: body,
        signal: opts.signal
      });
    } catch (err) {
      throw new Error('网络连接失败，请检查服务是否在线');
    }
    if (res.status === 401) {
      location.href = base + '/login?next=' + encodeURIComponent(location.pathname + location.search);
      throw new Error('登录已过期，正在跳转登录页');
    }
    if (!res.ok) {
      var text = '';
      try { text = await res.text(); } catch (e) { /* ignore */ }
      var msg = '请求失败（HTTP ' + res.status + '）';
      if (text) {
        try {
          var j = JSON.parse(text);
          if (j && j.error && j.error.message) msg = j.error.message;
          else if (j && j.message) msg = j.message;
        } catch (e) {
          if (text.length < 200 && text.indexOf('<') < 0) msg = text;
        }
      }
      throw new Error(msg);
    }
    if (res.status === 204) return null;
    try { return await res.json(); } catch (e) { return null; }
  }

  function pickList(payload) {
    if (!payload) return [];
    if (Array.isArray(payload)) return payload;
    return payload.messages || payload.items || payload.data || payload.results ||
      (payload.message ? [payload.message] : []) || [];
  }

  function pickCursor(payload) {
    if (!payload || Array.isArray(payload)) return null;
    return payload.nextCursor || payload.next_cursor || payload.cursor || null;
  }

  function pickMessage(payload) {
    if (!payload) return {};
    if (payload.message && typeof payload.message === 'object') return payload.message;
    if (payload.data && typeof payload.data === 'object') return payload.data;
    return payload;
  }

  function normalizeSummary(raw) {
    raw = raw || {};
    var fromEmail = raw.fromEmail || raw.from_email || raw.email || '';
    var fromName = raw.fromName || raw.from_name || raw.from || fromEmail;
    return {
      id: String(raw.id || raw.ID || ''),
      accountId: raw.accountId || raw.account_id || '',
      accountEmail: raw.accountEmail || raw.account_email || '',
      accountName: raw.accountName || raw.account_name || '',
      folderId: raw.folderId || raw.folder_id || '',
      folderName: raw.folderName || raw.folder_name || '',
      subject: raw.subject == null ? '(无主题)' : String(raw.subject),
      fromName: fromName || '(未知发件人)',
      fromEmail: fromEmail,
      snippet: raw.snippet || raw.preview || '',
      receivedAt: raw.receivedAt || raw.received_at || raw.date || raw.sentAt || raw.sent_at || '',
      isRead: !!(raw.isRead || raw.is_read || raw.read),
      isStarred: !!(raw.isStarred || raw.is_starred || raw.starred),
      hasAttachments: !!(raw.hasAttachments || raw.has_attachments)
    };
  }

  /* ---------- Toast / 无障碍播报 ---------- */

  function ensureLive() {
    if (live) return;
    live = $('srLive');
    if (!live) {
      live = div('sr-only');
      live.id = 'srLive';
      live.setAttribute('aria-live', 'polite');
      live.setAttribute('aria-atomic', 'true');
      document.body.appendChild(live);
    }
  }

  function announce(text) {
    ensureLive();
    live.textContent = '';
    setTimeout(function () { live.textContent = text; }, 40);
  }

  function hideToast() {
    clearTimeout(toastTimer);
    if (el.toast) {
      el.toast.classList.add('hidden');
      el.toast.onclick = null;
    }
  }

  function showToast(opts) {
    if (!el.toast) return;
    el.toast.className = 'toast' + (opts.kind ? ' toast-' + opts.kind : '');
    el.toast.innerHTML = '';
    if (opts.title) el.toast.appendChild(div('toast-title', opts.title));
    if (opts.code) el.toast.appendChild(div('toast-code', opts.code));
    if (opts.text) {
      var t = div('toast-text', opts.text);
      el.toast.appendChild(t);
    }
    if (opts.hint) el.toast.appendChild(div('toast-hint', opts.hint));
    el.toast.classList.remove('hidden');
    el.toast.onclick = function () {
      if (opts.code) {
        copyText(opts.code).then(function () {
          showToast({ title: opts.title, code: opts.code, hint: '已复制到剪贴板', kind: 'copied' });
        }).catch(function () {
          showToast({ title: opts.title, code: opts.code, hint: '复制失败，请手动选择', kind: 'error' });
        });
        if (opts.onClick) opts.onClick();
      } else {
        if (opts.onClick) opts.onClick();
        hideToast();
      }
    };
    clearTimeout(toastTimer);
    var ms = opts.duration || (opts.code ? 20000 : 4000);
    toastTimer = setTimeout(hideToast, ms);
  }

  function toastText(text, kind) {
    showToast({ text: text, kind: kind, duration: kind === 'error' ? 6000 : 3500 });
  }

  function toastError(err) {
    var msg = err && err.message ? err.message : String(err);
    showToast({ title: '操作失败', text: msg, kind: 'error', duration: 6000 });
  }

  function copyText(text) {
    function fallback() {
      try {
        var ta = document.createElement('textarea');
        ta.value = text;
        ta.setAttribute('readonly', '');
        ta.style.position = 'fixed';
        ta.style.top = '-1000px';
        ta.style.opacity = '0';
        document.body.appendChild(ta);
        ta.select();
        ta.setSelectionRange(0, ta.value.length);
        var ok = document.execCommand('copy');
        document.body.removeChild(ta);
        return ok ? Promise.resolve() : Promise.reject(new Error('复制失败'));
      } catch (err) {
        return Promise.reject(err);
      }
    }
    if (navigator.clipboard && navigator.clipboard.writeText && window.isSecureContext) {
      return navigator.clipboard.writeText(text).catch(fallback);
    }
    return fallback();
  }

  /* ---------- 主题 ---------- */

  function mediaDark() {
    return !!(window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
  }

  function resolveTheme(pref) {
    if (pref === 'dark' || pref === 'light') return pref;
    return mediaDark() ? 'dark' : 'light';
  }

  function applyTheme(pref, persist) {
    state.theme = pref || 'auto';
    var resolved = resolveTheme(state.theme);
    document.documentElement.setAttribute('data-theme', resolved);
    if (persist !== false) {
      try { localStorage.setItem(THEME_KEY, state.theme); } catch (e) { /* 隐私模式忽略 */ }
    }
    syncThemeButton(resolved);
    window.dispatchEvent(new CustomEvent('zmail:theme', { detail: { pref: state.theme, resolved: resolved } }));
  }

  function syncThemeButton(resolved) {
    if (!el.themeBtn) return;
    el.themeBtn.setAttribute('aria-pressed', resolved === 'dark' ? 'true' : 'false');
    el.themeBtn.title = resolved === 'dark' ? '切换到浅色主题' : '切换到深色主题';
    el.themeBtn.setAttribute('aria-label', el.themeBtn.title);
  }

  function initTheme() {
    var pref = 'auto';
    try { pref = localStorage.getItem(THEME_KEY) || 'auto'; } catch (e) { /* ignore */ }
    if (pref !== 'auto' && pref !== 'light' && pref !== 'dark') pref = 'auto';
    applyTheme(pref, false);
    if (window.matchMedia) {
      var mq = window.matchMedia('(prefers-color-scheme: dark)');
      var onChange = function () { if (state.theme === 'auto') applyTheme('auto', false); };
      if (mq.addEventListener) mq.addEventListener('change', onChange);
      else if (mq.addListener) mq.addListener(onChange);
    }
  }

  function toggleTheme() {
    var resolved = resolveTheme(state.theme);
    applyTheme(resolved === 'dark' ? 'light' : 'dark');
  }

  /* ---------- 侧栏 ---------- */

  function normalizeFolder(raw) {
    return {
      id: String(raw.id || raw.ID || ''),
      accountId: raw.accountId || raw.account_id || '',
      name: raw.name || raw.remotePath || '',
      remotePath: raw.remotePath || raw.remote_path || '',
      role: raw.role || 'custom',
      unreadCount: Number(raw.unreadCount || raw.unread_count || 0) || 0,
      totalCount: Number(raw.totalCount || raw.total_count || 0) || 0,
      enabled: raw.enabled !== false
    };
  }

  async function loadSidebar() {
    var results = await Promise.all([
      api(API + '/accounts').catch(function () { return null; }),
      api(API + '/folders').catch(function () { return null; })
    ]);
    var accPayload = results[0];
    var folderPayload = results[1];

    var accounts = accPayload ? (accPayload.accounts || (Array.isArray(accPayload) ? accPayload : [])) : [];
    var folders = folderPayload ? (folderPayload.folders || (Array.isArray(folderPayload) ? folderPayload : [])) : [];

    state.accounts = accounts.map(function (a) {
      return {
        id: String(a.id || a.ID || ''),
        email: a.email || '',
        displayName: a.displayName || a.display_name || '',
        status: a.status || '',
        lastError: a.lastError || a.last_error || ''
      };
    });
    state.accountById = {};
    state.accounts.forEach(function (a) { state.accountById[a.id] = a; });

    state.folders = folders.map(normalizeFolder).filter(function (f) { return f.id; });
    state.folderById = {};
    state.folders.forEach(function (f) { state.folderById[f.id] = f; });

    renderSidebar();
    refreshUnreadTotal();

    if (state.folderId) {
      var cur = folderById(state.folderId);
      state.folderName = cur ? folderLabel(cur) : '邮件';
    }
  }

  function folderButton(f) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'folder-item';
    if (f.id) b.setAttribute('data-folder-id', f.id);
    if (f.accountId) b.setAttribute('data-account-id', f.accountId);
    b.setAttribute('data-folder-name', f.name || '');
    b.setAttribute('title', f.name || '');
    var iconEl = span('folder-icon');
    iconEl.innerHTML = f.icon || folderIcon('');
    b.appendChild(iconEl);
    b.appendChild(span('folder-name', f.label || f.name || ''));
    var badge = span('badge' + (Number(f.unread) > 0 ? '' : ' zero'), String(Number(f.unread) || 0));
    b.appendChild(badge);
    if (f.key) state.badgeEls[f.key] = badge;
    return b;
  }

  var COLLAPSE_KEY = 'zmail-collapsed-accounts';

  function loadCollapsedAccounts() {
    if (state.collapsedAccounts) return state.collapsedAccounts;
    var set = {};
    try {
      var raw = window.localStorage.getItem(COLLAPSE_KEY);
      var arr = raw ? JSON.parse(raw) : [];
      if (Array.isArray(arr)) arr.forEach(function (id) { if (id) set[String(id)] = true; });
    } catch (e) { /* localStorage 不可用时忽略 */ }
    state.collapsedAccounts = set;
    return set;
  }

  function saveCollapsedAccounts() {
    try {
      var ids = Object.keys(state.collapsedAccounts || {});
      window.localStorage.setItem(COLLAPSE_KEY, JSON.stringify(ids));
    } catch (e) { /* ignore */ }
  }

  function applyCollapsedState(group, accountId) {
    var set = loadCollapsedAccounts();
    var collapsed = !!set[String(accountId)];
    group.classList.toggle('collapsed', collapsed);
    var head = group.querySelector('.account-head');
    if (head) head.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
  }

  function toggleAccountGroup(head) {
    var group = head.closest('.account-group');
    if (!group) return;
    var id = head.getAttribute('data-toggle-account') || group.getAttribute('data-account-id') || '';
    if (!id) return;
    var set = loadCollapsedAccounts();
    var collapsed = !group.classList.contains('collapsed');
    group.classList.toggle('collapsed', collapsed);
    head.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    if (collapsed) set[String(id)] = true;
    else delete set[String(id)];
    saveCollapsedAccounts();
    announce((collapsed ? '已折叠 ' : '已展开 ') + (head.querySelector('.account-email') || {}).textContent + ' 的文件夹');
  }

  function renderSidebar() {
    if (!el.sidebarContent) return;
    state.folderEls = {};
    state.badgeEls = {};
    el.sidebarContent.innerHTML = '';

    var all = folderButton({
      key: '__all__',
      name: '全部收件箱',
      label: '全部收件箱',
      icon: folderIcon('all'),
      unread: 0
    });
    all.setAttribute('data-all', '1');
    if (!state.folderId) all.classList.add('active');
    s(state.badgeEls, '__all__', all.querySelector('.badge'));
    el.sidebarContent.appendChild(all);
    state.allInboxBadge = all.querySelector('.badge');

    state.accounts.forEach(function (acc, idx) {
      var group = div('account-group');
      group.setAttribute('data-account-id', acc.id);

      var accFolders = state.folders.filter(function (f) { return f.accountId === acc.id; });
      var accUnread = 0;
      accFolders.forEach(function (f) { accUnread += Number(f.unreadCount) || 0; });

      var head = document.createElement('button');
      head.type = 'button';
      head.className = 'account-head account-title';
      head.setAttribute('data-toggle-account', acc.id);
      head.setAttribute('aria-expanded', 'true');
      head.title = '折叠/展开 ' + (acc.email || acc.id);
      var dot = span('dot');
      dot.style.background = accountColor(acc.id);
      head.appendChild(dot);
      head.appendChild(span('account-email', acc.email || acc.displayName || acc.id));
      if (accUnread > 0) {
        var badge = span('badge account-badge', String(accUnread));
        head.appendChild(badge);
      }
      var st = state.syncByAccount[acc.id] || acc.status || '';
      var stEl = span('account-state', acc.status === 'auth_failed' ? '认证失败' : syncLabel(st));
      stEl.setAttribute('data-state', st === 'error' || acc.status === 'auth_failed' ? 'error' : st);
      if (acc.lastError) stEl.title = acc.lastError;
      head.appendChild(stEl);
      var caret = span('account-caret');
      caret.setAttribute('aria-hidden', 'true');
      caret.textContent = '▾';
      head.appendChild(caret);
      group.appendChild(head);

      var list = div('account-folders');
      accFolders.forEach(function (f) {
        var btn = folderButton({
          key: f.id,
          name: f.name,
          label: folderLabel(f),
          icon: folderIcon(f.role),
          unread: f.unreadCount
        });
        btn.setAttribute('data-folder-id', f.id);
        btn.setAttribute('data-account-id', acc.id);
        if (state.folderId === f.id) btn.classList.add('active');
        list.appendChild(btn);
        state.folderEls[f.id] = btn;
      });
      group.appendChild(list);
      applyCollapsedState(group, acc.id);
      el.sidebarContent.appendChild(group);
    });

    var orphan = state.folders.filter(function (f) { return f.accountId && !state.accountById[f.accountId]; });
    if (orphan.length) {
      var g = div('account-group');
      orphan.forEach(function (f) {
        var btn = folderButton({ key: f.id, name: f.name, label: folderLabel(f), icon: folderIcon(f.role), unread: f.unreadCount });
        btn.setAttribute('data-account-id', f.accountId);
        if (state.folderId === f.id) btn.classList.add('active');
        g.appendChild(btn);
        state.folderEls[f.id] = btn;
      });
      el.sidebarContent.appendChild(g);
    }
  }

  function s(obj, key, val) { if (val) obj[key] = val; }

  function syncLabel(status) {
    if (status === 'syncing') return '同步中';
    if (status === 'error') return '异常';
    if (status === 'offline') return '离线';
    return '';
  }

  function refreshUnreadTotal() {
    var total = 0;
    state.folders.forEach(function (f) { if (f.role === 'inbox') total += f.unreadCount || 0; });
    state.unreadTotal = total;
    updateUnreadUI();
  }

  function updateUnreadUI() {
    var n = Math.max(0, Number(state.unreadTotal) || 0);
    if (state.allInboxBadge) {
      state.allInboxBadge.textContent = String(n);
      state.allInboxBadge.classList.toggle('zero', n <= 0);
    }
    document.title = (n > 0 ? '(' + n + ') ' : '') + PAGE_TITLE.replace(/^\(\d+\)\s*/, '');
  }

  function adjustUnread(folderId, delta) {
    if (!delta) return;
    var f = folderById(folderId);
    if (f) {
      f.unreadCount = Math.max(0, (f.unreadCount || 0) + delta);
      var badge = state.badgeEls[folderId];
      if (badge) {
        badge.textContent = String(f.unreadCount);
        badge.classList.toggle('zero', f.unreadCount <= 0);
      }
    }
    if (!f) state.unreadTotal = Math.max(0, state.unreadTotal + delta);
    else refreshUnreadTotal();
    if (f) updateUnreadUI();
  }

  async function selectFolder(btn) {
    var fid = btn.getAttribute('data-folder-id') || '';
    var all = btn.getAttribute('data-all') === '1';
    if (!all && !fid) return;
    state.folderId = all ? null : fid;
    var f = folderById(fid);
    state.folderName = all ? '全部收件箱' : (f ? folderLabel(f) : '邮件');
    state.scope = state.folderId ? '' : 'inbox';
    state.mode = 'list';
    state.searchQuery = '';
    if (el.search) el.search.value = '';
    document.body.classList.remove('sidebar-open');
    document.body.classList.remove('reading');
    clearReader();
    await resetAndLoad();
    refreshSidebarActive();
  }

  function refreshSidebarActive() {
    if (!el.sidebarContent) return;
    var buttons = el.sidebarContent.querySelectorAll('.folder-item');
    Array.prototype.forEach.call(buttons, function (b) {
      var isAll = b.getAttribute('data-all') === '1';
      var active = isAll ? !state.folderId : b.getAttribute('data-folder-id') === state.folderId;
      b.classList.toggle('active', !!active);
    });
  }

  /* ---------- 邮件列表 ---------- */

  function messageQuery(first) {
    var q = new URLSearchParams();
    if (state.folderId) q.set('folderId', state.folderId);
    else q.set('scope', 'inbox');
    q.set('limit', '30');
    if (!first && state.cursor) q.set('before', state.cursor);
    return API + '/messages?' + q.toString();
  }

  function updateListHeader() {
    if (el.listTitle) {
      el.listTitle.textContent = state.mode === 'search'
        ? '搜索结果'
        : (state.folderName || '全部收件箱');
    }
    var n = state.messages.length;
    if (el.listCount) {
      el.listCount.textContent = state.mode === 'search'
        ? (n ? n + ' 封结果' : '无结果')
        : (n ? n + ' 封' : '');
    }
    if (el.listMeta) {
      if (state.mode === 'search') {
        el.listMeta.textContent = '搜索「' + state.searchQuery + '」';
      } else if (state.folderId) {
        el.listMeta.textContent = '当前文件夹 · 新邮件会自动出现在这里';
      } else {
        el.listMeta.textContent = '所有账号的收件箱 · 新邮件会自动出现在这里';
      }
    }
  }

  function resetList() {
    state.messages = [];
    state.byId = {};
    state.nodes = {};
    state.cursor = null;
    state.hasMore = false;
    state.selectedId = null;
    if (el.messageList) el.messageList.innerHTML = '';
    updateListHeader();
  }

  function showSkeleton() {
    if (!el.messageList) return;
    var frag = document.createDocumentFragment();
    for (var i = 0; i < 8; i++) {
      var sk = div('skeleton');
      var l1 = div('skeleton-line'); l1.style.width = (36 + (i % 3) * 8) + '%';
      var l2 = div('skeleton-line'); l2.style.width = '78%';
      var l3 = div('skeleton-line'); l3.style.width = '58%';
      sk.appendChild(l1); sk.appendChild(l2); sk.appendChild(l3);
      frag.appendChild(sk);
    }
    el.messageList.innerHTML = '';
    el.messageList.appendChild(frag);
  }

  function showListEmpty(text) {
    if (!el.messageList) return;
    el.messageList.innerHTML = '';
    var e = div('empty', text || '没有邮件');
    el.messageList.appendChild(e);
    updateListHeader();
  }

  function groupLabel(text) {
    var n = div('group-label', text);
    n.setAttribute('data-group', text);
    return n;
  }

  function messageNode(m) {
    var node = document.createElement('div');
    node.className = 'message-item' + (m.isRead ? '' : ' unread') + (m.isStarred ? ' starred' : '');
    node.setAttribute('data-id', m.id);
    node.setAttribute('data-account-id', m.accountId || '');
    node.setAttribute('role', 'button');
    node.setAttribute('tabindex', '0');
    node.setAttribute('aria-label', (m.isRead ? '' : '未读，') + (m.fromName || '') + '，' + m.subject);

    var acc = div('message-account');
    var dot = span('dot');
    dot.style.background = accountColor(m.accountId);
    acc.appendChild(dot);
    acc.appendChild(span('account-email', m.accountEmail || accountEmailOf(m.accountId)));
    node.appendChild(acc);

    var row = div('message-row');
    row.appendChild(span('from', m.fromName || m.fromEmail || '(未知发件人)'));
    row.appendChild(span('time', fmtTime(m.receivedAt)));
    node.appendChild(row);
    node.appendChild(div('subject', m.subject || '(无主题)'));
    if (m.snippet) node.appendChild(div('snippet', m.snippet));

    var flags = div('item-flags');
    var star = document.createElement('button');
    star.type = 'button';
    star.className = 'star-btn';
    star.setAttribute('data-star', m.id);
    star.setAttribute('aria-pressed', m.isStarred ? 'true' : 'false');
    star.setAttribute('aria-label', m.isStarred ? '取消星标' : '添加星标');
    star.textContent = m.isStarred ? '★' : '☆';
    flags.appendChild(star);
    if (m.hasAttachments) {
      var att = span('attach-flag', '附');
      att.title = '含附件';
      flags.appendChild(att);
    }
    node.appendChild(flags);
    return node;
  }

  function appendMessages(list) {
    if (!el.messageList || !list.length) return;
    var empty = el.messageList.querySelector('.empty');
    if (empty) empty.remove();
    var loadMore = el.messageList.querySelector('.load-more');
    if (loadMore) loadMore.remove();

    var frag = document.createDocumentFragment();
    var lastGroup = state.lastLastGroup || '';
    list.forEach(function (raw) {
      var m = normalizeSummary(raw);
      if (!m.id || state.byId[m.id]) return;
      state.messages.push(m);
      state.byId[m.id] = m;
      var g = groupOf(m.receivedAt);
      if (g !== lastGroup) {
        frag.appendChild(groupLabel(g));
        lastGroup = g;
      }
      var node = messageNode(m);
      state.nodes[m.id] = node;
      frag.appendChild(node);
    });
    state.lastLastGroup = lastGroup;
    el.messageList.appendChild(frag);
    updateLoadMore();
    updateListHeader();
  }

  function updateLoadMore() {
    if (!el.messageList) return;
    var old = el.messageList.querySelector('.load-more');
    if (old) old.remove();
    var older = el.messageList.querySelector('.load-older');
    if (older) older.remove();
    if (state.hasMore) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'load-more';
      b.textContent = state.loading ? '加载中…' : '加载更多';
      b.disabled = !!state.loading;
      b.addEventListener('click', function () { loadMessages(false); });
      el.messageList.appendChild(b);
      return;
    }
    // 本地已到底：提供“从服务器拉取更早邮件”的入口。
    if (!state.messages.length) return;
    var old2 = document.createElement('button');
    old2.type = 'button';
    old2.className = 'load-older';
    old2.textContent = state.backfilling ? '正在从服务器拉取更早邮件…' : '加载更早邮件（从服务器拉取）';
    old2.disabled = !!(state.loading || state.backfilling);
    old2.title = '从 IMAP 服务器再拉取一批更早的邮件';
    old2.addEventListener('click', backfillOlder);
    el.messageList.appendChild(old2);
  }

  // cursorOf 用最后一条邮件构造与服务端一致的游标。
  function cursorOf(m) {
    if (!m || !m.receivedAt || !m.id) return null;
    var d = new Date(m.receivedAt);
    if (isNaN(d.getTime())) return null;
    return d.toISOString() + '|' + m.id;
  }

  // backfillOlder 请求服务端回填一批更早的邮件，然后继续按游标追加。
  async function backfillOlder() {
    if (state.loading || state.backfilling) return;
    state.backfilling = true;
    updateLoadMore();
    try {
      var body = { limit: 50 };
      if (state.folderId) body.folderId = state.folderId;
      else body.scope = 'inbox';
      var data = await api(API + '/messages/backfill', { method: 'POST', body: body });
      var inserted = (data && data.inserted) || 0;
      if (!inserted) {
        toastText((data && data.done) ? '服务器上没有更早的邮件了' : '暂时没有拉到更早的邮件，可稍后重试');
        return;
      }
      var last = state.messages[state.messages.length - 1];
      var cur = cursorOf(last);
      if (cur) state.cursor = cur;
      state.hasMore = true;
      await loadMessages(false);
      toastText('已拉取 ' + inserted + ' 封更早邮件');
    } catch (err) {
      toastError(err);
    } finally {
      state.backfilling = false;
      updateLoadMore();
    }
  }

  async function loadMessages(first) {
    if (!el.messageList || state.loading) return;
    state.loading = true;
    // 服务端已渲染列表时不再闪骨架屏，等新数据回来再整体替换。
    var hadItems = !!(first && el.messageList.querySelector('.message-item'));
    if (first && !hadItems) {
      resetList();
      state.lastLastGroup = '';
      showSkeleton();
    } else if (!first) {
      updateLoadMore();
    }
    try {
      var data = await api(messageQuery(first));
      var list = pickList(data);
      var cursor = pickCursor(data);
      if (first) {
        // 注意：resetList 会清空 cursor/hasMore，必须在读取响应之后再重置，
        // 否则首屏永远拿不到“加载更多”的游标（历史 bug：只能看到最近 30 封）。
        resetList();
        state.lastLastGroup = '';
      }
      state.cursor = cursor || null;
      state.hasMore = !!cursor && list.length > 0;
      if (!list.length) {
        if (first) showListEmpty('收件箱是空的');
      } else {
        appendMessages(list);
      }
      if (!state.hasMore) updateLoadMore();
    } catch (err) {
      if (first) showListEmpty('加载失败：' + (err && err.message ? err.message : '未知错误'));
      toastError(err);
    } finally {
      state.loading = false;
      updateLoadMore();
    }
  }

  function resetAndLoad() {
    return loadMessages(true);
  }

  function rowInView() {
    if (!state.selectedId) return null;
    return state.nodes[state.selectedId] || null;
  }

  function moveSelection(delta) {
    if (!state.messages.length) return;
    var idx = -1;
    for (var i = 0; i < state.messages.length; i++) {
      if (state.messages[i].id === state.selectedId) { idx = i; break; }
    }
    if (idx < 0) idx = delta > 0 ? -1 : 0;
    var next = Math.min(state.messages.length - 1, Math.max(0, idx + delta));
    setSelection(state.messages[next].id, true);
  }

  function setSelection(id, scroll) {
    var prev = rowInView();
    if (prev) prev.classList.remove('active');
    state.selectedId = id;
    var node = state.nodes[id];
    if (node) {
      node.classList.add('active');
      if (scroll) node.scrollIntoView({ block: 'nearest' });
    }
    if (window.matchMedia && window.matchMedia('(pointer: coarse)').matches) return;
  }

  /* ---------- 邮件详情 ---------- */

  function updateItemFlags(id) {
    var m = state.byId[id];
    var node = state.nodes[id];
    if (!m || !node) return;
    node.classList.toggle('unread', !m.isRead);
    node.classList.toggle('starred', !!m.isStarred);
    node.setAttribute('aria-label', (m.isRead ? '' : '未读，') + (m.fromName || '') + '，' + m.subject);
    var star = node.querySelector('.star-btn');
    if (star) {
      star.textContent = m.isStarred ? '★' : '☆';
      star.setAttribute('aria-pressed', m.isStarred ? 'true' : 'false');
      star.setAttribute('aria-label', m.isStarred ? '取消星标' : '添加星标');
    }
  }

  function removeFromList(id) {
    var node = state.nodes[id];
    if (node) node.remove();
    delete state.nodes[id];
    delete state.byId[id];
    state.messages = state.messages.filter(function (m) { return m.id !== id; });
    if (el.messageList) {
      var labels = el.messageList.querySelectorAll('.group-label');
      Array.prototype.forEach.call(labels, function (l) {
        var nx = l.nextElementSibling;
        if (!nx || nx.classList.contains('group-label') || nx.classList.contains('load-more')) l.remove();
      });
      if (!el.messageList.querySelector('.message-item') && !state.loading) showListEmpty('收件箱是空的');
    }
  }

  async function ensureCached(id) {
    if (!id || state.byId[id] || state.detailCache[id]) return;
    try {
      state.detailCache[id] = await api(API + '/messages/' + encodeURIComponent(id));
    } catch (e) {
      /* 详情拉取失败时由调用方提示 */
    }
  }

  async function ensureDetail(id) {
    if (!id) return null;
    if (state.detailCache[id]) return state.detailCache[id];
    try {
      state.detailCache[id] = await api(API + '/messages/' + encodeURIComponent(id));
    } catch (e) {
      return null;
    }
    return state.detailCache[id];
  }

  function prefetch(id) {
    if (!id || state.detailCache[id] || state.inflight[id]) return;
    state.inflight[id] = true;
    api(API + '/messages/' + encodeURIComponent(id))
      .then(function (data) { state.detailCache[id] = data; })
      .catch(function () { /* 预取失败静默 */ })
      .then(function () { delete state.inflight[id]; });
  }

  function readerLoading() {
    if (!el.reader) return;
    el.reader.innerHTML = '';
    el.reader.appendChild(div('empty', '加载中…'));
  }

  function readerEmpty(text) {
    if (!el.reader) return;
    el.reader.innerHTML = '';
    el.reader.appendChild(div('empty', text || '选择一封邮件'));
  }

  async function openMessage(id) {
    if (!id) return;
    setSelection(id, true);
    document.body.classList.add('reading');
    readerLoading();
    var summary = state.byId[id];
    if (summary && !summary.isRead) patchFlags(id, { isRead: true }, true);
    var data = state.detailCache[id];
    if (!data) {
      try {
        data = await api(API + '/messages/' + encodeURIComponent(id));
        state.detailCache[id] = data;
      } catch (err) {
        readerEmpty('加载失败：' + (err && err.message ? err.message : '未知错误'));
        toastError(err);
        return;
      }
    }
    if (state.selectedId !== id) return;
    var detail = pickMessage(data);
    renderReader(detail, data);
    syncReaderFlags(id);
    if (detail.isRead === false) patchFlags(id, { isRead: true }, true);
  }

  function actionButton(label, act, opts) {
    opts = opts || {};
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn' + (opts.cls ? ' ' + opts.cls : '');
    b.setAttribute('data-act', act);
    b.textContent = label;
    if (opts.pressed !== undefined) b.setAttribute('aria-pressed', opts.pressed ? 'true' : 'false');
    if (opts.title) b.title = opts.title;
    return b;
  }

  // readerPosition 返回当前邮件在列表中的序号（0 基）与总数。
  function readerPosition(id) {
    var idx = -1;
    for (var i = 0; i < state.messages.length; i++) {
      if (state.messages[i].id === id) { idx = i; break; }
    }
    return { index: idx < 0 ? 0 : idx, total: state.messages.length };
  }

  // openRelative 打开列表中的上一封/下一封，用于连续浏览。
  async function openRelative(delta) {
    if (!state.messages.length) return;
    var pos = readerPosition(state.selectedId);
    var idx = pos.index + delta;
    if (idx < 0) {
      toastText('已经是第一封了');
      return;
    }
    if (idx >= state.messages.length) {
      if (state.hasMore) {
        await loadMessages(false);
        if (idx >= state.messages.length) {
          toastText('已经是最后一封了');
          return;
        }
      } else {
        toastText('已经是最后一封了');
        return;
      }
    }
    var target = state.messages[idx];
    if (!target) return;
    setSelection(target.id, true);
    await openMessage(target.id);
  }

  function renderReader(m, payload) {
    if (!el.reader) return;
    el.reader.innerHTML = '';
    var head = div('reader-head');

    var title = document.createElement('h2');
    title.className = 'reader-title';
    title.textContent = m.subject || '(无主题)';
    head.appendChild(title);

    var meta = div('reader-meta');
    var dot = span('dot');
    dot.style.background = accountColor(m.accountId);
    meta.appendChild(dot);
    var acc = document.createElement('span');
    acc.className = 'meta-strong';
    acc.textContent = m.accountEmail || accountEmailOf(m.accountId);
    meta.appendChild(acc);
    if (m.folderName) meta.appendChild(span('', m.folderName));
    var from = document.createElement('span');
    from.textContent = (m.fromName || '') + (m.fromEmail ? ' <' + m.fromEmail + '>' : '');
    meta.appendChild(from);
    if (m.receivedAt) meta.appendChild(span('', fmtFullTime(m.receivedAt)));
    head.appendChild(meta);

    var actions = div('reader-actions');
    // 上一封 / 下一封：不必返回列表就能连续浏览其他邮件（移动端尤其需要）。
    var pos = readerPosition(m.id);
    var prevBtn = actionButton('上一封', 'prev');
    if (pos.index <= 0) prevBtn.disabled = true;
    var nextBtn = actionButton('下一封', 'next');
    if (pos.index >= pos.total - 1 && !state.hasMore) nextBtn.disabled = true;
    actions.appendChild(prevBtn);
    actions.appendChild(nextBtn);
    if (pos.total > 0) {
      var posEl = span('reader-position', (pos.index + 1) + ' / ' + pos.total);
      actions.appendChild(posEl);
    }
    actions.appendChild(actionButton('回复', 'reply', { cls: 'btn-primary' }));
    actions.appendChild(actionButton('转发', 'forward'));
    actions.appendChild(actionButton(m.isStarred ? '★ 已星标' : '☆ 星标', 'star', { pressed: !!m.isStarred }));
    actions.appendChild(actionButton(m.isRead === false ? '标为已读' : '标为未读', 'read', { pressed: !!(m.isRead) }));

    var bodyHTML = m.bodyHTML || m.bodyHtml || m.htmlBody || m.html || '';
    var hasQuote = /<blockquote|<div[^>]+class="[^"]*gmail_quote/i.test(bodyHTML);
    if (hasQuote) actions.appendChild(actionButton('显示引用', 'quote', { cls: 'quote-btn' }));
    actions.appendChild(actionButton('删除', 'delete', { cls: 'btn-danger' }));
    actions.appendChild(actionButton('返回列表', 'back', { cls: 'back-btn' }));
    head.appendChild(actions);
    el.reader.appendChild(head);

    var body = div('reader-body');
    if (hasQuote && state.quotesCollapsed !== false) body.classList.add('quotes-collapsed');

    var code = m.code || m.verificationCode || '';
    if (code) {
      var box = div('');
      box.id = 'codeBox';
      var wrap = div('');
      wrap.appendChild(div('code-label', '验证码'));
      wrap.appendChild(div('code-value', String(code)));
      box.appendChild(wrap);
      var copyBtn = actionButton('复制', 'copy-code');
      box.appendChild(copyBtn);
      body.appendChild(box);
    }

    if (bodyHTML) {
      body.innerHTML = bodyHTML; // 服务端已消毒，前端直接渲染
    } else if (m.bodyText) {
      var pre = div('plain-text');
      pre.textContent = m.bodyText;
      body.appendChild(pre);
    } else {
      body.appendChild(div('muted', '(无正文)'));
    }
    body.querySelectorAll('a[href]').forEach(function (a) {
      a.setAttribute('target', '_blank');
      a.setAttribute('rel', 'noopener noreferrer');
    });
    body.querySelectorAll('img').forEach(function (img) {
      img.setAttribute('loading', 'lazy');
      img.setAttribute('referrerpolicy', 'no-referrer');
    });
    el.reader.appendChild(body);

    var attachments = (payload && payload.attachments) || m.attachments || [];
    if (attachments && attachments.length) {
      var sec = div('attachments');
      var h3 = document.createElement('h3');
      h3.textContent = '附件（' + attachments.length + '）';
      sec.appendChild(h3);
      var list = div('attachment-list');
      attachments.forEach(function (att) {
        var a = document.createElement('a');
        a.className = 'attachment-item';
        a.href = att.url ? base + att.url : base + API + '/attachments/' + encodeURIComponent(att.id || '');
        a.setAttribute('download', att.filename || '');
        var nm = span('attachment-name', att.filename || '附件');
        nm.title = att.filename || '';
        a.appendChild(nm);
        if (att.size) a.appendChild(span('attachment-size', fmtSize(att.size)));
        list.appendChild(a);
      });
      sec.appendChild(list);
      el.reader.appendChild(sec);
    }

    if (el.reader) el.reader.scrollTop = 0;
    var bodyEl = body;
    var quoteBtn = actions.querySelector('[data-act="quote"]');
    if (quoteBtn) {
      quoteBtn.textContent = bodyEl.classList.contains('quotes-collapsed') ? '显示引用' : '隐藏引用';
    }
  }

  function syncSSRReader(id) {
    if (!el.reader || el.reader.querySelector('[data-act]')) return;
    var m = state.byId[id] || (state.detailCache[id] ? pickMessage(state.detailCache[id]) : null);
    if (!m) return;
    var star = el.reader.querySelector('[data-action="toggle-star"]');
    if (star) {
      star.classList.toggle('on', !!m.isStarred);
      star.setAttribute('data-starred', String(!!m.isStarred));
    }
    var read = el.reader.querySelector('[data-action="toggle-read"]');
    if (read) read.setAttribute('data-read', String(!!m.isRead));
  }

  function clearReader() {
    state.selectedId = null;
    if (!el.reader) return;
    readerEmpty('选择一封邮件');
    clearSelection();
  }

  function clearSelection() {
    if (el.messageList) {
      var active = el.messageList.querySelector('.message-item.active');
      if (active) active.classList.remove('active');
    }
  }

  async function patchFlags(id, patch, silent) {
    var m = state.byId[id];
    var detail = state.detailCache[id] ? pickMessage(state.detailCache[id]) : null;
    if (!m && !detail) return null;
    var before = m ? { isRead: m.isRead, isStarred: m.isStarred } : null;
    var wasUnread = m ? !m.isRead : (detail && detail.isRead === false);
    var folderId = (m && m.folderId) || (detail && detail.folderId) || '';

    if (m) {
      if (patch.isRead !== undefined) m.isRead = !!patch.isRead;
      if (patch.isStarred !== undefined) m.isStarred = !!patch.isStarred;
      updateItemFlags(id);
    }
    syncReaderFlags(id);
    if (patch.isRead === true && wasUnread) adjustUnread(folderId, -1);

    try {
      var res = await api(API + '/messages/' + encodeURIComponent(id), { method: 'PATCH', body: patch });
      var updated = res ? pickMessage(res) : null;
      if (updated && updated !== res && updated.id) {
        if (m && updated.isRead !== undefined) m.isRead = !!updated.isRead;
        if (m && updated.isStarred !== undefined) m.isStarred = !!updated.isStarred;
        updateItemFlags(id);
        syncReaderFlags(id);
      }
      if (detail) {
        if (patch.isRead !== undefined) detail.isRead = !!patch.isRead;
        if (patch.isStarred !== undefined) detail.isStarred = !!patch.isStarred;
      }
      return res;
    } catch (err) {
      if (m && before) {
        m.isRead = before.isRead;
        m.isStarred = before.isStarred;
        updateItemFlags(id);
        syncReaderFlags(id);
        if (patch.isRead === true && wasUnread) adjustUnread(folderId, 1);
      }
      if (!silent) toastError(err);
      return null;
    }
  }

  function syncReaderFlags(id) {
    if (state.selectedId !== id || !el.reader) return;
    var m = state.byId[id];
    var detail = state.detailCache[id] ? pickMessage(state.detailCache[id]) : null;
    var starred = m ? m.isStarred : (detail ? !!detail.isStarred : false);
    var read = m ? m.isRead : (detail ? !!detail.isRead : true);
    var starBtn = el.reader.querySelector('[data-act="star"]');
    if (starBtn) {
      starBtn.textContent = starred ? '★ 已星标' : '☆ 星标';
      starBtn.setAttribute('aria-pressed', starred ? 'true' : 'false');
    }
    var readBtn = el.reader.querySelector('[data-act="read"]');
    if (readBtn) {
      readBtn.textContent = read ? '标为未读' : '标为已读';
      readBtn.setAttribute('aria-pressed', read ? 'true' : 'false');
    }
  }

  async function toggleStar(id) {
    await ensureCached(id);
    var m = state.byId[id];
    var detail = state.detailCache[id] ? pickMessage(state.detailCache[id]) : null;
    var current = m ? !!m.isStarred : !!(detail && detail.isStarred);
    await patchFlags(id, { isStarred: !current });
  }

  async function toggleRead(id) {
    await ensureCached(id);
    var m = state.byId[id];
    var detail = state.detailCache[id] ? pickMessage(state.detailCache[id]) : null;
    var current = m ? !!m.isRead : !!(detail && detail.isRead);
    await patchFlags(id, { isRead: !current });
  }

  async function deleteMessage(id) {
    if (!id) return;
    var m = state.byId[id];
    var subject = m ? m.subject : '这封邮件';
    if (!window.confirm('确定删除「' + subject + '」？')) return;
    try {
      await api(API + '/messages/' + encodeURIComponent(id), { method: 'DELETE' });
      if (m && !m.isRead) adjustUnread(m.folderId, -1);
      removeFromList(id);
      delete state.detailCache[id];
      if (state.selectedId === id) {
        document.body.classList.remove('reading');
        clearReader();
      }
      toastText('已删除：' + subject);
      announce('已删除邮件');
    } catch (err) {
      toastError(err);
    }
  }

  /* ---------- Composer（写信 / 回复 / 转发） ---------- */

  var composerState = {
    files: [],
    mode: 'new',
    inReplyTo: '',
    references: '',
    sending: false,
    open: false
  };

  var COMPOSE_TITLES = { new: '新邮件', reply: '回复', forward: '转发' };

  function compose(mode) {
    var id = state.selectedId;
    var summary = id ? state.byId[id] : null;
    var detail = id && state.detailCache[id] ? pickMessage(state.detailCache[id]) : null;
    window.dispatchEvent(new CustomEvent('zmail:compose', {
      detail: {
        mode: mode || 'new',
        messageId: id || '',
        accountId: (summary && summary.accountId) || (detail && detail.accountId) || '',
        message: detail || summary || null
      }
    }));
  }

  function bindComposer() {
    window.addEventListener('zmail:compose', function (e) {
      var d = (e && e.detail) || {};
      openComposer(d.mode || 'new', d);
    });
    document.addEventListener('click', function (e) {
      var trigger = e.target.closest('#composeBtn,[data-compose]');
      if (!trigger) return;
      e.preventDefault();
      openComposer(trigger.getAttribute('data-compose') || 'new');
    });
  }

  function ensureComposerDom() {
    if (el.composer) return;
    var wrap = document.createElement('div');
    wrap.id = 'composer';
    wrap.className = 'composer hidden';
    wrap.setAttribute('role', 'dialog');
    wrap.setAttribute('aria-modal', 'true');
    wrap.setAttribute('aria-labelledby', 'composeTitle');
    wrap.innerHTML =
      '<div class="composer-dialog">' +
        '<div class="composer-head">' +
          '<h2 id="composeTitle" class="composer-title">新邮件</h2>' +
          '<button type="button" class="icon-btn" id="composeClose" aria-label="关闭写信窗口">✕</button>' +
        '</div>' +
        '<form id="composeForm" class="composer-body" novalidate>' +
          '<label class="composer-field"><span class="field-label">发件账号</span><select id="composeAccount"></select></label>' +
          '<label class="composer-field"><span class="field-label">收件人</span><input id="composeTo" type="text" autocomplete="off" placeholder="多个地址用逗号分隔" required></label>' +
          '<label class="composer-field"><span class="field-label">抄送</span><input id="composeCc" type="text" autocomplete="off" placeholder="可选"></label>' +
          '<label class="composer-field"><span class="field-label">主题</span><input id="composeSubject" type="text" autocomplete="off" maxlength="998" required></label>' +
          '<label class="composer-field field-grow"><span class="field-label">正文</span><textarea id="composeBody" spellcheck="true" placeholder="纯文本正文，Ctrl+Enter 发送"></textarea></label>' +
          '<div class="compose-attach-row">' +
            '<button type="button" class="btn" id="composeAttachBtn">添加附件</button>' +
            '<input id="composeFiles" class="sr-only" type="file" multiple aria-label="选择附件">' +
            '<span class="compose-hint">单个 ≤10MB，总计 ≤25MB</span>' +
          '</div>' +
          '<ul id="composeAttachmentList" class="compose-files"></ul>' +
          '<div class="composer-actions">' +
            '<button type="submit" class="btn btn-primary" id="composeSend">发送</button>' +
            '<button type="button" class="btn" id="composeCancel">取消</button>' +
            '<span id="composeStatus" class="compose-status" role="status"></span>' +
          '</div>' +
        '</form>' +
      '</div>';
    document.body.appendChild(wrap);

    el.composer = wrap;
    el.composeTitle = $('composeTitle');
    el.composeForm = $('composeForm');
    el.composeAccount = $('composeAccount');
    el.composeTo = $('composeTo');
    el.composeCc = $('composeCc');
    el.composeSubject = $('composeSubject');
    el.composeBody = $('composeBody');
    el.composeFiles = $('composeFiles');
    el.composeAttachBtn = $('composeAttachBtn');
    el.composeAttachmentList = $('composeAttachmentList');
    el.composeSend = $('composeSend');
    el.composeCancel = $('composeCancel');
    el.composeClose = $('composeClose');
    el.composeStatus = $('composeStatus');

    el.composeClose.addEventListener('click', function () { closeComposer(); });
    el.composeCancel.addEventListener('click', function () { closeComposer(); });
    el.composeAttachBtn.addEventListener('click', function () { el.composeFiles.click(); });
    el.composeFiles.addEventListener('change', function () {
      addComposeFiles(el.composeFiles.files);
      el.composeFiles.value = '';
    });
    el.composeForm.addEventListener('submit', submitComposer);
    el.composer.addEventListener('click', function (e) {
      if (e.target === el.composer) closeComposer();
    });
    el.composeAttachmentList.addEventListener('click', function (e) {
      var rm = e.target.closest('[data-remove]');
      if (!rm) return;
      var idx = Number(rm.getAttribute('data-remove'));
      if (idx >= 0 && idx < composerState.files.length) {
        composerState.files.splice(idx, 1);
        renderComposerFiles();
      }
    });
    el.composeBody.addEventListener('keydown', function (e) {
      if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
        e.preventDefault();
        submitComposer();
      }
    });
  }

  function ensureAccounts() {
    if (state.accounts.length) return Promise.resolve();
    return loadSidebar().catch(function () { /* 账号拉取失败时按无账号处理 */ });
  }

  function populateComposeAccounts(defaultId) {
    var sel = el.composeAccount;
    if (!sel) return;
    sel.innerHTML = '';
    if (!state.accounts.length) {
      var empty = document.createElement('option');
      empty.value = '';
      empty.textContent = '（暂无账号，请先在设置中添加）';
      sel.appendChild(empty);
      sel.disabled = true;
      return;
    }
    sel.disabled = false;
    state.accounts.forEach(function (a) {
      var opt = document.createElement('option');
      opt.value = a.id;
      opt.textContent = a.email || a.displayName || a.id;
      sel.appendChild(opt);
    });
    var pick = defaultId || cfg.accountId || '';
    if (!pick || !state.accountById[pick]) pick = state.accounts[0].id;
    sel.value = pick;
  }

  function plainBody(msg) {
    if (msg && msg.bodyText) return String(msg.bodyText);
    if (msg && msg.snippet) return String(msg.snippet);
    return '';
  }

  function quoteReply(msg) {
    var from = msg.fromName ? msg.fromName + ' <' + (msg.fromEmail || '') + '>' : (msg.fromEmail || '');
    var head = '\n\n在 ' + fmtFullTime(msg.receivedAt) + '，' + from + ' 写道：\n';
    var quoted = plainBody(msg).split('\n').map(function (line) {
      return line ? '> ' + line : '>';
    }).join('\n');
    return head + quoted;
  }

  function forwardBlock(msg) {
    var to = (msg.to || []).map(function (a) {
      return a && a.email ? a.email : String(a || '');
    }).filter(Boolean).join(', ');
    var lines = [
      '---------- 转发邮件 ----------',
      '发件人：' + (msg.fromName ? msg.fromName + ' <' + (msg.fromEmail || '') + '>' : (msg.fromEmail || '')),
      '收件人：' + (to || '(未知)'),
      '时间：' + fmtFullTime(msg.receivedAt),
      '主题：' + (msg.subject || '(无主题)'),
      ''
    ];
    var body = plainBody(msg);
    if (body) lines.push(body);
    return lines.join('\n');
  }

  function prefillComposer(msg, mode) {
    var subject = String(msg.subject || '').trim();
    if (mode === 'reply') {
      el.composeTo.value = msg.fromEmail || '';
      el.composeSubject.value = subject ? (/^re\s*:/i.test(subject) ? subject : 'Re: ' + subject) : 'Re: (无主题)';
      el.composeBody.value = quoteReply(msg);
      composerState.inReplyTo = msg.messageId || '';
      composerState.references = composerState.inReplyTo;
    } else if (mode === 'forward') {
      el.composeSubject.value = subject ? (/^fw(d)?\s*:/i.test(subject) ? subject : 'Fwd: ' + subject) : 'Fwd: (无主题)';
      el.composeBody.value = forwardBlock(msg);
    }
  }

  async function openComposer(mode, context) {
    context = context || {};
    ensureComposerDom();
    if (!el.composer) return;
    if (composerState.sending) {
      toastText('正在发送，请稍候');
      return;
    }
    resetComposerForm();
    composerState.open = true;
    composerState.mode = mode || 'new';
    el.composeTitle.textContent = COMPOSE_TITLES[composerState.mode] || '写信';
    closePalette();
    if (el.help) el.help.classList.add('hidden');
    document.body.classList.remove('sidebar-open');
    el.composer.classList.remove('hidden');
    document.body.classList.add('composer-open');

    await ensureAccounts();
    if (!composerState.open) return;
    populateComposeAccounts(context.accountId || '');

    if (composerState.mode === 'reply' || composerState.mode === 'forward') {
      var id = context.messageId || state.selectedId;
      var msg = context.message || null;
      if (!msg && id) {
        var data = await ensureDetail(id);
        if (!composerState.open) return;
        msg = data ? pickMessage(data) : null;
      }
      if (!msg) {
        closeComposer(true);
        toastText('请先选择一封邮件');
        return;
      }
      prefillComposer(msg, composerState.mode);
    }

    setTimeout(function () {
      if (!el.composeTo) return;
      if (composerState.mode === 'new' || !el.composeTo.value) el.composeTo.focus();
      else el.composeBody.focus();
    }, 0);
  }

  function closeComposer(force) {
    if (!el.composer) return;
    if (composerState.sending && !force) {
      toastText('正在发送，请稍候');
      return;
    }
    el.composer.classList.add('hidden');
    document.body.classList.remove('composer-open');
    composerState.open = false;
    if (force) resetComposerForm();
  }

  function resetComposerForm() {
    composerState.files = [];
    composerState.inReplyTo = '';
    composerState.references = '';
    composerState.sending = false;
    if (!el.composer) return;
    el.composeTo.value = '';
    el.composeCc.value = '';
    el.composeSubject.value = '';
    el.composeBody.value = '';
    if (el.composeFiles) el.composeFiles.value = '';
    setComposerSending(false, '');
    renderComposerFiles();
  }

  function setComposerSending(sending, text) {
    composerState.sending = sending;
    if (el.composeSend) {
      el.composeSend.disabled = sending;
      el.composeSend.textContent = sending ? '发送中…' : '发送';
    }
    if (el.composeCancel) el.composeCancel.disabled = sending;
    if (el.composeClose) el.composeClose.disabled = sending;
    if (el.composeStatus && text !== undefined) el.composeStatus.textContent = text;
  }

  function composerError(msg) {
    if (el.composeStatus) el.composeStatus.textContent = msg;
    toastText(msg, 'error');
  }

  function renderComposerFiles() {
    if (!el.composeAttachmentList) return;
    el.composeAttachmentList.innerHTML = '';
    composerState.files.forEach(function (f, idx) {
      var li = document.createElement('li');
      var name = span('compose-file-name', f.name);
      name.title = f.name;
      var size = span('compose-file-size', fmtSize(f.size));
      var rm = document.createElement('button');
      rm.type = 'button';
      rm.className = 'compose-file-remove';
      rm.setAttribute('data-remove', String(idx));
      rm.setAttribute('aria-label', '移除附件 ' + f.name);
      rm.textContent = '✕';
      li.appendChild(name);
      li.appendChild(size);
      li.appendChild(rm);
      el.composeAttachmentList.appendChild(li);
    });
  }

  function addComposeFiles(fileList) {
    var incoming = Array.prototype.slice.call(fileList || []);
    var total = composerState.files.reduce(function (n, f) { return n + f.size; }, 0);
    var maxSingle = 10 * 1024 * 1024;
    var maxTotal = 25 * 1024 * 1024;
    for (var i = 0; i < incoming.length; i++) {
      var f = incoming[i];
      if (composerState.files.length >= 20) {
        toastText('附件最多 20 个', 'error');
        break;
      }
      if (f.size > maxSingle) {
        toastText('附件「' + f.name + '」超过 10MB', 'error');
        continue;
      }
      if (total + f.size > maxTotal) {
        toastText('附件总大小超过 25MB', 'error');
        break;
      }
      composerState.files.push(f);
      total += f.size;
    }
    renderComposerFiles();
  }

  async function submitComposer(e) {
    if (e) e.preventDefault();
    if (!el.composer || composerState.sending) return;

    var accountId = el.composeAccount.value;
    var to = el.composeTo.value.trim();
    var cc = el.composeCc.value.trim();
    var subject = el.composeSubject.value.trim();
    var text = el.composeBody.value;

    if (!accountId) { composerError('请先在设置中添加邮箱账号'); return; }
    if (!to) { composerError('请填写收件人'); el.composeTo.focus(); return; }
    if (!subject) { composerError('请填写主题'); el.composeSubject.focus(); return; }
    if (!text.trim()) { composerError('请填写正文'); el.composeBody.focus(); return; }

    var form = new FormData();
    form.append('accountId', accountId);
    form.append('to', to);
    if (cc) form.append('cc', cc);
    form.append('subject', subject);
    form.append('textBody', text);
    if (composerState.inReplyTo) {
      form.append('inReplyTo', composerState.inReplyTo);
      form.append('references', composerState.references || composerState.inReplyTo);
    }
    composerState.files.forEach(function (f) { form.append('attachments', f, f.name); });

    setComposerSending(true, '发送中…');
    try {
      await api(API + '/messages/send', { method: 'POST', body: form });
      closeComposer(true);
      toastText('邮件已发送');
      announce('邮件已发送');
      state.mode = 'list';
      state.searchQuery = '';
      if (el.search) el.search.value = '';
      loadMessages(true);
    } catch (err) {
      setComposerSending(false, '');
      composerError(err && err.message ? err.message : '发送失败');
      toastError(err);
    }
  }

  /* ---------- 搜索 ---------- */

  async function runSearch(q) {
    state.mode = 'search';
    state.searchQuery = q;
    state.loading = true;
    showSkeleton();
    try {
      var data = await api(API + '/search?q=' + encodeURIComponent(q) + '&limit=50');
      var list = pickList(data);
      resetList();
      if (!list.length) {
        showListEmpty('没有找到匹配「' + q + '」的邮件');
      } else {
        appendMessages(list);
        state.hasMore = false;
        updateLoadMore();
      }
    } catch (err) {
      showListEmpty('搜索失败：' + (err && err.message ? err.message : '未知错误'));
      toastError(err);
    } finally {
      state.loading = false;
    }
  }

  function exitSearch() {
    if (state.mode !== 'search') return;
    state.mode = 'list';
    state.searchQuery = '';
    resetAndLoad();
  }

  /* ---------- SSE ---------- */

  function setSyncState(kind, text) {
    if (!el.syncState) return;
    el.syncState.setAttribute('data-state', kind || 'idle');
    el.syncState.textContent = text || '';
    el.syncState.title = text || '';
  }

  function aggregateSyncState() {
    var syncing = 0, errors = 0, lastErr = '';
    Object.keys(state.syncByAccount).forEach(function (k) {
      var st = state.syncByAccount[k];
      if (st.status === 'syncing') syncing++;
      if (st.status === 'error') { errors++; lastErr = st.message || lastErr; }
    });
    if (errors) setSyncState('error', lastErr ? '同步失败：' + lastErr : '同步失败');
    else if (syncing) setSyncState('syncing', '同步中…');
    else if (sseDown) setSyncState('offline', '重连中…');
    else setSyncState('idle', '已连接');
    updateAccountStates();
  }

  function updateAccountStates() {
    if (!el.sidebarContent) return;
    Array.prototype.forEach.call(el.sidebarContent.querySelectorAll('.account-group'), function (g) {
      var id = g.getAttribute('data-account-id');
      var st = state.syncByAccount[id];
      var label = g.querySelector('.account-state');
      if (!label) return;
      var status = st ? st.status : '';
      label.textContent = status === 'error' ? '异常' : syncLabel(status);
      label.setAttribute('data-state', status || '');
      if (st && st.message) label.title = st.message;
    });
  }

  function folderOfMessage(m) {
    if (m.folderId) return folderById(m.folderId);
    return null;
  }

  function matchesView(m) {
    if (state.mode === 'search') return false;
    if (state.folderId) return m.folderId === state.folderId;
    if (state.scope !== 'inbox') return false;
    var f = folderOfMessage(m);
    if (f) return f.role === 'inbox';
    return true; // 未知文件夹时按收件箱处理
  }

  function prependMessage(raw) {
    if (!el.messageList) return;
    var m = normalizeSummary(raw);
    if (!m.id || state.byId[m.id]) return;
    state.messages.unshift(m);
    state.byId[m.id] = m;

    var empty = el.messageList.querySelector('.empty');
    if (empty) empty.remove();
    var loadMore = el.messageList.querySelector('.load-more');
    if (loadMore) loadMore.remove();

    var g = groupOf(m.receivedAt);
    var firstLabel = el.messageList.querySelector('.group-label');
    var frag = document.createDocumentFragment();
    var needLabel = !firstLabel || firstLabel.textContent !== g;
    if (needLabel) frag.appendChild(groupLabel(g));
    var node = messageNode(m);
    state.nodes[m.id] = node;
    frag.appendChild(node);

    var ref = null;
    if (needLabel) ref = el.messageList.firstChild;
    else ref = firstLabel.nextSibling;
    el.messageList.insertBefore(frag, ref);
    if (state.hasMore) updateLoadMore();
  }

  function onMailNew(ev) {
    var data;
    try { data = JSON.parse(ev.data); } catch (e) { return; }
    if (!data || !data.id) return;
    var m = normalizeSummary(data);
    if (matchesView(m)) prependMessage(data);
    adjustUnread(m.folderId, 1);
    announce('收到新邮件，来自' + (m.fromName || '未知发件人') + '，' + m.subject);
    if (!state.selectedId && document.visibilityState === 'hidden') {
      document.title = '● ' + document.title;
    }
  }

  function onMailCode(ev) {
    var data;
    try { data = JSON.parse(ev.data); } catch (e) { return; }
    var code = data && (data.code || data.value);
    if (!code) return;
    var title = (data.from || data.fromName || '') + (data.subject ? ' · ' + data.subject : '');
    showToast({
      title: title || '验证码邮件',
      code: String(code),
      hint: '点击复制验证码'
    });
    announce('收到验证码 ' + String(code).split('').join(' '));
    notify('验证码 ' + code, title || 'zMail 验证码');
  }

  function onMailUpdate(ev) {
    var data;
    try { data = JSON.parse(ev.data); } catch (e) { return; }
    if (!data || !data.id) return;
    var m = state.byId[data.id];
    if (m) {
      if (data.isRead !== undefined) m.isRead = !!data.isRead;
      if (data.isStarred !== undefined) m.isStarred = !!data.isStarred;
      updateItemFlags(data.id);
      syncReaderFlags(data.id);
    }
    if (data.unreadTotal !== undefined) {
      state.unreadTotal = Math.max(0, Number(data.unreadTotal) || 0);
      updateUnreadUI();
    }
  }

  function onSyncStatus(ev) {
    var data;
    try { data = JSON.parse(ev.data); } catch (e) { return; }
    if (!data) return;
    if (data.accountId) {
      state.syncByAccount[data.accountId] = {
        status: data.status || data.state || '',
        message: data.message || data.lastError || ''
      };
    }
    aggregateSyncState();
  }

  function initSSE() {
    if (typeof EventSource === 'undefined') {
      setSyncState('offline', '浏览器不支持实时推送');
      return;
    }
    try {
      es = new EventSource(base + API + '/events');
    } catch (e) {
      setSyncState('offline', '实时推送连接失败');
      return;
    }
    es.onopen = function () {
      var wasDown = sseDown;
      sseDown = false;
      aggregateSyncState();
      if (wasDown) refreshIncremental();
    };
    es.onerror = function () {
      sseDown = true;
      aggregateSyncState();
    };
    if (es.addEventListener) {
      es.addEventListener('mail:new', onMailNew);
      es.addEventListener('mail:code', onMailCode);
      es.addEventListener('mail:update', onMailUpdate);
      es.addEventListener('sync:status', onSyncStatus);
    }
  }

  async function refreshIncremental() {
    if (!el.messageList || state.mode === 'search') return;
    try {
      var data = await api(messageQuery(true));
      var list = pickList(data);
      var fresh = list.map(normalizeSummary).filter(function (m) { return m.id && !state.byId[m.id]; });
      for (var i = fresh.length - 1; i >= 0; i--) prependMessage(fresh[i]);
    } catch (e) {
      /* 重连后的增量刷新失败不打扰用户 */
    }
  }

  function notify(title, body) {
    if (!('Notification' in window) || Notification.permission !== 'granted') return;
    try {
      new Notification(title, { body: body, tag: 'zmail-code', icon: base + '/static/icons/icon-192.png' });
    } catch (e) { /* 部分浏览器要求 SW 通知 */ }
  }

  function requestNotify() {
    if (!('Notification' in window)) {
      toastText('当前浏览器不支持桌面通知', 'error');
      return;
    }
    Notification.requestPermission().then(function (permission) {
      if (permission === 'granted') {
        toastText('桌面通知已开启');
        notify('zMail', '通知已开启，验证码邮件会弹窗提醒');
      } else {
        toastText('桌面通知未开启，可在浏览器设置中允许');
      }
    }).catch(function () {
      toastText('桌面通知申请失败', 'error');
    });
  }

  /* ---------- 命令面板 ---------- */

  function commandItems() {
    var items = [];
    items.push({
      label: '全部收件箱', kind: '文件夹', group: '跳转',
      run: function () { selectFolderByAttrs(null, '全部收件箱'); }
    });
    state.accounts.forEach(function (acc) {
      state.folders.filter(function (f) { return f.accountId === acc.id; }).forEach(function (f) {
        items.push({
          label: (acc.email || acc.id) + ' · ' + folderLabel(f), kind: '文件夹', group: '跳转',
          run: function () { selectFolderByAttrs(f.id, folderLabel(f)); }
        });
      });
    });
    items.push({
      label: '切换深色 / 浅色主题', kind: '操作', group: '操作',
      run: function () { toggleTheme(); }
    });
    items.push({
      label: '刷新当前列表', kind: '操作', group: '操作',
      run: function () { loadMessages(true); }
    });
    items.push({
      label: '开启桌面通知', kind: '操作', group: '操作',
      run: requestNotify
    });
    items.push({
      label: '显示快捷键帮助', kind: '操作', group: '操作',
      run: function () { toggleHelp(true); }
    });
    if (state.selectedId) {
      items.push({
        label: '标记当前邮件为已读', kind: '邮件', group: '操作',
        run: function () { patchFlags(state.selectedId, { isRead: true }); }
      });
      items.push({
        label: '切换当前邮件星标', kind: '邮件', group: '操作',
        run: function () { toggleStar(state.selectedId); }
      });
    }
    return items;
  }

  function selectFolderByAttrs(folderId, name) {
    state.folderId = folderId || null;
    state.scope = folderId ? '' : 'inbox';
    state.folderName = name || '全部收件箱';
    state.mode = 'list';
    state.searchQuery = '';
    if (el.search) el.search.value = '';
    document.body.classList.remove('reading');
    clearReader();
    refreshSidebarActive();
    resetAndLoad();
  }

  async function paletteSearch(q) {
    var seq = ++state.paletteSeq;
    try {
      var data = await api(API + '/search?q=' + encodeURIComponent(q) + '&limit=20');
      if (seq !== state.paletteSeq) return;
      var list = pickList(data).map(normalizeSummary).filter(function (m) { return m.id; });
      state.paletteItems = list.map(function (m) {
        return {
          label: (m.fromName || '') + ' · ' + m.subject,
          kind: '邮件',
          group: '邮件',
          run: function () { closePalette(); openMessage(m.id); }
        };
      });
      state.paletteIndex = 0;
      renderPalette();
    } catch (e) {
      if (seq !== state.paletteSeq) return;
      state.paletteItems = [];
      renderPalette(true);
    }
  }

  function renderPalette(error) {
    if (!el.paletteResults) return;
    el.paletteResults.innerHTML = '';
    var items = state.paletteItems;
    if (!items.length) {
      el.paletteResults.appendChild(div('palette-empty', error ? '搜索失败，请稍后重试' : '无匹配结果'));
      return;
    }
    var lastGroup = '';
    items.forEach(function (it, idx) {
      var g = it.group || '操作';
      if (g !== lastGroup) {
        lastGroup = g;
        el.paletteResults.appendChild(div('palette-group', g));
      }
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'palette-item' + (idx === state.paletteIndex ? ' active' : '');
      b.setAttribute('data-index', String(idx));
      b.appendChild(span('palette-label', it.label));
      b.appendChild(span('palette-kind', it.kind || ''));
      el.paletteResults.appendChild(b);
    });
    var active = el.paletteResults.querySelector('.palette-item.active');
    if (active) active.scrollIntoView({ block: 'nearest' });
  }

  function openPalette() {
    if (!el.palette) return;
    el.palette.classList.remove('hidden');
    state.paletteItems = commandItems();
    state.paletteIndex = 0;
    state.paletteSeq++;
    if (el.paletteInput) el.paletteInput.value = '';
    renderPalette();
    if (el.paletteInput) el.paletteInput.focus();
  }

  function closePalette() {
    if (el.palette) el.palette.classList.add('hidden');
    state.paletteSeq++;
  }

  function togglePalette() {
    if (!el.palette) return;
    if (el.palette.classList.contains('hidden')) openPalette();
    else closePalette();
  }

  function onPaletteInput() {
    var q = el.paletteInput.value.trim();
    state.paletteIndex = 0;
    if (q.length >= 2) {
      paletteSearch(q);
    } else {
      state.paletteSeq++;
      state.paletteItems = commandItems();
      if (q) {
        state.paletteItems = state.paletteItems.filter(function (it) {
          return it.label.toLowerCase().indexOf(q.toLowerCase()) >= 0;
        });
      }
      renderPalette();
    }
  }

  function runPaletteIndex(idx) {
    var it = state.paletteItems[idx];
    if (!it) return;
    closePalette();
    try { it.run(); } catch (e) { toastError(e); }
  }

  /* ---------- 帮助面板 ---------- */

  function renderHelp() {
    if (!el.help || el.help.getAttribute('data-ready') === '1') return;
    el.help.setAttribute('data-ready', '1');
    el.help.innerHTML = '';
    var h = document.createElement('h2');
    h.textContent = '快捷键与帮助';
    el.help.appendChild(h);

    var grid = div('help-grid');
    [
      ['j / k', '下移 / 上移选中邮件'],
      ['Enter', '打开选中的邮件'],
      ['r / c', '回复 / 写信'],
      ['e', '归档（从当前列表删除）'],
      ['s', '切换星标'],
      ['Delete', '删除选中邮件'],
      ['/', '聚焦搜索框'],
      ['Ctrl + K', '打开命令面板'],
      ['?', '显示 / 隐藏本面板'],
      ['Esc', '关闭面板或返回列表']
    ].forEach(function (pair) {
      var k = document.createElement('kbd');
      k.textContent = pair[0];
      grid.appendChild(k);
      grid.appendChild(div('', pair[1]));
    });
    el.help.appendChild(grid);

    var actions = div('help-actions');
    var notifyBtn = actionButton('开启桌面通知', 'notify');
    var closeBtn = actionButton('关闭', 'close-help');
    actions.appendChild(notifyBtn);
    actions.appendChild(closeBtn);
    el.help.appendChild(actions);
  }

  function toggleHelp(force) {
    if (!el.help) return;
    var willShow = force === true ? true : el.help.classList.contains('hidden');
    if (willShow) {
      renderHelp();
      el.help.classList.remove('hidden');
      announce('快捷键帮助已打开');
    } else {
      el.help.classList.add('hidden');
    }
  }

  /* ---------- 事件绑定 ---------- */

  // ensureOverlays 在设置页等没有弹层标记的页面按需创建帮助、Toast 与命令面板，
  // 保证顶栏的「?」按钮与全局快捷键在任何页面都可用。
  function ensureOverlays() {
    if (!$('help')) {
      var help = document.createElement('div');
      help.id = 'help';
      help.className = 'help hidden';
      help.setAttribute('role', 'dialog');
      help.setAttribute('aria-label', '快捷键帮助');
      document.body.appendChild(help);
    }
    if (!$('toast')) {
      var toast = document.createElement('div');
      toast.id = 'toast';
      toast.className = 'toast hidden';
      toast.setAttribute('role', 'status');
      toast.setAttribute('aria-live', 'polite');
      document.body.appendChild(toast);
    }
    if (!$('palette')) {
      var palette = document.createElement('div');
      palette.id = 'palette';
      palette.className = 'palette hidden';
      palette.setAttribute('role', 'dialog');
      palette.setAttribute('aria-label', '命令面板');
      var input = document.createElement('input');
      input.id = 'paletteInput';
      input.placeholder = '搜索邮件或输入命令...';
      input.setAttribute('autocomplete', 'off');
      var results = document.createElement('div');
      results.id = 'paletteResults';
      results.className = 'palette-results';
      palette.appendChild(input);
      palette.appendChild(results);
      document.body.appendChild(palette);
    }
  }

  function cacheDom() {
    ensureOverlays();
    el.sidebar = $('sidebar');
    el.sidebarContent = $('sidebarContent');
    el.messageList = $('messageList');
    el.reader = $('reader');
    el.search = $('search');
    el.toast = $('toast');
    el.palette = $('palette');
    el.paletteInput = $('paletteInput');
    el.paletteResults = $('paletteResults');
    el.help = $('help');
    el.syncState = $('syncState');
    el.themeBtn = $('themeBtn');
    el.helpBtn = $('helpBtn');
    el.menuBtn = $('menuBtn');
    el.refreshBtn = $('refreshBtn');
    el.listTitle = $('listTitle');
    el.listCount = $('listCount');
    el.listMeta = $('listMeta');
  }

  function bindUI() {
    if (el.sidebarContent) {
      el.sidebarContent.addEventListener('click', function (e) {
        var toggle = e.target.closest('[data-toggle-account]');
        if (toggle) {
          e.preventDefault();
          toggleAccountGroup(toggle);
          return;
        }
        var btn = e.target.closest('.folder-item');
        if (!btn) return;
        e.preventDefault(); // 服务端渲染时为 <a>，交给前端做局部刷新
        selectFolder(btn);
      });
    }
    if (el.messageList) {
      el.messageList.addEventListener('click', function (e) {
        var star = e.target.closest('[data-star]');
        if (star) {
          e.stopPropagation();
          toggleStar(star.getAttribute('data-star'));
          return;
        }
        var item = e.target.closest('.message-item');
        if (item) {
          // 服务端渲染的列表项是 <a href>，必须阻止默认跳转，否则会整页刷新；
          // 移动端整页刷新后不会带上 body.reading，看起来就像“点了没反应”。
          e.preventDefault();
          openMessage(item.getAttribute('data-id'));
        }
      });
      el.messageList.addEventListener('keydown', function (e) {
        if (e.key === 'Enter' || e.key === ' ') {
          var item = e.target.closest('.message-item');
          if (item) {
            e.preventDefault();
            openMessage(item.getAttribute('data-id'));
          }
        }
      });
      el.messageList.addEventListener('mouseover', function (e) {
        var item = e.target.closest('.message-item');
        if (!item) return;
        clearTimeout(prefetchTimer);
        prefetchTimer = setTimeout(function () { prefetch(item.getAttribute('data-id')); }, 150);
      });
      el.messageList.addEventListener('mouseleave', function () { clearTimeout(prefetchTimer); });
      el.messageList.addEventListener('scroll', function () {
        if (state.mode !== 'list' || !state.hasMore || state.loading) return;
        if (el.messageList.scrollHeight - el.messageList.scrollTop - el.messageList.clientHeight < 320) {
          loadMessages(false);
        }
      }, { passive: true });
    }
    if (el.reader) {
      el.reader.addEventListener('click', function (e) {
        // 兼容服务端首次渲染的阅读器按钮（data-action），操作后切换为前端渲染。
        var ssrBtn = e.target.closest('[data-action]');
        if (ssrBtn) {
          e.preventDefault();
          var ssrId = ssrBtn.getAttribute('data-id') || state.selectedId;
          if (!ssrId) return;
          var ssrAct = ssrBtn.getAttribute('data-action');
          if (ssrAct === 'back') {
            e.preventDefault();
            document.body.classList.remove('reading');
            return;
          }
          state.selectedId = ssrId;
          if (ssrAct === 'toggle-star') {
            toggleStar(ssrId).then(function () { syncSSRReader(ssrId); });
          } else if (ssrAct === 'toggle-read') {
            toggleRead(ssrId).then(function () { syncSSRReader(ssrId); });
          } else if (ssrAct === 'delete') {
            deleteMessage(ssrId).then(function () {
              if (el.reader && !el.reader.querySelector('[data-act]')) {
                readerEmpty('选择一封邮件');
                document.body.classList.remove('reading');
              }
            });
          }
          return;
        }
        var btn = e.target.closest('[data-act]');
        if (!btn) return;
        var act = btn.getAttribute('data-act');
        var id = state.selectedId;
        if (act === 'reply') compose('reply');
        else if (act === 'forward') compose('forward');
        else if (act === 'prev') openRelative(-1);
        else if (act === 'next') openRelative(1);
        else if (act === 'star' && id) toggleStar(id);
        else if (act === 'read' && id) toggleRead(id);
        else if (act === 'delete' && id) deleteMessage(id);
        else if (act === 'back') document.body.classList.remove('reading');
        else if (act === 'quote') {
          var body = el.reader.querySelector('.reader-body');
          if (!body) return;
          body.classList.toggle('quotes-collapsed');
          var hidden = body.classList.contains('quotes-collapsed');
          btn.textContent = hidden ? '显示引用' : '隐藏引用';
          state.quotesCollapsed = hidden;
        } else if (act === 'copy-code') {
          var codeEl = el.reader.querySelector('#codeBox .code-value');
          if (codeEl) copyText(codeEl.textContent).then(function () { toastText('验证码已复制'); }).catch(function () { toastText('复制失败', 'error'); });
        }
      });
    }
    if (el.search) {
      el.search.addEventListener('input', function () {
        clearTimeout(searchTimer);
        var q = el.search.value.trim();
        if (!q) { exitSearch(); return; }
        searchTimer = setTimeout(function () { runSearch(q); }, 280);
      });
      el.search.addEventListener('keydown', function (e) {
        if (e.key === 'Escape') {
          el.search.value = '';
          el.search.blur();
          exitSearch();
        }
      });
    }
    if (el.menuBtn) el.menuBtn.addEventListener('click', function () { document.body.classList.toggle('sidebar-open'); });
    if (el.themeBtn) el.themeBtn.addEventListener('click', toggleTheme);
    if (el.helpBtn) el.helpBtn.addEventListener('click', function () { toggleHelp(); });
    if (el.refreshBtn) el.refreshBtn.addEventListener('click', function (e) {
      e.preventDefault();
      loadMessages(true);
    });

    if (el.help) {
      el.help.addEventListener('click', function (e) {
        var btn = e.target.closest('[data-act]');
        if (!btn) return;
        if (btn.getAttribute('data-act') === 'notify') requestNotify();
        else if (btn.getAttribute('data-act') === 'close-help') el.help.classList.add('hidden');
      });
    }
    if (el.palette) {
      if (el.paletteInput) {
        el.paletteInput.addEventListener('input', onPaletteInput);
        el.paletteInput.addEventListener('keydown', function (e) {
          var n = state.paletteItems.length;
          if (e.key === 'ArrowDown') {
            e.preventDefault();
            state.paletteIndex = Math.min(n - 1, state.paletteIndex + 1);
            renderPalette();
          } else if (e.key === 'ArrowUp') {
            e.preventDefault();
            state.paletteIndex = Math.max(0, state.paletteIndex - 1);
            renderPalette();
          } else if (e.key === 'Enter') {
            e.preventDefault();
            runPaletteIndex(state.paletteIndex);
          } else if (e.key === 'Escape') {
            e.preventDefault();
            closePalette();
          }
        });
      }
      if (el.paletteResults) {
        el.paletteResults.addEventListener('click', function (e) {
          var item = e.target.closest('[data-index]');
          if (item) runPaletteIndex(Number(item.getAttribute('data-index')));
        });
      }
    }

    document.addEventListener('keydown', onGlobalKey);
    document.addEventListener('click', function (e) {
      if (el.palette && !el.palette.classList.contains('hidden')) {
        if (!el.palette.contains(e.target)) closePalette();
      }
      if (el.help && !el.help.classList.contains('hidden') && !el.help.contains(e.target) && !e.target.closest('#helpBtn')) {
        el.help.classList.add('hidden');
      }
      if (document.body.classList.contains('sidebar-open') && !e.target.closest('.sidebar') && !e.target.closest('#menuBtn')) {
        document.body.classList.remove('sidebar-open');
      }
    });
    document.addEventListener('visibilitychange', function () {
      if (document.visibilityState === 'visible') updateUnreadUI();
    });
  }

  function onGlobalKey(e) {
    var target = e.target;
    var tag = (target && target.tagName || '').toLowerCase();
    var typing = tag === 'input' || tag === 'textarea' || tag === 'select' || (target && target.isContentEditable);

    // 写信弹层打开时，除 Esc 外不响应全局快捷键。
    if (composerState.open && e.key !== 'Escape') return;
    if ((e.ctrlKey || e.metaKey) && !e.altKey && (e.key === 'k' || e.key === 'K')) {
      e.preventDefault();
      togglePalette();
      return;
    }
    if (e.key === 'Escape') {
      if (composerState.open) { closeComposer(); return; }
      if (el.palette && !el.palette.classList.contains('hidden')) { closePalette(); return; }
      if (el.help && !el.help.classList.contains('hidden')) { el.help.classList.add('hidden'); return; }
      if (document.body.classList.contains('sidebar-open')) { document.body.classList.remove('sidebar-open'); return; }
      if (document.body.classList.contains('reading')) { document.body.classList.remove('reading'); return; }
      return;
    }
    if (typing) return;
    if (e.ctrlKey || e.metaKey || e.altKey) return;

    var key = e.key;
    if (key === '?') { e.preventDefault(); toggleHelp(); return; }
    if (key === '/') { e.preventDefault(); if (el.search) { el.search.focus(); el.search.select(); } return; }
    if (key === 'j' || key === 'J') {
      e.preventDefault();
      if (document.body.classList.contains('reading')) openRelative(1);
      else moveSelection(1);
      return;
    }
    if (key === 'k' || key === 'K') {
      e.preventDefault();
      if (document.body.classList.contains('reading')) openRelative(-1);
      else moveSelection(-1);
      return;
    }
    if (key === 'Enter') {
      if (state.selectedId) { e.preventDefault(); openMessage(state.selectedId); }
      return;
    }
    if (key === 's' || key === 'S') { if (state.selectedId) { e.preventDefault(); toggleStar(state.selectedId); } return; }
    if (key === 'e' || key === 'E') { if (state.selectedId) { e.preventDefault(); deleteMessage(state.selectedId); } return; }
    if (key === 'Delete') { if (state.selectedId) { e.preventDefault(); deleteMessage(state.selectedId); } return; }
    if (key === 'r' || key === 'R') { compose('reply'); return; }
    if (key === 'c' || key === 'C') { compose('new'); return; }
  }

  /* ---------- PWA ---------- */

  function registerSW() {
    if (!('serviceWorker' in navigator)) return;
    if (location.protocol !== 'https:' && location.hostname !== 'localhost' && location.hostname !== '127.0.0.1') return;
    window.addEventListener('load', function () {
      var urls = [base + '/static/sw.js', base + '/sw.js'];
      function tryNext(i) {
        if (i >= urls.length) return;
        navigator.serviceWorker.register(urls[i]).catch(function () { tryNext(i + 1); });
      }
      tryNext(0);
    });
  }

  /* ---------- 启动 ---------- */

  // bindSettingsChrome 为没有邮件列表的页面（设置页）绑定主题与快捷键帮助。
  function bindSettingsChrome() {
    if (el.themeBtn) el.themeBtn.addEventListener('click', toggleTheme);
    if (el.helpBtn) el.helpBtn.addEventListener('click', function (e) {
      e.preventDefault();
      toggleHelp();
    });
    if (el.help) {
      el.help.addEventListener('click', function (e) {
        var btn = e.target.closest('[data-act]');
        if (!btn) return;
        if (btn.getAttribute('data-act') === 'notify') requestNotify();
        else if (btn.getAttribute('data-act') === 'close-help') el.help.classList.add('hidden');
      });
    }
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape' && el.help && !el.help.classList.contains('hidden')) {
        el.help.classList.add('hidden');
        return;
      }
      var tag = (e.target && e.target.tagName || '').toLowerCase();
      if (tag === 'input' || tag === 'textarea' || tag === 'select' || (e.target && e.target.isContentEditable)) return;
      if (e.key === '?') {
        e.preventDefault();
        toggleHelp();
      }
    });
    document.addEventListener('click', function (e) {
      if (el.help && !el.help.classList.contains('hidden') && !el.help.contains(e.target) && !e.target.closest('#helpBtn')) {
        el.help.classList.add('hidden');
      }
    });
    registerSW();
  }

  function boot() {
    cacheDom();
    initTheme();
    if (!el.messageList || !el.reader) {
      // 设置页没有邮件列表，但仍允许切换主题与查看快捷键帮助。
      bindSettingsChrome();
      return;
    }
    state.booted = true;
    bindUI();
    bindComposer();
    updateUnreadUI();
    if (!el.reader.querySelector('.reader-head')) readerEmpty('选择一封邮件');
    if (!el.messageList.querySelector('.message-item')) showSkeleton();
    loadSidebar().then(function () {
      if (state.folderId) {
        var f = folderById(state.folderId);
        state.scope = '';
        state.folderName = f ? folderLabel(f) : '邮件';
        refreshSidebarActive();
      }
      return resetAndLoad();
    }).then(function () {
      if (cfg.messageId) openMessage(cfg.messageId);
    }).catch(function (err) {
      toastError(err);
    });
    initSSE();
    registerSW();
    aggregateSyncState();

    window.addEventListener('zmail:refresh', function () { loadSidebar().then(resetAndLoad); });
    window.zmail = window.zmail || {};
    window.zmail.refresh = function () { loadSidebar().then(resetAndLoad); };
    window.zmail.open = openMessage;
    window.zmail.compose = compose;
    window.zmail.toast = toastText;
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
