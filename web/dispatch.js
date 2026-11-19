// MeshRelay Command — dispatch console
//
// Layout: channels/units (left), map + radio log (center), transmit /
// emergencies / now talking (right). Server messages: see dispatch_audio.go
// (audio, floor) and dispatch_command.go (directory, locations, talk activity,
// emergencies) in the server source.

let ws = null;
let wsReconnectTimer = null;
let currentAgency = '';         // '' = super admin (every agency)
let currentUsername = '';
let volumeLevel = 1.0;
let vizCtx = null;
let vizAnimId = null;

let directory = { agencies: [], channels: [] };
let units = [];                 // field units (consoles filtered out)
let takUnits = [];              // TAK users from the agency's TAK Server (tak.go)
let txKey = loadPref('cmd.tx', '');            // "agency:channel" we transmit on
const monitored = new Set(loadPref('cmd.monitor', []));
// Agency groups start closed; this remembers the ones the dispatcher opened.
const expandedAgencies = new Set(loadPref('cmd.expanded', []));

const talkState = new Map();    // "agency|channel|user" -> { agency, channel, user, start, last, entry }
const radioLog = [];            // newest first
const emergencies = new Map();  // "agency|user" -> { agency, channel, user, at, active, acked, lat, lon }
const MAX_LOG = 300, KEEP_AUDIO = 40, TALK_GAP_MS = 1500;

// ─── Prefs / helpers ───────────────────────────────────────────────────────

// Warning triangle for emergencies (inline SVG, takes the text colour; no emoji).
const WARN_ICON = '<svg class="warn-icon" viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M1 21h22L12 2 1 21zm12-3h-2v-2h2v2zm0-4h-2v-4h2v4z"/></svg>';

function loadPref(key, fallback) {
    try { const v = localStorage.getItem(key); return v === null ? fallback : JSON.parse(v); } catch (_) { return fallback; }
}
function savePref(key, value) { try { localStorage.setItem(key, JSON.stringify(value)); } catch (_) {} }

const chKey = (agency, ch) => `${agency}:${ch}`;
const splitKey = (k) => { const i = k.lastIndexOf(':'); const tail = k.slice(i + 1); return [k.slice(0, i), tail === 'all' ? 'all' : parseInt(tail, 10)]; };
const isAllCall = (k) => !!k && k.endsWith(':all');
function txAgency() { return txKey ? splitKey(txKey)[0] : currentAgency; }

function agencyName(id) {
    const a = directory.agencies.find(x => x.id === id);
    return a ? a.name : id;
}
const AGENCY_COLORS = ['#3B82F6', '#22C55E', '#F59E0B', '#EC4899', '#14B8A6', '#58A6FF', '#F97316', '#84CC16', '#06B6D4', '#EAB308'];
function agencyColor(id) {
    const i = directory.agencies.findIndex(x => x.id === id);
    if (i >= 0) return AGENCY_COLORS[i % AGENCY_COLORS.length];
    let h = 0; for (const c of id || '') h = (h * 31 + c.charCodeAt(0)) | 0;
    return AGENCY_COLORS[Math.abs(h) % AGENCY_COLORS.length];
}
// The phone app's channel colours (res/values/colors.xml channel_1..10): a
// talkgroup gets colour (number % 10); bridge / mutual-aid rows use channel_1,
// as on the phone.
const APP_CHANNEL_COLORS = ['#4CAF50', '#2196F3', '#FFC107', '#FF5722', '#9C27B0',
                            '#00BCD4', '#E91E63', '#8BC34A', '#FF9800', '#3F51B5'];
function channelColor(c) {
    return c.kind === 'talkgroup' ? APP_CHANNEL_COLORS[c.channel % APP_CHANNEL_COLORS.length] : APP_CHANNEL_COLORS[0];
}

function channelInfo(agency, ch) {
    return directory.channels.find(c => c.agency === agency && c.channel === ch);
}
function channelLabel(agency, ch) {
    if (ch === 'all') return 'All Call';
    if (typeof ch === 'number' && ch < 0) return 'TAK';
    // Names come from the agency (dashboard → Channels), the same on every
    // phone and console; there is no per-console renaming.
    const c = channelInfo(agency, ch);
    if (c) return c.name;
    return ch < 50 ? `Channel ${ch + 1}` : `Channel ${ch}`;
}
function unitLabel(agency, user) {
    return (!currentAgency && directory.agencies.length > 1) ? `${user} (${agencyName(agency)})` : user;
}
function fmtTime(t) { return new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }); }
function fmtDur(ms) { const s = Math.max(1, Math.round(ms / 1000)); return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`; }
function ago(t) {
    const s = Math.round((Date.now() - t) / 1000);
    if (s < 60) return `${s}s ago`;
    if (s < 3600) return `${Math.round(s / 60)}m ago`;
    return `${Math.round(s / 3600)}h ago`;
}

// ─── Startup ───────────────────────────────────────────────────────────────

window.addEventListener('DOMContentLoaded', () => {
    vizCtx = document.getElementById('audio-visualizer').getContext('2d');
    clearViz();
    initMap();
    const p = loadPref('cmd.ui', {});
    document.getElementById('map-follow').checked = !!p.follow;
    document.getElementById('map-labels').checked = p.labels !== false;
    document.getElementById('channels-active-only').checked = loadPref('cmd.inUse', true);
    tickClock(); setInterval(tickClock, 1000);
    setInterval(sweepTalk, 300);
    setInterval(() => { renderUnits(); renderMarkers(); renderEmergencies(); }, 15000);
    checkSession();
    unlockAudioOnGesture();
});

function saveUiPrefs() {
    savePref('cmd.ui', {
        follow: document.getElementById('map-follow').checked,
        labels: document.getElementById('map-labels').checked,
    });
}

function tickClock() {
    const d = new Date();
    document.getElementById('clock-local').textContent = d.toLocaleTimeString([], { hour12: false });
    document.getElementById('clock-date').textContent = d.toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric' });
}

async function checkSession() {
    try {
        const r = await fetch('/api/session', { credentials: 'same-origin' });
        const data = await r.json();
        if (!r.ok || !['dispatch', 'agency_admin', 'super_admin'].includes(data.role)) {
            window.location.href = '/login.html?next=/command';
            return;
        }
        currentAgency = data.agency_id || '';
        currentUsername = data.username || '';
        document.getElementById('dispatch-agency-label').textContent = currentAgency ? '' : 'All agencies';
        document.getElementById('dispatch-user-label').textContent = currentUsername;
        if (currentAgency && txKey && splitKey(txKey)[0] !== currentAgency) txKey = '';
        connectDispatchWS();
        requestMic();
    } catch (e) {
        window.location.href = '/login.html?next=/command';
    }
}

// ─── WebSocket ─────────────────────────────────────────────────────────────

function connectDispatchWS() {
    if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return;
    if (ws) { try { ws.onopen = ws.onclose = ws.onerror = ws.onmessage = null; ws.close(); } catch (_) {} }

    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(`${proto}//${location.host}/ws/dispatch`);
    ws.binaryType = 'arraybuffer';
    setWsStatus('connecting');

    ws.onopen = () => {
        setWsStatus('connected');
        if (wsReconnectTimer) { clearTimeout(wsReconnectTimer); wsReconnectTimer = null; }
        sendTuning();
    };
    ws.onclose = () => {
        setWsStatus('error');
        stopPTT();
        if (wsReconnectTimer) clearTimeout(wsReconnectTimer);
        wsReconnectTimer = setTimeout(connectDispatchWS, 3000);
    };
    ws.onerror = () => setWsStatus('error');
    ws.onmessage = (e) => {
        if (e.data instanceof ArrayBuffer) {
            if (!e.data.byteLength) return;
            const t = new Uint8Array(e.data, 0, 1)[0];
            if (t === FRAME_AUDIO_OUT) onAudioFrame(e.data);
            else onControlPacket(e.data);
            return;
        }
        let msg;
        try { msg = JSON.parse(e.data); } catch (_) { return; }
        if (msg.type === 'update') onUpdate(msg.data || {});
        else if (msg.type === 'talk') noteTalk(msg.agency, msg.channel, msg.user, false);
        else if (msg.type === 'emergency') handleEmergency(msg);
        else if (msg.type === 'status') onStatus(msg);
        else if (msg.type === 'message') onMessage(msg, true);
        else if (msg.type === 'messages') { messages.length = 0; (msg.list || []).forEach(m => onMessage(m, false)); markVisibleMessagesRead(); renderMessages(); }
        else if (msg.type === 'message_read') onMessageRead(msg.id);
        else if (msg.type === 'recording') onRecording(msg);
        else if (msg.type === 'transcript') onTranscript(msg);
    };
}

function setWsStatus(state) {
    const el = document.getElementById('dispatch-ws-status');
    el.className = 'ws-status ws-' + state;
    el.textContent = state === 'connected' ? 'Connected' : state === 'connecting' ? 'Connecting…' : 'Disconnected';
}

function sendTuning() {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    if (txKey) {
        const [agency, channel] = splitKey(txKey);
        if (channel === 'all') ws.send(JSON.stringify({ type: 'channel', channel: 0, agency, allcall: true }));
        else ws.send(JSON.stringify({ type: 'channel', channel, agency }));
    }
    ws.send(JSON.stringify({ type: 'monitor', keys: [...monitored] }));
}

function onUpdate(data) {
    directory = { agencies: data.agencies || [], channels: data.channels || [] };
    units = (data.clients || []).filter(c => !c.console);
    takUnits = data.tak_units || [];
    if (!txKey && directory.channels.length) {
        const first = directory.channels.find(c => c.kind !== 'emergency' && (!currentAgency || c.agency === currentAgency))
            || directory.channels.find(c => c.kind !== 'emergency') || directory.channels[0];
        selectTx(first.agency, first.channel);
    }
    if (!currentAgency) document.getElementById('dispatch-agency-label').textContent = `All agencies (${directory.agencies.length})`;
    else document.getElementById('dispatch-agency-label').textContent = agencyName(currentAgency);
    renderChannels(); renderUnits(); renderMarkers(); renderTxPanel(); renderHeader(); renderMsgTargets();
}

// ─── Channels ──────────────────────────────────────────────────────────────

// The EMERGENCY channel (server emergency_channel.go): always heard, nobody
// transmits on it — only a unit with an active emergency is put on it.
const EMERGENCY_CHANNEL = 99;

function selectTx(agency, channel) {
    if (Number(channel) === EMERGENCY_CHANNEL) {
        flashMessage('The emergency channel is listen-only. It is always heard here.');
        return;
    }
    const key = chKey(agency, channel);
    if (currentAgency && agency !== currentAgency) return;
    if (key === txKey) return;
    stopPTT();
    txKey = key;
    savePref('cmd.tx', txKey);
    sendTuning();
    renderChannels(); renderTxPanel();
}

