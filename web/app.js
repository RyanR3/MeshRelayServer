// WebSocket connection
let ws = null;
let reconnectInterval = null;
let reconnectAttempts = 0;

// Currently editing user
let editingUsername = '';

// Current session info
let currentRole = '';
let currentAgencyId = '';
let pttPort = 5000;
let agenciesList = []; // cached for dropdowns
let connectedClients = []; // updated by WebSocket for online status
let allUsers = []; // full user list for pagination
let currentUsersPage = 1;
const USERS_PER_PAGE = 10;
let lastStats = {}; // cache latest stats for system health panel
let networkChart = null;
let activityChart = null;
let lastHealth = null;

// --- Sidebar Collapse ---

function toggleSidebar() {
    const sidebar = document.getElementById('sidebar');
    sidebar.classList.toggle('collapsed');
}

// --- Page Navigation ---

// --- Mobile drawer (phone widths only; see style.css "Mobile (phone) layout") ---
function openDrawer(open) {
    const sb = document.getElementById('sidebar');
    const bd = document.getElementById('sidebar-backdrop');
    if (!sb) return;
    sb.classList.toggle('open', open);
    if (bd) bd.classList.toggle('open', open);
}
document.addEventListener('DOMContentLoaded', () => {
    // Give every data table its own horizontal scroll box so a wide table
    // scrolls inside its card on a phone instead of widening the page.
    document.querySelectorAll('table.data-table').forEach(t => {
        if (t.parentElement.classList.contains('table-scroll')) return;
        const w = document.createElement('div');
        w.className = 'table-scroll';
        t.parentNode.insertBefore(w, t);
        w.appendChild(t);
    });
    const btn = document.getElementById('mobile-menu-btn');
    const bd = document.getElementById('sidebar-backdrop');
    if (btn) btn.addEventListener('click', () => openDrawer(!document.getElementById('sidebar').classList.contains('open')));
    if (bd) bd.addEventListener('click', () => openDrawer(false));
});

function navigateTo(pageName) {
    openDrawer(false);
    const nav = document.querySelector('.nav-item[data-page="' + pageName + '"] .nav-label');
    const title = document.getElementById('mobile-bar-title');
    if (title) title.textContent = nav ? nav.textContent.trim() : 'MeshRelay';
    // Update nav items
    document.querySelectorAll('.nav-item').forEach(item => {
        item.classList.toggle('active', item.dataset.page === pageName);
    });

    // Update pages
    document.querySelectorAll('.page').forEach(page => {
        page.classList.toggle('active', page.id === 'page-' + pageName);
    });

    // Refresh data when switching to certain pages
    if (pageName === 'agencies') {
        fetchAgencies();
    } else if (pageName === 'users') {
        fetchUsers();
    } else if (pageName === 'mutual-aid') {
        // Super admins keep the drag-and-drop builder (they can patch any two
        // agencies). Agency admins get discovery + request/approve instead.
        if (currentRole === 'super_admin') {
            document.getElementById('ma-agency-view').style.display = 'none';
            document.getElementById('ma-superadmin-view').style.display = '';
            fetchMutualAid();
        } else {
            document.getElementById('ma-superadmin-view').style.display = 'none';
            document.getElementById('ma-agency-view').style.display = '';
            fetchAgencyMutualAid();
        }
    } else if (pageName === 'maps') {
        initMapsPage();
    } else if (pageName === 'channels') {
        fetchAgencyChannels();
    } else if (pageName === 'emergency') {
        fetchAgencyEmergency();
    } else if (pageName === 'guest-events') {
        fetchGuestEvents();
    } else if (pageName === 'zello-bridges') {
        // super_admin manages every bridge on the server; an agency admin sees
        // only the bridges it owns or has been granted.
        if (currentRole === 'super_admin') {
            document.getElementById('zb-agency-view').style.display = 'none';
            document.getElementById('zb-superadmin-view').style.display = '';
            fetch('/api/agencies').then(r => r.json()).then(a => { agenciesList = a || []; }).catch(() => {});
            fetchZelloBridges();
        } else {
            document.getElementById('zb-superadmin-view').style.display = 'none';
            document.getElementById('zb-agency-view').style.display = '';
            fetchAgencyZelloBridges();
        }
    } else if (pageName === 'lmr-bridges') {
        if (currentRole === 'super_admin') {
            document.getElementById('lmr-agency-view').style.display = 'none';
            document.getElementById('lmr-superadmin-view').style.display = '';
            fetchLmrBridges();
        } else {
            document.getElementById('lmr-superadmin-view').style.display = 'none';
            document.getElementById('lmr-agency-view').style.display = '';
            fetchAgencyLmrBridges();
        }
    } else if (pageName === 'activity') {
        fetchActivityPage();
    } else if (pageName === 'settings') {
        fetchVersionInfo();
        if (currentRole === 'super_admin') initMigrationPanel();
        if (currentRole === 'super_admin' || currentRole === 'agency_admin') initFloorControlPanel();
    }
}

// --- Talk floor control (Settings) -----------------------------------------
// One-talker-per-channel arbitration is an agency setting. Agency admins see
// their own agency; super admins pick one from the dropdown.

function initFloorControlPanel() {
    const sel = document.getElementById('floor-control-agency');
    const guestSel = document.getElementById('guest-invites-agency');
    const recSel = document.getElementById('recording-agency');
    const takSel = document.getElementById('tak-agency');
    if (currentRole === 'super_admin' && sel) {
        fetch('/api/agencies').then(r => r.json()).then(agencies => {
            agenciesList = agencies || [];
            const options = agenciesList.map(a =>
                `<option value="${escapeHtml(a.id)}">${escapeHtml(a.name)}</option>`).join('');
            [sel, guestSel, recSel, takSel].forEach(el => {
                if (!el) return;
                const current = el.value;
                el.innerHTML = options;
                if (current && agenciesList.some(a => a.id === current)) el.value = current;
            });
            fetchFloorControl();
            fetchGuestInvites();
            fetchRecordingSetting();
            fetchTranscription();
            fetchTak();
            fetchTakMaster();
        }).catch(() => { fetchFloorControl(); fetchGuestInvites(); fetchRecordingSetting(); fetchTak(); fetchTakMaster(); });
    } else {
        fetchFloorControl();
        fetchGuestInvites();
        fetchRecordingSetting();
        fetchTak();
    }
}

// --- TAK Server (Settings) ---------------------------------------------------

function takAgencyId() {
    const sel = document.getElementById('tak-agency');
    return (currentRole === 'super_admin' && sel && sel.value) ? sel.value : '';
}

function renderTakState(d) {
    const el = document.getElementById('tak-state');
    if (!el) return;
    if (!d.configured) { el.textContent = 'Not configured'; return; }
    const exp = d.cert_expires ? ` · certificate valid until ${new Date(d.cert_expires).toLocaleDateString()}` : '';
    const states = {
        connected: `Connected — ${d.tak_users || 0} TAK user${d.tak_users === 1 ? '' : 's'} seen`,
        connecting: 'Connecting…', disabled: 'Off', error: `Not connected: ${d.error || 'error'} (retrying)`,
    };
    el.textContent = (states[d.status] || d.status) + exp;
    el.style.color = d.status === 'connected' ? '#3FB950' : d.status === 'error' ? '#F85149' : '';
}

function fetchTak() {
    const host = document.getElementById('tak-host');
    if (!host) return;
    const agency = takAgencyId();
    fetch('/api/my-agency/tak' + (agency ? '?agency=' + encodeURIComponent(agency) : ''))
        .then(r => r.json())
        .then(d => {
            if (d.error) { showStatus('tak-status-msg', d.error, 'error'); return; }
            host.value = d.host || '';
            document.getElementById('tak-stream-port').value = d.stream_port || 8089;
            document.getElementById('tak-enroll-port').value = d.enroll_port || 8446;
            document.getElementById('tak-api-port').value = d.api_port || 8443;
            const gs = document.getElementById('tak-groups-section');
            gs.style.display = d.enrolled ? 'block' : 'none';
            if (d.enrolled && d.status === 'connected') loadTakGroups();
            else if (d.enrolled) document.getElementById('tak-groups-list').innerHTML =
                '<span style="color:#8B949E;font-size:12px;">Groups load once the connection is up.</span>';
            document.getElementById('tak-username').value = d.username || '';
            document.getElementById('tak-password').value = '';
            document.getElementById('tak-callsign').value = d.callsign || 'MeshRelay Dispatch';
            document.getElementById('tak-enabled').checked = d.configured ? !!d.enabled : true;
            document.getElementById('tak-password').placeholder = d.enrolled ? 'only to re-enroll' : 'needed to enroll';
            renderTakState(d);
            renderTakShare(d);
        })
        .catch(err => showStatus('tak-status-msg', 'Failed to load: ' + err.message, 'error'));
}

// The agency's "Share with the master TAK server" switch, the master's
// status, and the same-server note. No master set up: the switch is hidden
// (unless it is still on, so it can be switched off).
const TAK_MASTER_STATES = { connected: 'connected', connecting: 'connecting…', disabled: 'switched off', error: 'not connected (retrying)' };
let takShareLocked = false;
function renderTakShare(d) {
    const master = d.master || {};
    const sw = document.getElementById('tak-share-switch');
    if (!sw) return;
    const box = document.getElementById('tak-share-master');
    box.checked = !!d.share_master;
    // The home agency always takes part: shown on, cannot be switched off.
    takShareLocked = !!d.master_home;
    box.disabled = takShareLocked;
    sw.title = d.master_home ? 'The home agency always shares with the master TAK server' : '';
    sw.style.display = (master.configured || d.share_master) ? '' : 'none';
    document.getElementById('tak-share-home').style.display = d.master_home ? '' : 'none';
    const routing = document.getElementById('tak-share-routing');
    if (master.configured && master.home_agency_name) {
        routing.textContent = 'Replies from TAK users come back to the agency that wrote to them. Messages from TAK users nobody has written to go to '
            + master.home_agency_name + '\'s console.';
        routing.style.display = '';
    } else {
        routing.style.display = 'none';
    }
    const state = document.getElementById('tak-share-state');
    state.textContent = master.configured
        ? 'Master TAK server: ' + (TAK_MASTER_STATES[master.status] || master.status || 'unknown')
        : 'Master TAK server: not set up';
    state.style.color = master.status === 'connected' ? '#3FB950' : master.status === 'error' ? '#F85149' : '';
    document.getElementById('tak-share-same').style.display = d.same_server_as_master ? '' : 'none';
}

function saveTakShareMaster() {
    const box = document.getElementById('tak-share-master');
    const body = { share: box.checked };
    const agency = takAgencyId();
    if (agency) body.agency_id = agency;
    box.disabled = true;
    fetch('/api/my-agency/tak/share-master', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
        .then(r => r.json())
        .then(d => {
            if (d.error) { showStatus('tak-status-msg', d.error, 'error'); box.checked = !body.share; return; }
            renderTakShare(d);
            showStatus('tak-status-msg', body.share ? 'Sharing with the master TAK server' : 'No longer sharing with the master TAK server', 'success');
            if (currentRole === 'super_admin') fetchTakMaster();
        })
        .catch(err => { box.checked = !body.share; showStatus('tak-status-msg', 'Failed: ' + err.message, 'error'); })
        .finally(() => { box.disabled = takShareLocked; });
}

// --- Master TAK server (Settings, super admin) ---------------------------------

function renderTakMasterState(d) {
    const el = document.getElementById('takm-state');
    if (!el) return;
    if (!d.configured) { el.textContent = 'Not configured'; el.style.color = ''; return; }
    const exp = d.cert_expires ? ` · certificate valid until ${new Date(d.cert_expires).toLocaleDateString()}` : '';
    const states = {
        connected: `Connected — ${d.tak_users || 0} TAK user${d.tak_users === 1 ? '' : 's'} seen`,
        connecting: 'Connecting…', disabled: 'Off', error: `Not connected: ${d.error || 'error'} (retrying)`,
    };
    el.textContent = (states[d.status] || d.status) + exp;
    el.style.color = d.status === 'connected' ? '#3FB950' : d.status === 'error' ? '#F85149' : '';
}

function renderTakMasterSharing(d) {
    const el = document.getElementById('takm-sharing');
    if (!el) return;
    const list = d.sharing || [];
    if (!list.length) { el.textContent = 'None. An agency admin switches sharing on in the agency\'s TAK settings.'; return; }
    el.innerHTML = list.map(a => `<div>${escapeHtml(a.name || a.agency_id)}${a.home
        ? ' <span style="color:#8B949E;">· home agency (always takes part)</span>' : ''}${a.same_server
        ? ' <span style="color:#8B949E;">· own TAK connection goes to this same server (sent once)</span>' : ''}</div>`).join('');
}

// The home agency selector: the agencies, with the one in effect selected.
function renderTakMasterHome(d) {
    const row = document.getElementById('takm-home-row');
    if (!row) return;
    row.style.display = d.configured ? '' : 'none';
    const sel = document.getElementById('takm-home');
    const eff = d.effective_home_agency || '';
    sel.innerHTML = (d.agencies || []).map(a => `<option value="${escapeHtml(a.id)}">${escapeHtml(a.name || a.id)}</option>`).join('');
    sel.value = eff;
    const name = ((d.agencies || []).find(a => a.id === eff) || {}).name || eff;
    document.getElementById('takm-home-state').textContent = eff
        ? 'In effect: ' + name + (d.home_agency_how === 'fallback' ? ' (not chosen yet: the default agency, or the first one)' : '')
        : 'No agencies';
}

function saveTakMasterHome() {
    const sel = document.getElementById('takm-home');
    sel.disabled = true;
    fetch('/api/tak-master/home', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ agency_id: sel.value }) })
        .then(r => r.json())
        .then(d => {
            if (d.error) { showStatus('takm-status-msg', d.error, 'error'); fetchTakMaster(); return; }
            renderTakMasterHome(d);
            renderTakMasterSharing(d);
            showStatus('takm-status-msg', 'Home agency saved', 'success');
            fetchTak();
        })
        .catch(err => showStatus('takm-status-msg', 'Failed: ' + err.message, 'error'))
        .finally(() => { sel.disabled = false; });
}

function fetchTakMaster() {
    const host = document.getElementById('takm-host');
    if (!host || currentRole !== 'super_admin') return;
    fetch('/api/tak-master')
        .then(r => r.json())
        .then(d => {
            if (d.error) { showStatus('takm-status-msg', d.error, 'error'); return; }
            host.value = d.host || '';
            document.getElementById('takm-stream-port').value = d.stream_port || 8089;
            document.getElementById('takm-enroll-port').value = d.enroll_port || 8446;
            document.getElementById('takm-api-port').value = d.api_port || 8443;
            document.getElementById('takm-username').value = d.username || '';
            document.getElementById('takm-password').value = '';
            document.getElementById('takm-password').placeholder = d.enrolled ? 'only to re-enroll' : 'needed to enroll';
            document.getElementById('takm-callsign').value = d.callsign || 'MeshRelay';
            document.getElementById('takm-enabled').checked = d.configured ? !!d.enabled : true;
            const gs = document.getElementById('takm-groups-section');
            gs.style.display = d.enrolled ? 'block' : 'none';
            if (d.enrolled && d.status === 'connected') loadTakMasterGroups();
            else if (d.enrolled) document.getElementById('takm-groups-list').innerHTML =
                '<span style="color:#8B949E;font-size:12px;">Groups load once the connection is up.</span>';
            renderTakMasterState(d);
            renderTakMasterSharing(d);
            renderTakMasterHome(d);
        })
        .catch(err => showStatus('takm-status-msg', 'Failed to load: ' + err.message, 'error'));
}

function saveTakMaster(trustFingerprint) {
    const body = {
        host: document.getElementById('takm-host').value.trim(),
        stream_port: parseInt(document.getElementById('takm-stream-port').value, 10) || 8089,
        enroll_port: parseInt(document.getElementById('takm-enroll-port').value, 10) || 8446,
        api_port: parseInt(document.getElementById('takm-api-port').value, 10) || 8443,
        username: document.getElementById('takm-username').value.trim(),
        password: document.getElementById('takm-password').value,
        callsign: document.getElementById('takm-callsign').value.trim(),
        enabled: document.getElementById('takm-enabled').checked,
    };
    if (trustFingerprint) body.trust_fingerprint = trustFingerprint;
    const btn = document.getElementById('takm-save-btn');
    btn.disabled = true; btn.textContent = body.password ? 'Enrolling…' : 'Saving…';
    fetch('/api/tak-master', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
        .then(r => r.json())
        .then(d => {
            if (d.needs_trust && d.certificate) {
                // Nothing has been sent yet: confirm the exact certificate,
                // then the password goes only to it (as for an agency link).
                const c = d.certificate;
                const fp = (c.fingerprint || '').toUpperCase().match(/.{1,2}/g).join(':');
                const okToTrust = confirm('Confirm the TAK server\'s certificate before your password is sent.\n\n'
                    + 'Server: ' + body.host + ':' + body.enroll_port + '\n'
                    + 'Issued to: ' + c.subject + '\nIssued by: ' + c.issuer + '\nExpires: ' + new Date(c.not_after).toLocaleDateString() + '\n\n'
                    + 'SHA-256 fingerprint:\n' + fp + '\n\n'
                    + 'If you can, compare the fingerprint with the TAK server administrator. Continue only if this is your TAK server.');
                if (okToTrust) { saveTakMaster(c.fingerprint); } else { showStatus('takm-status-msg', 'Not connected: certificate not confirmed', 'error'); }
                return;
            }
            if (d.error) { showStatus('takm-status-msg', d.error, 'error'); return; }
            document.getElementById('takm-password').value = '';
            renderTakMasterState(d);
            renderTakMasterSharing(d);
            renderTakMasterHome(d);
            showStatus('takm-status-msg', d.status === 'connected' ? 'Connected to the master TAK Server' : 'Saved', d.status === 'error' ? 'error' : 'success');
            setTimeout(() => { fetchTakMaster(); fetchTak(); }, 4000);
        })
        .catch(err => showStatus('takm-status-msg', 'Failed: ' + err.message, 'error'))
        .finally(() => { btn.disabled = false; btn.textContent = 'Save & Connect'; });
}

function removeTakMaster() {
    if (!confirm('Disconnect from the master TAK Server and forget its certificate? Markers and alerts it carries are withdrawn.')) return;
    fetch('/api/tak-master', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ remove: true }) })
        .then(r => r.json())
        .then(d => {
            if (d.error) { showStatus('takm-status-msg', d.error, 'error'); return; }
            fetchTakMaster(); fetchTak();
            showStatus('takm-status-msg', 'Master TAK connection removed', 'success');
        })
        .catch(err => showStatus('takm-status-msg', 'Failed: ' + err.message, 'error'));
}

function loadTakMasterGroups() {
    const list = document.getElementById('takm-groups-list');
    list.innerHTML = '<span style="color:#8B949E;font-size:12px;">Loading…</span>';
    fetch('/api/tak-master/groups')
        .then(r => r.json())
        .then(d => {
            if (d.error) { list.innerHTML = `<span style="color:#F85149;font-size:12px;">${escapeHtml(d.error)}</span>`; return; }
            const groups = d.groups || [];
            if (!groups.length) { list.innerHTML = '<span style="color:#8B949E;font-size:12px;">This TAK account is not in any groups.</span>'; return; }
            list.innerHTML = groups.map(g => `<label><input type="checkbox" value="${escapeHtml(g.name)}" ${g.active ? 'checked' : ''}>
                <span>${escapeHtml(g.name)}${g.description ? `<small>${escapeHtml(g.description)}</small>` : ''}</span></label>`).join('');
        })
        .catch(err => { list.innerHTML = `<span style="color:#F85149;font-size:12px;">${escapeHtml(err.message)}</span>`; });
}

function saveTakMasterGroups() {
    const groups = [...document.querySelectorAll('#takm-groups-list input:checked')].map(i => i.value);
    const btn = document.getElementById('takm-groups-save');
    btn.disabled = true;
    fetch('/api/tak-master/groups', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ groups }) })
        .then(r => r.json())
        .then(d => {
            if (d.error) { showStatus('takm-status-msg', d.error, 'error'); return; }
            showStatus('takm-status-msg', `The master connection is now in: ${groups.join(', ')}`, 'success');
            loadTakMasterGroups();
        })
        .catch(err => showStatus('takm-status-msg', 'Failed: ' + err.message, 'error'))
        .finally(() => { btn.disabled = false; });
}

function saveTak(trustFingerprint) {
    const body = {
        host: document.getElementById('tak-host').value.trim(),
        stream_port: parseInt(document.getElementById('tak-stream-port').value, 10) || 8089,
        enroll_port: parseInt(document.getElementById('tak-enroll-port').value, 10) || 8446,
        api_port: parseInt(document.getElementById('tak-api-port').value, 10) || 8443,
        username: document.getElementById('tak-username').value.trim(),
        password: document.getElementById('tak-password').value,
        callsign: document.getElementById('tak-callsign').value.trim(),
        enabled: document.getElementById('tak-enabled').checked,
    };
    const agency = takAgencyId();
    if (agency) body.agency_id = agency;
    const btn = document.getElementById('tak-save-btn');
    btn.disabled = true; btn.textContent = body.password ? 'Enrolling…' : 'Saving…';
    if (trustFingerprint) body.trust_fingerprint = trustFingerprint;
    fetch('/api/my-agency/tak', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
        .then(r => r.json())
        .then(d => {
            if (d.needs_trust && d.certificate) {
                // The TAK server's certificate is not publicly trusted (normal
                // for TAK). Nothing has been sent yet: the admin confirms the
                // exact certificate, then the password goes only to it.
                const c = d.certificate;
                const fp = (c.fingerprint || '').toUpperCase().match(/.{1,2}/g).join(':');
                const okToTrust = confirm('Confirm the TAK server\'s certificate before your password is sent.\n\n'
                    + 'Server: ' + body.host + ':' + body.enroll_port + '\n'
                    + 'Issued to: ' + c.subject + '\nIssued by: ' + c.issuer + '\nExpires: ' + new Date(c.not_after).toLocaleDateString() + '\n\n'
                    + 'SHA-256 fingerprint:\n' + fp + '\n\n'
                    + 'If you can, compare the fingerprint with the TAK server administrator. Continue only if this is your TAK server.');
                if (okToTrust) { saveTak(c.fingerprint); } else { showStatus('tak-status-msg', 'Not connected: certificate not confirmed', 'error'); }
                return;
            }
            if (d.error) { showStatus('tak-status-msg', d.error, 'error'); return; }
            document.getElementById('tak-password').value = '';
            renderTakState(d);
            showStatus('tak-status-msg', d.status === 'connected' ? 'Connected to TAK Server' : 'Saved', d.status === 'error' ? 'error' : 'success');
            setTimeout(fetchTak, 4000);
        })
        .catch(err => showStatus('tak-status-msg', 'Failed: ' + err.message, 'error'))
        .finally(() => { btn.disabled = false; btn.textContent = 'Save & Connect'; });
}

function loadTakGroups() {
    const list = document.getElementById('tak-groups-list');
    const agency = takAgencyId();
    list.innerHTML = '<span style="color:#8B949E;font-size:12px;">Loading…</span>';
    fetch('/api/my-agency/tak/groups' + (agency ? '?agency=' + encodeURIComponent(agency) : ''))
        .then(r => r.json())
        .then(d => {
            if (d.error) { list.innerHTML = `<span style="color:#F85149;font-size:12px;">${escapeHtml(d.error)}</span>`; return; }
            const groups = d.groups || [];
            if (!groups.length) { list.innerHTML = '<span style="color:#8B949E;font-size:12px;">This TAK account is not in any groups.</span>'; return; }
            list.innerHTML = groups.map(g => `<label><input type="checkbox" value="${escapeHtml(g.name)}" ${g.active ? 'checked' : ''}>
                <span>${escapeHtml(g.name)}${g.description ? `<small>${escapeHtml(g.description)}</small>` : ''}</span></label>`).join('');
        })
        .catch(err => { list.innerHTML = `<span style="color:#F85149;font-size:12px;">${escapeHtml(err.message)}</span>`; });
}

function saveTakGroups() {
    const groups = [...document.querySelectorAll('#tak-groups-list input:checked')].map(i => i.value);
    const body = { groups };
    const agency = takAgencyId();
    if (agency) body.agency_id = agency;
    const btn = document.getElementById('tak-groups-save');
    btn.disabled = true;
    fetch('/api/my-agency/tak/groups', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
        .then(r => r.json())
        .then(d => {
            if (d.error) { showStatus('tak-status-msg', d.error, 'error'); return; }
            showStatus('tak-status-msg', `Dispatch is now in: ${groups.join(', ')}`, 'success');
            loadTakGroups();
        })
        .catch(err => showStatus('tak-status-msg', 'Failed: ' + err.message, 'error'))
        .finally(() => { btn.disabled = false; });
}

function removeTak() {
    if (!confirm('Disconnect from the TAK Server and forget its certificate?')) return;
    const body = { remove: true };
    const agency = takAgencyId();
    if (agency) body.agency_id = agency;
    fetch('/api/my-agency/tak', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
        .then(r => r.json()).then(d => { renderTakState(d); fetchTak(); showStatus('tak-status-msg', 'TAK connection removed', 'success'); });
}

// --- Radio recording (Settings) ----------------------------------------------

function recordingAgencyId() {
    const sel = document.getElementById('recording-agency');
    return (currentRole === 'super_admin' && sel && sel.value) ? sel.value : '';
}

function fetchRecordingSetting() {
    const toggle = document.getElementById('recording-toggle');
    if (!toggle) return;
    const agency = recordingAgencyId();
    fetch('/api/my-agency/recording' + (agency ? '?agency=' + encodeURIComponent(agency) : ''))
        .then(r => r.json())
        .then(data => {
            if (data.error) { showStatus('recording-status', data.error, 'error'); return; }
            toggle.checked = !!data.enabled;
            document.getElementById('recording-retention').textContent = data.retention_days;
            const ri = document.getElementById('recording-retention-input');
            if (ri) {
                ri.value = data.agency_retention_days || '';
                ri.placeholder = data.server_retention_days;
                document.getElementById('recording-server-retention').textContent = data.server_retention_days;
            }
            const tr = document.getElementById('transcribe-toggle');
            if (tr) {
                tr.checked = !!data.transcribe;
                tr.disabled = !data.enabled || (!data.transcription_available && !data.transcribe);
                document.getElementById('transcribe-unavailable').style.display = data.transcription_available ? 'none' : 'inline';
                document.getElementById('transcribe-words-row').style.display = data.transcribe ? 'flex' : 'none';
                document.getElementById('transcribe-words').value = data.transcribe_words || '';
            }
        })
        .catch(err => showStatus('recording-status', 'Failed to load: ' + err.message, 'error'));
}

function saveRecordingSetting() {
    const toggle = document.getElementById('recording-toggle');
    const body = { enabled: toggle.checked };
    const agency = recordingAgencyId();
    if (agency) body.agency_id = agency;
    fetch('/api/my-agency/recording', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body)
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('recording-status', data.error, 'error'); fetchRecordingSetting(); return; }
        showStatus('recording-status', toggle.checked ? 'Radio traffic is being recorded' : 'Recording is off for this agency', 'success');
        fetchRecordingSetting();
    })
    .catch(err => { showStatus('recording-status', 'Failed to save: ' + err.message, 'error'); fetchRecordingSetting(); });
}

function putRecordingSetting(body, okMsg) {
    const agency = recordingAgencyId();
    if (agency) body.agency_id = agency;
    return fetch('/api/my-agency/recording', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body)
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('recording-status', data.error, 'error'); }
        else showStatus('recording-status', okMsg, 'success');
        fetchRecordingSetting();
    })
    .catch(err => { showStatus('recording-status', 'Failed to save: ' + err.message, 'error'); fetchRecordingSetting(); });
}

function saveRecordingRetention() {
    const raw = document.getElementById('recording-retention-input').value.trim();
    const days = raw === '' ? 0 : parseInt(raw, 10);
    if (raw !== '' && (!Number.isInteger(days) || days < 1 || days > 3650)) {
        showStatus('recording-status', 'Enter a number of days from 1 to 3650, or leave it blank for the server default', 'error');
        return;
    }
    if (days && !confirm(`Recordings older than ${days} days will be deleted now and from then on. Continue?`)) return;
    putRecordingSetting({ retention_days: days }, days ? `Recordings are kept for ${days} days` : 'Recordings use the server default');
}

function saveTranscribeSetting() {
    const on = document.getElementById('transcribe-toggle').checked;
    putRecordingSetting({ transcribe: on }, on ? 'Recordings will be transcribed' : 'Transcription is off for this agency');
}

function saveTranscribeWords() {
    putRecordingSetting({ transcribe_words: document.getElementById('transcribe-words').value }, 'Word list saved');
}

// --- Transcription engine (Settings, super admin) ----------------------------

