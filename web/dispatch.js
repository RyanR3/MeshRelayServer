// MeshRelay Dispatch Console

let ws = null;
let wsReconnectTimer = null;
let currentChannel = 0;
let currentAgency = '';
let currentUsername = '';
let volumeLevel = 1.0;

// WebRTC mic state
let micStream = null;
let audioCtxOut = null;
let micProcessor = null;
let isPTT = false;

// Audio playback context
let audioCtxIn = null;
let playGain = null;        // persistent gain node for incoming audio
let nextPlayTime = 0;       // scheduling cursor on the audio timeline (seconds)

// Visualizer
let vizCtx = null;
let vizAnimId = null;
let vizAnalyser = null;

// All connected clients (unfiltered)
let allClients = [];

// ─── Startup ───────────────────────────────────────────────────────────────

window.addEventListener('DOMContentLoaded', () => {
    vizCtx = document.getElementById('audio-visualizer').getContext('2d');
    initViz();
    checkSession();
    unlockAudioOnGesture();
});

let audioUnlocked = false;

// Browsers block audio playback until the user interacts with the page.
// We create + resume the playback AudioContext inside a real gesture handler.
// A visible banner guarantees the operator provides that gesture; we also
// listen passively so any click/keypress unlocks audio.
function unlockAudioOnGesture() {
    const unlock = () => {
        try {
            if (!audioCtxIn || audioCtxIn.state === 'closed') {
                audioCtxIn = new AudioContext({ sampleRate: 32000 });
            }
            if (audioCtxIn.state === 'suspended') {
                audioCtxIn.resume().then(markUnlocked).catch(() => {});
            } else {
                markUnlocked();
            }
        } catch (_) {}
    };
    function markUnlocked() {
        if (audioCtxIn && audioCtxIn.state === 'running') {
            audioUnlocked = true;
            const banner = document.getElementById('audio-unlock-banner');
            if (banner) banner.style.display = 'none';
            document.removeEventListener('click', unlock);
            document.removeEventListener('keydown', unlock);
            document.removeEventListener('touchstart', unlock);
            console.log('[dispatch] audio unlocked');
        }
    }
    document.addEventListener('click', unlock);
    document.addEventListener('keydown', unlock);
    document.addEventListener('touchstart', unlock);

    // Show the banner until audio is confirmed unlocked.
    const banner = document.getElementById('audio-unlock-banner');
    if (banner) {
        banner.style.display = 'flex';
        banner.onclick = unlock;
    }
}

async function checkSession() {
    try {
        const r = await fetch('/api/session', { credentials: 'same-origin' });
        const data = await r.json();
        if (!r.ok || !['dispatch', 'agency_admin', 'super_admin'].includes(data.role)) {
            window.location.href = '/login.html';
            return;
        }
        currentAgency = data.agency_id || '';
        currentUsername = data.username || '';
        document.getElementById('dispatch-agency-label').textContent = currentAgency || 'All Agencies';
        document.getElementById('dispatch-user-label').textContent = currentUsername;
        updateChannelDisplay();
        connectDispatchWS();
        requestMic();
    } catch (e) {
        window.location.href = '/login.html';
    }
}

// ─── WebSocket ──────────────────────────────────────────────────────────────

function connectDispatchWS() {
    // Guard against duplicate sockets. If one is already OPEN or still
    // CONNECTING, don't open another — two live sockets both receive the
    // fanned-out audio and play it twice ("doubling up").
    if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return;

    // Fully detach any old socket before replacing it so its handlers can't
    // fire against the new connection.
    if (ws) {
        try { ws.onopen = ws.onclose = ws.onerror = ws.onmessage = null; ws.close(); } catch (_) {}
    }

    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(`${proto}//${location.host}/ws/dispatch`);
    ws.binaryType = 'arraybuffer';

    setWsStatus('connecting');

    ws.onopen = () => {
        console.log('[dispatch] WebSocket OPEN');
        setWsStatus('connected');
        if (wsReconnectTimer) { clearTimeout(wsReconnectTimer); wsReconnectTimer = null; }
        // Sync current channel to server
        sendChannelSwitch(currentChannel);
    };

    ws.onclose = (ev) => {
        console.warn('[dispatch] WebSocket CLOSED code=' + ev.code + ' reason=' + ev.reason + ' wasClean=' + ev.wasClean);
        setWsStatus('error');
        // Only one pending reconnect at a time, so timers can't stack and
        // spawn multiple sockets.
        if (wsReconnectTimer) clearTimeout(wsReconnectTimer);
        wsReconnectTimer = setTimeout(connectDispatchWS, 3000);
    };

    ws.onerror = (ev) => {
        console.error('[dispatch] WebSocket ERROR', ev);
        setWsStatus('error');
    };

    ws.onmessage = (e) => {
        if (e.data instanceof ArrayBuffer) {
            // Incoming PTT audio from field units
            console.log('[dispatch] audio frame ' + e.data.byteLength + ' bytes');
            playIncomingAudio(e.data);
        } else {
            try {
                const msg = JSON.parse(e.data);
                if (msg.type === 'update') {
                    const clients = (msg.data.clients || []).filter(c => !c.username.startsWith('dispatch-'));
                    allClients = clients;
                    renderClientList(clients);
                }
            } catch (_) {}
        }
    };
}

