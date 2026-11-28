#!/bin/bash
# MeshRelay Server installer and updater for Debian-family Linux (Ubuntu,
# Debian, Raspberry Pi OS). Install:  sudo ./install.sh
# Update:   git pull && sudo ./install.sh   (as the user who cloned the repo)
#
# Who owns what after this script (nothing else is changed on re-run):
#   - The checkout (.git and every tracked file) belongs to the user who
#     cloned it, so that user can `git pull` without sudo.
#   - The install folder, web/ and transcribe/ also have group SERVICE_USER
#     with group write, so the service can keep its state files next to the
#     binary and the dashboard self-update can replace the binary and web/.
#     The install folder and transcribe/ carry the sticky bit: the service can
#     add and replace ITS OWN files there, never the cloned files (this
#     script, the sources) that root later runs or builds.
#   - The binary and everything the server writes (config.json, users.json,
#     web_admins.json, recordings/, backups/, ...) belong to SERVICE_USER.
set -e
set -o pipefail

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

INSTALL_DIR="$(cd "$(dirname "$0")" && pwd)"
SERVICE_USER="ptt"
SERVICE_NAME="ptt-server"
# Go toolchains this script downloads live here, one folder per version. They
# are never put on PATH and never replace a Go installed by the system.
TOOLCHAIN_ROOT="/usr/local/lib/meshrelay-server"

die() {
    echo -e "${RED}$*${NC}" >&2
    exit 1
}

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

# Run a command as another user, without a login shell (the service user has
# none). runuser is part of util-linux, so it is always there.
as_user() {
    local u="$1"
    shift
    local home
    home="$(getent passwd "$u" | cut -d: -f6)"
    runuser -u "$u" -- env HOME="${home:-/}" PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" "$@"
}

# fetch URL FILE: download over HTTPS with curl or wget (curl is installed
# when neither is present).
fetch() {
    if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
        echo -e "  Installing curl..."
        { apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq curl ca-certificates >/dev/null 2>&1; } ||
            die "  Could not install curl (no internet access?). It is needed to download Go."
    fi
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --retry 2 -o "$2" "$1"
    else
        wget -q --https-only --timeout=15 --tries=3 -O "$2" "$1"
    fi
}

# version_ge HAVE NEED: true when Go version HAVE is NEED or newer.
version_ge() {
    [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]
}

# go_version BIN: "1.26.11" for that go binary, empty if it does not run.
go_version() {
    GOTOOLCHAIN=local "$1" env GOVERSION 2>/dev/null | sed -n 's/^go\([0-9][0-9.a-z]*\).*/\1/p' | head -n1
}

# Step 1: Go
echo -e "${YELLOW}[1/7] Checking Go...${NC}"
# The version is read from go.mod, the one place it is set: the `toolchain`
# line (the Go release builds use) when there is one, otherwise the `go` line.
[ -f "$INSTALL_DIR/go.mod" ] || die "  go.mod not found in $INSTALL_DIR"
GO_NEED="$(awk '$1 == "toolchain" && $2 ~ /^go[0-9]/ { sub(/^go/, "", $2); print $2; exit }' "$INSTALL_DIR/go.mod")"
if [ -z "$GO_NEED" ]; then
    GO_NEED="$(awk '$1 == "go" { print $2; exit }' "$INSTALL_DIR/go.mod")"
    # a two-part "go X.Y" line names the release goX.Y.0
    case "$GO_NEED" in *.*.*) ;; *.*) GO_NEED="${GO_NEED}.0" ;; esac
fi
[ -n "$GO_NEED" ] || die "  Could not read the Go version from go.mod"

GO_BIN=""
for cand in "$(command -v go 2>/dev/null || true)" /usr/local/go/bin/go /usr/lib/go/bin/go "$TOOLCHAIN_ROOT/go${GO_NEED}/bin/go"; do
    [ -n "$cand" ] && [ -x "$cand" ] || continue
    have="$(go_version "$cand")"
    if [ -n "$have" ] && version_ge "$have" "$GO_NEED"; then
        GO_BIN="$cand"
        break
    fi
    [ -n "$have" ] && echo -e "  $cand is Go $have, older than the $GO_NEED this version needs"
