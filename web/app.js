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

function navigateTo(pageName) {
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
        fetchMutualAid();
    } else if (pageName === 'guest-events') {
        fetchGuestEvents();
    } else if (pageName === 'zello-bridges') {
        // Load agencies for the per-mapping agency dropdowns, then bridges.
        fetch('/api/agencies').then(r => r.json()).then(a => { agenciesList = a || []; }).catch(() => {});
        fetchZelloBridges();
    } else if (pageName === 'lmr-bridges') {
        fetchLmrBridges();
    } else if (pageName === 'activity') {
        fetchActivityPage();
    } else if (pageName === 'settings') {
        fetchAuthStatus();
        fetchVersionInfo();
        if (currentRole === 'super_admin') fetchGitHubTokenStatus();
    }
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
                <div class="channel-name">Channel ${channel.id}${agencyLabel}</div>
                <div class="channel-count">${channel.client_count} ${channel.client_count === 1 ? 'client' : 'clients'}</div>
            </div>
        `;
    }).join('');
}

// Update clients list
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
                    <span class="client-channel">Ch ${client.channel}</span>
                </div>
                <div class="client-info">
                    ${client.address} • Connected ${connectedTime}
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

function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

// Avatar color based on username hash
const avatarColors = ['#7C3AED', '#3B82F6', '#10B981', '#F59E0B', '#EC4899', '#06B6D4'];

function getAvatarColor(username) {
    let hash = 0;
    for (let i = 0; i < username.length; i++) {
        hash = username.charCodeAt(i) + ((hash << 5) - hash);
    }
    return avatarColors[Math.abs(hash) % avatarColors.length];
}

function isUserOnline(username) {
    return connectedClients.some(c => c.username === username);
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

function fetchAuthStatus() {
    fetch('/api/auth/status')
        .then(r => r.json())
        .then(data => {
            document.getElementById('auth-toggle').checked = data.auth_enabled;
        })
        .catch(err => console.error('Error fetching auth status:', err));
}

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
    if (!query) return allUsers;
    return allUsers.filter(u => u.username.toLowerCase().includes(query) || (u.name || '').toLowerCase().includes(query) || (u.agency_id || '').toLowerCase().includes(query));
}

function renderUsersPage() {
    const filtered = getFilteredUsers();
    const total = filtered.length;
    const totalPages = Math.max(1, Math.ceil(total / USERS_PER_PAGE));

    if (currentUsersPage > totalPages) currentUsersPage = totalPages;

    const start = (currentUsersPage - 1) * USERS_PER_PAGE;
    const pageUsers = filtered.slice(start, start + USERS_PER_PAGE);

    renderUsersTable(pageUsers);
    renderPagination(total, totalPages);
    updateUsersPageStats();
}

function renderUsersTable(users) {
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
        tbody.innerHTML = '<tr><td colspan="8" style="text-align: center; color: #8B949E; padding: 30px;">No matching users</td></tr>';
        return;
    }

    tbody.innerHTML = users.map(user => {
        const created = user.created_at ? new Date(user.created_at).toLocaleString() : 'Unknown';
        const safeName = escapeHtml(user.name || '');
        const safeUsername = escapeHtml(user.username);
        const safeAgency = escapeHtml(user.agency_id || 'default');
        const agencyCol = currentRole === 'super_admin' ? `<td><span class="agency-badge">${safeAgency}</span></td>` : '';
        const role = user.role || 'user';
        const superAdminOpt = currentRole === 'super_admin' ? `<option value="super_admin" ${role === 'super_admin' ? 'selected' : ''}>Super Admin</option>` : '';
        const initial = (user.name || user.username).charAt(0).toUpperCase();
        const avatarColor = getAvatarColor(user.username);
        const online = isUserOnline(user.username);
        const statusDot = online ? '<span class="status-dot-online"></span>' : '<span class="status-dot-offline"></span>';
        // Dispatch users are web-only — show badge instead of editable role dropdown; no QR/Edit
        const isDispatch = role === 'dispatch';
        const roleCell = isDispatch
            ? `<span class="link-status" style="background:rgba(99,102,241,0.15);color:#818CF8;border-color:rgba(99,102,241,0.3);">Dispatch</span>`
            : `<select class="role-select" onchange="updateUserRole('${safeUsername}', this.value, '${safeAgency}')" data-current="${role}">
                    <option value="user" ${role === 'user' ? 'selected' : ''}>User</option>
                    <option value="admin" ${role === 'admin' ? 'selected' : ''}>Admin</option>
                    ${superAdminOpt}
               </select>`;
        const actionButtons = isDispatch
            ? `<button class="btn-action delete" onclick="deleteUser('${safeUsername}', '${safeAgency}')">Delete</button>`
            : `<button class="btn-action qr" onclick="showQRCode('${safeUsername}', '${safeAgency}')">QR Code</button>
               <button class="btn-action edit" onclick="showEditUser('${safeUsername}', '${safeAgency}')">Edit</button>
               <button class="btn-action delete" onclick="deleteUser('${safeUsername}', '${safeAgency}')">Delete</button>`;
        return `
            <tr>
                <td><div class="user-avatar" style="background: ${avatarColor};">${initial}</div></td>
                <td>${safeName || '<span style="color:#8B949E;">—</span>'}</td>
                <td><strong>${safeUsername}</strong></td>
                <td style="text-align: center;">${statusDot}</td>
                ${agencyCol}
                <td>${roleCell}</td>
                <td>${created}</td>
                <td>
                    <div class="actions-cell">${actionButtons}</div>
                </td>
            </tr>
        `;
    }).join('');
}

function renderPagination(total, totalPages) {
    const pagination = document.getElementById('users-pagination');
    const info = document.getElementById('pagination-info');
    const controls = document.getElementById('pagination-controls');

    if (total <= USERS_PER_PAGE) {
        pagination.style.display = 'none';
        return;
    }

    pagination.style.display = 'flex';

    const start = (currentUsersPage - 1) * USERS_PER_PAGE + 1;
    const end = Math.min(currentUsersPage * USERS_PER_PAGE, total);
    info.textContent = `Showing ${start}-${end} of ${total} users`;

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
    const filtered = getFilteredUsers();
    const totalPages = Math.max(1, Math.ceil(filtered.length / USERS_PER_PAGE));
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
    const online = allUsers.filter(u => isUserOnline(u.username)).length;
    const offline = total - online;

    // Count users created in last 7 days
    const sevenDaysAgo = new Date();
    sevenDaysAgo.setDate(sevenDaysAgo.getDate() - 7);
    const newUsers = allUsers.filter(u => u.created_at && new Date(u.created_at) > sevenDaysAgo).length;

    totalEl.textContent = total;
    onlineEl.textContent = online;
    offlineEl.textContent = offline;
    newEl.textContent = newUsers;
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
                    borderColor: '#A78BFA',
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
                backgroundColor: 'rgba(124, 58, 237, 0.6)',
                borderColor: '#7C3AED',
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

function updateUserRole(username, newRole, agency) {
    const agencyParam = agency ? `?agency=${encodeURIComponent(agency)}` : '';
    fetch(`/api/users/${encodeURIComponent(username)}/role${agencyParam}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ role: newRole })
    })
    .then(r => r.json().then(data => ({ ok: r.ok, data })))
    .then(({ ok, data }) => {
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
            expiryEl.textContent = 'Enrollment key expired — generate a new QR code';
            clearInterval(qrExpiryInterval);
            qrExpiryInterval = null;
            return;
        }
        const mins = Math.floor(remaining / 60);
        const secs = remaining % 60;
        const timeStr = `${mins}:${secs.toString().padStart(2, '0')}`;

        if (remaining <= 120) {
            expiryEl.className = 'qr-expiry expiring';
            expiryEl.textContent = `Enrollment key expires in ${timeStr}`;
        } else {
            expiryEl.className = 'qr-expiry active';
            expiryEl.textContent = `Enrollment key expires in ${timeStr}`;
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
                    <button class="ma-patch-remove" onclick="deleteMutualAidLink('${safeId}')" title="Remove patch">&times;</button>
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
        ` <span class="ma-chip-x" onclick="removeFromPatch('${builderId}','${escapeAttr(agencyId)}',this)">&times;</span>`;
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
        const card = document.querySelector(`[data-zbid="${b.id}"]`);
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
                    <button class="btn-secondary zb-addmap" onclick="addMappingRow('${safeId}')">+ Add channel</button>
                    <button class="btn-primary zb-apply" onclick="applyZelloMapping('${safeId}')">Apply</button>
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
                <button class="btn-secondary zb-toggle" onclick="toggleZelloBridge('${safeId}')">${b.enabled ? 'Disable' : 'Enable'}</button>
                <button class="btn-action" onclick='editZelloBridge(${JSON.stringify(b).replace(/'/g,"&#39;")})'>Edit</button>
                <button class="btn-action delete" onclick="deleteZelloBridge('${safeId}')">Delete</button>
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