let transcriptionPoll = null;
function fetchTranscription() {
    const sel = document.getElementById('transcription-model');
    if (!sel || currentRole !== 'super_admin') return;
    fetch('/api/transcription').then(r => r.json()).then(st => {
        if (st.error) return;
        const prev = sel.value;
        sel.innerHTML = st.models.map(m =>
            `<option value="${escapeHtml(m.name)}">${escapeHtml(m.label)} (${m.size_mb} MB download, ~${m.ram_mb} MB memory)</option>`).join('');
        sel.value = prev || st.model || 'base.en';
        const inst = st.install || {};
        const busy = !!inst.in_progress;
        const sum = document.getElementById('transcription-summary');
        if (st.installed) {
            const m = st.models.find(x => x.name === st.model);
            let speed = '';
            if (st.last_ms && st.last_audio_ms) speed = ` Last transmission: ${(st.last_audio_ms / 1000).toFixed(1)} s of audio in ${(st.last_ms / 1000).toFixed(1)} s.`;
            sum.innerHTML = `<strong>Installed</strong> — model ${escapeHtml(m ? m.label : st.model)}, ${st.threads} of ${st.cpus} CPU cores (${escapeHtml(st.arch)}).
                Since the server started: ${st.transcribed} transcribed, ${st.no_speech} without speech, ${st.failed} failed${st.queue ? ', ' + st.queue + ' waiting' : ''}.${speed}`;
        } else {
            sum.textContent = `Not installed. Choose a model and press Install (${st.cpus} CPU cores, ${st.arch}). Base suits most servers; Tiny for a Raspberry Pi or a busy server.`;
        }
        document.getElementById('transcription-install-btn').textContent = st.installed ? 'Change model' : 'Install';
        document.getElementById('transcription-install-btn').disabled = busy;
        document.getElementById('transcription-remove-btn').style.display = st.installed && !busy ? '' : 'none';
        const prog = document.getElementById('transcription-progress');
        prog.style.display = (busy || inst.stage === 'error') ? 'block' : 'none';
        document.getElementById('transcription-progress-bar').style.width = (inst.progress || 0) + '%';
        document.getElementById('transcription-stage').textContent = inst.error ? 'Failed: ' + inst.error : (inst.message || '');
        clearTimeout(transcriptionPoll);
        if (busy) transcriptionPoll = setTimeout(fetchTranscription, 1000);
        else if (inst.stage === 'done') { showStatus('transcription-status-msg', inst.message, 'success'); fetchRecordingSetting(); }
    }).catch(() => {});
}

function installTranscription() {
    const model = document.getElementById('transcription-model').value;
    fetch('/api/transcription/install', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ model })
    }).then(r => r.json()).then(d => {
        if (d.error) showStatus('transcription-status-msg', d.error, 'error');
        fetchTranscription();
    }).catch(err => showStatus('transcription-status-msg', 'Failed: ' + err.message, 'error'));
}

function removeTranscription() {
    if (!confirm('Remove speech recognition from this server? Transcripts already made are kept; new recordings will not be transcribed until it is installed again.')) return;
    fetch('/api/transcription/remove', { method: 'POST' }).then(r => r.json()).then(d => {
        if (d.error) showStatus('transcription-status-msg', d.error, 'error');
        else showStatus('transcription-status-msg', 'Transcription removed', 'success');
        fetchTranscription();
        fetchRecordingSetting();
    });
}

// --- Guest invites from the app (Settings) ---------------------------------

function guestInvitesAgencyId() {
    const sel = document.getElementById('guest-invites-agency');
    return (currentRole === 'super_admin' && sel && sel.value) ? sel.value : '';
}

function fetchGuestInvites() {
    const toggle = document.getElementById('guest-invites-toggle');
    if (!toggle) return;
    const agency = guestInvitesAgencyId();
    fetch('/api/my-agency/guest-invites' + (agency ? '?agency=' + encodeURIComponent(agency) : ''))
        .then(r => r.json())
        .then(data => {
            if (data.error) { showStatus('guest-invites-status', data.error, 'error'); return; }
            toggle.checked = !!data.enabled;
            const count = document.getElementById('guest-invites-count');
            if (count) count.textContent = data.active_guests
                ? `Active guests right now: ${data.active_guests}.` : '';
        })
        .catch(err => showStatus('guest-invites-status', 'Failed to load: ' + err.message, 'error'));
}

function saveGuestInvites() {
    const toggle = document.getElementById('guest-invites-toggle');
    const body = { enabled: toggle.checked };
    const agency = guestInvitesAgencyId();
    if (agency) body.agency_id = agency;
    fetch('/api/my-agency/guest-invites', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('guest-invites-status', data.error, 'error'); fetchGuestInvites(); return; }
        showStatus('guest-invites-status', toggle.checked
            ? 'Members can invite guests from the app'
            : 'Guest invites are off (existing guests keep access until they expire)', 'success');
    })
    .catch(err => { showStatus('guest-invites-status', 'Failed to save: ' + err.message, 'error'); fetchGuestInvites(); });
}

function floorControlAgencyQuery() {
    const sel = document.getElementById('floor-control-agency');
    return (currentRole === 'super_admin' && sel && sel.value) ? '?agency=' + encodeURIComponent(sel.value) : '';
}

// Policy value <-> API fields.
function floorPolicyOf(data) {
    if (!data.floor_control) return 'open';
    return data.queue_when_busy ? 'queue' : 'busy';
}
function floorPolicyBody(policy) {
    return { floor_control: policy !== 'open', queue_when_busy: policy === 'queue' };
}
const FLOOR_POLICY_TEXT = {
    open: 'Open PTT: simultaneous talkers are mixed together',
    busy: 'Busy tone: one talker per channel, a second press is refused',
    queue: 'Queue and replay: a second talker is recorded and played after the current one'
};

function fetchFloorControl() {
    const sel = document.getElementById('floor-control-policy');
    if (!sel) return;
    fetch('/api/my-agency/floor-control' + floorControlAgencyQuery())
        .then(r => r.json())
        .then(data => {
            if (data.error) { showStatus('floor-control-status', data.error, 'error'); return; }
            sel.value = floorPolicyOf(data);
            sel.disabled = !!data.from_config;
            if (data.from_config) {
                showStatus('floor-control-status',
                    'Set by floor_control_agencies in the server config file; remove it there to control it here.', 'error');
            }
        })
        .catch(err => showStatus('floor-control-status', 'Failed to load: ' + err.message, 'error'));
}

function saveFloorControl() {
    const sel = document.getElementById('floor-control-policy');
    const agencySel = document.getElementById('floor-control-agency');
    const body = floorPolicyBody(sel.value);
    if (currentRole === 'super_admin' && agencySel && agencySel.value) body.agency_id = agencySel.value;
    fetch('/api/my-agency/floor-control', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('floor-control-status', data.error, 'error'); fetchFloorControl(); return; }
        showStatus('floor-control-status', FLOOR_POLICY_TEXT[sel.value], 'success');
    })
    .catch(err => { showStatus('floor-control-status', 'Failed to save: ' + err.message, 'error'); fetchFloorControl(); });
}

// Agencies table policy select (super admin).
function setAgencyFloorControl(agencyId, policy, el) {
    const body = floorPolicyBody(policy);
    body.agency_id = agencyId;
    fetch('/api/my-agency/floor-control', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('agency-status', data.error, 'error'); fetchAgencies(); return; }
        showStatus('agency-status', `${agencyId}: ${FLOOR_POLICY_TEXT[policy]}`, 'success');
    })
    .catch(err => { showStatus('agency-status', 'Failed to save: ' + err.message, 'error'); fetchAgencies(); });
}

// Connect to WebSocket
function connectWebSocket() {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const wsUrl = `${protocol}//${window.location.host}/ws`;

    ws = new WebSocket(wsUrl);

    ws.onopen = () => {
        console.log('WebSocket connected');
        updateConnectionStatus(true);
        addLog('Connected to server', 'success');
        reconnectAttempts = 0;

        if (reconnectInterval) {
            clearInterval(reconnectInterval);
            reconnectInterval = null;
        }
    };

    ws.onmessage = (event) => {
        const message = JSON.parse(event.data);
        if (message.type === 'update') {
            updateDashboard(message.data);
            maCheckRevision(message.data);
        }
    };

    ws.onclose = () => {
        console.log('WebSocket disconnected');
        updateConnectionStatus(false);
        addLog('Disconnected from server', 'warning');

        // Attempt to reconnect every 3 seconds, reload page after 5 failures
        if (!reconnectInterval) {
            reconnectInterval = setInterval(() => {
                reconnectAttempts++;
                if (reconnectAttempts > 5) {
                    clearInterval(reconnectInterval);
                    reconnectInterval = null;
                    location.reload();
                    return;
                }
                connectWebSocket();
            }, 3000);
        }
    };

    ws.onerror = (error) => {
        console.error('WebSocket error:', error);
        addLog('Connection error', 'error');
    };
}

// Update connection status indicator
function updateConnectionStatus(connected) {
    const indicator = document.getElementById('status-indicator');
    const text = document.getElementById('status-text');

    if (connected) {
        indicator.classList.add('connected');
        text.textContent = 'Connected';
    } else {
        indicator.classList.remove('connected');
        text.textContent = 'Disconnected';
    }
    const mb = document.getElementById('mobile-bar-status');
    if (mb) { mb.classList.toggle('connected', connected); mb.classList.toggle('disconnected', !connected); }
}

// Update all dashboard elements
function updateDashboard(data) {
    // For agency admins, filter WebSocket data to their agency only
    if (currentRole === 'agency_admin' && currentAgencyId) {
        if (data.clients) {
            data.clients = data.clients.filter(c => c.agency_id === currentAgencyId);
        }
        if (data.channels) {
            data.channels = data.channels.filter(c => c.agency_id === currentAgencyId);
        }
        if (data.stats) {
            data.stats.total_clients = data.clients ? data.clients.length : 0;
        }
        if (data.activity) {
            data.activity = data.activity.filter(e => e.agency_id === currentAgencyId);
        }
    }
    connectedClients = data.clients || [];
    lastStats = data.stats || {};
    updateStats(data.stats);
    updateChannels(data.channels);
    updateClients(data.clients);
    updateUsersPageStats();
    const ocm = document.getElementById('onlineClientsModal');
    if (ocm && ocm.classList.contains('active')) renderOnlineClientsModal();

    // Network activity chart
    if (data.network_timeseries) {
        updateNetworkChart(data.network_timeseries);
    }

    // System health from server
    if (data.health) {
        updateHealthFromServer(data.health);
    }

    // Activity feed
    if (data.activity) {
        updateActivityFeed(data.activity);
        updateActivityChart(data.activity);
    }
}

// Update statistics cards
function updateStats(stats) {
    document.getElementById('total-clients').textContent = stats.total_clients || 0;
    document.getElementById('uptime').textContent = formatUptime(stats.uptime_seconds || 0);
    document.getElementById('packets-sent').textContent = formatNumber(stats.packets_sent || 0);
    document.getElementById('packets-received').textContent = formatNumber(stats.packets_received || 0);
}

// Update channels list
function updateChannels(channels) {
    const channelList = document.getElementById('channel-list');
    const topCount = document.getElementById('top-channels-count');

    // Sort channels by ID
    if (channels) channels.sort((a, b) => a.id - b.id);

    // Only show channels with clients
    const activeChannels = channels ? channels.filter(ch => ch.client_count > 0) : [];

    // Update top info count
    if (topCount) topCount.textContent = activeChannels.length;

    if (activeChannels.length === 0) {
        channelList.innerHTML = '<div class="empty-state"><div class="empty-state-icon">&#x1F4E1;</div><div>No active channels</div></div>';
        return;
    }

    channelList.innerHTML = activeChannels.map(channel => {
        const agencyLabel = currentRole === 'super_admin' && channel.agency_id ? ` <span class="agency-badge">${escapeHtml(channel.agency_id)}</span>` : '';
        return `
            <div class="channel-item">
                <div class="channel-name">${escapeHtml(channel.label || ('Channel ' + channel.id))}${agencyLabel}</div>
                <div class="channel-count">${escapeHtml(channel.client_count)} ${channel.client_count === 1 ? 'client' : 'clients'}</div>
            </div>
        `;
    }).join('');
}

// Update clients list
// Which sign-in protocol a connected phone is using. "Secure" = the server is
// verified by the phone and every packet is encrypted and checked. "Older
// protocol" = an app from before that; it still works, less protected.
function protocolBadge(c) {
    if (c.console) return '';
    return c.secure
        ? ' <span class="guest-badge" style="background:rgba(63,185,80,0.15);color:#3FB950;border-color:rgba(63,185,80,0.35)" title="Secure protocol: the phone verifies this server and every packet is encrypted and checked">Secure</span>'
        : ' <span class="guest-badge" title="This app uses the older sign-in protocol. It works, but is less protected. Update the app.">Older protocol</span>';
}

// The server's identity key: put in every enrollment QR so a phone knows the
// real server from its first connection.
let serverIdentity = null;
function fetchServerIdentity() {
    return fetch('/api/server-identity').then(r => r.ok ? r.json() : null)
        .then(d => { if (d && d.public_key) serverIdentity = d; return serverIdentity; })
        .catch(() => null);
}

function updateClients(clients) {
    const clientList = document.getElementById('client-list');
    const topCount = document.getElementById('top-clients-count');

    if (topCount) topCount.textContent = clients ? clients.length : 0;

    if (!clients || clients.length === 0) {
        clientList.innerHTML = '<div class="empty-state"><div class="empty-state-icon">&#x1F465;</div><div>No connected clients</div></div>';
        return;
    }

    // Sort clients by connection time (newest first)
    clients.sort((a, b) => new Date(b.connected_at) - new Date(a.connected_at));

    clientList.innerHTML = clients.map(client => {
        const connectedTime = timeSince(new Date(client.connected_at));
        const agencyLabel = currentRole === 'super_admin' && client.agency_id ? ` <span class="agency-badge">${escapeHtml(client.agency_id)}</span>` : '';
        return `
            <div class="client-item">
                <div class="client-username">
                    ${escapeHtml(client.username)}${agencyLabel}
                    <span class="client-channel">Ch ${escapeHtml(client.channel)}</span>${protocolBadge(client)}
                </div>
                <div class="client-info">
                    Connected ${escapeHtml(connectedTime)}
                </div>
            </div>
        `;
    }).join('');
}

// Add log entry
function addLog(message, type = 'info') {
    const logContainer = document.getElementById('log-container');
    const time = new Date().toLocaleTimeString();

    const logEntry = document.createElement('div');
    logEntry.className = `log-entry ${type}`;
    logEntry.innerHTML = `<span class="log-time">${time}</span>${escapeHtml(message)}`;

    logContainer.appendChild(logEntry);
    logContainer.scrollTop = logContainer.scrollHeight;

    // Keep only last 100 log entries
    while (logContainer.children.length > 100) {
        logContainer.removeChild(logContainer.firstChild);
    }
}

// Helper Functions

function formatUptime(seconds) {
    const hours = Math.floor(seconds / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);

    if (hours > 0) {
        return `${hours}h ${minutes}m`;
    } else {
        return `${minutes}m`;
    }
}

function formatNumber(num) {
    if (num >= 1000000) {
        return (num / 1000000).toFixed(1) + 'M';
    } else if (num >= 1000) {
        return (num / 1000).toFixed(1) + 'K';
    } else {
        return num.toString();
    }
}

function timeSince(date) {
    const seconds = Math.floor((new Date() - date) / 1000);

    const intervals = {
        year: 31536000,
        month: 2592000,
        week: 604800,
        day: 86400,
        hour: 3600,
        minute: 60
    };

    for (const [name, secondsInInterval] of Object.entries(intervals)) {
        const interval = Math.floor(seconds / secondsInInterval);
        if (interval >= 1) {
            return `${interval} ${name}${interval !== 1 ? 's' : ''} ago`;
        }
    }

    return 'just now';
}

// Escape text for HTML element content AND attribute values (both quote
// styles). The old textContent/innerHTML trick left " and ' alone, so a name
// could break out of an attribute.
function escapeHtml(text) {
    return String(text == null ? '' : text)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
}

// Encode a value as a JavaScript literal for an inline handler written in a
// DOUBLE-quoted attribute, e.g. onclick="fn(${jsArg(name)})". JSON.stringify
// produces a valid JS literal; escapeAttr then protects the attribute. The
// browser decodes the entities before running the handler, so the function
// receives the original value exactly. Never wrap the result in quotes, and
// never use it inside a single-quoted attribute.
function jsArg(v) {
    return escapeAttr(JSON.stringify(v === undefined ? null : v));
}

// Avatar color based on username hash
const avatarColors = ['#1F6FEB', '#3B82F6', '#10B981', '#F59E0B', '#EC4899', '#06B6D4'];

function getAvatarColor(username) {
    let hash = 0;
    for (let i = 0; i < username.length; i++) {
        hash = username.charCodeAt(i) + ((hash << 5) - hash);
    }
    return avatarColors[Math.abs(hash) % avatarColors.length];
}

// Online = a live connection signed in with this ACCOUNT. The callsign a phone
// joins under can differ from its login account (login "Unit1", callsign
// "Medic4"), so match the connection's account, not just its callsign.
function isUserOnline(user) {
    if (typeof user === 'string') user = { username: user };
    return connectedClients.some(c =>
        (c.account || c.username) === user.username &&
        (!user.agency_id || !c.agency_id || c.agency_id === user.agency_id));
}

// --- Users table: sort, group, filter --------------------------------------
// Column headers sort (click again to reverse); "Group by" stacks users under
// headers (online/offline, agency, role, guests) that can be collapsed; the
// stat cards filter (all / online / offline / new). Choices are remembered.

function loadPref(key, fallback) {
    try { const v = localStorage.getItem(key); return v === null ? fallback : JSON.parse(v); } catch (e) { return fallback; }
}
function savePref(key, value) {
    try { localStorage.setItem(key, JSON.stringify(value)); } catch (e) {}
}

let usersSort = loadPref('usersSortV2', { key: 'name', dir: 1 });
let usersGroup = loadPref('usersGroup', 'none');
let usersFilter = 'all';               // per visit: all | online | offline | new
const collapsedUserGroups = new Set();

const USER_SORT_DEFAULT_DIR = { last_login: -1, created: -1, status: -1 };

function sortUsersBy(key) {
    if (usersSort.key === key) usersSort = { key, dir: -usersSort.dir };
    else usersSort = { key, dir: USER_SORT_DEFAULT_DIR[key] || 1 };
    savePref('usersSortV2', usersSort);
    currentUsersPage = 1;
    renderUsersPage();
}

function setUsersGroup(value) {
    usersGroup = value;
    savePref('usersGroup', value);
    collapsedUserGroups.clear();
    currentUsersPage = 1;
    renderUsersPage();
}

function setUsersFilter(value) {
    usersFilter = (usersFilter === value) ? 'all' : value;
    currentUsersPage = 1;
    renderUsersPage();
}

function toggleUserGroup(id) {
    if (collapsedUserGroups.has(id)) collapsedUserGroups.delete(id); else collapsedUserGroups.add(id);
    renderUsersPage();
}

function userTime(v) { const n = v ? Date.parse(v) : NaN; return isNaN(n) ? 0 : n; }
function userLabel(u) { return (u.name || u.username || '').toLowerCase(); }
const ROLE_ORDER = { super_admin: 0, admin: 1, dispatch: 2, user: 3 };

function sortUsers(list) {
    const { key, dir } = usersSort;
    const cmpText = (x, y) => x.localeCompare(y, undefined, { sensitivity: 'base', numeric: true });
    const value = {
        name: u => userLabel(u),
        username: u => (u.username || '').toLowerCase(),
        status: u => (isUserOnline(u) ? 1 : 0),
        agency: u => (u.agency_id || 'default').toLowerCase(),
        role: u => ROLE_ORDER[u.role || 'user'] ?? 9,
        last_login: u => (isUserOnline(u) ? Date.now() + 1 : userTime(u.last_login)),
        created: u => userTime(u.created_at),
    }[key] || (u => userLabel(u));
    return list.slice().sort((a, b) => {
        const va = value(a), vb = value(b);
        const c = (typeof va === 'string') ? cmpText(va, vb) : (va - vb);
        return (c * dir) || cmpText(userLabel(a), userLabel(b));
    });
}

// Group key + label for the current grouping, or null when not grouping.
function userGroupOf(u) {
    switch (usersGroup) {
        case 'status': return isUserOnline(u) ? { id: 'online', label: 'Online', order: 0 } : { id: 'offline', label: 'Offline', order: 1 };
        case 'agency': {
            const a = u.agency_id || 'default';
            const ag = (typeof agenciesList !== 'undefined' && agenciesList || []).find(x => x.id === a);
            return { id: 'agency:' + a, label: ag && ag.name && ag.name !== a ? `${ag.name} (${a})` : a, order: a.toLowerCase() };
        }
        case 'role': {
            const r = u.role || 'user';
            const names = { super_admin: 'Super Admins', admin: 'Admins', dispatch: 'Dispatch', user: 'Users' };
            return { id: 'role:' + r, label: names[r] || r, order: ROLE_ORDER[r] ?? 9 };
        }
        case 'guest': return u.guest_expires_at ? { id: 'guest', label: 'Guests', order: 1 } : { id: 'member', label: 'Members', order: 0 };
        default: return null;
    }
}

// --- Online clients popup ---------------------------------------------------

function showOnlineClientsModal() {
    renderOnlineClientsModal();
    showModal('onlineClientsModal');
}

function renderOnlineClientsModal() {
    const list = document.getElementById('online-clients-list');
    const count = document.getElementById('online-clients-count');
    if (!list) return;
    const clients = (connectedClients || []).slice()
        .sort((a, b) => (a.username || '').localeCompare(b.username || '', undefined, { sensitivity: 'base' }));
    if (count) count.textContent = `(${clients.length})`;
    if (clients.length === 0) {
        list.innerHTML = '<div class="empty-state"><div>No one is connected</div></div>';
        return;
    }
    list.innerHTML = clients.map(c => {
        const agency = currentRole === 'super_admin' && c.agency_id ? ` <span class="agency-badge">${escapeHtml(c.agency_id)}</span>` : '';
        const account = c.account && c.account !== c.username ? ` · signed in as <strong>${escapeHtml(c.account)}</strong>` : '';
        const since = c.connected_at ? timeSince(new Date(c.connected_at)) : '';
        return `
            <div class="client-item">
                <div class="client-username">${statusDotHtml(true)} ${escapeHtml(c.username)}${agency}
                    <span class="client-channel">Ch ${escapeHtml(String(c.channel))}</span>${protocolBadge(c)}</div>
                <div class="client-info">${since ? 'Connected ' + escapeHtml(since) : ''}${account}</div>
            </div>`;
    }).join('');
}

function statusDotHtml(online) {
    return online ? '<span class="status-dot-online"></span>' : '<span class="status-dot-offline"></span>';
}

function filterUsersTable() {
    currentUsersPage = 1;
    renderUsersPage();
}

// (Top Info tabs removed — now side-by-side columns)

// --- Modal Management ---

function showModal(id) {
    document.getElementById(id).classList.add('active');
}

function closeModal(id) {
    document.getElementById(id).classList.remove('active');
}

// Close modal on backdrop click (except forced password change — that one is non-dismissible)
document.addEventListener('click', (e) => {
    if (e.target.classList.contains('modal') && e.target.id !== 'changeAdminPasswordModal') {
        e.target.classList.remove('active');
    }
});

// --- Status Messages ---

function showStatus(elementId, message, type) {
    const el = document.getElementById(elementId);
    if (!el) return;
    el.textContent = message;
    el.className = `status-message ${type}`;
    el.style.display = 'block';

    // Auto-hide after 5 seconds
    setTimeout(() => {
        el.style.display = 'none';
    }, 5000);
}

// --- User Management ---

function fetchUsers() {
    let url = '/api/users';
    if (currentRole === 'super_admin') {
        const filter = document.getElementById('users-agency-filter');
        if (filter && filter.value) {
            url += '?agency=' + encodeURIComponent(filter.value);
        }
    }
    fetch(url)
        .then(r => r.json())
        .then(users => {
            allUsers = users || [];
            currentUsersPage = 1;
            renderUsersPage();
        })
        .catch(err => console.error('Error fetching users:', err));
}

function getFilteredUsers() {
    const query = (document.getElementById('users-search').value || '').toLowerCase();
    const weekAgo = Date.now() - 7 * 86400000;
    let list = allUsers;
    if (usersFilter === 'online') list = list.filter(u => isUserOnline(u));
    else if (usersFilter === 'offline') list = list.filter(u => !isUserOnline(u));
    else if (usersFilter === 'new') list = list.filter(u => userTime(u.created_at) > weekAgo);
    if (query) list = list.filter(u => u.username.toLowerCase().includes(query) || (u.name || '').toLowerCase().includes(query) || (u.agency_id || '').toLowerCase().includes(query));
    return sortUsers(list);
}

// Rows to show: users, with group headers in front of each group when grouping.
function getUserTableItems() {
    const users = getFilteredUsers();
    if (usersGroup === 'none') return users.map(u => ({ type: 'user', user: u }));
    const groups = new Map();
    for (const u of users) {
        const g = userGroupOf(u);
        if (!groups.has(g.id)) groups.set(g.id, { ...g, users: [] });
        groups.get(g.id).users.push(u);
    }
    const ordered = [...groups.values()].sort((x, y) =>
        typeof x.order === 'string' ? x.order.localeCompare(y.order) : x.order - y.order);
    const items = [];
    for (const g of ordered) {
        const collapsed = collapsedUserGroups.has(g.id);
        const online = g.users.filter(u => isUserOnline(u)).length;
        items.push({ type: 'group', group: g, collapsed, online });
        if (!collapsed) g.users.forEach(u => items.push({ type: 'user', user: u }));
    }
    return items;
}

function renderUsersPage() {
    const groupSel = document.getElementById('users-group');
    if (groupSel && groupSel.value !== usersGroup) groupSel.value = usersGroup;
    const items = getUserTableItems();
    const total = items.length;
    const totalPages = Math.max(1, Math.ceil(total / USERS_PER_PAGE));

    if (currentUsersPage > totalPages) currentUsersPage = totalPages;

    const start = (currentUsersPage - 1) * USERS_PER_PAGE;
    let pageItems = items.slice(start, start + USERS_PER_PAGE);
    // A page that starts mid-group repeats that group's header.
    if (pageItems.length && pageItems[0].type === 'user' && usersGroup !== 'none') {
        for (let i = start - 1; i >= 0; i--) {
            if (items[i].type === 'group') { pageItems = [{ ...items[i], continued: true }, ...pageItems]; break; }
        }
    }

    renderUsersHeader();
    renderUsersTable(pageItems);
    renderPagination(getFilteredUsers().length, totalPages, total);
    updateUsersPageStats();
}

// Sort arrows on the clickable headers.
function renderUsersHeader() {
    document.querySelectorAll('#users-data-table th[data-sort]').forEach(th => {
        const key = th.getAttribute('data-sort');
        const arrow = th.querySelector('.sort-arrow');
        const active = usersSort.key === key;
        th.classList.toggle('sorted', active);
        if (arrow) arrow.textContent = active ? (usersSort.dir > 0 ? '▲' : '▼') : '';
    });
}

function shortDateTime(v) {
    const d = new Date(v);
    if (isNaN(d)) return '';
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
}

