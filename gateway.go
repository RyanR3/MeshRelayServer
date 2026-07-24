package main

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// MCX Step 5 — SIP gateway (STUB + architecture). This translates between the
// MeshRelay protocol and an external MCX platform's SIP signaling (RFC 3261) +
// floor control (3GPP TS 24.380) + RTP media. It is a STUB: it proves the
// translation layer and establishes the architecture. Full SIP dialog state,
// re-INVITE, RTCP, IMS registration, SIPS, and full 24.380 compliance are
// DEFERRED until a concrete interop requirement exists.
//
// It is DISABLED by default (config.SipGateway == nil or Enabled == false) and
// has zero impact when off — the server never starts the listener and the floor
// controller never calls the bridge.
//
//   [MeshRelay clients + server] <-> [SIP Gateway] <-> [External MCX / FirstNet]
//         MeshRelay UDP protocol                        SIP + RTP
//
// Because Step 1 aligned audio to RTP, Step 2 to user@domain identities, and
// Step 3/4 to floor control + group types, the translation here is thin.

// MCXBridge — the MeshRelay server calls these when the gateway is active, to
// push MeshRelay-side events out toward the external MCX platform.
type MCXBridge interface {
	OnFloorRequest(channelKey, userMCXID string, priority uint8) error
	OnFloorRelease(channelKey, userMCXID string) error
	OnAudioPacket(channelKey string, rtpPacket []byte) error
	OnEmergencyAlert(channelKey, userMCXID string) error
}

// MeshRelayCallback — the gateway calls these to push external MCX events INTO
// MeshRelay (grant/deny floor, deliver audio, broadcast emergency).
type MeshRelayCallback interface {
	GrantFloor(channelKey, userMCXID string) error
	DenyFloor(channelKey, userMCXID string, reason uint8) error
	DeliverAudio(channelKey string, rtpPacket []byte) error
	BroadcastEmergency(channelKey, userMCXID string) error
}

// SipGateway is the stub gateway implementing MCXBridge (MeshRelay -> MCX) and
// holding a MeshRelayCallback (MCX -> MeshRelay).
type SipGateway struct {
	cfg      *SipGatewayConfig
	mesh     MeshRelayCallback
	conn     *net.UDPConn
	mu       sync.Mutex
	running  bool
	boundPort int // actual bound port (useful when SipPort==0 in tests)
	registered bool
	// bridgeKey is the MeshRelay channel key ("agency:channel") bound to the
	// external MCX group.
	bridgeKey string
	// external SIP peers we've seen (remote SIP client addr), for RTP/floor routing.
	peers map[string]*net.UDPAddr
}

// NewSipGateway builds the gateway from config and a callback into MeshRelay.
func NewSipGateway(cfg *SipGatewayConfig, mesh MeshRelayCallback) *SipGateway {
	return &SipGateway{
		cfg:       cfg,
		mesh:      mesh,
		bridgeKey: channelKey(cfg.BridgeAgency, cfg.BridgeChannel),
		peers:     make(map[string]*net.UDPAddr),
	}
}

// Start opens the SIP UDP listener and (stub) registers toward the MCX platform.
// Returns nil and does nothing if the gateway is disabled.
func (g *SipGateway) Start() error {
	if g == nil || g.cfg == nil || !g.cfg.Enabled {
		return nil
	}
	port := g.cfg.SipPort
	if port == 0 {
		port = 5060
	}
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: port}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("sip gateway listen :%d: %w", port, err)
	}
	g.mu.Lock()
	g.conn = conn
	g.running = true
	g.boundPort = conn.LocalAddr().(*net.UDPAddr).Port
	g.mu.Unlock()

	log.Printf("[SIP-GW] listening on UDP :%d, bridging %s <-> MCX %s", g.boundPort, g.bridgeKey, g.cfg.McxPlatformHost)

	// Stub REGISTER toward the external MCX platform.
	if g.cfg.McxPlatformHost != "" {
		go g.register()
	}
	go g.readLoop()
	return nil
}