function toggleMonitor(agency, channel, ev) {
    if (ev) ev.stopPropagation();
    const key = chKey(agency, channel);
    if (monitored.has(key)) monitored.delete(key); else monitored.add(key);
    savePref('cmd.monitor', [...monitored]);
    sendTuning();
    renderChannels();
}

function toggleAgency(id) {
    if (expandedAgencies.has(id)) expandedAgencies.delete(id); else expandedAgencies.add(id);
    savePref('cmd.expanded', [...expandedAgencies]);
    renderChannels();
}

function talkersOn(agency, channel) {
    const out = [];
    for (const t of talkState.values()) if (t.agency === agency && t.channel === channel) out.push(t.user);
    return out;
}

function renderChannels() {
    const tree = document.getElementById('channel-tree');
    if (!tree) return;
    const q = (document.getElementById('channel-search').value || '').toLowerCase();
    const activeOnly = document.getElementById('channels-active-only').checked;
    let html = '';
    for (const a of directory.agencies) {
        const rows = directory.channels.filter(c => c.agency === a.id).filter(c => {
            const key = chKey(c.agency, c.channel);
            const label = channelLabel(c.agency, c.channel).toLowerCase();
            if (q && !label.includes(q) && !a.name.toLowerCase().includes(q)) return false;
            if (activeOnly && !(c.units > 0 || talkersOn(c.agency, c.channel).length || key === txKey || monitored.has(key))) return false;
            return true;
        });
        if (!rows.length && (q || !activeOnly)) continue;   // keep the agency (and All Call) when merely quiet
        const total = rows.reduce((n, c) => n + c.units, 0);
        const collapsed = !expandedAgencies.has(a.id) && !q;   // a search opens every group
        html += `<div class="agency-group">
            <div class="agency-head" onclick="toggleAgency('${escAttr(a.id)}')">
                <span>${collapsed ? '▸' : '▾'}</span><span class="agency-swatch" style="background:${agencyColor(a.id)}"></span>
                ${escHtml(a.name)}<span class="count">${total} on</span></div>`;
        if (!collapsed) {
            const allKey = chKey(a.id, 'all');
            html += `<div class="ch-row allcall${allKey === txKey ? ' tx' : ''}" onclick="selectTx('${escAttr(a.id)}', 'all')" title="Transmit on every talkgroup of ${escAttr(a.name)}">
                <div class="ch-name">📢 All Call${allKey === txKey ? '<span class="ch-tx-tag">TX</span>' : ''}</div><span></span><span></span>
                <div class="ch-sub">Every talkgroup at once</div></div>`;
            if (!rows.length) html += '<div class="list-hint" style="padding:6px 8px">No channels in use — untick "In use" to see all.</div>';
            for (const c of rows) {
                const key = chKey(c.agency, c.channel);
                const talking = talkersOn(c.agency, c.channel);
                if (c.kind === 'emergency') {
                    html += `<div class="ch-row emergency-ch${talking.length ? ' talking' : ''}" title="Listen-only. Always heard. A unit with an active emergency talks here.">
                        <div class="ch-name">${WARN_ICON}${escHtml(c.name)}<span class="ch-kind">listen only</span></div>
                        <span></span><span></span>
                        <div class="ch-sub">${talking.length ? `<span class="talker">● ${escHtml(talking.join(', '))}</span>` : 'Always heard'}</div>
                    </div>`;
                    continue;
                }
                const kind = c.kind !== 'talkgroup' ? `<span class="ch-kind">${escHtml(c.kind.replace('_', ' '))}</span>` : '';
                const sub = talking.length
                    ? `<span class="talker">● ${escHtml(talking.join(', '))}</span>`
                    : `${c.units} unit${c.units === 1 ? '' : 's'}${monitored.has(key) ? ' · monitoring' : ''}`;
                html += `<div class="ch-row${key === txKey ? ' tx' : ''}${talking.length ? ' talking' : ''}" style="--ch:${channelColor(c)}"
                        onclick="selectTx('${escAttr(c.agency)}', ${c.channel})" title="Click to transmit here">
                    <div class="ch-name">${escHtml(channelLabel(c.agency, c.channel))}${kind}${key === txKey ? '<span class="ch-tx-tag">TX</span>' : ''}</div>
                    <span class="ch-units">${c.units}</span>
                    <button class="ch-mon${monitored.has(key) ? ' on' : ''}" title="Monitor (listen)" onclick="toggleMonitor('${escAttr(c.agency)}', ${c.channel}, event)">🔈</button>
                    <div class="ch-sub">${sub}</div>
                </div>`;
            }
        }
        html += '</div>';
    }
    tree.innerHTML = html || '<div class="empty-small">No channels</div>';
}

function renderTxPanel() {
    const name = document.getElementById('tx-channel-name');
    const meta = document.getElementById('tx-channel-meta');
    if (!txKey) { name.textContent = '—'; meta.textContent = 'Pick a channel'; return; }
    const [agency, ch] = splitKey(txKey);
    if (ch === 'all') {
        const tgs = directory.channels.filter(c => c.agency === agency && c.kind === 'talkgroup');
        name.textContent = '📢 All Call';
        meta.textContent = `${agencyName(agency)} · ${tgs.length} talkgroups · ${tgs.reduce((n, c) => n + c.units, 0)} units`;
        return;
    }
    const info = channelInfo(agency, ch);
    name.textContent = channelLabel(agency, ch);
    meta.textContent = `${agencyName(agency)} · ${info ? info.units : 0} unit${info && info.units === 1 ? '' : 's'}`;
}

// ─── Units ─────────────────────────────────────────────────────────────────

const unitKey = (u) => `${u.agency_id}|${u.username}`;
function isTalking(agency, user) {
    for (const t of talkState.values()) if (t.agency === agency && t.user === user) return true;
    return false;
}
function locAge(u) { return u.location ? Date.now() - Date.parse(u.location.at) : Infinity; }

function renderUnits() {
    const list = document.getElementById('units-list');
    if (!list) return;
    document.getElementById('units-tab-count').textContent = units.length;
    const q = (document.getElementById('unit-search').value || '').toLowerCase();
    const sort = document.getElementById('unit-sort').value;
    let rows = units.filter(u => !q || u.username.toLowerCase().includes(q) || (u.account || '').toLowerCase().includes(q)
        || channelLabel(u.agency_id, u.channel).toLowerCase().includes(q));
    rows.sort((a, b) => sort === 'status' ? ((a.status ? a.status.code : 99) - (b.status ? b.status.code : 99)) || a.username.localeCompare(b.username)
        : sort === 'channel' ? (a.agency_id + a.channel).localeCompare(b.agency_id + b.channel, undefined, { numeric: true })
        : sort === 'recent' ? locAge(a) - locAge(b)
        : a.username.localeCompare(b.username, undefined, { sensitivity: 'base' }));
    const tak = takUnits.filter(t => !q || t.callsign.toLowerCase().includes(q) || (t.group || '').toLowerCase().includes(q))
        .sort((a, b) => a.callsign.localeCompare(b.callsign, undefined, { sensitivity: 'base' }));
    const takHtml = tak.length ? `<div class="units-section">TAK users (${tak.length})</div>` + tak.map(t => `
        <div class="unit-row${t.stale ? ' stale-row' : ''}" onclick="focusUnit('tak|${escAttr(t.uid)}')">
            <div class="unit-avatar tak-avatar" style="background:${takTeamColor(t.group)}">${escHtml(t.callsign.charAt(0).toUpperCase())}</div>
            <div style="min-width:0"><div class="unit-name">${escHtml(t.callsign)} <span class="tak-tag">TAK</span></div>
                <div class="unit-meta">${escHtml([t.group, t.role].filter(Boolean).join(' · ') || 'TAK user')}${!currentAgency ? ' · ' + escHtml(agencyName(t.agency)) : ''}</div></div>
            <div class="unit-right">${t.battery ? `<span class="${t.battery <= 20 ? 'batt-low' : ''}">${t.battery}%</span> · ` : ''}${ago(Date.parse(t.at))}</div>
        </div>`).join('') : '';
    document.getElementById('units-tab-count').textContent = units.length + takUnits.length;
    if (!rows.length && !tak.length) { list.innerHTML = '<div class="empty-small">No units online</div>'; return; }
    list.innerHTML = (rows.length && tak.length ? '<div class="units-section">MeshRelay</div>' : '') + rows.map(u => {
        const loc = u.location;
        const batt = loc && loc.battery !== undefined ? `<span class="${loc.battery <= 20 ? 'batt-low' : ''}">${loc.battery}%</span> · ` : '';
        const right = loc ? `${batt}${ago(Date.parse(loc.at))}` : '<span class="nofix">no position</span>';
        const signedIn = u.account && u.account !== u.username ? ` · ${escHtml(u.account)}` : '';
        const st = u.status ? `<span class="status-pill"><i style="background:${statusColor(u.status.code)}"></i>${escHtml(u.status.name)}</span> ` : '';
        return `<div class="unit-row${isTalking(u.agency_id, u.username) ? ' talking' : ''}" onclick="focusUnit('${escAttr(unitKey(u))}')">
            <div class="unit-avatar" style="background:${agencyColor(u.agency_id)}">${escHtml(u.username.charAt(0).toUpperCase())}</div>
            <div style="min-width:0"><div class="unit-name">${escHtml(u.username)} ${st}</div>
                <div class="unit-meta">${escHtml(channelLabel(u.agency_id, u.channel))}${!currentAgency ? ' · ' + escHtml(agencyName(u.agency_id)) : ''}${signedIn}</div></div>
            <div class="unit-right">${right}</div>
        </div>`;
    }).join('') + takHtml;
}

function showLeftTab(name) {
    document.querySelectorAll('.tab').forEach(t => t.classList.toggle('active', t.dataset.tab === name));
    document.getElementById('tab-channels').style.display = name === 'channels' ? 'flex' : 'none';
    document.getElementById('tab-units').style.display = name === 'units' ? 'flex' : 'none';
}

// ─── Map ───────────────────────────────────────────────────────────────────

let map = null;
const markers = new Map();      // unitKey -> { marker, cls, label }
let fittedOnce = false;