function setWsStatus(state) {
    const el = document.getElementById('dispatch-ws-status');
    el.className = 'ws-status ws-' + state;
    el.textContent = state === 'connected' ? 'Connected' : state === 'connecting' ? 'Connecting...' : 'Disconnected';
}

function sendChannelSwitch(ch) {
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'channel', channel: ch }));
    }
}

// ─── Client List ────────────────────────────────────────────────────────────

function renderClientList(clients) {
    const list = document.getElementById('units-list');
    const countEl = document.getElementById('unit-count');
    const chCountEl = document.getElementById('channel-unit-count');

    // Count units on current channel
    const onChannel = clients.filter(c => c.channel === currentChannel).length;
    chCountEl.textContent = `${onChannel} unit${onChannel !== 1 ? 's' : ''} on this channel`;

    // Apply search + channel filter
    const search = document.getElementById('unit-search').value.toLowerCase();
    const chFilter = document.getElementById('unit-channel-filter').value;
    let filtered = clients;
    if (search) filtered = filtered.filter(c => c.username.toLowerCase().includes(search));
    if (chFilter !== '') filtered = filtered.filter(c => String(c.channel) === chFilter);

    countEl.textContent = `${clients.length} online`;

    // Update channel filter options
    const chSel = document.getElementById('unit-channel-filter');
    const prevChVal = chSel.value;
    const usedChannels = [...new Set(clients.map(c => c.channel))].sort((a, b) => a - b);
    chSel.innerHTML = '<option value="">All Channels</option>';
    usedChannels.forEach(ch => {
        const opt = document.createElement('option');
        opt.value = String(ch);
        opt.textContent = `Channel ${ch}`;
        if (String(ch) === prevChVal) opt.selected = true;
        chSel.appendChild(opt);
    });

    if (filtered.length === 0) {
        list.innerHTML = '<div class="units-empty">No units connected</div>';
        return;
    }

    list.innerHTML = filtered.map(c => {
        const initial = c.username.charAt(0).toUpperCase();
        const color = avatarColor(c.username);
        return `<div class="unit-row">
            <div class="unit-avatar" style="background:${color};">${initial}</div>
            <div class="unit-info">
                <div class="unit-name">${escHtml(c.username)}</div>
                <div class="unit-meta">${escHtml(c.agency_id || '')}</div>
            </div>
            <span class="unit-channel">CH ${c.channel}</span>
        </div>`;
    }).join('');
}

function filterUnits() {
    renderClientList(allClients);
}

// ─── Channel control ────────────────────────────────────────────────────────

function changeChannel(delta) {
    currentChannel = Math.max(0, Math.min(19, currentChannel + delta));
    updateChannelDisplay();
    sendChannelSwitch(currentChannel);
    renderClientList(allClients); // refresh channel count
}

function updateChannelDisplay() {
    document.getElementById('channel-display').textContent = `CH ${currentChannel}`;
    document.getElementById('ptt-ch-hint').textContent = currentChannel;
}

// ─── Microphone + PTT ───────────────────────────────────────────────────────

async function requestMic() {
    const errEl = document.getElementById('mic-error-text');
    const httpBlocked = location.protocol === 'http:' && location.hostname !== 'localhost' && location.hostname !== '127.0.0.1';
    try {
        // On plain HTTP (non-localhost) browsers either block getUserMedia or
        // omit navigator.mediaDevices entirely (Firefox). Guard against both so
        // a missing mic never breaks the rest of the console (incoming audio,
        // client list) which do not need it.
        if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
            throw new Error('mediaDevices unavailable (requires HTTPS)');
        }
        micStream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true } });
        // Mic ready — keep stream alive but don't connect processor yet
        document.getElementById('status-mic-error').style.display = 'none';
    } catch (e) {
        document.getElementById('status-mic-error').style.display = 'flex';
        if (httpBlocked) {
            errEl.innerHTML = 'Mic needs HTTPS — outgoing PTT is disabled over HTTP. Incoming audio still works. Enable HTTPS on the server to transmit.';
        } else {
            errEl.innerHTML = 'Mic access denied — <a href="#" onclick="requestMic()">Retry</a>';
        }
        console.warn('Mic unavailable:', e);
    }
}