// Stop closes the listener.
func (g *SipGateway) Stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.running = false
	if g.conn != nil {
		g.conn.Close()
		g.conn = nil
	}
	g.mu.Unlock()
}

// register sends a SIP REGISTER toward the MCX platform (stub: fire-and-forget,
// mark registered). A full implementation handles auth (401 + digest) and
// refresh. Deferred per Step 5.
func (g *SipGateway) register() {
	host := g.cfg.McxPlatformHost
	raddr, err := net.ResolveUDPAddr("udp", host)
	if err != nil {
		log.Printf("[SIP-GW] cannot resolve MCX host %q: %v", host, err)
		return
	}
	msg := g.buildRegister()
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()
	if conn == nil {
		return
	}
	if _, err := conn.WriteToUDP([]byte(msg), raddr); err != nil {
		log.Printf("[SIP-GW] REGISTER send failed: %v", err)
		return
	}
	g.mu.Lock()
	g.registered = true
	g.mu.Unlock()
	log.Printf("[SIP-GW] sent REGISTER to %s as %s", host, g.cfg.GatewayMcxID)
}

func (g *SipGateway) buildRegister() string {
	branch := fmt.Sprintf("z9hG4bK%d", time.Now().UnixNano())
	return strings.Join([]string{
		"REGISTER sip:" + g.cfg.McxDomain + " SIP/2.0",
		"Via: SIP/2.0/UDP " + g.localHostPort() + ";branch=" + branch,
		"From: <sip:" + g.cfg.GatewayMcxID + ">;tag=meshrelay",
		"To: <sip:" + g.cfg.GatewayMcxID + ">",
		"Call-ID: meshrelay-gw@" + g.cfg.McxDomain,
		"CSeq: 1 REGISTER",
		"Contact: <sip:" + g.cfg.GatewayMcxID + ">",
		"Expires: 3600",
		"Content-Length: 0",
		"", "",
	}, "\r\n")
}

func (g *SipGateway) localHostPort() string {
	p := g.cfg.SipPort
	if p == 0 {
		p = 5060
	}
	return fmt.Sprintf("0.0.0.0:%d", p)
}

// readLoop reads inbound SIP datagrams and dispatches minimal method handling.
func (g *SipGateway) readLoop() {
	buf := make([]byte, 4096)
	for {
		g.mu.Lock()
		conn := g.conn
		running := g.running
		g.mu.Unlock()
		if !running || conn == nil {
			return
		}
		n, raddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return // conn closed
		}
		g.handleSip(string(buf[:n]), raddr)
	}
}

// handleSip is the STUB SIP method handler: enough to accept a REGISTER/INVITE
// from a test softphone and translate floor intent, not a full dialog machine.
func (g *SipGateway) handleSip(msg string, raddr *net.UDPAddr) {
	line := firstLine(msg)
	method := strings.Fields(line)
	if len(method) == 0 {
		return
	}
	switch strings.ToUpper(method[0]) {
	case "REGISTER":
		g.remember(raddr)
		g.reply(msg, raddr, "200 OK")
		log.Printf("[SIP-GW] inbound REGISTER from %s -> 200 OK", raddr)
	case "INVITE":
		// Group call setup from the SIP side. Accept and treat as a floor request
		// on the bridged channel by the external user (from the From: URI).
		g.remember(raddr)
		g.reply(msg, raddr, "200 OK")
		user := sipUser(headerValue(msg, "From"))
		log.Printf("[SIP-GW] INVITE from %s (%s) -> floor request on %s", raddr, user, g.bridgeKey)
		if g.mesh != nil {
			_ = g.mesh.GrantFloor(g.bridgeKey, user) // stub: grant so audio can flow
		}
	case "BYE":
		g.reply(msg, raddr, "200 OK")
		user := sipUser(headerValue(msg, "From"))
		if g.mesh != nil {
			_ = g.mesh.DenyFloor(g.bridgeKey, user, FloorDenyBusy) // release-ish
		}
	default:
		// Unknown/unsupported (OPTIONS, etc.) — politely accept.
		g.reply(msg, raddr, "200 OK")
	}
}

