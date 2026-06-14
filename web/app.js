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
        return `
            <tr>
                <td><div class="user-avatar" style="background: ${avatarColor};">${initial}</div></td>
                <td>${safeName || '<span style="color:#8B949E;">—</span>'}</td>
                <td><strong>${safeUsername}</strong></td>
                <td style="text-align: center;">${statusDot}</td>
                ${agencyCol}
                <td>
                    <select class="role-select" onchange="updateUserRole('${safeUsername}', this.value, '${safeAgency}')" data-current="${role}">
                        <option value="user" ${role === 'user' ? 'selected' : ''}>User</option>
                        <option value="admin" ${role === 'admin' ? 'selected' : ''}>Admin</option>
                        ${superAdminOpt}
                    </select>
                </td>
                <td>${created}</td>
                <td>
                    <div class="actions-cell">
                        <button class="btn-action qr" onclick="showQRCode('${safeUsername}', '${safeAgency}')">QR Code</button>
                        <button class="btn-action edit" onclick="showEditUser('${safeUsername}', '${safeAgency}')">Edit</button>
                        <button class="btn-action delete" onclick="deleteUser('${safeUsername}', '${safeAgency}')">Delete</button>
                    </div>
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

    // Dashboard system health panel — data-driven from server
    const serviceIds = ['health-web', 'health-db', 'health-relay', 'health-push', 'health-storage'];
    health.services.forEach((svc, i) => {
        const el = document.getElementById(serviceIds[i]);
        if (!el) return;

        el.textContent = svc.status === 'healthy' ? 'Healthy' : svc.status === 'warning' ? 'Warning' : 'Error';
        el.className = 'health-svc-status ' + svc.status;

        const dot = el.parentElement.querySelector('.health-svc-dot');
        if (dot) {
            dot.className = 'health-svc-dot ' + svc.status;
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
    // Show/hide super_admin option based on role
    const superAdminOption = document.querySelector('#newUserRole option[value="super_admin"]');
    if (superAdminOption) {
        superAdminOption.style.display = currentRole === 'super_admin' ? '' : 'none';
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

function fetchMutualAid() {
    // Ensure agencies are loaded for the modal dropdowns
    if (agenciesList.length === 0) {
        fetch('/api/agencies').then(r => r.json()).then(agencies => {
            agenciesList = agencies;
        }).catch(() => {});
    }

    fetch('/api/mutual-aid')
    .then(r => r.json())
    .then(links => {
        renderMutualAidTable(links);
    })
    .catch(err => {
        console.error('Failed to fetch mutual aid links:', err);
    });
}

function renderMutualAidTable(links) {
    const tbody = document.getElementById('mutual-aid-table-body');
    const table = document.getElementById('mutual-aid-table');
    const empty = document.getElementById('mutual-aid-empty');

    if (!links || links.length === 0) {
        table.style.display = 'none';
        empty.style.display = 'block';
        return;
    }

    table.style.display = '';
    empty.style.display = 'none';

    tbody.innerHTML = links.map(link => {
        const created = new Date(link.created_at).toLocaleDateString();
        const statusClass = link.active ? 'status-active' : 'status-inactive';
        const statusText = link.active ? 'Active' : 'Inactive';
        const safeId = link.id.replace(/'/g, "\\'");
        // Build agencies display from arrays
        const agencies = link.agencies || [];
        const agencyNames = link.agency_names || [];
        const agenciesHtml = agencies.map((aid, i) => {
            const name = agencyNames[i] || aid;
            return `${name} <span class="agency-badge">${aid}</span>`;
        }).join(', ');
        return `<tr>
            <td>${agenciesHtml}</td>
            <td>${link.channel_name || 'Channel ' + link.channel}</td>
            <td><span class="link-status ${statusClass}">${statusText}</span></td>
            <td>${created}</td>
            <td class="actions-cell">
                <button class="btn-action delete" onclick="deleteMutualAidLink('${safeId}')">Remove</button>
            </td>
        </tr>`;
    }).join('');
}

function showCreateMutualAidModal() {
    const list = document.getElementById('mutualAidAgencyList');
    list.innerHTML = '<div style="color:#888;padding:8px 0;">Loading agencies...</div>';
    document.getElementById('mutualAidChannelName').value = '';
    document.getElementById('mutualAidError').style.display = 'none';
    showModal('createMutualAidModal');

    // Always fetch fresh agencies list
    fetch('/api/agencies').then(r => r.json()).then(agencies => {
        agenciesList = agencies;
        list.innerHTML = '';
        agencies.forEach(agency => {
            list.innerHTML += `<label><input type="checkbox" value="${agency.id}"> ${agency.name} <span class="agency-badge">${agency.id}</span></label>`;
        });
    }).catch(() => {
        list.innerHTML = '<div style="color:#FF6B6B;padding:8px 0;">Failed to load agencies</div>';
    });
}

function createMutualAidLink() {
    const checkboxes = document.querySelectorAll('#mutualAidAgencyList input[type="checkbox"]:checked');
    const agencies = Array.from(checkboxes).map(cb => cb.value);
    const channelName = document.getElementById('mutualAidChannelName').value.trim();
    const errorEl = document.getElementById('mutualAidError');

    if (agencies.length < 2) {
        errorEl.textContent = 'Select at least two agencies';
        errorEl.style.display = 'block';
        return;
    }

    if (!channelName) {
        errorEl.textContent = 'Channel name is required';
        errorEl.style.display = 'block';
        return;
    }

    errorEl.style.display = 'none';

    fetch('/api/mutual-aid', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agencies: agencies, channel_name: channelName })
    })
    .then(r => r.json())
    .then(data => {
        if (data.error) {
            errorEl.textContent = data.error;
            errorEl.style.display = 'block';
            return;
        }
        closeModal('createMutualAidModal');
        showStatus('mutual-aid-status', 'Mutual aid link created — all clients notified', 'success');
        fetchMutualAid();
    })
    .catch(err => {
        errorEl.textContent = 'Failed to create link: ' + err.message;
        errorEl.style.display = 'block';
    });
}

function deleteMutualAidLink(id) {
    if (!confirm('Remove this mutual aid link? Agencies will no longer be able to communicate on this channel.')) {
        return;
    }

    fetch('/api/mutual-aid/' + id, { method: 'DELETE' })
    .then(r => r.json())
    .then(data => {
        if (data.error) {
            showStatus('mutual-aid-status', data.error, 'error');
            return;
        }
        showStatus('mutual-aid-status', 'Mutual aid link removed', 'success');
        fetchMutualAid();
    })
    .catch(err => {
        showStatus('mutual-aid-status', 'Failed to remove link: ' + err.message, 'error');
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
    const maxChannels = parseInt(document.getElementById('newAgencyMaxChannels').value) || 20;

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
            web_url: latestRelease.web_url || '',
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