function renderUsersTable(items) {
    const users = items;
    const tbody = document.getElementById('users-table-body');
    const emptyState = document.getElementById('users-empty');
    const table = document.getElementById('users-data-table');
    const pagination = document.getElementById('users-pagination');

    if (!allUsers || allUsers.length === 0) {
        tbody.innerHTML = '';
        if (table) table.style.display = 'none';
        if (pagination) pagination.style.display = 'none';
        emptyState.style.display = 'block';
        return;
    }

    if (table) table.style.display = 'table';
    emptyState.style.display = 'none';

    if (!users || users.length === 0) {
        tbody.innerHTML = '<tr><td colspan="9" style="text-align: center; color: #8B949E; padding: 30px;">No matching users</td></tr>';
        return;
    }

    const colCount = document.querySelectorAll('#users-data-table thead th').length -
        (currentRole === 'super_admin' ? 0 : 1);
    tbody.innerHTML = users.map(item => {
        if (item.type === 'group') {
            const g = item.group;
            const n = g.users.length;
            return `
            <tr class="user-group-row" onclick="toggleUserGroup(${jsArg(g.id)})">
                <td colspan="${colCount}">
                    <span class="group-caret">${item.collapsed ? '▸' : '▾'}</span>
                    <span class="group-label">${escapeHtml(g.label)}</span>
                    <span class="group-count">${n} ${n === 1 ? 'user' : 'users'}${usersGroup !== 'status' ? ` · ${item.online} online` : ''}${item.continued ? ' (continued)' : ''}</span>
                </td>
            </tr>`;
        }
        const user = item.user;
        const created = user.created_at
            ? `<span title="${escapeHtml(new Date(user.created_at).toLocaleString())}">${escapeHtml(shortDateTime(user.created_at))}</span>`
            : 'Unknown';
        const online = isUserOnline(user);
        const lastLogin = online
            ? '<span style="color:#3FB950;">Online now</span>'
            : (user.last_login
                ? `<span title="${escapeHtml(new Date(user.last_login).toLocaleString())}">${escapeHtml(timeSince(new Date(user.last_login)))}</span>`
                : '<span style="color:#8B949E;">Never</span>');
        const safeName = escapeHtml(user.name || '');
        const safeUsername = escapeHtml(user.username);
        const safeAgency = escapeHtml(user.agency_id || 'default');
        const agencyCol = currentRole === 'super_admin' ? `<td><span class="agency-badge">${safeAgency}</span></td>` : '';
        const role = user.role || 'user';
        const superAdminOpt = currentRole === 'super_admin' ? `<option value="super_admin" ${role === 'super_admin' ? 'selected' : ''}>Super Admin</option>` : '';
        const initial = (user.name || user.username).charAt(0).toUpperCase();
        const avatarColor = getAvatarColor(user.username);
        const statusDot = online ? '<span class="status-dot-online"></span>' : '<span class="status-dot-offline"></span>';
        // Dispatch users are web-only — show badge instead of editable role dropdown; no QR/Edit
        const isDispatch = role === 'dispatch';
        // Guests invited from the app: who invited them and when they expire.
        const guestLeft = user.guest_expires_at ? Date.parse(user.guest_expires_at) - Date.now() : 0;
        const guestLeftText = guestLeft <= 0 ? 'expiring'
            : guestLeft < 3600000 ? `${Math.max(1, Math.round(guestLeft / 60000))}m left`
            : guestLeft < 2 * 86400000 ? `${Math.round(guestLeft / 3600000)}h left`
            : `${Math.round(guestLeft / 86400000)}d left`;
        const guestBadge = user.guest_expires_at
            ? `<div><span class="guest-badge" title="Guest of ${escapeHtml(user.invited_by || '?')} until ${escapeHtml(new Date(user.guest_expires_at).toLocaleString())}">Guest · ${guestLeftText}</span></div>`
            : '';
        // Phones enrolled by one-time QR. "older app" keys accept a copy of
        // their QR too, until "Sign out phones" removes them.
        const devs = user.devices || [];
        const legacyDevs = devs.filter(d => d.legacy).length;
        const phonesBadge = devs.length
            ? `<div><span class="guest-badge" title="${escapeHtml(devs.map(d => `Enrolled ${d.created_at ? new Date(d.created_at).toLocaleString() : '?'} from ${d.enrolled_from || '?'}${d.legacy ? ' (older app)' : ''}`).join('\n'))}">${devs.length} ${devs.length === 1 ? 'phone' : 'phones'}${legacyDevs ? ` · ${legacyDevs} older app` : ''}</span></div>`
            : '';
        const roleCell = isDispatch
            ? `<span class="link-status" style="background:rgba(31,111,235,0.15);color:#58A6FF;border-color:rgba(31,111,235,0.3);">Dispatch</span>`
            : `<select class="role-select" onchange="updateUserRole(${jsArg(user.username)}, this.value, ${jsArg(user.agency_id || 'default')})" data-current="${escapeHtml(role)}">
                    <option value="user" ${role === 'user' ? 'selected' : ''}>User</option>
                    <option value="admin" ${role === 'admin' ? 'selected' : ''}>Admin</option>
                    ${superAdminOpt}
               </select>`;
        const actionButtons = isDispatch
            ? `<button class="btn-action delete" onclick="deleteUser(${jsArg(user.username)}, ${jsArg(user.agency_id || 'default')})">Delete</button>`
            : `<button class="btn-action qr" onclick="showQRCode(${jsArg(user.username)}, ${jsArg(user.agency_id || 'default')})">QR Code</button>
               <button class="btn-action edit" onclick="showEditUser(${jsArg(user.username)}, ${jsArg(user.agency_id || 'default')})">Edit</button>
               ${devs.length || online ? `<button class="btn-action edit" title="Remove this account's QR phone keys and disconnect every phone signed in to it" onclick="signOutPhones(${jsArg(user.username)}, ${jsArg(user.agency_id || 'default')})">Sign out phones</button>` : ''}
               <button class="btn-action delete" onclick="deleteUser(${jsArg(user.username)}, ${jsArg(user.agency_id || 'default')})">Delete</button>`;
        return `
            <tr>
                <td><div class="user-avatar" style="background: ${avatarColor};">${escapeHtml(initial)}</div></td>
                <td class="user-name-cell">${safeName || '<span style="color:#8B949E;">—</span>'}</td>
                <td class="nowrap"><strong>${safeUsername}</strong>${guestBadge}${phonesBadge}</td>
                <td style="text-align: center;">${statusDot}</td>
                ${agencyCol}
                <td>${roleCell}</td>
                <td class="nowrap">${lastLogin}</td>
                <td class="nowrap">${created}</td>
                <td>
                    <div class="actions-cell">${actionButtons}</div>
                </td>
            </tr>
        `;
    }).join('');
}

function renderPagination(userCount, totalPages, rowCount) {
    const pagination = document.getElementById('users-pagination');
    const info = document.getElementById('pagination-info');
    const controls = document.getElementById('pagination-controls');
    const total = rowCount === undefined ? userCount : rowCount;

    if (total <= USERS_PER_PAGE) {
        pagination.style.display = 'none';
        return;
    }

    pagination.style.display = 'flex';

    if (usersGroup === 'none') {
        const start = (currentUsersPage - 1) * USERS_PER_PAGE + 1;
        const end = Math.min(currentUsersPage * USERS_PER_PAGE, total);
        info.textContent = `Showing ${start}-${end} of ${userCount} users`;
    } else {
        info.textContent = `Page ${currentUsersPage} of ${totalPages} · ${userCount} users`;
    }

    let html = '';

    // Previous button
    html += `<button class="pagination-btn" onclick="goToUsersPage(${currentUsersPage - 1})" ${currentUsersPage === 1 ? 'disabled' : ''}>&laquo;</button>`;

    // Page numbers (show max 5 centered around current)
    let startPage = Math.max(1, currentUsersPage - 2);
    let endPage = Math.min(totalPages, startPage + 4);
    if (endPage - startPage < 4) startPage = Math.max(1, endPage - 4);

    for (let i = startPage; i <= endPage; i++) {
        html += `<button class="pagination-btn ${i === currentUsersPage ? 'active' : ''}" onclick="goToUsersPage(${i})">${i}</button>`;
    }

    // Next button
    html += `<button class="pagination-btn" onclick="goToUsersPage(${currentUsersPage + 1})" ${currentUsersPage === totalPages ? 'disabled' : ''}>&raquo;</button>`;

    controls.innerHTML = html;
}

function goToUsersPage(page) {
    const totalPages = Math.max(1, Math.ceil(getUserTableItems().length / USERS_PER_PAGE));
    if (page < 1 || page > totalPages) return;
    currentUsersPage = page;
    renderUsersPage();
}

function updateUsersPageStats() {
    const totalEl = document.getElementById('users-total');
    const onlineEl = document.getElementById('users-online');
    const offlineEl = document.getElementById('users-offline');
    const newEl = document.getElementById('users-new');

    if (!totalEl) return; // not on users page

    const total = allUsers.length;
    const online = allUsers.filter(u => isUserOnline(u)).length;
    const offline = total - online;

    // Count users created in last 7 days
    const sevenDaysAgo = new Date();
    sevenDaysAgo.setDate(sevenDaysAgo.getDate() - 7);
    const newUsers = allUsers.filter(u => u.created_at && new Date(u.created_at) > sevenDaysAgo).length;

    totalEl.textContent = total;
    onlineEl.textContent = online;
    offlineEl.textContent = offline;
    newEl.textContent = newUsers;

    document.querySelectorAll('.users-stat-card[data-filter]').forEach(card => {
        const f = card.getAttribute('data-filter');
        card.classList.toggle('active', f !== 'all' && f === usersFilter);
    });
}

// Create or remove the Zello Bridge health row so it only appears when a bridge
// is enabled (server includes the "Zello Bridge" service only in that case).
function syncZelloHealthRow(present) {
    const existing = document.getElementById('health-zello');
    if (present && !existing) {
        const services = document.querySelector('.health-services');
        if (!services) return;
        const row = document.createElement('div');
        row.className = 'health-service-row';
        row.innerHTML =
            '<span class="health-svc-dot healthy"></span>' +
            '<svg class="health-svc-icon" viewBox="0 0 24 24" style="stroke:#34D399;">' +
            '<path d="M4.5 16.5c-1.5 1.26-2 5-2 5s3.74-.5 5-2c.71-.84.7-2.13-.09-2.91a2.18 2.18 0 0 0-2.91-.09z"/>' +
            '<path d="M12 15l-3-3a22 22 0 0 1 2-3.95A12.88 12.88 0 0 1 22 2c0 2.72-.78 7.5-6 11a22.35 22.35 0 0 1-4 2z"/>' +
            '<path d="M9 12H4s.55-3.03 2-4c1.62-1.08 5 0 5 0"/></svg>' +
            '<span class="health-svc-name">Zello Bridge</span>' +
            '<span class="health-svc-status healthy" id="health-zello">Healthy</span>';
        services.appendChild(row);
    } else if (!present && existing) {
        const row = existing.parentElement;
        // Remove any open detail panel too.
        const panel = row.nextElementSibling;
        if (panel && panel.classList.contains('health-detail-panel')) panel.remove();
        row.remove();
    }
}

function updateHealthFromServer(health) {
    lastHealth = health;

    // Update Users page sidebar health panel
    const clientsEl = document.getElementById('health-clients');
    const uptimeEl = document.getElementById('health-uptime');
    const versionEl = document.getElementById('health-version');
    const sidebarVersion = document.getElementById('sidebar-version');

    if (clientsEl) clientsEl.textContent = connectedClients.length;
    if (uptimeEl) uptimeEl.textContent = formatUptime(lastStats.uptime_seconds || 0);
    if (versionEl && lastStats.version) versionEl.textContent = lastStats.version;
    if (sidebarVersion && lastStats.version) sidebarVersion.textContent = lastStats.version;

    // Dashboard system health panel — data-driven from server.
    // The Zello Bridge row is CONDITIONAL: the server only includes a service
    // named "Zello Bridge" when a bridge is enabled. Create/remove its row here.
    syncZelloHealthRow(health.services.some(s => s.name === 'Zello Bridge'));

    const serviceIds = ['health-web', 'health-db', 'health-relay', 'health-push', 'health-storage', 'health-zello'];
    health.services.forEach((svc, i) => {
        const el = document.getElementById(serviceIds[i]);
        if (!el) return;

        el.textContent = svc.status === 'healthy' ? 'Healthy' : svc.status === 'warning' ? 'Warning' : 'Error';
        el.className = 'health-svc-status ' + svc.status;

        const row = el.parentElement;
        const dot = row.querySelector('.health-svc-dot');
        if (dot) {
            dot.className = 'health-svc-dot ' + svc.status;
        }

        // Make non-healthy rows clickable to reveal the server's detail message.
        const clickable = svc.status !== 'healthy';
        row.classList.toggle('health-clickable', clickable);
        row.dataset.detail = svc.details || 'No additional information available.';
        row.dataset.status = svc.status;
        row.dataset.svcName = svc.name;

        if (clickable && !row.dataset.bound) {
            row.dataset.bound = '1';
            row.addEventListener('click', () => toggleHealthDetail(row));
        }

        // If a detail panel is already open for this row, refresh its text.
        const openPanel = row.nextElementSibling;
        if (openPanel && openPanel.classList.contains('health-detail-panel')) {
            if (clickable) {
                openPanel.querySelector('.health-detail-msg').textContent = row.dataset.detail;
            } else {
                openPanel.remove(); // it recovered — close the panel
            }
        }
    });

    // Update health ring
    const ringCircle = document.getElementById('health-ring-circle');
    const ringPct = document.getElementById('health-ring-pct');
    const ringSublabel = document.getElementById('health-ring-sublabel');
    if (ringCircle) {
        const pct = health.overall_percent;
        const circumference = 2 * Math.PI * 15.5; // ~97.4
        const offset = circumference - (pct / 100) * circumference;
        ringCircle.style.strokeDashoffset = offset;
        ringCircle.style.stroke = pct >= 80 ? '#34D399' : pct >= 50 ? '#FBBF24' : '#F87171';
        if (ringPct) ringPct.textContent = pct + '%';
        if (ringSublabel) ringSublabel.textContent = `${health.healthy_count} of ${health.total_count} services healthy`;
    }
}

// Toggle an inline detail panel below a health row, showing the server's
// diagnostic message for a warning/error service.
function toggleHealthDetail(row) {
    const existing = row.nextElementSibling;
    if (existing && existing.classList.contains('health-detail-panel')) {
        existing.remove();
        row.classList.remove('health-detail-open');
        return;
    }
    // Close any other open panels first (accordion behavior).
    document.querySelectorAll('.health-detail-panel').forEach(p => p.remove());
    document.querySelectorAll('.health-service-row.health-detail-open')
        .forEach(r => r.classList.remove('health-detail-open'));

    const status = row.dataset.status || 'warning';
    const panel = document.createElement('div');
    panel.className = 'health-detail-panel ' + status;
    panel.innerHTML =
        '<div class="health-detail-title">' +
            (status === 'error' ? '&#9888; Error' : '&#9888; Warning') +
            ' &mdash; ' + escapeHtml(row.dataset.svcName || '') +
        '</div>' +
        '<div class="health-detail-msg">' + escapeHtml(row.dataset.detail || '') + '</div>';
    row.after(panel);
    row.classList.add('health-detail-open');
}

// --- Network Activity Chart ---

function initNetworkChart() {
    const canvas = document.getElementById('network-chart');
    if (!canvas) return;

    networkChart = new Chart(canvas, {
        type: 'line',
        data: {
            labels: [],
            datasets: [
                {
                    label: 'Packets Sent',
                    data: [],
                    borderColor: '#60A5FA',
                    backgroundColor: 'rgba(96, 165, 250, 0.1)',
                    borderWidth: 2,
                    fill: true,
                    tension: 0.3,
                    pointRadius: 0,
                    pointHitRadius: 10,
                },
                {
                    label: 'Packets Received',
                    data: [],
                    borderColor: '#A78BFA',                  // purple kept for this chart
                    backgroundColor: 'rgba(167, 139, 250, 0.1)',
                    borderWidth: 2,
                    fill: true,
                    tension: 0.3,
                    pointRadius: 0,
                    pointHitRadius: 10,
                }
            ]
        },
        options: {
            responsive: true,
            maintainAspectRatio: false,
            animation: { duration: 300 },
            plugins: {
                legend: { display: false },
                tooltip: {
                    mode: 'index',
                    intersect: false,
                    backgroundColor: '#151B23',
                    borderColor: '#1E2733',
                    borderWidth: 1,
                    titleColor: '#E6EDF3',
                    bodyColor: '#8B949E',
                }
            },
            scales: {
                x: {
                    grid: { color: 'rgba(30, 39, 51, 0.5)', drawBorder: false },
                    ticks: { color: '#4D5562', font: { size: 10 }, maxTicksLimit: 10 }
                },
                y: {
                    beginAtZero: true,
                    grid: { color: 'rgba(30, 39, 51, 0.5)', drawBorder: false },
                    ticks: { color: '#4D5562', font: { size: 10 } }
                }
            },
            interaction: { mode: 'nearest', axis: 'x', intersect: false }
        }
    });
}

function updateNetworkChart(timeseries) {
    if (!timeseries || timeseries.length === 0) return;

    if (!networkChart) initNetworkChart();
    if (!networkChart) return;

    const labels = timeseries.map(p => {
        const d = new Date(p.timestamp);
        return d.toLocaleTimeString('en-US', { hour: 'numeric', minute: '2-digit', hour12: false });
    });

    networkChart.data.labels = labels;
    networkChart.data.datasets[0].data = timeseries.map(p => p.packets_sent);
    networkChart.data.datasets[1].data = timeseries.map(p => p.packets_received);
    networkChart.update('none');
}

// --- Activity Overview Chart (Users sidebar) ---

function initActivityChart() {
    const canvas = document.getElementById('activity-chart');
    if (!canvas) return;

    activityChart = new Chart(canvas, {
        type: 'bar',
        data: {
            labels: [],
            datasets: [{
                label: 'Events',
                data: [],
                backgroundColor: 'rgba(31, 111, 235, 0.6)',
                borderColor: '#1F6FEB',
                borderWidth: 1,
                borderRadius: 4,
            }]
        },
        options: {
            responsive: true,
            maintainAspectRatio: false,
            animation: { duration: 300 },
            plugins: { legend: { display: false } },
            scales: {
                x: {
                    grid: { display: false },
                    ticks: { color: '#4D5562', font: { size: 9 } }
                },
                y: {
                    beginAtZero: true,
                    grid: { color: 'rgba(30, 39, 51, 0.5)', drawBorder: false },
                    ticks: { color: '#4D5562', font: { size: 9 }, stepSize: 1 }
                }
            }
        }
    });
}

function updateActivityChart(events) {
    if (!events || events.length === 0) return;

    if (!activityChart) initActivityChart();
    if (!activityChart) return;

    // Aggregate events by day (last 7 days)
    const dayCounts = {};
    const now = new Date();
    for (let i = 6; i >= 0; i--) {
        const d = new Date(now);
        d.setDate(d.getDate() - i);
        const key = d.toISOString().split('T')[0];
        dayCounts[key] = 0;
    }

    events.forEach(e => {
        const day = e.timestamp.split('T')[0];
        if (day in dayCounts) dayCounts[day]++;
    });

    const labels = Object.keys(dayCounts).map(d => {
        const parts = d.split('-');
        return parts[1] + '/' + parts[2];
    });

    activityChart.data.labels = labels;
    activityChart.data.datasets[0].data = Object.values(dayCounts);
    activityChart.update('none');
}

// --- Recent Activity Feed ---

function updateActivityFeed(events) {
    const feed = document.getElementById('recent-activity-feed');
    if (!feed) return;

    if (!events || events.length === 0) {
        feed.innerHTML = '<div class="activity-empty">No recent activity</div>';
        return;
    }

    // Show up to 10 most recent events
    const items = events.slice(0, 10);

    feed.innerHTML = items.map(event => {
        const dotClass = getEventDotClass(event.type);
        const text = formatEventText(event);
        const timeAgo = formatEventTime(event.timestamp);

        return `<div class="activity-item">
            <div class="activity-dot ${dotClass}"></div>
            <div class="activity-item-content">
                <div class="activity-item-text">${text}</div>
                <div class="activity-item-time">${timeAgo}</div>
            </div>
        </div>`;
    }).join('');
}

function getEventDotClass(type) {
    switch (type) {
        case 'client_connect': return 'connect';
        case 'client_disconnect': return 'disconnect';
        case 'channel_switch': return 'switch';
        case 'user_created':
        case 'user_deleted': return 'user';
        case 'admin_login': return 'admin';
        case 'auth_toggle': return 'auth';
        default: return 'admin';
    }
}

function formatEventText(event) {
    const user = escapeHtml(event.username || 'System');
    switch (event.type) {
        case 'client_connect': return `<strong>${user}</strong> connected — ${escapeHtml(event.details)}`;
        case 'client_disconnect': return `<strong>${user}</strong> disconnected — ${escapeHtml(event.details)}`;
        case 'channel_switch': return `<strong>${user}</strong> ${escapeHtml(event.details)}`;
        case 'user_created': return `User <strong>${user}</strong> created`;
        case 'user_deleted': return `User <strong>${user}</strong> deleted`;
        case 'admin_login': return `<strong>${user}</strong> logged into dashboard`;
        case 'auth_toggle': return escapeHtml(event.details);
        default: return `${user}: ${escapeHtml(event.details)}`;
    }
}

function formatEventTime(timestamp) {
    const now = new Date();
    const then = new Date(timestamp);
    const seconds = Math.floor((now - then) / 1000);

    if (seconds < 60) return 'just now';
    if (seconds < 3600) return Math.floor(seconds / 60) + 'm ago';
    if (seconds < 86400) return Math.floor(seconds / 3600) + 'h ago';
    return Math.floor(seconds / 86400) + 'd ago';
}

function fetchActivityPage() {
    fetch('/api/activity')
    .then(r => r.json())
    .then(events => {
        const feed = document.getElementById('activity-page-feed');
        if (!feed) return;
        if (!events || events.length === 0) {
            feed.innerHTML = '<div style="text-align: center; color: #8B949E; padding: 30px;">No activity recorded yet</div>';
            return;
        }
        feed.innerHTML = events.map(event => {
            const dotClass = getEventDotClass(event.type);
            const text = formatEventText(event);
            const timeAgo = formatEventTime(event.timestamp);
            return `<div class="activity-item">
                <div class="activity-dot ${dotClass}"></div>
                <div class="activity-item-content">
                    <div class="activity-item-text">${text}</div>
                    <div class="activity-item-time">${timeAgo}</div>
                </div>
            </div>`;
        }).join('');
    })
    .catch(() => {});
}

// --- Server Time ---

function updateServerTime() {
    const el = document.getElementById('server-time');
    if (el) {
        el.textContent = new Date().toLocaleTimeString('en-US', { hour: 'numeric', minute: '2-digit', second: '2-digit', hour12: true });
    }
}
setInterval(updateServerTime, 1000);
updateServerTime();

// --- Create User ---

function showCreateUserModal() {
    document.getElementById('newUserName').value = '';
    document.getElementById('newUsername').value = '';
    document.getElementById('newPassword').value = '';
    document.getElementById('newUserRole').value = 'user';
    // Populate agency dropdown for super admins
    if (currentRole === 'super_admin') {
        populateAgencyDropdown('newUserAgency');
    }
    // Show/hide super_admin option based on role. Hiding an <option> via CSS
    // is unreliable in native selects, so add/remove the option outright.
    const roleSelect = document.getElementById('newUserRole');
    const superAdminOption = roleSelect.querySelector('option[value="super_admin"]');
    if (currentRole === 'super_admin') {
        if (!superAdminOption) {
            const opt = document.createElement('option');
            opt.value = 'super_admin';
            opt.textContent = 'Super Admin';
            opt.className = 'super-admin-only';
            roleSelect.appendChild(opt);
        }
    } else if (superAdminOption) {
        superAdminOption.remove();
    }
    showModal('createUserModal');
    document.getElementById('newUserName').focus();
}

function createUser() {
    const nameEl = document.getElementById('newUserName');
    const usernameEl = document.getElementById('newUsername');
    const passwordEl = document.getElementById('newPassword');
    const name = nameEl.value.trim();
    const username = usernameEl.value.trim();
    const password = passwordEl.value;

    if (!username || !password) {
        showStatus('user-status', 'Username and password are required', 'warning');
        return;
    }

    const body = { username, password };
    if (name) body.name = name;
    if (currentRole === 'super_admin') {
        const agencySelect = document.getElementById('newUserAgency');
        if (agencySelect && agencySelect.value) {
            body.agency_id = agencySelect.value;
        }
    }
    const roleSelect = document.getElementById('newUserRole');
    if (roleSelect && roleSelect.value) {
        body.role = roleSelect.value;
    }

    fetch('/api/users', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    })
    .then(r => r.text().then(text => ({ status: r.status, ok: r.ok, body: text })))
    .then(({ status, ok, body }) => {
        let data;
        try {
            data = JSON.parse(body);
        } catch (e) {
            throw new Error(`Server returned invalid JSON (${status}): ${body.substring(0, 100)}`);
        }

        if (!ok) {
            throw new Error(data.error || `Server error (${status})`);
        }

        closeModal('createUserModal');
        showStatus('user-status', `User "${username}" created successfully`, 'success');
        addLog(`User "${username}" created`, 'success');
        fetchUsers();

        // Show QR code with enrollment token (not the real password)
        const agencyForQr = data.agency_id || currentAgencyId || '';
        const enrollToken = data.enrollment_token || '';
        if (enrollToken) {
            showQRCodeWithEnrollment(username, enrollToken, agencyForQr);
        } else {
            showQRCode(username, agencyForQr);
        }
    })
    .catch(err => {
        showStatus('user-status', `Failed to create user: ${err.message}`, 'error');
        addLog(`Failed to create user: ${err.message}`, 'error');
    });
}

// --- Bulk user upload (CSV) ---

// Download the CSV template the server accepts.
function downloadUserTemplate() {
    window.location.href = '/api/users/template';
}

// Called when a CSV file is chosen for bulk upload.
function handleBulkUsersFile(input) {
    const file = input.files && input.files[0];
    if (!file) return;
    const form = new FormData();
    form.append('file', file);
    showStatus('user-status', `Uploading "${file.name}"…`, 'info');
    fetch('/api/users/bulk', { method: 'POST', body: form })
        .then(r => r.text().then(text => ({ ok: r.ok, status: r.status, body: text })))
        .then(({ ok, status, body }) => {
            let data;
            try { data = JSON.parse(body); }
            catch (e) { throw new Error(`Server returned invalid JSON (${status}): ${body.substring(0, 120)}`); }
            if (!ok) throw new Error(data.error || `Server error (${status})`);

            const { created = 0, skipped = 0, failed = 0, results = [] } = data;
            const kind = failed > 0 ? 'warning' : 'success';
            showStatus('user-status',
                `Bulk upload: ${created} created, ${skipped} skipped, ${failed} failed`, kind);
            addLog(`Bulk user upload: ${created} created, ${skipped} skipped, ${failed} failed`, kind);

            // Surface the specific problem rows so the admin can fix them.
            results.filter(r => r.status !== 'created').forEach(r => {
                addLog(`  row ${r.line} (${r.username || '—'}): ${r.status}${r.detail ? ' — ' + r.detail : ''}`,
                    r.status === 'error' ? 'error' : 'warning');
            });
            fetchUsers();
        })
        .catch(err => {
            showStatus('user-status', `Bulk upload failed: ${err.message}`, 'error');
            addLog(`Bulk upload failed: ${err.message}`, 'error');
        })
        .finally(() => { input.value = ''; }); // allow re-selecting the same file
}

// --- Edit User ---

var editingAgency = '';
var editingUserData = null;

function showEditUser(username, agency) {
    editingUsername = username;
    editingAgency = agency || '';

    // Find the user data from allUsers
    editingUserData = allUsers ? allUsers.find(u => u.username === username && (u.agency_id || 'default') === (agency || 'default')) : null;

    document.getElementById('editUserTitle').textContent = username;
    document.getElementById('editName').value = editingUserData ? (editingUserData.name || '') : '';
    document.getElementById('editUsername').value = username;
    document.getElementById('editNewPassword').value = '';

    // Populate agency dropdown for super admins
    if (currentRole === 'super_admin') {
        populateAgencyDropdown('editAgency');
        document.getElementById('editAgency').value = agency || 'default';
    }

    showModal('editUserModal');
    document.getElementById('editName').focus();
}

function saveUserEdits() {
    const name = document.getElementById('editName').value.trim();
    const newUsername = document.getElementById('editUsername').value.trim();
    const password = document.getElementById('editNewPassword').value;

    if (!newUsername) {
        showStatus('user-status', 'Username cannot be empty', 'warning');
        return;
    }

    const body = {};
    body.name = name;
    if (newUsername !== editingUsername) {
        body.new_username = newUsername;
    }
    if (currentRole === 'super_admin') {
        const newAgency = document.getElementById('editAgency').value;
        if (newAgency && newAgency !== editingAgency) {
            body.new_agency_id = newAgency;
        }
    }
    if (password) {
        body.password = password;
    }

    const agencyParam = editingAgency ? `?agency=${encodeURIComponent(editingAgency)}` : '';
    fetch(`/api/users/${encodeURIComponent(editingUsername)}${agencyParam}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    })
    .then(r => r.text().then(text => ({ status: r.status, ok: r.ok, body: text })))
    .then(({ status, ok, body }) => {
        let data;
        try {
            data = JSON.parse(body);
        } catch (e) {
            throw new Error(`Server returned invalid JSON (${status}): ${body.substring(0, 100)}`);
        }

        if (!ok) {
            throw new Error(data.error || `Server error (${status})`);
        }

        closeModal('editUserModal');
        showStatus('user-status', `User "${editingUsername}" updated`, 'success');
        addLog(`User "${editingUsername}" updated`, 'success');
        fetchUsers();
    })
    .catch(err => {
        showStatus('user-status', `Failed to update user: ${err.message}`, 'error');
        addLog(`Failed to update user: ${err.message}`, 'error');
    });
}

// --- Sign out phones ---

function signOutPhones(username, agency) {
    if (!confirm(`Sign out every phone using "${username}"?\n\nPhones enrolled by QR code lose their key and need a new QR code. Phones signed in with the password are disconnected and must sign in again. The password itself keeps working — reset it too if it may have been shared.`)) return;
    const agencyParam = agency ? `?agency=${encodeURIComponent(agency)}` : '';
    fetch(`/api/users/${encodeURIComponent(username)}/sign-out-phones${agencyParam}`, { method: 'POST' })
    .then(r => r.json().then(data => ({ ok: r.ok, data })))
    .then(({ ok, data }) => {
        if (!ok) throw new Error(data.error || 'Failed');
        showStatus('user-status', `Signed out phones for "${username}"`, 'success');
        addLog(`Signed out phones for "${username}" (${data.removed || 0} QR key(s) removed)`, 'success');
        fetchUsers();
    })
    .catch(err => showStatus('user-status', `Failed to sign out phones: ${err.message}`, 'error'));
}

// --- Delete User ---

function deleteUser(username, agency) {
    if (!confirm(`Delete user "${username}"? This cannot be undone.`)) return;

    const agencyParam = agency ? `?agency=${encodeURIComponent(agency)}` : '';
    fetch(`/api/users/${encodeURIComponent(username)}${agencyParam}`, { method: 'DELETE' })
    .then(r => {
        if (!r.ok) {
            return r.text().then(text => {
                try {
                    const d = JSON.parse(text);
                    throw new Error(d.error || 'Failed');
                } catch (e) {
                    if (e.message && e.message !== 'Failed') throw e;
                    throw new Error(`Server error (${r.status})`);
                }
            });
        }
        return r.json();
    })
    .then(() => {
        showStatus('user-status', `User "${username}" deleted`, 'success');
        addLog(`User "${username}" deleted`, 'success');
        fetchUsers();
    })
    .catch(err => {
        showStatus('user-status', `Failed to delete user: ${err.message}`, 'error');
        addLog(`Failed to delete user: ${err.message}`, 'error');
    });
}

// --- Update User Role ---

function updateUserRole(username, newRole, agency, dashboardPassword) {
    const agencyParam = agency ? `?agency=${encodeURIComponent(agency)}` : '';
    const body = { role: newRole };
    if (dashboardPassword) body.password = dashboardPassword;
    fetch(`/api/users/${encodeURIComponent(username)}/role${agencyParam}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    })
    .then(r => r.json().then(data => ({ ok: r.ok, data })))
    .then(({ ok, data }) => {
        if (!ok && data.error === 'needs_dashboard_password') {
            // First dashboard login for this account: the admin chooses its
            // password (never copied from the app or a QR code).
            const pw = prompt(`"${username}" has no dashboard login yet.\n\nEnter a dashboard password for them (at least 12 characters):`);
            if (!pw) { fetchUsers(); return null; }
            updateUserRole(username, newRole, agency, pw);
            return null;
        }
        if (!ok) throw new Error(data.error || 'Failed');
        const roleLabel = newRole === 'super_admin' ? 'Super Admin' : newRole === 'admin' ? 'Admin' : 'User';
        showStatus('user-status', `Role updated to "${roleLabel}" for "${username}"`, 'success');
        addLog(`Role updated for "${username}" to ${roleLabel}`, 'success');
    })
    .catch(err => {
        showStatus('user-status', `Failed to update role: ${err.message}`, 'error');
        addLog(`Failed to update role: ${err.message}`, 'error');
        fetchUsers(); // Revert the dropdown
    });
}

