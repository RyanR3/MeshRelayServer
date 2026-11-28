# Quick Start Checklist

Follow these steps to get your PTT Server running on Ubuntu.

## ☐ Step 1: Copy to Server

```bash
scp -r /path/to/PTTServer user@YOUR_SERVER_IP:/home/user/
```

## ☐ Step 2: SSH into Server

```bash
ssh user@YOUR_SERVER_IP
```

## ☐ Step 3: Install Go

`sudo ./install.sh` does steps 3 to 7 for you, including Go. To do it by hand, install at
least the Go version on the `toolchain` line of `go.mod` from https://go.dev/dl/ (the
`golang-go` package of Ubuntu 22.04 or Debian 12 is too old), then check:

```bash
go version
```

## ☐ Step 4: Build Server

```bash
cd PTTServer
./build.sh
```

## ☐ Step 5: Test Run

```bash
./ptt-server
```

Should see:
```
=== PTT Server Starting ===
PTT Port: 5000
Web GUI: http://localhost:8080
```

Press Ctrl+C to stop.

## ☐ Step 6: Open Firewall

```bash
sudo ufw allow 5000/udp
sudo ufw allow 8080/tcp
```

## ☐ Step 7: Install as Service

```bash
# Edit service file with your username
sudo nano /etc/systemd/system/ptt-server.service
```

Paste content from `ptt-server.service` file (update YOUR_USERNAME first!)

```bash
sudo systemctl daemon-reload
sudo systemctl enable ptt-server
sudo systemctl start ptt-server
sudo systemctl status ptt-server
```

## ☐ Step 8: Access Dashboard

Open browser to:
```
http://YOUR_SERVER_IP:8080
```

## ☐ Done!

Server is running! Check [README.md](README.md) for full documentation.

### Monitor Logs

```bash
journalctl -u ptt-server -f
```

### Server Commands

```bash
sudo systemctl start ptt-server    # Start
sudo systemctl stop ptt-server     # Stop
sudo systemctl restart ptt-server  # Restart
sudo systemctl status ptt-server   # Status
```

---

**Need help?** See [DEPLOYMENT.md](DEPLOYMENT.md) for detailed instructions.