function initMap() {
    if (typeof L === 'undefined') return;
    const view = loadPref('cmd.view', null);
    map = L.map('map', { zoomControl: true, attributionControl: true })
        .setView(view ? [view.lat, view.lon] : [39.5, -98.35], view ? view.zoom : 4);
    const layers = {
        // Esri's dark canvas + its label layer: no API key needed.
        'Dark': L.layerGroup([
            L.tileLayer('https://server.arcgisonline.com/ArcGIS/rest/services/Canvas/World_Dark_Gray_Base/MapServer/tile/{z}/{y}/{x}',
                { maxZoom: 16, attribution: 'Tiles &copy; Esri, HERE, Garmin, &copy; OpenStreetMap contributors' }),
            L.tileLayer('https://server.arcgisonline.com/ArcGIS/rest/services/Canvas/World_Dark_Gray_Reference/MapServer/tile/{z}/{y}/{x}',
                { maxZoom: 16 }),
        ]),
        // OpenStreetMap and OpenTopoMap refuse tiles requested without a
        // Referer ("Access blocked"), and the page's policy (same-origin) sends
        // none to other sites. These layers send just the site's address.
        'Streets': L.tileLayer('https://tile.openstreetmap.org/{z}/{x}/{y}.png',
            { maxZoom: 19, attribution: '&copy; OpenStreetMap contributors', referrerPolicy: 'strict-origin-when-cross-origin' }),
        'Satellite': L.tileLayer('https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}',
            { maxZoom: 19, attribution: 'Imagery &copy; Esri' }),
        'Topo': L.tileLayer('https://{s}.tile.opentopomap.org/{z}/{x}/{y}.png',
            { maxZoom: 17, attribution: '&copy; OpenStreetMap contributors, SRTM | &copy; OpenTopoMap', referrerPolicy: 'strict-origin-when-cross-origin' }),
    };
    const base = loadPref('cmd.layer', 'Dark');
    (layers[base] || layers.Dark).addTo(map);
    const layerControl = L.control.layers(layers, null, { position: 'topleft' }).addTo(map);
    map.on('baselayerchange', e => savePref('cmd.layer', e.name));
    loadCustomMapLayers(layerControl, layers, base);
    map.on('moveend', () => { const c = map.getCenter(); savePref('cmd.view', { lat: c.lat, lon: c.lng, zoom: map.getZoom() }); });
    // The map's box changes with the layout (panels appearing, the emergency
    // panel, window resizes Leaflet doesn't see). Without telling Leaflet, it
    // centres on a stale size and a "located" unit lands off-centre.
    if (typeof ResizeObserver !== 'undefined') {
        new ResizeObserver(() => map.invalidateSize({ pan: false })).observe(document.getElementById('map'));
    }
    if (view) fittedOnce = true;
}

function markerClass(u) {
    let cls = 'unit-marker';
    if (locAge(u) > 5 * 60000) cls += ' stale';
    if (isTalking(u.agency_id, u.username)) cls += ' talking';
    const em = emergencies.get(`${u.agency_id}|${u.username}`);
    if (em && em.active) cls += ' emergency';
    return cls;
}

function renderMarkers(force) {
    if (!map) return;
    const showLabels = document.getElementById('map-labels').checked;
    const seen = new Set();
    let located = 0;
    for (const u of units) {
        if (!u.location) continue;
        located++;
        const key = unitKey(u);
        seen.add(key);
        const pos = [u.location.lat, u.location.lon];
        const cls = markerClass(u);
        const label = showLabels ? u.username : '';
        let m = markers.get(key);
        if (!m) {
            const marker = L.marker(pos, { icon: unitIcon(u, cls, label), keyboard: false });
            marker.bindPopup(() => unitPopup(key), { maxWidth: 260 });
            marker.addTo(map);
            m = { marker, cls, label };
            markers.set(key, m);
        } else {
            m.marker.setLatLng(pos);
            if (force || m.cls !== cls || m.label !== label) {
                m.marker.setIcon(unitIcon(u, cls, label));
                m.cls = cls; m.label = label;
            }
        }
        m.marker.setZIndexOffset(cls.includes('emergency') ? 2000 : cls.includes('talking') ? 1000 : 0);
    }
    for (const t of takUnits) {
        if (t.lat === undefined) continue;
        located++;
        const key = 'tak|' + t.uid;
        seen.add(key);
        let cls = 'unit-marker tak-marker' + (t.stale ? ' stale' : '');
        const em = emergencies.get(`${t.agency}|${t.callsign} (TAK)`);
        if (em && em.active) cls += ' emergency';
        const label = showLabels ? t.callsign : '';
        let m = markers.get(key);
        const icon = () => L.divIcon({ className: '', iconSize: [0, 0],
            html: `<div class="${cls}"><div class="unit-dot tak-dot" style="background:${takTeamColor(t.group)}"></div>${label ? `<div class="unit-label">${escHtml(label)} <span class="tak-tag">TAK</span></div>` : ''}</div>` });
        if (!m) {
            const marker = L.marker([t.lat, t.lon], { icon: icon(), keyboard: false });
            marker.bindPopup(() => takPopup(t.uid), { maxWidth: 260 });
            marker.addTo(map);
            m = { marker, cls, label };
            markers.set(key, m);
        } else {
            m.marker.setLatLng([t.lat, t.lon]);
            if (force || m.cls !== cls || m.label !== label) { m.marker.setIcon(icon()); m.cls = cls; m.label = label; }
        }
    }
    for (const [key, m] of markers) {
        if (!seen.has(key)) { m.marker.remove(); markers.delete(key); }
    }
    document.getElementById('map-empty').style.display = located ? 'none' : 'block';
    if (!fittedOnce && located) { fittedOnce = true; fitAllUnits(); }
}

function unitIcon(u, cls, label) {
    return L.divIcon({
        className: '', iconSize: [0, 0],
        html: `<div class="${cls}"><div class="unit-dot" style="background:${agencyColor(u.agency_id)}"></div>${label ? `<div class="unit-label">${escHtml(label)}</div>` : ''}</div>`,
    });
}

function unitPopup(key) {
    const u = units.find(x => unitKey(x) === key);
    if (!u) return 'Offline';
    const l = u.location || {};
    const rows = [
        `${escHtml(channelLabel(u.agency_id, u.channel))} · ${escHtml(agencyName(u.agency_id))}`,
        u.account && u.account !== u.username ? `Signed in as ${escHtml(u.account)}` : '',
        l.at ? `Position ${ago(Date.parse(l.at))}${l.acc ? ` · ±${Math.round(l.acc)} m` : ''}` : '',
        l.speed ? `Moving ${Math.round(l.speed * 2.237)} mph, heading ${Math.round(l.heading || 0)}°` : '',
        l.battery !== undefined ? `Battery ${l.battery}%` : '',
        l.lat ? `${l.lat.toFixed(5)}, ${l.lon.toFixed(5)}` : '',
    ].filter(Boolean).map(r => `<div class="popup-row">${r}</div>`).join('');
    const a = escAttr(u.agency_id), ch = u.channel, un = escAttr(u.username);
    const code = u.status ? u.status.code : 0;
    const opts = STATUS_NAMES.map((n, i) => `<option value="${i}"${i === code ? ' selected' : ''}>${n}</option>`).join('');
    return `<div class="popup-title">${escHtml(u.username)}</div>${rows}
        <div class="popup-status">Status <select onchange="setUnitStatus('${a}', '${un}', this.value)">${opts}</select></div>
        <div class="popup-actions">
            <button onclick="selectTx('${a}', ${ch})">Talk on channel</button>
            <button onclick="toggleMonitor('${a}', ${ch})">${monitored.has(chKey(u.agency_id, ch)) ? 'Stop monitoring' : 'Monitor'}</button>
            <button onclick="messageUnit('${a}', '${un}')">Message</button>
        </div>`;
}

// Maps the agency added on the dashboard (Maps page): ZXY tile URLs, as base
// maps or overlays. The last base map and the overlays that were on are
// remembered per browser.
function loadCustomMapLayers(control, builtIn, savedBase) {
    fetch('/api/dispatch/maps').then(r => r.ok ? r.json() : []).then(list => {
        if (!Array.isArray(list) || !list.length) return;
        const onOverlays = new Set(loadPref('cmd.overlays', []));
        const multiAgency = !currentAgency && directory.agencies.length > 1;
        for (const m of list) {
            let label = m.name;
            if (multiAgency && m.agency) label += ` (${agencyName(m.agency)})`;
            if (builtIn[label]) label += ' (custom)';
            const opts = {
                maxZoom: m.max_zoom || 19,
                opacity: m.opacity || 1,
                attribution: m.attribution ? escHtml(m.attribution) : '',
                referrerPolicy: 'strict-origin-when-cross-origin',
            };
            if (m.subdomains) opts.subdomains = m.subdomains;
            const layer = L.tileLayer(m.url, opts);
            const name = escHtml(label);   // the layer control shows names as HTML
            if (m.kind === 'overlay') {
                layer._cmdOverlay = name;
                control.addOverlay(layer, name);
                if (onOverlays.has(name)) layer.addTo(map);
            } else {
                control.addBaseLayer(layer, name);
                if (name === savedBase && !builtIn[savedBase]) {
                    Object.values(builtIn).forEach(l => map.removeLayer(l));
                    layer.addTo(map);
                }
            }
        }
        const saveOverlays = () => {
            const on = [];
            map.eachLayer(l => { if (l._cmdOverlay) on.push(l._cmdOverlay); });
            savePref('cmd.overlays', on);
        };
        map.on('overlayadd overlayremove', saveOverlays);
    }).catch(() => {});
}

function fitAllUnits() {
    if (!map || !markers.size) return;
    const pts = [...markers.values()].map(m => m.marker.getLatLng());
    if (pts.length === 1) map.setView(pts[0], 15);
    else map.fitBounds(L.latLngBounds(pts), { padding: [40, 40], maxZoom: 16 });
}

// Put latlng in the middle of the map (and optionally open marker's popup
// without letting it pan the map off the unit).
function centerOn(latlng, zoom, marker) {
    if (!map) return;
    map.invalidateSize({ pan: false });
    map.setView(latlng, zoom, { animate: false });
    if (marker && marker.getPopup()) {
        const p = marker.getPopup();
        const auto = p.options.autoPan;
        p.options.autoPan = false;
        marker.openPopup();
        p.options.autoPan = auto;
    }
}

function focusUnit(key) {
    if (key.endsWith(' (TAK)')) {   // an emergency/message name: find the TAK user's marker
        const [agency, name] = [key.slice(0, key.indexOf('|')), key.slice(key.indexOf('|') + 1, -6)];
        const t = takUnits.find(x => x.agency === agency && x.callsign === name);
        if (t) key = 'tak|' + t.uid;
    }
    const m = markers.get(key);
    if (m && map) {
        centerOn(m.marker.getLatLng(), Math.max(map.getZoom(), 15), m.marker);
    } else {
        const u = units.find(x => unitKey(x) === key);
        if (u) flashMessage(`${u.username} has not shared a position.`);
    }
}

function flashMessage(text) {
    const el = document.getElementById('map-empty');
    el.textContent = text;
    el.style.display = 'block';
    setTimeout(() => { el.textContent = 'No unit positions yet. Units appear when their app shares its location with dispatch (Settings → Share location with dispatch).'; renderMarkers(); }, 3000);
}

// ─── Talk activity + radio log ─────────────────────────────────────────────

let renderQueued = false;
function queueRender() {
    if (renderQueued) return;
    renderQueued = true;
    setTimeout(() => {
        renderQueued = false;
        renderLog(); renderTalking(); renderChannels(); renderMarkers(); renderHeader();
    }, 200);
}

