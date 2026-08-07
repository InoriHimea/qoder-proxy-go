/* global app.js — vanilla JS SPA */
'use strict';

// ── State ────────────────────────────────────────────────────────────────────
const state = {
  page: 'endpoints',
  config: {},
  settings: { backend: 'global', token: '', hasToken: false, models: [] },
  status: { status: 'unknown', uptime: 0, memoryMB: '—', version: '' },
  models: [],
  chat: { messages: [], model: 'auto', streaming: false, streamMode: 'stream' },
  logs: { entries: [], filter: '', autoRefresh: false, timer: null, expanded: null },
  sysLogs: { entries: [], filter: '', level: 'all', autoRefresh: false, timer: null },
};

// ── Utils ────────────────────────────────────────────────────────────────────
const $ = (id) => document.getElementById(id);
const el = (tag, cls, html) => { const e = document.createElement(tag); if (cls) e.className = cls; if (html) e.innerHTML = html; return e; };

async function api(url, opts = {}) {
  const r = await fetch(url, { headers: { 'Content-Type': 'application/json' }, ...opts });
  if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
  return r.json();
}

function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  return d.toLocaleTimeString('en-GB', { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0');
}
function fmtDate(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  return d.toLocaleDateString('en-GB', { day: '2-digit', month: 'short' }) + ' ' + d.toLocaleTimeString('en-GB', { hour12: false });
}
function fmtUptime(s) {
  s = Math.floor(s);
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  return h ? `${h}h ${m}m` : m ? `${m}m ${sec}s` : `${sec}s`;
}
function fmtMs(ms) { return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(2)}s`; }

function escHtml(str) {
  if (str == null) return '';
  return String(str).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function mdToHtml(text) {
  let h = escHtml(text);
  h = h.replace(/```(?:[a-z]*)\n?([\s\S]*?)```/g, (_, c) => `<pre><code>${c}</code></pre>`);
  h = h.replace(/`([^`]+)`/g, '<code>$1</code>');
  h = h.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  h = h.replace(/\n/g, '<br>');
  return h;
}

function syntaxJson(obj) {
  if (obj == null) return '<span class="json-null">null</span>';
  const str = typeof obj === 'string' ? obj : JSON.stringify(obj, null, 2);
  return str.replace(/("(\\u[a-zA-Z0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d*)?(?:[eE][+-]?\d+)?)/g, (m) => {
    if (/^"/.test(m)) return /:$/.test(m) ? `<span class="json-key">${m}</span>` : `<span class="json-string">${m}</span>`;
    if (/true|false/.test(m)) return `<span class="json-bool">${m}</span>`;
    if (/null/.test(m)) return `<span class="json-null">${m}</span>`;
    return `<span class="json-number">${m}</span>`;
  });
}

window.skipToContent = (event) => {
  event.preventDefault();
  $('content').focus();
};

async function copyText(text, btn) {
  try {
    await navigator.clipboard.writeText(text);
    const orig = btn.textContent;
    btn.textContent = 'Copied';
    btn.classList.add('copied');
    setTimeout(() => { btn.textContent = orig; btn.classList.remove('copied'); }, 1800);
  } catch {
    showToast('Copy failed — select and copy the value manually.', 'error');
  }
}

function showToast(msg, type = 'success') {
  const t = el('div', `toast toast-${type}`, msg);
  document.body.appendChild(t);
  setTimeout(() => t.classList.add('show'), 10);
  setTimeout(() => { t.classList.remove('show'); setTimeout(() => t.remove(), 400); }, 3000);
}

// ── Theme ────────────────────────────────────────────────────────────────────
window.toggleTheme = () => {
  const cur = document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
  const next = cur === 'light' ? 'dark' : 'light';
  document.documentElement.setAttribute('data-theme', next);
  try { localStorage.setItem('qoder-theme', next); } catch (e) {}
};

// ── Routing ──────────────────────────────────────────────────────────────────
const routes = {
  endpoints: renderEndpoints,
  playground: renderPlayground,
  logs: renderLogs,
  'system-logs': renderSystemLogs,
  settings: renderSettings,
};

function navigateTo(page) {
  if (page === 'usage') page = 'settings';
  if (!routes[page]) page = 'endpoints';
  clearInterval(state.logs.timer); state.logs.autoRefresh = false;
  clearInterval(state.sysLogs.timer); state.sysLogs.autoRefresh = false;
  state.page = page;
  window.location.hash = page;
  updateSidebar();
  closeSidebar();
  routes[page]();
  $('content').focus({ preventScroll: true });
}

window.addEventListener('hashchange', () => navigateTo(window.location.hash.slice(1)));

function updateSidebar() {
  document.querySelectorAll('.nav-item').forEach(a => {
    const active = a.dataset.page === state.page;
    a.classList.toggle('active', active);
    if (active) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
}

function closeSidebar(returnFocus = false) {
  const sidebar = $('sidebar');
  const toggle = $('menu-toggle');
  if (!sidebar || !toggle) return;
  sidebar.classList.remove('open');
  document.body.classList.remove('sidebar-open');
  toggle.setAttribute('aria-expanded', 'false');
  if (returnFocus) toggle.focus();
}

window.toggleSidebar = () => {
  const sidebar = $('sidebar');
  const toggle = $('menu-toggle');
  if (!sidebar || !toggle) return;
  const open = !sidebar.classList.contains('open');
  sidebar.classList.toggle('open', open);
  document.body.classList.toggle('sidebar-open', open);
  toggle.setAttribute('aria-expanded', String(open));
  if (open) sidebar.querySelector('.sidebar-close, .nav-item')?.focus();
};

// ── Status polling ────────────────────────────────────────────────────────────
async function fetchStatus() {
  try {
    const d = await api('/dashboard/api/status');
    state.status = d;
    const dot = $('status-indicator'), lbl = $('status-label');
    dot.className = `status-dot status-${d.status === 'ok' ? 'ok' : 'degraded'}`;
    lbl.textContent = d.status === 'ok' ? 'Online' : 'Degraded';
    lbl.style.color = d.status === 'ok' ? 'var(--success)' : 'var(--error)';
    $('uptime-label').textContent = `Up ${fmtUptime(d.uptime)}`;
    $('mem-label').textContent = `${d.memoryMB} MB`;
    $('sidebar-version').textContent = `v${d.version}`;
    updateHomeStatus();
  } catch {
    state.status = { status: 'unknown', uptime: 0, memoryMB: '—', version: state.status?.version || '' };
    const dot = $('status-indicator'), lbl = $('status-label');
    if (dot) dot.className = 'status-dot status-unknown';
    if (lbl) { lbl.textContent = 'Unavailable'; lbl.style.color = 'var(--text3)'; }
    updateHomeStatus();
  }
}

// ── Page: Account (accounts + quota + usage + config) ──────────────────────────

async function fetchQuota() {
  const area = $('quota-info');
  if (!area) return;
  try {
    const info = await api('/dashboard/api/quota');
    state.quotaCache = info;
    renderAccountStatStrip();
    renderQuotaTo(area, info);
  } catch (err) {
    area.innerHTML = `<div class="empty-state">Quota unavailable: ${escHtml(err.message)}</div>`;
  }
}

function renderQuotaBucket(label, b) {
  const total = b.total || 0;
  const used = b.used || 0;
  const remaining = b.remaining ?? (total - used);
  const pct = total > 0 ? (used / total) * 100 : 0;
  const color = pct > 90 ? 'var(--error)' : pct > 60 ? 'var(--warning)' : 'var(--success)';
  return `
    <div>
      <div style="display:flex; justify-content:space-between; font-size:13px; margin-bottom:4px;">
        <span>${escHtml(label)}</span>
        <span>${remaining.toFixed(0)} / ${total.toFixed(0)} ${escHtml(b.unit || 'credits')} left</span>
      </div>
      <div style="height:8px; background:var(--elevated); border-radius:4px; overflow:hidden;">
        <div style="width:${pct}%; height:100%; background:${color}; transition: width 0.3s ease;"></div>
      </div>
      <div style="font-size:11px; color:var(--text3); margin-top:3px;">${pct.toFixed(0)}% used</div>
    </div>`;
}

function renderQuotaTo(area, info) {
  const plan = info.userQuota || {};
  const addOn = info.addOnQuota || {};
  const pct = info.totalUsagePercentage;
  let html = '';
  html += `<div style="display:flex; gap:30px; flex-wrap:wrap;">`;
  html += renderQuotaBucket('Plan quota', plan);
  html += renderQuotaBucket('Add-on quota', addOn);
  html += `</div>`;
  html += `<div style="margin-top:16px; font-size:13px; color:var(--text2);">`;
  html += `Overall usage: <strong>${(pct * 100).toFixed(0)}%</strong>`;
  if (info.isQuotaExceeded) html += ` <span class="status-chip s5xx">Exceeded</span>`;
  html += ` · User type: <strong>${escHtml(info.userType)}</strong>`;
  if (info.expiresAt) {
    const d = new Date(info.expiresAt);
    html += ` · Expires: <strong>${d.toLocaleString()}</strong>`;
  }
  if (info.upgradeUrl) html += ` · <a href="${escHtml(info.upgradeUrl)}" target="_blank" rel="noopener">Upgrade</a>`;
  html += `</div>`;
  area.innerHTML = html;
}

window.refreshQuota = async () => {
  try {
    const info = await api('/dashboard/api/quota?force=true');
    state.quotaCache = info;
    renderAccountStatStrip();
    const area = $('quota-info');
    if (area) renderQuotaTo(area, info);
    showToast('Quota refreshed');
  } catch (err) {
    showToast(`Error refreshing quota: ${err.message}`, 'error');
  }
};

async function fetchUsage() {
  try {
    const data = await api('/usage/local');
    state.usageCache = data;
    renderAccountStatStrip();

    // Render Overview
    const modelEntries = Object.entries(data.requests_by_model || {});
    const tiers = data.model_tiers || {};
    let overviewHtml;
    if (modelEntries.length === 0) {
      overviewHtml = '<div class="empty-state">No requests tracked yet.</div>';
    } else {
      overviewHtml = `<div class="model-stats-grid" style="display:grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap:12px;">`;
      for (const [model, count] of modelEntries) {
        const tier = tiers[model];
        const badge = tier === 'free'
          ? '<span style="font-size:10px;background:var(--accent);color:#fff;padding:1px 6px;border-radius:4px;">FREE</span>'
          : tier === 'new'
            ? '<span style="font-size:10px;background:var(--info);color:#fff;padding:1px 6px;border-radius:4px;">NEW</span>'
            : '';
        overviewHtml += `
          <div class="card card-sm" style="background:var(--elevated); padding: 12px;">
            <div style="font-size:11px; color:var(--text3); margin-bottom:4px; text-overflow:ellipsis; overflow:hidden;">${badge} ${escHtml(model)}</div>
            <div style="font-size:18px; font-weight:600;">${count}</div>
          </div>`;
      }
      overviewHtml += `</div>`;
    }
    const overviewEl = $('usage-overview');
    if (overviewEl) overviewEl.innerHTML = overviewHtml;

    // Render Recent Records
    const records = data.recent_records || [];
    let recordsHtml = '';
    if (records.length === 0) {
      recordsHtml = '<div class="empty-state">No requests tracked yet.</div>';
    } else {
      recordsHtml = `
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Model</th>
              <th>Input Tokens</th>
              <th>Output Tokens</th>
              <th>Duration</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            ${records.map(r => `
              <tr>
                <td class="td-ts">${fmtTime(r.timestamp)}</td>
                <td><code>${escHtml(r.model)}</code></td>
                <td>${r.input_length}</td>
                <td>${r.output_length}</td>
                <td>${fmtMs(r.duration_ms)}</td>
                <td><span class="status-chip ${r.is_error ? 's5xx' : 's2xx'}">${r.is_error ? 'Error' : 'OK'}</span></td>
              </tr>
            `).join('')}
          </tbody>
        </table>
      `;
    }
    const recordsEl = $('usage-records');
    if (recordsEl) recordsEl.innerHTML = recordsHtml;

  } catch (err) {
    showToast(`Failed to load usage: ${err.message}`, 'error');
  }
}

window.resetUsage = async () => {
  if (!confirm('Are you sure you want to reset all local usage statistics?')) return;
  try {
    await api('/usage/reset-local', { method: 'POST' });
    showToast('Usage statistics reset');
    fetchUsage();
  } catch (err) {
    showToast(`Error resetting usage: ${err.message}`, 'error');
  }
};

// ── Boot ──────────────────────────────────────────────────────────────────────
async function init() {
  try {
    document.querySelectorAll('.nav-item').forEach(a => {
      a.addEventListener('click', (e) => { e.preventDefault(); navigateTo(a.dataset.page); });
    });
    const [cfg, mdl, stg] = await Promise.all([
      api('/dashboard/api/config'),
      api('/dashboard/api/models'),
      api('/dashboard/api/settings'),
    ]);
    state.config  = cfg;
    state.models  = mdl.models || [];
    state.settings = stg;
    if (state.models.length) state.chat.model = state.models[0].id;

    await fetchStatus();
    setInterval(fetchStatus, 15000);

    navigateTo(window.location.hash.slice(1) || 'endpoints');
  } catch (err) {
    $('content').innerHTML = `<div class="empty-state" style="color:var(--error)">Boot error: ${escHtml(err.message)}</div>`;
  }
}

// ── Page: Endpoints ───────────────────────────────────────────────────────────
function updateHomeStatus() {
  if (state.page !== 'endpoints') return;
  const status = state.status || {};
  const homeDot = $('home-status-dot');
  const homeLabel = $('home-status-label');
  const homeUptime = $('home-uptime');
  const homeMemory = $('home-memory');
  const tone = status.status === 'ok' ? 'ok' : status.status === 'unknown' ? 'unknown' : 'degraded';
  if (homeDot) homeDot.className = `status-dot status-${tone}`;
  if (homeLabel) homeLabel.textContent = status.status === 'ok' ? 'Proxy online' : status.status === 'unknown' ? 'Status unavailable' : 'Proxy degraded';
  if (homeUptime) homeUptime.textContent = status.status === 'unknown' ? 'Uptime unavailable' : `Up ${fmtUptime(status.uptime || 0)}`;
  if (homeMemory) homeMemory.textContent = `${status.memoryMB || '—'} MB memory`;
}

function renderEndpoints() {
  const base = state.config.publicBaseUrl || window.location.origin;
  const v1 = `${base}/v1`;
  const status = state.status || {};
  const backend = String(state.settings.backend || 'global').toLowerCase();
  const direct = Boolean(state.settings.useDirectApi);
  const hasToken = Boolean(state.settings.hasToken);
  const modelCount = state.models.length;

  const statusTone = status.status === 'ok' ? 'ok' : status.status === 'unknown' ? 'unknown' : 'degraded';
  const statusLabel = status.status === 'ok' ? 'Proxy online' : status.status === 'unknown' ? 'Status unavailable' : 'Proxy degraded';
  const endpoints = [
    { method:'GET', path:'/v1/models', protocol:'OpenAI', desc:'List configured models and aliases.', curl:`curl ${v1}/models` },
    { method:'POST', path:'/v1/chat/completions', protocol:'OpenAI', desc:'Chat completions with streaming and tools.', curl:`curl ${v1}/chat/completions \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"auto","messages":[{"role":"user","content":"Hello!"}]}'` },
    { method:'POST', path:'/v/chat', protocol:'OpenAI', desc:'Short alias for chat completions.', curl:`curl ${base}/v/chat \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"auto","messages":[{"role":"user","content":"Hello!"}]}'` },
    { method:'POST', path:'/v1/messages', protocol:'Anthropic', desc:'Messages API for Claude Code and Anthropic SDKs.', curl:`curl ${v1}/messages \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"auto","max_tokens":1024,"messages":[{"role":"user","content":"Hello!"}]}'` },
    { method:'POST', path:'/v1/message', protocol:'Anthropic', desc:'Alias for the Messages API.', curl:`curl ${v1}/message \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"auto","max_tokens":1024,"messages":[{"role":"user","content":"Hello!"}]}'` },
    { method:'POST', path:'/v1/responses', protocol:'Codex', desc:'Responses compatibility upgrade is planned.', curl:`curl ${v1}/responses \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"auto","messages":[{"role":"user","content":"Write a small Go function"}]}'`, planned:true },
    { method:'GET', path:'/health', protocol:'Core', desc:'Lightweight health probe.', curl:`curl ${base}/health` },
  ];

  const epCards = endpoints.map((ep, i) => `
    <article class="endpoint-card${ep.planned ? ' endpoint-planned' : ''}">
      <div class="ep-header">
        <span class="method-badge method-${ep.method}">${ep.method}</span>
        <code class="ep-path">${escHtml(ep.path)}</code>
        <span class="ep-protocol">${escHtml(ep.protocol)}</span>
      </div>
      <div class="ep-body">
        <p class="ep-desc">${escHtml(ep.desc)}</p>
        <div class="ep-curl"><code>${escHtml(ep.curl)}</code><button class="copy-curl" type="button" onclick="copyEndpoint(${i},this)" aria-label="Copy ${escHtml(ep.path)} curl command">Copy</button></div>
      </div>
    </article>`).join('');

  window.endpointCurlCommands = endpoints.map(ep => ep.curl);

  const overview = [
    { label:'Backend', value:backend === 'cn' ? 'China gateway' : 'Global gateway', meta:backend === 'cn' ? 'CN' : 'GLOBAL', tone:'violet' },
    { label:'Request path', value:direct ? 'Direct API' : 'CLI bridge', meta:direct ? 'DIRECT' : 'CLI', tone:direct ? 'green' : 'blue' },
    { label:'Model catalog', value:`${modelCount} model${modelCount === 1 ? '' : 's'}`, meta:modelCount ? 'READY' : 'EMPTY', tone:modelCount ? 'green' : 'amber' },
    { label:'Account', value:hasToken ? 'Connected' : 'Needs token', meta:hasToken ? 'AUTH' : 'SETUP', tone:hasToken ? 'green' : 'amber' },
  ].map(item => `
    <div class="overview-card">
      <div class="overview-card-top"><span>${item.label}</span><span class="micro-badge micro-${item.tone}">${item.meta}</span></div>
      <strong>${item.value}</strong>
    </div>`).join('');

  $('content').innerHTML = `
    <section class="home-intro" aria-labelledby="home-title">
      <div>
        <p class="eyebrow">Local AI gateway</p>
        <h1 class="page-title home-title" id="home-title">One endpoint. Every client.</h1>
        <p class="page-sub">Route OpenAI and Anthropic clients through your Qoder account.</p>
      </div>
      <a class="btn btn-ghost" href="#settings" onclick="event.preventDefault();navigateTo('settings')">Manage account</a>
    </section>

    <section class="hero-card home-hero" aria-label="Proxy base URL">
      <div class="hero-orbit" aria-hidden="true"></div>
      <div class="hero-copy">
        <div class="hero-status">
          <span id="home-status-dot" class="status-dot status-${statusTone}" aria-hidden="true"></span>
          <span id="home-status-label">${statusLabel}</span>
          <span class="hero-divider" aria-hidden="true"></span>
          <span id="home-uptime">${status.status === 'unknown' ? 'Uptime unavailable' : `Up ${fmtUptime(status.uptime || 0)}`}</span>
          <span id="home-memory">${status.memoryMB || '—'} MB memory</span>
        </div>
        <p class="hero-label">OpenAI-compatible base URL</p>
        <code class="hero-url" id="hero-url">${escHtml(v1)}</code>
        <div class="hero-actions">
          <button class="copy-btn" type="button" onclick="copyText(document.getElementById('hero-url').textContent,this)">Copy base URL</button>
          <button class="copy-btn-ghost" type="button" onclick="navigateTo('playground')">Open playground</button>
        </div>
      </div>
    </section>

    <section aria-labelledby="overview-title">
      <div class="section-heading"><div><p class="eyebrow">At a glance</p><h2 id="overview-title">Runtime overview</h2></div></div>
      <div class="overview-grid">${overview}</div>
    </section>

    <section class="home-split">
      <div class="card compatibility-panel">
        <div class="section-heading compact"><div><p class="eyebrow">Protocol coverage</p><h2>Client compatibility</h2></div></div>
        <div class="compat-list">
          <div class="compat-row"><span class="compat-mark compat-ready">O</span><div><strong>OpenAI</strong><small>Chat Completions · Models</small></div><span class="status-pill status-ready">Ready</span></div>
          <div class="compat-row"><span class="compat-mark compat-ready">A</span><div><strong>Anthropic</strong><small>Messages · Tool use · SSE</small></div><span class="status-pill status-ready">Ready</span></div>
          <div class="compat-row"><span class="compat-mark compat-planned">C</span><div><strong>Codex CLI</strong><small>Responses API</small></div><span class="status-pill status-planned">Upgrade planned</span></div>
        </div>
      </div>
      <div class="card quickstart-panel">
        <div class="section-heading compact"><div><p class="eyebrow">First request</p><h2>Quick start</h2></div></div>
        <ol class="quickstart-list">
          <li><span>1</span><div><strong>Copy the base URL</strong><small>Use the highlighted endpoint above.</small></div></li>
          <li><span>2</span><div><strong>Configure your client</strong><small>Set <code>base_url</code> to the copied value.</small></div></li>
          <li><span>3</span><div><strong>Send a request</strong><small>Start with model <code>auto</code>.</small></div></li>
        </ol>
      </div>
    </section>

    <section aria-labelledby="endpoint-title">
      <div class="section-heading"><div><p class="eyebrow">API surface</p><h2 id="endpoint-title">Endpoints</h2></div><span class="section-count">${endpoints.length} routes</span></div>
      <div class="endpoint-grid">${epCards}</div>
    </section>`;
}

window.copyEndpoint = (index, button) => copyText(window.endpointCurlCommands[index], button);

// ── Page: Playground ──────────────────────────────────────────────────────────
function renderPlayground() {
  $('content').innerHTML = `
    <div class="playground-wrap" style="height:calc(100vh - 110px)">
      <div class="page-header" style="margin-bottom:0">
        <div><h1 class="page-title">Playground</h1><p class="page-sub">Test the proxy in real-time with any model</p></div>
      </div>
      <div class="pg-toolbar">
        <div class="model-select-wrap" id="model-wrap">
          <button class="model-select-btn" id="model-btn" onclick="toggleModelDropdown()">
            <span id="model-label-display">auto</span>
            <svg class="arrow" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="6 9 12 15 18 9"/></svg>
          </button>
          <div class="model-dropdown" id="model-dropdown">
            <div id="model-list"></div>
          </div>
        </div>
        <button class="btn btn-ghost" onclick="clearChat()">Clear</button>
      </div>
      <div class="chat-area" id="chat-area"></div>
      <div class="chat-input-row">
        <textarea id="chat-input" placeholder="Type a message…" rows="1" onkeydown="chatKeydown(event)"></textarea>
        <button class="send-btn" id="send-btn" onclick="sendMessage()">Send</button>
      </div>
    </div>`;
  buildModelList(state.models);
  renderMessages();
}

function buildModelList(models) {
  const list = $('model-list');
  if (!list) return;
  list.innerHTML = models.map(m => `
    <div class="model-option" onclick="selectModel('${m.id}')">
      <div class="model-name">${escHtml(m.label)}</div>
      <div class="model-desc">${escHtml(m.id)}</div>
    </div>`).join('');
}

window.toggleModelDropdown = () => $('model-dropdown').classList.toggle('open');
window.selectModel = (id) => {
  state.chat.model = id;
  $('model-label-display').textContent = id;
  $('model-dropdown').classList.remove('open');
};

function messageBubbleHtml(m, isStreamingTarget) {
  let contentHtml = m.role === 'assistant' ? mdToHtml(m.content) : escHtml(m.content);
  if (m.role === 'assistant' && isStreamingTarget && m.content === '') {
    contentHtml = `<div class="typing-indicator"><div class="typing-dot"></div><div class="typing-dot"></div><div class="typing-dot"></div></div>`;
  }
  return contentHtml;
}

function renderMessages() {
  const area = $('chat-area');
  if (!area) return;
  if (state.chat.messages.length === 0) {
    area.innerHTML = '<div class="chat-empty"><p>Select a model and start a conversation.</p></div>';
    return;
  }
  const lastIdx = state.chat.messages.length - 1;
  area.innerHTML = state.chat.messages.map((m, i) => {
    const contentHtml = messageBubbleHtml(m, state.chat.streaming && i === lastIdx);
    const bubbleStyle = contentHtml.includes('typing-indicator') ? 'background:transparent;border:none;padding:0' : '';
    return `
    <div class="msg msg-${m.role}" data-msg-index="${i}">
      <div class="msg-bubble" style="${bubbleStyle}">${contentHtml}</div>
    </div>`;
  }).join('');
  area.scrollTop = area.scrollHeight;
}

// Updates only the last message bubble's content in place instead of
// rebuilding the whole chat-area innerHTML — called once per SSE token, so a
// full rebuild here was the cause of the visible flicker during streaming.
function updateLastMessageBubble() {
  const area = $('chat-area');
  if (!area) return;
  const lastIdx = state.chat.messages.length - 1;
  const bubble = area.querySelector(`[data-msg-index="${lastIdx}"] .msg-bubble`);
  if (!bubble) { renderMessages(); return; }
  const m = state.chat.messages[lastIdx];
  const contentHtml = messageBubbleHtml(m, state.chat.streaming);
  bubble.innerHTML = contentHtml;
  bubble.style.cssText = contentHtml.includes('typing-indicator') ? 'background:transparent;border:none;padding:0' : '';
  area.scrollTop = area.scrollHeight;
}

window.clearChat = () => { state.chat.messages = []; renderMessages(); };
window.chatKeydown = (e) => { if (e.key==='Enter' && !e.shiftKey) { e.preventDefault(); sendMessage(); } };

window.sendMessage = async () => {
  const inp = $('chat-input');
  const text = inp.value.trim();
  if (!text || state.chat.streaming) return;
  inp.value = '';

  state.chat.messages.push({ role: 'user', content: text });
  const assistant = { role: 'assistant', content: '' };
  state.chat.messages.push(assistant);
  state.chat.streaming = true;
  renderMessages();

  try {
    const res = await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ stream: true, messages: state.chat.messages.slice(0, -1), model: state.chat.model }),
    });

    const reader = res.body.getReader(), dec = new TextDecoder();
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      const lines = dec.decode(value).split('\n');
      for (const line of lines) {
        if (line.startsWith('data: ') && line !== 'data: [DONE]') {
          try {
            const chunk = JSON.parse(line.slice(6));
            const delta = chunk.choices?.[0]?.delta?.content || '';
            assistant.content += delta;
            updateLastMessageBubble();
          } catch (e) {}
        }
      }
    }
  } catch (err) { assistant.content = `Error: ${err.message}`; renderMessages(); }
  state.chat.streaming = false;
};

