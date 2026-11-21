# PTT Server - Deployment Guide

Quick guide to deploy PTT Server on your Ubuntu Server VM.

## Step 1: Copy Files to Server

From your Mac, copy the entire PTTServer directory to your Ubuntu server:

```bash
# Replace with your server details
scp -r /Users/ryanfridlich/PTTServer user@your-ubuntu-server:/home/user/
```

## Step 2: Install Go on Ubuntu Server

SSH into your Ubuntu server and install Go:

```bash
ssh user@your-ubuntu-server

# Update packages
sudo apt update

# Install Go
sudo apt install golang-go

# Verify installation
go version
```

## Step 3: Build the Server

```bash
cd PTTServer

# Run the build script
./build.sh

# Or build manually
go mod download
go build -o ptt-server
```

## Step 4: Test the Server

Run the server to make sure it works:

```bash
./ptt-server
```

You should see:
```
=== PTT Server Starting ===
PTT Port: 5000
Web GUI: http://localhost:8080
Max Clients: 1000
Max Channels: 100
PTT server listening on UDP port 5000
Web GUI server starting on http://localhost:8080
```

Press `Ctrl+C` to stop it.

## Step 5: Set Up as a System Service

Create the systemd service file:

```bash
sudo nano /etc/systemd/system/ptt-server.service
```

Paste this content (update paths if needed):

```ini
[Unit]
Description=PTT Relay Server
After=network.target

[Service]
Type=simple
User=YOUR_USERNAME
WorkingDirectory=/home/YOUR_USERNAME/PTTServer
ExecStart=/home/YOUR_USERNAME/PTTServer/ptt-server
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

**Replace YOUR_USERNAME with your actual username!**

Enable and start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable ptt-server
sudo systemctl start ptt-server
```

Check status:

```bash
sudo systemctl status ptt-server
```

View logs:

```bash
journalctl -u ptt-server -f
```

## Step 6: Configure Firewall

Open the required ports:

```bash
# If using UFW
sudo ufw allow 5000/udp comment 'PTT Server'
sudo ufw allow 8080/tcp comment 'PTT Dashboard'

# If using iptables
sudo iptables -A INPUT -p udp --dport 5000 -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 8080 -j ACCEPT
sudo iptables-save | sudo tee /etc/iptables/rules.v4
```

## Step 7: Access the Dashboard

From your web browser, navigate to:

```
http://YOUR_SERVER_IP:8080
```

You should see the PTT Server dashboard!

## Step 8: Update Android App (Future)

To use the server with your Android app, you'll need to modify the app to:

1. Connect to your server IP instead of using multicast
2. Send JOIN packets when connecting
3. Send HEARTBEAT packets periodically
4. Route all audio/text through the server

This will require updating the Android app code.

## Troubleshooting

### Server won't start

Check logs:
```bash
journalctl -u ptt-server -n 50
```

### Can't access dashboard

1. Check if service is running: `sudo systemctl status ptt-server`
2. Check if port is open: `sudo ufw status`
3. Try accessing from server itself: `curl http://localhost:8080`

### Firewall issues

Check if ports are open:
```bash
sudo netstat -tulpn | grep 5000
sudo netstat -tulpn | grep 8080
```

## Server Management Commands

```bash
# Start server
sudo systemctl start ptt-server

# Stop server
sudo systemctl stop ptt-server

# Restart server
sudo systemctl restart ptt-server

# View status
sudo systemctl status ptt-server

# View logs (live)
journalctl -u ptt-server -f

# View last 100 log lines
journalctl -u ptt-server -n 100
```

## Next Steps

1. **Test the server** - Verify dashboard is accessible
2. **Monitor logs** - Watch for any errors
3. **Plan Android app updates** - To support server mode
4. **Consider security** - Sign-in is always required; before exposing the server to the internet, use strong passwords and put the dashboard behind HTTPS

---

For detailed information, see [README.md](README.md)