// --- QR Code ---

let qrExpiryInterval = null;

function showQRCode(username, agencyId) {
    // Generate a fresh enrollment token for existing user
    const agency = agencyId || currentAgencyId || '';
    const agencyParam = agency ? `?agency=${encodeURIComponent(agency)}` : '';

    fetch(`/api/users/${encodeURIComponent(username)}/enroll${agencyParam}`, {
        method: 'POST'
    })
    .then(r => r.json())
    .then(data => {
        if (data.enrollment_token) {
            showQRCodeWithEnrollment(username, data.enrollment_token, data.agency_id || agency);
        } else {
            // Fallback: show QR without enrollment token
            showQRCodeBasic(username, agency);
        }
    })
    .catch(() => {
        showQRCodeBasic(username, agency);
    });
}

function showQRCodeBasic(username, agencyId) {
    const serverUrl = window.location.hostname;
    const agency = agencyId || currentAgencyId || '';
    document.getElementById('qrCodeTitle').textContent = `QR Code for ${username}`;
    document.getElementById('qrServer').textContent = serverUrl;
    document.getElementById('qrUsername').textContent = username;
    document.getElementById('qrPassword').textContent = '(set in app)';

    const expiryEl = document.getElementById('qrExpiry');
    expiryEl.style.display = 'none';
    if (qrExpiryInterval) { clearInterval(qrExpiryInterval); qrExpiryInterval = null; }

    const container = document.getElementById('qrCodeCanvas');
    container.innerHTML = '';

    const qrObj = {
        server: serverUrl,
        port: pttPort,
        username: username
    };
    if (agency) qrObj.agency_id = agency;
    // Server identity key (older apps ignore it).
    if (serverIdentity && serverIdentity.public_key) qrObj.spk = serverIdentity.public_key;
    const fpEl = document.getElementById('qrServerKey');
    if (fpEl) fpEl.textContent = serverIdentity ? serverIdentity.fingerprint.toUpperCase().match(/.{1,4}/g).join(' ') : 'not available';
    const qrData = JSON.stringify(qrObj);

    new QRCode(container, {
        text: qrData,
        width: 256,
        height: 256,
        colorDark: '#000000',
        colorLight: '#ffffff',
        correctLevel: QRCode.CorrectLevel.L
    });

    showModal('qrCodeModal');
}

function showQRCodeWithEnrollment(username, enrollmentToken, agencyId) {
    const serverUrl = window.location.hostname;
    const agency = agencyId || currentAgencyId || '';
    document.getElementById('qrCodeTitle').textContent = `QR Enrollment for ${username}`;
    document.getElementById('qrServer').textContent = serverUrl;
    document.getElementById('qrUsername').textContent = username;
    document.getElementById('qrPassword').textContent = enrollmentToken;

    const container = document.getElementById('qrCodeCanvas');
    container.innerHTML = '';

    const qrObj = {
        server: serverUrl,
        port: pttPort,
        username: username,
        password: enrollmentToken
    };
    if (agency) qrObj.agency_id = agency;
    // Server identity key (older apps ignore it).
    if (serverIdentity && serverIdentity.public_key) qrObj.spk = serverIdentity.public_key;
    const fpEl = document.getElementById('qrServerKey');
    if (fpEl) fpEl.textContent = serverIdentity ? serverIdentity.fingerprint.toUpperCase().match(/.{1,4}/g).join(' ') : 'not available';
    const qrData = JSON.stringify(qrObj);

    new QRCode(container, {
        text: qrData,
        width: 256,
        height: 256,
        colorDark: '#000000',
        colorLight: '#ffffff',
        correctLevel: QRCode.CorrectLevel.L
    });

    // Start expiry countdown (15 minutes from now)
    startExpiryCountdown(15 * 60);
    showModal('qrCodeModal');
}

function startExpiryCountdown(seconds) {
    const expiryEl = document.getElementById('qrExpiry');
    expiryEl.style.display = 'block';
    let remaining = seconds;

    if (qrExpiryInterval) clearInterval(qrExpiryInterval);

    function updateExpiry() {
        if (remaining <= 0) {
            expiryEl.className = 'qr-expiry expired';
            expiryEl.textContent = 'Expired — generate a new QR code';
            clearInterval(qrExpiryInterval);
            qrExpiryInterval = null;
            return;
        }
        const mins = Math.floor(remaining / 60);
        const secs = remaining % 60;
        const timeStr = `${mins}:${secs.toString().padStart(2, '0')}`;

        if (remaining <= 120) {
            expiryEl.className = 'qr-expiry expiring';
            expiryEl.textContent = `One phone only · expires in ${timeStr}`;
        } else {
            expiryEl.className = 'qr-expiry active';
            expiryEl.textContent = `One phone only · expires in ${timeStr}`;
        }
        remaining--;
    }

    updateExpiry();
    qrExpiryInterval = setInterval(updateExpiry, 1000);
}

// Legacy function kept for compatibility
function showQRCodeWithData(username, password, agencyId) {
    showQRCodeWithEnrollment(username, password, agencyId);
}

// --- Mutual Aid ---

// ===== Mutual Aid — drag-and-drop patch builder =====
//
// Left column lists every agency as a draggable card. Right column holds one or
// more "patch" builders: drag 2+ agencies into a builder, name it, click Create
// → POST /api/mutual-aid. Existing patches render below as read-only cards.

let maBuilderSeq = 0;                 // unique id for each new-patch builder
const maBuilders = {};                // builderId -> Set(agencyId)

function fetchMutualAid() {
    // Load agencies (left pool) and existing links in parallel.
    Promise.all([
        fetch('/api/agencies').then(r => r.json()).catch(() => []),
        fetch('/api/mutual-aid').then(r => r.json()).catch(() => [])
    ]).then(([agencies, links]) => {
        agenciesList = agencies || [];
        renderAgencyPool(agenciesList);
        renderPatches(links || []);
    }).catch(err => console.error('Failed to load mutual aid:', err));
}

function renderAgencyPool(agencies) {
    const pool = document.getElementById('ma-agency-pool');
    const countEl = document.getElementById('ma-agency-count');
    if (!pool) return;
    if (countEl) countEl.textContent = agencies.length;

    if (!agencies.length) {
        pool.innerHTML = '<div class="ma-empty">No agencies yet. Create agencies first.</div>';
        return;
    }
    pool.innerHTML = agencies.map(a => `
        <div class="ma-agency-card" draggable="true" data-agency-id="${escapeAttr(a.id)}"
             ondragstart="maDragStart(event)" ondragend="maDragEnd(event)">
            <div class="ma-agency-name">${escapeHtml(a.name)}</div>
            <div class="ma-agency-id">${escapeHtml(a.id)}</div>
        </div>`).join('');
}

function renderPatches(links) {
    const wrap = document.getElementById('ma-patches');
    if (!wrap) return;

    // Preserve any open new-patch builders, then render existing links after them.
    const openBuilders = wrap.querySelectorAll('.ma-patch-builder');
    let existingHtml = '';
    if (links.length) {
        existingHtml = '<div class="ma-existing-label">Active Patches</div>' + links.map(link => {
            const safeId = escapeAttr(link.id);
            const names = (link.agency_names || link.agencies || []);
            const chips = (link.agencies || []).map((aid, i) =>
                `<span class="ma-chip">${escapeHtml(names[i] || aid)}</span>`).join('');
            const created = link.created_at ? new Date(link.created_at).toLocaleDateString() : '';
            return `<div class="ma-patch-card ma-patch-existing">
                <div class="ma-patch-card-head">
                    <span class="ma-patch-title">${escapeHtml(link.channel_name || ('Channel ' + link.channel))}</span>
                    <button class="ma-patch-remove" onclick="deleteMutualAidLink(${jsArg(link.id)})" title="Remove patch">&times;</button>
                </div>
                <div class="ma-patch-chips">${chips}</div>
                <div class="ma-patch-meta">Channel ${link.channel} &middot; ${(link.agencies||[]).length} agencies${created ? ' &middot; ' + created : ''}</div>
            </div>`;
        }).join('');
    }

    // If no open builders exist, start with one empty builder so the user can begin.
    if (openBuilders.length === 0) {
        wrap.innerHTML = '';
        addPatchBuilder();
        wrap.insertAdjacentHTML('beforeend', existingHtml);
    } else {
        // Keep builders, replace only the existing-patches section.
        let old = wrap.querySelector('.ma-existing-section');
        if (old) old.remove();
        wrap.insertAdjacentHTML('beforeend',
            `<div class="ma-existing-section">${existingHtml}</div>`);
    }
}

function addPatchBuilder() {
    const wrap = document.getElementById('ma-patches');
    if (!wrap) return;
    const id = 'mab-' + (++maBuilderSeq);
    maBuilders[id] = new Set();
    const html = `
        <div class="ma-patch-card ma-patch-builder" id="${id}">
            <input type="text" class="ma-patch-name" placeholder="Patch name (e.g. Fire Response)" />
            <div class="ma-dropzone" ondragover="maDragOver(event)" ondragleave="maDragLeave(event)"
                 ondrop="maDrop(event,'${id}')">
                <div class="ma-dropzone-hint">Drag agencies here (2 or more)</div>
                <div class="ma-dropzone-items"></div>
            </div>
            <div class="ma-patch-actions">
                <button class="btn-primary ma-create-btn" onclick="createPatch('${id}')" disabled>Create Patch</button>
                <button class="ma-patch-cancel" onclick="removeBuilder('${id}')">Clear</button>
            </div>
        </div>`;
    // Insert builders at the TOP (before existing patches).
    const existing = wrap.querySelector('.ma-existing-section, .ma-existing-label');
    if (existing) existing.insertAdjacentHTML('beforebegin', html);
    else wrap.insertAdjacentHTML('afterbegin', html);
}

function removeBuilder(id) {
    delete maBuilders[id];
    const el = document.getElementById(id);
    if (el) el.remove();
    // Always keep at least one builder available.
    const wrap = document.getElementById('ma-patches');
    if (wrap && !wrap.querySelector('.ma-patch-builder')) addPatchBuilder();
}

// --- drag handlers ---
function maDragStart(e) {
    e.dataTransfer.setData('text/plain', e.target.dataset.agencyId);
    e.dataTransfer.setData('agency-name', e.target.querySelector('.ma-agency-name').textContent);
    e.target.classList.add('ma-dragging');
    e.dataTransfer.effectAllowed = 'copy';
}
function maDragEnd(e) { e.target.classList.remove('ma-dragging'); }
function maDragOver(e) { e.preventDefault(); e.currentTarget.classList.add('ma-dropzone-over'); e.dataTransfer.dropEffect = 'copy'; }
function maDragLeave(e) { e.currentTarget.classList.remove('ma-dropzone-over'); }

function maDrop(e, builderId) {
    e.preventDefault();
    e.currentTarget.classList.remove('ma-dropzone-over');
    const agencyId = e.dataTransfer.getData('text/plain');
    const agencyName = e.dataTransfer.getData('agency-name') || agencyId;
    if (!agencyId) return;
    const set = maBuilders[builderId];
    if (!set || set.has(agencyId)) return; // no dupes
    set.add(agencyId);

    const items = document.querySelector(`#${builderId} .ma-dropzone-items`);
    const chip = document.createElement('span');
    chip.className = 'ma-chip ma-chip-removable';
    chip.dataset.agencyId = agencyId;
    chip.innerHTML = escapeHtml(agencyName) +
        ` <span class="ma-chip-x" onclick="removeFromPatch('${builderId}',${jsArg(agencyId)},this)">&times;</span>`;
    items.appendChild(chip);
    refreshBuilderState(builderId);
}

function removeFromPatch(builderId, agencyId, chipX) {
    const set = maBuilders[builderId];
    if (set) set.delete(agencyId);
    const chip = chipX.closest('.ma-chip');
    if (chip) chip.remove();
    refreshBuilderState(builderId);
}

function refreshBuilderState(builderId) {
    const el = document.getElementById(builderId);
    if (!el) return;
    const count = maBuilders[builderId] ? maBuilders[builderId].size : 0;
    const hint = el.querySelector('.ma-dropzone-hint');
    if (hint) hint.style.display = count > 0 ? 'none' : '';
    const btn = el.querySelector('.ma-create-btn');
    if (btn) {
        btn.disabled = count < 2;
        btn.textContent = count < 2 ? `Create Patch (${count}/2)` : `Create Patch (${count} agencies)`;
    }
}

function createPatch(builderId) {
    const el = document.getElementById(builderId);
    if (!el) return;
    const set = maBuilders[builderId];
    const agencies = set ? Array.from(set) : [];
    const nameInput = el.querySelector('.ma-patch-name');
    const channelName = nameInput.value.trim();

    if (agencies.length < 2) {
        showStatus('mutual-aid-status', 'Drag at least two agencies into the patch', 'warning');
        return;
    }
    if (!channelName) {
        showStatus('mutual-aid-status', 'Give the patch a name first', 'warning');
        nameInput.focus();
        return;
    }

    const btn = el.querySelector('.ma-create-btn');
    if (btn) { btn.disabled = true; btn.textContent = 'Creating…'; }

    fetch('/api/mutual-aid', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agencies: agencies, channel_name: channelName })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) {
            showStatus('mutual-aid-status', data.error, 'error');
            if (btn) { btn.disabled = false; refreshBuilderState(builderId); }
            return;
        }
        showStatus('mutual-aid-status', `Patch "${channelName}" created — all clients notified`, 'success');
        removeBuilder(builderId);   // clear this builder
        fetchMutualAid();           // reload existing patches
    })
    .catch(err => {
        showStatus('mutual-aid-status', 'Failed to create patch: ' + err.message, 'error');
        if (btn) { btn.disabled = false; refreshBuilderState(builderId); }
    });
}

function deleteMutualAidLink(id) {
    if (!confirm('Remove this patch? Agencies will no longer be able to communicate on this channel.')) {
        return;
    }
    fetch('/api/mutual-aid/' + id, { method: 'DELETE' })
    .then(r => r.json())
    .then(data => {
        if (data.error) {
            showStatus('mutual-aid-status', data.error, 'error');
            return;
        }
        showStatus('mutual-aid-status', 'Patch removed', 'success');
        fetchMutualAid();
    })
    .catch(err => {
        showStatus('mutual-aid-status', 'Failed to remove patch: ' + err.message, 'error');
    });
}

// Small helper for safe attribute values (single-quote / HTML safe).
function escapeAttr(s) {
    return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/'/g,'&#39;').replace(/"/g,'&quot;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}

// ===== Zello Bridges =====

let zelloBridgesPoll = null;

let zelloRenderKey = ''; // signature of the last full render (to avoid clobbering)

function fetchZelloBridges() {
    fetch('/api/zello-bridges').then(r => r.json()).then(bridges => {
        renderZelloBridges(bridges);
    }).catch(err => console.error('Failed to fetch zello bridges:', err));

    // Poll every 5s while on this page. IMPORTANT: only do a full re-render when
    // the bridge SET/connection-state actually changes — otherwise a poll would
    // rebuild the cards and reset an in-progress channel dropdown selection back
    // to the server value before the user hits Apply. When nothing structural
    // changed, just refresh the status dots in place.
    if (zelloBridgesPoll) clearInterval(zelloBridgesPoll);
    zelloBridgesPoll = setInterval(() => {
        const page = document.getElementById('page-zello-bridges');
        if (!page || !page.classList.contains('active')) {
            clearInterval(zelloBridgesPoll); zelloBridgesPoll = null; return;
        }
        fetch('/api/zello-bridges')
            .then(r => r.json())
            .then(b => {
                if (!Array.isArray(b)) return;
                const key = b.map(x => `${x.id}:${x.status}:${x.enabled}`).join('|');
                if (key === zelloRenderKey) {
                    updateZelloStatusInPlace(b);   // no structural change → don't rebuild
                } else {
                    renderZelloBridges(b);         // connection state changed → full render
                }
            })
            .catch(() => {});
    }, 5000);
}

// Update only the status dot/label + error text on existing cards, leaving the
// channel dropdowns (and any unsaved selection) untouched.
function updateZelloStatusInPlace(bridges) {
    bridges.forEach(b => {
        const card = document.querySelector(`[data-zbid="${CSS.escape(String(b.id))}"]`);
        if (!card) return;
        const dot = zelloStatusColor(b.status);
        const dotEl = card.querySelector('.zb-dot');
        const statusEl = card.querySelector('.zb-status');
        if (dotEl) { dotEl.style.background = dot; dotEl.style.boxShadow = `0 0 6px ${dot}66`; }
        if (statusEl) { statusEl.style.color = dot; statusEl.textContent = b.enabled ? (b.status || 'disconnected') : 'disabled'; }
    });
}

function zelloStatusColor(status) {
    return status === 'connected' ? '#34D399'
         : status === 'connecting' ? '#FBBF24'
         : status === 'error' ? '#F87171' : '#6E7681';
}

function renderZelloBridges(bridges) {
    const list = document.getElementById('zello-bridges-list');
    const empty = document.getElementById('zello-bridges-empty');
    if (!list) return;
    if (!Array.isArray(bridges)) bridges = []; // guard against error objects / non-arrays
    if (!bridges.length) {
        list.innerHTML = '';
        if (empty) empty.style.display = 'block';
        return;
    }
    if (empty) empty.style.display = 'none';
    list.innerHTML = bridges.map(b => {
        const dot = zelloStatusColor(b.status);
        const statusLabel = b.enabled ? (b.status || 'disconnected') : 'disabled';
        const err = b.last_error ? `<div class="zb-error">${escapeHtml(b.last_error)}</div>` : '';
        const safeId = escapeAttr(b.id);
        // Channel-mapping list (one row per Zello↔Mesh pair) shown once connected.
        const connected = b.status === 'connected';
        const mappings = Array.isArray(b.mappings) ? b.mappings : [];
        const rowsHtml = mappings.map((m, idx) =>
            mappingRowHtml(safeId, idx, m.zello_channel, m.mesh_channel,
                           m.agency_id || b.agency_id, m.extra_agencies || [])).join('');
        const mapRow = connected ? `
            <div class="zb-maps" id="zbmaps-${safeId}" data-agency="${escapeAttr(b.agency_id || '')}">
                ${rowsHtml || '<div class="zb-map-empty">No channels mapped yet. Add one below.</div>'}
                <div class="zb-map-controls">
                    <button class="btn-secondary zb-addmap" onclick="addMappingRow(${jsArg(b.id)})">+ Add channel</button>
                    <button class="btn-primary zb-apply" onclick="applyZelloMapping(${jsArg(b.id)})">Apply</button>
                </div>
            </div>` : '';

        return `<div class="zb-card" data-zbid="${safeId}">
            <div class="zb-card-main">
                <div class="zb-card-head">
                    <span class="zb-dot" style="background:${dot};box-shadow:0 0 6px ${dot}66;"></span>
                    <span class="zb-name">${escapeHtml(b.name)}</span>
                    <span class="zb-status" style="color:${dot};">${escapeHtml(statusLabel)}</span>
                    <span class="zb-net">${escapeHtml(b.network)} &middot; ${escapeHtml(b.agency_id)}</span>
                </div>
                ${mapRow}
                ${err}
            </div>
            <div class="zb-actions">
                <button class="btn-secondary zb-toggle" onclick="toggleZelloBridge(${jsArg(b.id)})">${b.enabled ? 'Disable' : 'Enable'}</button>
                <button class="btn-action" title="Guests can always listen. This decides whether they may transmit toward Zello." onclick="zelloGuests(${jsArg(b.id)}, ${b.allow_guests ? 'true' : 'false'})">Guests: ${b.allow_guests ? 'can transmit' : 'listen only'}</button>
                <button class="btn-action" onclick="editZelloBridge(${jsArg(b)})">Edit</button>
                <button class="btn-action delete" onclick="deleteZelloBridge(${jsArg(b.id)})">Delete</button>
            </div>
        </div>`;
    }).join('');

    // Remember what we rendered so the poll can tell if a rebuild is needed.
    zelloRenderKey = bridges.map(x => `${x.id}:${x.status}:${x.enabled}`).join('|');

    // After rendering, load the live Zello channel list into each connected card's
    // mapping dropdowns.
    bridges.filter(b => b.status === 'connected').forEach(b => loadCardZelloChannels(b.id));
}

// Agency <option>s for a mapping row (from the cached agenciesList).
function mappingAgencyOptions(sel) {
    const list = Array.isArray(agenciesList) ? agenciesList : [];
    if (list.length === 0 && sel) {
        // Fallback: at least show the currently-set agency if the list isn't loaded.
        return `<option value="${escapeAttr(sel)}" selected>${escapeHtml(sel)}</option>`;
    }
    return list.map(a =>
        `<option value="${escapeAttr(a.id)}" ${a.id === sel ? 'selected' : ''}>${escapeHtml(a.name || a.id)}</option>`
    ).join('');
}

// One mapping row: Zello channel → Agency → Also shared with → Mesh channel + ×.
function mappingRowHtml(safeId, idx, zello, mesh, agency, extras) {
    const z = zello || '';
    const ex = Array.isArray(extras) ? extras : [];
    return `<div class="zb-map" data-mapidx="${idx}">
        <div class="zb-map-field">
            <label>Zello channel</label>
            <select class="zbmap-zello">
                <option value="${escapeAttr(z)}" selected>${escapeHtml(z || '— select —')}</option>
            </select>
        </div>
        <span class="zb-arrow">&#8646;</span>
        <div class="zb-map-field">
            <label>Agency</label>
            <select class="zbmap-agency" onchange="onMappingAgencyChange(this)">${mappingAgencyOptions(agency || '')}</select>
        </div>
        <div class="zb-map-field zb-share">
            <label>Also shared with</label>
            <button type="button" class="zb-share-btn" onclick="toggleSharePanel(this)"
                    data-extras="${escapeAttr(ex.join(','))}">${escapeHtml(shareLabel(ex))}</button>
            <div class="zb-share-panel hidden">${sharePanelOptions(agency || '', ex)}</div>
        </div>
        <div class="zb-map-field">
            <label>MeshRelay channel</label>
            <select class="zbmap-mesh">${meshChannelOptions(mesh)}</select>
        </div>
        <button class="zb-map-remove" title="Remove" onclick="this.closest('.zb-map').remove()">&times;</button>
    </div>`;
}

// Summary text on the picker button.
function shareLabel(extras) {
    const n = (extras || []).length;
    if (n === 0) return 'None';
    if (n === 1) {
        const a = (agenciesList || []).find(x => x.id === extras[0]);
        return a ? (a.name || a.id) : extras[0];
    }
    return `${n} agencies`;
}

// Checkbox list of every agency EXCEPT the primary (already implied).
function sharePanelOptions(primary, extras) {
    const list = Array.isArray(agenciesList) ? agenciesList : [];
    if (list.length === 0) return `<div class="zb-share-empty">No agencies loaded</div>`;
    const sel = new Set(extras || []);
    const rows = list.map(a => {
        const isPrimary = a.id === primary;
        const label = escapeHtml(a.name || a.id);
        if (isPrimary) {
            return `<label class="zb-share-opt is-primary" title="Primary agency for this mapping">
                <input type="checkbox" checked disabled> ${label} <span>(primary)</span></label>`;
        }
        return `<label class="zb-share-opt">
            <input type="checkbox" value="${escapeAttr(a.id)}" ${sel.has(a.id) ? 'checked' : ''}
                   onchange="onShareToggle(this)"> ${label}</label>`;
    }).join('');
    return rows;
}

// Open/close one picker (closing any other that's open).
function toggleSharePanel(btn) {
    const panel = btn.parentElement.querySelector('.zb-share-panel');
    const wasHidden = panel.classList.contains('hidden');
    document.querySelectorAll('.zb-share-panel').forEach(p => p.classList.add('hidden'));
    if (wasHidden) panel.classList.remove('hidden');
}

// A checkbox changed: refresh the button's summary + stored value.
function onShareToggle(cb) {
    const field = cb.closest('.zb-share');
    const btn = field.querySelector('.zb-share-btn');
    const chosen = Array.from(field.querySelectorAll('.zb-share-panel input[type=checkbox]'))
        .filter(x => x.checked && !x.disabled).map(x => x.value);
    btn.setAttribute('data-extras', chosen.join(','));
    btn.textContent = shareLabel(chosen);
    btn.classList.toggle('has-sel', chosen.length > 0);
}

// Changing the PRIMARY agency rebuilds the panel so the new primary is shown as
// implied, and drops it from the extras if it was checked there.
function onMappingAgencyChange(sel) {
    const row = sel.closest('.zb-map');
    const field = row.querySelector('.zb-share');
    const btn = field.querySelector('.zb-share-btn');
    const panel = field.querySelector('.zb-share-panel');
    const extras = (btn.getAttribute('data-extras') || '').split(',')
        .filter(v => v && v !== sel.value);
    panel.innerHTML = sharePanelOptions(sel.value, extras);
    btn.setAttribute('data-extras', extras.join(','));
    btn.textContent = shareLabel(extras);
    btn.classList.toggle('has-sel', extras.length > 0);
}

// Clicking outside closes any open picker.
document.addEventListener('click', function (e) {
    if (e.target.closest('.zb-share')) return;
    document.querySelectorAll('.zb-share-panel').forEach(p => p.classList.add('hidden'));
});

// Add a blank mapping row to a card, then populate its Zello dropdown.
// Defaults the agency to the bridge's default agency (stashed on the card).
function addMappingRow(id) {
    const wrap = document.getElementById(`zbmaps-${id}`);
    if (!wrap) return;
    const controls = wrap.querySelector('.zb-map-controls');
    const emptyMsg = wrap.querySelector('.zb-map-empty');
    if (emptyMsg) emptyMsg.remove();
    const defAgency = wrap.getAttribute('data-agency') || '';
    const tmp = document.createElement('div');
    tmp.innerHTML = mappingRowHtml(id, Date.now(), '', 10, defAgency, []);
    const row = tmp.firstElementChild;
    controls.before(row);
    if (zelloCardChannels[id]) fillZelloSelect(row.querySelector('.zbmap-zello'), zelloCardChannels[id], '');
}

// MeshRelay channel <option>s for a Zello mapping. Zello has its own block in
// the server's channel map (0-49 talkgroups, 50-98 mutual aid and ad-hoc
// groups, 100-149 LMR bridges, 150-199 Zello), so a bridged channel arrives as
// a NEW named channel on clients and cannot collide with a talkgroup.
const ZELLO_CHANNEL_MIN = 150;
const ZELLO_CHANNEL_MAX = 199;

function meshChannelOptions(sel) {
    let o = '';
    for (let i = ZELLO_CHANNEL_MIN; i <= ZELLO_CHANNEL_MAX; i++) {
        o += `<option value="${i}" ${i === sel ? 'selected' : ''}>Bridge channel ${i}</option>`;
    }
    return o;
}

let zelloCardChannels = {}; // id -> cached channel names

function fillZelloSelect(sel, channels, keep) {
    if (!sel) return;
    const cur = keep !== undefined ? keep : sel.value;
    sel.innerHTML = channels.map(c =>
        `<option value="${escapeAttr(c)}" ${c === cur ? 'selected' : ''}>${escapeHtml(c)}</option>`).join('');
}

// Populate ALL mapping-row Zello dropdowns on a card from the live account list.
function loadCardZelloChannels(id) {
    const wrap = document.getElementById(`zbmaps-${id}`);
    if (!wrap) return;
    fetch(`/api/zello-bridges/${id}/channels`)
        .then(r => r.json())
        .then(data => {
            if (!data.channels) return;
            zelloCardChannels[id] = data.channels;
            document.querySelectorAll(`#zbmaps-${id} .zbmap-zello`).forEach(sel => {
                fillZelloSelect(sel, data.channels, sel.value);
            });
            // Re-fill agency dropdowns too (agenciesList may have loaded after render).
            document.querySelectorAll(`#zbmaps-${id} .zbmap-agency`).forEach(sel => {
                sel.innerHTML = mappingAgencyOptions(sel.value);
            });
            // Agencies may have loaded after the first render — rebuild each
            // share panel from its stored selection so it isn't left empty.
            document.querySelectorAll(`#zbmaps-${id} .zb-share`).forEach(field => {
                const btn = field.querySelector('.zb-share-btn');
                const panel = field.querySelector('.zb-share-panel');
                const primary = field.closest('.zb-map').querySelector('.zbmap-agency').value;
                const extras = (btn.getAttribute('data-extras') || '').split(',').filter(v => v);
                panel.innerHTML = sharePanelOptions(primary, extras);
                btn.textContent = shareLabel(extras);
                btn.classList.toggle('has-sel', extras.length > 0);
            });
        })
        .catch(() => {});
}

// legacy no-op kept for the old single-select signature callers (none now)
// Apply ALL channel mappings from a connected card (sends the full list).
function applyZelloMapping(id) {
    const rows = document.querySelectorAll(`#zbmaps-${id} .zb-map`);
    const mappings = [];
    rows.forEach(row => {
        const z = row.querySelector('.zbmap-zello');
        const m = row.querySelector('.zbmap-mesh');
        const a = row.querySelector('.zbmap-agency');
        const shareBtn = row.querySelector('.zb-share-btn');
        const extras = ((shareBtn && shareBtn.getAttribute('data-extras')) || '')
            .split(',').filter(v => v);
        if (z && z.value) mappings.push({
            zello_channel: z.value,
            mesh_channel: parseInt(m.value, 10) || 0,
            agency_id: (a && a.value) || '',
            extra_agencies: extras
        });
    });
    fetch(`/api/zello-bridges/${id}/map`, { method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({ mappings }) })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('zello-bridges-status', data.error, 'error'); return; }
        showStatus('zello-bridges-status', `Applied ${mappings.length} channel mapping(s)`, 'success');
        setTimeout(fetchZelloBridges, 800);
    })
    .catch(err => showStatus('zello-bridges-status', 'Failed: ' + err.message, 'error'));
}