// ── Page: Settings (Dynamic UI) ──────────────────────────────────────────────
function renderSettings() {
  $('content').innerHTML = `
    <div class="page-header"><div><h1 class="page-title">Account</h1><p class="page-sub">Accounts, quota, usage and configuration</p></div></div>

    <div class="stat-strip" id="account-stat-strip"></div>

    <div class="card">
      <div class="card-title">Accounts</div>
      <div class="table-wrap" id="accounts-list">Loading...</div>
      <div class="add-model-row" style="margin-top:12px;">
        <input type="text" id="new-acc-name" placeholder="Account name (e.g. work)">
        <select id="new-acc-backend">
          <option value="global">Qoder International</option>
          <option value="cn">Qoder CN</option>
        </select>
        <button class="btn btn-sm" onclick="addAccount()">+ Add Account</button>
      </div>
    </div>

    <div class="card">
      <div class="card-title">Quota & Credits</div>
      <div id="quota-info">Loading...</div>
      <button class="btn btn-ghost" style="margin-top:12px;" onclick="refreshQuota()">Refresh</button>
    </div>

    <div class="card">
      <div class="card-title">Usage by Model</div>
      <div id="usage-overview">Loading...</div>
      <button class="btn btn-danger" style="margin-top: 15px;" onclick="resetUsage()">Reset Statistics</button>
    </div>

    <div class="card">
      <div class="card-title">Recent Requests</div>
      <div class="table-wrap" id="usage-records">Loading...</div>
    </div>

    <div class="card">
      <div class="card-title">Core Configuration</div>
      <div class="settings-form">
        <div class="field">
          <label>Backend Type</label>
          <select id="set-backend" class="select-full">
            <option value="global" ${state.settings.backend === 'global' ? 'selected' : ''}>Qoder International (qodercli)</option>
            <option value="cn" ${state.settings.backend === 'cn' ? 'selected' : ''}>Qoder CN (qoderclicn)</option>
          </select>
        </div>
        <div class="field">
          <label>Personal Access Token</label>
          <input type="password" id="set-token" autocomplete="new-password" placeholder="${state.settings.token || 'Enter new token...'}" class="input-full">
          <p class="field-help">Changes take effect immediately for new requests.</p>
        </div>
        <div class="field">
          <label style="display: flex; align-items: center; gap: 8px; cursor: pointer; color: var(--text2); font-size: 13px;">
            <input type="checkbox" id="set-direct" ${state.settings.useDirectApi ? 'checked' : ''} style="width: auto;">
            Use Direct API (Bypass CLI, required for Claude Code local tools)
          </label>
        </div>
        <div class="field" style="margin-top: 10px;">
          <label>Proxy URL (Optional, for Direct API)</label>
          <input type="text" id="set-proxyUrl" placeholder="http://127.0.0.1:7890" value="${escHtml(state.settings.proxyUrl || '')}">
          <div style="font-size: 11px; color: var(--text3); margin-top: 4px;">Use this to resolve TLS Handshake Timeouts in Direct Mode if your environment requires a specific proxy.</div>
        </div>
        <button class="btn btn-primary" onclick="saveSettings()">Save Configuration</button>

      </div>
    </div>

    <div class="card">
      <div class="card-title">OAuth Login (Device Code)</div>
      <div class="settings-form">
        <div id="oauth-status-line" class="field-help">Checking OAuth status...</div>
        <div style="display:flex; gap:8px; margin-top:8px;">
          <button class="btn btn-primary" id="oauth-login-btn" onclick="startOAuthLogin()">Login via OAuth</button>
          <button class="btn btn-sm btn-danger" id="oauth-logout-btn" onclick="oauthLogout()" style="display:none;">Logout</button>
        </div>
        <div id="oauth-login-hint" class="field-help" style="display:none; margin-top:8px;"></div>
      </div>
    </div>

    <div class="card">
      <div class="card-title">Custom Models</div>
      <div class="models-table-wrap">
        <table class="models-table">
          <thead><tr><th>ID</th><th>Label</th><th>Tier</th><th>Actions</th></tr></thead>
          <tbody id="models-tbody"></tbody>
        </table>
      </div>
      <div class="add-model-row">
        <input type="text" id="new-m-id" placeholder="id (e.g. qmodel_latest)">
        <input type="text" id="new-m-label" placeholder="Label">
        <select id="new-m-tier">
          <option value="new">New</option>
          <option value="paid">Paid</option>
          <option value="free">Free</option>
        </select>
        <button class="btn btn-sm" onclick="addModel()">Add Model</button>
        <button class="btn btn-sm" style="margin-left:8px;background:var(--accent);color:#fff" onclick="refreshModels()">Auto Fetch Latest</button>
      </div>
    </div>`;
  renderModelsTable();
  renderAccountStatStrip();
  refreshOAuthStatus();
  refreshAccounts();
  fetchUsage();
  fetchQuota();
}