done

if [ -z "$GO_BIN" ]; then
    # Official toolchain from go.dev for this machine, checked against the
    # SHA-256 that go.dev publishes before it is unpacked.
    case "$(dpkg --print-architecture 2>/dev/null || uname -m)" in
        amd64 | x86_64) GOARCH_DL=amd64 ;;
        arm64 | aarch64) GOARCH_DL=arm64 ;;
        armhf | armel | armv6l | armv7l) GOARCH_DL=armv6l ;;
        i386 | i686) GOARCH_DL=386 ;;
        *) die "  No Go download for this machine ($(uname -m)). Install Go $GO_NEED or newer and re-run." ;;
    esac
    GO_FILE="go${GO_NEED}.linux-${GOARCH_DL}.tar.gz"
    echo -e "  Downloading Go ${GO_NEED} (${GOARCH_DL}) from go.dev..."
    mkdir -p "$TOOLCHAIN_ROOT"
    GO_TMP="$(mktemp -d "$TOOLCHAIN_ROOT/.download.XXXXXX")"
    trap 'rm -rf "$GO_TMP"' EXIT
    if ! fetch "https://go.dev/dl/?mode=json&include=all" "$GO_TMP/releases.json" ||
        ! fetch "https://go.dev/dl/${GO_FILE}" "$GO_TMP/$GO_FILE"; then
        die "  Could not download Go ${GO_NEED} from go.dev.
  This machine needs internet access (HTTPS to go.dev and dl.google.com) to
  fetch it, and to proxy.golang.org for the build's modules. Alternatively
  install Go ${GO_NEED} or newer as /usr/local/go and re-run."
    fi
    GO_FILE_RE="${GO_FILE//./\\.}"
    GO_SHA="$(tr -d ' \t\r\n' <"$GO_TMP/releases.json" | grep -o "\"filename\":\"${GO_FILE_RE}\"[^}]*" |
        grep -o '"sha256":"[0-9a-f]\{64\}"' | head -n1 | cut -d'"' -f4 || true)"
    [ -n "$GO_SHA" ] || die "  go.dev does not list a checksum for ${GO_FILE}; not installing it."
    if ! echo "${GO_SHA}  $GO_TMP/$GO_FILE" | sha256sum -c --status; then
        die "  Checksum MISMATCH for ${GO_FILE} (expected ${GO_SHA}); the download was not used."
    fi
    echo -e "  Checksum verified (${GO_SHA})"
    tar -C "$GO_TMP" -xzf "$GO_TMP/$GO_FILE"
    [ "$(go_version "$GO_TMP/go/bin/go")" = "$GO_NEED" ] || die "  The downloaded Go does not run on this machine."
    rm -rf "$TOOLCHAIN_ROOT/go${GO_NEED}"
    mv "$GO_TMP/go" "$TOOLCHAIN_ROOT/go${GO_NEED}"
    rm -rf "$GO_TMP"
    trap - EXIT
    # Older toolchains from earlier updates are no longer needed.
    find "$TOOLCHAIN_ROOT" -mindepth 1 -maxdepth 1 -type d -name 'go[0-9]*' ! -name "go${GO_NEED}" -exec rm -rf {} +
    GO_BIN="$TOOLCHAIN_ROOT/go${GO_NEED}/bin/go"
fi
echo -e "  Using Go $(go_version "$GO_BIN") ($GO_BIN)"

# Step 2: Service user (it must exist before ownership is sorted out)
echo -e "${YELLOW}[2/7] Checking service user...${NC}"
if ! id "$SERVICE_USER" &>/dev/null; then
    useradd -r -s /bin/false "$SERVICE_USER"
    echo -e "  Created user: ${SERVICE_USER}"
else
    echo -e "  User ${SERVICE_USER} already exists"
fi

# Step 3: Ownership of the checkout
echo -e "${YELLOW}[3/7] Checking ownership...${NC}"
if [ -d "$INSTALL_DIR/.git" ]; then
    SRC_OWNER="$(stat -c %U "$INSTALL_DIR/.git")"
else
    SRC_OWNER="$(stat -c %U "$INSTALL_DIR")"
