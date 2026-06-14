/* global app.js — vanilla JS SPA */
'use strict';

// ── State ────────────────────────────────────────────────────────────────────
const state = {
  page: 'endpoints',
  config: {},
  settings: { backend: 'global', token: '', hasToken: false, models: [] },
  models: [],
  chat: { messages: [], model: 'auto', streaming: false, streamMode: 'stream' },
  logs: { entries: [], filter: '', autoRefresh: false, timer: null, expanded: null },
  sysLogs: { entries: [], autoRefresh: false, timer: null },
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

function copyText(text, btn) {
  navigator.clipboard.writeText(text).then(() => {
    const orig = btn.textContent;
    btn.textContent = '✓ Copied'; btn.classList.add('copied');
    setTimeout(() => { btn.textContent = orig; btn.classList.remove('copied'); }, 1800);
  });
}

function showToast(msg, type = 'success') {
  const t = el('div', `toast toast-${type}`, msg);
  document.body.appendChild(t);
  setTimeout(() => t.classList.add('show'), 10);
  setTimeout(() => { t.classList.remove('show'); setTimeout(() => t.remove(), 400); }, 3000);
}

// ── Routing ──────────────────────────────────────────────────────────────────
const routes = {
  endpoints: renderEndpoints,
  playground: renderPlayground,
  logs: renderLogs,
  'system-logs': renderSystemLogs,
  settings: renderSettings,
  usage: renderUsage
};

function navigateTo(page) {
  if (!routes[page]) page = 'endpoints';
  clearInterval(state.logs.timer); state.logs.autoRefresh = false;
  clearInterval(state.sysLogs.timer); state.sysLogs.autoRefresh = false;
  state.page = page;
  window.location.hash = page;
  updateSidebar();
  routes[page]();
}

window.addEventListener('hashchange', () => navigateTo(window.location.hash.slice(1)));

function updateSidebar() {
  document.querySelectorAll('.nav-item').forEach(a => a.classList.toggle('active', a.dataset.page === state.page));
}

// ── Status polling ────────────────────────────────────────────────────────────
async function fetchStatus() {
  try {
    const d = await api('/dashboard/api/status');
    const dot = $('status-indicator'), lbl = $('status-label');
    dot.className = `status-dot status-${d.status === 'ok' ? 'ok' : 'degraded'}`;
    lbl.textContent = d.status === 'ok' ? 'Online' : 'Degraded';
    lbl.style.color = d.status === 'ok' ? '#34d399' : '#f87171';
    $('qodercli-ver').textContent = `qodercli ${d.qodercli || '1.x'}`;
    $('uptime-label').textContent = `Up ${fmtUptime(d.uptime)}`;
    $('mem-label').textContent = `${d.memoryMB} MB`;
    $('sidebar-version').textContent = `v${d.version}`;
  } catch { /* ignore */ }
}

// ── Page: Usage ──────────────────────────────────────────────────────────────
function renderUsage() {
  $('content').innerHTML = `
    <div class="page-header"><div><h1 class="page-title">Usage & Credits</h1><p class="page-sub">Local usage tracking for proxy requests</p></div></div>

    <div class="card">
      <div class="card-title">Overview</div>
      <div id="usage-overview">Loading...</div>
      <button class="btn btn-danger" style="margin-top: 15px;" onclick="resetUsage()">Reset Statistics</button>
    </div>

    <div class="card" style="margin-top: 20px;">
      <div class="card-title">Recent Requests</div>
      <div class="table-wrap" id="usage-records">Loading...</div>
    </div>`;

  fetchUsage();
}

async function fetchUsage() {
  try {
    const data = await api('/usage/local');

    // Render Overview
    let overviewHtml = `
      <div style="display: flex; gap: 40px; margin-bottom: 20px;">
        <div class="stat-item">
          <div class="stat-label" style="font-size:12px; color:var(--text3); text-transform:uppercase;">Total Requests</div>
          <div class="stat-value" style="font-size:24px; font-weight:700;">${data.total_requests}</div>
        </div>
        <div class="stat-item">
          <div class="stat-label" style="font-size:12px; color:var(--text3); text-transform:uppercase;">Total Errors</div>
          <div class="stat-value" style="font-size:24px; font-weight:700; color:#f87171">${data.total_errors}</div>
        </div>
      </div>
      <h4 style="margin-bottom:12px; font-size:14px; color:var(--text2)">Requests by Model</h4>
      <div class="model-stats-grid" style="display:grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap:12px;">
    `;
    for (const [model, count] of Object.entries(data.requests_by_model || {})) {
      overviewHtml += `
        <div class="card card-sm" style="background:var(--elevated); padding: 12px;">
          <div style="font-size:11px; color:var(--text3); margin-bottom:4px; text-overflow:ellipsis; overflow:hidden;">${escHtml(model)}</div>
          <div style="font-size:18px; font-weight:600;">${count}</div>
        </div>`;
    }
    overviewHtml += `</div>`;
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
              <th>Input Chars</th>
              <th>Output Chars</th>
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
    $('content').innerHTML = `<div class="empty-state" style="color:#f87171">Boot error: ${escHtml(err.message)}</div>`;
  }
}

// ── Page: Endpoints ───────────────────────────────────────────────────────────
function renderEndpoints() {
  const base = state.config.publicBaseUrl || window.location.origin;
  const v1   = `${base}/v1`;
  const key  = state.config.proxyApiKey;

  const endpoints = [
    { method:'GET',  path:'/v1/models',            desc:'List all available models and aliases.',        curl:`curl ${v1}/models${key ? ` \\\n  -H "Authorization: Bearer ${key}"` : ''}` },
    { method:'POST', path:'/v1/chat/completions',  desc:'OpenAI-compatible chat completions (streaming supported).', curl:`curl ${v1}/chat/completions \\\n  -H "Content-Type: application/json"${key ? ` \\\n  -H "Authorization: Bearer ${key}"` : ''} \\\n  -d '{"model":"auto","messages":[{"role":"user","content":"Hello!"}]}'` },
    { method:'POST', path:'/v/chat',               desc:'Alias for chat completions.',                   curl:`curl ${base}/v/chat \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"auto","messages":[{"role":"user","content":"Hi!"}]}'` },
    { method:'POST', path:'/v1/messages',          desc:'Anthropic-compatible messages endpoint.',       curl:`curl ${v1}/messages \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"claude-3-5-sonnet-20240620","messages":[{"role":"user","content":"Hello!"}]}'` },
    { method:'POST', path:'/responses',            desc:'Codex-style completion endpoint.',              curl:`curl ${base}/responses \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"auto","messages":[{"role":"user","content":"Write code"}]}'` },
    { method:'POST', path:'/v1/responses',         desc:'Codex-style endpoint alias.',                   curl:`curl ${v1}/responses \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"auto","messages":[{"role":"user","content":"Code"}]}'` },
    { method:'GET',  path:'/health',               desc:'Health check — returns server status.',         curl:`curl ${base}/health` },
  ];

  const epCards = endpoints.map((ep, i) => `
    <div class="endpoint-card">
      <div class="ep-header">
        <span class="method-badge method-${ep.method}">${ep.method}</span>
        <span class="ep-path">${escHtml(ep.path)}</span>
      </div>
      <div class="ep-body">
        <p class="ep-desc">${escHtml(ep.desc)}</p>
        <div class="ep-curl" id="curl-${i}">${escHtml(ep.curl)}<button class="copy-curl" onclick="copyText(document.getElementById('curl-${i}').innerText,this)">Copy</button></div>
      </div>
    </div>`).join('');

  $('content').innerHTML = `
    <div class="page-header"><div><h1 class="page-title">Endpoints</h1><p class="page-sub">Your multi-protocol AI proxy — Drop this URL into any application</p></div></div>
    <div class="hero-card">
      <div class="hero-label">🌐 Base URL (OpenAI-compatible)</div>
      <div class="hero-url" id="hero-url">${escHtml(v1)}</div>
      <div class="hero-actions">
        <button class="copy-btn" id="copy-url-btn" onclick="copyText('${v1}',this)">Copy Base URL</button>
      </div>
    </div>
    <div class="endpoint-grid">${epCards}</div>`;
}

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

function renderMessages() {
  const area = $('chat-area');
  if (!area) return;
  if (state.chat.messages.length === 0) {
    area.innerHTML = '<div class="chat-empty"><p>Select a model and start a conversation.</p></div>';
    return;
  }
  area.innerHTML = state.chat.messages.map(m => `
    <div class="msg msg-${m.role}">
      <div class="msg-bubble">${m.role === 'assistant' ? mdToHtml(m.content) : escHtml(m.content)}</div>
    </div>`).join('');
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
            renderMessages();
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
    <div class="page-header"><div><h1 class="page-title">Settings</h1><p class="page-sub">Configure backend, token and model list</p></div></div>

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
          <input type="password" id="set-token" placeholder="${state.settings.token || 'Enter new token...'}" class="input-full">
          <p class="field-help">Changes take effect immediately for new requests.</p>
        </div>
        <button class="btn btn-primary" onclick="saveSettings()">Save Configuration</button>
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
      </div>
    </div>`;
  renderModelsTable();
}

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

window.deleteModel = (i) => {
  state.settings.models.splice(i, 1);
  renderModelsTable();
};

window.saveSettings = async () => {
  const backend = $('set-backend').value;
  const token = $('set-token').value;

  try {
    await api('/dashboard/api/settings', {
      method: 'POST',
      body: JSON.stringify({ backend, token, models: state.settings.models })
    });
    showToast('Settings saved successfully');
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
        <button class="btn btn-ghost btn-sm" onclick="fetchLogs()">Refresh</button>
        <button class="btn btn-danger btn-sm" onclick="clearLogs()">Clear</button>
      </div>
    </div>
    <div id="log-list" class="table-wrap">Loading...</div>`;
  fetchLogs();
}

async function fetchLogs() {
  try {
    const d = await api('/dashboard/api/logs');
    state.logs.entries = d.logs || [];
    const area = $('log-list');
    if (!area) return;

    if (state.logs.entries.length === 0) {
      area.innerHTML = '<div class="empty-state">No logs yet.</div>';
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
          ${state.logs.entries.map(l => `
            <tr onclick="showLogDetail('${l.id}')">
              <td class="td-ts">${fmtTime(l.timestamp)}</td>
              <td><span class="method-badge method-${l.method}">${l.method}</span></td>
              <td class="td-path">${escHtml(l.path)}</td>
              <td><span class="status-chip ${l.statusCode < 400 ? 's2xx' : l.statusCode < 500 ? 's4xx' : 's5xx'}">${l.statusCode}</span></td>
              <td>${l.is_sse ? '<span class="stream-chip">SSE</span>' : '—'}</td>
            </tr>
          `).reverse().join('')}
        </tbody>
      </table>`;
  } catch (err) { showToast('Failed to fetch logs', 'error'); }
}

window.showLogDetail = async (id) => {
  const modal = $('log-modal');
  const body = $('log-modal-body');
  modal.classList.add('open');
  body.innerHTML = '<div class="spinner"></div>';
  
  try {
    const data = await api(`/dashboard/api/logs/${id}`);
    body.innerHTML = `
      <div class="log-detail-grid">
        <div>
          <div class="log-detail-label">Request Body</div>
          <div class="json-block">${syntaxJson(data.body)}</div>
        </div>
        <div>
          <div class="log-detail-label">Response Data</div>
          <div class="json-block">${syntaxJson(data.response_body)}</div>
        </div>
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

window.closeLogModal = () => $('log-modal').classList.remove('open');
window.clearLogs = async () => {
  if (!confirm('Clear all request logs?')) return;
  await api('/dashboard/api/logs', { method: 'DELETE' });
  fetchLogs();
};

function renderSystemLogs() {
  $('content').innerHTML = `
    <div class="page-header">
      <div><h1 class="page-title">System Logs</h1><p class="page-sub">Runtime events and process output</p></div>
      <button class="btn btn-ghost btn-sm" onclick="fetchSystemLogs()">Refresh</button>
    </div>
    <div id="sys-list" class="terminal">Loading...</div>`;
  fetchSystemLogs();
}

async function fetchSystemLogs() {
  try {
    const d = await api('/dashboard/api/logs/system');
    const area = $('sys-list');
    if (!area) return;
    area.innerHTML = d.logs.map(l => `
      <div class="log-line level-${l.level}">
        <span class="log-ts">[${fmtTime(l.timestamp)}]</span>
        <span class="log-src src-${l.source || 'system'}">${l.source || 'SYS'}</span>
        <span class="log-msg">${escHtml(l.message)}</span>
      </div>`).reverse().join('');
  } catch (err) { showToast('Failed to fetch system logs', 'error'); }
}

init();