function renderAccountStatStrip() {
  const strip = $('account-stat-strip');
  if (!strip) return;
  const active = (state.accountsCache || []).find(a => a.active);
  strip.innerHTML = `
    <div class="stat-chip"><span class="stat-chip-value">${escHtml(active ? active.name : '—')}</span><span class="stat-chip-label">Active Account</span></div>
    <div class="stat-chip"><span class="stat-chip-value">${state.usageCache ? state.usageCache.total_requests : '—'}</span><span class="stat-chip-label">Total Requests</span></div>
    <div class="stat-chip"><span class="stat-chip-value" style="color:${state.usageCache && state.usageCache.total_errors ? 'var(--error)' : 'var(--text1)'}">${state.usageCache ? state.usageCache.total_errors : '—'}</span><span class="stat-chip-label">Total Errors</span></div>
    <div class="stat-chip"><span class="stat-chip-value" style="color:var(--info)">${state.quotaCache ? (state.quotaCache.totalUsagePercentage * 100).toFixed(0) + '%' : '—'}</span><span class="stat-chip-label">Quota Used</span></div>`;
}

async function refreshAccounts() {
  const area = $('accounts-list');
  if (!area) return;
  try {
    const { accounts } = await api('/dashboard/api/accounts');
    state.accountsCache = accounts || [];
    renderAccountStatStrip();
    if (!accounts || accounts.length === 0) {
      area.innerHTML = '<div class="empty-state">No accounts.</div>';
      return;
    }
    area.innerHTML = `
      <table>
        <thead><tr><th>Name</th><th>Backend</th><th>Token</th><th>Status</th><th>Actions</th></tr></thead>
        <tbody>
          ${accounts.map(a => `
            <tr>
              <td>${escHtml(a.name)}</td>
              <td><span class="tier-badge">${escHtml(a.backend)}</span></td>
              <td><code>${a.hasToken ? escHtml(a.maskedToken) : '—'}</code></td>
              <td>${a.active ? '<span class="status-chip s2xx">Active</span>' : ''}</td>
              <td>
                ${a.active ? '' : `<button class="btn-text" onclick="activateAccount('${a.id}')">Set Active</button>`}
                <button class="btn-text btn-danger" onclick="deleteAccount('${a.id}')">Delete</button>
              </td>
            </tr>`).join('')}
        </tbody>
      </table>`;
  } catch (err) {
    area.innerHTML = `<div class="empty-state" style="color:var(--error)">Failed to load accounts: ${escHtml(err.message)}</div>`;
  }
}