func (g *SipGateway) remember(raddr *net.UDPAddr) {
	g.mu.Lock()
	g.peers[raddr.String()] = raddr
	g.mu.Unlock()
}

// reply sends a minimal SIP response echoing Via/From/To/Call-ID/CSeq.
func (g *SipGateway) reply(reqMsg string, raddr *net.UDPAddr, status string) {
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()
	if conn == nil {
		return
	}
	resp := strings.Join([]string{
		"SIP/2.0 " + status,
		"Via: " + headerValue(reqMsg, "Via"),
		"From: " + headerValue(reqMsg, "From"),
		"To: " + headerValue(reqMsg, "To"),
		"Call-ID: " + headerValue(reqMsg, "Call-ID"),
		"CSeq: " + headerValue(reqMsg, "CSeq"),
		"Content-Length: 0",
		"", "",
	}, "\r\n")
	conn.WriteToUDP([]byte(resp), raddr)
}

// ---- MCXBridge: MeshRelay -> external MCX (stub translations) ----

func (g *SipGateway) OnFloorRequest(channelKey, userMCXID string, priority uint8) error {
	if g == nil || !g.isActiveFor(channelKey) {
		return nil
	}
	// A MeshRelay user took the floor on the bridged channel — signal the MCX side.
	// Stub: log the translated 3GPP-style floor "Floor Request". Full impl sends
	// an MBCP Floor Request over the MCX floor-control channel.
	log.Printf("[SIP-GW] MeshRelay floor -> MCX: %s took floor on %s (pri %d)", userMCXID, channelKey, priority)
	return nil
}

func (g *SipGateway) OnFloorRelease(channelKey, userMCXID string) error {
	if g == nil || !g.isActiveFor(channelKey) {
		return nil
	}
	log.Printf("[SIP-GW] MeshRelay floor release -> MCX: %s on %s", userMCXID, channelKey)
	return nil
}

func (g *SipGateway) OnAudioPacket(channelKey string, rtpPacket []byte) error {
	if g == nil || !g.isActiveFor(channelKey) {
		return nil
	}
	// RTP passthrough: Step 1 already produced a valid RTP packet, so forwarding to
	// the MCX media plane needs only to strip our 1-byte type prefix. Stub: forward
	// the RTP bytes to known SIP peers.
	rtp := rtpPacket
	if len(rtp) > 0 && rtp[0] == PacketTypeAudioRtp {
		rtp = rtp[1:] // drop MeshRelay type byte -> pure RTP
	}
	g.mu.Lock()
	conn := g.conn
	var peers []*net.UDPAddr
	for _, p := range g.peers {
		peers = append(peers, p)
	}
	g.mu.Unlock()
	if conn == nil {
		return nil
	}
	for _, p := range peers {
		conn.WriteToUDP(rtp, p) // stub: same socket; a full impl uses the RTP port range
	}
	return nil
}

func (g *SipGateway) OnEmergencyAlert(channelKey, userMCXID string) error {
	if g == nil || !g.isActiveFor(channelKey) {
		return nil
	}
	log.Printf("[SIP-GW] MeshRelay EMERGENCY -> MCX: %s on %s", userMCXID, channelKey)
	return nil
}

func (g *SipGateway) isActiveFor(channelKey string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running && channelKey == g.bridgeKey
}

// ---- small SIP text helpers (stub-grade parsing) ----

func firstLine(msg string) string {
	if i := strings.IndexAny(msg, "\r\n"); i >= 0 {
		return msg[:i]
	}
	return msg
}

func headerValue(msg, name string) string {
	for _, ln := range strings.Split(msg, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.HasPrefix(strings.ToLower(ln), strings.ToLower(name)+":") {
			return strings.TrimSpace(ln[len(name)+1:])
		}
	}
	return ""
}

// sipUser extracts the user part from a From/To header like
// "<sip:raptor7@domain>;tag=..." -> "raptor7".
func sipUser(header string) string {
	s := header
	if i := strings.Index(s, "sip:"); i >= 0 {
		s = s[i+4:]
	}
	if i := strings.IndexAny(s, "@>;"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
