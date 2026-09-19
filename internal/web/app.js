'use strict';

// The dashboard is vanilla JS on purpose: no framework, no bundler, no CDN.
// See internal/web/embed.go for why. Everything here is a fetch, a table
// render, and one SSE subscription.
//
// Wire-format note: store.Event/Session/Warning and the stats shapes carry no
// JSON tags, so their keys are the Go field names ("ID", "TotalPromptTokens",
// "ApiEquivalentCostUSD"). The api-local response types (statsResponse,
// pricesResponse, healthResponse) DO carry tags, so those keys are lowercase.
// Mixing the two up is the one easy mistake here; each render below names the
// keys it expects.

const $ = (id) => document.getElementById(id);

// ---------------------------------------------------------------- formatting

const fmtInt = (n) => (n || 0).toLocaleString();

// Costs arrive as pointers: null means "no row of that billing mode", which is
// NOT the same as $0.00 (invariant 5). Rendering null as "$0.00" would invent
// a figure; rendering it as "--" says what is true.
function fmtUSD(v) {
  if (v === null || v === undefined) return '--';
  const n = Number(v);
  if (n === 0) return '$0.00';
  return n < 0.01 ? '$' + n.toFixed(6) : '$' + n.toFixed(2);
}

function fmtTime(iso) {
  if (!iso) return '--';
  const d = new Date(iso);
  if (isNaN(d)) return '--';
  return d.toLocaleString();
}

