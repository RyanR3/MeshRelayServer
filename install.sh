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
  "auth_enabled": false,
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

# Step 7: Start service
echo -e "${YELLOW}[7/7] Starting server...${NC}"
systemctl restart "$SERVICE_NAME"
sleep 2

# Verify and print info
if systemctl is-active --quiet "$SERVICE_NAME"; then
    IP=$(hostname -I 2>/dev/null | awk '{print $1}')
    [ -z "$IP" ] && IP="your-server-ip"
    echo ""
    echo -e "${GREEN}============================================${NC}"
    echo -e "${GREEN}  MeshRelay Server is running!${NC}"
    echo -e "${GREEN}============================================${NC}"
    echo ""
    echo -e "  Dashboard:  ${GREEN}http://${IP}:8080${NC}"
    echo -e "  Login:      admin / admin"
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
