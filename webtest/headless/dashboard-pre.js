// Fake server for webtest/headless/run-dashboard.sh: a headless check of the
// dashboard (web/index.html + app.js) as a super admin, with the API answered
// in the page. No network, no real server.
window.__calls = [];
window.__errors = [];
window.addEventListener('error', (e) => window.__errors.push(String(e.message)));
window.addEventListener('unhandledrejection', (e) => window.__errors.push('rejection: ' + String(e.reason && e.reason.message || e.reason)));
window.Chart = function () { return { update() {}, destroy() {}, data: { labels: [], datasets: [{ data: [] }, { data: [] }] }, options: {} }; };
window.QRCode = function () {};
window.confirm = () => true;
const json = (v, status) => new Response(JSON.stringify(v), { status: status || 200, headers: { 'Content-Type': 'application/json' } });
const PLAN = {
  source: { server_version: 'v4.8.0', exported_at: '2026-11-26T01:00:00Z', exported_by: 'root', scope: '', has_identity_key: true },
  plan: {
    counts: { agency_create: 1, user_new: 1, user_replace: 0, user_skip: 1, dashboard_skip: 1 },
    identity: { in_file: true, fingerprint: 'aaaabbbbccccdddd1111', current: 'eeeeffff00001111', replace: false, same: false },
    items: [
      { kind: 'agency', agency: 'fire', name: 'Fire <b>Dept</b>', action: 'create' },
      { kind: 'user', agency: 'fire', name: 'medic', display: 'Medic "One"', role: 'user', phones: 1, action: 'new' },
      { kind: 'user', agency: 'fire', name: '<img src=x onerror=window.__xss=1>', role: 'user', action: 'skip', reason: 'not valid: username must not contain "<"' },
      { kind: 'dashboard', agency: 'fire', name: 'chief', role: 'agency_admin', action: 'skip', reason: 'dashboard accounts are not imported (option off)' }
    ]
  }
};
window.fetch = async (url, opts) => {
  url = String(url);
  const method = (opts && opts.method) || 'GET';
  window.__calls.push(method + ' ' + url);
  if (url.startsWith('/api/session')) return json({ authenticated: true, valid: true, role: 'super_admin', agency_id: '', username: 'root', ptt_port: 5000 });
  if (url.startsWith('/api/agencies')) return json([{ id: 'fire', name: 'Fire Dept' }, { id: 'ems', name: 'EMS' }]);
  if (url.startsWith('/api/user-migration/export')) {
    window.__exportBody = JSON.parse(opts.body);
    return new Response('{"format":"meshrelay-user-export"}', { status: 200, headers: { 'Content-Disposition': 'attachment; filename="meshrelay-users-all-test.mrusers"' } });
  }
  if (url.startsWith('/api/user-migration/preview')) {
    const f = opts.body; window.__previewForm = {}; for (const [k, v] of f.entries()) window.__previewForm[k] = typeof v === 'string' ? v : 'FILE:' + v.name;
    if (window.__previewForm.passphrase === 'wrong') return json({ error: 'wrong passphrase, or the file was changed or damaged' }, 400);
    return json(PLAN);
  }
  if (url.startsWith('/api/user-migration/import')) {
    return json(Object.assign({ status: 'ok', agencies_created: 1, users: 1, dashboard: 0, replaced: 0, skipped: 2, sessions_ended: 0, identity_replaced: false, backups: [] }, PLAN));
  }
  if (url.startsWith('/api/tak-master/home')) {
    window.__homeBody = JSON.parse(opts.body);
    return json({ configured: true, status: 'connected', home_agency: window.__homeBody.agency_id, effective_home_agency: window.__homeBody.agency_id,
      home_agency_how: 'chosen', agencies: [{ id: 'ems', name: 'EMS' }, { id: 'fire', name: 'Fire <b>Dept</b>' }],
      sharing: [{ agency_id: 'ems', name: 'EMS', same_server: false, home: window.__homeBody.agency_id === 'ems' }] });
  }
  if (url.startsWith('/api/tak-master')) {
    if (method === 'PUT') window.__takMasterBody = JSON.parse(opts.body);
    return json({ configured: true, host: 'tak.example.org', stream_port: 8089, enroll_port: 8446, api_port: 8443, username: 'svc',
      callsign: 'MeshRelay', enabled: true, enrolled: true, status: 'connected', tak_users: 3,
      sharing: [{ agency_id: 'fire', name: 'Fire <b>Dept</b>', same_server: true, home: true }, { agency_id: 'ems', name: 'EMS', same_server: false }],
      home_agency: '', effective_home_agency: 'fire', home_agency_how: 'fallback',
      agencies: [{ id: 'ems', name: 'EMS' }, { id: 'fire', name: 'Fire <b>Dept</b>' }] });
  }
  if (url.startsWith('/api/my-agency/tak/share-master')) {
    window.__shareBody = JSON.parse(opts.body);
    return json({ agency_id: 'fire', configured: true, share_master: window.__shareBody.share, master: { configured: true, status: 'connected', home_agency_name: 'EMS' }, same_server_as_master: true, master_home: false });
  }
  if (url.startsWith('/api/my-agency/tak/groups')) return json({ groups: [], selected: [] });
  if (url.startsWith('/api/my-agency/tak')) {
    return json({ agency_id: 'fire', configured: true, host: 'tak.example.org', stream_port: 8089, enrolled: false, status: 'connected',
      share_master: false, master: { configured: true, status: 'connected', home_agency_name: 'EMS' }, same_server_as_master: true, master_home: false });
  }
  if (url.startsWith('/api/users')) return json([]);
  if (url.startsWith('/api/activity')) return json([]);
  return json({});
};
class FakeWS { constructor() { this.readyState = 0; } send() {} close() {} }
FakeWS.OPEN = 1;
window.WebSocket = FakeWS;
