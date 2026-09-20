'use strict';

// The dashboard is vanilla JS on purpose: no framework, no bundler, no CDN.
// See internal/web/embed.go for why. Everything here is a fetch, a table
// render, and one SSE subscription.
//
// Wire-format note: store.Event/Session/Warning and the stats shapes carry no
// JSON tags, so their keys are the Go field names ("ID", "TotalPromptTokens",
// "ApiEquivalentCostUSD"). The api-local response types (statsResponse,
// pricesResponse, healthResponse, sourcesResponse, quotaResponse,
// reconcileResponse, modelsResponse) DO carry tags, so those keys are
// lowercase — and snake_case, not Go's casing. Mixing the two up is the one
// easy mistake here; each render below names the keys it expects.

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

// fmtPct renders a percentage that is allowed to be absent. Utilization against
// a limit nobody configured is exactly that case, and it must not print as 0%.
function fmtPct(v) {
  if (v === null || v === undefined) return '--';
  return Number(v).toFixed(1) + '%';
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
// like "still loading". rowClass is optional: it returns a class for a row, or
// '' for none, which is how the Sources tab paints a dead collector red.
function table(container, cols, rows, emptyLabel, rowClass) {
  if (!rows || rows.length === 0) {
    container.innerHTML = '<p class="muted">' + esc(emptyLabel || 'nothing here yet') + '</p>';
    return;
  }
  const head = cols.map((c) => '<th>' + esc(c.head) + '</th>').join('');
  const body = rows.map((r, i) => {
    const tds = cols.map((c) => '<td>' + c.cell(r) + '</td>').join('');
    const cls = rowClass ? rowClass(r, i) : '';
    return '<tr' + (cls ? ' class="' + esc(cls) + '"' : '') + '>' + tds + '</tr>';
  }).join('');
  container.innerHTML = '<table><thead><tr>' + head + '</tr></thead><tbody>' + body + '</tbody></table>';
}

function cards(container, items) {
  container.innerHTML = items.map((i) =>
    '<div class="card"><div class="card-label">' + esc(i.label) + '</div>' +
    '<div class="card-value">' + i.value + '</div></div>').join('');
}

const mono = (s) => '<code>' + esc(s) + '</code>';

// ------------------------------------------------------------------- bodies

// decode.Completeness crosses the wire as its integer value: the type has a
// String() for the server's own logs but no MarshalJSON, so encoding/json
// writes 0/1/2/3. Named here so the marker branches below read as the states
// they are rather than as magic numbers.
const COMPLETE = 0;
const TRUNCATED_AT_CAP = 1;
const PARTIAL_CORRUPT = 2;
const NOT_DECODED = 3;

// wireBytes turns the base64 encoding/json uses for a Go []byte into the bytes
// themselves. A body is arbitrary bytes, not necessarily text -- a compressed
// stream that would not decompress is stored raw -- so the byte count comes
// from the decoded length and the text decode is told to substitute rather
// than throw.
function wireBytes(b64) {
  if (!b64) return new Uint8Array(0);
  let bin;
  try { bin = atob(b64); } catch (e) { return new Uint8Array(0); }
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

// bodySection renders one body: collapsed by default, byte count in the
// summary, marker line above it so an incomplete body is never mistaken for a
// whole one.
//
// This is the single place a body reaches innerHTML, which is the point. Bodies
// are arbitrary bytes from a remote endpoint landing in a page that also holds
// a replay button that spends money, so it is the story's one real injection
// surface and it has one esc() to review rather than one per call site. label
// and marker are literals from this file -- never wire data -- so esc() wraps
// the one thing that is.
function bodySection(label, bytesB64, marker) {
  const bytes = wireBytes(bytesB64);
  const head = marker ? '<p class="body-marker">' + marker + '</p>' : '';
  return head + '<details class="body"><summary>' + label + ' — ' + fmtInt(bytes.length) +
    ' bytes</summary>' +
    '<pre class="body-text">' + esc(new TextDecoder('utf-8').decode(bytes)) + '</pre></details>';
}

// headerRows renders a stored header blob as kv rows. Redaction ran before the
// row was written, so "[redacted]" is the redactor's own output and is shown as
// such: hiding it would make a redacted credential look like an absent one,
// which is a different claim. A blob that will not parse is labelled, never
// silently rendered as an empty table.
function headerRows(label, blob) {
  const heading = '<h3>' + esc(label) + '</h3>';
  if (!blob) return heading + '<p class="muted">no headers stored on this row</p>';
  let h;
  try { h = JSON.parse(blob); } catch (e) {
    return heading + '<p class="muted">stored headers are not parseable</p>';
  }
  const rows = Object.keys(h).sort().map((k) =>
    '<tr><th>' + esc(k) + '</th><td>' + esc((h[k] || []).join(', ')) + '</td></tr>').join('');
  return rows ? heading + '<table class="kv">' + rows + '</table>'
    : heading + '<p class="muted">no headers stored on this row</p>';
}

// readPathMarker says how the response body on screen relates to the bytes that
// were stored, or '' when it is the whole thing and needs no caveat.
//
// BodyCapBytes is checked FIRST, before RespBodyCompleteness is consulted. With
// the cap unwired the server never decoded at all and reports Complete by
// construction, so ordering it second would mostly work -- and would then
// render "would not decompress" for a body that decompresses fine the moment a
// stale completeness rode along on an unwired response. A missing cap is not
// evidence about the bytes.
function readPathMarker(e) {
  if (!e.BodyCapBytes) return 'response shown raw — read cap not configured';
  if (e.RespBodyCompleteness === TRUNCATED_AT_CAP) {
    return 'response truncated at the read cap of ' + fmtInt(e.BodyCapBytes) + ' bytes';
  }
  if (e.RespBodyCompleteness === PARTIAL_CORRUPT) {
    return 'response decoded only partially — its tail was corrupt';
  }
  if (e.RespBodyCompleteness === NOT_DECODED) {
    return 'response shown undecoded — it would not decompress';
  }
  return '';
}

// captureMarker reports a capture the proxy could not finish, in the CLI's own
// wording. CaptureComplete is false for either cause -- a body cut at the cap,
// or a stream that ended without message_stop -- so the line names the cap only
// in the one case where it is knowably the cause, and says so plainly when the
// row does not record which of the two it was. Claiming the cap unconditionally
// would be a second, quieter defect in the thing that exists to report the
// first.
//
// CaptureComplete decides *whether* the marker draws; the length comparison
// only decides *which* body to name in it. The order matters and is the
// deliberate half: a body whose length merely equals the cap is a coincidence,
// not evidence, so it must not conjure a marker for a capture the proxy
// recorded as whole. What it can do is stop the marker saying "the row does not
// record which cause" when one stored body is sitting there at exactly the cap
// — which is what checking the response alone produces for a request-side cut
// (br-GI-7-08).
//
// Rows written before that bead keep capture_complete = 1 and draw nothing,
// because nothing here can distinguish their cut request body from a
// coincidence either. The fix is not retroactive.
function captureMarker(e) {
  if (e.CaptureComplete) return '';
  const atCap = [];
  if (e.BodyCapBytes) {
    if (wireBytes(e.ReqBody).length === e.BodyCapBytes) atCap.push('request');
    if (wireBytes(e.RespBody).length === e.BodyCapBytes) atCap.push('response');
  }
  if (atCap.length === 0) {
    return 'incomplete (truncated, or the stream ended early) — the row does not record which cause';
  }
  return 'incomplete (truncated, or the stream ended early) — the stored ' +
    (atCap.length === 2 ? 'request and response bodies are' : atCap[0] + ' body is') +
    ' exactly the ' + fmtInt(e.BodyCapBytes) + '-byte read cap, so the cap is the cause on this row';
}

// transcriptCapMarker is captureMarker's counterpart for the third content
// column. transcript_content is bounded by the same --body-cap-bytes the two
// bodies are (br-GI-7-09), and without this a reconstruction cut at the cap
// would render identically to a whole one -- the "a truncated capture and a
// complete one look identical" defect this story exists to close, reintroduced
// on the column the bodies' marker does not cover.
//
// It is deliberately NOT driven by CaptureComplete: that flag is about the two
// teed bodies, and a capped transcript has not truncated any capture. Length
// against the cap is the only signal there is, and it is the same one the
// bodies use, and with the same caveat the wording carries: a content exactly
// the cap's length might be a coincidence, which is why the marker names the
// measurement and then the inference rather than asserting the line was cut.
function transcriptCapMarker(e) {
  if (!e.BodyCapBytes) return '';
  if (wireBytes(e.TranscriptContent).length !== e.BodyCapBytes) return '';
  // States the measurement, then names the inference -- the same shape
  // captureMarker uses, and for the same reason: a content exactly the cap's
  // length may simply have been that long, so asserting "it carried more"
  // would claim a cause this row does not record.
  return 'capped, or exactly this long — the stored reconstruction is the ' +
    fmtInt(e.BodyCapBytes) + '-byte read cap, so the cap is the cause on this row';
}

// --------------------------------------------------------- list/detail modes

// Selecting a call shows the detail *instead of* the list it was clicked in:
// the list is hidden exactly when its detail is shown. Deriving the mode from
// the two `hidden` flags rather than a parallel boolean leaves nothing that can
// drift from what is actually on screen.
function setCallDetail(on) {
  $('calls-list').hidden = on;
  $('call-detail').hidden = !on;
}

function setSessionDetail(on) {
  $('sessions-list').hidden = on;
  $('session-detail').hidden = !on;
}

// Every view change and every drill-down takes the next generation. A detail
// response whose generation is no longer current is dropped instead of
// rendered: the fetch is not cancelled, only its result is.
let detailSeq = 0;

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

async function showCall(id, seq) {
  const { body } = await api('/api/requests/' + id); // may throw
  // Stale by the time it landed: a later view change or drill-down has taken a
  // newer generation, so these bodies are not what was asked for. Rendering
  // them would put one call's request and response on screen under a click for
  // a different one.
  if (seq !== detailSeq) return;
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

  // A transcript row has no *wire* bodies because transcripts have none, not
  // because capture failed -- and it carries no headers at all, so it gets no
  // header tables, the same distinction sources.go draws for collector health.
  // Its content, when br-GI-7-06 filled the row, is one assistant message: a
  // reconstruction of intent, not the request that produced it, so it is
  // labelled as neither half of the exchange. The two transcript states are
  // separate because absent content means one of three things -- written before
  // that bead, a non-assistant line, or br-GI-7-09's --body-policy off -- which
  // is a different claim from "reconstructed, and empty". The row does not
  // record which, so the copy names none of them.
  const sections = e.Source === 'jsonl'
    ? (e.TranscriptContent
      ? bodySection('reconstructed from transcript — not a wire capture', e.TranscriptContent,
        transcriptCapMarker(e))
      : '<p class="muted">not captured — transcript source</p>')
    : headerRows('Request headers', e.ReqHeaders) +
      headerRows('Response headers', e.RespHeaders) +
      bodySection('Request body', e.ReqBody, '') +
      bodySection('Response body', e.RespBodyDecoded, readPathMarker(e));

  const capture = captureMarker(e);

  // Only now, once there is a detail to show: flipping the mode before the
  // fetch resolves would blank the list for a request that may still fail.
  setCallDetail(true);
  $('call-detail').innerHTML =
    '<p><button type="button" id="call-back">‹ all calls</button></p>' +
    '<h2>Call ' + esc(e.ID) + '</h2><table class="kv">' + details + '</table>' +
    (capture ? '<p class="body-marker">' + capture + '</p>' : '') +
    sections +
    (warnings ? '<h3>Warnings</h3><ul>' + warnings + '</ul>' : '') +
    '<h3>Replay</h3><div class="row">' +
    '<label>set <input id="replay-set" placeholder="max_tokens=250"></label>' +
    '<button type="button" id="replay-go">Replay</button></div>' +
    '<p class="muted">Replay is off unless <code>clens serve --replay</code> is running. ' +
    'It re-sends this call and bills your account.</p>';

  // Bound here rather than parked in index.html so the control exists only
  // while a detail does -- the same shape as the replay button below. It
  // re-fetches: on a drill-down that started from another tab this is the
  // list's first load, so there is nothing already on screen to return to.
  $('call-back').addEventListener('click', () => {
    setCallDetail(false);
    loadCalls();
  });

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

  // Hiding a fifty-row list collapses the page under a scroll position that
  // pointed into it, and the browser clamps to the new height -- landing at the
  // *bottom* of the detail. scroll-margin-top (style.css) keeps the sticky
  // header off the back control this scrolls to.
  $('call-detail').scrollIntoView();
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

async function showSession(id, seq) {
  const { body } = await api('/api/sessions/' + id); // may throw
  // Same generation rule as showCall, and here it can fire on its own: with no
  // view switch the sessions list stays on screen until this resolves, so a
  // second session click while the first is in flight is possible.
  if (seq !== detailSeq) return;
  const calls = (body.calls || []).map((e) =>
    '<tr><td><a href="#" data-call="' + e.ID + '">' + e.ID + '</a></td><td>' +
    esc(fmtTime(e.StartedAt)) + '</td><td>' + esc(e.ModelResolved || e.ModelRequested) +
    '</td><td>' + fmtInt(e.TotalPromptTokens) + '</td><td>' + fmtInt(e.OutputTokens) +
    '</td><td>' + fmtUSD(e.CostUSD) + '</td><td>' + fmtUSD(e.ApiEquivalentCostUSD) +
    '</td></tr>').join('');

  setSessionDetail(true);
  $('session-detail').innerHTML =
    '<p><button type="button" id="session-back">‹ all sessions</button></p>' +
    '<h2>Session ' + esc(body.ID) + '</h2>' +
    '<table class="kv"><tr><th>calls</th><td>' + fmtInt(body.RequestCount) + '</td></tr>' +
    '<tr><th>prompt</th><td>' + fmtInt(body.TotalPromptTokens) + '</td></tr>' +
    '<tr><th>output</th><td>' + fmtInt(body.OutputTokens) + '</td></tr>' +
    '<tr><th>api $</th><td>' + fmtUSD(body.TotalCostUSD) + '</td></tr>' +
    '<tr><th>sub $</th><td>' + fmtUSD(body.TotalApiEquivalentCostUSD) + '</td></tr></table>' +
    (calls ? '<h3>Calls</h3><table><thead><tr><th>id</th><th>time</th><th>model</th>' +
      '<th>prompt</th><th>output</th><th>api $</th><th>sub $</th></tr></thead><tbody>' +
      calls + '</tbody></table>' : '');

  $('session-back').addEventListener('click', () => {
    setSessionDetail(false);
    loadSessions();
  });
  $('session-detail').scrollIntoView();
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
// billing models that must never be summed onto one axis (invariant 5).
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

// ------------------------------------------------------------ view: sources

// loadSources is the tab that makes "it is collecting" checkable. One row per
// source, and a source that errored gets a red row -- not an absent one.
//
// A collector that has never run reports status "unknown" and null timestamps.
// That is neither health nor failure, so it is neither red nor dated: the
// status cell says unknown and last-success says "never".
async function loadSources() {
  const { body } = await api('/api/sources');
  table($('sources-table'), [
    { head: 'source', cell: (s) => esc(s.source) },
    { head: 'status', cell: (s) => '<span class="status-' + esc(s.status) + '">' + esc(s.status) + '</span>' },
    { head: 'last success', cell: (s) => (s.last_success_at ? esc(fmtTime(s.last_success_at)) : 'never') },
    { head: 'last error', cell: (s) => (s.last_error_at ? esc(fmtTime(s.last_error_at)) : '--') },
    { head: 'rows written', cell: (s) => fmtInt(s.rows_written) },
    { head: 'cursor', cell: (s) => (s.cursor_position ? mono(s.cursor_position) : '--') },
    { head: 'reason', cell: (s) => esc(s.last_error || '--') },
  ], body.sources || [], 'no collectors have reported yet — run `clens ingest`',
    (s) => (s.status === 'error' ? 'row-bad' : ''));
}

// -------------------------------------------------------------- view: quota

// loadQuota renders one block per subscription account. Limits arrive as query
// params because nothing else configures them (internal/api/quota.go), so the
// two inputs here are the whole limit configuration.
//
// A percentage is rendered only when the route says limit_state is
// "configured". Otherwise the cell states the literal "unconfigured" -- the
// alternative, a percentage of an invented ceiling, is the one figure this tab
// must never show.
async function loadQuota() {
  const q = new URLSearchParams();
  document.querySelectorAll('#view-quota [data-window]').forEach((el) => {
    const v = el.value.trim();
    if (v) q.set('limit_' + el.dataset.window, v);
  });
  const { body } = await api('/api/quota' + (q.toString() ? '?' + q.toString() : ''));
  const accounts = body.accounts || [];
  if (!accounts.length) {
    $('quota-body').innerHTML = '<p class="muted">no subscription account is configured</p>';
    return;
  }

  $('quota-body').innerHTML = accounts.map((a) => {
    // Rebuilt rather than appended to a real container, so the table() helper's
    // empty-label still applies: give it a throwaway node, then read it back.
    const holder = document.createElement('div');
    table(holder, [
      { head: 'window', cell: (r) => esc(r.window) },
      { head: 'tokens burned', cell: (r) => fmtInt(r.tokens) },
      { head: 'requests', cell: (r) => fmtInt(r.requests) },
      { head: 'limit', cell: (r) => (r.limit_state === 'configured' ? fmtInt(r.limit) : '<span class="muted">unconfigured</span>') },
      { head: 'of limit', cell: (r) => (r.limit_state === 'configured' ? fmtPct(r.utilization_pct) : '--') },
      { head: 'approaching', cell: (r) => (r.limit_state === 'configured' ? (r.approaching ? 'yes' : 'no') : '--') },
      { head: 'last snapshot', cell: (r) => (r.last_snapshot
        ? esc(fmtTime(r.last_snapshot.observed_at)) + ' ' + fmtPct(r.last_snapshot.utilization_pct)
        : '<span class="muted">never polled</span>') },
    ], a.windows || [], 'no window rows', (r) => (r.approaching ? 'row-warn' : ''));

    const learned = (a.calibration || []).map((c) =>
      '<li>' + esc(c.window) + ': ' + fmtInt(c.tokens_at_100) + ' tokens reached 100% on ' +
      esc(fmtTime(c.observed_at)) + '</li>').join('');

    return '<h2>' + esc(a.account) + (a.plan ? ' <span class="muted">' + esc(a.plan) + '</span>' : '') + '</h2>' +
      chartQuota(a.windows || []) +
      holder.innerHTML +
      (learned ? '<h3>Candidate limits (offered, not applied)</h3><ul>' + learned + '</ul>' : '');
  }).join('');
}

// chartQuota draws tokens burned per rolling window, with each window's
// configured limit as a dashed line when there is one. Bars are scaled to the
// tallest of (our burn, the configured limit), so a limit line lands inside the
// box instead of clipping. With no limit the bars are scaled to our own
// measurements alone -- a ratio of what was observed, which is all there is,
// and it invents no ceiling.
function chartQuota(windows) {
  if (!windows || !windows.length) return '';
  const w = 640, h = 120, gap = 8;
  const bw = (w - gap * (windows.length - 1)) / windows.length;
  const top = Math.max.apply(null, windows.map((r) => Math.max(r.tokens, r.limit || 0))) || 1;
  const bars = windows.map((r, i) => {
    const x = i * (bw + gap);
    const bh = Math.max(1, Math.round((r.tokens / top) * (h - 24)));
    let s = '<rect class="' + (r.approaching ? 'bar-warn' : '') + '" x="' + x.toFixed(1) +
      '" y="' + (h - bh) + '" width="' + bw.toFixed(1) + '" height="' + bh + '"><title>' +
      esc(r.window) + ': ' + fmtInt(r.tokens) + ' tokens</title></rect>';
    if (r.limit_state === 'configured' && r.limit > 0) {
      const ly = h - Math.max(1, Math.round((r.limit / top) * (h - 24)));
      s += '<line class="limit-line" x1="' + x.toFixed(1) + '" x2="' + (x + bw).toFixed(1) +
        '" y1="' + ly + '" y2="' + ly + '"><title>limit: ' + fmtInt(r.limit) + ' tokens</title></line>';
    }
    s += '<text class="chart-label" x="' + (x + bw / 2).toFixed(1) + '" y="' + (h - 6) + '">' +
      esc(r.window) + '</text>';
    return s;
  }).join('');
  return '<svg class="chart auto" viewBox="0 0 ' + w + ' ' + h + '" role="img" ' +
    'aria-label="tokens burned per rolling window">' + bars + '</svg>';
}

// ---------------------------------------------------------- view: reconcile

// loadReconcile shows the two sources' figures side by side, never combined.
// computed and billed are different sources' answers about the same usage, so
// they are two labelled series on one scale -- not two halves of a total
// (invariant 5). A row with no computed figure draws only the billed bar;
// there is no zero to draw, and a zero-height bar would claim there is.
async function loadReconcile() {
  const { body } = await api('/api/reconcile');
  const rows = body.rows || [];
  $('reconcile-scope').textContent = body.scope || '';

  cards($('reconcile-cards'), [
    { label: 'drift threshold', value: fmtUSD(body.threshold_usd) },
    { label: 'rows compared', value: fmtInt(rows.length) },
    { label: 'drifted', value: fmtInt(rows.filter((r) => r.cost_drift).length) },
    { label: 'source mismatches', value: fmtInt(body.source_mismatch_count) },
  ]);
  $('reconcile-chart').innerHTML = chartReconcile(rows);

  table($('reconcile-table'), [
    { head: 'day', cell: (r) => esc(fmtTime(r.day)) },
    { head: 'model', cell: (r) => esc(r.model) },
    { head: 'computed (ours)', cell: (r) => fmtUSD(r.computed_usd) },
    { head: 'billed (admin)', cell: (r) => fmtUSD(r.billed_usd) },
    { head: 'diverged', cell: (r) => fmtUSD(r.diverged_usd) },
    { head: 'drift', cell: (r) => (r.cost_drift ? 'yes' : 'no') },
  ], rows, 'nothing billed yet — the Admin cost report is the billing side',
    (r) => (r.cost_drift ? 'row-bad' : ''));
}

// chartReconcile draws computed and billed as two bars per row. Both series
// share one scale, so the two heights are directly comparable, and they are
// never stacked: stacking would draw a sum neither source reports. Only a row
// with a computed figure gets a computed bar.
function chartReconcile(rows) {
  const drawable = (rows || []).filter((r) => r.billed_usd || r.computed_usd !== null);
  if (!drawable.length) return '';
  const w = 640, h = 140, gap = 10;
  const slot = (w - gap * (drawable.length - 1)) / drawable.length;
  const bw = Math.max(3, (slot - 4) / 2);
  const top = Math.max.apply(null, drawable.map((r) => Math.max(r.billed_usd || 0, r.computed_usd || 0))) || 1;
  const bars = drawable.map((r, i) => {
    const x = i * (slot + gap);
    const scale = (v) => Math.max(1, Math.round((v / top) * (h - 26)));
    let s = '<rect x="' + x.toFixed(1) + '" y="' + (h - scale(r.billed_usd)) + '" width="' + bw.toFixed(1) +
      '" height="' + scale(r.billed_usd) + '"><title>billed ' + esc(fmtUSD(r.billed_usd)) + '</title></rect>';
    if (r.computed_usd !== null && r.computed_usd !== undefined) {
      s += '<rect class="sw-computed" x="' + (x + bw + 2).toFixed(1) + '" y="' + (h - scale(r.computed_usd)) +
        '" width="' + bw.toFixed(1) + '" height="' + scale(r.computed_usd) + '"><title>computed ' +
        esc(fmtUSD(r.computed_usd)) + '</title></rect>';
    }
    s += '<text class="chart-label" x="' + (x + slot / 2).toFixed(1) + '" y="' + (h - 8) + '">' +
      esc(r.model.replace(/^claude-/, '').slice(0, 10)) + '</text>';
    return s;
  }).join('');
  return '<h3>Computed vs billed</h3><div class="legend">' +
    '<span><svg class="sw" viewBox="0 0 10 10"><rect width="10" height="10"/></svg>billed (admin report)</span>' +
    '<span><svg class="sw" viewBox="0 0 10 10"><rect class="sw-computed" width="10" height="10"/></svg>computed (ours)</span>' +
    '</div><svg class="chart auto" viewBox="0 0 ' + w + ' ' + h + '" role="img" ' +
    'aria-label="computed versus billed cost per day and model">' + bars + '</svg>';
}

// --------------------------------------------------------------- view: models

// loadModels lists every model the catalogue knows plus every one traffic used,
// each with a coverage label. An unpriced model is a labelled gap: this table
// has no cost column at all, so there is no cell a $0.00 could land in.
async function loadModels() {
  const { body } = await api('/api/models');
  table($('models-table'), [
    { head: 'model', cell: (m) => esc(m.model) + (m.display_name ? ' <span class="muted">' + esc(m.display_name) + '</span>' : '') },
    { head: 'known from', cell: (m) => esc(m.source) },
    { head: 'pricing', cell: (m) => '<span class="label-' + esc(m.pricing) + '">' + esc(m.pricing) + '</span>' +
        (m.rate_source && m.rate_source !== m.pricing ? ' <span class="muted">' + esc(m.rate_source) + '</span>' : '') },
    { head: 'requests', cell: (m) => fmtInt(m.requests) },
    { head: 'priced', cell: (m) => fmtInt(m.priced_count) },
    { head: 'unpriced', cell: (m) => fmtInt(m.unpriced_count) },
  ], body.models || [], 'the catalogue is empty',
    (m) => (m.pricing === 'unpriced' && m.requests > 0 ? 'row-warn' : ''));
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
  sources: loadSources,
  quota: loadQuota,
  reconcile: loadReconcile,
  models: loadModels,
  settings: loadSettings,
};

let current = 'overview';

// reveal switches the tab and un-hides the section -- the half of show() that
// does no fetching. The drill-down needs exactly this and nothing more: it is
// about to render one call, and a list of fifty is a fetch nobody sees.
function reveal(view) {
  detailSeq++; // take a fresh generation: any detail fetch still in flight is now stale (D10)
  current = view;
  document.querySelectorAll('#tabs .tab').forEach((b) => {
    b.classList.toggle('active', b.dataset.view === view);
  });
  document.querySelectorAll('.view').forEach((s) => {
    s.hidden = s.id !== 'view-' + view;
  });
  setStatus('');
}

async function show(view) {
  reveal(view);
  // Both details close on ANY tab click, unconditionally. A reset guarded by the
  // tab's own name would leave a detail open when the user leaves via Overview
  // or Warnings -- and both of those render their own data-call links,
  // so the next drill-down would reveal the previous call's request and response
  // bodies under a click for a different call until the fetch resolved.
  setCallDetail(false);
  setSessionDetail(false);
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
    // Reveal the Calls view without running its loader: the detail is what was
    // asked for, and fetching fifty rows only to hide them is a fetch nobody
    // sees. reveal() has just taken this drill-down's generation (D10), so a
    // late response from an earlier drill-down cannot land in #call-detail.
    reveal('calls');
    const seq = detailSeq;
    try {
      await showCall(call.dataset.call, seq);
    } catch (err) {
      // Only report a failure that is still current. A stale rejection must not
      // pull the user back to a view they have already left, and the fallback
      // below would bump the generation and take a newer detail down with it.
      if (seq === detailSeq) {
        await show('calls');
        setStatus(err.message, true);
      }
    }
    return;
  }
  const sess = ev.target.closest('[data-session]');
  if (sess) {
    ev.preventDefault();
    // No view change here, so this drill-down takes its generation directly.
    const seq = ++detailSeq;
    try {
      await showSession(sess.dataset.session, seq);
    } catch (err) {
      // Same symmetry as the [data-call] branch: only a failure that is still
      // current writes the status line; a stale one must not overwrite a newer
      // detail's.
      if (seq === detailSeq) setStatus(err.message, true);
    }
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
$('q-apply').addEventListener('click', loadQuota);

// The collect button is the one place the dashboard makes outbound calls (to
// claude.ai and the Admin API) rather than reading what is already stored, so
// it is a button and not something a view load does on its own. Re-reading
// /api/sources afterwards is what proves the run happened -- and shows a
// collector that failed during it as the red row it is.
$('src-collect').addEventListener('click', async (ev) => {
  ev.target.disabled = true;
  setStatus('collecting…');
  try {
    await api('/api/ingest', { method: 'POST' });
    setStatus('');
  } catch (err) {
    // A 500 here means the run finished with a failing collector, whose detail
    // is on the table below. The status line says so rather than swallowing it.
    setStatus(err.message, true);
  } finally {
    ev.target.disabled = false;
    await loadSources();
  }
});

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

// The proxy-mode badge: whether Claude Code is actually pointed at this
// process, and whether this process is actually receiving. Neither half alone
// is the answer -- "configured" reads fine while the proxy is dead, and
// "receiving" reads fine while the client is pointed somewhere else.
//
// The label is rendered server-side and printed verbatim. The state -> label
// grid is a pure Go function in internal/api/mode.go, which is what makes it
// testable at all: there is no JS runtime in this toolchain, so a mapping here
// could only be asserted by a source-shape check.
async function loadProxyMode() {
  const el = $('proxy-mode');
  try {
    const { body } = await api('/api/mode');
    el.textContent = body.Badge;
    el.dataset.state = body.Configured + '/' + (body.Observed ? 'on' : 'off');
  } catch (err) {
    // Fail open, like loadTotals -- but clear it rather than leaving the last
    // label up: a stale "proxy: active" is worse than no badge, because it is
    // the exact false reassurance this exists to prevent.
    el.textContent = '';
    delete el.dataset.state;
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
      await Promise.all([loadTotals(), loadProxyMode(), loaders[current]()]);
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
loadProxyMode();
show('overview');
subscribe();
