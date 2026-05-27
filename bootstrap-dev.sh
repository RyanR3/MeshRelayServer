#!/bin/bash
#
# bootstrap-dev.sh — One-shot setup for a FRESH Ubuntu dev server.
#
# Goal: stand up the MeshRelay server on an internal IP to validate the FULL
# setup (new auth, key exchange, channel encryption, client<->server TLS) before
# rolling the same configuration to the production box on this network.
#
# DEFAULT = self-signed TLS on the server's internal IP, port 8443. No FQDN, no
# DNS, no public exposure. This mirrors production (which will also use TLS), so
# the only thing that changes at production cutover is the IP/cert — every code
# path, including TLS and client cert-pinning, is exercised on dev.
#
# What it does:
#   1. Installs prerequisites a fresh box lacks (build tools, git, openssl, Go).
#   2. Locates the MeshRelay server source (uploaded by you via scp/SFTP).
#   3. (TLS mode) Generates a self-signed cert with the server's IP(s) in the SAN.
#   4. Writes TLS settings + port into config.json.
#   5. Hands off to the repo's existing install.sh (build, service user, perms,
#      systemd unit, firewall, start).
#   6. Prints the cert SHA-256 fingerprint so you can PIN it in the client.
#
# USAGE (run on the fresh server, script placed next to install.sh):
#   sudo bash bootstrap-dev.sh                 # TLS on 8443, LAN IP auto-detected
#   sudo bash bootstrap-dev.sh 10.8.0.2        # also add a WireGuard IP to the cert
#   sudo bash bootstrap-dev.sh 10.8.0.2 192.168.50.10   # multiple extra IPs
#   sudo bash bootstrap-dev.sh --http          # plain HTTP on 8080 (quick logic only)
#
set -e

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'

# ---- OPTIONAL: clone from GitHub instead of using uploaded files ----
REPO_URL=""                       # leave empty to use already-uploaded local files
CLONE_DIR="MeshRelayServer"
# ---------------------------------------------------------------------

# ---- Parse args: --http disables TLS; any IP-looking args are extra SAN IPs ----
USE_TLS=1
EXTRA_IPS=()
for arg in "$@"; do
    case "$arg" in
        --http) USE_TLS=0 ;;
        --tls)  USE_TLS=1 ;;
        *) EXTRA_IPS+=("$arg") ;;   # treat anything else as an IP for the cert SAN
    esac
done

echo ""
echo -e "${GREEN}=== MeshRelay Dev Server Bootstrap ===${NC}"
[ "$USE_TLS" -eq 1 ] && echo -e "Mode: ${GREEN}TLS (self-signed, internal IP, :8443)${NC}" \
                     || echo -e "Mode: ${YELLOW}plain HTTP (:8080) — quick logic testing only${NC}"
echo ""

# Must be root.
if [ "$EUID" -ne 0 ]; then
    echo -e "${RED}Please run as root: sudo bash bootstrap-dev.sh${NC}"
    exit 1
fi

BUILD_USER="${SUDO_USER:-$USER}"
echo -e "${YELLOW}[1/7] Build user: ${BUILD_USER}${NC}"

# Step 2: prerequisites
echo -e "${YELLOW}[2/7] Installing prerequisites...${NC}"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq build-essential git ca-certificates curl coreutils openssl
if ! command -v go &>/dev/null; then
    echo -e "  Installing Go (golang-go)..."
    apt-get install -y -qq golang-go
fi
echo -e "  ${GREEN}$(go version)${NC}"

# Step 3: locate source
echo -e "${YELLOW}[3/7] Locating source...${NC}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -n "$REPO_URL" ]; then
    if [ ! -d "$CLONE_DIR" ]; then
        sudo -u "$BUILD_USER" git clone "$REPO_URL" "$CLONE_DIR"
    else
        sudo -u "$BUILD_USER" git -C "$CLONE_DIR" pull --ff-only || true
    fi
    SRC_DIR="$(cd "$CLONE_DIR" && pwd)"
else
    if [ -f "$SCRIPT_DIR/install.sh" ]; then
        SRC_DIR="$SCRIPT_DIR"
    elif [ -f "$SCRIPT_DIR/MeshRelayServer/install.sh" ]; then
        SRC_DIR="$SCRIPT_DIR/MeshRelayServer"
    else
        echo -e "${RED}  Could not find install.sh. Put this script next to it.${NC}"
        exit 1
    fi
fi
echo -e "  ${GREEN}Source: ${SRC_DIR}${NC}"

# Step 4: verify expected files
echo -e "${YELLOW}[4/7] Verifying source files...${NC}"
MISSING=0
for f in install.sh main.go server.go web.go config.go go.mod; do
    [ -f "$SRC_DIR/$f" ] || { echo -e "  ${RED}Missing: $f${NC}"; MISSING=1; }