window.addAccount = async () => {
  const name = $('new-acc-name').value.trim();
  const backend = $('new-acc-backend').value;
  if (!name) return showToast('Account name required', 'error');
  try {
    await api('/dashboard/api/accounts', { method: 'POST', body: JSON.stringify({ name, backend }) });
    showToast('Account added and set active');
    $('new-acc-name').value = '';
    const stg = await api('/dashboard/api/settings');
    state.settings = stg;
    renderModelsTable();
    await refreshAccounts();
    await refreshOAuthStatus();
  } catch (err) {
    showToast(`Error adding account: ${err.message}`, 'error');
  }
};

window.activateAccount = async (id) => {
  try {
    await api(`/dashboard/api/accounts/${id}/activate`, { method: 'POST' });
    showToast('Active account switched');
    const stg = await api('/dashboard/api/settings');
    state.settings = stg;
    renderModelsTable();
    await refreshAccounts();
    await refreshOAuthStatus();
  } catch (err) {
    showToast(`Error switching account: ${err.message}`, 'error');
  }
};

window.deleteAccount = async (id) => {
  if (!confirm('Delete this account?')) return;
  try {
    await api(`/dashboard/api/accounts/${id}`, { method: 'DELETE' });
    showToast('Account deleted');
    const stg = await api('/dashboard/api/settings');
    state.settings = stg;
    renderModelsTable();
    await refreshAccounts();
    await refreshOAuthStatus();
  } catch (err) {
    showToast(`Error deleting account: ${err.message}`, 'error');
  }
};