// Someone is talking (a "talk" event from the server, or an audio frame we
// hear). A gap of more than TALK_GAP_MS starts a new log entry.
function noteTalk(agency, channel, user, heard) {
    // Our own all-call comes back as talk on every talkgroup: one entry.
    if (user === currentUsername && isAllCall(txKey) && splitKey(txKey)[0] === agency && channel !== 'all') {
        const c = channelInfo(agency, channel);
        if (!c || c.kind === 'talkgroup') channel = 'all';
    }
    const key = `${agency}|${channel}|${user}`;
    const now = Date.now();
    let t = talkState.get(key);
    if (!t) {
        const entry = { agency, channel, user, start: now, end: now, heard: false, pcm: [], pcmLen: 0 };
        radioLog.unshift(entry);
        if (radioLog.length > MAX_LOG) radioLog.length = MAX_LOG;
        radioLog.forEach((e, i) => { if (i >= KEEP_AUDIO && e.pcm.length) { e.pcm = []; e.pcmLen = 0; } });
        t = { agency, channel, user, start: now, last: now, entry };
        talkState.set(key, t);
        if (document.getElementById('map-follow').checked) {
            const m = markers.get(`${agency}|${user}`);
            if (m && map) map.panTo(m.marker.getLatLng());
        }
    }
    t.last = now;
    t.entry.end = now;
    if (heard) t.entry.heard = true;
    queueRender();
}

function recordAudio(agency, channel, user, samples) {
    const t = talkState.get(`${agency}|${channel}|${user}`);
    if (!t || t.entry.pcmLen > 120 * audioCtx.sampleRate) return;   // cap 2 min per entry
    t.entry.pcm.push(samples.slice());
    t.entry.pcmLen += samples.length;
}

function sweepTalk() {
    const now = Date.now();
    let changed = false;
    for (const [k, t] of talkState) {
        if (now - t.last > TALK_GAP_MS) { talkState.delete(k); changed = true; }
    }
    if (changed) queueRender();
    else if (talkState.size) renderTalking();
}

function renderLog() {
    const body = document.getElementById('log-body');
    const filter = document.getElementById('log-filter').value;
    const rows = radioLog.filter(e => filter !== 'heard' || e.heard).slice(0, 150);
    document.getElementById('log-empty').style.display = rows.length ? 'none' : 'block';
    body.innerHTML = rows.map((e, i) => {
        const live = [...talkState.values()].some(t => t.entry === e);
        const idx = radioLog.indexOf(e);
        const replay = (e.pcmLen || e.recId) ? `<button class="replay-btn" onclick="replay(${idx})">▶ Replay</button>` : '';
        const ch = `${escHtml(channelLabel(e.agency, e.channel))}${!currentAgency && directory.agencies.length > 1 ? ' · ' + escHtml(agencyName(e.agency)) : ''}`;
        const what = e.note ? escHtml(e.note) : e.emergency ? (e.active ? WARN_ICON + 'EMERGENCY' : 'Emergency cleared') : (live ? 'talking…' : fmtDur(e.end - e.start));
        return `<tr class="${live ? 'live' : ''}${e.emergency ? ' emerg' : ''}">
            <td>${fmtTime(e.start)}</td><td class="log-ch" title="${ch}">${ch}</td>
            <td><span class="log-unit" onclick="focusUnit('${escAttr(e.agency + '|' + e.user)}')">${escHtml(e.user)}</span></td>
            <td>${what}</td><td>${replay}</td></tr>${transcriptRow(e.text, e.transcribing)}`;
    }).join('');
}

let replaySource = null;
function replay(idx) {
    const e = radioLog[idx];
    if (!e) return;
    if (!e.pcmLen && e.recId) return playRecording(e.recId);
    if (!e.pcmLen || !audioCtx) return;
    ensureAudio();
    if (replaySource) { try { replaySource.stop(); } catch (_) {} }
    const buf = audioCtx.createBuffer(1, e.pcmLen, audioCtx.sampleRate);
    const data = buf.getChannelData(0);
    let o = 0;
    for (const p of e.pcm) { data.set(p, o); o += p.length; }
    replaySource = audioCtx.createBufferSource();
    replaySource.buffer = buf;
    replaySource.connect(playGain || audioCtx.destination);
    replaySource.start();
}

function clearLog() { radioLog.length = 0; renderLog(); }

function exportLog() {
    const lines = [['time', 'agency', 'channel', 'unit', 'seconds', 'note', 'transcript']];
    for (const e of [...radioLog].reverse()) {
        lines.push([new Date(e.start).toISOString(), agencyName(e.agency), channelLabel(e.agency, e.channel), e.user,
            e.emergency ? '' : Math.round((e.end - e.start) / 1000), e.emergency ? (e.active ? 'EMERGENCY' : 'emergency cleared') : '', e.text || '']);
    }
    const csv = lines.map(r => r.map(v => `"${String(v).replace(/"/g, '""')}"`).join(',')).join('\n');
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([csv], { type: 'text/csv' }));
    a.download = `radio-log-${new Date().toISOString().slice(0, 10)}.csv`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

function renderTalking() {
    const el = document.getElementById('talking-list');
    const list = [...talkState.values()].sort((a, b) => a.start - b.start);
    if (!list.length) { el.innerHTML = '<div class="empty-small">Quiet</div>'; return; }
    el.innerHTML = list.map(t => `<div class="talking-item" onclick="focusUnit('${escAttr(t.agency + '|' + t.user)}')">
        <span class="status-dot dot-red pulse"></span>
        <div><div class="name">${escHtml(t.user)}</div><div class="where">${escHtml(channelLabel(t.agency, t.channel))}${!currentAgency ? ' · ' + escHtml(agencyName(t.agency)) : ''} · ${fmtDur(Date.now() - t.start)}</div></div>
        ${t.entry.heard ? '<span class="heard">🔊 heard</span>' : ''}
    </div>`).join('');
}

function renderHeader() {
    document.getElementById('hdr-units').textContent = units.length;
    document.getElementById('hdr-located').textContent = units.filter(u => u.location).length + takUnits.length;
    document.getElementById('hdr-talking').textContent = new Set([...talkState.values()].map(t => t.agency + '|' + t.user)).size;
    const open = [...emergencies.values()].filter(e => e.active && !e.acked).length;
    document.getElementById('hdr-emergency').style.display = open ? 'flex' : 'none';
    document.getElementById('hdr-emergency-count').textContent = open;
}

// ─── Emergencies ───────────────────────────────────────────────────────────

function handleEmergency(m) {
    const key = `${m.agency}|${m.user}`;
    const prev = emergencies.get(key);
    const e = prev || { agency: m.agency, user: m.user };
    e.channel = m.channel;
    e.tak = !!m.tak;
    e.alert = m.alert || '';
    e.active = !!m.active;
    e.at = typeof m.at === 'number' ? m.at : Date.now();
    if (m.lat !== undefined) { e.lat = m.lat; e.lon = m.lon; }
    if (e.active) e.acked = false;
    emergencies.set(key, e);
    radioLog.unshift({ agency: m.agency, channel: m.channel, user: m.user, start: e.at, end: e.at,
        emergency: true, active: e.active, heard: false, pcm: [], pcmLen: 0 });
    if (e.active) alarm();
    renderEmergencies(); renderHeader(); renderLog(); renderMarkers(true);
    if (e.active) focusEmergencies();
}

function renderEmergencies() {
    const el = document.getElementById('emerg-list');
    const list = [...emergencies.values()].sort((a, b) => (b.active - a.active) || (b.at - a.at));
    document.getElementById('emerg-panel').classList.toggle('has-active', list.some(e => e.active && !e.acked));
    if (!list.length) { el.innerHTML = '<div class="empty-small">No active emergencies</div>'; return; }
    el.innerHTML = list.map(e => {
        const k = escAttr(e.agency + '|' + e.user);
        return `<div class="emerg-item${e.active ? ' active' : ''}${e.acked ? ' acked' : ''}">
            <div class="emerg-head"><span>${e.active ? WARN_ICON : ''}${escHtml(e.user)}</span><span>${e.active ? (e.acked ? 'ACKNOWLEDGED' : 'ACTIVE') : 'Cleared'}</span></div>
            <div class="emerg-meta">${e.tak ? escHtml(e.alert || 'TAK alert') : escHtml(channelLabel(e.agency, e.channel))} · ${escHtml(agencyName(e.agency))} · ${fmtTime(e.at)} (${ago(e.at)})</div>
            <div class="emerg-actions">
                <button class="mini-btn" onclick="locateEmergency('${k}')">Locate</button>
                ${e.tak ? '' : `<button class="mini-btn" onclick="selectTx('${escAttr(e.agency)}', ${e.channel})">Talk on channel</button>`}
                ${e.active && !e.acked ? `<button class="mini-btn" onclick="ackEmergency('${k}')">Acknowledge</button>` : ''}
                ${!e.active ? `<button class="mini-btn" onclick="emergencies.delete('${k}'); renderEmergencies();">Dismiss</button>` : ''}
            </div></div>`;
    }).join('');
}

function ackEmergency(key) {
    const e = emergencies.get(key);
    if (e) e.acked = true;
    renderEmergencies(); renderHeader();
}

function locateEmergency(key) {
    const e = emergencies.get(key);
    if (!e) return;
    if (e.tak && e.lat !== undefined && map) { centerOn([e.lat, e.lon], 16); return; }  // where the alert was raised
    if (markers.has(key) || e.tak) return focusUnit(key);           // the unit's live position
    if (e.lat !== undefined && map) centerOn([e.lat, e.lon], 16);
    else flashMessage(`${e.user} has not shared a position.`);
}

function focusEmergencies() {
    document.getElementById('emerg-panel').scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}

function alarm() {
    try {
        ensureAudio();
        const t0 = audioCtx.currentTime;
        for (let i = 0; i < 3; i++) {
            const o = audioCtx.createOscillator(), g = audioCtx.createGain();
            o.type = 'square';
            o.frequency.setValueAtTime(960, t0 + i * 0.5);
            o.frequency.setValueAtTime(720, t0 + i * 0.5 + 0.2);
            g.gain.setValueAtTime(0.12, t0 + i * 0.5);
            g.gain.setValueAtTime(0, t0 + i * 0.5 + 0.4);
            o.connect(g).connect(audioCtx.destination);
            o.start(t0 + i * 0.5); o.stop(t0 + i * 0.5 + 0.42);
        }
    } catch (_) {}
}

// ─── Audio engine ───────────────────────────────────────────────────────────
// Field units talk 16 kHz Opus. The console decodes and encodes it in the
// browser (WebCodecs AudioDecoder / AudioEncoder), so the server only re-frames
// packets. Playback goes through an AudioWorklet that mixes one jitter-buffered
// queue per talker — replacing per-frame BufferSource scheduling, which
// doubled and stalled. Wire format: see dispatch_audio.go on the server.