function esc(s) {
  return String(s === null || s === undefined ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// -------------------------------------------------------------------- fetch

async function api(path, opts) {
  const res = await fetch(path, opts);
  const text = await res.text();
  let body = null;
  if (text) {
    try { body = JSON.parse(text); } catch (e) { body = text; }
  }
  if (!res.ok) {
    const msg = body && body.error ? body.error : res.status + ' ' + res.statusText;
    throw new Error(msg);
  }
  return {
    body,
    total: Number(res.headers.get('X-Total-Count') || 0),
    limit: Number(res.headers.get('X-Limit') || 0),
    offset: Number(res.headers.get('X-Offset') || 0),
  };
}

function setStatus(text, isError) {
  const s = $('status');
  s.textContent = text || '';
  s.className = isError ? 'error' : 'muted';
  s.hidden = !text;
}

// ------------------------------------------------------------------- tables

// table renders rows into a container. cols is [{head, cell}], and an empty
// row set renders the label rather than an empty box, so "no data" never looks
// like "still loading".
function table(container, cols, rows, emptyLabel) {
  if (!rows || rows.length === 0) {
    container.innerHTML = '<p class="muted">' + esc(emptyLabel || 'nothing here yet') + '</p>';
    return;
  }
  const head = cols.map((c) => '<th>' + esc(c.head) + '</th>').join('');
  const body = rows.map((r) => {
    const tds = cols.map((c) => '<td>' + c.cell(r) + '</td>').join('');
    return '<tr>' + tds + '</tr>';
  }).join('');
  container.innerHTML = '<table><thead><tr>' + head + '</tr></thead><tbody>' + body + '</tbody></table>';
}

function cards(container, items) {
  container.innerHTML = items.map((i) =>
    '<div class="card"><div class="card-label">' + esc(i.label) + '</div>' +
    '<div class="card-value">' + i.value + '</div></div>').join('');
}

const mono = (s) => '<code>' + esc(s) + '</code>';

// -------------------------------------------------------------- view: calls

const callState = { offset: 0, limit: 50 };

function callFilter() {
  const q = new URLSearchParams();
  const src = $('f-source').value.trim();
  const model = $('f-model').value.trim();
  const billing = $('f-billing').value;
  if (src) q.set('source', src);
  if (model) q.set('model', model);
  if (billing) q.set('billing_mode', billing);
  q.set('limit', String(callState.limit));
  q.set('offset', String(callState.offset));
  return q;
}

async function loadCalls() {
  const { body, total } = await api('/api/requests?' + callFilter().toString());
  const rows = body || [];
  table($('calls-table'), [
    { head: 'id', cell: (e) => '<a href="#" data-call="' + e.ID + '">' + e.ID + '</a>' },
    { head: 'time', cell: (e) => esc(fmtTime(e.StartedAt)) },
    { head: 'source', cell: (e) => esc(e.Source) },
    { head: 'model', cell: (e) => esc(e.ModelResolved || e.ModelRequested) },
    { head: 'billing', cell: (e) => esc(e.BillingMode) },
    { head: 'prompt', cell: (e) => fmtInt(e.TotalPromptTokens) },
    { head: 'output', cell: (e) => fmtInt(e.OutputTokens) },
    { head: 'api $', cell: (e) => fmtUSD(e.CostUSD) },
    { head: 'sub $', cell: (e) => fmtUSD(e.ApiEquivalentCostUSD) },
    { head: 'status', cell: (e) => (e.Status ? e.Status : '--') },
  ], rows, 'no calls captured yet');

  const from = rows.length ? callState.offset + 1 : 0;
  $('calls-range').textContent = from + '-' + (callState.offset + rows.length) + ' of ' + total;
  $('calls-prev').disabled = callState.offset <= 0;
  $('calls-next').disabled = callState.offset + rows.length >= total;
}

async function showCall(id) {
  const { body } = await api('/api/requests/' + id);
  const e = body;
  const details = [
    ['request id', mono(e.RequestID)],
    ['session', e.SessionID ? mono(e.SessionID) : '--'],
    ['project', esc(e.Project || '--')],
    ['path', e.Path ? mono(e.Method + ' ' + e.Path) : '--'],
    // The prompt size is the SUM, not input_tokens alone: input_tokens is the
    // uncached remainder (CLAUDE.md's tested invariant).
    ['prompt breakdown', fmtInt(e.InputTokens) + ' in + ' + fmtInt(e.CacheWrite5mTokens) +
      ' 5m + ' + fmtInt(e.CacheWrite1hTokens) + ' 1h + ' + fmtInt(e.CacheReadTokens) +
      ' read = ' + fmtInt(e.TotalPromptTokens)],
    ['thinking', fmtInt(e.ThinkingTokens)],
    ['stop', esc(e.StopReason || '--')],
    ['cost source', esc(e.CostSource || '--')],
    ['replay of', e.ReplayOf ? mono(e.ReplayOf) : '--'],
    ['replay edits', e.ReplayEdits ? mono(e.ReplayEdits) : '--'],
  ].map(([k, v]) => '<tr><th>' + esc(k) + '</th><td>' + v + '</td></tr>').join('');

  const warnings = (e.warnings || []).map((w) =>
    '<li><strong>' + esc(w.Kind) + '</strong> <span class="sev-' + esc(w.Severity) + '">' +
    esc(w.Severity) + '</span> ' + esc(w.Detail) + '</li>').join('');

  $('call-detail').innerHTML =
    '<h2>Call ' + e.ID + '</h2><table class="kv">' + details + '</table>' +
    (warnings ? '<h3>Warnings</h3><ul>' + warnings + '</ul>' : '') +
    '<h3>Replay</h3><div class="row">' +
    '<label>set <input id="replay-set" placeholder="max_tokens=250"></label>' +
    '<button type="button" id="replay-go">Replay</button></div>' +
    '<p class="muted">Replay is off unless <code>clens serve --replay</code> is running. ' +
    'It re-sends this call and bills your account.</p>';

  $('replay-go').addEventListener('click', async () => {
    const set = $('replay-set').value.trim();
    const q = set ? '?set=' + encodeURIComponent(set) : '';
    try {
      setStatus('replaying…');
      const r = await api('/api/requests/' + id + '/replay' + q, { method: 'POST' });
      setStatus('replayed as id ' + r.body.ID, false);
      await loadCalls();
    } catch (err) {
      setStatus('replay failed: ' + err.message, true);
    }
  });
}

// ----------------------------------------------------------- view: sessions

const sessionState = { offset: 0, limit: 50 };

async function loadSessions() {
  const { body, total } = await api('/api/sessions?limit=' + sessionState.limit +
    '&offset=' + sessionState.offset);
  const rows = body || [];
  table($('sessions-table'), [
    { head: 'session', cell: (s) => '<a href="#" data-session="' + esc(s.ID) + '">' +
        esc(s.ID.slice(0, 12)) + '</a>' },
    { head: 'first seen', cell: (s) => esc(fmtTime(s.FirstSeen)) },
    { head: 'last seen', cell: (s) => esc(fmtTime(s.LastSeen)) },
    { head: 'calls', cell: (s) => fmtInt(s.RequestCount) },
    { head: 'prompt', cell: (s) => fmtInt(s.TotalPromptTokens) },
    { head: 'output', cell: (s) => fmtInt(s.OutputTokens) },
    { head: 'api $', cell: (s) => fmtUSD(s.TotalCostUSD) },
    { head: 'sub $', cell: (s) => fmtUSD(s.TotalApiEquivalentCostUSD) },
    { head: 'priced', cell: (s) => fmtInt(s.PricedCount) + '/' +
        fmtInt(s.PricedCount + s.UnpricedCount) },
    { head: 'warnings', cell: (s) => fmtInt(s.WarningCount) },
    { head: 'models', cell: (s) => esc((s.ModelSet || []).join(', ')) },
  ], rows, 'no sessions resolved yet');
}

async function showSession(id) {
  const { body } = await api('/api/sessions/' + id);
  const calls = (body.calls || []).map((e) =>
    '<tr><td><a href="#" data-call="' + e.ID + '">' + e.ID + '</a></td><td>' +
    esc(fmtTime(e.StartedAt)) + '</td><td>' + esc(e.ModelResolved || e.ModelRequested) +
    '</td><td>' + fmtInt(e.TotalPromptTokens) + '</td><td>' + fmtInt(e.OutputTokens) +
    '</td><td>' + fmtUSD(e.CostUSD) + '</td><td>' + fmtUSD(e.ApiEquivalentCostUSD) +
    '</td></tr>').join('');

  $('session-detail').innerHTML = '<h2>Session ' + esc(body.ID) + '</h2>' +
    '<table class="kv"><tr><th>calls</th><td>' + fmtInt(body.RequestCount) + '</td></tr>' +
    '<tr><th>prompt</th><td>' + fmtInt(body.TotalPromptTokens) + '</td></tr>' +
    '<tr><th>output</th><td>' + fmtInt(body.OutputTokens) + '</td></tr>' +
    '<tr><th>api $</th><td>' + fmtUSD(body.TotalCostUSD) + '</td></tr>' +
    '<tr><th>sub $</th><td>' + fmtUSD(body.TotalApiEquivalentCostUSD) + '</td></tr></table>' +
    (calls ? '<h3>Calls</h3><table><thead><tr><th>id</th><th>time</th><th>model</th>' +
      '<th>prompt</th><th>output</th><th>api $</th><th>sub $</th></tr></thead><tbody>' +
      calls + '</tbody></table>' : '');
}

// ----------------------------------------------------------- view: warnings

const warningState = { offset: 0, limit: 50, kind: '' };

async function loadWarnings() {
  const summary = (await api('/api/warnings/summary')).body || [];
  const total = summary.reduce((a, s) => a + s.Count, 0);
  cards($('warnings-summary'), [{ label: 'all kinds', value: fmtInt(total) }].concat(
    summary.map((s) => ({
      label: s.Kind,
      value: '<a href="#" data-kind="' + esc(s.Kind) + '">' + fmtInt(s.Count) + '</a>',
    }))));

  const q = new URLSearchParams({ limit: String(warningState.limit), offset: String(warningState.offset) });
  if (warningState.kind) q.set('kind', warningState.kind);
  const { body, total: count } = await api('/api/warnings?' + q.toString());
  table($('warnings-table'), [
    { head: 'id', cell: (w) => w.ID },
    { head: 'time', cell: (w) => esc(fmtTime(w.CreatedAt)) },
    { head: 'kind', cell: (w) => esc(w.Kind) },
    { head: 'severity', cell: (w) => '<span class="sev-' + esc(w.Severity) + '">' +
        esc(w.Severity) + '</span>' },
    { head: 'call', cell: (w) => '<a href="#" data-call="' + w.EventID + '">' + w.EventID + '</a>' },
    { head: 'path', cell: (w) => (w.Path ? mono(w.Path) : '--') },
    { head: 'detail', cell: (w) => esc(w.Detail) },
  ], body, warningState.kind ? 'no warnings of kind ' + warningState.kind : 'no warnings raised');

  if (warningState.kind) setStatus('filtered to ' + warningState.kind + ' (' + count + ')');
}

// -------------------------------------------------------------- view: stats

async function loadStats() {
  const q = new URLSearchParams();
  if ($('s-since').value.trim()) q.set('since', $('s-since').value.trim());
  if ($('s-until').value.trim()) q.set('until', $('s-until').value.trim());
  q.set('granularity', $('s-granularity').value);

  const { body } = await api('/api/stats?' + q.toString());
  const s = body.summary;

  cards($('stats-body'), [
    { label: 'calls', value: fmtInt(s.RequestCount) },
    { label: 'prompt tokens', value: fmtInt(s.TotalPromptTokens) },
    { label: 'output tokens', value: fmtInt(s.OutputTokens) },
    { label: 'api $', value: fmtUSD(s.TotalCostUSD) },
    { label: 'sub $', value: fmtUSD(s.TotalApiEquivalentCostUSD) },
    { label: 'priced', value: fmtInt(s.PricedCount) + '/' + fmtInt(s.PricedCount + s.UnpricedCount) },
  ]);

  const byModel = (body.by_model || []).map((m) =>
    '<tr><td>' + esc(m.Model) + '</td><td>' + fmtInt(m.RequestCount) + '</td><td>' +
    fmtInt(m.TotalPromptTokens) + '</td><td>' + fmtInt(m.OutputTokens) + '</td><td>' +
    fmtUSD(m.TotalCostUSD) + '</td><td>' + fmtUSD(m.TotalApiEquivalentCostUSD) + '</td></tr>').join('');
  const byPeriod = (body.by_period || []).map((p) =>
    '<tr><td>' + esc(fmtTime(p.PeriodStart)) + '</td><td>' + fmtInt(p.RequestCount) + '</td><td>' +
    fmtInt(p.TotalPromptTokens) + '</td><td>' + fmtInt(p.OutputTokens) + '</td><td>' +
    fmtUSD(p.TotalCostUSD) + '</td><td>' + fmtUSD(p.TotalApiEquivalentCostUSD) + '</td></tr>').join('');
  const bySource = (body.cost_sources || []).map((c) =>
    '<tr><td>' + esc(c.CostSource || 'unpriced') + '</td><td>' + fmtInt(c.RequestCount) +
    '</td><td>' + fmtUSD(c.TotalCostUSD) + '</td></tr>').join('');

  $('stats-body').innerHTML +=
    chartByPeriod(body.by_period || []) +
    '<h3>By model</h3>' + wrap(byModel, 'model', 'no models in this window') +
    '<h3>By period</h3>' + wrap(byPeriod, 'period', 'no periods in this window') +
    '<h3>By cost source</h3>' + wrap(bySource, 'cost source', 'no priced calls in this window');
}

function wrap(rows, what, empty) {
  if (!rows) return '<p class="muted">' + esc(empty) + '</p>';
  return '<table><thead><tr><th>' + esc(what) + '</th><th>calls</th><th>prompt</th>' +
    '<th>output</th><th>api $</th><th>sub $</th></tr></thead><tbody>' + rows + '</tbody></table>';
}

// chartByPeriod is a hand-rolled inline SVG bar chart of requests per period.
// Inline SVG rather than a charting library because a library is a build step
// or a CDN, both of which embed.go's rationale rules out. It charts request
// COUNT only: height is a ratio, and the two cost columns are different
// billing models that must never be summed onto one axis (invariant 5). The
// real charts land in br-GI-1-18.
function chartByPeriod(periods) {
  if (!periods.length) return '';
  const w = 640, h = 120, gap = 2;
  const max = Math.max.apply(null, periods.map((p) => p.RequestCount)) || 1;
  const bw = (w - gap * (periods.length - 1)) / periods.length;
  const bars = periods.map((p, i) => {
    const bh = Math.max(1, Math.round((p.RequestCount / max) * (h - 20)));
    const x = i * (bw + gap);
    return '<rect x="' + x.toFixed(1) + '" y="' + (h - bh) + '" width="' + bw.toFixed(1) +
      '" height="' + bh + '"><title>' + esc(fmtTime(p.PeriodStart)) + ': ' +
      fmtInt(p.RequestCount) + ' calls</title></rect>';
  }).join('');
  return '<h3>Requests per period</h3><svg class="chart" viewBox="0 0 ' + w + ' ' + h +
    '" preserveAspectRatio="none" role="img" aria-label="requests per period">' + bars + '</svg>';
}

// ----------------------------------------------------------- view: settings

async function loadSettings() {
  const health = (await api('/api/health')).body;
  cards($('health-cards'), [
    { label: 'sink accepted', value: fmtInt(health.sink_accepted) },
    { label: 'sink dropped', value: fmtInt(health.sink_dropped) },
    { label: 'consumer processed', value: fmtInt(health.consumer_processed) },
    { label: 'consumer failed', value: fmtInt(health.consumer_failed) },
    { label: 'consumer drained', value: fmtInt(health.consumer_drained) },
    { label: 'last write', value: health.last_write_at ? esc(fmtTime(health.last_write_at)) : 'never' },
    { label: 'replay rejected', value: fmtInt(health.replay_rejected) },
  ]);

  let models;
  try {
    models = (await api('/api/prices')).body.models || [];
  } catch (err) {
    $('prices-table').innerHTML = '<p class="error">' + esc(err.message) + '</p>';
    return;
  }

  const cols = ['input_rate', 'output_rate', 'cache_write_5m_rate', 'cache_write_1h_rate', 'cache_read_rate'];
  const head = '<tr><th>model</th>' + cols.map((c) => '<th>' + c.replace('_rate', '') + '</th>').join('') +
    '<th>source</th><th></th></tr>';
  const rows = models.map((m) => {
    const cells = cols.map((c) => {
      const v = m[c];
      return '<td><input type="number" step="any" min="0" data-rate="' + c +
        '" value="' + (v === null || v === undefined ? '' : v) + '" placeholder="unset"></td>';
    }).join('');
    return '<tr data-model="' + esc(m.model) + '"><td>' + esc(m.model) + '</td>' + cells +
      '<td>' + esc(m.source) + '</td><td><button type="button" data-save>Save</button></td></tr>';
  }).join('');
  $('prices-table').innerHTML = '<table><thead>' + head + '</thead><tbody>' + rows + '</tbody></table>';

  $('prices-table').addEventListener('click', async (ev) => {
    const btn = ev.target.closest('button[data-save]');
    if (!btn) return;
    const tr = btn.closest('tr');
    // Blank means unset, which is not 0: a 0 rate would claim the model is
    // free rather than unpriced. The empty string is dropped, not coerced.
    const payload = { model: tr.dataset.model };
    tr.querySelectorAll('input[data-rate]').forEach((inp) => {
      if (inp.value.trim() !== '') payload[inp.dataset.rate] = Number(inp.value);
    });
    try {
      setStatus('saving ' + payload.model + '…');
      await api('/api/prices', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      setStatus('saved ' + payload.model + ': prices the next call the consumer sees', false);
    } catch (err) {
      setStatus('save failed: ' + err.message, true);
    }
  });
}

// -------------------------------------------------------------- view: shell

async function loadOverview() {
  const { body } = await api('/api/stats?since=24h');
  const s = body.summary;
  cards($('overview-cards'), [
    { label: 'calls (24h)', value: fmtInt(s.RequestCount) },
    { label: 'prompt tokens', value: fmtInt(s.TotalPromptTokens) },
    { label: 'output tokens', value: fmtInt(s.OutputTokens) },
    { label: 'api $ (24h)', value: fmtUSD(s.TotalCostUSD) },
    { label: 'sub $ (24h)', value: fmtUSD(s.TotalApiEquivalentCostUSD) },
  ]);

  const { body: recent } = await api('/api/requests?limit=25');
  table($('overview-calls'), [
    { head: 'id', cell: (e) => '<a href="#" data-call="' + e.ID + '">' + e.ID + '</a>' },
    { head: 'time', cell: (e) => esc(fmtTime(e.StartedAt)) },
    { head: 'model', cell: (e) => esc(e.ModelResolved || e.ModelRequested) },
    { head: 'prompt', cell: (e) => fmtInt(e.TotalPromptTokens) },
    { head: 'api $', cell: (e) => fmtUSD(e.CostUSD) },
    { head: 'sub $', cell: (e) => fmtUSD(e.ApiEquivalentCostUSD) },
  ], recent, 'no calls captured yet');
}

const loaders = {
  overview: loadOverview,
  calls: loadCalls,
  sessions: loadSessions,
  warnings: loadWarnings,
  stats: loadStats,
  settings: loadSettings,
};

let current = 'overview';

async function show(view) {
  current = view;
  document.querySelectorAll('#tabs .tab').forEach((b) => {
    b.classList.toggle('active', b.dataset.view === view);
  });
  document.querySelectorAll('.view').forEach((s) => {
    s.hidden = s.id !== 'view-' + view;
  });
  setStatus('');
  try {
    await loaders[view]();
  } catch (err) {
    setStatus(err.message, true);
  }
}

// ------------------------------------------------------------------ wiring

$('tabs').addEventListener('click', (ev) => {
  const tab = ev.target.closest('.tab');
  if (tab) show(tab.dataset.view);
});

// One delegated listener for every drill-down link the tables render.
document.addEventListener('click', async (ev) => {
  const call = ev.target.closest('[data-call]');
  if (call) {
    ev.preventDefault();
    await show('calls');
    try { await showCall(call.dataset.call); } catch (err) { setStatus(err.message, true); }
    return;
  }
  const sess = ev.target.closest('[data-session]');
  if (sess) {
    ev.preventDefault();
    try { await showSession(sess.dataset.session); } catch (err) { setStatus(err.message, true); }
    return;
  }
  const kind = ev.target.closest('[data-kind]');
  if (kind) {
    ev.preventDefault();
    warningState.kind = kind.dataset.kind;
    warningState.offset = 0;
    await loadWarnings();
  }
});

$('f-apply').addEventListener('click', () => { callState.offset = 0; loadCalls(); });
$('calls-prev').addEventListener('click', () => {
  callState.offset = Math.max(0, callState.offset - callState.limit);
  loadCalls();
});
$('calls-next').addEventListener('click', () => {
  callState.offset += callState.limit;
  loadCalls();
});
$('s-apply').addEventListener('click', loadStats);

// The header totals come from the same stats call the Overview uses, minus the
// 24h window: they are all-time, which is what a header total should be.
async function loadTotals() {
  try {
    const { body } = await api('/api/stats');
    const s = body.summary;
    $('total-calls').textContent = fmtInt(s.RequestCount);
    $('total-tokens').textContent = fmtInt(s.TotalPromptTokens + s.OutputTokens);
    $('total-cost-api').textContent = fmtUSD(s.TotalCostUSD);
    $('total-cost-sub').textContent = fmtUSD(s.TotalApiEquivalentCostUSD);
  } catch (err) {
    /* fail open: a header total is not worth an error banner over the view */
  }
}

// Live updates: every event the consumer commits is pushed over SSE. Re-running
// the current view's loader on each event is the lazy correct thing here -- the
// alternative is patching rows in place, which would have to re-derive the
// pagination totals and the period chart anyway.
//
// ponytail: refetch-per-event, coalesced to one in flight. At a few calls a
// second this is nothing; if a burst ever makes it visible, buffer and reload
// on a timer instead.
let refreshing = false;
function subscribe() {
  const es = new EventSource('/api/stream');
  es.onmessage = async () => {
    if (refreshing) return;
    refreshing = true;
    try {
      await Promise.all([loadTotals(), loaders[current]()]);
    } catch (err) {
      setStatus(err.message, true);
    } finally {
      refreshing = false;
    }
  };
  // EventSource reconnects on its own; the browser handles backoff. Saying so
  // beats a banner the user cannot act on.
  es.onerror = () => setStatus('live updates disconnected — retrying…', true);
}

loadTotals();
show('overview');
subscribe();