done
[ "$MISSING" -ne 0 ] && { echo -e "${RED}  Source incomplete — re-upload.${NC}"; exit 1; }
echo -e "  ${GREEN}All expected files present${NC}"

# Detect primary LAN IP
LAN_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -z "$LAN_IP" ] && LAN_IP="127.0.0.1"

# Step 5: TLS cert generation (if TLS mode)
CERT_FILE="$SRC_DIR/server.crt"
KEY_FILE="$SRC_DIR/server.key"
WEB_PORT=8080
if [ "$USE_TLS" -eq 1 ]; then
    echo -e "${YELLOW}[5/7] Generating self-signed TLS cert...${NC}"
    WEB_PORT=8443

    # Build SAN list: localhost + LAN IP + any extra IPs passed as args
    SAN="IP:127.0.0.1,DNS:localhost,IP:${LAN_IP}"
    for ip in "${EXTRA_IPS[@]}"; do
        SAN="${SAN},IP:${ip}"
    done
    echo -e "  Cert will be valid for: ${GREEN}${SAN}${NC}"

    if [ -f "$CERT_FILE" ] && [ -f "$KEY_FILE" ]; then
        echo -e "  Existing cert found — keeping it (delete server.crt/server.key to regenerate)"
    else
        openssl req -x509 -newkey rsa:4096 -sha256 -days 825 -nodes \
            -keyout "$KEY_FILE" -out "$CERT_FILE" \
            -subj "/CN=MeshRelay-Dev/O=R3 Systems" \
            -addext "subjectAltName=${SAN}" >/dev/null 2>&1
        echo -e "  ${GREEN}Created server.crt / server.key (valid ~825 days)${NC}"
    fi
else
    echo -e "${YELLOW}[5/7] Skipping TLS (plain HTTP mode)${NC}"
fi

# Step 6: write config.json (TLS fields + port) BEFORE install.sh runs
echo -e "${YELLOW}[6/7] Writing config.json...${NC}"
CONF="$SRC_DIR/config.json"
if [ "$USE_TLS" -eq 1 ]; then
    cat > "$CONF" << CONF_EOF
{
  "ptt_port": 5000,
  "web_port": ${WEB_PORT},
  "max_clients": 1000,
  "max_channels": 100,
  "log_level": "INFO",
  "auth_enabled": true,
  "users_file": "users.json",
  "web_admins_file": "web_admins.json",
  "agencies_file": "agencies.json",
  "tls_cert_file": "server.crt",
  "tls_key_file": "server.key"
}
CONF_EOF
else
    cat > "$CONF" << 'CONF_EOF'
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
CONF_EOF
fi
echo -e "  ${GREEN}config.json written (web_port=${WEB_PORT})${NC}"

# NOTE: install.sh opens 8080/tcp by default. In TLS mode we open 8443 here so
# the dashboard is reachable; install.sh's 8080 rule is harmless (nothing listens).
if [ "$USE_TLS" -eq 1 ] && command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q active; then
    ufw allow 8443/tcp comment 'MeshRelay Dashboard TLS' >/dev/null 2>&1 || true
    echo -e "  Firewall: opened TCP 8443"
fi

# Step 7: hand off to install.sh
echo -e "${YELLOW}[7/7] Running install.sh...${NC}"
echo ""
chmod +x "$SRC_DIR/install.sh"
cd "$SRC_DIR"
./install.sh

# Post-install: cert fingerprint for client pinning
echo ""
echo -e "${GREEN}=== Bootstrap complete ===${NC}"
echo ""
echo -e "Initial admin password (from the log):"
echo -e "  ${GREEN}journalctl -u ptt-server | grep 'INITIAL ADMIN PASSWORD'${NC}"
echo ""
if [ "$USE_TLS" -eq 1 ]; then
    FP="$(openssl x509 -in "$CERT_FILE" -noout -fingerprint -sha256 2>/dev/null | sed 's/.*=//')"
    echo -e "Dashboard:  ${GREEN}https://${LAN_IP}:8443${NC}"
    for ip in "${EXTRA_IPS[@]}"; do echo -e "        or  ${GREEN}https://${ip}:8443${NC}"; done
    echo -e "  (self-signed — your browser will warn once; that's expected on dev)"
    echo ""
    echo -e "${YELLOW}PIN THIS in the PTT client (it will reject the server otherwise):${NC}"
    echo -e "  Cert SHA-256 fingerprint:"
    echo -e "  ${GREEN}${FP}${NC}"
    echo -e "  (Same cert-pinning pattern your TAK client uses. Do NOT use"
    echo -e "   InsecureSkipVerify — pin this fingerprint instead.)"
else
    echo -e "Dashboard:  ${GREEN}http://${LAN_IP}:8080${NC}  (plain HTTP — dev logic testing only)"
fi
echo ""
echo -e "When dev is confirmed, replicate on production with the production"
echo -e "server's IP (regenerate the cert there with its IP in the SAN, or use"
echo -e "your domain's cert if production has an FQDN)."
echo ""