function startPTT(e) {
    if (e) e.preventDefault();
    if (isPTT) return;
    // Mic is unavailable over plain HTTP. Don't keep re-requesting it on every
    // press — that disrupts the page. Just show the hint and bail.
    if (!micStream) {
        document.getElementById('status-mic-error').style.display = 'flex';
        return;
    }
    if (!ws || ws.readyState !== WebSocket.OPEN) return;

    isPTT = true;
    document.getElementById('ptt-btn').classList.add('active');
    document.getElementById('status-ready').style.display = 'none';
    document.getElementById('status-transmitting').style.display = 'flex';

    try {
        audioCtxOut = new AudioContext({ sampleRate: 32000 }); // match Android's 32kHz sample rate
        const src = audioCtxOut.createMediaStreamSource(micStream);

        // Analyser for visualizer
        vizAnalyser = audioCtxOut.createAnalyser();
        vizAnalyser.fftSize = 256;
        src.connect(vizAnalyser);

        micProcessor = audioCtxOut.createScriptProcessor(1024, 1, 1);
        micProcessor.onaudioprocess = (ev) => {
            if (!isPTT || !ws || ws.readyState !== WebSocket.OPEN) return;
            const float32 = ev.inputBuffer.getChannelData(0);
            ws.send(float32ToInt16(float32).buffer);
        };
        src.connect(micProcessor);
        micProcessor.connect(audioCtxOut.destination);
        drawViz();
    } catch (err) {
        console.error('PTT start error:', err);
        stopPTT();
    }
}

function stopPTT() {
    if (!isPTT) return;
    isPTT = false;
    document.getElementById('ptt-btn').classList.remove('active');
    document.getElementById('status-ready').style.display = 'flex';
    document.getElementById('status-transmitting').style.display = 'none';

    try {
        if (micProcessor) { micProcessor.disconnect(); micProcessor = null; }
        if (audioCtxOut)  { audioCtxOut.close(); audioCtxOut = null; }
        vizAnalyser = null;
    } catch (_) {}
    clearViz();
}

// ─── Incoming audio playback ─────────────────────────────────────────────────

function playIncomingAudio(data) {
    showReceiving();

    // data = [0x00 type][username_len int16 BE][username bytes][audio_len int32 BE][pcm int16 LE bytes]
    const view = new DataView(data);
    let offset = 1; // skip packet type byte
    if (data.byteLength < 3) return;
    const usernameLen = view.getInt16(offset, false); offset += 2;
    if (usernameLen < 0 || offset + usernameLen > data.byteLength) return;
    const usernameBytes = new Uint8Array(data, offset, usernameLen);
    const senderName = new TextDecoder().decode(usernameBytes);
    offset += usernameLen;
    if (offset + 4 > data.byteLength) return;
    const audioLen = view.getInt32(offset, false); offset += 4;
    if (audioLen <= 0 || offset + audioLen > data.byteLength) return;

    // Copy audio bytes to an aligned buffer (offset may not be 2-byte aligned)
    const rawBytes = new Uint8Array(data, offset, audioLen);
    const aligned = new ArrayBuffer(audioLen);
    new Uint8Array(aligned).set(rawBytes);
    const pcmSamples = new Int16Array(aligned);

    const float32 = new Float32Array(pcmSamples.length);
    for (let i = 0; i < pcmSamples.length; i++) float32[i] = pcmSamples[i] / 32768.0;

    document.getElementById('receiving-from').textContent = senderName;

    schedulePCM(float32);
}