fi
if [ "$SRC_OWNER" = "$SERVICE_USER" ] || [ "$SRC_OWNER" = "UNKNOWN" ]; then
    # Installed by an older install.sh, which gave the whole folder to the
    # service user (so `git pull` as yourself failed). Hand the checkout back
    # to the user running sudo; the server's own files stay as they are.
    SRC_OWNER="${SUDO_USER:-root}"
    [ "$SRC_OWNER" != "$SERVICE_USER" ] || die "  Run this as the user who cloned the repository: sudo ./install.sh"
    echo -e "  Returning the checkout to ${SRC_OWNER} (it belonged to ${SERVICE_USER})"
    # git checks the owner of the work tree's top folder as well as .git.
    chown "$SRC_OWNER:" "$INSTALL_DIR"
    if [ -d "$INSTALL_DIR/.git" ]; then
        chown -R "$SRC_OWNER:" "$INSTALL_DIR/.git"
        # Every tracked file, and each folder that holds one.
        as_user "$SRC_OWNER" git -C "$INSTALL_DIR" ls-files -z | (
            cd "$INSTALL_DIR"
            while IFS= read -r -d '' f; do
                [ -e "$f" ] || [ -L "$f" ] || continue
                chown -h "$SRC_OWNER:" "$f"
                d="$(dirname "$f")"
                while [ "$d" != "." ]; do
                    chown "$SRC_OWNER:" "$d"
                    d="$(dirname "$d")"
                done
            done
        )
    fi
fi
[ "$SRC_OWNER" != "$SERVICE_USER" ] || die "  The checkout must not belong to ${SERVICE_USER}."
SRC_GROUP="$(id -gn "$SRC_OWNER")"

# Folders the service writes into: group SERVICE_USER, group-writable.
chgrp "$SERVICE_USER" "$INSTALL_DIR"
chmod g+rwx,+t "$INSTALL_DIR"
if [ -d "$INSTALL_DIR/transcribe" ]; then
    chgrp "$SERVICE_USER" "$INSTALL_DIR/transcribe"
    chmod g+rwx,+t "$INSTALL_DIR/transcribe"
fi
if [ -d "$INSTALL_DIR/web" ]; then
    # The self-update renames new files into web/, so only the folders need
    # group write; the service only has to read the files. Files a
    # self-update left behind go back to the checkout's owner.
    find "$INSTALL_DIR/web" -type d -exec chown "$SRC_OWNER:$SERVICE_USER" {} + -exec chmod g+rwx {} +
    find "$INSTALL_DIR/web" ! -type d -exec chown -h "$SRC_OWNER:$SERVICE_USER" {} +
    find "$INSTALL_DIR/web" -type f -exec chmod g+r {} +
fi
echo -e "  Checkout: ${SRC_OWNER}:${SRC_GROUP}; the service (${SERVICE_USER}) writes its own files only"

# The service user must be able to pass through every folder above the
# install folder. Grant exactly that, to that user only, where it is missing.
missing=()
p="$INSTALL_DIR"
while [ "$p" != "/" ]; do
    p="$(dirname "$p")"
    runuser -u "$SERVICE_USER" -- test -x "$p" 2>/dev/null || missing=("$p" "${missing[@]}")