async function refreshOAuthStatus() {
  const line = $('oauth-status-line');
  if (!line) return;
  try {
    const st = await api('/dashboard/api/oauth/status');
    if (st.loggedIn) {
      line.textContent = `Logged in via ${st.tokenType === 'device_token' ? 'OAuth (device token)' : st.tokenType}` +
        (st.userID ? ` — user: ${st.userID}` : '') +
        (st.expireTime ? ` — expires: ${new Date(st.expireTime * 1000).toLocaleString()}` : '');
      $('oauth-logout-btn').style.display = '';
    } else {
      line.textContent = 'Not logged in.';
      $('oauth-logout-btn').style.display = 'none';
    }
  } catch (err) {
    line.textContent = `Could not load OAuth status: ${err.message}`;
  }
}

window.startOAuthLogin = async () => {
  const btn = $('oauth-login-btn');
  const hint = $('oauth-login-hint');
  btn.disabled = true;
  hint.style.display = '';
  hint.textContent = 'Creating login session...';
  try {
    const { session_id, auth_url } = await api('/oauth/login', { method: 'POST' });
    hint.innerHTML = `Open this URL to authorize: <a href="${escHtml(auth_url)}" target="_blank" rel="noopener">${escHtml(auth_url)}</a><br>Waiting for approval...`;
    window.open(auth_url, '_blank');

    const result = await api(`/oauth/session/${session_id}`);
    if (result.error) {
      hint.textContent = `OAuth login failed: ${result.error}`;
      showToast(`OAuth login failed: ${result.error}`, 'error');
    } else {
      hint.textContent = 'OAuth login succeeded!';
      showToast('OAuth login succeeded');
      const stg = await api('/dashboard/api/settings');
      state.settings = stg;
      await refreshOAuthStatus();
    }
  } catch (err) {
    hint.textContent = `Error: ${err.message}`;
    showToast(`OAuth login error: ${err.message}`, 'error');
  } finally {
    btn.disabled = false;
  }
};