// MeshRelay channel <option>s. High slots (10-19) so a bridged channel is a NEW
// named channel on clients (like mutual aid) and doesn't collide with 0-9.
function meshChannelOptions(sel) {
    let o = '';
    for (let i = 10; i < 20; i++) {
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
    const method = id ? 'PUT' : 'POST';
    const path = id ? '/api/zello-bridges/' + id : '/api/zello-bridges';
    fetch(path, { method, headers: {'Content-Type':'application/json'}, body: JSON.stringify(body) })
    .then(r => r.json())
    .then(data => {
        if (data.error) { errorEl.textContent = data.error; errorEl.style.display = 'block'; return; }
        closeModal('zelloBridgeModal');
        showStatus('zello-bridges-status', id ? 'Bridge updated' : 'Bridge created', 'success');
        fetchZelloBridges();
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
        const safeId = ev.id.replace(/'/g, "\\'");
        const safeName = ev.name.replace(/'/g, "\\'");
        const safeUsername = ev.username.replace(/'/g, "\\'");
        const safeToken = ev.token.replace(/'/g, "\\'");
        const safeAgency = (ev.agency_id || '').replace(/'/g, "\\'");
        const expiresText = ev.expires_at ? new Date(ev.expires_at).toLocaleString() : 'Never';
        const connected = ev.connected || 0;
        return `<tr>
            <td><strong>${ev.name}</strong></td>
            <td>${ev.agency_name || ev.agency_id} <span class="agency-badge">${ev.agency_id}</span></td>
            <td>Ch ${ev.channel}</td>
            <td><span class="${connected > 0 ? 'status-active' : ''}">${connected} online</span></td>
            <td>${expiresText}</td>
            <td>${created}</td>
            <td class="actions-cell">
                <button class="btn-action qr" onclick="showGuestEventQR('${safeId}', '${safeName}', '${safeUsername}', '${safeToken}', ${JSON.stringify(ev.expires_at || null)}, '${safeAgency}')">QR Code</button>
                <button class="btn-action delete" onclick="deleteGuestEvent('${safeId}', '${safeName}')">End Event</button>
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
        fetch('/api/agencies').then(r => r.json()).then(agencies => {
            agenciesList = agencies || [];
            // Exclude auto-created event agencies from the list.
            const selectable = agenciesList.filter(a => !String(a.id).startsWith('event-'));
            if (!selectable.length) {
                list.innerHTML = '<div style="color:#6E7681;padding:8px 0;font-size:13px;">No agencies to include.</div>';
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

    fetch('/api/guest-events/' + id, { method: 'DELETE' })
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

// --- Auth Toggle ---

function toggleAuth() {
    const toggle = document.getElementById('auth-toggle');
    const action = toggle.checked ? 'enable' : 'disable';
    if (!confirm(`Are you sure you want to ${action} authentication? This affects all client connections.`)) {
        toggle.checked = !toggle.checked; // revert the toggle
        return;
    }

    fetch('/api/auth/toggle', { method: 'POST' })
    .then(r => r.json())
    .then(data => {
        const status = data.auth_enabled ? 'enabled' : 'disabled';
        addLog(`Authentication ${status}`, 'info');
    })
    .catch(err => {
        addLog(`Failed to toggle auth: ${err.message}`, 'error');
        fetchAuthStatus();
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
            currentRole = data.role || 'super_admin';
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
        return `
            <tr>
                <td><strong>${safeName}</strong></td>
                <td><code>${safeId}</code></td>
                <td>${agency.user_count || 0}</td>
                <td>
                    <div class="actions-cell">
                        ${!isDefault ? `<button class="btn-action delete" onclick="deleteAgency('${safeId}')">Delete</button>` : ''}
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

            if (data.update_available) {
                latestRelease = data;
                if (resultEl) {
                    resultEl.className = 'status-message warning';
                    resultEl.innerHTML = `Update available: <strong>${data.latest_version}</strong> (current: ${data.current_version})`;
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
    // Reset confirm section
    const confirmSection = document.getElementById('update-confirm-section');
    if (confirmSection) {
        confirmSection.style.display = 'none';
        document.getElementById('update-confirm-password').value = '';
    }

    showModal('updateModal');
}

function applyUpdate() {
    if (!latestRelease) return;

    const confirmSection = document.getElementById('update-confirm-section');
    const passwordInput = document.getElementById('update-confirm-password');

    // First click: show the confirm section with password field
    if (confirmSection && confirmSection.style.display === 'none') {
        confirmSection.style.display = 'block';
        passwordInput.focus();
        document.getElementById('update-apply-btn').textContent = 'Confirm & Update';
        return;
    }

    // Second click: validate password is entered
    if (!passwordInput || !passwordInput.value) {
        showUpdateError('Please re-enter your password to confirm the update.');
        return;
    }

    const applyBtn = document.getElementById('update-apply-btn');
    applyBtn.disabled = true;
    applyBtn.textContent = 'Updating...';
    if (confirmSection) confirmSection.style.display = 'none';

    document.getElementById('update-progress').style.display = 'block';
    document.getElementById('update-error').style.display = 'none';

    fetch('/api/update/apply', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            binary_url: latestRelease.binary_url,
            binary_name: latestRelease.binary_name || '',
            web_url: latestRelease.web_url || '',
            checksum_url: latestRelease.checksum_url || '',
            password: passwordInput.value
        })
    })
    .then(r => r.json())
    .then(data => {
        // Clear the password from the input immediately
        passwordInput.value = '';
        if (data.error) {
            showUpdateError(data.error);
            return;
        }
        // Start polling for progress
        startUpdatePolling();
    })
    .catch(err => {
        passwordInput.value = '';
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

// --- GitHub Token ---

function fetchGitHubTokenStatus() {
    fetch('/api/github-token')
        .then(r => r.json())
        .then(data => {
            const el = document.getElementById('gh-token-status');
            if (el) {
                el.textContent = data.configured ? '(Token configured)' : '(No token set)';
                el.style.color = data.configured ? '#4CAF50' : '#B0B0B0';
            }
        })
        .catch(() => {});
}

function saveGitHubToken() {
    const token = document.getElementById('github-token-input').value.trim();
    if (!token) return;

    fetch('/api/github-token', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token })
    })
    .then(r => r.json())
    .then(data => {
        document.getElementById('github-token-input').value = '';
        fetchGitHubTokenStatus();
        addLog('GitHub token saved', 'success');
    })
    .catch(err => addLog('Failed to save token: ' + err.message, 'error'));
}

function clearGitHubToken() {
    fetch('/api/github-token', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: '' })
    })
    .then(r => r.json())
    .then(() => {
        fetchGitHubTokenStatus();
        addLog('GitHub token cleared', 'info');
    })
    .catch(err => addLog('Failed to clear token: ' + err.message, 'error'));
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
        fetchAuthStatus();
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
                    onclick="lmrRemoveAgency('${safeId}','${escapeAttr(aid)}')"
                    title="Remove agency">&times;</button></span>`).join('');
            const dot = b.online ? 'lmr-dot-on' : 'lmr-dot-off';
            const state = b.enabled ? 'Enabled' : 'Disabled';
            return `<div class="ma-patch-card ma-patch-existing" id="lmr-card-${safeId}">
                <div class="ma-patch-card-head">
                    <span class="ma-patch-title">
                        <span class="lmr-dot ${dot}" title="${b.online ? 'Gateway connected' : 'Gateway offline'}"></span>
                        ${escapeHtml(b.channel_name || b.name || 'LMR Bridge')}
                    </span>
                    <button class="ma-patch-remove" onclick="deleteLmrBridge('${safeId}')" title="Remove bridge">&times;</button>
                </div>
                <div class="ma-dropzone lmr-edit-zone"
                     ondragover="maDragOver(event)" ondragleave="maDragLeave(event)"
                     ondrop="lmrDropExisting(event,'${safeId}')">
                    <div class="ma-patch-chips">${chips}</div>
                    <div class="ma-dropzone-hint lmr-edit-hint">Drop an agency here to add it</div>
                </div>
                <div class="ma-patch-meta">
                    Channel ${b.channel} &middot; gateway login <code>${escapeHtml(b.username || '')}</code>
                    &middot; ${(b.agencies || []).length} agencies
                    &middot; <a href="#" onclick="toggleLmrBridge('${safeId}');return false;">${state}</a>
                </div>
                <div class="lmr-actions">
                    <button class="btn-secondary lmr-qr-btn"
                            onclick="showLmrBridgeQr('${escapeAttr(b.username || '')}','${escapeAttr(b.agencies && b.agencies[0] || '')}')">
                        Enrollment QR
                    </button>
                </div>
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
                class="ma-chip-x" onclick="lmrUndrop('${builderId}','${escapeAttr(agencyId)}')">&times;</button></span>`);
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
