#!/bin/bash

# MeshRelay Server Update Script
# Usage: ./update.sh [path-to-update-files]
#        ./update.sh --rollback

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Determine install directory (where this script lives)
INSTALL_DIR="$(cd "$(dirname "$0")" && pwd)"
BACKUP_DIR="$INSTALL_DIR/backups"
UPDATE_DIR="${1:-$HOME/pttserver-update}"
SERVICE_NAME="ptt-server"

echo ""
echo -e "${GREEN}=== MeshRelay Server Update ===${NC}"
echo ""

# --- Rollback Mode ---
if [ "$1" = "--rollback" ]; then
    LATEST_BACKUP=$(ls -td "$BACKUP_DIR"/*/ 2>/dev/null | head -1)
    if [ -z "$LATEST_BACKUP" ]; then
        echo -e "${RED}No backups found in $BACKUP_DIR${NC}"
        exit 1
    fi

    echo -e "${YELLOW}Rolling back to: $(basename "$LATEST_BACKUP")${NC}"
    echo ""

    sudo systemctl stop "$SERVICE_NAME" 2>/dev/null || true

    if [ -f "$LATEST_BACKUP/ptt-server" ]; then
        cp "$LATEST_BACKUP/ptt-server" "$INSTALL_DIR/ptt-server"
        chmod +x "$INSTALL_DIR/ptt-server"
        echo -e "${GREEN}  Restored binary${NC}"
    fi

    if [ -d "$LATEST_BACKUP/web" ]; then
        rm -rf "$INSTALL_DIR/web"
        cp -r "$LATEST_BACKUP/web" "$INSTALL_DIR/web"
        echo -e "${GREEN}  Restored web files${NC}"
    fi

    sudo systemctl start "$SERVICE_NAME"
    echo ""
    echo -e "${GREEN}Rollback complete!${NC}"
    sudo systemctl status "$SERVICE_NAME" --no-pager
    exit 0
fi

# --- Update Mode ---

# Check for update files
if [ ! -d "$UPDATE_DIR" ]; then
    echo -e "${RED}Update directory not found: $UPDATE_DIR${NC}"
    echo ""
    echo "Place your update files in ~/pttserver-update/ or specify a path:"
    echo "  ./update.sh /path/to/update/files"
    echo ""
    echo "The update directory should contain:"
    echo "  ptt-server    (the binary)"
    echo "  web/          (the web folder)"
    exit 1
fi

if [ ! -f "$UPDATE_DIR/ptt-server" ]; then
    echo -e "${RED}No ptt-server binary found in $UPDATE_DIR${NC}"
    exit 1
fi

echo "Install directory: $INSTALL_DIR"
echo "Update from:       $UPDATE_DIR"
echo ""

# Prompt for sudo upfront
sudo -v

# 1. Create backup
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
BACKUP_PATH="$BACKUP_DIR/$TIMESTAMP"
mkdir -p "$BACKUP_PATH"

echo -e "${YELLOW}[1/5] Creating backup...${NC}"
cp "$INSTALL_DIR/ptt-server" "$BACKUP_PATH/ptt-server" 2>/dev/null || true
if [ -d "$INSTALL_DIR/web" ]; then
    cp -r "$INSTALL_DIR/web" "$BACKUP_PATH/web"
fi
echo -e "  Backup saved to: $BACKUP_PATH"

# 2. Stop service
echo -e "${YELLOW}[2/5] Stopping service...${NC}"
sudo systemctl stop "$SERVICE_NAME" 2>/dev/null || true
echo -e "  Service stopped"

# 3. Copy new files
echo -e "${YELLOW}[3/5] Installing update...${NC}"
cp "$UPDATE_DIR/ptt-server" "$INSTALL_DIR/ptt-server"
chmod +x "$INSTALL_DIR/ptt-server"
echo -e "  Updated binary"

if [ -d "$UPDATE_DIR/web" ]; then
    # Copy web files but don't remove the whole directory (preserves any extra files)
    cp -r "$UPDATE_DIR/web/"* "$INSTALL_DIR/web/"
    echo -e "  Updated web files"
fi

# 4. Start service
echo -e "${YELLOW}[4/5] Starting service...${NC}"
sudo systemctl start "$SERVICE_NAME"
echo -e "  Service started"

# 5. Verify
echo -e "${YELLOW}[5/5] Verifying...${NC}"
sleep 1
echo ""
sudo systemctl status "$SERVICE_NAME" --no-pager

echo ""
echo -e "${GREEN}Update complete!${NC}"
echo ""
echo "Data files preserved (not overwritten):"
echo "  - config.json"
echo "  - users.json"
echo "  - web_admins.json"
echo ""
echo -e "To rollback: ${YELLOW}./update.sh --rollback${NC}"
echo ""