const FRAME_AUDIO_OUT = 0xA0;   // server -> console: field audio
const FRAME_AUDIO_IN = 0xB0;    // console -> server: our microphone
const OPUS_RATE = 16000;
const OPUS_FRAME = 320;         // 20 ms at 16 kHz
const PREBUFFER_MS = 80;        // per talker, before playback starts
const MAX_QUEUE_MS = 600;       // beyond this a talker's oldest audio is dropped

let audioCtx = null;
let playerNode = null;
let playGain = null;
let playAnalyser = null;
let workletReady = null;
const talkers = new Map();      // "agency/sender" -> { decoder, ts, lastHeard, name, agency }

const WORKLET_SRC = `
class Player extends AudioWorkletProcessor {
  constructor(opts) {
    super();
    this.q = new Map();          // id -> { bufs: [Float32Array], off, started, queued }
    this.pre = opts.processorOptions.prebuffer;
    this.max = opts.processorOptions.maxQueue;
    this.port.onmessage = (e) => {
      const { id, samples } = e.data;
      let t = this.q.get(id);
      if (!t) { t = { bufs: [], off: 0, started: false, queued: 0 }; this.q.set(id, t); }
      t.bufs.push(samples); t.queued += samples.length;
      while (t.queued > this.max && t.bufs.length > 1) { t.queued -= t.bufs[0].length - t.off; t.bufs.shift(); t.off = 0; }
      if (t.queued >= this.pre) t.started = true;
    };
  }
  process(inputs, outputs) {
    const out = outputs[0][0];
    out.fill(0);
    for (const [id, t] of this.q) {
      if (!t.started) continue;
      let i = 0;
      while (i < out.length && t.bufs.length) {
        const b = t.bufs[0];
        const n = Math.min(out.length - i, b.length - t.off);
        for (let k = 0; k < n; k++) out[i + k] += b[t.off + k];
        i += n; t.off += n; t.queued -= n;
        if (t.off >= b.length) { t.bufs.shift(); t.off = 0; }
      }
      if (!t.bufs.length) { t.started = false; t.queued = 0; }  // underrun: re-buffer
    }
    for (let k = 0; k < out.length; k++) out[k] = Math.max(-1, Math.min(1, out[k]));
    return true;
  }
}
registerProcessor('mr-player', Player);

class Capture extends AudioWorkletProcessor {
  process(inputs) {
    const ch = inputs[0] && inputs[0][0];
    if (ch && ch.length) this.port.postMessage(ch.slice(0));
    return true;
  }
}
registerProcessor('mr-capture', Capture);
`;

function audioSupported() {
    return typeof AudioDecoder !== 'undefined' && typeof AudioEncoder !== 'undefined'
        && typeof AudioWorkletNode !== 'undefined';
}

// Create the shared AudioContext + worklets. Must first run inside a user
// gesture (browsers block audio until then).
function ensureAudio() {
    if (audioCtx && audioCtx.state !== 'closed') {
        if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
        return workletReady;
    }
    audioCtx = new AudioContext({ latencyHint: 'interactive' });
    const url = URL.createObjectURL(new Blob([WORKLET_SRC], { type: 'application/javascript' }));
    workletReady = audioCtx.audioWorklet.addModule(url).then(() => {
        const rate = audioCtx.sampleRate;
        playerNode = new AudioWorkletNode(audioCtx, 'mr-player', {
            numberOfInputs: 0, numberOfOutputs: 1, outputChannelCount: [1],
            processorOptions: { prebuffer: Math.round(rate * PREBUFFER_MS / 1000), maxQueue: Math.round(rate * MAX_QUEUE_MS / 1000) },
        });
        playGain = audioCtx.createGain();
        playGain.gain.value = volumeLevel;
        playAnalyser = audioCtx.createAnalyser();
        playAnalyser.fftSize = 256;
        playerNode.connect(playGain).connect(audioCtx.destination);
        playGain.connect(playAnalyser);
        console.log('[dispatch] audio ready at ' + rate + ' Hz');
    }).catch(e => { console.error('[dispatch] audio worklet failed', e); throw e; });
    return workletReady;
}

let audioUnlocked = false;

// Browsers block audio until the user interacts with the page. The banner
// guarantees a gesture; any click or key also unlocks.
function unlockAudioOnGesture() {
    const banner = document.getElementById('audio-unlock-banner');
    if (!audioSupported()) {
        if (banner) {
            banner.textContent = 'This browser cannot play MeshRelay audio. Use a current Chrome, Edge, or Firefox.';
            banner.style.background = '#B45309';
            banner.style.display = 'flex';
        }
        return;
    }
    const unlock = () => {
        try {
            ensureAudio();
            if (audioCtx.state === 'suspended') audioCtx.resume().then(markUnlocked).catch(() => {});
            else markUnlocked();
        } catch (_) {}
    };
    function markUnlocked() {
        if (audioCtx && audioCtx.state === 'running') {
            audioUnlocked = true;
            if (banner) banner.style.display = 'none';
            document.removeEventListener('click', unlock);
            document.removeEventListener('keydown', unlock);
            document.removeEventListener('touchstart', unlock);
        }
    }
    document.addEventListener('click', unlock);
    document.addEventListener('keydown', unlock);
    document.addEventListener('touchstart', unlock);
    if (banner) { banner.style.display = 'flex'; banner.onclick = unlock; }
}

// Resample mono float samples (linear; speech-grade).
function resample(input, fromRate, toRate) {
    if (fromRate === toRate) return input;
    const outLen = Math.max(1, Math.round(input.length * toRate / fromRate));
    const out = new Float32Array(outLen);
    const step = fromRate / toRate;
    for (let i = 0; i < outLen; i++) {
        const pos = i * step, i0 = Math.floor(pos), f = pos - i0;
        const a = input[Math.min(i0, input.length - 1)], b = input[Math.min(i0 + 1, input.length - 1)];
        out[i] = a + (b - a) * f;
    }
    return out;
}

// ─── Incoming audio ─────────────────────────────────────────────────────────

function onAudioFrame(buf) {
    // [0xA0][u32 channel][u16 len|sender][u16 len|agency][opus]
    const v = new DataView(buf);
    let o = 1;
    if (buf.byteLength < 9) return;
    const channel = v.getUint32(o); o += 4;
    const sl = v.getUint16(o); o += 2;
    const sender = new TextDecoder().decode(new Uint8Array(buf, o, sl)); o += sl;
    const al = v.getUint16(o); o += 2;
    const agency = new TextDecoder().decode(new Uint8Array(buf, o, al)); o += al;
    const opus = new Uint8Array(buf, o);
    if (!opus.length) return;

    noteTalk(agency, channel, sender, true);
    showReceiving(sender, agency, channel);
    if (!audioCtx || !playerNode || audioCtx.state !== 'running') {
        if (!audioUnlocked) { const b = document.getElementById('audio-unlock-banner'); if (b) b.style.display = 'flex'; }
        return;
    }
    const id = agency + '/' + sender;
    let t = talkers.get(id);
    if (t) t.channel = channel;
    if (!t) {
        t = { ts: 0, lastHeard: 0, name: sender, agency, channel };
        t.decoder = new AudioDecoder({
            output: (data) => {
                try {
                    const pcm = new Float32Array(data.numberOfFrames);
                    data.copyTo(pcm, { planeIndex: 0, format: 'f32-planar' });
                    const samples = resample(pcm, data.sampleRate, audioCtx.sampleRate);
                    recordAudio(agency, t.channel, sender, samples);
                    playerNode.port.postMessage({ id, samples }, [samples.buffer]);
                } finally { data.close(); }
            },
            error: (e) => { console.warn('[dispatch] decoder error for ' + id, e); talkers.delete(id); },
        });
        t.decoder.configure({ codec: 'opus', sampleRate: OPUS_RATE, numberOfChannels: 1 });
        talkers.set(id, t);
    }
    t.lastHeard = performance.now();
    try {
        t.decoder.decode(new EncodedAudioChunk({ type: 'key', timestamp: t.ts, data: opus }));
        t.ts += 20000;
    } catch (e) {
        console.warn('[dispatch] decode failed', e);
    }
}

// Close decoders for talkers who have gone quiet.
setInterval(() => {
    const now = performance.now();
    for (const [id, t] of talkers) {
        if (now - t.lastHeard > 10000) {
            try { t.decoder.close(); } catch (_) {}
            talkers.delete(id);
        }
    }
}, 5000);

let receivingTimer = null;
function showReceiving(sender, agency, channel) {
    document.getElementById('status-receiving').style.display = 'flex';
    document.getElementById('receiving-from').textContent = `${unitLabel(agency, sender)} · ${channelLabel(agency, channel)}`;
    if (!isPTT && playAnalyser && !vizAnimId) drawViz(playAnalyser);
    if (receivingTimer) clearTimeout(receivingTimer);
    receivingTimer = setTimeout(() => {
        document.getElementById('status-receiving').style.display = 'none';
        document.getElementById('receiving-from').textContent = '';
        if (!isPTT) clearViz();
    }, 900);
}

// ─── Microphone + PTT ───────────────────────────────────────────────────────

let micStream = null;
let micSource = null;
let micNode = null;
let micSink = null;
let micAnalyser = null;
let encoder = null;
let encTs = 0;
let encBuf = new Float32Array(OPUS_FRAME);
let encFill = 0;
let isPTT = false;
let floorState = 'idle';   // idle | requested | granted | queued | denied

async function requestMic() {
    const errEl = document.getElementById('mic-error-text');
    const httpBlocked = location.protocol === 'http:' && location.hostname !== 'localhost' && location.hostname !== '127.0.0.1';
    try {
        if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
            throw new Error('mediaDevices unavailable (requires HTTPS)');
        }
        micStream = await navigator.mediaDevices.getUserMedia({
            audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
        });
        document.getElementById('status-mic-error').style.display = 'none';
    } catch (e) {
        document.getElementById('status-mic-error').style.display = 'flex';
        errEl.innerHTML = httpBlocked
            ? 'Talking needs HTTPS — open this console over https:// to use the microphone. Listening works.'
            : 'Microphone blocked — <a href="#" onclick="requestMic(); return false;">Retry</a>';
        console.warn('Mic unavailable:', e);
    }
}

function makeEncoder() {
    const enc = new AudioEncoder({
        output: (chunk) => {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            const out = new Uint8Array(1 + chunk.byteLength);
            out[0] = FRAME_AUDIO_IN;
            chunk.copyTo(out.subarray(1));
            ws.send(out.buffer);
        },
        error: (e) => { console.error('[dispatch] encoder error', e); stopPTT(); },
    });
    enc.configure({
        codec: 'opus', sampleRate: OPUS_RATE, numberOfChannels: 1, bitrate: 24000,
        opus: { frameDuration: 20000, complexity: 5 },
    });
    return enc;
}

