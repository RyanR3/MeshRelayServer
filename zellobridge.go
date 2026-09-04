package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// ZelloBridge configures a bridge between a MeshRelay agency/channel and a
// Zello Work channel. Configured entirely from the dashboard GUI (super_admin).
//
// PHASE 1: connect + logon + join channel, report live status. No audio yet.
// PHASE 2 (later): bidirectional Opus transcoded audio (requires libopus/cgo).
type ZelloBridge struct {
	ID   string `json:"id"`
	Name string `json:"name"` // friendly label for the dashboard

	// --- Zello Work side (connection) ---
	Network   string `json:"network"`    // Zello Work subdomain, e.g. "myorg" (=> wss://myorg.zellowork.com/ws)
	Username  string `json:"username"`   // bot account username
	Password  string `json:"password"`   // bot account password
	AuthToken string `json:"auth_token"` // API key from Zello Work admin console

	// --- Channel mappings: one login can bridge SEVERAL Zello channels, each to
	// its own MeshRelay channel IN ITS OWN AGENCY. Legacy single
	// ZelloChannel/MeshChannel fields are kept for migration and mirror
	// Mappings[0].
	//
	// AgencyID is the bridge-level DEFAULT agency: it's used for mappings that
	// don't set their own AgencyID (backward compatibility — every existing
	// bridge's mappings inherit it). New mappings may each target any agency.
	AgencyID     string         `json:"agency_id"`               // default agency for mappings that omit one
	Mappings     []ZelloMapping `json:"mappings"`                // zello_channel <-> mesh_channel pairs (each with its own agency)
	ZelloChannel string         `json:"zello_channel,omitempty"` // legacy single mapping
	MeshChannel  int            `json:"mesh_channel,omitempty"`  // legacy single mapping

	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`

	// --- Runtime status (not persisted) ---
	Status    string `json:"status,omitempty"`     // "disconnected" | "connecting" | "connected" | "error"
	LastError string `json:"last_error,omitempty"` // last connection error, if any

	// runtime internals
	conn    *websocket.Conn `json:"-"`
	stopCh  chan struct{}   `json:"-"`
	running int32           `json:"-"`
	seqID   uint32          `json:"-"`

	// --- audio bridge runtime (Phase 2) ---
	announced bool       `json:"-"` // whether channels have been announced to clients
	writeMu   sync.Mutex `json:"-"` // serialize WS writes (JSON + binary audio)
	statusMu  sync.Mutex `json:"-"`
	// inStreams maps a Zello stream_id -> the Zello channel name it belongs to,
	// so inbound binary frames route to the right mapping (populated on
	// on_stream_start).
	inStreams   map[int64]string `json:"-"`
	inStreamsMu sync.Mutex       `json:"-"`

	// Rate-limiting state for the inbound-audio diagnostics (logDrop/logRelay).
	logMu          sync.Mutex `json:"-"`
	lastDropLog    time.Time  `json:"-"`
	dropsSinceLog  int        `json:"-"`
	lastRelayLog   time.Time  `json:"-"`
	framesSinceLog int        `json:"-"`
	pendingOutIdx  int        `json:"-"` // mapping index whose start_stream is awaiting a stream_id
}

// ZelloMapping is one Zello-channel <-> MeshRelay-channel pair within a bridge,
// targeting a specific MeshRelay agency (so one bridge can fan out to several
// agencies — one per mapping).
type ZelloMapping struct {
	ZelloChannel string `json:"zello_channel"`
	MeshChannel  int    `json:"mesh_channel"`
	AgencyID     string `json:"agency_id,omitempty"` // primary target agency; empty = bridge default

	// ExtraAgencies lets ONE Zello channel be shared with several MeshRelay
	// agencies at once. The primary AgencyID above still owns the mapping (and
	// its mesh channel number); these are additional agencies that receive the
	// same audio and get the same channel announced to them.
	ExtraAgencies []string `json:"extra_agencies,omitempty"`

	// per-mapping outbound stream state (Mesh -> Zello for THIS zello channel)
	outStreamID int64 `json:"-"`
	outPacketID int64 `json:"-"`
	outLastSend int64 `json:"-"`
}

// mappings returns the effective mapping list, migrating a legacy single
// ZelloChannel/MeshChannel into Mappings on first use and backfilling each
// mapping's AgencyID from the bridge default when unset (so old bridges keep
// working and every mapping always has a concrete agency).
func (b *ZelloBridge) effectiveMappings() []ZelloMapping {
	if len(b.Mappings) == 0 && b.ZelloChannel != "" {
		b.Mappings = []ZelloMapping{{ZelloChannel: b.ZelloChannel, MeshChannel: b.MeshChannel, AgencyID: b.AgencyID}}
	}
	for i := range b.Mappings {
		if b.Mappings[i].AgencyID == "" {
			b.Mappings[i].AgencyID = b.AgencyID
		}
	}
	return b.Mappings
}

// targetAgencies returns every agency this mapping feeds: the primary first,
// then any extras, deduped and with empties dropped. Audio fan-out, channel
// announcement, and removal all iterate this so a shared channel behaves
// identically in every agency it reaches.
func (m ZelloMapping) targetAgencies() []string {
	out := make([]string, 0, 1+len(m.ExtraAgencies))
	seen := map[string]bool{}
	if m.AgencyID != "" {
		out = append(out, m.AgencyID)
		seen[m.AgencyID] = true
	}
	for _, a := range m.ExtraAgencies {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// servesAgency reports whether this mapping feeds the given agency, whether as
// its primary or as one of the shared extras.
func (m ZelloMapping) servesAgency(agencyID string) bool {
	if m.AgencyID == agencyID {
		return true
	}
	for _, a := range m.ExtraAgencies {
		if a == agencyID {
			return true
		}
	}
	return false
}

// zelloBridgePublic is the JSON shape returned to the dashboard — never leaks
// the password or full auth token.
type zelloBridgePublic struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Network   string         `json:"network"`
	Username  string         `json:"username"`
	HasAuth   bool           `json:"has_auth"` // whether a token/password is set (without revealing it)
	AgencyID  string         `json:"agency_id"`
	Mappings  []ZelloMapping `json:"mappings"` // zello_channel <-> mesh_channel pairs
	Enabled   bool           `json:"enabled"`
	Status    string         `json:"status"`
	LastError string         `json:"last_error,omitempty"`
	CreatedAt string         `json:"created_at"`
	CreatedBy string         `json:"created_by"`
}

func (b *ZelloBridge) public() zelloBridgePublic {
	b.statusMu.Lock()
	status, lastErr := b.Status, b.LastError
	b.statusMu.Unlock()
	if status == "" {
		status = "disconnected"
	}
	return zelloBridgePublic{
		ID: b.ID, Name: b.Name, Network: b.Network,
		Username: b.Username, HasAuth: b.AuthToken != "" || b.Password != "",
		AgencyID: b.AgencyID, Mappings: b.effectiveMappings(), Enabled: b.Enabled,
		Status: status, LastError: lastErr, CreatedAt: b.CreatedAt, CreatedBy: b.CreatedBy,
	}
}

func (b *ZelloBridge) setStatus(status, lastErr string) {
	b.statusMu.Lock()
	b.Status = status
	b.LastError = lastErr
	b.statusMu.Unlock()
}

// ===== Persistence (mirrors saveMutualAid/loadMutualAid) =====

func (s *Server) loadZelloBridges() {
	path := s.config.ZelloBridgesFile
	if path == "" {
		path = "zello_bridges.json"
	}
	file, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Warning: failed to open zello bridges file: %v", err)
		}
		return
	}
	defer file.Close()

	var bridges []*ZelloBridge
	if err := json.NewDecoder(file).Decode(&bridges); err != nil {
		log.Printf("Warning: failed to decode zello bridges: %v", err)
		return
	}
	s.mu.Lock()
	for _, b := range bridges {
		b.setStatus("disconnected", "")
		s.zelloBridges[b.ID] = b
	}
	s.mu.Unlock()
	log.Printf("Loaded %d Zello bridge(s)", len(bridges))
}

// saveZelloBridges persists all bridges. Caller must NOT hold s.mu (it RLocks).
func (s *Server) saveZelloBridges() error {
	path := s.config.ZelloBridgesFile
	if path == "" {
		path = "zello_bridges.json"
	}
	s.mu.RLock()
	bridges := make([]*ZelloBridge, 0, len(s.zelloBridges))
	for _, b := range s.zelloBridges {
		bridges = append(bridges, b)
	}
	s.mu.RUnlock()

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	return enc.Encode(bridges)
}

// StartEnabledZelloBridges launches runtime goroutines for every enabled bridge.
// Called once at startup after loadZelloBridges.
func (s *Server) StartEnabledZelloBridges() {
	s.mu.RLock()
	toStart := make([]*ZelloBridge, 0)
	for _, b := range s.zelloBridges {
		if b.Enabled {
			toStart = append(toStart, b)
		}
	}
	s.mu.RUnlock()
	for _, b := range toStart {
		s.startZelloBridge(b)
	}
}

// startZelloBridge begins the connect/reconnect loop for one bridge.
func (s *Server) startZelloBridge(b *ZelloBridge) {
	if !atomic.CompareAndSwapInt32(&b.running, 0, 1) {
		return // already running
	}
	b.stopCh = make(chan struct{})
	go s.runZelloBridge(b)
}

// stopZelloBridge stops the bridge and tells clients its channels are gone.
func (s *Server) stopZelloBridge(b *ZelloBridge) {
	s.stopZelloBridgeInternal(b, true)
}

// stopZelloBridgeForRestart stops the runtime WITHOUT un-announcing its
// channels. Used when a restart follows immediately (an edit or a remap): the
// blanket un-announce made every bridged channel vanish from clients and come
// back a moment later, which looks like a glitch on the phone. Channels that
// genuinely went away are removed separately by announceChannelRemoval.
func (s *Server) stopZelloBridgeForRestart(b *ZelloBridge) {
	s.stopZelloBridgeInternal(b, false)
}

func (s *Server) stopZelloBridgeInternal(b *ZelloBridge, unannounce bool) {
	if atomic.LoadInt32(&b.running) == 0 {
		// Even if not "running" (e.g. stopped after a kick), make sure any lingering
		// socket is closed so a subsequent start doesn't self-kick.
		if b.conn != nil {
			b.conn.Close()
			b.conn = nil
		}
		return
	}
	atomic.StoreInt32(&b.running, 0)
	if b.stopCh != nil {
		close(b.stopCh)
	}
	// Actively close the WebSocket so the goroutine's blocking ReadMessage()
	// returns immediately — otherwise the old session stays open and the next
	// connect kicks it (a self-inflicted "kicked" on every edit/channel switch).
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
	// Tell clients the bridged channel is no longer active — unless a restart
	// is about to re-announce the very same channels.
	if b.announced {
		b.announced = false
		if unannounce {
			s.notifyZelloBridgeChannels(b, false)
		}
	}
	b.setStatus("disconnected", "")
	// Give Zello a moment to fully tear down the old session before any restart.
	time.Sleep(600 * time.Millisecond)
}

// runZelloBridge is the connect/reconnect loop. PHASE 1: logon + join channel,
// then hold the connection and read/discard incoming frames (status only).
func (s *Server) runZelloBridge(b *ZelloBridge) {
	backoff := 2 * time.Second
	for atomic.LoadInt32(&b.running) == 1 {
		select {
		case <-b.stopCh:
			return
		default:
		}

		b.setStatus("connecting", "")
		err := s.zelloConnectAndServe(b)
		if err != nil {
			// A dropped/timed-out socket is transient — show "connecting" (we'll
			// reconnect) rather than a red "error", which is for real failures
			// like bad auth. Genuine logon rejections already set status=error
			// and stop the loop before returning here.
			msg := err.Error()
			if strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "closed network") ||
				strings.Contains(msg, "EOF") || strings.Contains(msg, "read:") {
				b.setStatus("connecting", "")
			} else {
				b.setStatus("error", msg)
			}
			log.Printf("Zello bridge %s (%s): %v", b.ID, b.Name, err)
		} else {
			b.setStatus("disconnected", "")
		}

		// Reconnect with backoff unless stopped.
		select {
		case <-b.stopCh:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// zelloConnectAndServe opens the WebSocket, logs on, joins the channel, and
// blocks reading until the connection drops or the bridge is stopped.
func (s *Server) zelloConnectAndServe(b *ZelloBridge) error {
	if b.Network == "" {
		return fmt.Errorf("network not set")
	}
	if b.AuthToken == "" && b.Username == "" {
		return fmt.Errorf("set either an API key or a username/password")
	}

	// Zello Work WebSocket endpoint. Per the working ZelloATAKPlugin, the
	// correct endpoint is wss://zellowork.io/ws/<networkName> — NOT
	// wss://<network>.zellowork.com/ws. Accept either the bare network name or
	// the full ...zellowork.com host and normalize to just the network name.
	network := strings.TrimSpace(b.Network)
	network = strings.TrimSuffix(network, "/")
	network = strings.TrimPrefix(network, "https://")
	network = strings.TrimPrefix(network, "http://")
	network = strings.TrimSuffix(network, ".zellowork.com")
	wsURL := fmt.Sprintf("wss://zellowork.io/ws/%s", url.PathEscape(network))

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	defer conn.Close()
	b.conn = conn
	b.announced = false // re-announce after each (re)connect

	// logon command — mirrors the working ZelloATAKPlugin exactly:
	//  - use username/password OR auth_token (the API key), NEVER both
	//  - "channels" is an ARRAY, plus listen_for_channels:true
	b.seqID++
	logon := map[string]interface{}{
		"command":             "logon",
		"seq":                 b.seqID,
		"listen_for_channels": true,
	}
	if b.Username != "" {
		logon["username"] = b.Username
		logon["password"] = b.Password
	} else {
		// AuthToken here is the Zello Work API key (the plugin uses API_KEY here).
		logon["auth_token"] = b.AuthToken
	}
	// Subscribe to ALL mapped Zello channels in one logon.
	chans := []string{}
	for _, m := range b.effectiveMappings() {
		if m.ZelloChannel != "" {
			chans = append(chans, m.ZelloChannel)
		}
	}
	logon["channels"] = chans
	b.inStreams = make(map[int64]string)
	log.Printf("Zello bridge %s: logon (channels=%v, mode=%s)", b.Name, chans,
		map[bool]string{true: "user/pass", false: "api_key"}[b.Username != ""])
	if err := conn.WriteJSON(logon); err != nil {
		return fmt.Errorf("send logon: %w", err)
	}

	// Idle-stream closer: sends stop_stream to Zello after outbound audio pauses.
	idleTicker := time.NewTicker(200 * time.Millisecond)
	idleDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-idleDone:
				return
			case <-idleTicker.C:
				b.stopOutboundStreamIfIdle()
			}
		}
	}()
	defer func() {
		idleTicker.Stop()
		close(idleDone)
		for i := range b.Mappings {
			atomic.StoreInt64(&b.Mappings[i].outStreamID, 0)
		}
	}()

	// Keepalive: Zello sends nothing on a quiet channel, so without pings the
	// read deadline fires (i/o timeout) and we needlessly reconnect. Send a
	// WebSocket ping every 30s; a pong resets the read deadline. The read
	// deadline is generous (90s) so a momentary hiccup doesn't drop us.
	const readWindow = 90 * time.Second
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(readWindow))
		return nil
	})
	pingTicker := time.NewTicker(30 * time.Second)
	pingDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-pingDone:
				return
			case <-pingTicker.C:
				b.writeMu.Lock()
				if b.conn != nil {
					b.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
					if err := b.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
						// A failed keepalive ping means the write side is dead.
						// Close so the read loop errors and we reconnect, rather
						// than sitting in a half-open zombie (reads OK, writes hang).
						log.Printf("Zello bridge %s: keepalive ping failed: %v — forcing reconnect", b.Name, err)
						b.failOutboundWrite()
					}
				}
				b.writeMu.Unlock()
			}
		}
	}()
	defer func() { pingTicker.Stop(); close(pingDone) }()

	// Re-announce the bridged channel to clients periodically. A one-time
	// announcement can be missed if a client is mid-join, sitting on the channel,
	// or the bridge was just remapped — which forced a manual channel-toggle to
	// pick it up. Re-broadcasting every 15s makes clients reliably show the
	// bridged channel without any toggle.
	reannounceTicker := time.NewTicker(15 * time.Second)
	reannounceDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-reannounceDone:
				return
			case <-reannounceTicker.C:
				// Re-announce so clients that joined after connect still get the
				// bridged channels. Gate on having ANY effective mapping — NOT on
				// the legacy ZelloChannel field, which is cleared once per-channel
				// mappings are configured (that gate silently stopped re-announce
				// for every mapping-based bridge).
				if b.announced && len(b.effectiveMappings()) > 0 {
					s.notifyZelloBridgeChannels(b, true)
				}
			}
		}
	}()
	defer func() { reannounceTicker.Stop(); close(reannounceDone) }()

	// Read loop — handles logon status, channel status, and audio streams both
	// ways (Opus passthrough: Zello Opus frames <-> MeshRelay Opus frames).
	conn.SetReadDeadline(time.Now().Add(readWindow))
	for {
		select {
		case <-b.stopCh:
			return nil
		default:
		}
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		// Any inbound activity extends the read window.
		conn.SetReadDeadline(time.Now().Add(readWindow))
		if msgType == websocket.BinaryMessage {
			// Inbound audio frame: [type(1)][stream_id(4 BE)][packet_id(4 BE)][opus].
			s.handleZelloBinaryAudio(b, data)
			continue
		}
		if msgType != websocket.TextMessage {
			continue
		}
		log.Printf("Zello bridge %s RX: %s", b.Name, string(data))
		var msg map[string]interface{}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		cmd, _ := msg["command"].(string)
		errStr, _ := msg["error"].(string)

		// Distinguish a LOGON REJECTION (top-level {"error":...} with no command)
		// from a runtime error command ({"command":"on_error","error":...}).
		if errStr != "" && cmd == "" {
			// Logon was rejected (bad creds / channel). Fatal — surface and stop
			// reconnecting so we don't hammer Zello.
			b.setStatus("error", "Zello: "+errStr)
			atomic.StoreInt32(&b.running, 0)
			return fmt.Errorf("zello logon rejected: %s", errStr)
		}

		if cmd == "on_error" {
			// A runtime error command. "kicked" = another session logged in as
			// this same Zello user, OR our own reconnect kicked us. Do NOT loop-
			// reconnect (that starts a kick war). Stop and report.
			if errStr == "kicked" {
				b.setStatus("error", "Kicked — this Zello user is logged in elsewhere. Use a dedicated bot account not signed in on any app/device.")
				atomic.StoreInt32(&b.running, 0)
				log.Printf("Zello bridge %s: kicked (duplicate session) — stopping to avoid a reconnect loop", b.Name)
				return nil
			}
			b.setStatus("error", "Zello: "+errStr)
			log.Printf("Zello bridge %s on_error: %s", b.Name, errStr)
			continue
		}

		if success, ok := msg["success"].(bool); ok && success {
			// A success with a stream_id is the response to OUR start_stream —
			// assign it to whichever mapping's stream we just opened (tracked in
			// pendingOutStream, since the response doesn't name the channel).
			if sid := jsonInt(msg["stream_id"]); sid > 0 {
				b.assignOutStreamID(sid)
			} else {
				b.setStatus("connected", "")
				log.Printf("Zello bridge %s connected on %s", b.Name, b.Network)
			}
		}
		if cmd == "on_channel_status" {
			b.setStatus("connected", "")
			// Announce all mapped channels to clients once they come online.
			if !b.announced {
				b.announced = true
				s.notifyZelloBridgeChannels(b, true)
			}
		}

		// Inbound stream lifecycle. Record stream_id -> its Zello channel so the
		// following binary frames route to the right MeshRelay mapping.
		if cmd == "on_stream_start" {
			sid := jsonInt(msg["stream_id"])
			from, _ := msg["from"].(string)
			zch, _ := msg["channel"].(string)
			b.inStreamsMu.Lock()
			b.inStreams[sid] = zch
			b.inStreamsMu.Unlock()
			log.Printf("Zello bridge %s: inbound stream %d from %q on channel %q", b.Name, sid, from, zch)
		}
		if cmd == "on_stream_stop" {
			sid := jsonInt(msg["stream_id"])
			b.inStreamsMu.Lock()
			delete(b.inStreams, sid)
			b.inStreamsMu.Unlock()
		}
	}
}

// assignOutStreamID assigns a Zello-confirmed stream id to the mapping whose
// start_stream is pending (set in sendAudioToZello before the request).
func (b *ZelloBridge) assignOutStreamID(sid int64) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.pendingOutIdx >= 0 && b.pendingOutIdx < len(b.Mappings) {
		atomic.StoreInt64(&b.Mappings[b.pendingOutIdx].outStreamID, sid)
		b.pendingOutIdx = -1
	}
}

// jsonInt coerces a JSON number (float64) or string to int64.
func jsonInt(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case string:
		i, _ := strconvParseInt(n)
		return i
	}
	return 0
}

func strconvParseInt(s string) (int64, error) {
	var x int64
	_, err := fmt.Sscan(s, &x)
	return x, err
}

// handleZelloBinaryAudio relays an inbound Zello audio frame into the bridged
// MeshRelay channel. Zello frame: [type(1)][stream_id(4 BE)][packet_id(4 BE)][opus].
// Both sides are Opus 16kHz, so we pass the Opus bytes straight through, wrapped
// in a MeshRelay Opus AudioPacket (sender = "Zello:<channel>").
func (s *Server) handleZelloBinaryAudio(b *ZelloBridge, frame []byte) {
	// Every early return below is a reason inbound Zello audio silently stops.
	// They are logged (rate-limited) because a bridge that "just doesn't work"
	// is otherwise indistinguishable from one receiving nothing at all.
	if len(frame) < 9 {
		b.logDrop("short frame (%d bytes)", len(frame))
		return
	}
	streamID := int64(binary.BigEndian.Uint32(frame[1:5]))
	// Route by the Zello channel this stream belongs to → the mapped mesh channel.
	b.inStreamsMu.Lock()
	zch := b.inStreams[streamID]
	b.inStreamsMu.Unlock()
	if zch == "" {
		b.logDrop("unknown stream %d (missed on_stream_start)", streamID)
		return
	}
	meshCh := -1
	var meshAgencies []string
	for _, m := range b.effectiveMappings() {
		if m.ZelloChannel == zch {
			meshCh = m.MeshChannel
			meshAgencies = m.targetAgencies() // primary + any shared agencies
			break
		}
	}
	if meshCh < 0 || len(meshAgencies) == 0 {
		b.logDrop("no mapping for Zello channel %q (mapped=%d)", zch, len(b.effectiveMappings()))
		return
	}
	opus := frame[9:]
	if len(opus) == 0 {
		b.logDrop("empty opus payload on %q", zch)
		return
	}

	sender := "Zello:" + zch
	pkt := &AudioPacket{Username: sender, AudioData: opus, Codec: CodecOpus}
	audioData := append([]byte{PacketTypeAudioOpus}, SerializeAudioPacket(pkt)...)

	// Deliver into EVERY agency this Zello channel is shared with. sentTo is
	// shared across all of them so a client reachable two ways (e.g. via its own
	// agency and a mutual-aid partner) still receives exactly one copy.
	sentTo := make(map[string]bool)
	for _, meshAgency := range meshAgencies {
		s.mu.RLock()
		ch := s.channels[channelKey(meshAgency, meshCh)]
		s.mu.RUnlock()
		if ch != nil {
			ch.mu.RLock()
			for _, c := range ch.Clients {
				if !sentTo[c.Addr.String()] {
					s.sendPacket(audioData, c.Addr)
					sentTo[c.Addr.String()] = true
				}
			}
			ch.mu.RUnlock()
		}

		// Also send to clients MONITORING this channel (always-listen) but tuned
		// elsewhere — this is what makes the monitor toggle hear Zello audio.
		s.mu.RLock()
		for _, mc := range s.clients {
			if mc.AgencyID != meshAgency || sentTo[mc.Addr.String()] {
				continue
			}
			for _, mch := range mc.MonitoredChannels {
				if mch == meshCh {
					s.sendPacket(audioData, mc.Addr)
					sentTo[mc.Addr.String()] = true
					break
				}
			}
		}
		s.mu.RUnlock()

		for _, partner := range s.getMutualAidPartners(meshAgency, meshCh) {
			s.mu.RLock()
			pc := s.channels[channelKey(partner, meshCh)]
			s.mu.RUnlock()
			if pc != nil {
				pc.mu.RLock()
				for _, c := range pc.Clients {
					if !sentTo[c.Addr.String()] {
						s.sendPacket(audioData, c.Addr)
						sentTo[c.Addr.String()] = true
					}
				}
				pc.mu.RUnlock()
			}
		}
	}

	// Zero recipients means the audio arrived and was mapped, but no client is
	// on (or monitoring) the channel — a different problem from not receiving.
	agencyLabel := strings.Join(meshAgencies, "+")
	if len(sentTo) == 0 {
		b.logDrop("%q -> ch %d [%s]: no listeners", zch, meshCh, agencyLabel)
	} else {
		b.logRelay(zch, meshCh, agencyLabel, len(sentTo))
	}
}

// logDrop reports why inbound Zello audio was discarded, at most once every 5s
// per bridge so a stuck stream can't flood the log at 50 frames/second.
func (b *ZelloBridge) logDrop(format string, args ...interface{}) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	now := time.Now()
	if now.Sub(b.lastDropLog) < 5*time.Second {
		b.dropsSinceLog++
		return
	}
	suffix := ""
	if b.dropsSinceLog > 0 {
		suffix = fmt.Sprintf(" (+%d more since last)", b.dropsSinceLog)
	}
	log.Printf("Zello bridge %q: dropped audio — "+format+"%s",
		append(append([]interface{}{b.Name}, args...), suffix)...)
	b.lastDropLog = now
	b.dropsSinceLog = 0
}

// logRelay confirms inbound audio is flowing, once every 5s per bridge.
func (b *ZelloBridge) logRelay(zch string, meshCh int, agency string, listeners int) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	now := time.Now()
	if now.Sub(b.lastRelayLog) < 5*time.Second {
		b.framesSinceLog++
		return
	}
	log.Printf("Zello bridge %q: %q -> ch %d [%s], %d listener(s), %d frames in last window",
		b.Name, zch, meshCh, agency, listeners, b.framesSinceLog+1)
	b.lastRelayLog = now
	b.framesSinceLog = 0
}

// ===== Outbound: MeshRelay -> Zello =====
//
// Called from handleAudioPacket for every Opus frame on a bridged channel. We
// manage a Zello stream lifecycle: start_stream on the first frame, binary Opus
// frames while audio flows, and stop_stream after a short gap. Opus passthrough
// (both sides 16kHz), so the frame bytes are forwarded unchanged.
func (s *Server) forwardMeshAudioToZello(agencyID string, channel int, sender string, codec int, opus []byte) {
	if len(opus) == 0 {
		return
	}
	if codec != CodecOpus {
		// Diagnostic: a bridge exists but the audio isn't Opus — log once-ish.
		s.mu.RLock()
		hasBridge := false
		for _, b := range s.zelloBridges {
			if !b.Enabled {
				continue
			}
			for _, m := range b.effectiveMappings() {
				if m.servesAgency(agencyID) && m.MeshChannel == channel {
					hasBridge = true
					break
				}
			}
			if hasBridge {
				break
			}
		}
		s.mu.RUnlock()
		if hasBridge && zelloFwdLog < 3 {
			log.Printf("Zello fwd SKIP: audio on bridged %s/ch%d is codec=%d (need Opus=%d) — client must send Opus", agencyID, channel, codec, CodecOpus)
			zelloFwdLog++
		}
		return
	}
	// Find an enabled, connected bridge with a mapping to THIS agency+mesh
	// channel (a mapping's own agency, not the bridge default).
	s.mu.RLock()
	var target *ZelloBridge
	mapIdx := -1
	for _, b := range s.zelloBridges {
		if b.Enabled && atomic.LoadInt32(&b.running) == 1 {
			for i, m := range b.effectiveMappings() {
				// A shared mapping forwards from ANY of its agencies, so a
				// client in an extra agency can talk to Zello too.
				if m.servesAgency(agencyID) && m.MeshChannel == channel {
					target, mapIdx = b, i
					break
				}
			}
		}
		if target != nil {
			break
		}
	}
	s.mu.RUnlock()
	if target == nil || target.conn == nil {
		return
	}
	// Don't echo Zello-originated audio back to Zello.
	if strings.HasPrefix(sender, "Zello:") {
		return
	}
	target.sendAudioToZello(mapIdx, opus)
}

var zelloFwdLog int

// sendAudioToZello starts a Zello stream for mapping[idx] if needed and sends
// one Opus frame to that mapping's Zello channel.
func (b *ZelloBridge) sendAudioToZello(idx int, opus []byte) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.conn == nil || idx < 0 || idx >= len(b.Mappings) {
		return
	}
	m := &b.Mappings[idx]

	// Start a new stream if none active for this mapping.
	if atomic.LoadInt64(&m.outStreamID) == 0 {
		b.seqID++
		hdr := []byte{0x80, 0x3E, 0x01, 0x14} // 16000 Hz LE, 1 frame, 20ms
		start := map[string]interface{}{
			"command":         "start_stream",
			"seq":             b.seqID,
			"channel":         m.ZelloChannel,
			"type":            "audio",
			"codec":           "opus",
			"codec_header":    base64.StdEncoding.EncodeToString(hdr),
			"packet_duration": 20,
		}
		// Bound the write: a stale socket must fail fast, not hang for the
		// OS TCP timeout. On failure, force a reconnect (see failOutboundWrite).
		b.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := b.conn.WriteJSON(start); err != nil {
			log.Printf("Zello bridge %s: start_stream failed: %v — forcing reconnect", b.Name, err)
			b.failOutboundWrite()
			return
		}
		b.pendingOutIdx = idx                // the success response's stream_id is for this mapping
		atomic.StoreInt64(&m.outStreamID, 1) // temp until Zello confirms
		atomic.StoreInt64(&m.outPacketID, 0)
		log.Printf("Zello bridge %s: started outbound stream -> %s", b.Name, m.ZelloChannel)
	}

	// Binary frame: [0x01][stream_id(4 BE)][packet_id(4 BE)][opus].
	sid := atomic.LoadInt64(&m.outStreamID)
	pid := atomic.AddInt64(&m.outPacketID, 1)
	buf := make([]byte, 9+len(opus))
	buf[0] = 0x01
	binary.BigEndian.PutUint32(buf[1:5], uint32(sid))
	binary.BigEndian.PutUint32(buf[5:9], uint32(pid))
	copy(buf[9:], opus)
	b.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := b.conn.WriteMessage(websocket.BinaryMessage, buf); err != nil {
		log.Printf("Zello bridge %s: audio send failed: %v — forcing reconnect", b.Name, err)
		b.failOutboundWrite()
		return
	}
	atomic.StoreInt64(&m.outLastSend, timeNowNano())
}

// failOutboundWrite handles a failed write to the Zello WebSocket. A write that
// times out (i/o timeout) means the socket is half-dead: reads may still deliver
// inbound audio while writes hang forever. Close the conn so the read loop's
// ReadMessage errors out and runZelloBridge's reconnect loop rebuilds the
// session. Caller must hold b.writeMu. Also resets outbound stream state so the
// reconnected session starts fresh streams.
func (b *ZelloBridge) failOutboundWrite() {
	if b.conn != nil {
		b.conn.Close()
	}
	for i := range b.Mappings {
		atomic.StoreInt64(&b.Mappings[i].outStreamID, 0)
		atomic.StoreInt64(&b.Mappings[i].outPacketID, 0)
	}
}

// stopOutboundStreamIfIdle sends stop_stream for any mapping whose outbound audio
// has paused. Runs from a periodic ticker started per bridge.
func (b *ZelloBridge) stopOutboundStreamIfIdle() {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.conn == nil {
		return
	}
	now := timeNowNano()
	for i := range b.Mappings {
		m := &b.Mappings[i]
		if atomic.LoadInt64(&m.outStreamID) == 0 {
			continue
		}
		last := atomic.LoadInt64(&m.outLastSend)
		if last == 0 || now-last < int64(400*time.Millisecond) {
			continue
		}
		b.seqID++
		b.conn.WriteJSON(map[string]interface{}{
			"command":   "stop_stream",
			"seq":       b.seqID,
			"stream_id": atomic.LoadInt64(&m.outStreamID),
		})
		atomic.StoreInt64(&m.outStreamID, 0)
	}
}

func timeNowNano() int64 { return time.Now().UnixNano() }

// pickZelloBridgeChannel returns a free high channel slot (10-19), avoiding
// numbers already used by other bridges/mappings, ad-hoc groups, or active
// mutual-aid links.
// Caller must hold s.mu.
func (s *Server) pickZelloBridgeChannel() int {
	// Shared with ad-hoc groups and mutual aid — see usedHighChannels.
	used := s.usedHighChannels()
	for ch := 10; ch <= 19; ch++ {
		if !used[ch] {
			return ch
		}
	}
	return 19
}

// announceChannelRemoval tells all clients in an agency to remove a specific
// bridged channel (ChannelSwitch with active=false).
func (s *Server) announceChannelRemoval(agencyID string, meshChannel int, name string) {
	pkt := SerializeChannelSwitch(&ChannelSwitchPacket{
		Channel:     meshChannel,
		ChannelName: name,
		Active:      false,
	})
	s.mu.RLock()
	var addrs []*net.UDPAddr
	for _, c := range s.clients {
		if c.AgencyID == agencyID {
			addrs = append(addrs, c.Addr)
		}
	}
	s.mu.RUnlock()
	for _, a := range addrs {
		s.sendPacket(pkt, a)
	}
	log.Printf("Zello: removed channel %d (%s) from %d clients in %s", meshChannel, name, len(addrs), agencyID)
}

// notifyZelloBridgeChannels announces every mapped channel of a bridge to all
// clients in its agency (each as a selectable named channel, like mutual aid).
func (s *Server) notifyZelloBridgeChannels(b *ZelloBridge, active bool) {
	// Snapshot mappings under lock; each mapping announces to ITS OWN agency's
	// clients (a bridge can span multiple agencies).
	s.mu.RLock()
	maps := b.effectiveMappings()
	s.mu.RUnlock()

	for _, m := range maps {
		name := m.ZelloChannel
		if name == "" {
			continue
		}
		// Announce into every agency this mapping is shared with, so the bridged
		// channel appears for all of them, not just the primary.
		for _, agency := range m.targetAgencies() {
			s.mu.RLock()
			var addrs []*net.UDPAddr
			for _, c := range s.clients {
				if c.AgencyID == agency {
					addrs = append(addrs, c.Addr)
				}
			}
			s.mu.RUnlock()

			pkt := SerializeChannelSwitch(&ChannelSwitchPacket{
				Channel:     m.MeshChannel,
				ChannelName: name,
				Active:      active,
			})
			for _, a := range addrs {
				s.sendPacket(pkt, a)
			}
			log.Printf("Zello bridge %s: announced channel %d as %q to %d clients in %s (active=%v)",
				b.Name, m.MeshChannel, name, len(addrs), agency, active)
		}
	}
}

// ===== HTTP handlers (super_admin) =====

func (s *Server) handleGetZelloBridges(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	out := make([]zelloBridgePublic, 0, len(s.zelloBridges))
	for _, b := range s.zelloBridges {
		out = append(out, b.public())
	}
	s.mu.RUnlock()
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleCreateZelloBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		Name         string `json:"name"`
		Network      string `json:"network"`
		ZelloChannel string `json:"zello_channel"`
		Username     string `json:"username"`
		Password     string `json:"password"`
		AuthToken    string `json:"auth_token"`
		AgencyID     string `json:"agency_id"`
		MeshChannel  int    `json:"mesh_channel"`
		Enabled      bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		zelloErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Name == "" || req.Network == "" || req.AgencyID == "" {
		// zello_channel is assigned on the card after connecting, so it's optional here.
		zelloErr(w, http.StatusBadRequest, "name, network, and agency_id are required")
		return
	}

	// Resolve the session BEFORE taking s.mu — getSession() itself RLocks s.mu,
	// and Go's RWMutex is not reentrant, so calling it while holding Lock()
	// deadlocks the whole server.
	createdBy := ""
	if sess := s.getSession(r); sess != nil {
		createdBy = sess.Username
	}

	s.mu.Lock()
	if _, ok := s.agencies[req.AgencyID]; !ok {
		s.mu.Unlock()
		zelloErr(w, http.StatusNotFound, "agency not found")
		return
	}
	idBytes := make([]byte, 8)
	rand.Read(idBytes)
	id := hex.EncodeToString(idBytes)
	// Bridged channels must live in the high range (10-19) so they appear as NEW
	// named channels on clients (like mutual aid), not collide with the client's
	// normal 0-9 channels (which requires a re-login to pick up).
	meshCh := req.MeshChannel
	if meshCh < 10 {
		meshCh = s.pickZelloBridgeChannel()
	}
	b := &ZelloBridge{
		ID: id, Name: req.Name, Network: req.Network, ZelloChannel: req.ZelloChannel,
		Username: req.Username, Password: req.Password, AuthToken: req.AuthToken,
		AgencyID: req.AgencyID, MeshChannel: meshCh, Enabled: req.Enabled,
		CreatedAt: time.Now().Format(time.RFC3339), CreatedBy: createdBy,
	}
	b.setStatus("disconnected", "")
	s.zelloBridges[id] = b
	s.mu.Unlock()

	if err := s.saveZelloBridges(); err != nil {
		log.Printf("Warning: failed to save zello bridges: %v", err)
	}
	if b.Enabled {
		s.startZelloBridge(b)
	}
	log.Printf("Created Zello bridge %s (%s): %s/%s <-> agency %s ch %d (by %s)",
		id, b.Name, b.Network, b.ZelloChannel, b.AgencyID, b.MeshChannel, createdBy)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "id": id})
}

func (s *Server) handleUpdateZelloBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	var req struct {
		Name         string `json:"name"`
		Network      string `json:"network"`
		ZelloChannel string `json:"zello_channel"`
		Username     string `json:"username"`
		Password     string `json:"password"`
		AuthToken    string `json:"auth_token"`
		AgencyID     string `json:"agency_id"`
		MeshChannel  int    `json:"mesh_channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		zelloErr(w, http.StatusBadRequest, "invalid request")
		return
	}

	s.mu.Lock()
	b, ok := s.zelloBridges[id]
	if !ok {
		s.mu.Unlock()
		zelloErr(w, http.StatusNotFound, "bridge not found")
		return
	}
	wasEnabled := b.Enabled
	b.Name = req.Name
	b.Network = req.Network
	b.ZelloChannel = req.ZelloChannel
	b.Username = req.Username
	if req.Password != "" { // empty = keep existing (so the GUI can omit it)
		b.Password = req.Password
	}
	if req.AuthToken != "" {
		b.AuthToken = req.AuthToken
	}
	b.AgencyID = req.AgencyID
	b.MeshChannel = req.MeshChannel
	s.mu.Unlock()

	// Restart the runtime if it was live, to pick up new settings. Same as the
	// remap path: don't un-announce, or the channel blinks out on every client
	// and comes straight back.
	if wasEnabled {
		s.stopZelloBridgeForRestart(b)
		s.startZelloBridge(b)
	}
	if err := s.saveZelloBridges(); err != nil {
		log.Printf("Warning: failed to save zello bridges: %v", err)
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleMapZelloBridge: PATCH just the channel mapping (zello_channel +
// mesh_channel) from the connected card, and restart to apply. Connection creds
// (network/user/pass/apikey) are untouched.
// handleMapZelloBridge replaces the bridge's full mapping list (the GUI sends
// the complete set each Apply). Zello channels with no valid mesh channel get a
// free high slot auto-assigned. Restarts the bridge to apply.
func (s *Server) handleMapZelloBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	var req struct {
		Mappings []ZelloMapping `json:"mappings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		zelloErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	s.mu.Lock()
	b, ok := s.zelloBridges[id]
	if !ok {
		s.mu.Unlock()
		zelloErr(w, http.StatusNotFound, "bridge not found")
		return
	}
	// Remember the OLD mappings so we can tell clients to remove any channel
	// that's no longer mapped (otherwise a deleted channel lingers on the client).
	oldMappings := append([]ZelloMapping(nil), b.effectiveMappings()...)

	// Build a clean mapping list: dedupe Zello channels, assign high mesh slots.
	// Mesh channel numbers are namespaced PER AGENCY (agency A ch10 and agency B
	// ch10 are distinct), so track used numbers per agency.
	seenZ := map[string]bool{}
	usedMesh := map[string]map[int]bool{}
	markUsed := func(agency string, ch int) {
		if usedMesh[agency] == nil {
			usedMesh[agency] = map[int]bool{}
		}
		usedMesh[agency][ch] = true
	}
	isUsed := func(agency string, ch int) bool {
		return usedMesh[agency] != nil && usedMesh[agency][ch]
	}
	// mesh numbers already used by OTHER bridges (per their mapping agencies).
	for _, ob := range s.zelloBridges {
		if ob.ID == id {
			continue
		}
		for _, m := range ob.effectiveMappings() {
			markUsed(m.AgencyID, m.MeshChannel)
		}
	}
	// mutual-aid links occupy a channel number within their agency.
	for _, l := range s.mutualAidLinks {
		if l.Active {
			markUsed(l.AgencyA, l.Channel)
			markUsed(l.AgencyB, l.Channel)
		}
	}
	pickHigh := func(agency string) int {
		for ch := 10; ch <= 19; ch++ {
			if !isUsed(agency, ch) {
				markUsed(agency, ch)
				return ch
			}
		}
		return 19
	}
	clean := make([]ZelloMapping, 0, len(req.Mappings))
	for _, m := range req.Mappings {
		if m.ZelloChannel == "" || seenZ[m.ZelloChannel] {
			continue
		}
		// Per-mapping agency: default to the bridge agency, and only accept a
		// valid existing agency (fall back to the default otherwise).
		agency := m.AgencyID
		if agency == "" {
			agency = b.AgencyID
		}
		if _, ok := s.agencies[agency]; !ok {
			agency = b.AgencyID
		}
		// Extra agencies this Zello channel is shared with. Keep only agencies
		// that exist and aren't the primary (which is already covered).
		var extras []string
		seenA := map[string]bool{agency: true}
		for _, ea := range m.ExtraAgencies {
			if ea == "" || seenA[ea] {
				continue
			}
			if _, ok := s.agencies[ea]; !ok {
				continue // unknown agency — drop rather than invent one
			}
			seenA[ea] = true
			extras = append(extras, ea)
		}
		seenZ[m.ZelloChannel] = true
		// The mesh channel number must be free in EVERY agency it will appear
		// in, since a shared mapping claims the same number in all of them.
		mesh := m.MeshChannel
		freeEverywhere := func(ch int) bool {
			if ch < 10 || ch > 19 {
				return false
			}
			if isUsed(agency, ch) {
				return false
			}
			for _, ea := range extras {
				if isUsed(ea, ch) {
					return false
				}
			}
			return true
		}
		if !freeEverywhere(mesh) {
			mesh = -1
			for ch := 10; ch <= 19; ch++ {
				if freeEverywhere(ch) {
					mesh = ch
					break
				}
			}
			if mesh < 0 {
				mesh = pickHigh(agency) // no slot free in all — fall back
			}
		}
		markUsed(agency, mesh)
		for _, ea := range extras {
			markUsed(ea, mesh)
		}
		clean = append(clean, ZelloMapping{
			ZelloChannel:  m.ZelloChannel,
			MeshChannel:   mesh,
			AgencyID:      agency,
			ExtraAgencies: extras,
		})
	}
	b.Mappings = clean
	b.ZelloChannel = "" // clear legacy single fields; mappings are authoritative now
	b.MeshChannel = 0
	enabled := b.Enabled
	s.mu.Unlock()

	// Tell clients to REMOVE any channel that was dropped from the mapping list.
	// Match on (agency, mesh channel) since mappings can now span agencies.
	type amKey struct {
		agency string
		mesh   int
	}
	newSet := map[amKey]bool{}
	for _, m := range clean {
		for _, a := range m.targetAgencies() {
			newSet[amKey{a, m.MeshChannel}] = true
		}
	}
	// Remove from every agency the old mapping reached — including shared ones
	// that were just un-shared, which would otherwise keep a dead channel.
	for _, om := range oldMappings {
		if om.ZelloChannel == "" {
			continue
		}
		for _, a := range om.targetAgencies() {
			if !newSet[amKey{a, om.MeshChannel}] {
				s.announceChannelRemoval(a, om.MeshChannel, om.ZelloChannel)
			}
		}
	}

	if enabled {
		s.stopZelloBridgeForRestart(b) // closes conn, waits; keeps channels announced
		s.startZelloBridge(b)
	}
	if err := s.saveZelloBridges(); err != nil {
		log.Printf("Warning: failed to save zello bridges: %v", err)
	}
	log.Printf("Zello bridge %s remapped: %d channel(s)", b.Name, len(clean))
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "mappings": clean})
}

// handleZelloBridgeChannels: GET the channel list for a SAVED bridge (reusing
// its stored creds), so the card's dropdown can populate without re-entering creds.
func (s *Server) handleZelloBridgeChannels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	s.mu.RLock()
	b, ok := s.zelloBridges[id]
	var network, apiKey, user, pass string
	if ok {
		network, apiKey, user, pass = b.Network, b.AuthToken, b.Username, b.Password
	}
	s.mu.RUnlock()
	if !ok {
		zelloErr(w, http.StatusNotFound, "bridge not found")
		return
	}
	channels, err := zelloFetchChannels(network, apiKey, user, pass)
	if err != nil {
		zelloErr(w, http.StatusBadGateway, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "channels": channels})
}

func (s *Server) handleToggleZelloBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	s.mu.Lock()
	b, ok := s.zelloBridges[id]
	if !ok {
		s.mu.Unlock()
		zelloErr(w, http.StatusNotFound, "bridge not found")
		return
	}
	b.Enabled = !b.Enabled
	enabled := b.Enabled
	s.mu.Unlock()

	if enabled {
		s.startZelloBridge(b)
	} else {
		s.stopZelloBridge(b)
	}
	if err := s.saveZelloBridges(); err != nil {
		log.Printf("Warning: failed to save zello bridges: %v", err)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "enabled": enabled})
}

func (s *Server) handleDeleteZelloBridge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := mux.Vars(r)["id"]
	s.mu.Lock()
	b, ok := s.zelloBridges[id]
	if !ok {
		s.mu.Unlock()
		zelloErr(w, http.StatusNotFound, "bridge not found")
		return
	}
	delete(s.zelloBridges, id)
	s.mu.Unlock()

	s.stopZelloBridge(b)
	if err := s.saveZelloBridges(); err != nil {
		log.Printf("Warning: failed to save zello bridges: %v", err)
	}
	log.Printf("Deleted Zello bridge %s (%s)", id, b.Name)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func zelloErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	w.Write([]byte(fmt.Sprintf(`{"error":%q}`, msg)))
}

// ===== Zello Work REST API: list channels =====
//
// Mirrors the working ZelloATAKPlugin flow:
//  1. GET  /user/gettoken            -> token, sid
//  2. POST /user/login?sid=<sid>     with md5(md5(pw)+token+apiKey)
//  3. GET  /channel/get?sid=<sid>    -> channel list
//
// Returns the channel names the account can use.
func zelloFetchChannels(network, apiKey, username, password string) ([]string, error) {
	network = strings.TrimSpace(network)
	network = strings.TrimPrefix(network, "https://")
	network = strings.TrimPrefix(network, "http://")
	network = strings.TrimSuffix(network, "/")
	network = strings.TrimSuffix(network, ".zellowork.com")
	base := "https://" + network + ".zellowork.com"

	client := &http.Client{Timeout: 12 * time.Second}
	getJSON := func(u string) (map[string]interface{}, error) {
		resp, err := client.Get(u)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var m map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			return nil, err
		}
		return m, nil
	}

	// Step 1: token + sid
	tok, err := getJSON(base + "/user/gettoken")
	if err != nil {
		return nil, fmt.Errorf("gettoken: %w", err)
	}
	if tok["status"] != "OK" {
		return nil, fmt.Errorf("gettoken failed: %v", tok["status"])
	}
	token, _ := tok["token"].(string)
	sid, _ := tok["sid"].(string)
	if token == "" || sid == "" {
		return nil, fmt.Errorf("gettoken returned no token/sid")
	}

	// Step 2: login with md5(md5(password) + token + apiKey)
	hashed := md5hex(md5hex(password) + token + apiKey)
	form := url.Values{}
	form.Set("username", username)
	form.Set("password", hashed)
	resp, err := client.PostForm(base+"/user/login?sid="+url.QueryEscape(sid), form)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	var loginJSON map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&loginJSON)
	resp.Body.Close()
	if loginJSON["status"] != "OK" {
		return nil, fmt.Errorf("login failed: %v (code %v)", loginJSON["status"], loginJSON["code"])
	}

	// Step 3: the logged-in user's OWN channels. The WebSocket logon only accepts
	// channels the user is actually a member of ("contact list"); /channel/get
	// returns ALL network channels, so we must filter to the user's membership —
	// otherwise a channel that merely exists (but the user isn't in) shows in the
	// dropdown yet fails the WebSocket logon with "not found in the contact list".
	myChannels := map[string]bool{}
	myJSON, err := getJSON(base + "/user/get/login/" + url.PathEscape(username) + "?sid=" + url.QueryEscape(sid))
	if err == nil {
		if users, ok := myJSON["users"].([]interface{}); ok && len(users) > 0 {
			if u0, ok := users[0].(map[string]interface{}); ok {
				if chs, ok := u0["channels"].([]interface{}); ok {
					for _, c := range chs {
						if n, ok := c.(string); ok {
							myChannels[n] = true
						}
					}
				}
			}
		}
	}

	// Step 4: all channels, filtered to the user's membership.
	chJSON, err := getJSON(base + "/channel/get?sid=" + url.QueryEscape(sid))
	if err != nil {
		return nil, fmt.Errorf("channel/get: %w", err)
	}
	names := []string{}
	if arr, ok := chJSON["channels"].([]interface{}); ok {
		for _, c := range arr {
			if cm, ok := c.(map[string]interface{}); ok {
				if n, ok := cm["name"].(string); ok && n != "" {
					// If we know the user's channels, only include those; otherwise
					// (couldn't determine membership) fall back to all.
					if len(myChannels) == 0 || myChannels[n] {
						names = append(names, n)
					}
				}
			}
		}
	}
	// Best-effort logout to free the session.
	go client.Get(base + "/user/logout?sid=" + url.QueryEscape(sid))
	return names, nil
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// handleZelloChannels: POST /api/zello-channels {network, api_key, username, password}
// -> {channels:[...]} — lets the GUI populate a dropdown before saving a bridge.
func (s *Server) handleZelloChannels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		Network  string `json:"network"`
		APIKey   string `json:"api_key"`
		Username string `json:"username"`
		Password string `json:"password"`
		BridgeID string `json:"bridge_id"` // optional: reuse a saved bridge's creds
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		zelloErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	// If editing an existing bridge, fill blank creds from the stored bridge.
	if req.BridgeID != "" {
		s.mu.RLock()
		if b, ok := s.zelloBridges[req.BridgeID]; ok {
			if req.Network == "" {
				req.Network = b.Network
			}
			if req.APIKey == "" {
				req.APIKey = b.AuthToken
			}
			if req.Username == "" {
				req.Username = b.Username
			}
			if req.Password == "" {
				req.Password = b.Password
			}
		}
		s.mu.RUnlock()
	}
	if req.Network == "" || req.Username == "" || req.Password == "" || req.APIKey == "" {
		zelloErr(w, http.StatusBadRequest, "network, api_key, username, and password are required to list channels")
		return
	}
	channels, err := zelloFetchChannels(req.Network, req.APIKey, req.Username, req.Password)
	if err != nil {
		zelloErr(w, http.StatusBadGateway, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "channels": channels})
}