// Schedule each 20ms frame back-to-back on the audio timeline so frames play
// seamlessly. The cursor (nextPlayTime) only ever moves FORWARD; it is only
// re-seeded to "now + lead" when it has already fallen at/behind now (i.e. all
// previously scheduled buffers have finished). This guarantees buffers never
// overlap — overlap was causing the "doubling" / worsening artifacts.
function schedulePCM(float32) {
    try {
        if (!audioCtxIn || audioCtxIn.state === 'closed') {
            audioCtxIn = new AudioContext({ sampleRate: 32000 });
            playGain = null;
            nextPlayTime = 0;
        }
        // The browser can auto-suspend an AudioContext mid-stream (tab
        // backgrounded, power saving). currentTime then freezes and playback
        // dies. Resume it; do NOT touch nextPlayTime here (resetting it while
        // buffers are still scheduled is what produced overlap/doubling).
        if (audioCtxIn.state !== 'running') {
            audioCtxIn.resume().catch(() => {});
            if (audioCtxIn.state === 'suspended' && !audioUnlocked) {
                const banner = document.getElementById('audio-unlock-banner');
                if (banner) banner.style.display = 'flex';
            }
            return; // skip this frame; resume is async, next frame will schedule
        }

        if (!playGain) {
            playGain = audioCtxIn.createGain();
            playGain.gain.value = volumeLevel;
            playGain.connect(audioCtxIn.destination);
        }

        const now = audioCtxIn.currentTime;
        const frameDur = float32.length / 32000;
        const LEAD = 0.10;      // target ~100ms jitter buffer ahead of now
        const MAX_LEAD = 0.50;  // if buffered audio exceeds this, we're backlogged

        // Re-seed only when the cursor has actually drained to/behind realtime.
        // Because we only move it forward otherwise, scheduled buffers can never
        // overlap each other.
        if (nextPlayTime < now + 0.01) {
            nextPlayTime = now + LEAD;
        } else if (nextPlayTime - now > MAX_LEAD) {
            // Sender outpacing playback — drop this frame to bound latency and
            // the pending-node count. Cursor stays put; realtime catches up.
            return;
        }

        const buf = audioCtxIn.createBuffer(1, float32.length, 32000);
        buf.copyToChannel(float32, 0);
        const srcNode = audioCtxIn.createBufferSource();
        srcNode.buffer = buf;
        srcNode.connect(playGain);
        srcNode.start(nextPlayTime);
        nextPlayTime += frameDur;
    } catch (e) {
        console.warn('[dispatch] playback error', e);
    }
}

let receivingTimer = null;
function showReceiving() {
    document.getElementById('status-receiving').style.display = 'flex';
    if (receivingTimer) clearTimeout(receivingTimer);
    receivingTimer = setTimeout(() => {
        document.getElementById('status-receiving').style.display = 'none';
        document.getElementById('receiving-from').textContent = '';
    }, 1500);
}

// ─── Volume ─────────────────────────────────────────────────────────────────

function setVolume(val) {
    volumeLevel = parseFloat(val);
    document.getElementById('volume-display').textContent = Math.round(volumeLevel * 100) + '%';
    if (playGain) playGain.gain.value = volumeLevel;
}

// ─── Visualizer ─────────────────────────────────────────────────────────────

function initViz() {
    clearViz();
}

function clearViz() {
    if (vizAnimId) { cancelAnimationFrame(vizAnimId); vizAnimId = null; }
    const canvas = document.getElementById('audio-visualizer');
    const ctx = canvas.getContext('2d');
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    // Draw flat baseline
    ctx.strokeStyle = 'rgba(124,58,237,0.25)';
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    ctx.moveTo(0, canvas.height / 2);
    ctx.lineTo(canvas.width, canvas.height / 2);
    ctx.stroke();
}

function drawViz() {
    if (!vizAnalyser) { clearViz(); return; }
    const canvas = document.getElementById('audio-visualizer');
    const ctx = canvas.getContext('2d');
    const bufLen = vizAnalyser.frequencyBinCount;
    const dataArr = new Uint8Array(bufLen);

    function frame() {
        if (!vizAnalyser) { clearViz(); return; }
        vizAnimId = requestAnimationFrame(frame);
        vizAnalyser.getByteTimeDomainData(dataArr);
        ctx.clearRect(0, 0, canvas.width, canvas.height);
        ctx.strokeStyle = '#7C3AED';
        ctx.lineWidth = 2;
        ctx.beginPath();
        const sliceW = canvas.width / bufLen;
        let x = 0;
        for (let i = 0; i < bufLen; i++) {
            const v = dataArr[i] / 128.0;
            const y = (v * canvas.height) / 2;
            i === 0 ? ctx.moveTo(x, y) : ctx.lineTo(x, y);
            x += sliceW;
        }
        ctx.lineTo(canvas.width, canvas.height / 2);
        ctx.stroke();
    }
    frame();
}

// ─── Logout ─────────────────────────────────────────────────────────────────

function logout() {
    fetch('/api/logout', { method: 'POST', credentials: 'same-origin' })
    .finally(() => { window.location.href = '/login.html'; });
}

// ─── Utilities ───────────────────────────────────────────────────────────────

function float32ToInt16(float32) {
    const int16 = new Int16Array(float32.length);
    for (let i = 0; i < float32.length; i++) {
        const s = Math.max(-1, Math.min(1, float32[i]));
        int16[i] = s < 0 ? s * 0x8000 : s * 0x7FFF;
    }
    return int16;
}

function escHtml(str) {
    return String(str).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

const AVATAR_COLORS = [
    '#7C3AED','#2563EB','#059669','#D97706','#DC2626',
    '#7C3AED','#0891B2','#BE185D','#065F46','#92400E',
];
function avatarColor(username) {
    let hash = 0;
    for (const c of username) hash = (hash * 31 + c.charCodeAt(0)) | 0;
    return AVATAR_COLORS[Math.abs(hash) % AVATAR_COLORS.length];
}