// Feed captured samples (context rate) to the encoder in 20 ms / 16 kHz frames.
function onMicSamples(samples) {
    if (!isPTT || !encoder) return;
    const pcm = resample(samples, audioCtx.sampleRate, OPUS_RATE);
    let i = 0;
    while (i < pcm.length) {
        const n = Math.min(OPUS_FRAME - encFill, pcm.length - i);
        encBuf.set(pcm.subarray(i, i + n), encFill);
        encFill += n; i += n;
        if (encFill === OPUS_FRAME) {
            const frame = new AudioData({
                format: 'f32', sampleRate: OPUS_RATE, numberOfFrames: OPUS_FRAME,
                numberOfChannels: 1, timestamp: encTs, data: encBuf,
            });
            encTs += 20000;
            try { encoder.encode(frame); } finally { frame.close(); }
            encBuf = new Float32Array(OPUS_FRAME);
            encFill = 0;
        }
    }
}

async function startPTT(e) {
    if (e) e.preventDefault();
    if (isPTT) return;
    if (!audioSupported()) return;
    if (!micStream) {
        document.getElementById('status-mic-error').style.display = 'flex';
        if (location.protocol === 'https:') requestMic();
        return;
    }
    if (!ws || ws.readyState !== WebSocket.OPEN || !txKey) return;

    isPTT = true;
    setPttUi('requested');
    ws.send(JSON.stringify({ type: 'ptt', down: true }));
    try {
        await ensureAudio();
        if (!isPTT) return;                      // released while starting
        encoder = makeEncoder();
        encTs = 0; encFill = 0;
        micSource = audioCtx.createMediaStreamSource(micStream);
        micNode = new AudioWorkletNode(audioCtx, 'mr-capture', { numberOfInputs: 1, numberOfOutputs: 1 });
        micNode.port.onmessage = (ev) => onMicSamples(ev.data);
        micSink = audioCtx.createGain();
        micSink.gain.value = 0;                  // keep the node pulled, never audible
        micAnalyser = audioCtx.createAnalyser();
        micAnalyser.fftSize = 256;
        micSource.connect(micNode).connect(micSink).connect(audioCtx.destination);
        micSource.connect(micAnalyser);
        drawViz(micAnalyser);
    } catch (err) {
        console.error('PTT start error:', err);
        stopPTT();
    }
}

function stopPTT() {
    if (!isPTT) return;
    isPTT = false;
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'ptt', down: false }));
    try {
        if (micNode) { micNode.port.onmessage = null; micNode.disconnect(); }
        if (micSource) micSource.disconnect();
        if (micSink) micSink.disconnect();
        if (encoder && encoder.state !== 'closed') { encoder.flush().catch(() => {}).finally(() => { try { encoder.close(); } catch (_) {} }); }
    } catch (_) {}
    micNode = micSource = micSink = micAnalyser = null;
    setTimeout(() => { if (!isPTT) encoder = null; }, 200);
    setPttUi('idle');
    clearViz();
}

// Floor / emergency packets from the server, same layout the apps receive.
function onControlPacket(buf) {
    const v = new DataView(buf);
    const type = v.getUint8(0);
    let o = 1;
    const str = () => { const n = v.getUint16(o); o += 2; const s = new TextDecoder().decode(new Uint8Array(buf, o, n)); o += n; return s; };
    try {
        if (type === 0x21) {            // granted: [i32 ch][str user][u16 dur][u8 queuePos]
            o += 4; str(); o += 2;
            const queuePos = v.getUint8(o);
            if (isPTT) setPttUi(queuePos > 0 ? 'queued' : 'granted', queuePos);
        } else if (type === 0x22) {     // denied: [i32 ch][u8 reason][u8 queuePos][str holder]
            o += 4;
            const reason = v.getUint8(o++); const queuePos = v.getUint8(o++); const holder = str();
            if (!isPTT) return;
            if (queuePos > 0) setPttUi('queued', queuePos, holder);
            else { setPttUi('denied', 0, holder, reason); stopPttAfterDeny(); }
        } else if (type === 0x23) {     // preempted: [i32 ch][str by][u8 pri]
            o += 4; const by = str();
            if (isPTT) { setPttUi('denied', 0, by, 'preempted'); stopPttAfterDeny(); }
        } else if (type === 0x26) {     // emergency: [i32 ch][str user][u8 active]
            const ch = v.getInt32(o); o += 4; const user = str(); const active = v.getUint8(o) === 1;
            showEmergency(ch, user, active);
        }
    } catch (e) { console.warn('[dispatch] bad control packet', e); }
}

let denyTimer = null;
function stopPttAfterDeny() {
    // Stop sending, but keep the "busy" message on screen for a moment.
    const keepUi = floorState;
    isPTT = true; stopPTT();
    floorState = keepUi;
    renderFloorState(lastFloorDetail);
    if (denyTimer) clearTimeout(denyTimer);
    denyTimer = setTimeout(() => { if (!isPTT) setPttUi('idle'); }, 2500);
}

let lastFloorDetail = {};
function setPttUi(state, queuePos = 0, holder = '', reason = null) {
    floorState = state;
    lastFloorDetail = { queuePos, holder, reason };
    renderFloorState(lastFloorDetail);
}

function renderFloorState({ queuePos, holder, reason }) {
    const btn = document.getElementById('ptt-btn');
    const tx = document.getElementById('status-transmitting');
    const txText = document.getElementById('transmitting-text');
    const ready = document.getElementById('status-ready');
    btn.classList.toggle('active', floorState === 'granted' || floorState === 'requested');
    btn.classList.toggle('busy', floorState === 'denied' || floorState === 'queued');
    const show = floorState !== 'idle';
    tx.style.display = show ? 'flex' : 'none';
    ready.style.display = show ? 'none' : 'flex';
    if (!txText) return;
    if (floorState === 'requested') txText.textContent = 'Transmitting…';
    else if (floorState === 'granted') txText.textContent = 'Transmitting';
    else if (floorState === 'queued') txText.textContent = `Queued #${queuePos}` + (holder ? ` — ${holder} is talking` : '');
    else if (floorState === 'denied') txText.textContent = reason === 'preempted'
        ? `Interrupted by ${holder || 'a higher priority user'}`
        : (reason === 1 ? 'Not allowed to talk on this channel' : `Channel busy${holder ? ' — ' + holder + ' is talking' : ''}`);
}

function showEmergency(ch, user, active) {
    handleEmergency({ agency: txAgency(), channel: ch, user, active });
}

// Hold Space to talk (when not typing in a field).
document.addEventListener('keydown', (e) => {
    if (e.code !== 'Space' || e.repeat) return;
    const tag = (e.target && e.target.tagName) || '';
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    e.preventDefault();
    startPTT();
});
document.addEventListener('keyup', (e) => {
    if (e.code !== 'Space') return;
    const tag = (e.target && e.target.tagName) || '';
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    e.preventDefault();
    stopPTT();
});
window.addEventListener('blur', () => stopPTT());

// ─── Volume ─────────────────────────────────────────────────────────────────

function setVolume(val) {
    volumeLevel = parseFloat(val);
    document.getElementById('volume-display').textContent = Math.round(volumeLevel * 100) + '%';
    if (playGain) playGain.gain.value = volumeLevel;
}

// ─── Unit status ───────────────────────────────────────────────────────────

const STATUS_NAMES = ['No status', 'Available', 'Busy', 'En route', 'On scene', 'Out of service'];
const STATUS_COLORS = ['#8B949E', '#22C55E', '#F59E0B', '#3B82F6', '#58A6FF', '#EF4444'];
function statusColor(code) { return STATUS_COLORS[code] || '#8B949E'; }

function setUnitStatus(agency, user, code) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: 'status', agency, user, code: parseInt(code, 10) }));
}

function onStatus(m) {
    const u = units.find(x => x.agency_id === m.agency && x.username === m.user);
    if (u) u.status = m.code ? { code: m.code, name: m.name, by: m.by, at: m.at } : null;
    radioLog.unshift({ agency: m.agency, channel: u ? u.channel : 0, user: m.user, start: Date.now(), end: Date.now(),
        note: `Status: ${m.name || 'cleared'}${m.by ? ' (by ' + m.by + ')' : ''}`, heard: false, pcm: [], pcmLen: 0 });
    renderUnits(); renderLog();
}

// ─── Messages ──────────────────────────────────────────────────────────────

const messages = [];

// Incoming messages count as read once this console is on screen and in
// focus; the unit then sees "Read" (the server tells it, once).
function consoleIsLooking() { return document.visibilityState === 'visible' && document.hasFocus(); }

function markVisibleMessagesRead() {
    if (!consoleIsLooking() || !ws || ws.readyState !== WebSocket.OPEN) return;
    const ids = messages.filter(m => m.dir === 'in' && !m.read && !m.readSent).map(m => m.id);
    if (!ids.length) return;
    messages.forEach(m => { if (ids.includes(m.id)) m.readSent = true; });
    ws.send(JSON.stringify({ type: 'read', ids }));
}
document.addEventListener('visibilitychange', () => { markVisibleMessagesRead(); renderMessages(); });
window.addEventListener('focus', () => { markVisibleMessagesRead(); renderMessages(); });

function onMessage(m, live) {
    messages.push(m);
    if (messages.length > 300) messages.shift();
    if (live && m.dir === 'in') chime();
    if (live) { markVisibleMessagesRead(); renderMessages(); }
}

function onMessageRead(id) {
    const m = messages.find(x => x.id === id);
    if (m) m.read = true;
    renderMessages();
}

function renderMessages() {
    const el = document.getElementById('msg-list');
    const badge = document.getElementById('msg-unread');
    const unread = messages.filter(m => m.dir === 'in' && !m.read && !m.readSent).length;
    badge.style.display = unread ? 'inline' : 'none';
    badge.textContent = unread;
    if (!messages.length) { el.innerHTML = '<div class="empty-small">No messages yet</div>'; return; }
    el.innerHTML = messages.slice(-80).map(m => {
        const who = m.dir === 'in'
            ? (m.tak_uid
                ? `<b onclick="messageTak('${escAttr(m.agency)}', '${escAttr(m.tak_uid)}', '${escAttr(m.from)}')">${escHtml(m.from)}</b> <span class="tak-tag">TAK</span> → Dispatch`
                : `<b onclick="messageUnit('${escAttr(m.agency)}', '${escAttr(m.from)}')">${escHtml(m.from)}</b> → Dispatch`)
            : `${escHtml(m.from)} → <b>${escHtml(m.to)}</b>${m.queued ? ` · ${m.queued} offline (will deliver)` : ''}${m.sent === 0 && !m.queued ? ' · nobody reached' : ''}`;
        const unreadCls = m.dir === 'in' && !m.read && !m.readSent ? ' unread' : '';
        return `<div class="msg ${m.dir}${unreadCls}"><div class="msg-meta">${who} · ${fmtTime(m.at)}</div>${escHtml(m.text)}</div>`;
    }).join('');
    el.scrollTop = el.scrollHeight;
}

