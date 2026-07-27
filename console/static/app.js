// Status string -> badge variant class (parity with the server-rendered badges).
function statusBadge(status) {
  const map = {completed: 'badge-ok', running: 'badge-muted', created: 'badge-muted', error: 'badge-danger'};
  const span = document.createElement('span');
  span.className = 'badge ' + (map[status] || 'badge-muted');
  span.textContent = status || 'unknown';
  return span;
}

// Replace the #sessions tbody with a single full-width state row (loading/empty/error).
function sessionsStateRow(tb, text) {
  tb.replaceChildren();
  const tr = document.createElement('tr');
  const td = document.createElement('td');
  td.colSpan = 5;
  td.className = 'empty';
  td.textContent = text;
  tr.appendChild(td);
  tb.appendChild(tr);
}

function fmtCompleted(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  const dd  = String(d.getDate()).padStart(2, '0');
  const mmm = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'][d.getMonth()];
  const yyyy = d.getFullYear();
  const hh  = String(d.getHours()).padStart(2, '0');
  const min = String(d.getMinutes()).padStart(2, '0');
  const ss  = String(d.getSeconds()).padStart(2, '0');
  return `${dd}/${mmm}/${yyyy} ${hh}:${min}:${ss}`;
}

function fmtDuration(ms) {
  if (ms == null) return '—';
  if (ms < 1000) return ms + ' ms';
  return (ms / 1000).toFixed(1) + ' s';
}

async function loadSessions() {
  const tb = document.querySelector('#sessions tbody');
  if (!tb) return;
  try {
    const res = await fetch(`/agents/${AGENT}/sessions`, {credentials: 'same-origin'});
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const rows = await res.json();
    if (!rows || rows.length === 0) {
      sessionsStateRow(tb, 'No sessions yet.');
      return;
    }
    // Build with safe DOM APIs (no innerHTML): session fields are agent-supplied
    // and must not be interpolated into markup.
    tb.replaceChildren();
    rows.forEach(s => {
      const tr = document.createElement('tr');

      const idCell = document.createElement('td');
      const a = document.createElement('a');
      a.className = 'mono';
      a.href = `/ui/agents/${encodeURIComponent(AGENT)}/sessions/${encodeURIComponent(s.id)}`;
      a.textContent = s.id;
      idCell.appendChild(a);

      const statusCell = document.createElement('td');
      statusCell.appendChild(statusBadge(s.status));

      const turnsCell = document.createElement('td');
      turnsCell.textContent = (s.turn_count ?? 0);

      const completedCell = document.createElement('td');
      completedCell.textContent = fmtCompleted(s.completed_at);

      const durationCell = document.createElement('td');
      durationCell.textContent = fmtDuration(s.duration_ms);

      tr.append(idCell, statusCell, turnsCell, completedCell, durationCell);
      tb.appendChild(tr);
    });
  } catch (e) {
    sessionsStateRow(tb, 'Could not load sessions. Refresh to retry.');
  }
}

// The transcript had exactly one appearance for three different situations:
// connecting, connected-but-silent, and stream-dead all rendered as the same
// empty dark slab, because onerror closed the connection without saying so. An
// operator opens this page specifically to find out what an agent did, so "no
// events yet" and "you are no longer receiving events" must not look alike.
//
// The state line sits ABOVE the <pre> rather than inside it: text written into
// the transcript would be indistinguishable from a log line the agent emitted.
function streamSession() {
  const out = document.getElementById('events');
  const state = document.getElementById('stream-state');
  let received = 0;

  const setState = (text, kind) => {
    if (!state) return;
    state.textContent = text;
    state.className = 'stream-state' + (kind ? ' is-' + kind : '');
  };

  setState('Connecting to the session stream…');

  const es = new EventSource(`/agents/${AGENT}/sessions/${SID}/stream?since=0`, {withCredentials: true});

  es.onopen = () => {
    // Open with nothing replayed yet is the ambiguous case the old code could
    // not express: the session exists, we are attached, there is just no output.
    setState(received ? `Live — ${received} events` : 'Connected. No events recorded for this session yet.', 'live');
  };

  es.onmessage = e => {
    out.textContent += e.data + "\n";
    received++;
    setState(`Live — ${received} event${received === 1 ? '' : 's'}`, 'live');
  };

  es.onerror = () => {
    es.close();
    // Distinguish "never connected" from "was connected and dropped": the first
    // is usually a wrong session id or an agent that is down, the second means
    // the transcript above is real but has stopped updating.
    setState(received
      ? `Stream disconnected after ${received} events. Refresh to reconnect.`
      : 'Could not connect to the session stream. The agent may be unreachable. Refresh to retry.', 'error');
  };
}
