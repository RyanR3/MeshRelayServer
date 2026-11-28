# MeshRelay Server

A high-performance Push-to-Talk relay server written in Go, designed to work with the MeshRelay Android app. Routes voice and text messages between clients over the internet.

## Quick Install (Ubuntu/Debian)

```bash
git clone https://github.com/RyanR3/MeshRelayServer.git
cd MeshRelayServer
sudo ./install.sh
```

`install.sh` builds the server from source. It uses the Go already on the machine when it is
at least the version on the `toolchain` line of `go.mod`; otherwise it downloads that Go
release from go.dev for this machine (checked against go.dev's published SHA-256) into
`/usr/local/lib/meshrelay-server/`, without touching any Go the system already has. The
machine needs internet access for that and for the Go modules.

Clone as your normal user, somewhere the service user `ptt` can reach. A home folder works:
where `ptt` cannot pass through a parent folder (Ubuntu's home folders are `750`),
`install.sh` grants `ptt` alone the right to pass through it with an ACL (installing the
`acl` package if needed) instead of opening the folder to everyone. `/opt/MeshRelayServer`
owned by your user works as well.

Dashboard: `http://your-server-ip:8080`
Login: `admin`, with the one-time password printed in the server log on first start
(`journalctl -u ptt-server | grep -A1 'INITIAL ADMIN PASSWORD'`). You set a new one at first sign-in.

### Update

As the user who cloned the repository:

```bash
cd MeshRelayServer
git pull
sudo ./install.sh
```

Your config, users, and agencies are preserved on update. The checkout stays yours; the
server's own files (`config.json`, `users.json`, `web_admins.json`, recordings, ...) and the
binary belong to `ptt`, and `install.sh` leaves them as they are.

**Installed with `install.sh` from v4.10.1 or earlier?** That version gave the whole folder
to `ptt`, so `git pull` as yourself fails ("dubious ownership"). Update once with:

```bash
cd MeshRelayServer
sudo -u ptt git pull
sudo ./install.sh
```

`install.sh` hands the checkout back to you (the server's files stay with `ptt`); after
that, the normal `git pull && sudo ./install.sh` works.

The dashboard's own updater (Settings) replaces the binary and `web/`. After using it,
`git status` shows the changed `web/` files; run `git checkout -- web` before the next
`git pull` (`install.sh` rebuilds the binary from the checkout anyway).

## Ways to run it

The same source runs three ways:

- **Standalone (above):** a systemd service, updated from the dashboard. The update check
  and the speech-to-text helper download use the public GitHub releases, no token needed.
  GitHub allows 60 unauthenticated API calls an hour per address, so the server asks at most
  once every 10 minutes and shares the answer between dashboards; a `github_token` left in an
  older `config.json` is ignored and removed at start-up.
- **Docker:** `docker compose up -d --build` with the included `Dockerfile` and
  `docker-compose.yml`. Non-root, read-only, data in the `meshrelay-data` volume.
  The dashboard listens on 127.0.0.1:8770 for a reverse proxy in front.
- **[infra-TAK](https://github.com/takwerx/infra-TAK):** installed and updated by infra-TAK's
  MeshRelay module. It runs the Docker image in managed mode (`MESHRELAY_MANAGED=infratak`):
  the dashboard's self-update is off, Caddy fronts the dashboard, sign-in can use Authentik
  (OpenID Connect), and the module links MeshRelay to the TAK Server on the same box.

Every setting can come from `MESHRELAY_*` environment variables (see `infratak.go`).

## Features

- **Real-time Audio Relay**: Low-latency audio packet forwarding
- **Multi-Channel Support**: 20 independent channels (matching Android app)
- **Text Messaging**: Relay text messages between clients
- **Web Dashboard**: Real-time monitoring and management
- **Client Management**: Automatic timeout and cleanup
- **Statistics Tracking**: Monitor packets, bandwidth, and connections
- **WebSocket Updates**: Live dashboard updates
- **TAK Server links**: each agency can connect its dispatch console to its own TAK Server
  (TAK users on the map, GeoChat both ways, emergencies both ways, LMR channel markers).
  A super admin can also set up one **master TAK server** for the whole server; agencies
  that switch on "Share with the master TAK server" (off by default) send their alerts,
  radio-channel markers and console messages to it and see its TAK users and alerts on
  their consoles. In TAK the master is one contact: a console's message goes out from it with
  the agency's name in front (`[Alpha] ...`), and MeshRelay remembers (in memory) which agencies
  wrote to which TAK user. A TAK user's message to that contact goes to every agency that wrote
  to that user in the last 12 hours and still shares; when none did, it goes to the **home
  agency** the super admin picks on the master settings (if none is picked, the agency
  `default`, otherwise the first agency). The home agency always takes part. When an agency's
  own link and the master go to the same TAK server, items are sent once and each TAK user is
  shown once. A console message to one TAK user is addressed to that user's UID, so the TAK
  Server delivers it to that user only.
- **Emergency alerts on TAK**: the server's 911 alert is placed at the unit's own position
  (from the alert, the phone's location reports, or the unit's TAK position), draws no line
  to the dispatch contact, names the unit, and is sent again every 4 minutes while the
  emergency lasts. When the unit's ATAK raises its own alert, the server does not add a
  second one. (The TAK Server itself repeats a 911 alert to its clients every 3 seconds;
  that is expected.)
- **One console entry per person**: a user signed in to MeshRelay whose ATAK also reports
  to TAK is listed once. Texts to them go to the MeshRelay app, or as ATAK chat for users of
  the MeshRelay ATAK plugin.

## Architecture

- **UDP Server** (Port 5000): Handles PTT client communication
- **Web Server** (Port 8080): Serves dashboard and REST API
- **WebSocket**: Real-time updates to web clients

## Manual Setup

If you prefer to set up manually instead of using `install.sh`:

```bash
# Install Go: at least the version on the `toolchain` line of go.mod, from
# https://go.dev/dl/ (distribution packages such as Ubuntu 22.04's golang-go
# are too old).

# Build
cd MeshRelayServer
go mod download
go build -ldflags "-X main.Version=$(git describe --tags --always)" -o ptt-server

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

### Behind a reverse proxy

To put the dashboard behind your own reverse proxy (Caddy, nginx) that terminates HTTPS,
add to `config.json`:

```json
{
  "web_bind": "127.0.0.1",
  "web_port": 8080,
  "trust_proxy": true
}
```

- `web_bind`: the address the dashboard listens on. Empty (the default) means every
  interface; `127.0.0.1` lets only the proxy on the same machine reach it. The PTT port
  (UDP) is not affected.
- `trust_proxy`: use the proxy's `X-Forwarded-For` (the last entry) as the browser's
  address, for the log and the sign-in limits, and its `X-Forwarded-Proto: https` to mark
  the session cookie `Secure`. The headers are used only on connections that come from a
  trusted proxy address; from anyone else they are ignored.
- `trusted_proxies`: the addresses or CIDRs the proxy connects from, for example
  `["10.0.0.5", "192.168.10.0/24"]`. Default: loopback only (`127.0.0.0/8`, `::1`).

Leave `tls_cert_file` / `tls_key_file` unset; the proxy does HTTPS. The proxy must pass
the `Host` header through unchanged and allow WebSocket upgrades (the dashboard checks that
a WebSocket's `Origin` matches its `Host`). Caddy does both by default:

```
meshrelay.example.org {
    reverse_proxy 127.0.0.1:8080
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name meshrelay.example.org;
    # ssl_certificate / ssl_certificate_key ...
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 1h;
    }
}
```

(nginx's `$proxy_add_x_forwarded_for` works too: the server reads the last entry, the one
the proxy added.) Restart the server after changing `config.json`.

### Where the server keeps its files

Relative paths in `config.json` (`users_file`, `web_admins_file`, `agencies_file`, ...),
`tls/`, `server_identity.key` and the activity log are all in the server's working directory
(`WorkingDirectory` of the service; the install folder with `install.sh`). These settings
move the rest:

- `recording_dir`: radio recordings (and their transcripts). Default `recordings` in the
  working directory. `recording_retention_days`: days to keep them (default 30).
- `transcribe_dir`: the speech-to-text helper, model and its settings. When unset:
  `transcribe/` next to the binary if that folder exists or the binary's folder is
  writable (the `install.sh` layout), otherwise `transcribe/` in the working directory.
  The folder in use is logged at start-up ("Transcription folder: ..."). Set it when the
  binary sits in a folder the service cannot write.
- `transcribe_helper_dir`: a folder of speech-to-text helper builds shipped with the server
  (named like the release assets, e.g. `mrtranscribe-linux-amd64`), used instead of
  downloading the helper from the GitHub release. The Docker image sets it.
- `web_dir`: the dashboard files. Default `web` in the working directory.
- `managed`: the name of a platform that installs and updates this server (for example
  `infratak`). The dashboard then does not offer its own update and says who updates the
  server instead.

Each can also come from a `MESHRELAY_*` environment variable (see `infratak.go`).

`adhoc_group_audio_tag` (default `false`): send ad-hoc group audio channel-tagged to members who are on another channel; leave it off until the phone apps play tagged audio for groups they have not selected.

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

Names that come from phones or from the dashboard (usernames, callsigns,
bridge and channel names) are written to the log in double quotes, Go
style: `Auth failed: unknown user "Engine 7" from 203.0.113.5:41000`. A
quote or line break inside a name shows as `\"` or `\n`, so every log entry
stays on one line and the name cannot pose as the address. An exact fail2ban
pattern: `Auth failed: (unknown user|wrong password for user) "(?:[^"\\]|\\.)*" from <HOST>`.

## Moving users to another server

Settings -> Move Users to Another Server (super admin only):

1. On the old server, choose all agencies or one, type a passphrase and
   download the export. The file is encrypted (scrypt + AES-256-GCM) and
   holds the agencies, the phone accounts with their passwords and phone keys,
   and the dashboard accounts.
2. On the new server, upload the file with the passphrase and press
   **Preview**. Nothing changes until you press **Import**. Existing accounts
   are kept unless you choose to replace them. Guests and dashboard accounts
   are only imported when you tick them. Copies of `agencies.json`,
   `users.json` and `web_admins.json` are saved next to them first
   (`*.before-import-<time>`).

Passwords and phone keys work on the new server as they are.

Phones remember the server's identity key for the address they connect to:
- **Same address (for example a DNS name moved to the new machine):** tick
  *Include this server's identity key* when exporting and *Use the server
  identity key from the file* when importing. The phones then connect without
  noticing. Otherwise they need a new QR code.
- **New address:** the phones need it anyway (QR code or settings), and they
  trust the new server on first contact.

Not moved, to set up again by hand:
- recordings and transcripts
- message history
- mutual-aid patches and requests
- guest events
- LMR and Zello bridges
- TAK connections, the master TAK server, and each agency's "Share with the master TAK
  server" switch (imported agencies start with it off)
- map layers
- `config.json` (TLS, single sign-on, ports)

## Server Requirements

| | Minimum | Recommended |
|---|---|---|
| **OS** | Ubuntu 22.04 / Debian 12 / Raspberry Pi OS 64-bit (Debian 12 based), x86-64 or ARM64 | Ubuntu 24.04 |
| **CPU** | 1 vCPU | 2 vCPU |
| **RAM** | 512 MB | 1 GB |
| **Disk** | 1 GB | 5 GB |
| **Network** | 1 Mbps up | 10 Mbps up |
| **Ports** | UDP 5000, TCP 8080 | |

`install.sh` needs a Debian-family system with systemd. Those are the systems it is tested
on; older releases may work but are not tested.

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
- `POST /api/admin/change-password` - Change your own dashboard password:
  `{"current_password": "...", "new_password": "..."}`. `current_password` may be left
  out only during the forced change at first sign-in. Ends the account's other sessions.

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
go build -ldflags "-X main.Version=$(git describe --tags --always)" -o ptt-server
```

The version comes from the git tag only; a build without `-X main.Version` reports `dev`.

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

Copyright (C) 2026 R3 Systems.

MeshRelay Server is free software: you can redistribute it and/or modify it under the
terms of the GNU Affero General Public License as published by the Free Software
Foundation, either version 3 of the License, or (at your option) any later version.
See [LICENSE](LICENSE). If you run a modified version for others over a network, the
AGPL requires you to offer them its source.

Third-party components and their licences are listed in
[THIRD_PARTY_LICENSES.txt](THIRD_PARTY_LICENSES.txt). The MeshRelay phone apps are
separate programs and are not covered by this licence.

## Support

For issues or questions, check the server logs:
```bash
journalctl -u ptt-server -f
```

Or run the server manually to see output:
```bash
./ptt-server
```
