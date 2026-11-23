// Fake server for webtest/headless/run.sh: a headless check of the console's emergency banner/alarm.
window.__log = [];
window.__server = { active: new Map(), sockets: [] };
window.fetch = async (url) => {
  if (String(url).startsWith('/api/session')) return new Response(JSON.stringify({ role: 'dispatch', agency_id: 'ag', username: 'disp' }), { status: 200 });
  return new Response('[]', { status: 200 });
};
try { navigator.mediaDevices.getUserMedia = () => Promise.reject(new Error('test')); } catch (_) {}
const UPDATE = { type: 'update', data: {
  agencies: [{ id: 'ag', name: 'Test County' }],
  channels: [{ agency: 'ag', agency_name: 'Test County', channel: 2, name: 'Fire Dispatch', kind: 'channel' }],
  clients: [{ agency_id: 'ag', username: 'Engine1', channel: 2 }, { agency_id: 'ag', username: 'Medic5', channel: 2, location: { lat: 41.7, lon: -74.8 } }],
  tak_units: [] } };
class FakeWS {
  constructor(url) { this.url = url; this.readyState = 0; window.__server.sockets.push(this); window.__wsCount = (window.__wsCount || 0) + 1;
    setTimeout(() => { this.readyState = 1; this.onopen && this.onopen();
      this.push(UPDATE);
      for (const m of window.__server.active.values()) this.push(m);   // as web.go does on connect
    }, 20); }
  push(obj) { if (this.readyState === 1 && this.onmessage) this.onmessage({ data: JSON.stringify(obj) }); }
  send() {}
  close() { this.readyState = 3; }
  drop() { this.readyState = 3; this.onclose && this.onclose(); }
}
FakeWS.OPEN = 1; FakeWS.CONNECTING = 0;
window.WebSocket = FakeWS;
window.__emit = (m) => {               // server-side sendToConsoles + rememberEmergency
  const k = m.agency + '|' + m.user;
  if (m.active) window.__server.active.set(k, m); else window.__server.active.delete(k);
  for (const s of window.__server.sockets) s.push(m);
};
if (location.hash === '#reload') {
  window.__server.active.set('ag|Medic5', { type: 'emergency', agency: 'ag', channel: 2, user: 'Medic5', active: true, at: Date.now() - 60000, lat: 41.7, lon: -74.8 });
  window.__server.active.set('ag|Bravo (TAK)', { type: 'emergency', agency: 'ag', channel: -1, user: 'Bravo (TAK)', active: true, tak: true, alert: '911 Alert', at: Date.now() - 30000 });
}
