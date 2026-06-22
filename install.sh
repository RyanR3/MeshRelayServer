#!/bin/bash
set -e

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

INSTALL_DIR="$(cd "$(dirname "$0")" && pwd)"
SERVICE_USER="ptt"
SERVICE_NAME="ptt-server"

echo ""
echo -e "${GREEN}=== MeshRelay Server Installer ===${NC}"
echo ""

# Must be root
if [ "$EUID" -ne 0 ]; then
    echo -e "${RED}Please run as root: sudo ./install.sh${NC}"
    exit 1
fi

# Check OS
if ! grep -qiE 'ubuntu|debian' /etc/os-release 2>/dev/null; then
    echo -e "${YELLOW}Warning: This script is designed for Ubuntu/Debian${NC}"
    echo ""
fi

# Step 1: Install Go
echo -e "${YELLOW}[1/7] Checking Go...${NC}"
if ! command -v go &>/dev/null; then
    echo -e "  Installing Go..."
    apt-get update -qq
    apt-get install -y -qq golang-go
    echo -e "  ${GREEN}Installed $(go version)${NC}"
else
    echo -e "  $(go version)"
fi

# Step 2: Build binary
echo -e "${YELLOW}[2/7] Building server...${NC}"
VERSION=$(git -C "$INSTALL_DIR" describe --tags --always 2>/dev/null || echo "dev")
cd "$INSTALL_DIR"

# Build as the non-root user so Go cache doesn't get owned by root
BUILD_USER="${SUDO_USER:-$USER}"
sudo -u "$BUILD_USER" go mod download 2>&1 | tail -1 || true
sudo -u "$BUILD_USER" go build -ldflags "-X main.Version=${VERSION}" -o ptt-server .
chmod +x ptt-server
echo -e "  ${GREEN}Built version: ${VERSION}${NC}"

# Step 3: Create config if missing
echo -e "${YELLOW}[3/7] Checking config...${NC}"
if [ ! -f "$INSTALL_DIR/config.json" ]; then
    if [ -f "$INSTALL_DIR/config.json.example" ]; then
        cp "$INSTALL_DIR/config.json.example" "$INSTALL_DIR/config.json"
        echo -e "  Created config.json from template"
    else
        cat > "$INSTALL_DIR/config.json" << 'CONF'
{
  "ptt_port": 5000,
  "web_port": 8080,
  "max_clients": 1000,
  "max_channels": 100,
  "log_level": "INFO",
  "auth_enabled": true,
  "users_file": "users.json",
  "web_admins_file": "web_admins.json",
  "agencies_file": "agencies.json"
}
CONF
        echo -e "  Created default config.json"
    fi
else
    echo -e "  config.json exists (preserved)"
fi

# Step 4: Create service user
echo -e "${YELLOW}[4/7] Checking service user...${NC}"
if ! id "$SERVICE_USER" &>/dev/null; then
    useradd -r -s /bin/false "$SERVICE_USER"
    echo -e "  Created user: ${SERVICE_USER}"
else
    echo -e "  User ${SERVICE_USER} already exists"
fi

# Step 5: Set permissions
echo -e "${YELLOW}[5/7] Setting permissions...${NC}"
chown -R "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR"

# Ensure the ptt user can traverse parent directories to reach the install dir
CURRENT="$INSTALL_DIR"
while [ "$CURRENT" != "/" ]; do
    CURRENT="$(dirname "$CURRENT")"
    chmod o+x "$CURRENT" 2>/dev/null || true
done
echo -e "  Ownership set to ${SERVICE_USER}"

# Restrict sensitive data files to owner-read/write only
for f in config.json users.json web_admins.json agencies.json mutual_aid.json; do
    if [ -f "$INSTALL_DIR/$f" ]; then
        chmod 600 "$INSTALL_DIR/$f"
    fi
done
echo -e "  Sensitive files restricted to owner-only (chmod 600)"

# Step 6: Install systemd service + firewall
echo -e "${YELLOW}[6/7] Installing service...${NC}"
cat > /etc/systemd/system/${SERVICE_NAME}.service << EOF
[Unit]
Description=MeshRelay PTT Server
After=network.target