window.oauthLogout = async () => {
  try {
    await api('/oauth/logout', { method: 'DELETE' });
    showToast('OAuth token cleared');
    const stg = await api('/dashboard/api/settings');
    state.settings = stg;
    await refreshOAuthStatus();
  } catch (err) {
    showToast(`Error logging out: ${err.message}`, 'error');
  }
};

function renderModelsTable() {
  const tbody = $('models-tbody');
  tbody.innerHTML = state.settings.models.map((m, i) => `
    <tr>
      <td><code>${escHtml(m.id)}</code></td>
      <td>${escHtml(m.label)}</td>
      <td><span class="tier-badge tier-${m.tier}">${m.tier}</span></td>
      <td><button class="btn-text btn-danger" onclick="deleteModel(${i})">Delete</button></td>
    </tr>`).join('');
}

window.addModel = () => {
  const id = $('new-m-id').value.trim(), label = $('new-m-label').value.trim(), tier = $('new-m-tier').value;
  if (!id || !label) return showToast('ID and Label required', 'error');
  state.settings.models.push({ id, label, tier, description: `${label} (custom)` });
  renderModelsTable();
  $('new-m-id').value = ''; $('new-m-label').value = '';
};

window.refreshModels = async () => {
  try {
    const res = await api('/dashboard/api/models/refresh', { method: 'POST' });
    if (res.models && res.models.length > 0) {
      state.settings.models = res.models;
      state.models = res.models;
      renderModelsTable();
      showToast('Models refreshed successfully');
    } else {
      showToast('No models found', 'error');
    }
  } catch (err) {
    showToast(`Error refreshing models: ${err.message}`, 'error');
  }
};

