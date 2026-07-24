# MeshRelay Server

A high-performance Push-to-Talk relay server written in Go, designed to work with the MeshRelay Android app. Routes voice and text messages between clients over the internet.

## Quick Install (Ubuntu/Debian)

```bash
git clone https://github.com/RyanR3/MeshRelayServer.git
cd MeshRelayServer
sudo ./install.sh
```

Dashboard: `http://your-server-ip:8080`
Default login: `admin` / `admin`

### Update

```bash
cd MeshRelayServer
git pull
sudo ./install.sh
```

Your config, users, and agencies are preserved on update.

## Features

- **Real-time Audio Relay**: Low-latency audio packet forwarding
- **Multi-Channel Support**: 20 independent channels (matching Android app)
- **Text Messaging**: Relay text messages between clients
- **Web Dashboard**: Real-time monitoring and management
- **Client Management**: Automatic timeout and cleanup
- **Statistics Tracking**: Monitor packets, bandwidth, and connections
- **WebSocket Updates**: Live dashboard updates

## Architecture

- **UDP Server** (Port 5000): Handles PTT client communication
- **Web Server** (Port 8080): Serves dashboard and REST API
- **WebSocket**: Real-time updates to web clients

## Manual Setup

If you prefer to set up manually instead of using `install.sh`:

```bash
# Install Go
sudo apt update && sudo apt install golang-go

# Build
cd MeshRelayServer
go mod download
go build -o ptt-server

# Run
./ptt-server
```

**Custom ports**: `./ptt-server -ptt-port=6000 -web-port=9090`

**Dashboard**: `http://localhost:8080`

## Configuration

Edit `config.json`:

```json
{
  "ptt_port": 5000,        // UDP port for PTT communication
  "web_port": 8080,        // HTTP port for web dashboard
  "max_clients": 1000,     // Maximum simultaneous clients
  "max_channels": 100,     // Maximum number of channels
  "log_level": "INFO"      // Logging level (DEBUG, INFO, WARN, ERROR)
}
```

## MCX SIP Gateway (optional, off by default)

The server can bridge to an external MCX / MCPTT platform (e.g. FirstNet PTT) via
a SIP gateway. It is **disabled by default and has zero impact when off** — enable
it only when you have a concrete interop requirement. When enabled it:

- listens for SIP (RFC 3261) on the configured port and REGISTERs toward the MCX platform,
- translates SIP call setup / floor control to/from MeshRelay floor packets, and
- passes RTP audio through (Step 1 already aligns MeshRelay audio to RTP, so only the 1-byte type prefix is stripped/added).

Add a `sip_gateway` block to `config.json`:

```json
{
  "sip_gateway": {
    "enabled": false,
    "sip_port": 5060,
    "mcx_platform_host": "mcx.example.com:5060",
    "mcx_domain": "example.com",
    "gateway_mcx_id": "gateway@example.com",
    "rtp_port_range": "20000-20100",
    "tls_enabled": true,
    "bridge_agency": "default",
    "bridge_channel": 10
  }
}
```

`bridge_agency` + `bridge_channel` select which MeshRelay channel is bridged to
the external MCX group. To test with a SIP softphone (Linphone/Zoiper), point it
at the gateway's `sip_port` and send an INVITE — it is accepted and translated to
a floor grant on the bridged channel.

> This is a **stub** (Step 5 of the MCX roadmap): it proves the translation layer
> and establishes the architecture. Full SIP dialog state, re-INVITE, RTCP, IMS
> registration, SIPS, and full 3GPP TS 24.380 compliance are deferred.

## Service Management

The install script sets up a systemd service automatically. Useful commands:

```bash
sudo systemctl status ptt-server    # Check status
sudo systemctl restart ptt-server   # Restart
sudo systemctl stop ptt-server      # Stop
journalctl -u ptt-server -f         # View logs
```

## Server Requirements

