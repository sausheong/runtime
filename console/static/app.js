async function loadSessions() {
  const res = await fetch(`/agents/${AGENT}/sessions`, {credentials: 'same-origin'});
  const rows = await res.json();
  const tb = document.querySelector('#sessions tbody');
  tb.innerHTML = '';
  (rows || []).forEach(s => {
    const tr = document.createElement('tr');
    tr.innerHTML = `<td><a href="/ui/agents/${AGENT}/sessions/${s.id}">${s.id}</a></td>`
      + `<td>${s.status}</td><td>${s.turn_count}</td>`;
    tb.appendChild(tr);
  });
}

function streamSession() {
  const out = document.getElementById('events');
  const es = new EventSource(`/agents/${AGENT}/sessions/${SID}/stream?since=0`, {withCredentials: true});
  es.onmessage = e => { out.textContent += e.data + "\n"; };
  es.onerror = () => { es.close(); };
}
