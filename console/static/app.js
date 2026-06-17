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
  td.colSpan = 3;
  td.className = 'empty';
  td.textContent = text;
  tr.appendChild(td);
  tb.appendChild(tr);
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

      tr.append(idCell, statusCell, turnsCell);
      tb.appendChild(tr);
    });
  } catch (e) {
    sessionsStateRow(tb, 'Could not load sessions. Refresh to retry.');
  }
}

function streamSession() {
  const out = document.getElementById('events');
  const es = new EventSource(`/agents/${AGENT}/sessions/${SID}/stream?since=0`, {withCredentials: true});
  es.onmessage = e => { out.textContent += e.data + "\n"; };
  es.onerror = () => { es.close(); };
}