function showCreateZelloBridgeModal() {
    // Re-enable the agency picker: the agency-admin path disables it, and the
    // element is shared between both roles.
    const agencySel = document.getElementById('zbAgency');
    if (agencySel) agencySel.disabled = false;
    document.getElementById('zelloBridgeModalTitle').textContent = 'Add Zello Connection';
    document.getElementById('zbEditId').value = '';
    ['zbName','zbNetwork','zbUsername','zbPassword','zbAuthToken'].forEach(id => document.getElementById(id).value = '');
    document.getElementById('zbZelloChannel').innerHTML = '';   // hidden; mapped on the card
    document.getElementById('zbMeshChannel').value = '0';
    document.getElementById('zbEnabled').checked = true;
    document.getElementById('zbEnabled').parentElement.style.display = '';
    document.getElementById('zelloBridgeError').style.display = 'none';
    populateZbAgencies('');
    showModal('zelloBridgeModal');
}

// Fetch the account's Zello channels via the server (classic ZelloWork REST) and
// populate the dropdown, so the user picks a real channel instead of typing it.
function editZelloBridge(b) {
    // The modal is shared with the agency-admin create path, which locks this.
    const agencySel = document.getElementById('zbAgency');
    if (agencySel) agencySel.disabled = false;
    document.getElementById('zelloBridgeModalTitle').textContent = 'Edit Zello Bridge';
    document.getElementById('zbEditId').value = b.id;
    document.getElementById('zbName').value = b.name || '';
    document.getElementById('zbNetwork').value = b.network || '';
    // Preserve the current channel mapping in the hidden fields (edited on the card).
    document.getElementById('zbZelloChannel').innerHTML =
        `<option value="${escapeAttr(b.zello_channel || '')}" selected></option>`;
    document.getElementById('zbMeshChannel').value = b.mesh_channel || 0;
    document.getElementById('zbUsername').value = b.username || '';
    document.getElementById('zbPassword').value = ''; // blank = keep existing
    document.getElementById('zbAuthToken').value = ''; // blank = keep existing
    document.getElementById('zbMeshChannel').value = b.mesh_channel || 0;
    document.getElementById('zbEnabled').checked = !!b.enabled;
    document.getElementById('zbEnabled').parentElement.style.display = 'none'; // enable via toggle instead
    document.getElementById('zelloBridgeError').style.display = 'none';
    populateZbAgencies(b.agency_id || '');
    showModal('zelloBridgeModal');
}

function populateZbAgencies(selected) {
    const sel = document.getElementById('zbAgency');
    fetch('/api/agencies').then(r => r.json()).then(agencies => {
        agenciesList = agencies || [];
        sel.innerHTML = agenciesList
            .filter(a => !String(a.id).startsWith('event-'))
            .map(a => `<option value="${escapeAttr(a.id)}" ${a.id===selected?'selected':''}>${escapeHtml(a.name)} (${escapeHtml(a.id)})</option>`)
            .join('');
    }).catch(()=>{ sel.innerHTML = '<option value="">Failed to load</option>'; });
}

function saveZelloBridge() {
    const id = document.getElementById('zbEditId').value;
    const errorEl = document.getElementById('zelloBridgeError');
    const body = {
        name: document.getElementById('zbName').value.trim(),
        network: document.getElementById('zbNetwork').value.trim(),
        zello_channel: (document.getElementById('zbZelloChannel').value || '').trim(),
        username: document.getElementById('zbUsername').value.trim(),
        password: document.getElementById('zbPassword').value,
        auth_token: document.getElementById('zbAuthToken').value.trim(),
        agency_id: document.getElementById('zbAgency').value,
        mesh_channel: parseInt(document.getElementById('zbMeshChannel').value, 10) || 0,
        enabled: document.getElementById('zbEnabled').checked
    };
    // Channel is assigned on the card after connecting, so it's not required here.
    if (!body.name || !body.network || !body.agency_id) {
        errorEl.textContent = 'Name, network, and agency are required';
        errorEl.style.display = 'block';
        return;
    }
    errorEl.style.display = 'none';
    // An agency admin creates through the agency endpoint, which stamps their
    // own agency as owner; the super_admin routes stay as they were.
    const agencyMode = currentRole !== 'super_admin';
    const method = id ? 'PUT' : 'POST';
    let path;
    if (agencyMode) {
        path = id ? '/api/zello-bridges/' + id : '/api/my-agency/zello-bridges';
    } else {
        path = id ? '/api/zello-bridges/' + id : '/api/zello-bridges';
    }
    fetch(path, { method, headers: {'Content-Type':'application/json'}, body: JSON.stringify(body) })
    .then(r => r.json())
    .then(data => {
        if (data.error) { errorEl.textContent = data.error; errorEl.style.display = 'block'; return; }
        closeModal('zelloBridgeModal');
        showStatus('zello-bridges-status', id ? 'Bridge updated' : 'Bridge created', 'success');
        if (agencyMode) fetchAgencyZelloBridges(); else fetchZelloBridges();
    })
    .catch(err => { errorEl.textContent = 'Failed to save: ' + err.message; errorEl.style.display = 'block'; });
}

function toggleZelloBridge(id) {
    fetch('/api/zello-bridges/' + id + '/toggle', { method: 'POST' })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('zello-bridges-status', data.error, 'error'); return; }
        fetchZelloBridges();
    }).catch(err => showStatus('zello-bridges-status', 'Failed: ' + err.message, 'error'));
}

function zelloGuests(id, allowed) {
    if (!allowed && !confirm('Let guests transmit on this Zello bridge?')) return;
    fetch('/api/zello-bridges/' + id + '/guests', { method: 'POST' })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('zello-bridges-status', data.error, 'error'); return; }
        fetchZelloBridges();
    }).catch(err => showStatus('zello-bridges-status', 'Failed: ' + err.message, 'error'));
}

function deleteZelloBridge(id) {
    if (!confirm('Delete this Zello bridge? It will disconnect from Zello.')) return;
    fetch('/api/zello-bridges/' + id, { method: 'DELETE' })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('zello-bridges-status', data.error, 'error'); return; }
        showStatus('zello-bridges-status', 'Bridge deleted', 'success');
        fetchZelloBridges();
    }).catch(err => showStatus('zello-bridges-status', 'Failed: ' + err.message, 'error'));
}

// --- Guest Events ---

function fetchGuestEvents() {
    fetch('/api/guest-events')
    .then(r => r.json())
    .then(events => renderGuestEventsTable(events))
    .catch(err => console.error('Failed to fetch guest events:', err));
}

function renderGuestEventsTable(events) {
    const tbody = document.getElementById('guest-events-table-body');
    const table = document.getElementById('guest-events-table');
    const empty = document.getElementById('guest-events-empty');

    if (!events || events.length === 0) {
        table.style.display = 'none';
        empty.style.display = 'block';
        return;
    }

    table.style.display = '';
    empty.style.display = 'none';

    tbody.innerHTML = events.map(ev => {
        const created = new Date(ev.created_at).toLocaleDateString();
        const expiresText = ev.expires_at ? new Date(ev.expires_at).toLocaleString() : 'Never';
        const connected = ev.connected || 0;
        return `<tr>
            <td><strong>${escapeHtml(ev.name)}</strong></td>
            <td>${escapeHtml(ev.agency_name || ev.agency_id)} <span class="agency-badge">${escapeHtml(ev.agency_id)}</span></td>
            <td>Ch ${escapeHtml(ev.channel)}</td>
            <td><span class="${connected > 0 ? 'status-active' : ''}">${escapeHtml(connected)} online</span></td>
            <td>${escapeHtml(expiresText)}</td>
            <td>${escapeHtml(created)}</td>
            <td class="actions-cell">
                <button class="btn-action qr" onclick="showGuestEventQR(${jsArg(ev.id)}, ${jsArg(ev.name)}, ${jsArg(ev.username)}, ${jsArg(ev.token)}, ${jsArg(ev.expires_at || null)}, ${jsArg(ev.agency_id || '')})">QR Code</button>
                <button class="btn-action delete" onclick="deleteGuestEvent(${jsArg(ev.id)}, ${jsArg(ev.name)})">End Event</button>
            </td>
        </tr>`;
    }).join('');
}

function showCreateGuestEventModal() {
    document.getElementById('guestEventName').value = '';
    document.getElementById('guestEventChannel').value = '0';
    document.getElementById('guestEventExpiry').value = '';
    document.getElementById('guestEventError').style.display = 'none';
    showModal('createGuestEventModal');

    // Load agencies for the "Include Agencies" multi-select.
    const list = document.getElementById('guestEventAgencyList');
    if (list) {
        list.innerHTML = '<div style="color:#6E7681;padding:8px 0;font-size:13px;">Loading agencies…</div>';
        // Only agencies the server will let this account bridge in: all for a
        // super admin; for an agency admin, its own plus those with a
        // standing agreement or an approved mutual-aid patch.
        fetch('/api/guest-events/agencies').then(r => r.json()).then(agencies => {
            const selectable = (Array.isArray(agencies) ? agencies : [])
                .filter(a => !String(a.id).startsWith('event-'))
                .sort((a, b) => String(a.name).localeCompare(String(b.name)));
            if (!selectable.length) {
                list.innerHTML = '<div style="color:#6E7681;padding:8px 0;font-size:13px;">No agencies to include. Agencies appear here once they have a mutual-aid agreement with yours.</div>';
                return;
            }
            list.innerHTML = selectable.map(a => `
                <label class="ge-agency-item">
                    <input type="checkbox" value="${escapeAttr(a.id)}">
                    <span class="ge-agency-name">${escapeHtml(a.name)}</span>
                    <span class="agency-badge">${escapeHtml(a.id)}</span>
                </label>`).join('');
        }).catch(() => {
            list.innerHTML = '<div style="color:#F87171;padding:8px 0;font-size:13px;">Failed to load agencies</div>';
        });
    }
}

function createGuestEvent() {
    const name = document.getElementById('guestEventName').value.trim();
    const channel = parseInt(document.getElementById('guestEventChannel').value, 10);
    const expiresIn = document.getElementById('guestEventExpiry').value;
    const errorEl = document.getElementById('guestEventError');

    if (!name) {
        errorEl.textContent = 'Event name is required';
        errorEl.style.display = 'block';
        return;
    }

    // Collect selected agencies to bridge into the event channel.
    const agencies = Array.from(
        document.querySelectorAll('#guestEventAgencyList input[type="checkbox"]:checked')
    ).map(cb => cb.value);

    errorEl.style.display = 'none';

    fetch('/api/guest-events', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, channel, expires_in: expiresIn, agencies })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) {
            errorEl.textContent = data.error;
            errorEl.style.display = 'block';
            return;
        }
        closeModal('createGuestEventModal');
        showStatus('guest-events-status', `Guest event "${data.name}" created`, 'success');
        fetchGuestEvents();
        // Show QR code immediately
        showGuestEventQR(data.id, data.name, data.username, data.token, data.expires_at || null, data.agency_id);
    })
    .catch(err => {
        errorEl.textContent = 'Failed to create event: ' + err.message;
        errorEl.style.display = 'block';
    });
}

function showGuestEventQR(id, name, username, token, expiresAt, agencyId) {
    const serverUrl = window.location.hostname;
    document.getElementById('guestQRTitle').textContent = `Guest: ${name}`;
    document.getElementById('guestQRServer').textContent = serverUrl;
    document.getElementById('guestQRUsername').textContent = username;
    document.getElementById('guestQRToken').textContent = token;

    const expiryRow = document.getElementById('guestQRExpiryRow');
    if (expiresAt) {
        expiryRow.style.display = '';
        document.getElementById('guestQRExpiry').textContent = new Date(expiresAt).toLocaleString();
    } else {
        expiryRow.style.display = 'none';
    }

    const container = document.getElementById('guestQRCanvas');
    container.innerHTML = '';

    const qrObj = { server: serverUrl, port: pttPort, username: username, password: token };
    if (agencyId) qrObj.agency_id = agencyId;
    new QRCode(container, {
        text: JSON.stringify(qrObj),
        width: 256,
        height: 256,
        colorDark: '#000000',
        colorLight: '#ffffff',
        correctLevel: QRCode.CorrectLevel.L
    });

    showModal('guestEventQRModal');
}

function deleteGuestEvent(id, name) {
    if (!confirm(`End guest event "${name}"?\n\nThis will immediately kick all connected clients and revoke the QR code.`)) {
        return;
    }

    fetch('/api/guest-events/' + encodeURIComponent(id), { method: 'DELETE' })
    .then(r => r.json())
    .then(data => {
        if (data.error) {
            showStatus('guest-events-status', data.error, 'error');
            return;
        }
        showStatus('guest-events-status', `Guest event "${name}" ended — clients kicked`, 'success');
        fetchGuestEvents();
    })
    .catch(err => {
        showStatus('guest-events-status', 'Failed to end event: ' + err.message, 'error');
    });
}

// --- Session & Auth ---

function checkSession() {
    return fetch('/api/session')
        .then(r => {
            if (!r.ok) {
                window.location.href = '/login.html';
                return false;
            }
            return r.json();
        })
        .then(data => {
            if (!data) return false;
            // Dispatch accounts use the /command console; the dashboard's
            // APIs are closed to them.
            if (data.role === 'dispatch') {
                window.location.href = '/command';
                return false;
            }
            currentRole = data.role || '';
            currentAgencyId = data.agency_id || '';
            pttPort = data.ptt_port || 5000;
            if (data.must_change_password) {
                showForcePasswordChange();
            }
            applyRoleUI();
            return true;
        })
        .catch(() => {
            window.location.href = '/login.html';
            return false;
        });
}

// Show/hide elements based on role
function applyRoleUI() {
    const isSuperAdmin = currentRole === 'super_admin';

    // Show/hide super-admin-only elements
    document.querySelectorAll('.super-admin-only').forEach(el => {
        el.style.display = isSuperAdmin ? '' : 'none';
    });

    // admin-only elements are visible to super_admin AND agency_admin
    const isAdmin = isSuperAdmin || currentRole === 'agency_admin';
    document.querySelectorAll('.admin-only').forEach(el => {
        el.style.display = isAdmin ? '' : 'none';
    });

    // Hide settings panels that agency admins shouldn't see
    if (!isSuperAdmin) {
        // Hide update, GitHub, and auth panels for agency admins
        document.querySelectorAll('#page-settings .panel').forEach((panel, i) => {
            // Keep only a basic view for agency admins
            if (i < 3) panel.style.display = 'none'; // version, github, auth panels
        });
        // Hide update banners and buttons
        const dashUpdateBtn = document.getElementById('dashboard-check-updates-btn');
        if (dashUpdateBtn) dashUpdateBtn.style.display = 'none';
        const dashVersion = document.getElementById('dashboard-version');
        if (dashVersion) dashVersion.style.display = 'none';
    }
}

function showForcePasswordChange() {
    // Hide the entire dashboard — only password change is allowed
    const layout = document.querySelector('.app-layout');
    if (layout) layout.style.display = 'none';
    const modal = document.getElementById('changeAdminPasswordModal');
    if (modal) {
        modal.style.display = 'flex';
        document.getElementById('adminNewPassword').focus();
    }
}

function changeAdminPassword() {
    const newPass = document.getElementById('adminNewPassword').value;
    const confirmPass = document.getElementById('adminConfirmPassword').value;
    const errorEl = document.getElementById('adminPasswordError');

    if (!newPass || newPass.length < 12) {
        errorEl.textContent = 'Password must be at least 12 characters';
        errorEl.style.display = 'block';
        return;
    }
    if (newPass !== confirmPass) {
        errorEl.textContent = 'Passwords do not match';
        errorEl.style.display = 'block';
        return;
    }

    fetch('/api/admin/change-password', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ new_password: newPass })
    })
    .then(r => r.json().then(data => ({ ok: r.ok, data })))
    .then(({ ok, data }) => {
        if (!ok) {
            errorEl.textContent = data.error || 'Failed to change password';
            errorEl.style.display = 'block';
            return;
        }
        // Password changed — dismiss modal, restore dashboard, re-check session
        document.getElementById('changeAdminPasswordModal').style.display = 'none';
        const layout = document.querySelector('.app-layout');
        if (layout) layout.style.display = 'flex';
        checkSession();
    })
    .catch(() => {
        errorEl.textContent = 'Connection error';
        errorEl.style.display = 'block';
    });
}

function logout() {
    fetch('/api/logout', { method: 'POST' })
        .finally(() => {
            window.location.href = '/login.html';
        });
}

// --- Agency Management ---

function fetchAgencies() {
    fetch('/api/agencies')
        .then(r => r.json())
        .then(agencies => {
            agenciesList = agencies;
            renderAgenciesTable(agencies);
            populateAgencyFilters();
        })
        .catch(err => console.error('Error fetching agencies:', err));
}

function renderAgenciesTable(agencies) {
    const tbody = document.getElementById('agencies-table-body');
    const emptyState = document.getElementById('agencies-empty');
    const table = document.getElementById('agencies-table');

    if (!agencies || agencies.length === 0) {
        tbody.innerHTML = '';
        table.style.display = 'none';
        emptyState.style.display = 'block';
        return;
    }

    table.style.display = 'table';
    emptyState.style.display = 'none';

    tbody.innerHTML = agencies.map(agency => {
        const safeName = escapeHtml(agency.name);
        const safeId = escapeHtml(agency.id);
        const isDefault = agency.id === 'default';
        const policy = floorPolicyOf(agency);
        return `
            <tr>
                <td><strong>${safeName}</strong></td>
                <td><code>${safeId}</code></td>
                <td>${agency.user_count || 0}</td>
                <td>
                    <select onchange="setAgencyFloorControl(${jsArg(agency.id)}, this.value, this)" style="margin-bottom: 0; width: auto;">
                        <option value="open" ${policy === 'open' ? 'selected' : ''}>Open</option>
                        <option value="busy" ${policy === 'busy' ? 'selected' : ''}>Busy tone</option>
                        <option value="queue" ${policy === 'queue' ? 'selected' : ''}>Queue and replay</option>
                    </select>
                </td>
                <td>
                    <div class="actions-cell">
                        ${!isDefault ? `<button class="btn-action delete" onclick="deleteAgency(${jsArg(agency.id)})">Delete</button>` : ''}
                    </div>
                </td>
            </tr>
        `;
    }).join('');
}

function populateAgencyFilters() {
    // Populate the users page filter dropdown
    const filter = document.getElementById('users-agency-filter');
    if (filter && agenciesList) {
        const currentVal = filter.value;
        filter.innerHTML = '<option value="">All Agencies</option>';
        agenciesList.forEach(a => {
            filter.innerHTML += `<option value="${escapeHtml(a.id)}">${escapeHtml(a.name)}</option>`;
        });
        filter.value = currentVal;
    }
}

function populateAgencyDropdown(selectId) {
    const select = document.getElementById(selectId);
    if (!select) return;
    // Fetch fresh list
    fetch('/api/agencies')
        .then(r => r.json())
        .then(agencies => {
            agenciesList = agencies;
            select.innerHTML = '<option value="">Select Agency</option>';
            agencies.forEach(a => {
                select.innerHTML += `<option value="${escapeHtml(a.id)}">${escapeHtml(a.name)}</option>`;
            });
        })
        .catch(() => {});
}

function showCreateAgencyModal() {
    document.getElementById('newAgencyName').value = '';
    document.getElementById('newAgencyId').value = '';
    document.getElementById('newAgencyMaxUsers').value = '100';
    document.getElementById('newAgencyMaxChannels').value = '20';
    showModal('createAgencyModal');
    document.getElementById('newAgencyName').focus();
}

function createAgency() {
    const name = document.getElementById('newAgencyName').value.trim();
    let id = document.getElementById('newAgencyId').value.trim().toLowerCase().replace(/[^a-z0-9_-]/g, '');
    const maxUsers = parseInt(document.getElementById('newAgencyMaxUsers').value) || 100;
    // Talkgroups only: 50+ is reserved for server-allocated channels
    // (mutual aid and ad-hoc groups, LMR bridges, Zello bridges).
    const maxChannels = parseInt(document.getElementById('newAgencyMaxChannels').value) || 50;

    if (!name || !id) {
        showStatus('agency-status', 'Name and ID are required', 'warning');
        return;
    }

    fetch('/api/agencies', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id, name, max_users: maxUsers, max_channels: maxChannels })
    })
    .then(r => r.json().then(data => ({ ok: r.ok, data })))
    .then(({ ok, data }) => {
        if (!ok) throw new Error(data.error || 'Failed');
        closeModal('createAgencyModal');
        showStatus('agency-status', `Agency "${name}" created`, 'success');
        addLog(`Agency "${name}" created`, 'success');
        fetchAgencies();
    })
    .catch(err => {
        showStatus('agency-status', `Failed: ${err.message}`, 'error');
    });
}

let agencyAdminAgencyId = '';

function showCreateAgencyAdminModal(agencyId, agencyName) {
    agencyAdminAgencyId = agencyId;
    document.getElementById('agencyAdminAgencyName').textContent = agencyName;
    document.getElementById('agencyAdminUsername').value = '';
    document.getElementById('agencyAdminPassword').value = '';
    showModal('createAgencyAdminModal');
    document.getElementById('agencyAdminUsername').focus();
}

function createAgencyAdmin() {
    const username = document.getElementById('agencyAdminUsername').value.trim();
    const password = document.getElementById('agencyAdminPassword').value;

    if (!username || !password) {
        showStatus('agency-status', 'Username and password required', 'warning');
        return;
    }

    fetch(`/api/agencies/${encodeURIComponent(agencyAdminAgencyId)}/admin`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username, password })
    })
    .then(r => r.json().then(data => ({ ok: r.ok, data })))
    .then(({ ok, data }) => {
        if (!ok) throw new Error(data.error || 'Failed');
        closeModal('createAgencyAdminModal');
        showStatus('agency-status', `Admin "${username}" created for agency`, 'success');
        addLog(`Agency admin "${username}" created`, 'success');
        fetchAgencies();
    })
    .catch(err => {
        showStatus('agency-status', `Failed: ${err.message}`, 'error');
    });
}

function deleteAgency(agencyId) {
    if (!confirm(`Delete agency "${agencyId}"? All users must be removed first.`)) return;

    fetch(`/api/agencies/${encodeURIComponent(agencyId)}`, { method: 'DELETE' })
    .then(r => r.json().then(data => ({ ok: r.ok, data })))
    .then(({ ok, data }) => {
        if (!ok) throw new Error(data.error || 'Failed');
        showStatus('agency-status', `Agency "${agencyId}" deleted`, 'success');
        addLog(`Agency "${agencyId}" deleted`, 'success');
        fetchAgencies();
    })
    .catch(err => {
        showStatus('agency-status', `Failed: ${err.message}`, 'error');
    });
}

// --- Server Update ---

let latestRelease = null;

function fetchVersionInfo() {
    fetch('/api/version')
        .then(r => r.json())
        .then(data => {
            const ver = data.version || 'Unknown';
            document.getElementById('current-version').textContent = ver;
            const dashVer = document.getElementById('dashboard-version');
            if (dashVer) dashVer.textContent = ver;
            const healthVer = document.getElementById('health-version');
            if (healthVer) healthVer.textContent = ver;
            const sidebarVer = document.getElementById('sidebar-version');
            if (sidebarVer) sidebarVer.textContent = ver;
            lastStats.version = ver;
        })
        .catch(err => console.error('Error fetching version:', err));
}

function checkForUpdates() {
    // Disable both buttons (settings + dashboard)
    const settingsBtn = document.getElementById('check-updates-btn');
    const dashboardBtn = document.getElementById('dashboard-check-updates-btn');
    const resultEl = document.getElementById('update-check-result');

    if (settingsBtn) { settingsBtn.disabled = true; settingsBtn.textContent = 'Checking...'; }
    if (dashboardBtn) { dashboardBtn.disabled = true; dashboardBtn.textContent = 'Checking...'; }
    if (resultEl) resultEl.style.display = 'none';

    fetch('/api/update/check', { method: 'POST' })
        .then(r => r.json())
        .then(data => {
            if (settingsBtn) { settingsBtn.disabled = false; settingsBtn.textContent = 'Check for Updates'; }
            if (dashboardBtn) { dashboardBtn.disabled = false; dashboardBtn.textContent = 'Check for Updates'; }
            if (resultEl) resultEl.style.display = 'block';

            if (data.error) {
                if (resultEl) {
                    resultEl.className = 'status-message error';
                    resultEl.textContent = data.error;
                }
                return;
            }

            if (data.managed) {
                // infra-TAK (or another platform) installs updates.
                if (resultEl) {
                    resultEl.className = 'status-message';
                    resultEl.textContent = data.message || 'Updates are installed by the platform that manages this server.';
                }
                return;
            }

            if (data.update_available) {
                latestRelease = data;
                if (resultEl) {
                    resultEl.className = 'status-message warning';
                    resultEl.innerHTML = `Update available: <strong>${escapeHtml(data.latest_version)}</strong> (current: ${escapeHtml(data.current_version)})`;
                }

                const versionText = `${data.current_version} → ${data.latest_version}`;

                // Show both update banners
                const settingsBanner = document.getElementById('update-banner');
                if (settingsBanner) {
                    settingsBanner.style.display = 'block';
                    document.getElementById('update-banner-version').textContent = versionText;
                }
                const dashBanner = document.getElementById('dashboard-update-banner');
                if (dashBanner) {
                    dashBanner.style.display = 'block';
                    document.getElementById('dashboard-update-banner-version').textContent = versionText;
                }
            } else {
                if (resultEl) {
                    resultEl.className = 'status-message success';
                    resultEl.textContent = 'You are running the latest version.';
                }
                // Hide both banners
                const settingsBanner = document.getElementById('update-banner');
                if (settingsBanner) settingsBanner.style.display = 'none';
                const dashBanner = document.getElementById('dashboard-update-banner');
                if (dashBanner) dashBanner.style.display = 'none';
            }
        })
        .catch(err => {
            if (settingsBtn) { settingsBtn.disabled = false; settingsBtn.textContent = 'Check for Updates'; }
            if (dashboardBtn) { dashboardBtn.disabled = false; dashboardBtn.textContent = 'Check for Updates'; }
            if (resultEl) {
                resultEl.style.display = 'block';
                resultEl.className = 'status-message error';
                resultEl.textContent = 'Failed to check for updates: ' + err.message;
            }
        });
}

function autoCheckForUpdates() {
    fetch('/api/update/check', { method: 'POST' })
        .then(r => r.json())
        .then(data => {
            if (data.error || !data.update_available) return;
            latestRelease = data;
            const versionText = `${data.current_version} → ${data.latest_version}`;
            const settingsBanner = document.getElementById('update-banner');
            if (settingsBanner) {
                settingsBanner.style.display = 'block';
                document.getElementById('update-banner-version').textContent = versionText;
            }
            const dashBanner = document.getElementById('dashboard-update-banner');
            if (dashBanner) {
                dashBanner.style.display = 'block';
                document.getElementById('dashboard-update-banner-version').textContent = versionText;
            }
        })
        .catch(() => {});
}

function showUpdateModal() {
    if (!latestRelease) return;

    document.getElementById('update-current-version').textContent = latestRelease.current_version;
    document.getElementById('update-new-version').textContent = latestRelease.latest_version;

    // Show release notes if available
    const notesEl = document.getElementById('update-release-notes');
    const notesContent = document.getElementById('update-notes-content');
    if (latestRelease.release_notes) {
        notesContent.textContent = latestRelease.release_notes;
        notesEl.style.display = 'block';
    } else {
        notesEl.style.display = 'none';
    }

    // Reset modal state
    document.getElementById('update-info').style.display = 'flex';
    document.getElementById('update-progress').style.display = 'none';
    document.getElementById('update-error').style.display = 'none';
    document.getElementById('update-success').style.display = 'none';
    document.getElementById('update-apply-btn').style.display = 'inline-block';
    document.getElementById('update-apply-btn').disabled = false;
    document.getElementById('update-apply-btn').textContent = 'Update Server';
    document.getElementById('update-close-btn').textContent = 'Cancel';

    showModal('updateModal');
}

// The modal itself IS the confirmation: it names the version and what will
// happen, and the endpoint is already super-admin gated. The old flow also
// demanded the password be re-typed, but the server never read it — the apply
// handler has no password field at all — so it added a step without adding any
// check.
function applyUpdate() {
    if (!latestRelease) return;

    const applyBtn = document.getElementById('update-apply-btn');
    applyBtn.disabled = true;
    applyBtn.textContent = 'Updating...';

    document.getElementById('update-progress').style.display = 'block';
    document.getElementById('update-error').style.display = 'none';

    fetch('/api/update/apply', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            binary_url: latestRelease.binary_url,
            binary_name: latestRelease.binary_name || '',
            web_url: latestRelease.web_url || '',
            checksum_url: latestRelease.checksum_url || ''
        })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) {
            showUpdateError(data.error);
            return;
        }
        // Start polling for progress
        startUpdatePolling();
    })
    .catch(err => {
        showUpdateError('Failed to start update: ' + err.message);
    });
}