| | Minimum | Recommended |
|---|---|---|
| **OS** | Ubuntu 20.04 / Debian 11 | Ubuntu 22.04+ |
| **CPU** | 1 vCPU | 2 vCPU |
| **RAM** | 512 MB | 1 GB |
| **Disk** | 1 GB | 5 GB |
| **Network** | 1 Mbps up | 10 Mbps up |
| **Ports** | UDP 5000, TCP 8080 | |

The server is lightweight — a $4-6/mo VPS (DigitalOcean, Vultr, Linode) handles 100+ concurrent PTT users comfortably. Each connected client uses ~10 KB of memory. CPU usage stays under 5% at idle and scales with active audio traffic.

## Firewall Configuration

Open required ports:

```bash
# UFW (Ubuntu)
sudo ufw allow 5000/udp  # PTT communication
sudo ufw allow 8080/tcp  # Web dashboard

# iptables
sudo iptables -A INPUT -p udp --dport 5000 -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 8080 -j ACCEPT
```

## Android App Integration

### Update App to Use Server Mode

The Android app will need modifications to support server mode:

1. **Add Server Settings**: Configure server IP and port
2. **Replace Multicast**: Use UDP to server instead of multicast groups
3. **Join Protocol**: Send JOIN packet on connection
4. **Heartbeat**: Send periodic heartbeats to stay connected

### Connection Flow

1. **Client connects** → Sends JOIN packet with username and channel
2. **Server registers** client and adds to channel
3. **Client sends heartbeat** every 10 seconds
4. **Audio/text** relayed to all clients in same channel
5. **Client disconnects** or times out → Removed from channel

## API Endpoints

### REST API

- `GET /api/stats` - Server statistics
- `GET /api/clients` - List of connected clients
- `GET /api/channels` - Channel information
- `GET /api/config` - Current configuration
- `POST /api/config` - Update configuration

### WebSocket

- `WS /ws` - Real-time updates (sends stats, clients, channels every 2 seconds)

## Packet Protocol

All packets start with a type byte, followed by payload:

### Audio Packet (0x00)
```
[0x00] [username_len:int16] [username:bytes] [audio_len:int32] [audio_data:bytes]
```

### Text Message (0x01)
```
[0x01] [0x01] [sender_len:int16] [sender:bytes] [recipient_len:int16] [recipient:bytes]
[content_len:int16] [content:bytes] [timestamp:int64]
```

### Heartbeat (0x02)
```
[0x02] [username_len:int16] [username:bytes]
```

### Join Channel (0x03)
```
[0x03] [username_len:int16] [username:bytes] [channel:int32]
```

### Leave Channel (0x04)
```
[0x04]
```

## Performance

- **Latency**: <10ms relay time (local network)
- **Throughput**: Handles 1000+ concurrent clients
- **Memory**: ~10-50 MB base + ~10KB per client
- **CPU**: <5% on modern hardware (idle), scales with traffic

## Troubleshooting

### Server won't start

**Port already in use**:
```bash
# Check what's using the port
sudo lsof -i :5000
sudo lsof -i :8080

# Use different ports
./ptt-server -ptt-port=5001 -web-port=8081
```

### Clients can't connect

**Firewall blocking**:
```bash
# Check firewall status
sudo ufw status

# Open ports
sudo ufw allow 5000/udp
```

**Wrong server address**: Ensure clients point to correct server IP

### Dashboard shows no clients

1. Verify clients are sending JOIN packets
2. Check server logs for errors
3. Ensure UDP port 5000 is reachable

### High CPU usage

- Check number of connected clients
- Monitor packet rate in dashboard
- Consider increasing server resources

## Development

### Building from Source

```bash
go build -o ptt-server
```

### Running in Development

```bash
go run *.go
```

### Cross-Compilation

```bash
# For Linux
GOOS=linux GOARCH=amd64 go build -o ptt-server-linux

# For macOS
GOOS=darwin GOARCH=amd64 go build -o ptt-server-mac

# For Windows
GOOS=windows GOARCH=amd64 go build -o ptt-server.exe
```

## License

This project is provided as-is for use with the PTT Android application.

## Support

For issues or questions, check the server logs:
```bash
journalctl -u ptt-server -f
```

Or run the server manually to see output:
```bash
./ptt-server
```