window.deleteModel = (i) => {
  state.settings.models.splice(i, 1);
  renderModelsTable();
};

window.saveSettings = async () => {
  const backend = $('set-backend').value;
  const token = $('set-token').value;
  const useDirectApi = $('set-direct').checked;
  const proxyUrl = $('set-proxyUrl').value;

  try {
    await api('/dashboard/api/settings', {
      method: 'POST',
      body: JSON.stringify({ backend, token, useDirectApi, proxyUrl, models: state.settings.models })
    });
    showToast('Settings saved successfully');
    // Update local state to keep UI in sync
    state.settings.backend = backend;
    state.settings.useDirectApi = useDirectApi;
    state.settings.proxyUrl = proxyUrl;
    if (token && (token !== '******' && !token.includes('...'))) {
        state.settings.token = '******';
        state.settings.hasToken = true;
    }

    // Refresh global model list
    const mdl = await api('/dashboard/api/models');

    state.models = mdl.models || [];
    state.settings.token = token ? '******' : state.settings.token;
  } catch (err) { showToast(`Error saving settings: ${err.message}`, 'error'); }
};

// ── Page: Logs & System Logs ──────────────────────────────────────────────────
function renderLogs() {
  $('content').innerHTML = `
    <div class="page-header">
      <div><h1 class="page-title">Request Logs</h1><p class="page-sub">Recent traffic through the proxy</p></div>
      <div class="logs-toolbar-right">
        <button id="ar-req-btn" class="auto-refresh-toggle ${state.logs.autoRefresh ? 'on' : ''}" onclick="toggleAutoRefresh('req')">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>
          Auto Refresh
        </button>
        <button class="btn btn-ghost btn-sm" onclick="fetchLogs()">Refresh</button>
        <button class="btn btn-danger btn-sm" onclick="clearLogs()">Clear</button>
      </div>
    </div>
    <div class="stat-strip" id="log-stat-strip"></div>
    <div class="logs-toolbar">
      <input type="text" id="log-filter" class="filter-input" placeholder="Filter by path…" value="${escHtml(state.logs.filter)}" oninput="onLogFilterInput(this.value)">
    </div>
    <div id="log-list" class="table-wrap">Loading...</div>`;
  fetchLogs();
  if (state.logs.autoRefresh && !state.logs.timer) {
    state.logs.timer = setInterval(fetchLogs, 3000);
  }
}

function renderLogStatStrip(entries) {
  const strip = $('log-stat-strip');
  if (!strip) return;
  const total = entries.length;
  const errors = entries.filter(l => l.statusCode >= 400).length;
  const sse = entries.filter(l => l.is_sse).length;
  strip.innerHTML = `
    <div class="stat-chip"><span class="stat-chip-value">${total}</span><span class="stat-chip-label">Total</span></div>
    <div class="stat-chip"><span class="stat-chip-value" style="color:${errors ? 'var(--error)' : 'var(--text1)'}">${errors}</span><span class="stat-chip-label">Errors</span></div>
    <div class="stat-chip"><span class="stat-chip-value" style="color:var(--info)">${sse}</span><span class="stat-chip-label">Streamed</span></div>`;
}

window.onLogFilterInput = (v) => {
  state.logs.filter = v;
  renderLogTable();
};

function renderLogTable() {
  const area = $('log-list');
  if (!area) return;
  const filter = state.logs.filter.trim().toLowerCase();
  const rows = filter ? state.logs.entries.filter(l => l.path.toLowerCase().includes(filter)) : state.logs.entries;

  if (state.logs.entries.length === 0) {
    area.innerHTML = '<div class="empty-state">No logs yet.</div>';
    return;
  }
  if (rows.length === 0) {
    area.innerHTML = '<div class="empty-state">No logs match this filter.</div>';
    return;
  }

  area.innerHTML = `
    <table>
      <thead>
        <tr>
          <th>Time</th>
          <th>Method</th>
          <th>Path</th>
          <th>Status</th>
          <th>SSE</th>
        </tr>
      </thead>
      <tbody>
        ${rows.map(l => `
          <tr onclick="showLogDetail('${l.id}')">
            <td class="td-ts">${fmtTime(l.timestamp)}</td>
            <td><span class="method-badge method-${l.method}">${l.method}</span></td>
            <td class="td-path">${escHtml(l.path)}</td>
            <td><span class="status-chip ${l.statusCode < 400 ? 's2xx' : l.statusCode < 500 ? 's4xx' : 's5xx'}">${l.statusCode}</span></td>
            <td>${l.is_sse ? '<span class="stream-chip">SSE</span>' : '—'}</td>
          </tr>
        `).join('')}
      </tbody>
    </table>`;
}

async function fetchLogs() {
  try {
    const d = await api('/dashboard/api/logs');
    state.logs.entries = d.logs || [];
    renderLogStatStrip(state.logs.entries);
    renderLogTable();
  } catch (err) { showToast('Failed to fetch logs', 'error'); }
}

window.showLogDetail = async (id) => {
  const modal = $('log-modal');
  const body = $('log-modal-body');
  state.modalLastFocus = document.activeElement;
  modal.classList.add('open');
  modal.setAttribute('aria-hidden', 'false');
  body.innerHTML = '<div class="spinner"></div>';
  requestAnimationFrame(() => modal.querySelector('.modal-close').focus());

  try {
    const data = await api(`/dashboard/api/logs/${id}`);

    // Build token metrics section if any token data exists
    let metricsHTML = '';
    if (data.input_tokens || data.output_tokens || data.thinking_tokens || data.cache_creation_tokens || data.cache_read_tokens) {
      metricsHTML = `
        <div class="token-metrics">
          ${data.input_tokens ? `<div class="token-metric"><div class="token-metric-value">${data.input_tokens}</div><div class="token-metric-label">Input</div></div>` : ''}
          ${data.output_tokens ? `<div class="token-metric"><div class="token-metric-value">${data.output_tokens}</div><div class="token-metric-label">Output</div></div>` : ''}
          ${data.thinking_tokens ? `<div class="token-metric"><div class="token-metric-value">${data.thinking_tokens}</div><div class="token-metric-label">Thinking</div></div>` : ''}
          ${data.cache_creation_tokens ? `<div class="token-metric"><div class="token-metric-value">${data.cache_creation_tokens}</div><div class="token-metric-label">Cache Create</div></div>` : ''}
          ${data.cache_read_tokens ? `<div class="token-metric"><div class="token-metric-value">${data.cache_read_tokens}</div><div class="token-metric-label">Cache Read</div></div>` : ''}
        </div>
      `;
    }

    // Build stream raw lines section if available
    let streamRawHTML = '';
    if (data.is_sse && data.stream_raw_lines) {
      try {
        const rawLines = JSON.parse(data.stream_raw_lines);
        if (rawLines && rawLines.length > 0) {
          streamRawHTML = `
            <div>
              <div class="log-detail-label">Stream Raw Lines (${rawLines.length} lines)</div>
              <div class="stream-raw-block">${rawLines.join('\n')}</div>
            </div>
          `;
        }
      } catch (e) {}
    }

    body.innerHTML = `
      ${metricsHTML}
      <div class="log-detail-grid">
        <div>
          <div class="log-detail-label">Request Body</div>
          <div class="json-block">${syntaxJson(data.body)}</div>
        </div>
        <div>
          <div class="log-detail-label">Response Data</div>
          <div class="json-block">${syntaxJson(data.response_body)}</div>
        </div>
        ${streamRawHTML}
      </div>
      <div style="margin-top:20px;">
        <div class="log-detail-label">Full Metadata</div>
        <div class="json-block">${syntaxJson({
          id: data.id,
          timestamp: data.timestamp,
          method: data.method,
          path: data.path,
          statusCode: data.statusCode,
          isSSE: data.is_sse
        })}</div>
      </div>
    `;
  } catch (err) { body.innerHTML = `<div class="empty-state" style="color:var(--error)">Failed to load detail: ${err.message}</div>`; }
};