let updatePollTimer = null;

function startUpdatePolling() {
    if (updatePollTimer) clearInterval(updatePollTimer);

    updatePollTimer = setInterval(() => {
        fetch('/api/update/status')
            .then(r => r.json())
            .then(status => {
                const bar = document.getElementById('update-progress-bar');
                const stage = document.getElementById('update-stage');

                bar.style.width = status.progress + '%';
                stage.textContent = formatStage(status.stage) + (status.message ? ' — ' + status.message : '');

                if (status.error) {
                    clearInterval(updatePollTimer);
                    updatePollTimer = null;
                    showUpdateError(status.error);
                    return;
                }

                if (!status.in_progress && status.stage === 'restarting') {
                    clearInterval(updatePollTimer);
                    updatePollTimer = null;
                    stage.textContent = 'Server restarting... Reconnecting shortly.';
                    bar.style.width = '100%';
                    waitForServerRestart();
                }
            })
            .catch(() => {
                // Server may be restarting — wait and try to reconnect
                clearInterval(updatePollTimer);
                updatePollTimer = null;
                document.getElementById('update-stage').textContent = 'Server restarting...';
                document.getElementById('update-progress-bar').style.width = '100%';
                waitForServerRestart();
            });
    }, 1000);
}

function waitForServerRestart() {
    const applyBtn = document.getElementById('update-apply-btn');
    applyBtn.style.display = 'none';

    let attempts = 0;
    const maxAttempts = 30;

    const checkInterval = setInterval(() => {
        attempts++;
        fetch('/api/version')
            .then(r => r.json())
            .then(data => {
                clearInterval(checkInterval);
                const successEl = document.getElementById('update-success');
                successEl.textContent = 'Update complete! Now running ' + data.version;
                successEl.style.display = 'block';
                document.getElementById('update-progress').style.display = 'none';
                document.getElementById('update-close-btn').textContent = 'Close';
                document.getElementById('current-version').textContent = data.version;
                const dashVer = document.getElementById('dashboard-version');
                if (dashVer) dashVer.textContent = data.version;
                document.getElementById('update-banner').style.display = 'none';
                const dashBanner = document.getElementById('dashboard-update-banner');
                if (dashBanner) dashBanner.style.display = 'none';
                document.getElementById('update-check-result').style.display = 'none';
                latestRelease = null;
                addLog('Server updated to ' + data.version, 'success');
            })
            .catch(() => {
                if (attempts >= maxAttempts) {
                    clearInterval(checkInterval);
                    showUpdateError('Server did not come back after restart. Check server manually.');
                }
            });
    }, 2000);
}

function showUpdateError(message) {
    const applyBtn = document.getElementById('update-apply-btn');
    applyBtn.disabled = false;
    applyBtn.textContent = 'Retry Update';
    document.getElementById('update-progress').style.display = 'none';

    const errorEl = document.getElementById('update-error');
    errorEl.textContent = message;
    errorEl.style.display = 'block';
    addLog('Update failed: ' + message, 'error');
}

function formatStage(stage) {
    const stages = {
        'downloading': 'Downloading update',
        'downloading_web': 'Downloading web files',
        'backing_up': 'Creating backup',
        'installing': 'Installing update',
        'restarting': 'Restarting server'
    };
    return stages[stage] || stage || 'Preparing';
}

// Wrap fetch to handle 401 and must_change_password globally, and ensure credentials are sent
const originalFetch = window.fetch;
window.fetch = function(url, options = {}) {
    // Always send cookies so the HttpOnly session cookie rides along
    options.credentials = options.credentials || 'same-origin';
    return originalFetch(url, options).then(response => {
        const urlStr = url.toString();
        if (response.status === 401 && !urlStr.includes('/api/session') && !urlStr.includes('/api/login')) {
            window.location.href = '/login.html';
        }
        // Handle must_change_password (403) — bounce to password-change screen
        if (response.status === 403 && !urlStr.includes('/api/admin/change-password')) {
            response.clone().json().then(data => {
                if (data.error === 'must_change_password') {
                    showForcePasswordChange();
                }
            }).catch(() => {});
        }
        return response;
    });
};

// Initialize on page load
document.addEventListener('DOMContentLoaded', () => {
    // Check session first, then initialize
    checkSession().then(valid => {
        if (!valid) return;

        connectWebSocket();
        fetchServerIdentity();
        fetchUsers();
        fetchVersionInfo();
        if (currentRole === 'super_admin') {
            fetchAgencies();
            // Auto-check for updates on login
            autoCheckForUpdates();
        }

        // Set up navigation click handlers
        document.querySelectorAll('.nav-item').forEach(item => {
            item.addEventListener('click', () => {
                if (item.dataset.href) {
                    window.open(item.dataset.href, '_blank', 'noopener');
                    return;
                }
                const page = item.dataset.page;
                if (page) navigateTo(page);
            });
        });

        // Logout button
        const logoutBtn = document.getElementById('nav-logout');
        if (logoutBtn) {
            logoutBtn.addEventListener('click', logout);
        }

        // Allow Enter key to submit modals
        document.getElementById('newPassword').addEventListener('keydown', (e) => {
            if (e.key === 'Enter') createUser();
        });
        document.getElementById('editNewPassword').addEventListener('keydown', (e) => {
            if (e.key === 'Enter') saveUserEdits();
        });
        document.getElementById('adminConfirmPassword').addEventListener('keydown', (e) => {
            if (e.key === 'Enter') changeAdminPassword();
        });
    });
});


// ===== LMR Bridges — drag-and-drop radio bridge builder =====
//
// Same interaction as Mutual Aid: drag agencies from the left pool onto a
// bridge. The difference is what a bridge IS — a radio gateway with its own
// login, on a channel from the dedicated LMR block (100-119), shared by every
// agency dropped onto it. Agencies see it as a named channel ("LMR Bridge")
// pushed the same way a mutual-aid patch is.
//
// The agency cards reuse the ma-* drag handlers, so a card dragged here behaves
// exactly as it does on the Mutual Aid page.

let lmrBuilderSeq = 0;                 // unique id per new-bridge builder
const lmrBuilders = {};                // builderId -> Set(agencyId)
const lmrEditSets = {};                // bridgeId  -> Set(agencyId) for live edits

function fetchLmrBridges() {
    Promise.all([
        fetch('/api/agencies').then(r => r.json()).catch(() => []),
        fetch('/api/lmr-bridges').then(r => r.json()).catch(() => [])
    ]).then(([agencies, bridges]) => {
        agenciesList = agencies || [];
        renderLmrAgencyPool(agenciesList);
        renderLmrBridges(bridges || []);
    }).catch(err => console.error('Failed to load LMR bridges:', err));
}

function renderLmrAgencyPool(agencies) {
    const pool = document.getElementById('lmr-agency-pool');
    const countEl = document.getElementById('lmr-agency-count');
    if (!pool) return;
    if (countEl) countEl.textContent = agencies.length;

    if (!agencies.length) {
        pool.innerHTML = '<div class="ma-empty">No agencies yet. Create agencies first.</div>';
        return;
    }
    pool.innerHTML = agencies.map(a => `
        <div class="ma-agency-card" draggable="true" data-agency-id="${escapeAttr(a.id)}"
             ondragstart="maDragStart(event)" ondragend="maDragEnd(event)">
            <div class="ma-agency-name">${escapeHtml(a.name)}</div>
            <div class="ma-agency-id">${escapeHtml(a.id)}</div>
        </div>`).join('');
}

function renderLmrBridges(bridges) {
    const wrap = document.getElementById('lmr-bridges-list');
    if (!wrap) return;

    let existingHtml = '';
    if (bridges.length) {
        existingHtml = '<div class="ma-existing-label">Active Bridges</div>' + bridges.map(b => {
            const safeId = escapeAttr(b.id);
            const names = b.agency_names || b.agencies || [];
            // Existing bridges are editable: each chip can be removed, and the
            // card is itself a dropzone for adding more agencies.
            const chips = (b.agencies || []).map((aid, i) =>
                `<span class="ma-chip">${escapeHtml(names[i] || aid)}<button class="ma-chip-x"
                    onclick="lmrRemoveAgency(${jsArg(b.id)},${jsArg(aid)})"
                    title="Remove agency">&times;</button></span>`).join('');
            const dot = b.online ? 'lmr-dot-on' : 'lmr-dot-off';
            const state = b.enabled ? 'Enabled' : 'Disabled';
            return `<div class="ma-patch-card ma-patch-existing" id="lmr-card-${safeId}">
                <div class="ma-patch-card-head">
                    <span class="ma-patch-title">
                        <span class="lmr-dot ${dot}" title="${b.online ? 'Gateway connected' : 'Gateway offline'}"></span>
                        ${escapeHtml(b.channel_name || b.name || 'LMR Bridge')}
                    </span>
                    <button class="ma-patch-remove" onclick="deleteLmrBridge(${jsArg(b.id)})" title="Remove bridge">&times;</button>
                </div>
                <div class="ma-dropzone lmr-edit-zone"
                     ondragover="maDragOver(event)" ondragleave="maDragLeave(event)"
                     ondrop="lmrDropExisting(event,${jsArg(b.id)})">
                    <div class="ma-patch-chips">${chips}</div>
                    <div class="ma-dropzone-hint lmr-edit-hint">Drop an agency here to add it</div>
                </div>
                <div class="ma-patch-meta">
                    Channel ${b.channel} &middot; gateway login <code>${escapeHtml(b.username || '')}</code>
                    &middot; ${(b.agencies || []).length} agencies
                    &middot; <a href="#" onclick="toggleLmrBridge(${jsArg(b.id)});return false;">${state}</a>
                </div>
                <div class="lmr-actions">
                    <button class="btn-secondary lmr-qr-btn"
                            onclick="showLmrBridgeQr(${jsArg(b.username || '')},${jsArg(b.agencies && b.agencies[0] || '')})">
                        Enrollment QR
                    </button>
                </div>
                ${lmrAtakSection(b, '/api/lmr-bridges/', lmrAtakSuperState(b))}
            </div>`;
        }).join('');
    }

    const openBuilders = wrap.querySelectorAll('.ma-patch-builder');
    if (openBuilders.length === 0) {
        wrap.innerHTML = '';
        addLmrBuilder();
        wrap.insertAdjacentHTML('beforeend', existingHtml);
    } else {
        const old = wrap.querySelector('.ma-existing-section');
        if (old) old.remove();
        wrap.insertAdjacentHTML('beforeend', `<div class="ma-existing-section">${existingHtml}</div>`);
    }
}

function addLmrBuilder() {
    const wrap = document.getElementById('lmr-bridges-list');
    if (!wrap) return;
    const id = 'lmrb-' + (++lmrBuilderSeq);
    lmrBuilders[id] = new Set();
    const html = `
        <div class="ma-patch-card ma-patch-builder" id="${id}">
            <input type="text" class="ma-patch-name lmr-name" placeholder="Bridge name (e.g. County VHF)" />
            <div class="lmr-cred-row">
                <input type="text" class="lmr-user" placeholder="Gateway username" autocomplete="off" />
                <input type="password" class="lmr-pass" placeholder="Gateway password" autocomplete="new-password" />
            </div>
            <div class="ma-dropzone" ondragover="maDragOver(event)" ondragleave="maDragLeave(event)"
                 ondrop="lmrDrop(event,'${id}')">
                <div class="ma-dropzone-hint">Drag agencies here (one or more)</div>
                <div class="ma-dropzone-items"></div>
            </div>
            <div class="ma-patch-actions">
                <button class="btn-primary lmr-create-btn" onclick="createLmrBridge('${id}')" disabled>Create Bridge</button>
                <button class="ma-patch-cancel" onclick="removeLmrBuilder('${id}')">Clear</button>
            </div>
        </div>`;
    const existing = wrap.querySelector('.ma-existing-section, .ma-existing-label');
    if (existing) existing.insertAdjacentHTML('beforebegin', html);
    else wrap.insertAdjacentHTML('afterbegin', html);
}

function removeLmrBuilder(id) {
    delete lmrBuilders[id];
    const el = document.getElementById(id);
    if (el) el.remove();
    const wrap = document.getElementById('lmr-bridges-list');
    if (wrap && !wrap.querySelector('.ma-patch-builder')) addLmrBuilder();
}

// Drop onto a NEW-bridge builder.
function lmrDrop(e, builderId) {
    e.preventDefault();
    e.currentTarget.classList.remove('ma-dropzone-over');
    const agencyId = e.dataTransfer.getData('text/plain');
    const agencyName = e.dataTransfer.getData('agency-name') || agencyId;
    if (!agencyId) return;
    const set = lmrBuilders[builderId];
    if (!set || set.has(agencyId)) return;
    set.add(agencyId);

    const items = document.querySelector(`#${builderId} .ma-dropzone-items`);
    if (items) {
        items.insertAdjacentHTML('beforeend',
            `<span class="ma-chip" data-agency-id="${escapeAttr(agencyId)}">${escapeHtml(agencyName)}<button
                class="ma-chip-x" onclick="lmrUndrop('${builderId}',${jsArg(agencyId)})">&times;</button></span>`);
    }
    refreshLmrBuilderState(builderId);
}

function lmrUndrop(builderId, agencyId) {
    const set = lmrBuilders[builderId];
    if (set) set.delete(agencyId);
    const chip = document.querySelector(`#${builderId} .ma-chip[data-agency-id="${CSS.escape(agencyId)}"]`);
    if (chip) chip.remove();
    refreshLmrBuilderState(builderId);
}

function refreshLmrBuilderState(builderId) {
    const el = document.getElementById(builderId);
    if (!el) return;
    const set = lmrBuilders[builderId];
    const btn = el.querySelector('.lmr-create-btn');
    const hint = el.querySelector('.ma-dropzone-hint');
    const n = set ? set.size : 0;
    if (btn) {
        btn.disabled = n < 1;
        btn.textContent = 'Create Bridge';
    }
    if (hint) hint.style.display = n ? 'none' : '';
}

function createLmrBridge(builderId) {
    const el = document.getElementById(builderId);
    if (!el) return;
    const set = lmrBuilders[builderId];
    const agencies = set ? Array.from(set) : [];
    const name = el.querySelector('.lmr-name').value.trim();
    const username = el.querySelector('.lmr-user').value.trim();
    const password = el.querySelector('.lmr-pass').value;

    if (!agencies.length) {
        showStatus('lmr-status', 'Drag at least one agency onto the bridge', 'warning');
        return;
    }
    if (!username || !password) {
        showStatus('lmr-status', 'Gateway username and password are required', 'warning');
        return;
    }

    const btn = el.querySelector('.lmr-create-btn');
    if (btn) { btn.disabled = true; btn.textContent = 'Creating…'; }

    fetch('/api/lmr-bridges', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            name: name, username: username, password: password,
            agencies: agencies, channel_name: name || 'LMR Bridge'
        })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) {
            showStatus('lmr-status', data.error, 'error');
            if (btn) { btn.disabled = false; refreshLmrBuilderState(builderId); }
            return;
        }
        showStatus('lmr-status',
            `Bridge created on channel ${data.channel} — point the gateway at login "${username}"`, 'success');
        removeLmrBuilder(builderId);
        fetchLmrBridges();
    })
    .catch(err => {
        showStatus('lmr-status', 'Failed to create bridge: ' + err.message, 'error');
        if (btn) { btn.disabled = false; refreshLmrBuilderState(builderId); }
    });
}

// --- editing an existing bridge -------------------------------------------

// Drop an agency onto an EXISTING bridge card to add it.
function lmrDropExisting(e, bridgeId) {
    e.preventDefault();
    e.currentTarget.classList.remove('ma-dropzone-over');
    const agencyId = e.dataTransfer.getData('text/plain');
    if (!agencyId) return;
    lmrPatchAgencies(bridgeId, agencyId, 'add');
}

function lmrRemoveAgency(bridgeId, agencyId) {
    lmrPatchAgencies(bridgeId, agencyId, 'remove');
}

// Read the bridge's current agencies, apply one add/remove, PUT the new set.
function lmrPatchAgencies(bridgeId, agencyId, op) {
    fetch('/api/lmr-bridges').then(r => r.json()).then(list => {
        const b = (list || []).find(x => x.id === bridgeId);
        if (!b) return;
        let agencies = (b.agencies || []).slice();
        if (op === 'add') {
            if (agencies.includes(agencyId)) return;   // already on the bridge
            agencies.push(agencyId);
        } else {
            if (agencies.length <= 1) {
                showStatus('lmr-status', 'A bridge needs at least one agency', 'warning');
                return;
            }
            agencies = agencies.filter(a => a !== agencyId);
        }
        return fetch('/api/lmr-bridges/' + bridgeId, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ agencies: agencies })
        })
        .then(r => r.json())
        .then(data => {
            if (data.error) { showStatus('lmr-status', data.error, 'error'); return; }
            showStatus('lmr-status',
                op === 'add' ? 'Agency added — clients notified' : 'Agency removed — clients notified',
                'success');
            fetchLmrBridges();
        });
    })
    .catch(err => showStatus('lmr-status', 'Failed to update bridge: ' + err.message, 'error'));
}

function toggleLmrBridge(id) {
    fetch('/api/lmr-bridges/' + id + '/toggle', { method: 'POST' })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('lmr-status', data.error, 'error'); return; }
        showStatus('lmr-status', data.enabled ? 'Bridge enabled' : 'Bridge disabled', 'success');
        fetchLmrBridges();
    })
    .catch(err => showStatus('lmr-status', 'Failed to toggle bridge: ' + err.message, 'error'));
}

function deleteLmrBridge(id) {
    if (!confirm('Remove this bridge? Its channel is withdrawn from all agencies and the gateway login is deleted.')) {
        return;
    }
    fetch('/api/lmr-bridges/' + id, { method: 'DELETE' })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('lmr-status', data.error, 'error'); return; }
        showStatus('lmr-status', 'Bridge removed', 'success');
        fetchLmrBridges();
    })
    .catch(err => showStatus('lmr-status', 'Failed to remove bridge: ' + err.message, 'error'));
}


/**
 * Show the enrollment QR for a bridge's gateway login.
 *
 * This is the same short-lived enrollment token the Users tab issues — the
 * bridge owns the account, so the QR is surfaced here rather than making the
 * operator go find the user. Scanning it in the MeshRelay Gateway app is the
 * whole of gateway setup: server, username and token in one code.
 */
function showLmrBridgeQr(username, agencyId) {
    if (!username) {
        showStatus('lmr-status', 'This bridge has no gateway login', 'warning');
        return;
    }
    showQRCode(username, agencyId || '');
}


// ===== Mutual Aid — agency-admin view =====
//
// Deliberately the same interaction as the super_admin builder: a pool of
// agencies on the left, a drop target on the right. The difference is what the
// action MEANS — an agency requests a patch with one partner, rather than
// creating one across many — so the dropzone holds a single agency and the
// button reads "Request Patch".
//
// A request waits for the other agency to approve, unless they already list us
// as a standing agreement, in which case it links immediately.

let maDirectoryCache = [];
let maDiscovery = null;
let maPendingReq = [];
let maPatches = [];
let maDropped = null;          // the agency currently in the dropzone

function fetchAgencyMutualAid() {
    const arr = v => Array.isArray(v) ? v : [];
    Promise.all([
        fetch('/api/my-agency/discovery').then(r => r.json()).catch(() => null),
        fetch('/api/mutual-aid/requests').then(r => r.json()).catch(() => []),
        fetch('/api/agencies/discoverable').then(r => r.json()).catch(() => []),
        fetch('/api/my-agency/mutual-aid').then(r => r.json()).catch(() => [])
    ]).then(([discovery, requests, directory, links]) => {
        maDiscovery = discovery;
        maPendingReq = arr(requests).filter(r => r.status === 'pending');
        maDirectoryCache = arr(directory);
        maPatches = arr(links);
        // Render each section independently: one failing endpoint must not
        // blank the others.
        const safe = (name, fn) => {
            try { fn(); } catch (e) { console.error('Mutual aid: ' + name, e); }
        };
        safe('discovery', renderDiscovery);
        safe('directory', () => filterDirectory());
        safe('patches', renderAgencyPatches);
    }).catch(err => console.error('Failed to load mutual aid:', err));
}

function renderDiscovery() {
    const cb = document.getElementById('ma-discoverable');
    if (cb && maDiscovery) cb.checked = !!maDiscovery.discoverable;
}

function saveDiscovery() {
    const cb = document.getElementById('ma-discoverable');
    fetch('/api/my-agency/discovery', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ discoverable: cb.checked })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('mutual-aid-status', data.error, 'error'); return; }
        showStatus('mutual-aid-status',
            cb.checked ? 'Your agency is now discoverable to other agencies'
                       : 'Your agency is hidden from the directory', 'success');
        fetchAgencyMutualAid();
    })
    .catch(err => showStatus('mutual-aid-status', 'Failed to save: ' + err.message, 'error'));
}

// --- left column: draggable agency cards ---

function filterDirectory() {
    const box = document.getElementById('ma-dir-search');
    const q = (box ? box.value : '').trim().toLowerCase();
    const shown = q
        ? maDirectoryCache.filter(a => (a.name || '').toLowerCase().includes(q)
                                    || (a.id || '').toLowerCase().includes(q))
        : maDirectoryCache;
    renderDirectoryPool(shown, q);
}

function renderDirectoryPool(agencies, query) {
    const pool = document.getElementById('ma-directory');
    const count = document.getElementById('ma-dir-count');
    const search = document.getElementById('ma-dir-search-row');
    if (!pool) return;
    if (count) count.textContent = maDirectoryCache.length;
    if (search) search.style.display = maDirectoryCache.length > 5 ? '' : 'none';

    if (!maDiscovery || !maDiscovery.discoverable) {
        pool.innerHTML = `<div class="ma-empty">
            Turn on <strong>Discoverable</strong> above to browse other agencies.
            Discovery is mutual &mdash; agencies that hide themselves are not listed.
        </div>`;
        return;
    }
    if (!agencies.length) {
        pool.innerHTML = query
            ? `<div class="ma-empty">No agency matches &ldquo;${escapeHtml(query)}&rdquo;.</div>`
            : '<div class="ma-empty">No other agencies are discoverable yet.</div>';
        return;
    }

    pool.innerHTML = agencies.map(a => {
        // An agency already patched or mid-request cannot be dragged again.
        const busy = a.state !== 'none';
        const label = a.state === 'linked' ? 'Patched'
                    : a.state === 'requested' ? 'Requested'
                    : a.state === 'awaiting_you' ? 'Awaiting you' : '';
        const trust = a.we_trust_them
            ? `<button class="ma-trust-btn ma-trust-on" onclick="toggleTrust(event,${jsArg(a.id)},false)"
                 title="You auto-accept their requests. Click to stop.">Trusted</button>`
            : `<button class="ma-trust-btn" onclick="toggleTrust(event,${jsArg(a.id)},true)"
                 title="Auto-accept future requests from this agency.">Trust</button>`;
        return `<div class="ma-agency-card${busy ? ' ma-agency-busy' : ''}"
                     ${busy ? '' : 'draggable="true"'}
                     data-agency-id="${escapeAttr(a.id)}"
                     ondragstart="maDragStart(event)" ondragend="maDragEnd(event)">
            <div class="ma-agency-name">${escapeHtml(a.name)}</div>
            <div class="ma-agency-id">${busy ? escapeHtml(label) : escapeHtml(a.id)}</div>
            ${a.auto_accepts_us && !busy
                ? '<div class="ma-agency-note">Auto-accepts &mdash; links instantly</div>' : ''}
            ${trust}
        </div>`;
    }).join('');
}

// --- right column: request builder, pending requests, live patches ---

function renderAgencyPatches() {
    const wrap = document.getElementById('ma-patches-agency');
    const count = document.getElementById('ma-request-count');
    if (!wrap) return;
    if (count) count.textContent = maPendingReq.length + maPatches.length;

    let html = requestBuilderHtml();

    if (maPendingReq.length) {
        html += '<div class="ma-existing-label">Pending Requests</div>';
        html += maPendingReq.map(r => {
            const inbound = r.direction === 'inbound';
            const who = inbound ? r.from_name : r.to_name;
            const safeId = escapeAttr(r.id);
            const actions = inbound
                ? `<button class="btn-primary" onclick="approveMaRequest(${jsArg(r.id)})">Approve</button>
                   <button class="ma-patch-cancel" onclick="declineMaRequest(${jsArg(r.id)})">Decline</button>`
                : `<button class="ma-patch-cancel" onclick="declineMaRequest(${jsArg(r.id)})">Withdraw</button>`;
            return `<div class="ma-patch-card">
                <div class="ma-patch-card-head">
                    <span class="ma-patch-title">${escapeHtml(r.channel_name || 'Mutual Aid')}</span>
                    <span class="ma-req-dir">${inbound ? 'requested by ' + escapeHtml(who)
                                                       : 'sent to ' + escapeHtml(who)}</span>
                </div>
                <div class="ma-patch-actions">${actions}</div>
            </div>`;
        }).join('');
    }

    if (maPatches.length) {
        html += '<div class="ma-existing-label">Active Patches</div>';
        html += maPatches.map(l => {
            const names = l.agency_names || l.agencies || [];
            const chips = (l.agencies || []).map((aid, i) =>
                `<span class="ma-chip">${escapeHtml(names[i] || aid)}</span>`).join('');
            return `<div class="ma-patch-card ma-patch-existing">
                <div class="ma-patch-card-head">
                    <span class="ma-patch-title">${escapeHtml(l.channel_name || 'Mutual Aid')}</span>
                    <button class="ma-patch-remove" onclick="leavePatch(${jsArg(l.id)})"
                            title="Leave this patch">&times;</button>
                </div>
                <div class="ma-patch-chips">${chips}</div>
                <div class="ma-patch-meta">Channel ${l.channel}</div>
            </div>`;
        }).join('');
    }
    wrap.innerHTML = html;
    refreshRequestBuilder();
}

function requestBuilderHtml() {
    return `<div class="ma-patch-card ma-patch-builder" id="ma-req-builder">
        <input type="text" class="ma-patch-name" id="ma-req-name"
               placeholder="Channel name (e.g. County Ops)" />
        <div class="ma-dropzone" ondragover="maDragOver(event)" ondragleave="maDragLeave(event)"
             ondrop="maAgencyDrop(event)">
            <div class="ma-dropzone-hint" id="ma-req-hint">Drag one agency here</div>
            <div class="ma-dropzone-items" id="ma-req-items"></div>
        </div>
        <div class="ma-patch-actions">
            <button class="btn-primary" id="ma-req-btn" onclick="sendPatchRequest()" disabled>
                Request Patch</button>
            <button class="ma-patch-cancel" onclick="clearRequestBuilder()">Clear</button>
        </div>
    </div>`;
}

// Drop handler for the request builder. Only ONE agency: a request is between
// two agencies, unlike a super_admin patch which may span several.
function maAgencyDrop(e) {
    e.preventDefault();
    e.currentTarget.classList.remove('ma-dropzone-over');
    const id = e.dataTransfer.getData('text/plain');
    const name = e.dataTransfer.getData('agency-name') || id;
    if (!id) return;
    const entry = maDirectoryCache.find(a => a.id === id);
    if (entry && entry.state !== 'none') {
        showStatus('mutual-aid-status',
            'That agency is already patched or has a request pending', 'warning');
        return;
    }
    maDropped = { id, name };
    refreshRequestBuilder();
}

function refreshRequestBuilder() {
    const items = document.getElementById('ma-req-items');
    const hint = document.getElementById('ma-req-hint');
    const btn = document.getElementById('ma-req-btn');
    if (!items) return;
    if (maDropped) {
        const a = maDirectoryCache.find(x => x.id === maDropped.id);
        items.innerHTML = `<span class="ma-chip">${escapeHtml(maDropped.name)}<button
            class="ma-chip-x" onclick="clearRequestBuilder()">&times;</button></span>`;
        if (hint) hint.style.display = 'none';
        if (btn) {
            btn.disabled = false;
            // Say up front whether this will link now or wait for approval.
            btn.textContent = (a && a.auto_accepts_us) ? 'Request Patch (links now)' : 'Request Patch';
        }
    } else {
        items.innerHTML = '';
        if (hint) hint.style.display = '';
        if (btn) { btn.disabled = true; btn.textContent = 'Request Patch'; }
    }
}

function clearRequestBuilder() {
    maDropped = null;
    refreshRequestBuilder();
}

function sendPatchRequest() {
    if (!maDropped) return;
    const nameEl = document.getElementById('ma-req-name');
    const channelName = (nameEl ? nameEl.value : '').trim() || 'Mutual Aid';
    const target = maDropped;
    const btn = document.getElementById('ma-req-btn');
    if (btn) { btn.disabled = true; btn.textContent = 'Requesting…'; }

    fetch('/api/mutual-aid/requests', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ to_agency: target.id, channel_name: channelName })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) {
            showStatus('mutual-aid-status', data.error, 'error');
            refreshRequestBuilder();
            return;
        }
        showStatus('mutual-aid-status', data.auto_accepted
            ? `Patched with ${target.name} on channel ${data.channel} (standing agreement)`
            : `Request sent to ${target.name} — waiting for their approval`, 'success');
        maDropped = null;
        if (nameEl) nameEl.value = '';
        fetchAgencyMutualAid();
    })
    .catch(err => {
        showStatus('mutual-aid-status', 'Failed to request: ' + err.message, 'error');
        refreshRequestBuilder();
    });
}

// --- actions ---

function approveMaRequest(id) {
    fetch('/api/mutual-aid/requests/' + id + '/approve', { method: 'POST' })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('mutual-aid-status', data.error, 'error'); return; }
        showStatus('mutual-aid-status', `Patched on channel ${data.channel}`, 'success');
        fetchAgencyMutualAid();
    })
    .catch(err => showStatus('mutual-aid-status', 'Failed to approve: ' + err.message, 'error'));
}