function renderMsgTargets() {
    const sel = document.getElementById('msg-target');
    if (!sel || sel === document.activeElement) return;
    const prev = sel.value;
    let html = '';
    if (txKey) {
        const [agency, ch] = splitKey(txKey);
        if (ch !== 'all') html += `<option value="ch|${escHtml(txKey)}">Everyone on ${escHtml(channelLabel(agency, ch))}</option>`;
    }
    for (const a of directory.agencies) html += `<option value="all|${escHtml(a.id)}">All units · ${escHtml(a.name)}</option>`;
    for (const u of [...units].sort((x, y) => x.username.localeCompare(y.username))) {
        html += `<option value="unit|${escHtml(u.agency_id)}|${escHtml(u.username)}">${escHtml(unitLabel(u.agency_id, u.username))}</option>`;
    }
    for (const t of [...takUnits].sort((x, y) => x.callsign.localeCompare(y.callsign))) {
        html += `<option value="tak|${escHtml(t.agency)}|${escHtml(t.uid)}">${escHtml(t.callsign)} (TAK)</option>`;
    }
    sel.innerHTML = html;
    if (prev && [...sel.options].some(o => o.value === prev)) sel.value = prev;
}

function messageUnit(agency, user) {
    renderMsgTargets();
    const sel = document.getElementById('msg-target');
    const v = `unit|${agency}|${user}`;
    if (![...sel.options].some(o => o.value === v)) {
        const o = document.createElement('option');
        o.value = v; o.textContent = `${user} (offline)`;
        sel.appendChild(o);
    }
    sel.value = v;
    document.getElementById('msg-text').focus();
}

function messageTak(agency, uid, callsign) {
    renderMsgTargets();
    const sel = document.getElementById('msg-target');
    const v = `tak|${agency}|${uid}`;
    if (![...sel.options].some(o => o.value === v)) {
        const o = document.createElement('option');
        o.value = v; o.textContent = `${callsign || uid} (TAK)`;
        sel.appendChild(o);
    }
    sel.value = v;
    document.getElementById('msg-text').focus();
}

function takPopup(uid) {
    const t = takUnits.find(x => x.uid === uid);
    if (!t) return 'No longer reporting';
    const rows = [
        [t.group, t.role].filter(Boolean).join(' · '),
        `TAK user${!currentAgency ? ' · ' + escHtml(agencyName(t.agency)) : ''}${t.device ? ' · ' + escHtml(t.device) : ''}`,
        `Position ${ago(Date.parse(t.at))}${t.stale ? ' (stale)' : ''}`,
        t.speed > 0.5 ? `Moving ${Math.round(t.speed * 2.237)} mph, heading ${Math.round(t.course || 0)}°` : '',
        t.battery ? `Battery ${t.battery}%` : '',
        `${t.lat.toFixed(5)}, ${t.lon.toFixed(5)}`,
    ].filter(Boolean).map(r => `<div class="popup-row">${escHtml(r)}</div>`).join('');
    return `<div class="popup-title">${escHtml(t.callsign)} <span class="tak-tag">TAK</span></div>${rows}
        <div class="popup-actions"><button onclick="messageTak('${escAttr(t.agency)}', '${escAttr(t.uid)}', '${escAttr(t.callsign)}')">Message</button></div>`;
}

// ATAK team colors.
const TAK_TEAMS = { white: '#FFFFFF', yellow: '#FACC15', orange: '#F97316', magenta: '#D946EF', red: '#EF4444', maroon: '#9F1239',
    purple: '#8B5CF6', 'dark blue': '#1E40AF', blue: '#3B82F6', cyan: '#22D3EE', teal: '#14B8A6', green: '#22C55E',
    'dark green': '#166534', brown: '#92400E' };
function takTeamColor(group) { return TAK_TEAMS[(group || '').toLowerCase()] || '#22D3EE'; }

function sendMessage() {
    const input = document.getElementById('msg-text');
    const text = input.value.trim();
    const target = document.getElementById('msg-target').value;
    if (!text || !target || !ws || ws.readyState !== WebSocket.OPEN) return;
    const parts = target.split('|');
    const msg = { type: 'message', text };
    if (parts[0] === 'unit') msg.unit = { agency: parts[1], user: parts.slice(2).join('|') };
    else if (parts[0] === 'tak') msg.tak = { agency: parts[1], uid: parts.slice(2).join('|') };
    else if (parts[0] === 'ch') msg.to_channel = parts[1];
    else msg.to_agency = parts[1];
    ws.send(JSON.stringify(msg));
    input.value = '';
}

function chime() {
    try {
        ensureAudio();
        const t0 = audioCtx.currentTime;
        [880, 1320].forEach((f, i) => {
            const o = audioCtx.createOscillator(), g = audioCtx.createGain();
            o.frequency.value = f;
            g.gain.setValueAtTime(0.08, t0 + i * 0.12);
            g.gain.exponentialRampToValueAtTime(0.0001, t0 + i * 0.12 + 0.18);
            o.connect(g).connect(audioCtx.destination);
            o.start(t0 + i * 0.12); o.stop(t0 + i * 0.12 + 0.2);
        });
    } catch (_) {}
}

// ─── Server recordings ─────────────────────────────────────────────────────

// A finished recording: attach it to its live log entry so it can be replayed
// even if this console was not monitoring that channel.
function onRecording(r) {
    for (const e of radioLog) {
        if (!e.recId && !e.emergency && !e.note && e.agency === r.agency && e.user === r.user
            && (e.channel === r.channel || e.channel === 'all')
            && Math.abs(e.start - r.start) < 3000) {
            e.recId = r.id;
            e.transcribing = !!r.transcribing;
            break;
        }
    }
    renderLog();
}

// A transcript of a recording (server-side speech recognition).
function onTranscript(t) {
    for (const e of radioLog) {
        if (e.recId === t.id) { e.text = t.text || ''; e.transcribing = false; break; }
    }
    renderLog();
    const h = historyRows.find(r => r.id === t.id);
    if (h) { h.text = t.text || ''; h.transcribed = true; renderHistory(); }
}

// The transcript line shown under a transmission.
function transcriptRow(text, pending, cols = 5) {
    const lead = '<td></td>'.repeat(cols - 4);
    if (pending) return `<tr class="trx">${lead}<td colspan="4" class="trx-text pending">transcribing…</td></tr>`;
    if (!text) return '';
    return `<tr class="trx">${lead}<td colspan="4" class="trx-text" title="Machine transcript — may contain mistakes">${escHtml(text)}</td></tr>`;
}

function showLogTab(name) {
    document.querySelectorAll('.log-tab').forEach(t => t.classList.toggle('active', t.dataset.logtab === name));
    const live = name === 'live';
    document.getElementById('live-wrap').style.display = live ? 'block' : 'none';
    document.getElementById('live-tools').style.display = live ? 'flex' : 'none';
    document.getElementById('history-wrap').style.display = live ? 'none' : 'block';
    document.getElementById('history-tools').style.display = live ? 'none' : 'flex';
    if (!live) {
        const d = document.getElementById('hist-date');
        if (!d.value) d.value = new Date(Date.now() - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 10);
        const sel = document.getElementById('hist-channel');
        const prev = sel.value;
        sel.innerHTML = '<option value="">All channels</option>' + directory.channels.map(c =>
            `<option value="${escHtml(c.agency)}|${c.channel}">${escHtml(channelLabel(c.agency, c.channel))}${!currentAgency ? ' · ' + escHtml(agencyName(c.agency)) : ''}</option>`).join('');
        sel.value = prev;
        searchRecordings();
    }
}

let historyRows = [];
async function searchRecordings() {
    const date = document.getElementById('hist-date').value;
    const ch = document.getElementById('hist-channel').value;
    const unit = document.getElementById('hist-unit').value.trim();
    const words = document.getElementById('hist-text').value.trim();
    const q = new URLSearchParams({ date, limit: 500 });
    if (ch) { const [a, n] = ch.split('|'); q.set('agency', a); q.set('channel', n); }
    if (unit) q.set('user', unit);
    if (words) q.set('text', words);
    try {
        const r = await fetch('/api/recordings?' + q.toString(), { credentials: 'same-origin' });
        historyRows = await r.json();
    } catch (e) { historyRows = []; }
    histSelected.clear();
    document.getElementById('hist-empty').style.display = historyRows.length ? 'none' : 'block';
    document.getElementById('hist-empty').textContent = historyRows.length ? '' : `No recordings on ${date}${ch || unit || words ? ' matching that filter' : ''}.`;
    renderHistory();
}

function renderHistory() {
    const body = document.getElementById('hist-body');
    body.innerHTML = historyRows.map((r, i) => `<tr>
        <td class="hist-sel"><input type="checkbox" class="hist-check" ${histSelected.has(r.id) ? 'checked' : ''} onchange="toggleHistRow('${escAttr(r.id)}', this.checked)" title="Select for download"></td>
        <td>${fmtTime(r.start)}</td>
        <td class="log-ch" title="${escHtml(channelLabel(r.agency, r.channel))}">${escHtml(channelLabel(r.agency, r.channel))}${!currentAgency && directory.agencies.length > 1 ? ' · ' + escHtml(agencyName(r.agency)) : ''}</td>
        <td><span class="log-unit">${escHtml(r.user)}</span></td>
        <td>${fmtDur(r.duration_ms)}</td>
        <td><button class="replay-btn" onclick="playRecording('${escAttr(r.id)}')">▶ Play</button></td></tr>${transcriptRow(r.text, false, 6)}`).join('');
    updateHistSelection();
}

// Fetch a recording's .mrr file ([u32 ms][u16 len][Opus] frames).
async function fetchRecording(id) {
    const r = await fetch(`/api/recordings/${encodeURIComponent(id)}/audio`, { credentials: 'same-origin' });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    return r.arrayBuffer();
}

// Decode a recording to mono PCM at `rate`, pauses preserved.
async function decodeRecording(buf, rate) {
    const v = new DataView(buf);
    const frames = [];
    for (let o = 0; o + 6 <= buf.byteLength;) {
        const at = v.getUint32(o), n = v.getUint16(o + 4);
        o += 6;
        if (o + n > buf.byteLength) break;
        frames.push({ at, data: new Uint8Array(buf, o, n) });
        o += n;
    }
    if (!frames.length) return new Float32Array(0);
    const total = Math.ceil((frames[frames.length - 1].at + 40) / 1000 * rate);
    const out = new Float32Array(total);
    await new Promise((resolve) => {
        const dec = new AudioDecoder({
            output: (d) => {
                try {
                    const pcm = new Float32Array(d.numberOfFrames);
                    d.copyTo(pcm, { planeIndex: 0, format: 'f32-planar' });
                    const res = resample(pcm, d.sampleRate, rate);
                    const at = Math.round(d.timestamp / 1e6 * rate);
                    out.set(res.subarray(0, Math.max(0, Math.min(res.length, total - at))), at);
                } finally { d.close(); }
            },
            error: () => resolve(),
        });
        dec.configure({ codec: 'opus', sampleRate: OPUS_RATE, numberOfChannels: 1 });
        for (const f of frames) {
            try { dec.decode(new EncodedAudioChunk({ type: 'key', timestamp: f.at * 1000, data: f.data })); } catch (_) {}
        }
        dec.flush().then(() => { dec.close(); resolve(); }).catch(() => resolve());
    });
    return out;
}

