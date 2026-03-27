// WebSocket connection
let ws = null;
let reconnectInterval = null;

// Currently editing user
let editingUsername = '';

// Current session info
let currentRole = '';
let currentAgencyId = '';
let agenciesList = []; // cached for dropdowns

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

        // Attempt to reconnect every 3 seconds
        if (!reconnectInterval) {
            reconnectInterval = setInterval(connectWebSocket, 3000);
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
    updateStats(data.stats);
    updateChannels(data.channels);
    updateClients(data.clients);
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

    if (!channels || channels.length === 0) {
        channelList.innerHTML = '<div class="empty-state"><div class="empty-state-icon">&#x1F4E1;</div><div>No active channels</div></div>';
        return;
    }

    // Sort channels by ID
    channels.sort((a, b) => a.id - b.id);

    // Only show channels with clients
    const activeChannels = channels.filter(ch => ch.client_count > 0);

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

// --- Modal Management ---

function showModal(id) {
    document.getElementById(id).classList.add('active');
}

function closeModal(id) {
    document.getElementById(id).classList.remove('active');
}

// Close modal on backdrop click
document.addEventListener('click', (e) => {
    if (e.target.classList.contains('modal')) {
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
        .then(users => renderUsersTable(users))
        .catch(err => console.error('Error fetching users:', err));
}

function renderUsersTable(users) {
    const tbody = document.getElementById('users-table-body');
    const emptyState = document.getElementById('users-empty');
    const table = document.querySelector('#page-users .data-table');

    if (!users || users.length === 0) {
        tbody.innerHTML = '';
        if (table) table.style.display = 'none';
        emptyState.style.display = 'block';
        return;
    }

    if (table) table.style.display = 'table';
    emptyState.style.display = 'none';

    tbody.innerHTML = users.map(user => {
        const created = user.created_at ? new Date(user.created_at).toLocaleString() : 'Unknown';
        const safeUsername = escapeHtml(user.username);
        const agencyCol = currentRole === 'super_admin' ? `<td><span class="agency-badge">${escapeHtml(user.agency_id || 'default')}</span></td>` : '';
        const role = user.role || 'user';
        const roleLabel = role === 'super_admin' ? 'Super Admin' : role === 'admin' ? 'Admin' : 'User';
        const roleClass = role === 'super_admin' ? 'role-super-admin' : role === 'admin' ? 'role-admin' : 'role-user';
        return `
            <tr>
                <td><strong>${safeUsername}</strong></td>
                ${agencyCol}
                <td><span class="role-badge ${roleClass}">${roleLabel}</span></td>
                <td>${created}</td>
                <td>
                    <div class="actions-cell">
                        <button class="btn-action qr" onclick="showQRCode('${safeUsername}')">QR Code</button>
                        <button class="btn-action edit" onclick="showEditPassword('${safeUsername}')">Edit</button>
                        <button class="btn-action delete" onclick="deleteUser('${safeUsername}')">Delete</button>
                    </div>
                </td>
            </tr>
        `;
    }).join('');
}

// --- Create User ---

function showCreateUserModal() {
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
    document.getElementById('newUsername').focus();
}

function createUser() {
    const usernameEl = document.getElementById('newUsername');
    const passwordEl = document.getElementById('newPassword');
    const username = usernameEl.value.trim();
    const password = passwordEl.value;

    if (!username || !password) {
        showStatus('user-status', 'Username and password are required', 'warning');
        return;
    }

    const body = { username, password };
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

        // Show QR code for the new user
        showQRCodeWithData(username, password);
    })
    .catch(err => {
        showStatus('user-status', `Failed to create user: ${err.message}`, 'error');
        addLog(`Failed to create user: ${err.message}`, 'error');
    });
}

// --- Edit Password ---

function showEditPassword(username) {
    editingUsername = username;
    document.getElementById('editPasswordUser').textContent = username;
    document.getElementById('editNewPassword').value = '';
    showModal('editPasswordModal');
    document.getElementById('editNewPassword').focus();
}

function updatePassword() {
    const password = document.getElementById('editNewPassword').value;

    if (!password) {
        showStatus('user-status', 'Password cannot be empty', 'warning');
        return;
    }

    fetch(`/api/users/${encodeURIComponent(editingUsername)}/password`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password })
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

        closeModal('editPasswordModal');
        showStatus('user-status', `Password updated for "${editingUsername}"`, 'success');
        addLog(`Password updated for "${editingUsername}"`, 'success');
    })
    .catch(err => {
        showStatus('user-status', `Failed to update password: ${err.message}`, 'error');
        addLog(`Failed to update password: ${err.message}`, 'error');
    });
}

// --- Delete User ---

function deleteUser(username) {
    if (!confirm(`Delete user "${username}"? This cannot be undone.`)) return;

    fetch(`/api/users/${encodeURIComponent(username)}`, { method: 'DELETE' })
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

// --- QR Code ---

function showQRCode(username) {
    // Show QR with server address and username (no password since we don't store plaintext)
    const serverUrl = window.location.hostname + ':5000';
    document.getElementById('qrCodeTitle').textContent = `QR Code for ${username}`;
    document.getElementById('qrServer').textContent = serverUrl;
    document.getElementById('qrUsername').textContent = username;
    document.getElementById('qrPassword').textContent = '(set in app)';

    const container = document.getElementById('qrCodeCanvas');
    container.innerHTML = '';

    const qrData = JSON.stringify({
        server: serverUrl,
        username: username
    });

    new QRCode(container, {
        text: qrData,
        width: 200,
        height: 200,
        colorDark: '#000000',
        colorLight: '#ffffff',
        correctLevel: QRCode.CorrectLevel.H
    });

    showModal('qrCodeModal');
}

function showQRCodeWithData(username, password) {
    const serverUrl = window.location.hostname + ':5000';
    document.getElementById('qrCodeTitle').textContent = `QR Code for ${username}`;
    document.getElementById('qrServer').textContent = serverUrl;
    document.getElementById('qrUsername').textContent = username;
    document.getElementById('qrPassword').textContent = password;

    const container = document.getElementById('qrCodeCanvas');
    container.innerHTML = '';

    const qrData = JSON.stringify({
        server: serverUrl,
        username: username,
        password: password
    });

    new QRCode(container, {
        text: qrData,
        width: 200,
        height: 200,
        colorDark: '#000000',
        colorLight: '#ffffff',
        correctLevel: QRCode.CorrectLevel.H
    });

    showModal('qrCodeModal');
}

// --- Auth Toggle ---

function toggleAuth() {
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

    if (!newPass || newPass.length < 4) {
        errorEl.textContent = 'Password must be at least 4 characters';
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
        document.getElementById('changeAdminPasswordModal').style.display = 'none';
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
    document.getElementById('update-close-btn').textContent = 'Cancel';

    showModal('updateModal');
}

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
            web_url: latestRelease.web_url || ''
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

// Wrap fetch to handle 401 globally
const originalFetch = window.fetch;
window.fetch = function(url, options) {
    return originalFetch(url, options).then(response => {
        if (response.status === 401 && !url.toString().includes('/api/session') && !url.toString().includes('/api/login')) {
            window.location.href = '/login.html';
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
            if (e.key === 'Enter') updatePassword();
        });
        document.getElementById('adminConfirmPassword').addEventListener('keydown', (e) => {
            if (e.key === 'Enter') changeAdminPassword();
        });
    });
});