function declineMaRequest(id) {
    fetch('/api/mutual-aid/requests/' + id + '/decline', { method: 'POST' })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('mutual-aid-status', data.error, 'error'); return; }
        showStatus('mutual-aid-status', 'Request closed', 'success');
        fetchAgencyMutualAid();
    })
    .catch(err => showStatus('mutual-aid-status', 'Failed: ' + err.message, 'error'));
}

function leavePatch(id) {
    if (!confirm('Leave this patch? The channel is removed from BOTH agencies.')) return;
    fetch('/api/mutual-aid/' + id + '/leave', { method: 'POST' })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('mutual-aid-status', data.error, 'error'); return; }
        showStatus('mutual-aid-status', 'Patch removed', 'success');
        fetchAgencyMutualAid();
    })
    .catch(err => showStatus('mutual-aid-status', 'Failed to leave: ' + err.message, 'error'));
}

// Standing agreements govern what WE accept, so trusting a partner does not let
// our own requests skip their approval — it lets theirs to us link instantly.
function toggleTrust(ev, agencyId, trust) {
    if (ev) ev.stopPropagation();   // the card is draggable; don't start a drag
    fetch('/api/my-agency/discovery')
    .then(r => r.json())
    .then(d => {
        const current = (d.auto_accept || []).map(x => x.id);
        const next = trust
            ? Array.from(new Set(current.concat([agencyId])))
            : current.filter(id => id !== agencyId);
        return fetch('/api/my-agency/discovery', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ auto_accept_from: next })
        }).then(r => r.json());
    })
    .then(data => {
        if (data.error) { showStatus('mutual-aid-status', data.error, 'error'); return; }
        showStatus('mutual-aid-status', trust
            ? 'Added to your standing agreements — their requests will link instantly'
            : 'Removed from your standing agreements', 'success');
        fetchAgencyMutualAid();
    })
    .catch(err => showStatus('mutual-aid-status', 'Failed: ' + err.message, 'error'));
}

// The dashboard socket carries a fingerprint of this agency's mutual-aid state.
// When it changes, something happened that the open page would otherwise only
// show after a manual refresh — an incoming request, an approval, a partner
// leaving. Comparing a revision keeps the endpoints idle while nothing happens.
let maLastRevision = null;

function maCheckRevision(data) {
    if (!data || data.mutual_aid_rev === undefined) return;
    const rev = data.mutual_aid_rev;
    if (maLastRevision === null) { maLastRevision = rev; return; }
    if (rev === maLastRevision) return;
    maLastRevision = rev;

    if (currentRole === 'super_admin') return;

    // Refresh whichever agency page is open. The fingerprint covers mutual aid
    // AND both bridge types, so a bridge appearing or being shared updates the
    // page the operator is actually looking at.
    const active = id => {
        const el = document.getElementById(id);
        return el && el.classList.contains('active');
    };
    if (active('page-mutual-aid')) fetchAgencyMutualAid();
    if (active('page-zello-bridges')) fetchAgencyZelloBridges();
    if (active('page-lmr-bridges')) fetchAgencyLmrBridges();
    maUpdateNavBadge();
}

// Pending inbound count on the nav item, so an admin on another page notices.
function maUpdateNavBadge() {
    if (currentRole === 'super_admin') return;
    fetch('/api/mutual-aid/requests')
        .then(r => r.json())
        .then(list => {
            const inbound = (Array.isArray(list) ? list : [])
                .filter(r => r.status === 'pending' && r.direction === 'inbound').length;
            const nav = document.querySelector('.nav-item[data-page="mutual-aid"]');
            if (!nav) return;
            let badge = nav.querySelector('.nav-badge');
            if (!inbound) { if (badge) badge.remove(); return; }
            if (!badge) {
                badge = document.createElement('span');
                badge.className = 'nav-badge';
                nav.appendChild(badge);
            }
            badge.textContent = inbound;
        })
        .catch(() => {});
}


// ===== Zello Bridges — agency-admin view =====
//
// A Zello bridge belongs to the agency that created it, backed by that agency's
// own Zello Work account. It is private: no other agency knows it exists until
// the owner grants access. Granting is a direct act by the owner — their
// bridge, their account — so there is nothing to approve.
//
// A recipient gets USE only: the channel appears for their clients, but the API
// never sends them the Zello network, username or mappings, and they cannot
// share, edit or delete. They may leave, which withdraws the channel.
//
// Layout mirrors Mutual Aid: agencies on the left, bridges on the right, drag
// to grant.

let zbDirectory = [];
let zbDiscovery = null;
let zbAgencyName = '';
let zbBridges = [];

function fetchAgencyZelloBridges() {
    const arr = v => Array.isArray(v) ? v : [];
    Promise.all([
        fetch('/api/my-agency/zello-bridges').then(r => r.json()).catch(() => []),
        fetch('/api/agencies/discoverable').then(r => r.json()).catch(() => []),
        // The session carries only the agency ID; this is where the NAME comes
        // from, for the locked Agency field in the create modal.
        fetch('/api/my-agency/discovery').then(r => r.json()).catch(() => null)
    ]).then(([bridges, directory, discovery]) => {
        zbDiscovery = discovery;
        zbBridges = arr(bridges);
        zbDirectory = arr(directory);
        if (discovery && discovery.agency_name) zbAgencyName = discovery.agency_name;
        const safe = (name, fn) => {
            try { fn(); } catch (e) { console.error('Zello: ' + name, e); }
        };
        safe('directory', zbFilterDirectory);
        safe('bridges', zbRenderBridges);
    }).catch(err => console.error('Failed to load Zello bridges:', err));
}

// --- left column: agencies you can grant access to ---

function zbFilterDirectory() {
    const box = document.getElementById('zb-dir-search');
    const q = (box ? box.value : '').trim().toLowerCase();
    const shown = q
        ? zbDirectory.filter(a => (a.name || '').toLowerCase().includes(q)
                               || (a.id || '').toLowerCase().includes(q))
        : zbDirectory;
    zbRenderDirectory(shown, q);
}

function zbRenderDirectory(agencies, query) {
    const pool = document.getElementById('zb-directory');
    const count = document.getElementById('zb-dir-count');
    const search = document.getElementById('zb-dir-search-row');
    if (!pool) return;
    if (count) count.textContent = zbDirectory.length;
    if (search) search.style.display = zbDirectory.length > 5 ? '' : 'none';

    if (zbDiscovery && !zbDiscovery.discoverable) {
        pool.innerHTML = `<div class="ma-empty">
            Turn on <strong>Discoverable</strong> on the Mutual Aid page to share bridges with other agencies.
            Discovery is mutual &mdash; agencies that hide themselves are not listed and cannot see you.
        </div>`;
        return;
    }
    if (!agencies.length) {
        pool.innerHTML = query
            ? `<div class="ma-empty">No agency matches &ldquo;${escapeHtml(query)}&rdquo;.</div>`
            : `<div class="ma-empty">No other agencies are discoverable.
                 Only discoverable agencies can be granted access.</div>`;
        return;
    }
    pool.innerHTML = agencies.map(a => `
        <div class="ma-agency-card" draggable="true" data-agency-id="${escapeAttr(a.id)}"
             ondragstart="maDragStart(event)" ondragend="maDragEnd(event)">
            <div class="ma-agency-name">${escapeHtml(a.name)}</div>
            <div class="ma-agency-id">${escapeHtml(a.id)}</div>
        </div>`).join('');
}

// --- right column: your bridges ---

function zbRenderBridges() {
    const wrap = document.getElementById('zb-agency-list');
    const count = document.getElementById('zb-bridge-count');
    const addBtn = document.getElementById('zb-add-btn');
    if (!wrap) return;
    if (count) count.textContent = zbBridges.length;
    if (addBtn) addBtn.style.display = '';

    if (!zbBridges.length) {
        wrap.innerHTML = `<div class="ma-empty">
            No Zello bridges yet. Add one to connect your agency's Zello Work
            account, then drag an agency here to share it.
        </div>`;
        return;
    }

    wrap.innerHTML = zbBridges.map(b => {
        const owner = b.access === 'owner';
        const dot = zelloStatusColor(b.status);
        const safeId = escapeAttr(b.id);

        if (!owner) {
            // Shared with us: channels and nothing else. No credentials, no
            // mapping editor, no delete — only the option to leave.
            const chans = (b.channels || []).map(c =>
                `<span class="ma-chip">${escapeHtml(c.name || 'channel')} &middot; ch ${c.channel}</span>`
            ).join('') || '<span class="zb-map-empty">no channels</span>';
            return `<div class="ma-patch-card ma-patch-existing">
                <div class="ma-patch-card-head">
                    <span class="ma-patch-title">
                        <span class="zb-dot" style="background:${dot}"></span>
                        ${escapeHtml(b.name)}
                    </span>
                    <button class="ma-patch-remove" onclick="zbLeave(${jsArg(b.id)})"
                            title="Leave this bridge">&times;</button>
                </div>
                <div class="ma-patch-chips">${chans}</div>
                <div class="ma-patch-meta">Shared by ${escapeHtml(b.owner_name || 'another agency')}
                    &middot; ${escapeHtml(b.status || 'unknown')}</div>
            </div>`;
        }

        // Owner: the bridge is a drop target for granting access.
        const shared = (b.shared_with || []).map(a =>
            `<span class="ma-chip">${escapeHtml(a.name)}<button class="ma-chip-x"
                onclick="zbUnshare(${jsArg(b.id)},${jsArg(a.id)})"
                title="Revoke access">&times;</button></span>`).join('');
        const maps = (b.mappings || []).map(m =>
            `<div class="zb-map">${escapeHtml(m.zello_channel || '(unmapped)')}
                <span class="zb-arrow">&rarr;</span> ch ${m.mesh_channel}</div>`).join('')
            || '<div class="zb-map-empty">No channels mapped yet</div>';

        return `<div class="ma-patch-card" id="zb-card-${safeId}">
            <div class="ma-patch-card-head">
                <span class="ma-patch-title">
                    <span class="zb-dot" style="background:${dot}"></span>
                    ${escapeHtml(b.name)}
                </span>
                <button class="ma-patch-remove" onclick="zbDelete(${jsArg(b.id)},${jsArg(b.name)})"
                        title="Delete this bridge">&times;</button>
            </div>
            <div class="ma-patch-meta">${escapeHtml(b.network || '')}
                &middot; ${escapeHtml(b.status || 'disconnected')}
                ${b.last_error ? '&middot; <span class="zb-error">' + escapeHtml(b.last_error) + '</span>' : ''}
            </div>
            <div class="zb-maps">${maps}</div>
            <div class="ma-dropzone zb-share-zone"
                 ondragover="maDragOver(event)" ondragleave="maDragLeave(event)"
                 ondrop="zbShareDrop(event,${jsArg(b.id)})">
                <div class="ma-patch-chips">${shared}</div>
                <div class="ma-dropzone-hint ma-edit-hint">Drag an agency here to share this bridge</div>
            </div>
        </div>`;
    }).join('');
}

// --- actions ---

function zbShareDrop(e, bridgeId) {
    e.preventDefault();
    e.currentTarget.classList.remove('ma-dropzone-over');
    const agencyId = e.dataTransfer.getData('text/plain');
    if (!agencyId) return;
    fetch('/api/my-agency/zello-bridges/' + bridgeId + '/share', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agency_id: agencyId })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('zello-bridges-status', data.error, 'error'); return; }
        showStatus('zello-bridges-status', 'Access granted — the channel now appears for them', 'success');
        fetchAgencyZelloBridges();
    })
    .catch(err => showStatus('zello-bridges-status', 'Failed to share: ' + err.message, 'error'));
}

function zbUnshare(bridgeId, agencyId) {
    fetch('/api/my-agency/zello-bridges/' + bridgeId + '/unshare', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agency_id: agencyId })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('zello-bridges-status', data.error, 'error'); return; }
        showStatus('zello-bridges-status', 'Access revoked — the channel was removed from their clients', 'success');
        fetchAgencyZelloBridges();
    })
    .catch(err => showStatus('zello-bridges-status', 'Failed: ' + err.message, 'error'));
}

function zbLeave(bridgeId) {
    if (!confirm('Leave this bridge? Its channel is removed from your clients.')) return;
    fetch('/api/my-agency/zello-bridges/' + bridgeId + '/unshare', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({})
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('zello-bridges-status', data.error, 'error'); return; }
        showStatus('zello-bridges-status', 'Left the bridge', 'success');
        fetchAgencyZelloBridges();
    })
    .catch(err => showStatus('zello-bridges-status', 'Failed to leave: ' + err.message, 'error'));
}

function zbDelete(bridgeId, name) {
    if (!confirm('Delete "' + name + '"? Its channels are removed from every agency using it.')) return;
    fetch('/api/my-agency/zello-bridges/' + bridgeId, { method: 'DELETE' })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('zello-bridges-status', data.error, 'error'); return; }
        showStatus('zello-bridges-status', 'Bridge deleted', 'success');
        fetchAgencyZelloBridges();
    })
    .catch(err => showStatus('zello-bridges-status', 'Failed to delete: ' + err.message, 'error'));
}

// "+ Add Bridge" uses the SAME modal for both roles — it is the only place that
// collects the API key, which the prompt-based flow this replaced did not. For
// an agency admin the Agency field is locked to their own agency: they cannot
// create a bridge owned by anyone else.
function zbAddBridge() {
    if (currentRole === 'super_admin') { showCreateZelloBridgeModal(); return; }

    document.getElementById('zelloBridgeModalTitle').textContent = 'Add Zello Connection';
    document.getElementById('zbEditId').value = '';
    ['zbName','zbNetwork','zbUsername','zbPassword','zbAuthToken']
        .forEach(id => document.getElementById(id).value = '');
    document.getElementById('zbZelloChannel').innerHTML = '';  // mapped on the card
    document.getElementById('zbMeshChannel').value = '0';
    document.getElementById('zbEnabled').checked = true;
    document.getElementById('zbEnabled').parentElement.style.display = '';
    document.getElementById('zelloBridgeError').style.display = 'none';

    // Lock the agency to the caller's own. Shown (so it is clear which agency
    // will own the bridge) but not changeable.
    const sel = document.getElementById('zbAgency');
    sel.innerHTML = `<option value="${escapeAttr(currentAgencyId || '')}">${
        escapeHtml(zbAgencyName || currentAgencyId || 'Your agency')}</option>`;
    sel.value = currentAgencyId || '';
    sel.disabled = true;

    showModal('zelloBridgeModal');
}


// ===== LMR Bridges — ATAK marker =====
//
// A bridge can be shown on the ATAK map: the server publishes a marker for the
// channel on the TAK server connection of each agency in the bridge. Same card
// section on the super admin and agency views; only the API path differs.

const lmrAtakDrafts = {};   // bridgeId -> {lat, lon} typed but not saved yet

const LMR_ATAK_NOTE = 'The marker is visible to everyone on the agency\'s TAK server. '
    + 'Listening still needs a MeshRelay login with access to this channel.';

// Why a marker is or is not going out on one agency's TAK link.
function lmrAtakTakText(tak) {
    switch (tak) {
        case 'connected': return { ok: true, text: 'sent to the TAK server' };
        case 'down': return { ok: false, text: 'marker not being sent: the TAK connection is down (sent again when it reconnects)' };
        case 'off': return { ok: false, text: 'marker not being sent: the TAK connection is switched off' };
        default: return { ok: false, text: 'marker not being sent: no TAK connection is set up' };
    }
}

function lmrAtakFmt(v) {
    return (typeof v === 'number' && isFinite(v)) ? String(v) : '';
}

// The ATAK section of an owner's bridge card. base is the API prefix the card
// belongs to ('/api/my-agency/lmr-bridges/' or '/api/lmr-bridges/');
// stateHtml is the line saying where the marker goes.
function lmrAtakSection(b, base, stateHtml) {
    const draft = lmrAtakDrafts[b.id];
    const lat = draft ? draft.lat : lmrAtakFmt(b.lat);
    const lon = draft ? draft.lon : lmrAtakFmt(b.lon);
    return `<div class="lmr-atak" data-bridge="${escapeAttr(b.id)}" data-base="${escapeAttr(base)}">
        <div class="lmr-atak-row">
            <span class="lmr-atak-label">Show in ATAK</span>
            <label class="toggle-switch lmr-atak-switch" title="Publish a map marker for this channel to the agency's TAK server">
                <input type="checkbox" class="lmr-atak-on" ${b.show_in_atak ? 'checked' : ''}
                       onchange="lmrAtakSave(this)">
                <span class="toggle-slider"></span>
            </label>
            <input type="text" inputmode="decimal" class="lmr-atak-coord lmr-atak-lat" placeholder="Latitude"
                   value="${escapeAttr(lat)}" oninput="lmrAtakDraft(this)" aria-label="Latitude">
            <input type="text" inputmode="decimal" class="lmr-atak-coord lmr-atak-lon" placeholder="Longitude"
                   value="${escapeAttr(lon)}" oninput="lmrAtakDraft(this)" aria-label="Longitude">
            <button class="btn-secondary lmr-atak-save" onclick="lmrAtakSave(this)">Save location</button>
        </div>
        ${stateHtml}
        <div class="lmr-atak-note">${escapeHtml(LMR_ATAK_NOTE)}</div>
    </div>`;
}

function lmrAtakStateLine(ok, text) {
    return `<div class="lmr-atak-state ${ok === null ? '' : (ok ? 'lmr-atak-ok' : 'lmr-atak-warn')}">${escapeHtml(text)}</div>`;
}

// Agency view: the caller's own TAK link (atak_tak).
function lmrAtakAgencyState(b) {
    if (!b.show_in_atak) return lmrAtakStateLine(null, 'Off: no marker is sent.');
    if (b.enabled === false) return lmrAtakStateLine(false, 'Marker not being sent: the bridge is paused.');
    const t = lmrAtakTakText(b.atak_tak);
    let text = t.ok ? 'Marker is being sent to your agency\'s TAK server.' : t.text.charAt(0).toUpperCase() + t.text.slice(1) + ' for your agency.';
    if ((b.shared_with || []).length) text += ' Agencies you share this bridge with get it on their own TAK connections.';
    return lmrAtakStateLine(t.ok, text);
}

// Super admin view: every agency in the bridge (atak_marker).
function lmrAtakSuperState(b) {
    if (!b.show_in_atak) return lmrAtakStateLine(null, 'Off: no marker is sent.');
    if (b.enabled === false) return lmrAtakStateLine(false, 'Marker not being sent: the bridge is disabled.');
    const rows = (b.atak_marker || []).map(a => {
        const t = lmrAtakTakText(a.tak);
        return `<div class="lmr-atak-state ${t.ok ? 'lmr-atak-ok' : 'lmr-atak-warn'}">${escapeHtml((a.name || a.agency_id) + ': ' + t.text)}</div>`;
    });
    return rows.join('');
}

function lmrAtakDraft(el) {
    const box = el.closest('.lmr-atak');
    lmrAtakDrafts[box.dataset.bridge] = {
        lat: box.querySelector('.lmr-atak-lat').value,
        lon: box.querySelector('.lmr-atak-lon').value
    };
}

function lmrAtakSave(el) {
    const box = el.closest('.lmr-atak');
    const id = box.dataset.bridge;
    const base = box.dataset.base;
    const sw = box.querySelector('.lmr-atak-on');
    const latS = box.querySelector('.lmr-atak-lat').value.trim();
    const lonS = box.querySelector('.lmr-atak-lon').value.trim();
    const refresh = () => (base.indexOf('/my-agency/') >= 0 ? fetchAgencyLmrBridges() : fetchLmrBridges());

    let lat = null, lon = null;
    if (latS !== '' || lonS !== '') {
        lat = Number(latS);
        lon = Number(lonS);
        if (latS === '' || lonS === '' || !isFinite(lat) || !isFinite(lon)) {
            showStatus('lmr-status', 'Enter both latitude and longitude as decimal degrees (for example 41.6512 and -74.6931)', 'warning');
            if (el === sw) sw.checked = !sw.checked;
            return;
        }
    }
    if (sw.checked && lat === null) {
        showStatus('lmr-status', 'Set the bridge\'s location before showing it in ATAK', 'warning');
        sw.checked = false;
        return;
    }
    fetch(base + encodeURIComponent(id) + '/atak', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ show_in_atak: sw.checked, lat: lat, lon: lon })
    })
    .then(r => r.json())
    .then(d => {
        if (d.error) {
            showStatus('lmr-status', d.error, 'error');
            refresh();
            return;
        }
        delete lmrAtakDrafts[id];
        showStatus('lmr-status', d.show_in_atak ? 'Shown in ATAK' : (lat === null ? 'ATAK marker off, location cleared' : 'ATAK marker off, location saved'), 'success');
        refresh();
    })
    .catch(e => { showStatus('lmr-status', 'Failed to save ATAK marker: ' + e.message, 'error'); refresh(); });
}


// ===== LMR Bridges — agency-admin view =====
//
// An LMR bridge belongs to the agency that created it: their gateway box, their
// radio, their gateway login. It is private until the owner grants another
// agency access, which makes the bridged channel appear for them too.
//
// A recipient gets USE only — never the gateway login, which is a credential.
// Same layout and grant model as the agency Zello view.

let lmrDirectory = [];
let lmrDiscovery = null;
let lmrMyBridges = [];

// Pause / resume a bridge: paused = the server sends the gateway nothing and
// the gateway inhibits its transmitter, so the radio cannot be keyed from here.
function lmrToggle(id, resume) {
    if (!resume && !confirm('Pause this bridge? The radio will stop transmitting immediately and the channel is withdrawn from phones until you resume.')) return;
    fetch('/api/my-agency/lmr-bridges/' + id + '/toggle', { method: 'POST' })
        .then(r => r.json())
        .then(d => {
            if (d.error) { showStatus('lmr-status', d.error, 'error'); return; }
            showStatus('lmr-status', d.enabled ? 'Bridge resumed' : 'Bridge paused — the radio will not transmit', d.enabled ? 'success' : 'error');
            fetchAgencyLmrBridges();
        })
        .catch(e => showStatus('lmr-status', 'Failed: ' + e.message, 'error'));
}

// Guests on a bridge: listen only (default) or allowed to key the radio.
function lmrGuests(id, allowed) {
    if (!allowed && !confirm('Let guests transmit on this bridge? Guest accounts (invited from a phone, or guest-event logins) will be able to key the radio.')) return;
    fetch('/api/my-agency/lmr-bridges/' + id + '/guests', { method: 'POST' })
        .then(r => r.json())
        .then(d => {
            if (d.error) { showStatus('lmr-status', d.error, 'error'); return; }
            showStatus('lmr-status', d.allow_guests ? 'Guests can now transmit on this bridge' : 'Guests are listen-only on this bridge', 'success');
            fetchAgencyLmrBridges();
        })
        .catch(e => showStatus('lmr-status', 'Failed: ' + e.message, 'error'));
}

function fetchAgencyLmrBridges() {
    const arr = v => Array.isArray(v) ? v : [];
    Promise.all([
        fetch('/api/my-agency/lmr-bridges').then(r => r.json()).catch(() => []),
        fetch('/api/agencies/discoverable').then(r => r.json()).catch(() => []),
        fetch('/api/my-agency/discovery').then(r => r.json()).catch(() => null)
    ]).then(([bridges, directory, discovery]) => {
        lmrMyBridges = arr(bridges);
        lmrDiscovery = discovery;
        lmrDirectory = arr(directory);
        const safe = (name, fn) => {
            try { fn(); } catch (e) { console.error('LMR: ' + name, e); }
        };
        safe('directory', lmrFilterDirectory);
        safe('bridges', lmrRenderAgencyBridges);
    }).catch(err => console.error('Failed to load LMR bridges:', err));
}

function lmrFilterDirectory() {
    const box = document.getElementById('lmr-dir-search');
    const q = (box ? box.value : '').trim().toLowerCase();
    const shown = q
        ? lmrDirectory.filter(a => (a.name || '').toLowerCase().includes(q)
                                || (a.id || '').toLowerCase().includes(q))
        : lmrDirectory;
    lmrRenderDirectory(shown, q);
}

function lmrRenderDirectory(agencies, query) {
    const pool = document.getElementById('lmr-directory');
    const count = document.getElementById('lmr-dir-count');
    const search = document.getElementById('lmr-dir-search-row');
    if (!pool) return;
    if (count) count.textContent = lmrDirectory.length;
    if (search) search.style.display = lmrDirectory.length > 5 ? '' : 'none';

    if (lmrDiscovery && !lmrDiscovery.discoverable) {
        pool.innerHTML = `<div class="ma-empty">
            Turn on <strong>Discoverable</strong> on the Mutual Aid page to share bridges with other agencies.
            Discovery is mutual &mdash; agencies that hide themselves are not listed and cannot see you.
        </div>`;
        return;
    }
    if (!agencies.length) {
        pool.innerHTML = query
            ? `<div class="ma-empty">No agency matches &ldquo;${escapeHtml(query)}&rdquo;.</div>`
            : `<div class="ma-empty">No other agencies are discoverable.
                 Only discoverable agencies can be granted access.</div>`;
        return;
    }
    pool.innerHTML = agencies.map(a => `
        <div class="ma-agency-card" draggable="true" data-agency-id="${escapeAttr(a.id)}"
             ondragstart="maDragStart(event)" ondragend="maDragEnd(event)">
            <div class="ma-agency-name">${escapeHtml(a.name)}</div>
            <div class="ma-agency-id">${escapeHtml(a.id)}</div>
        </div>`).join('');
}

function lmrRenderAgencyBridges() {
    const wrap = document.getElementById('lmr-agency-list');
    if (!wrap) return;
    // A push update can re-render mid-entry; keep an open builder and its
    // typed values rather than discarding them.
    const openBuilder = document.getElementById('lmr-agency-builder');
    const keep = openBuilder ? openBuilder.outerHTML : null;

    if (!lmrMyBridges.length) {
        wrap.innerHTML = (keep || '') + `<div class="ma-empty">
            No LMR bridges yet. Create one to connect a radio gateway, then drag
            an agency here to share its channel.
        </div>`;
        return;
    }

    wrap.innerHTML = lmrMyBridges.map(b => {
        const safeId = escapeAttr(b.id);
        if (b.access !== 'owner') {
            // Shared with us: the channel and who shared it. No gateway login.
            return `<div class="ma-patch-card ma-patch-existing">
                <div class="ma-patch-card-head">
                    <span class="ma-patch-title">${escapeHtml(b.channel_name || 'LMR Bridge')}</span>
                    <button class="ma-patch-remove" onclick="lmrLeave(${jsArg(b.id)})"
                            title="Leave this bridge">&times;</button>
                </div>
                <div class="ma-patch-meta">Channel ${b.channel}
                    &middot; shared by ${escapeHtml(b.owner_name || 'another agency')}</div>
                ${b.show_in_atak ? `<div class="lmr-atak lmr-atak-readonly">${lmrAtakAgencyState(b)}</div>` : ''}
            </div>`;
        }

        const dot = b.online ? 'lmr-dot-on' : 'lmr-dot-off';
        const shared = (b.shared_with || []).map(a =>
            `<span class="ma-chip">${escapeHtml(a.name)}<button class="ma-chip-x"
                onclick="lmrUnshare(${jsArg(b.id)},${jsArg(a.id)})"
                title="Revoke access">&times;</button></span>`).join('');
        const paused = b.enabled === false;
        return `<div class="ma-patch-card${paused ? ' lmr-paused' : ''}">
            <div class="ma-patch-card-head">
                <span class="ma-patch-title">
                    <span class="lmr-dot ${dot}" title="${b.online ? 'Gateway connected' : 'Gateway offline'}"></span>
                    ${escapeHtml(b.channel_name || b.name || 'LMR Bridge')}
                    ${paused ? '<span class="ma-chip" style="margin-left:8px;background:#7a1f1f;color:#fff">PAUSED — radio will not transmit</span>' : ''}
                </span>
                <button class="btn-action${paused ? '' : ' delete'}" style="margin-right:8px"
                        onclick="lmrToggle(${jsArg(b.id)}, ${paused ? 'true' : 'false'})"
                        title="${paused ? 'Resume: the gateway may transmit again' : 'Pause: stop all audio to the gateway so the radio cannot be keyed'}">
                    ${paused ? 'Resume' : 'Pause'}
                </button>
                <button class="btn-action" style="margin-right:8px"
                        onclick="lmrGuests(${jsArg(b.id)}, ${b.allow_guests ? 'true' : 'false'})"
                        title="Guests (invited from a phone, or guest-event logins) can always listen. This decides whether they may key the radio.">
                    Guests: ${b.allow_guests ? 'can transmit' : 'listen only'}
                </button>
                <button class="ma-patch-remove" onclick="lmrDelete(${jsArg(b.id)},${jsArg(b.channel_name || b.name)})"
                        title="Delete this bridge">&times;</button>
            </div>
            <div class="ma-patch-meta">
                Channel ${b.channel} &middot; gateway login <code>${escapeHtml(b.username || '')}</code>
                &middot; <a href="#" onclick="showLmrBridgeQr(${jsArg(b.username || '')},'');return false;">Enrollment QR</a>
            </div>
            <div class="ma-dropzone zb-share-zone"
                 ondragover="maDragOver(event)" ondragleave="maDragLeave(event)"
                 ondrop="lmrShareDrop(event,${jsArg(b.id)})">
                <div class="ma-patch-chips">${shared}</div>
                <div class="ma-dropzone-hint ma-edit-hint">Drag an agency here to share this bridge</div>
            </div>
            ${lmrAtakSection(b, '/api/my-agency/lmr-bridges/', lmrAtakAgencyState(b))}
        </div>`;
    }).join('');
    if (keep) wrap.insertAdjacentHTML('afterbegin', keep);
}