// Fetch a recording, decode its Opus frames, and play it (gaps preserved).
async function playRecording(id) {
    if (!audioSupported()) return;
    await ensureAudio();
    let buf;
    try { buf = await fetchRecording(id); } catch (e) { console.warn('[dispatch] recording fetch failed', e); return; }
    const rate = audioCtx.sampleRate;
    const out = await decodeRecording(buf, rate);
    if (!out.length) return;
    if (replaySource) { try { replaySource.stop(); } catch (_) {} }
    const ab = audioCtx.createBuffer(1, out.length, rate);
    ab.getChannelData(0).set(out);
    replaySource = audioCtx.createBufferSource();
    replaySource.buffer = ab;
    replaySource.connect(playGain || audioCtx.destination);
    replaySource.start();
}

// ─── Download recordings as a zip ─────────────────────────────────────────────
//
// The selected recordings become WAV files (16 kHz mono — plays anywhere)
// plus transcripts.csv, zipped in the browser.

const histSelected = new Set();

function toggleHistRow(id, on) {
    if (on) histSelected.add(id); else histSelected.delete(id);
    updateHistSelection();
}

function toggleHistAll(on) {
    histSelected.clear();
    if (on) historyRows.forEach(r => histSelected.add(r.id));
    document.querySelectorAll('.hist-check').forEach(c => { c.checked = on; });
    updateHistSelection();
}

function updateHistSelection() {
    const btn = document.getElementById('hist-download');
    const n = histSelected.size;
    if (btn) {
        btn.disabled = n === 0 || histDownloading;
        btn.textContent = histDownloading ? btn.textContent : (n ? `Download (${n})` : 'Download');
    }
    const all = document.getElementById('hist-all');
    if (all) {
        all.checked = n > 0 && n === historyRows.length;
        all.indeterminate = n > 0 && n < historyRows.length;
    }
}

function wavFile(pcm, rate) {
    const b = new ArrayBuffer(44 + pcm.length * 2);
    const v = new DataView(b);
    const str = (o, t) => { for (let i = 0; i < t.length; i++) v.setUint8(o + i, t.charCodeAt(i)); };
    str(0, 'RIFF'); v.setUint32(4, 36 + pcm.length * 2, true); str(8, 'WAVE');
    str(12, 'fmt '); v.setUint32(16, 16, true); v.setUint16(20, 1, true); v.setUint16(22, 1, true);
    v.setUint32(24, rate, true); v.setUint32(28, rate * 2, true); v.setUint16(32, 2, true); v.setUint16(34, 16, true);
    str(36, 'data'); v.setUint32(40, pcm.length * 2, true);
    for (let i = 0; i < pcm.length; i++) {
        const x = Math.max(-1, Math.min(1, pcm[i]));
        v.setInt16(44 + i * 2, x < 0 ? x * 0x8000 : x * 0x7FFF, true);
    }
    return new Uint8Array(b);
}

const CRC_TABLE = (() => {
    const t = new Uint32Array(256);
    for (let n = 0; n < 256; n++) {
        let c = n;
        for (let k = 0; k < 8; k++) c = c & 1 ? 0xEDB88320 ^ (c >>> 1) : c >>> 1;
        t[n] = c >>> 0;
    }
    return t;
})();
function crc32(data) {
    let c = 0xFFFFFFFF;
    for (let i = 0; i < data.length; i++) c = CRC_TABLE[(c ^ data[i]) & 0xFF] ^ (c >>> 8);
    return (c ^ 0xFFFFFFFF) >>> 0;
}

// A plain (stored, uncompressed) zip — what every OS opens natively.
function makeZip(files) {
    const enc = new TextEncoder();
    const parts = [], central = [];
    let offset = 0;
    const now = new Date();
    const dosTime = (now.getHours() << 11) | (now.getMinutes() << 5) | (now.getSeconds() >> 1);
    const dosDate = ((now.getFullYear() - 1980) << 9) | ((now.getMonth() + 1) << 5) | now.getDate();
    for (const f of files) {
        const name = enc.encode(f.name);
        const crc = crc32(f.data);
        const h = new DataView(new ArrayBuffer(30));
        h.setUint32(0, 0x04034b50, true); h.setUint16(4, 20, true); h.setUint16(6, 0x0800, true); h.setUint16(8, 0, true);
        h.setUint16(10, dosTime, true); h.setUint16(12, dosDate, true); h.setUint32(14, crc, true);
        h.setUint32(18, f.data.length, true); h.setUint32(22, f.data.length, true); h.setUint16(26, name.length, true);
        parts.push(new Uint8Array(h.buffer), name, f.data);
        const c = new DataView(new ArrayBuffer(46));
        c.setUint32(0, 0x02014b50, true); c.setUint16(4, 20, true); c.setUint16(6, 20, true); c.setUint16(8, 0x0800, true);
        c.setUint16(12, dosTime, true); c.setUint16(14, dosDate, true); c.setUint32(16, crc, true);
        c.setUint32(20, f.data.length, true); c.setUint32(24, f.data.length, true); c.setUint16(28, name.length, true);
        c.setUint32(42, offset, true);
        central.push(new Uint8Array(c.buffer), name);
        offset += 30 + name.length + f.data.length;
    }
    const cdSize = central.reduce((n, p) => n + p.length, 0);
    const e = new DataView(new ArrayBuffer(22));
    e.setUint32(0, 0x06054b50, true); e.setUint16(8, files.length, true); e.setUint16(10, files.length, true);
    e.setUint32(12, cdSize, true); e.setUint32(16, offset, true);
    return new Blob([...parts, ...central, new Uint8Array(e.buffer)], { type: 'application/zip' });
}

function fileSafe(s) { return String(s).replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 40) || 'x'; }

let histDownloading = false;
async function downloadSelectedRecordings() {
    const rows = historyRows.filter(r => histSelected.has(r.id)).sort((a, b) => a.start - b.start);
    if (!rows.length || histDownloading) return;
    if (typeof AudioDecoder === 'undefined') { alert('This browser cannot decode the recordings. Use a current Chrome, Edge, Firefox or Safari.'); return; }
    const totalSec = rows.reduce((n, r) => n + r.duration_ms, 0) / 1000;
    if (totalSec > 4 * 3600) { alert('That is more than 4 hours of audio. Select fewer recordings and download them in parts.'); return; }
    histDownloading = true;
    const btn = document.getElementById('hist-download');
    const RATE = 16000;
    const files = [];
    const csv = [['time', 'agency', 'channel', 'unit', 'seconds', 'file', 'transcript']];
    const used = new Set();
    let failed = 0;
    try {
        for (let i = 0; i < rows.length; i++) {
            const r = rows[i];
            btn.textContent = `Preparing ${i + 1}/${rows.length}…`;
            const d = new Date(r.start);
            const pad = (n) => String(n).padStart(2, '0');
            const stamp = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}_${pad(d.getHours())}-${pad(d.getMinutes())}-${pad(d.getSeconds())}`;
            let name = `${stamp}_${fileSafe(channelLabel(r.agency, r.channel))}_${fileSafe(r.user)}`;
            while (used.has(name)) name += '_';
            used.add(name);
            try {
                const pcm = await decodeRecording(await fetchRecording(r.id), RATE);
                files.push({ name: name + '.wav', data: wavFile(pcm, RATE) });
            } catch (e) {
                failed++;
                name = '';
            }
            csv.push([d.toISOString(), agencyName(r.agency), channelLabel(r.agency, r.channel), r.user,
                (r.duration_ms / 1000).toFixed(1), name ? name + '.wav' : '(could not be downloaded)', r.text || '']);
        }
        const csvText = '\ufeff' + csv.map(row => row.map(v => `"${String(v).replace(/"/g, '""')}"`).join(',')).join('\r\n');
        files.push({ name: 'transcripts.csv', data: new TextEncoder().encode(csvText) });
        const date = document.getElementById('hist-date').value || new Date().toISOString().slice(0, 10);
        const a = document.createElement('a');
        a.href = URL.createObjectURL(makeZip(files));
        a.download = `recordings-${date}.zip`;
        a.click();
        setTimeout(() => URL.revokeObjectURL(a.href), 5000);
        if (failed) alert(`${failed} recording(s) could not be downloaded; they are listed in transcripts.csv.`);
    } finally {
        histDownloading = false;
        updateHistSelection();
    }
}

// ─── Visualizer ────────────────────────────────────────────────────────────

function clearViz() {
    if (vizAnimId) { cancelAnimationFrame(vizAnimId); vizAnimId = null; }
    const canvas = document.getElementById('audio-visualizer');
    const ctx = canvas.getContext('2d');
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    ctx.strokeStyle = 'rgba(31,111,235,0.3)';
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    ctx.moveTo(0, canvas.height / 2);
    ctx.lineTo(canvas.width, canvas.height / 2);
    ctx.stroke();
}

function drawViz(analyser) {
    if (vizAnimId) { cancelAnimationFrame(vizAnimId); vizAnimId = null; }
    if (!analyser) { clearViz(); return; }
    const canvas = document.getElementById('audio-visualizer');
    const ctx = canvas.getContext('2d');
    const bufLen = analyser.frequencyBinCount;
    const dataArr = new Uint8Array(bufLen);
    const color = isPTT ? '#EF4444' : '#3B82F6';
    function frame() {
        vizAnimId = requestAnimationFrame(frame);
        analyser.getByteTimeDomainData(dataArr);
        ctx.clearRect(0, 0, canvas.width, canvas.height);
        ctx.strokeStyle = color;
        ctx.lineWidth = 2;
        ctx.beginPath();
        const sliceW = canvas.width / bufLen;
        let x = 0;
        for (let i = 0; i < bufLen; i++) {
            const y = (dataArr[i] / 128.0) * canvas.height / 2;
            i === 0 ? ctx.moveTo(x, y) : ctx.lineTo(x, y);
            x += sliceW;
        }
        ctx.stroke();
    }
    frame();
}

// ─── Logout / utilities ────────────────────────────────────────────────────

function logout() {
    fetch('/api/logout', { method: 'POST', credentials: 'same-origin' })
        .finally(() => { window.location.href = '/login.html?next=/command'; });
}

function escHtml(str) {
    return String(str ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}
// For values inside a single-quoted JS string in an HTML attribute.
function escAttr(str) {
    return escHtml(String(str ?? '').replace(/\\/g, '\\\\').replace(/'/g, "\\'"));
}