[Service]
Type=simple
User=${SERVICE_USER}
WorkingDirectory=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/ptt-server
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable "$SERVICE_NAME" >/dev/null 2>&1
echo -e "  Service installed and enabled"

# Firewall
if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "active"; then
    ufw allow 5000/udp comment 'MeshRelay PTT' >/dev/null 2>&1 || true
    ufw allow 8080/tcp comment 'MeshRelay Dashboard' >/dev/null 2>&1 || true
    echo -e "  Firewall: opened UDP 5000 and TCP 8080"
else
    echo -e "  Firewall: UFW not active (configure manually if needed)"
fi

# Sudoers: restrict ptt user to only restart its own service (for self-update)
SUDOERS_FILE="/etc/sudoers.d/ptt-server"
echo "${SERVICE_USER} ALL=(root) NOPASSWD: /usr/bin/systemctl restart ${SERVICE_NAME}" > "$SUDOERS_FILE"
chmod 440 "$SUDOERS_FILE"
echo -e "  Sudoers: ${SERVICE_USER} can only restart ${SERVICE_NAME}"

# Router port forwarding reminder
PUBLIC_IP=$(curl -s --max-time 5 https://api.ipify.org 2>/dev/null || echo "unknown")
echo ""
echo -e "${YELLOW}============================================${NC}"
echo -e "${YELLOW}  ACTION REQUIRED: Router Port Forwarding${NC}"
echo -e "${YELLOW}============================================${NC}"
echo ""
echo -e "  Log into your router and forward these ports to this server:"
echo ""
echo -e "  ${GREEN}UDP 5000${NC}  → PTT audio (required for all clients)"
echo -e "  ${GREEN}TCP 8080${NC}  → Web dashboard (required if accessing remotely)"
echo -e "  ${GREEN}TCP 443${NC}   → HTTPS dashboard (required if you set up a domain below)"
echo -e "  ${GREEN}TCP 80${NC}    → Temp only during domain cert setup (can close after)"
echo ""
if [ "$PUBLIC_IP" != "unknown" ]; then
    echo -e "  This server's public IP appears to be: ${GREEN}${PUBLIC_IP}${NC}"
fi
echo -e "  This server's local IP is: ${GREEN}$(hostname -I 2>/dev/null | awk '{print $1}')${NC}"
echo ""
read -r -p "  Press Enter once port forwarding is configured (or to skip for now)..." _

# Optional Step: FQDN + TLS via Let's Encrypt
echo ""
echo -e "${YELLOW}[Optional] Set up a domain name with HTTPS (Let's Encrypt)?${NC}"
echo -e "  Requires a domain already pointed at this server's public IP (${PUBLIC_IP})."
echo -e "  If you haven't created a DNS A record yet, do that now before continuing."
echo ""
read -r -p "  Enter your domain (e.g. ptt.example.com), or press Enter to skip: " DOMAIN
if [ -n "$DOMAIN" ]; then
    echo ""
    echo -e "${YELLOW}  Before continuing, make sure:${NC}"
    echo -e "  1. Your domain registrar has an A record: ${GREEN}${DOMAIN}${NC} → ${GREEN}${PUBLIC_IP}${NC}"
    echo -e "  2. Router port 80 is forwarded to this server (needed for verification)"
    echo -e "  3. DNS has had time to propagate (can take a few minutes)"
    echo ""
    read -r -p "  Press Enter when the A record is set and DNS is propagated, or Ctrl+C to abort: " _
    echo -e "${YELLOW}  Installing certbot...${NC}"
    apt-get install -y -qq certbot

    # Open port 80 temporarily for ACME challenge
    if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "active"; then
        ufw allow 80/tcp >/dev/null 2>&1 || true
    fi

    echo -e "${YELLOW}  Requesting certificate for ${DOMAIN}...${NC}"
    if certbot certonly --standalone --non-interactive --agree-tos --register-unsafely-without-email -d "$DOMAIN"; then
        CERT_DIR="/etc/letsencrypt/live/${DOMAIN}"

        # Update config.json with cert paths
        python3 - <<PYEOF
import json, sys
path = "${INSTALL_DIR}/config.json"
with open(path) as f:
    cfg = json.load(f)
cfg["tls_cert_file"] = "${CERT_DIR}/fullchain.pem"
cfg["tls_key_file"]  = "${CERT_DIR}/privkey.pem"
cfg["web_port"]      = 443
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
print("  config.json updated")
PYEOF

        # Let the ptt service user read the certs
        # live/ contains symlinks into archive/ — must fix both
        chown -R root:"$SERVICE_USER" "$CERT_DIR"
        chown -R root:"$SERVICE_USER" "/etc/letsencrypt/archive/${DOMAIN}"
        chmod 750 "$CERT_DIR"
        chmod 750 "/etc/letsencrypt/archive/${DOMAIN}"
        chmod 640 "/etc/letsencrypt/archive/${DOMAIN}"/*.pem

        # Also allow the service to bind to port 443
        setcap 'cap_net_bind_service=+ep' "${INSTALL_DIR}/ptt-server" 2>/dev/null || true

        # Open port 443, close 8080
        if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "active"; then
            ufw allow 443/tcp comment 'MeshRelay Dashboard HTTPS' >/dev/null 2>&1 || true
            ufw delete allow 8080/tcp >/dev/null 2>&1 || true
            ufw delete allow 80/tcp >/dev/null 2>&1 || true
        fi

        # Set up auto-renewal and service restart
        RENEW_HOOK="/etc/letsencrypt/renewal-hooks/deploy/ptt-server-restart.sh"
        cat > "$RENEW_HOOK" << HOOK
#!/bin/bash
chown -R root:${SERVICE_USER} /etc/letsencrypt/live/${DOMAIN}
chown -R root:${SERVICE_USER} /etc/letsencrypt/archive/${DOMAIN}
chmod 750 /etc/letsencrypt/live/${DOMAIN}
chmod 750 /etc/letsencrypt/archive/${DOMAIN}
chmod 640 /etc/letsencrypt/archive/${DOMAIN}/*.pem
systemctl restart ${SERVICE_NAME}
HOOK
        chmod +x "$RENEW_HOOK"

        echo -e "  ${GREEN}TLS certificate installed for ${DOMAIN}${NC}"
        echo -e "  ${GREEN}Dashboard will be available at https://${DOMAIN}${NC}"
        echo -e "  Auto-renewal configured via certbot"
        DASHBOARD_URL="https://${DOMAIN}"
    else
        echo -e "  ${RED}Certificate request failed.${NC}"
        echo -e "  Make sure ${DOMAIN} points to this server's public IP and port 80 is reachable."
        echo -e "  Continuing without TLS..."
        DASHBOARD_URL="http://$(hostname -I 2>/dev/null | awk '{print $1}'):8080"
        # Re-close port 80 if we opened it
        if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "active"; then
            ufw delete allow 80/tcp >/dev/null 2>&1 || true
        fi
    fi
else
    echo -e "  Skipping — dashboard will be available over HTTP"
    DASHBOARD_URL="http://$(hostname -I 2>/dev/null | awk '{print $1}'):8080"
fi

# Step 7: Start service
echo -e "${YELLOW}[7/7] Starting server...${NC}"
systemctl restart "$SERVICE_NAME"
sleep 2

# Verify and print info
if systemctl is-active --quiet "$SERVICE_NAME"; then
    echo ""
    echo -e "${GREEN}============================================${NC}"
    echo -e "${GREEN}  MeshRelay Server is running!${NC}"
    echo -e "${GREEN}============================================${NC}"
    echo ""
    echo -e "  Dashboard:  ${GREEN}${DASHBOARD_URL}${NC}"
    echo -e "  Login:      Get initial admin password with:"
    echo -e "              journalctl -u ${SERVICE_NAME} --no-pager -o cat | grep 'INITIAL ADMIN PASSWORD'"
    echo -e "  PTT Port:   UDP 5000"
    echo ""
    echo -e "  Logs:       journalctl -u ${SERVICE_NAME} -f"
    echo -e "  Status:     systemctl status ${SERVICE_NAME}"
    echo -e "  Restart:    sudo systemctl restart ${SERVICE_NAME}"
    echo ""
    echo -e "  To update:  git pull && sudo ./install.sh"
    echo ""
else
    echo ""
    echo -e "${RED}Server failed to start. Check logs:${NC}"
    echo "  journalctl -u ${SERVICE_NAME} --no-pager -n 20"
    echo ""
    exit 1
fi