done
if [ ${#missing[@]} -gt 0 ]; then
    if ! command -v setfacl >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
        echo -e "  Installing acl (to let ${SERVICE_USER} pass through ${missing[*]})..."
        apt-get install -y -qq acl >/dev/null 2>&1 || { apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq acl >/dev/null 2>&1; } || true
    fi
    for p in "${missing[@]}"; do
        if command -v setfacl >/dev/null 2>&1 && setfacl -m "u:${SERVICE_USER}:x" "$p" 2>/dev/null &&
            runuser -u "$SERVICE_USER" -- test -x "$p" 2>/dev/null; then
            echo -e "  Allowed ${SERVICE_USER} (only) to pass through $p (ACL u:${SERVICE_USER}:x)"
        else
            die "  The service user ${SERVICE_USER} cannot enter $p, so it cannot reach $INSTALL_DIR,
  and an ACL could not be set there. This script no longer opens parent
  folders to everyone. Either install the acl package (sudo apt install acl)
  and re-run, or clone the server somewhere ${SERVICE_USER} can reach, e.g.:
    sudo mkdir -p /opt/MeshRelayServer && sudo chown ${SRC_OWNER}: /opt/MeshRelayServer
    git clone <repository URL> /opt/MeshRelayServer && cd /opt/MeshRelayServer && sudo ./install.sh"
        fi
    done
fi

# Step 4: Build binary, as the checkout's owner (never as root), into a
# temporary file that root then moves into place.
echo -e "${YELLOW}[4/7] Building server...${NC}"
cd "$INSTALL_DIR"
# The version is the git tag (`git describe`), read as the checkout's owner so
# git's ownership check never gets in the way. No git metadata: "dev".
VERSION=""
if [ -d "$INSTALL_DIR/.git" ] && command -v git >/dev/null 2>&1; then
    VERSION="$(as_user "$SRC_OWNER" git -C "$INSTALL_DIR" describe --tags --always 2>/dev/null || true)"
fi
if [ -z "$VERSION" ]; then
    VERSION="dev"
    echo -e "  ${YELLOW}No git metadata: building as version \"dev\" (clone with git to get the release version)${NC}"
fi
BUILD_TMP="$(mktemp -d)"
trap 'rm -rf "$BUILD_TMP"' EXIT
chown "$SRC_OWNER:" "$BUILD_TMP"
GO_ENV=(GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly CGO_ENABLED=0)
if ! as_user "$SRC_OWNER" "${GO_ENV[@]}" "$GO_BIN" mod download; then
    die "  Could not download the Go modules (proxy.golang.org). Check this machine's internet access and re-run."
fi
as_user "$SRC_OWNER" "${GO_ENV[@]}" "$GO_BIN" build -trimpath -buildvcs=false \
    -ldflags "-X main.Version=${VERSION}" -o "$BUILD_TMP/ptt-server" . ||
    die "  Build failed; the installed server was not changed."
# Replace by rename: works while the old binary is running, and leaves the
# service user as owner so the dashboard self-update can replace it too.
install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$BUILD_TMP/ptt-server" "$INSTALL_DIR/.ptt-server.install"
mv -f "$INSTALL_DIR/.ptt-server.install" "$INSTALL_DIR/ptt-server"
rm -rf "$BUILD_TMP"
trap - EXIT
echo -e "  ${GREEN}Built version: ${VERSION}${NC}"

# Step 5: Create config if missing
echo -e "${YELLOW}[5/7] Checking config...${NC}"
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

# Restrict sensitive data files to the service user, owner-read/write only
for f in config.json users.json web_admins.json agencies.json mutual_aid.json guest_events.json; do
    if [ -f "$INSTALL_DIR/$f" ]; then
        chown "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR/$f"
        chmod 600 "$INSTALL_DIR/$f"
    fi
done
echo -e "  Sensitive files belong to ${SERVICE_USER}, owner-only (chmod 600)"

# Step 6: Install systemd service + firewall
echo -e "${YELLOW}[6/7] Installing service...${NC}"
# The service runs setcap before each start (to bind ports below 1024).
if ! command -v setcap >/dev/null 2>&1 && [ ! -x /sbin/setcap ]; then
    echo -e "  Installing setcap (libcap2-bin)..."
    apt-get install -y -qq libcap2-bin >/dev/null 2>&1 ||
        { apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq libcap2-bin >/dev/null 2>&1; } ||
        echo -e "  ${YELLOW}Could not install libcap2-bin; the service needs /sbin/setcap to start${NC}"
fi
cat > /etc/systemd/system/${SERVICE_NAME}.service << EOF
[Unit]
Description=MeshRelay PTT Server
After=network.target

[Service]
Type=simple
User=${SERVICE_USER}
WorkingDirectory=${INSTALL_DIR}
ExecStartPre=+/sbin/setcap cap_net_bind_service=+ep ${INSTALL_DIR}/ptt-server
ExecStart=${INSTALL_DIR}/ptt-server
AmbientCapabilities=CAP_NET_BIND_SERVICE
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

# systemd is PID 1 on every supported install; without it (a container, a
# chroot) the unit file is written but not enabled or started.
if [ -d /run/systemd/system ]; then
    HAVE_SYSTEMD=1
    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME" >/dev/null 2>&1
    echo -e "  Service installed and enabled"
else
    HAVE_SYSTEMD=0
    echo -e "  ${YELLOW}systemd is not running here: service file written, not enabled or started${NC}"
fi

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
read -r -p "  Press Enter once port forwarding is configured (or to skip for now)..." _ || true

# Optional Step: FQDN + TLS via Let's Encrypt
echo ""
echo -e "${YELLOW}[Optional] Set up a domain name with HTTPS (Let's Encrypt)?${NC}"
echo -e "  Requires a domain already pointed at this server's public IP (${PUBLIC_IP})."
echo -e "  If you haven't created a DNS A record yet, do that now before continuing."
echo ""
DOMAIN=""
read -r -p "  Enter your domain (e.g. ptt.example.com), or press Enter to skip: " DOMAIN || true
if [ -n "$DOMAIN" ]; then
    echo ""
    echo -e "${YELLOW}  Before continuing, make sure:${NC}"
    echo -e "  1. Your domain registrar has an A record: ${GREEN}${DOMAIN}${NC} → ${GREEN}${PUBLIC_IP}${NC}"
    echo -e "  2. Router port 80 is forwarded to this server (needed for verification)"
    echo -e "  3. DNS has had time to propagate (can take a few minutes)"
    echo ""
    read -r -p "  Press Enter when the A record is set and DNS is propagated, or Ctrl+C to abort: " _ || true
    echo -e "${YELLOW}  Installing certbot...${NC}"
    apt-get install -y -qq certbot

    # Open port 80 temporarily for ACME challenge
    if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "active"; then
        ufw allow 80/tcp >/dev/null 2>&1 || true
    fi

    echo -e "${YELLOW}  Requesting certificate for ${DOMAIN}...${NC}"
    if certbot certonly --standalone --non-interactive --agree-tos --register-unsafely-without-email -d "$DOMAIN"; then
        CERT_DIR="/etc/letsencrypt/live/${DOMAIN}"

        # Copy certs into the install directory so the ptt service user can read them
        # without needing to navigate letsencrypt's restrictive directory permissions.
        cp "$CERT_DIR/fullchain.pem" "$INSTALL_DIR/fullchain.pem"
        cp "$CERT_DIR/privkey.pem"   "$INSTALL_DIR/privkey.pem"
        chown "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR/fullchain.pem" "$INSTALL_DIR/privkey.pem"
        chmod 640 "$INSTALL_DIR/fullchain.pem" "$INSTALL_DIR/privkey.pem"

        # Update config.json to point at the local cert copies
        python3 - <<PYEOF
import json
path = "${INSTALL_DIR}/config.json"
with open(path) as f:
    cfg = json.load(f)
cfg["tls_cert_file"] = "fullchain.pem"
cfg["tls_key_file"]  = "privkey.pem"
cfg["web_port"]      = 443
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
print("  config.json updated")
PYEOF

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
# Re-copy renewed certs into the server directory and restart
cp /etc/letsencrypt/live/${DOMAIN}/fullchain.pem ${INSTALL_DIR}/fullchain.pem
cp /etc/letsencrypt/live/${DOMAIN}/privkey.pem   ${INSTALL_DIR}/privkey.pem
chown ${SERVICE_USER}:${SERVICE_USER} ${INSTALL_DIR}/fullchain.pem ${INSTALL_DIR}/privkey.pem
chmod 640 ${INSTALL_DIR}/fullchain.pem ${INSTALL_DIR}/privkey.pem
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
if [ "$HAVE_SYSTEMD" != 1 ]; then
    echo -e "  ${YELLOW}Not started: systemd is not running on this machine.${NC}"
    echo -e "  Build and files are in place in ${INSTALL_DIR}; start ./ptt-server as ${SERVICE_USER} by other means."
    exit 0
fi
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
    echo -e "              journalctl -u ${SERVICE_NAME} --no-pager -o cat | grep -A1 'INITIAL ADMIN PASSWORD'"
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