function lmrShareDrop(e, bridgeId) {
    e.preventDefault();
    e.currentTarget.classList.remove('ma-dropzone-over');
    const agencyId = e.dataTransfer.getData('text/plain');
    if (!agencyId) return;
    fetch('/api/my-agency/lmr-bridges/' + bridgeId + '/share', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agency_id: agencyId })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('lmr-status', data.error, 'error'); return; }
        showStatus('lmr-status', 'Access granted — the channel now appears for them', 'success');
        fetchAgencyLmrBridges();
    })
    .catch(err => showStatus('lmr-status', 'Failed to share: ' + err.message, 'error'));
}

function lmrUnshare(bridgeId, agencyId) {
    fetch('/api/my-agency/lmr-bridges/' + bridgeId + '/unshare', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agency_id: agencyId })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('lmr-status', data.error, 'error'); return; }
        showStatus('lmr-status', 'Access revoked — the channel was removed from their clients', 'success');
        fetchAgencyLmrBridges();
    })
    .catch(err => showStatus('lmr-status', 'Failed: ' + err.message, 'error'));
}

function lmrLeave(bridgeId) {
    if (!confirm('Leave this bridge? Its channel is removed from your clients.')) return;
    fetch('/api/my-agency/lmr-bridges/' + bridgeId + '/unshare', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({})
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('lmr-status', data.error, 'error'); return; }
        showStatus('lmr-status', 'Left the bridge', 'success');
        fetchAgencyLmrBridges();
    })
    .catch(err => showStatus('lmr-status', 'Failed to leave: ' + err.message, 'error'));
}

function lmrDelete(bridgeId, name) {
    if (!confirm('Delete "' + name + '"? The gateway login is removed and the channel '
               + 'disappears from every agency using it.')) return;
    fetch('/api/my-agency/lmr-bridges/' + bridgeId, { method: 'DELETE' })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('lmr-status', data.error, 'error'); return; }
        showStatus('lmr-status', 'Bridge removed', 'success');
        fetchAgencyLmrBridges();
    })
    .catch(err => showStatus('lmr-status', 'Failed to delete: ' + err.message, 'error'));
}

// Inline builder, mirroring the super_admin one: name, gateway credentials, and
// a dropzone. The difference is what a dropped agency MEANS — the bridge is
// owned by the caller's agency, so agencies dropped here are ones to SHARE it
// with, and dropping none is perfectly valid (a private bridge).
let lmrAgencyShare = new Set();

function lmrAddBridge() {
    const wrap = document.getElementById('lmr-agency-list');
    if (!wrap) return;
    if (document.getElementById('lmr-agency-builder')) return;   // one at a time
    lmrAgencyShare = new Set();
    wrap.insertAdjacentHTML('afterbegin', `
        <div class="ma-patch-card ma-patch-builder" id="lmr-agency-builder">
            <input type="text" class="ma-patch-name" id="lmr-new-name"
                   placeholder="Bridge name (e.g. County VHF)" />
            <div class="lmr-cred-row">
                <input type="text" id="lmr-new-user" placeholder="Gateway username" autocomplete="off" />
                <input type="password" id="lmr-new-pass" placeholder="Gateway password" autocomplete="new-password" />
            </div>
            <div class="ma-dropzone" ondragover="maDragOver(event)" ondragleave="maDragLeave(event)"
                 ondrop="lmrNewDrop(event)">
                <div class="ma-dropzone-hint" id="lmr-new-hint">
                    Optional: drag agencies here to share this bridge with them</div>
                <div class="ma-dropzone-items" id="lmr-new-items"></div>
            </div>
            <div class="ma-patch-actions">
                <button class="btn-primary" onclick="createMyLmrBridge()">Create Bridge</button>
                <button class="ma-patch-cancel" onclick="cancelLmrBuilder()">Clear</button>
            </div>
        </div>`);
    const nameEl = document.getElementById('lmr-new-name');
    if (nameEl) nameEl.focus();
}

function cancelLmrBuilder() {
    lmrAgencyShare = new Set();
    const el = document.getElementById('lmr-agency-builder');
    if (el) el.remove();
}

function lmrNewDrop(e) {
    e.preventDefault();
    e.currentTarget.classList.remove('ma-dropzone-over');
    const id = e.dataTransfer.getData('text/plain');
    const name = e.dataTransfer.getData('agency-name') || id;
    if (!id || lmrAgencyShare.has(id)) return;
    lmrAgencyShare.add(id);
    const items = document.getElementById('lmr-new-items');
    const hint = document.getElementById('lmr-new-hint');
    if (items) {
        items.insertAdjacentHTML('beforeend',
            `<span class="ma-chip" data-agency-id="${escapeAttr(id)}">${escapeHtml(name)}<button
                class="ma-chip-x" onclick="lmrNewUndrop(${jsArg(id)})">&times;</button></span>`);
    }
    if (hint) hint.style.display = 'none';
}

function lmrNewUndrop(agencyId) {
    lmrAgencyShare.delete(agencyId);
    const chip = document.querySelector(
        `#lmr-new-items .ma-chip[data-agency-id="${CSS.escape(agencyId)}"]`);
    if (chip) chip.remove();
    const hint = document.getElementById('lmr-new-hint');
    if (hint && !lmrAgencyShare.size) hint.style.display = '';
}

function createMyLmrBridge() {
    const name = (document.getElementById('lmr-new-name').value || '').trim();
    const username = (document.getElementById('lmr-new-user').value || '').trim();
    const password = document.getElementById('lmr-new-pass').value || '';
    if (!username || !password) {
        showStatus('lmr-status', 'Gateway username and password are required', 'warning');
        return;
    }
    const shareWith = Array.from(lmrAgencyShare);

    fetch('/api/my-agency/lmr-bridges', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            name: name || 'LMR Bridge',
            channel_name: name || 'LMR Bridge',
            username: username, password: password
        })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('lmr-status', data.error, 'error'); return null; }
        // Grant any agencies dropped into the builder, in sequence so one
        // failure does not silently skip the rest.
        return shareWith.reduce((chain, agencyId) => chain.then(() =>
            fetch('/api/my-agency/lmr-bridges/' + data.id + '/share', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ agency_id: agencyId })
            }).then(r => r.json()).then(res => {
                if (res.error) showStatus('lmr-status', 'Shared partly: ' + res.error, 'warning');
            })
        ), Promise.resolve()).then(() => data);
    })
    .then(data => {
        if (!data) return;
        cancelLmrBuilder();
        showStatus('lmr-status',
            `Bridge created on channel ${data.channel} — scan the Enrollment QR on the gateway`,
            'success');
        fetchAgencyLmrBridges();
    })
    .catch(err => showStatus('lmr-status', 'Failed to create: ' + err.message, 'error'));
}


// --- Channels (agency channel count + names, synced to phones and Command) ---
// Agency admins edit their own agency; a super admin sees every agency.

let agencyChannelsData = [];

function fetchAgencyChannels() {
    fetch('/api/my-agency/channels')
        .then(r => r.json())
        .then(data => {
            if (data.error) { showStatus('channels-status', data.error, 'error'); return; }
            agencyChannelsData = data || [];
            renderAgencyChannels();
        })
        .catch(err => showStatus('channels-status', 'Failed to load: ' + err.message, 'error'));
}

function renderAgencyChannels() {
    const box = document.getElementById('channels-agencies');
    if (!box) return;
    if (!agencyChannelsData.length) { box.innerHTML = '<div class="empty-state">No agencies</div>'; return; }
    box.innerHTML = agencyChannelsData.map((a, i) => {
        const rows = a.names.map((n, ch) => `
            <div class="chn-row">
                <span class="chn-num">${ch + 1}</span>
                <input type="text" maxlength="32" class="chn-input" data-agency="${i}" data-ch="${ch}"
                       value="${escapeHtml(n)}" placeholder="e.g. ${ch + 1}-TG${ch + 1}">
            </div>`).join('');
        return `
            <div class="panel chn-panel" style="margin-bottom: 20px;">
                <div class="chn-head">
                    <h3>${escapeHtml(a.agency_name)}${currentRole === 'super_admin' ? ` <span class="agency-badge">${escapeHtml(a.agency_id)}</span>` : ''}</h3>
                    <label class="chn-count">Channels
                        <input type="number" min="1" max="${a.limit}" value="${a.count}" id="chn-count-${i}"
                               onchange="resizeAgencyChannels(${i}, this.value)">
                        <span class="chn-limit">of ${a.limit}</span>
                    </label>
                </div>
                <div class="chn-grid">${rows}</div>
                <div class="chn-actions">
                    <button class="btn-primary" onclick="saveAgencyChannels(${i})">Save</button>
                </div>
            </div>`;
    }).join('');
}

// Change the number of rows before saving (names typed so far are kept).
function resizeAgencyChannels(i, value) {
    const a = agencyChannelsData[i];
    let n = parseInt(value, 10);
    if (!(n >= 1)) n = 1;
    if (n > a.limit) n = a.limit;
    a.names = collectAgencyChannelNames(i);
    while (a.names.length < n) a.names.push('');
    a.names.length = n;
    a.count = n;
    renderAgencyChannels();
}

function collectAgencyChannelNames(i) {
    const names = agencyChannelsData[i].names.slice();
    document.querySelectorAll(`.chn-input[data-agency="${i}"]`).forEach(el => {
        names[parseInt(el.dataset.ch, 10)] = el.value.trim();
    });
    return names;
}

function saveAgencyChannels(i) {
    const a = agencyChannelsData[i];
    const names = collectAgencyChannelNames(i);
    const body = { count: names.length, names };
    if (currentRole === 'super_admin') body.agency_id = a.agency_id;
    fetch('/api/my-agency/channels', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('channels-status', data.error, 'error'); return; }
        agencyChannelsData[i] = data;
        renderAgencyChannels();
        showStatus('channels-status', `${data.agency_name}: ${data.count} channels saved and sent to its phones and consoles`, 'success');
    })
    .catch(err => showStatus('channels-status', 'Failed to save: ' + err.message, 'error'));
}


// --- Emergency Alerts (the listen-only EMERGENCY channel) -------------------
// Name of the channel, the LMR bridge its audio is also sent out on, and the
// active emergencies (an admin can clear one from here).

let agencyEmergencyData = [];
let emergencyRefresh = null;

function fetchAgencyEmergency() {
    fetch('/api/my-agency/emergency')
        .then(r => r.json())
        .then(data => {
            if (data.error) { showStatus('emergency-status', data.error, 'error'); return; }
            agencyEmergencyData = data || [];
            renderAgencyEmergency();
        })
        .catch(err => showStatus('emergency-status', 'Failed to load: ' + err.message, 'error'));
    // Keep the active list current while the page is open.
    if (!emergencyRefresh) {
        emergencyRefresh = setInterval(() => {
            const page = document.getElementById('page-emergency');
            if (!page || !page.classList.contains('active')) { clearInterval(emergencyRefresh); emergencyRefresh = null; return; }
            if (document.activeElement && document.activeElement.closest && document.activeElement.closest('#emergency-agencies')) return;
            fetch('/api/my-agency/emergency').then(r => r.json()).then(d => {
                if (!d.error) { agencyEmergencyData = d || []; renderAgencyEmergency(); }
            }).catch(() => {});
        }, 10000);
    }
}

function renderAgencyEmergency() {
    const box = document.getElementById('emergency-agencies');
    if (!box) return;
    if (!agencyEmergencyData.length) { box.innerHTML = '<div class="empty-state">No agencies</div>'; return; }
    box.innerHTML = agencyEmergencyData.map((a, i) => {
        const opts = ['<option value="">None (phones and Command only)</option>'].concat(a.bridges.map(b =>
            `<option value="${escapeHtml(b.id)}"${b.id === a.lmr_bridge ? ' selected' : ''}>${escapeHtml(b.name)} (ch ${b.channel})${b.shared ? ' — shared with you' : ''}${b.enabled ? '' : ' — paused'}</option>`)).join('');
        const active = a.active.length ? a.active.map(e => `
                <div class="em-active-row">
                    <span class="em-dot"></span>
                    <span class="em-user">${escapeHtml(e.user)}</span>
                    <span class="em-meta">was on ${escapeHtml(e.channel_name)} · since ${new Date(e.since).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</span>
                    <button class="btn-secondary" onclick="clearAgencyEmergency(${i}, ${jsArg(e.user)})">Clear</button>
                </div>`).join('') : '<div class="list-hint">No active emergencies.</div>';
        return `
            <div class="panel chn-panel" style="margin-bottom: 20px;">
                <div class="chn-head">
                    <h3>${escapeHtml(a.agency_name)}${currentRole === 'super_admin' ? ` <span class="agency-badge">${escapeHtml(a.agency_id)}</span>` : ''}</h3>
                </div>
                <div class="settings-field" style="margin-bottom: 14px;">
                    <label class="modal-field-label">Channel name</label>
                    <input type="text" maxlength="32" class="chn-input em-field" id="em-name-${i}" value="${escapeHtml(a.channel_name)}" placeholder="EMERGENCY">
                    <div class="list-hint">Shown on every phone and on Command (channel ${a.channel}, listen-only).</div>
                </div>
                <div class="settings-field" style="margin-bottom: 14px;">
                    <label class="modal-field-label">Also send emergency audio out on an LMR radio channel</label>
                    <select class="chn-input em-field" id="em-bridge-${i}">${opts}</select>
                    <div class="list-hint">${a.bridges.length
                        ? 'The bridge\'s radio transmits the emergency audio. One way only: radio traffic is not put on the EMERGENCY channel.'
                        : 'Your agency has no LMR bridges. Add one on the LMR Bridges page to use this.'}</div>
                </div>
                <div class="chn-actions">
                    <button class="btn-primary" onclick="saveAgencyEmergency(${i})">Save</button>
                </div>
                <h4 style="margin: 18px 0 8px;">Active emergencies</h4>
                ${active}
            </div>`;
    }).join('');
}

function saveAgencyEmergency(i) {
    const a = agencyEmergencyData[i];
    const body = {
        channel_name: document.getElementById(`em-name-${i}`).value.trim(),
        lmr_bridge: document.getElementById(`em-bridge-${i}`).value
    };
    if (currentRole === 'super_admin') body.agency_id = a.agency_id;
    fetch('/api/my-agency/emergency', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('emergency-status', data.error, 'error'); return; }
        agencyEmergencyData[i] = data;
        renderAgencyEmergency();
        showStatus('emergency-status', `${data.agency_name}: emergency channel saved`, 'success');
    })
    .catch(err => showStatus('emergency-status', 'Failed to save: ' + err.message, 'error'));
}

function clearAgencyEmergency(i, user) {
    const a = agencyEmergencyData[i];
    if (!confirm(`Clear ${user}'s emergency? Their phone, dispatch and everyone in the agency will see it cleared.`)) return;
    const body = { user };
    if (currentRole === 'super_admin') body.agency_id = a.agency_id;
    fetch('/api/my-agency/emergency/clear', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('emergency-status', data.error, 'error'); return; }
        agencyEmergencyData[i] = data;
        renderAgencyEmergency();
        showStatus('emergency-status', `${user}'s emergency cleared`, 'success');
    })
    .catch(err => showStatus('emergency-status', 'Failed to clear: ' + err.message, 'error'));
}


// --- Maps (custom ZXY tile layers for the Command console) -----------------

function initMapsPage() {
    const sel = document.getElementById('map-agency');
    if (currentRole === 'super_admin' && sel) {
        fetch('/api/agencies').then(r => r.json()).then(agencies => {
            agenciesList = agencies || [];
            const current = sel.value;
            sel.innerHTML = '<option value="">Every agency</option>' + agenciesList
                .filter(a => !String(a.id).startsWith('event-'))
                .map(a => `<option value="${escapeHtml(a.id)}">${escapeHtml(a.name)}</option>`).join('');
            sel.value = current;
            fetchMapLayers();
        }).catch(() => fetchMapLayers());
    } else {
        fetchMapLayers();
    }
}

function fetchMapLayers() {
    fetch('/api/my-agency/maps')
        .then(r => r.json())
        .then(list => {
            if (list.error) { showStatus('maps-status', list.error, 'error'); return; }
            renderMapLayers(list || []);
        })
        .catch(err => showStatus('maps-status', 'Failed to load: ' + err.message, 'error'));
}

function mapAgencyLabel(id) {
    if (!id) return 'Every agency';
    const a = (agenciesList || []).find(x => x.id === id);
    return a ? a.name : id;
}

function renderMapLayers(list) {
    const body = document.getElementById('maps-table-body');
    document.getElementById('maps-empty').style.display = list.length ? 'none' : 'block';
    body.innerHTML = list.map(m => {
        const mine = currentRole === 'super_admin' || (m.agency && m.agency === currentAgencyId);
        return `<tr>
            <td><strong>${escapeHtml(m.name)}</strong></td>
            <td>${m.kind === 'overlay' ? 'Overlay' + (m.opacity < 1 ? ` (${Math.round(m.opacity * 100)}%)` : '') : 'Base map'}</td>
            <td class="super-admin-only">${escapeHtml(mapAgencyLabel(m.agency))}</td>
            <td class="map-url-cell" title="${escapeHtml(m.url)}">${escapeHtml(m.url)}</td>
            <td>${mine ? `<button class="btn-action delete" onclick="deleteMapLayer(${jsArg(m.id)}, ${jsArg(m.name)})">Delete</button>`
                       : '<span class="map-shared">Added by the server admin</span>'}</td>
        </tr>`;
    }).join('');
    if (currentRole !== 'super_admin') body.querySelectorAll('.super-admin-only').forEach(el => el.style.display = 'none');
}

function mapKindChanged() {
    const overlay = document.getElementById('map-kind').value === 'overlay';
    document.getElementById('map-opacity-wrap').style.display = overlay ? '' : 'none';
}

// Fill a ZXY template for one tile (z=4 over the eastern US).
function sampleTileUrl(tpl, subdomains) {
    const z = 4, x = 4, y = 6;
    return tpl.replace('{z}', z).replace('{x}', x).replace('{y}', y)
        .replace('{-y}', (1 << z) - 1 - y).replace('{s}', (subdomains || 'a')[0]).replace('{r}', '');
}

function previewMapTile() {
    const url = document.getElementById('map-url').value.trim();
    document.getElementById('map-subdomains-wrap').style.display = url.includes('{s}') ? '' : 'none';
    const img = document.getElementById('map-preview-img');
    const txt = document.getElementById('map-preview-text');
    if (!/^https:\/\//i.test(url) || !url.includes('{z}') || !url.includes('{x}') || !(url.includes('{y}') || url.includes('{-y}'))) {
        img.style.display = 'none';
        txt.style.display = '';
        txt.textContent = url ? 'Needs https:// and {z}, {x}, {y}' : 'Paste a tile URL to preview one tile';
        return;
    }
    img.referrerPolicy = 'strict-origin-when-cross-origin';
    img.onload = () => { img.style.display = ''; txt.style.display = 'none'; };
    img.onerror = () => { img.style.display = 'none'; txt.style.display = ''; txt.textContent = "Couldn't load a tile from this URL"; };
    img.src = sampleTileUrl(url, document.getElementById('map-subdomains').value.trim());
}

function addMapLayer() {
    const body = {
        name: document.getElementById('map-name').value.trim(),
        url: document.getElementById('map-url').value.trim(),
        kind: document.getElementById('map-kind').value,
        max_zoom: parseInt(document.getElementById('map-maxzoom').value, 10) || 19,
        opacity: document.getElementById('map-kind').value === 'overlay' ? parseFloat(document.getElementById('map-opacity').value) : 1,
        subdomains: document.getElementById('map-subdomains').value.trim(),
        attribution: document.getElementById('map-attribution').value.trim(),
    };
    if (currentRole === 'super_admin') body.agency_id = document.getElementById('map-agency').value;
    fetch('/api/my-agency/maps', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) { showStatus('maps-status', data.error, 'error'); return; }
        showStatus('maps-status', `"${data.name}" added. Consoles pick it up when they next load the page.`, 'success');
        ['map-name', 'map-url', 'map-attribution', 'map-subdomains'].forEach(id => document.getElementById(id).value = '');
        previewMapTile();
        fetchMapLayers();
    })
    .catch(err => showStatus('maps-status', 'Failed to add: ' + err.message, 'error'));
}

function deleteMapLayer(id, name) {
    if (!confirm(`Remove the map "${name}" from every console?`)) return;
    fetch('/api/my-agency/maps/' + encodeURIComponent(id), { method: 'DELETE' })
        .then(r => r.json())
        .then(data => {
            if (data.error) { showStatus('maps-status', data.error, 'error'); return; }
            showStatus('maps-status', `"${name}" removed`, 'success');
            fetchMapLayers();
        })
        .catch(err => showStatus('maps-status', 'Failed to remove: ' + err.message, 'error'));
}

// --- Move users to another server (Settings, super admin) -------------------
// Export: the server encrypts the file with the passphrase typed here; the
// passphrase is sent once over the dashboard connection and never stored.
// Import: the file and passphrase are sent for a preview (nothing changes),
// then again for the import itself, with the same options.

let migPreviewKey = '';   // options + file the shown preview belongs to

function initMigrationPanel() {
    const sel = document.getElementById('mig-export-agency');
    if (!sel) return;
    fetch('/api/agencies').then(r => r.json()).then(list => {
        const current = sel.value;
        sel.innerHTML = '<option value="">All agencies</option>' + (list || []).map(a =>
            `<option value="${escapeHtml(a.id)}">${escapeHtml(a.name)} (${escapeHtml(a.id)})</option>`).join('');
        if (current && (list || []).some(a => a.id === current)) sel.value = current;
    }).catch(() => {});
}

function migExport() {
    const pass = document.getElementById('mig-export-pass').value;
    const pass2 = document.getElementById('mig-export-pass2').value;
    const agency = document.getElementById('mig-export-agency').value;
    const withKey = document.getElementById('mig-export-identity').checked;
    if (pass.length < 12) { showStatus('migration-status', 'The passphrase must be at least 12 characters.', 'error'); return; }
    if (pass !== pass2) { showStatus('migration-status', 'The two passphrases are not the same.', 'error'); return; }
    if (withKey && !confirm('The file will contain this server\'s private identity key. Anyone with the file and the passphrase can pose as this server to the phones. Continue?')) return;
    const btn = document.getElementById('mig-export-btn');
    btn.disabled = true;
    showStatus('migration-status', 'Encrypting the export…', 'info');
    fetch('/api/user-migration/export', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agency: agency, include_identity_key: withKey, passphrase: pass })
    }).then(r => {
        if (!r.ok) return r.json().catch(() => ({})).then(d => { throw new Error(d.error || ('Server error ' + r.status)); });
        const cd = r.headers.get('Content-Disposition') || '';
        const m = /filename="([^"]+)"/.exec(cd);
        return r.blob().then(b => ({ blob: b, name: m ? m[1] : 'meshrelay-users.mrusers' }));
    }).then(({ blob, name }) => {
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = name;
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 10000);
        document.getElementById('mig-export-pass').value = '';
        document.getElementById('mig-export-pass2').value = '';
        showStatus('migration-status', 'Export downloaded: ' + name, 'success');
        addLog('Users exported (' + (agency || 'all agencies') + ')', 'success');
    }).catch(err => {
        showStatus('migration-status', 'Export failed: ' + err.message, 'error');
    }).finally(() => { btn.disabled = false; });
}

function migOptions() {
    return {
        mode: document.getElementById('mig-import-mode').value,
        create_agencies: document.getElementById('mig-create-agencies').checked ? '1' : '',
        include_guests: document.getElementById('mig-include-guests').checked ? '1' : '',
        import_web_admins: document.getElementById('mig-import-admins').checked ? '1' : '',
        use_identity_key: document.getElementById('mig-use-identity').checked ? '1' : ''
    };
}

function migKey() {
    const f = document.getElementById('mig-import-file').files[0];
    return JSON.stringify([f ? f.name + ':' + f.size + ':' + f.lastModified : '',
        document.getElementById('mig-import-pass').value.length, migOptions()]);
}

// Any change after a preview: the Import button waits for a new preview.
function migInputsChanged() {
    document.getElementById('mig-import-btn').disabled = !migPreviewKey || migPreviewKey !== migKey();
}

function migForm() {
    const f = document.getElementById('mig-import-file').files[0];
    const pass = document.getElementById('mig-import-pass').value;
    if (!f) throw new Error('Choose the export file.');
    if (!pass) throw new Error('Type the passphrase the file was exported with.');
    const form = new FormData();
    form.append('file', f);
    form.append('passphrase', pass);
    const o = migOptions();
    Object.keys(o).forEach(k => form.append(k, o[k]));
    return form;
}

function migPost(path) {
    let form;
    try { form = migForm(); } catch (e) { return Promise.reject(e); }
    return fetch(path, { method: 'POST', body: form }).then(r => r.json().catch(() => ({})).then(d => {
        if (!r.ok) throw new Error(d.error || ('Server error ' + r.status));
        return d;
    }));
}

const migActionText = {
    create: 'Created', exists: 'Already here, kept', new: 'Added', replace: 'Replaced', skip: 'Skipped'
};
const migKindText = { agency: 'Agency', user: 'Phone account', dashboard: 'Dashboard account' };

function migRenderPreview(d, applied) {
    const src = d.source || {};
    const plan = d.plan || {};
    const items = plan.items || [];
    const c = plan.counts || {};
    const n = k => c[k] || 0;
    const id = plan.identity || {};
    const idNote = document.getElementById('mig-identity-note');
    const idBox = document.getElementById('mig-use-identity');
    idBox.disabled = !id.in_file || id.same;
    if (idBox.disabled) idBox.checked = false;
    idNote.textContent = !id.in_file ? '(this file has none)'
        : id.same ? '(this server already has that identity)'
        : '(file key ' + String(id.fingerprint || '').slice(0, 16) + '…, this server ' + String(id.current || '').slice(0, 16) + '…)';
    const verb = applied ? 'Imported' : 'Would import';
    let summary = `File from ${src.server_version || 'unknown version'}, exported ${src.exported_at || ''} by ${src.exported_by || ''}` +
        ` (${src.scope ? 'agency ' + src.scope : 'all agencies'}). ${verb}: ` +
        `${n('agency_create')} new agencies, ${n('user_new')} new and ${n('user_replace')} replaced phone accounts, ` +
        `${n('dashboard_new')} new and ${n('dashboard_replace')} replaced dashboard accounts; ` +
        `${n('agency_skip') + n('user_skip') + n('dashboard_skip')} skipped.`;
    if (id.replace) summary += applied ? ' The server identity key was replaced.' : ' The server identity key will be replaced.';
    document.getElementById('mig-preview-summary').textContent = summary;
    document.getElementById('mig-preview-body').innerHTML = items.map(it => {
        const act = migActionText[it.action] || it.action;
        const why = it.reason ? ' — ' + it.reason : '';
        const name = it.kind === 'agency' ? it.name : (it.name + (it.display ? ' (' + it.display + ')' : '') +
            (it.guest ? ' · guest' : '') + (it.phones ? ' · ' + it.phones + ' phone key' + (it.phones === 1 ? '' : 's') : ''));
        return `<tr><td>${escapeHtml(migKindText[it.kind] || it.kind)}</td><td>${escapeHtml(it.agency)}</td>` +
            `<td>${escapeHtml(name)}</td><td>${escapeHtml(it.role || '')}</td>` +
            `<td class="mig-act-${escapeHtml(it.action)}">${escapeHtml(act + why)}</td></tr>`;
    }).join('');
    document.getElementById('mig-preview').style.display = '';
}

function migPreview() {
    const btn = document.getElementById('mig-preview-btn');
    btn.disabled = true;
    showStatus('migration-status', 'Checking the file…', 'info');
    const key = migKey();
    migPost('/api/user-migration/preview').then(d => {
        migRenderPreview(d, false);
        migPreviewKey = migKey() === key ? key : '';
        migInputsChanged();
        showStatus('migration-status', 'Preview ready. Nothing has been changed yet.', 'success');
    }).catch(err => {
        migPreviewKey = '';
        migInputsChanged();
        showStatus('migration-status', 'Preview failed: ' + err.message, 'error');
    }).finally(() => { btn.disabled = false; });
}

function migApply() {
    if (!migPreviewKey || migPreviewKey !== migKey()) { showStatus('migration-status', 'Preview the import first.', 'error'); return; }
    const o = migOptions();
    let msg = 'Import these accounts now? Copies of the current users, dashboard accounts and agencies are saved next to them first.';
    if (o.mode === 'replace') msg += '\n\nExisting accounts will be REPLACED by the file\'s; their phones are signed out if their password or phone key changed.';
    if (o.use_identity_key) msg += '\n\nThis server\'s identity key will be REPLACED by the one in the file. Phones that trust this server\'s current key will refuse it.';
    if (!confirm(msg)) return;
    const btn = document.getElementById('mig-import-btn');
    btn.disabled = true;
    showStatus('migration-status', 'Importing…', 'info');
    migPost('/api/user-migration/import').then(d => {
        migRenderPreview(d, true);
        migPreviewKey = '';
        document.getElementById('mig-import-pass').value = '';
        const text = `Import done: ${d.agencies_created} agencies, ${d.users} phone accounts, ${d.dashboard} dashboard accounts` +
            ` (${d.replaced} replaced, ${d.skipped} skipped, ${d.sessions_ended} phone sessions ended).`;
        showStatus('migration-status', text, 'success');
        addLog(text, 'success');
        fetchUsers();
    }).catch(err => {
        showStatus('migration-status', 'Import failed: ' + err.message, 'error');
    }).finally(() => { migInputsChanged(); });
}