window.closeLogModal = () => {
  const modal = $('log-modal');
  modal.classList.remove('open');
  modal.setAttribute('aria-hidden', 'true');
  if (state.modalLastFocus && document.contains(state.modalLastFocus)) state.modalLastFocus.focus();
};

window.handleModalOverlayClick = (event) => {
  if (event.target === $('log-modal')) closeLogModal();
};

document.addEventListener('keydown', (event) => {
  const modal = $('log-modal');
  if (event.key === 'Escape') {
    if (modal.classList.contains('open')) closeLogModal();
    else closeSidebar(true);
    return;
  }
  if (event.key !== 'Tab' || !modal.classList.contains('open')) return;
  const focusable = [...modal.querySelectorAll('button,[href],input,select,textarea,[tabindex]:not([tabindex="-1"])')]
    .filter(node => !node.disabled && node.offsetParent !== null);
  if (!focusable.length) return;
  const first = focusable[0], last = focusable[focusable.length - 1];
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
});

window.clearLogs = async () => {
  if (!confirm('Clear all request logs?')) return;
  await api('/dashboard/api/logs', { method: 'DELETE' });
  fetchLogs();
};

window.toggleAutoRefresh = (type) => {
  if (type === 'req') {
    state.logs.autoRefresh = !state.logs.autoRefresh;
    $('ar-req-btn').classList.toggle('on', state.logs.autoRefresh);
    if (state.logs.autoRefresh) state.logs.timer = setInterval(fetchLogs, 3000);
    else clearInterval(state.logs.timer);
  } else {
    state.sysLogs.autoRefresh = !state.sysLogs.autoRefresh;
    $('ar-sys-btn').classList.toggle('on', state.sysLogs.autoRefresh);
    if (state.sysLogs.autoRefresh) state.sysLogs.timer = setInterval(fetchSystemLogs, 3000);
    else clearInterval(state.sysLogs.timer);
  }
};

function renderSystemLogs() {
  $('content').innerHTML = `
    <div class="page-header">
      <div><h1 class="page-title">System Logs</h1><p class="page-sub">Runtime events and process output</p></div>
      <div class="logs-toolbar-right">
        <button id="ar-sys-btn" class="auto-refresh-toggle ${state.sysLogs.autoRefresh ? 'on' : ''}" onclick="toggleAutoRefresh('sys')">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>
          Auto Refresh
        </button>
        <button class="btn btn-ghost btn-sm" onclick="fetchSystemLogs()">Refresh</button>
        <button class="btn btn-danger btn-sm" onclick="clearSystemLogs()">Clear</button>
      </div>
    </div>
    <div class="logs-toolbar">
      <div class="level-tabs" id="level-tabs">
        ${['all', 'error', 'warn', 'info', 'debug'].map(lv => `
          <button class="level-tab ${state.sysLogs.level === lv ? 'active' : ''}" data-level="${lv}" onclick="onLevelTabClick('${lv}')">${lv}</button>
        `).join('')}
      </div>
      <input type="text" id="sys-filter" class="filter-input" placeholder="Filter by message or source…" value="${escHtml(state.sysLogs.filter)}" oninput="onSysFilterInput(this.value)">
    </div>
    <div id="sys-list" class="terminal">Loading...</div>`;
  fetchSystemLogs();
  if (state.sysLogs.autoRefresh && !state.sysLogs.timer) {
    state.sysLogs.timer = setInterval(fetchSystemLogs, 3000);
  }
}

window.onLevelTabClick = (lv) => {
  state.sysLogs.level = lv;
  $('level-tabs').querySelectorAll('.level-tab').forEach(b => b.classList.toggle('active', b.dataset.level === lv));
  renderSystemLogTerminal();
};

window.onSysFilterInput = (v) => {
  state.sysLogs.filter = v;
  renderSystemLogTerminal();
};

function renderSystemLogTerminal() {
  const area = $('sys-list');
  if (!area) return;
  const filter = state.sysLogs.filter.trim().toLowerCase();
  const level = state.sysLogs.level;

  let rows = state.sysLogs.entries;
  if (level !== 'all') rows = rows.filter(l => l.level === level);
  if (filter) rows = rows.filter(l => l.message.toLowerCase().includes(filter) || (l.source || '').toLowerCase().includes(filter));

  if (state.sysLogs.entries.length === 0) {
    area.innerHTML = '<div class="empty-state">No system logs yet.</div>';
    return;
  }
  if (rows.length === 0) {
    area.innerHTML = '<div class="empty-state">No logs match this filter.</div>';
    return;
  }

  area.innerHTML = rows.map(l => `
    <div class="log-line level-${l.level}">
      <span class="log-ts">[${fmtTime(l.timestamp)}]</span>
      <span class="log-src src-${l.source || 'system'}">${l.source || 'SYS'}</span>
      <span class="log-msg">${escHtml(l.message)}</span>
    </div>`).reverse().join('');
}

async function fetchSystemLogs() {
  try {
    const d = await api('/dashboard/api/logs/system');
    state.sysLogs.entries = d.logs || [];
    renderSystemLogTerminal();
  } catch (err) { showToast('Failed to fetch system logs', 'error'); }
}

init();
window.clearSystemLogs = async () => {
  if (!confirm('Clear all system logs?')) return;
  await api('/dashboard/api/logs/system', { method: 'DELETE' });
  fetchSystemLogs();
};
