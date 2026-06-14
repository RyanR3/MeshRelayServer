package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type Client struct {
	Username      string
	AgencyID      string
	Addr          *net.UDPAddr
	Channel       int
	LastHeartbeat time.Time
	ConnectedAt   time.Time
}

type Channel struct {
	ID      int
	Name    string
	Clients map[string]*Client
	mu      sync.RWMutex
}

type Agency struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CreatedAt   string `json:"created_at"`
	MaxUsers    int    `json:"max_users"`
	MaxChannels int    `json:"max_channels"`
}

type UserAccount struct {
	Username     string `json:"username"`
	Name         string `json:"name,omitempty"`
	PasswordHash string `json:"password_hash"`
	AgencyID     string `json:"agency_id"`
	Role         string `json:"role"` // "user", "admin", or "super_admin"
	CreatedAt    string `json:"created_at"`
}

type MutualAidLink struct {
	ID          string   `json:"id"`
	Agencies    []string `json:"agencies"`
	Channel     int      `json:"channel"`
	ChannelName string   `json:"channel_name"`
	CreatedAt   string   `json:"created_at"`
	CreatedBy   string   `json:"created_by"`
	Active      bool     `json:"active"`
	// Deprecated fields kept for migration from old format
	AgencyA string `json:"agency_a,omitempty"`
	AgencyB string `json:"agency_b,omitempty"`
}

type UpdateStatus struct {
	InProgress bool   `json:"in_progress"`
	Stage      string `json:"stage"`
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
	Error      string `json:"error,omitempty"`
	mu         sync.RWMutex
}

type WebSession struct {
	Username string
	Role     string
	AgencyID string
}

// EnrollmentToken is a temporary token that can be used in place of a password
// during QR code enrollment. It expires after 15 minutes.
type EnrollmentToken struct {
	Token        string    `json:"token"`
	Username     string    `json:"username"`
	AgencyID     string    `json:"agency_id"`
	PasswordHash string    `json:"-"` // the real password hash this token maps to
	ExpiresAt    time.Time `json:"expires_at"`
}

type Server struct {
	config            *Config
	configFile        string
	conn              *net.UDPConn
	clients           map[string]*Client        // username -> client
	channels          map[string]*Channel       // "agencyID:channelNum" -> channel
	groups            map[string]*GroupPacket    // groupID -> group
	users             map[string]*UserAccount    // "agencyID:username" -> account
	agencies          map[string]*Agency         // agencyID -> agency
	authenticatedAddrs map[string]string         // addr string -> agencyID
	pendingChallenges  map[string][32]byte       // addr string -> nonce (Phase 4f challenge-response)
	enrollmentTokens   map[string]*EnrollmentToken // token string -> enrollment token
	webAdmins          map[string]*WebAdmin      // "agencyID:username" -> admin account (web dashboard)
	webSessions        map[string]*WebSession    // session token -> session
	mutualAidLinks     map[string]*MutualAidLink // linkID -> link
	mu                sync.RWMutex
	shutdown          chan struct{}
	stats             *Stats
	updateStatus      *UpdateStatus
	activityLog       *ActivityLog
	networkTimeSeries *NetworkTimeSeries
	wsConnCount       int64 // atomic counter for active WebSocket connections
}

type Stats struct {
	PacketsReceived uint64
	PacketsSent     uint64
	BytesReceived   uint64
	BytesSent       uint64
	StartTime       time.Time
	mu              sync.RWMutex
}

// --- Activity Logging ---

type ActivityEvent struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Username  string `json:"username"`
	Details   string `json:"details"`
	AgencyID  string `json:"agency_id"`
}

type ActivityLog struct {
	Events   []*ActivityEvent `json:"events"`
	mu       sync.RWMutex
	filePath string
	maxSize  int
}

func NewActivityLog(filePath string, maxSize int) *ActivityLog {
	return &ActivityLog{
		Events:   make([]*ActivityEvent, 0),
		filePath: filePath,
		maxSize:  maxSize,
	}
}

func (al *ActivityLog) Load() {
	file, err := os.Open(al.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("No activity log found at %s, starting fresh", al.filePath)
			return
		}
		log.Printf("Error loading activity log: %v", err)
		return
	}
	defer file.Close()

	var events []*ActivityEvent
	if err := json.NewDecoder(file).Decode(&events); err != nil {
		log.Printf("Error parsing activity log: %v", err)
		return
	}

	al.Events = events
	log.Printf("Loaded %d activity events", len(al.Events))
}

func (al *ActivityLog) Save() error {
	al.mu.RLock()
	events := make([]*ActivityEvent, len(al.Events))
	copy(events, al.Events)
	al.mu.RUnlock()

	file, err := os.Create(al.filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(events)
}

func (al *ActivityLog) Add(eventType, username, details, agencyID string) {
	al.mu.Lock()
	defer al.mu.Unlock()

	event := &ActivityEvent{
		Timestamp: time.Now().Format(time.RFC3339),
		Type:      eventType,
		Username:  username,
		Details:   details,
		AgencyID:  agencyID,
	}

	al.Events = append(al.Events, event)

	// Trim to max size
	if len(al.Events) > al.maxSize {
		al.Events = al.Events[len(al.Events)-al.maxSize:]
	}
}

func (al *ActivityLog) GetRecent(n int) []*ActivityEvent {
	al.mu.RLock()
	defer al.mu.RUnlock()

	total := len(al.Events)
	if n > total {
		n = total
	}
	if n == 0 {
		return []*ActivityEvent{}
	}

	// Return newest first
	result := make([]*ActivityEvent, n)
	for i := 0; i < n; i++ {
		result[i] = al.Events[total-1-i]
	}
	return result
}

func (al *ActivityLog) startAutoSave(shutdown chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-shutdown:
			return
		case <-ticker.C:
			if err := al.Save(); err != nil {
				log.Printf("Error auto-saving activity log: %v", err)
			}
		}
	}
}

// --- Network Time-Series ---

type NetworkDataPoint struct {
	Timestamp       time.Time `json:"timestamp"`
	PacketsSent     uint64    `json:"packets_sent"`
	PacketsReceived uint64    `json:"packets_received"`
}

type NetworkTimeSeries struct {
	Points          []NetworkDataPoint `json:"points"`
	lastPacketsSent uint64
	lastPacketsRecv uint64
	mu              sync.RWMutex
}

func NewNetworkTimeSeries() *NetworkTimeSeries {
	return &NetworkTimeSeries{
		Points: make([]NetworkDataPoint, 0, 60),
	}
}

func (nts *NetworkTimeSeries) Record(currentSent, currentRecv uint64) {
	nts.mu.Lock()
	defer nts.mu.Unlock()

	deltaSent := currentSent - nts.lastPacketsSent
	deltaRecv := currentRecv - nts.lastPacketsRecv

	// Skip first recording (delta would be total since startup)
	if nts.lastPacketsSent > 0 || nts.lastPacketsRecv > 0 {
		nts.Points = append(nts.Points, NetworkDataPoint{
			Timestamp:       time.Now(),
			PacketsSent:     deltaSent,
			PacketsReceived: deltaRecv,
		})

		// Keep last 60 points (1 hour of minute-by-minute data)
		if len(nts.Points) > 60 {
			nts.Points = nts.Points[len(nts.Points)-60:]
		}
	}

	nts.lastPacketsSent = currentSent
	nts.lastPacketsRecv = currentRecv
}

func (nts *NetworkTimeSeries) GetPoints() []NetworkDataPoint {
	nts.mu.RLock()
	defer nts.mu.RUnlock()

	result := make([]NetworkDataPoint, len(nts.Points))
	copy(result, nts.Points)
	return result
}

func (s *Server) startNetworkRecorder() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdown:
			return
		case <-ticker.C:
			s.stats.mu.RLock()
			sent := s.stats.PacketsSent
			recv := s.stats.PacketsReceived
			s.stats.mu.RUnlock()
			s.networkTimeSeries.Record(sent, recv)
		}
	}
}

// --- Health Checks ---

type ServiceHealth struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Details string `json:"details"`
}

type HealthReport struct {
	Services       []ServiceHealth `json:"services"`
	OverallPercent int             `json:"overall_percent"`
	HealthyCount   int             `json:"healthy_count"`
	TotalCount     int             `json:"total_count"`
}

func (s *Server) CheckHealth() *HealthReport {
	services := make([]ServiceHealth, 0, 5)
	healthyCount := 0

	// 1. Web Server — always healthy if we're responding
	services = append(services, ServiceHealth{Name: "Web Server", Status: "healthy", Details: "Responding"})
	healthyCount++

	// 2. Database — check JSON data files
	dbHealthy := true
	dbDetails := "All files accessible"
	for _, f := range []string{s.config.UsersFile, s.config.WebAdminsFile, s.config.AgenciesFile} {
		if _, err := os.Stat(f); err != nil {
			dbHealthy = false
			dbDetails = fmt.Sprintf("File inaccessible: %s", f)
			break
		}
	}
	if dbHealthy {
		services = append(services, ServiceHealth{Name: "Database", Status: "healthy", Details: dbDetails})
		healthyCount++
	} else {
		services = append(services, ServiceHealth{Name: "Database", Status: "warning", Details: dbDetails})
	}

	// 3. UDP Relay — check listener
	if s.conn != nil {
		services = append(services, ServiceHealth{Name: "Relay Service", Status: "healthy", Details: "Listener active"})
		healthyCount++
	} else {
		services = append(services, ServiceHealth{Name: "Relay Service", Status: "error", Details: "Listener not active"})
	}

	// 4. WebSocket — check active connection count
	wsCount := atomic.LoadInt64(&s.wsConnCount)
	if wsCount > 0 {
		services = append(services, ServiceHealth{Name: "WebSocket", Status: "healthy", Details: fmt.Sprintf("%d connections", wsCount)})
		healthyCount++
	} else {
		services = append(services, ServiceHealth{Name: "WebSocket", Status: "warning", Details: "No active connections"})
	}

	// 5. Storage — check all data files accessible and get total size
	storageHealthy := true
	var totalSize int64
	for _, f := range []string{s.config.UsersFile, s.config.WebAdminsFile, s.config.AgenciesFile, s.config.MutualAidFile, "activity_log.json"} {
		info, err := os.Stat(f)
		if err != nil {
			if !os.IsNotExist(err) {
				storageHealthy = false
			}
			continue
		}
		totalSize += info.Size()
	}
	sizeStr := fmt.Sprintf("Total: %s", formatBytes(totalSize))
	if storageHealthy {
		services = append(services, ServiceHealth{Name: "Storage", Status: "healthy", Details: sizeStr})
		healthyCount++
	} else {
		services = append(services, ServiceHealth{Name: "Storage", Status: "warning", Details: sizeStr})
	}

	totalCount := len(services)
	pct := 0
	if totalCount > 0 {
		pct = (healthyCount * 100) / totalCount
	}

	return &HealthReport{
		Services:       services,
		OverallPercent: pct,
		HealthyCount:   healthyCount,
		TotalCount:     totalCount,
	}
}

func formatBytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	} else if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
}

// channelKey builds the map key for agency-scoped channels.
func channelKey(agencyID string, channelNum int) string {
	return fmt.Sprintf("%s:%d", agencyID, channelNum)
}

// userKey builds the map key for agency-scoped users: "agencyID:username".
func userKey(agencyID, username string) string {
	return agencyID + ":" + username
}

// ensureAgencyChannels creates channels 0-19 for an agency if they don't exist.
func (s *Server) ensureAgencyChannels(agencyID string) {
	for i := 0; i < 20; i++ {
		key := channelKey(agencyID, i)
		if _, exists := s.channels[key]; !exists {
			s.channels[key] = &Channel{
				ID:      i,
				Name:    fmt.Sprintf("Channel %d", i),
				Clients: make(map[string]*Client),
			}
		}
	}
}

func NewServer(config *Config, configFile string) *Server {
	s := &Server{
		config:            config,
		configFile:        configFile,
		clients:           make(map[string]*Client),
		channels:          make(map[string]*Channel),
		groups:            make(map[string]*GroupPacket),
		users:             make(map[string]*UserAccount),
		agencies:          make(map[string]*Agency),
		authenticatedAddrs: make(map[string]string),
		pendingChallenges:  make(map[string][32]byte),
		enrollmentTokens:   make(map[string]*EnrollmentToken),
		webAdmins:          make(map[string]*WebAdmin),
		webSessions:        make(map[string]*WebSession),
		mutualAidLinks:     make(map[string]*MutualAidLink),
		shutdown:          make(chan struct{}),
		stats: &Stats{
			StartTime: time.Now(),
		},
		updateStatus: &UpdateStatus{},
	}

	s.activityLog = NewActivityLog("activity_log.json", 1000)
	s.activityLog.Load()
	s.networkTimeSeries = NewNetworkTimeSeries()

	s.loadAgencies()
	s.loadUsers()
	s.loadWebAdmins()
	s.loadMutualAid()

	// Migrate to agency model if needed (creates default agency, backfills users/admins)
	s.migrateToAgencies()

	// Ensure channels exist for all agencies
	for agencyID := range s.agencies {
		s.ensureAgencyChannels(agencyID)
	}

	return s
}

func (s *Server) StartPTT() error {
	addr := &net.UDPAddr{
		Port: s.config.PTTPort,
		IP:   net.IPv4zero,
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to bind UDP port: %w", err)
	}

	s.conn = conn
	log.Printf("MeshRelay server listening on UDP port %d", s.config.PTTPort)

	// Start heartbeat checker
	go s.heartbeatChecker()

	// Start network time-series recorder, activity log auto-save, and token cleanup
	go s.startNetworkRecorder()
	go s.activityLog.startAutoSave(s.shutdown)
	s.startTokenCleanup()

	// Start receiving packets
	buffer := make([]byte, 65507) // Max UDP packet size

	for {
		select {
		case <-s.shutdown:
			return nil
		default:
			n, addr, err := conn.ReadFromUDP(buffer)
			if err != nil {
				if s.isShuttingDown() {
					return nil
				}
				log.Printf("Error reading UDP packet: %v", err)
				continue
			}

			// Update stats
			s.stats.mu.Lock()
			s.stats.PacketsReceived++
			s.stats.BytesReceived += uint64(n)
			s.stats.mu.Unlock()

			// Process packet in goroutine
			go s.handlePacket(buffer[:n], addr)
		}
	}
}

func (s *Server) handlePacket(data []byte, addr *net.UDPAddr) {
	packetType, payload, err := ParsePacket(data)
	if err != nil {
		log.Printf("Error parsing packet from %s: %v", addr, err)
		return
	}

	switch packetType {
	case PacketTypeAudio:
		s.handleAudioPacket(payload.(*AudioPacket), addr)
	case PacketTypeDirectAudio:
		s.handleDirectAudioPacket(payload.(*DirectAudioPacket), addr)
	case PacketTypeText:
		s.handleTextPacket(payload.(*TextMessagePacket), addr)
	case PacketTypeHeartbeat:
		s.handleHeartbeat(payload.(*HeartbeatPacket), addr)
	case PacketTypeJoin:
		s.handleJoin(payload.(*JoinPacket), addr)
	case PacketTypeLeave:
		s.handleLeave(addr)
	case PacketTypeGroupCreate:
		s.handleGroupCreate(payload.(*GroupPacket), addr)
	case PacketTypeGroupUpdate:
		s.handleGroupUpdate(payload.(*GroupPacket), addr)
	case PacketTypeAuthRequest:
		s.handleAuthRequest(payload.(*AuthRequestPacket), addr)
	case PacketTypeAuthChallengeReq:
		s.handleAuthChallengeRequest(payload.(*AuthChallengeRequestPacket), addr)
	case PacketTypeKeyJoinRequest, PacketTypeKeyJoinResponse, PacketTypeKeyAnnounce:
		s.relayRawPacket(data, addr)
	}
}

func (s *Server) handleAudioPacket(packet *AudioPacket, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}

	s.mu.RLock()
	client, clientKey := s.findClientByUsername(packet.Username)
	s.mu.RUnlock()

	if client == nil {
		log.Printf("Audio from unknown client: %s (%s)", packet.Username, addr)
		return
	}
	_ = clientKey

	// Update client's last heartbeat
	client.LastHeartbeat = time.Now()

	// Get client's agency-scoped channel
	key := channelKey(client.AgencyID, client.Channel)
	channel := s.channels[key]
	if channel == nil {
		return
	}

	// Prepend packet type to the serialized data
	audioData := append([]byte{PacketTypeAudio}, SerializeAudioPacket(packet)...)

	// Broadcast audio to all clients in the same channel (except sender)
	channel.mu.RLock()
	relayCount := 0
	for _, otherClient := range channel.Clients {
		if otherClient.Username != client.Username {
			s.sendPacket(audioData, otherClient.Addr)
			relayCount++
		}
	}
	channel.mu.RUnlock()

	// Check for mutual aid partners — also relay to partner agencies' same channel
	partners := s.getMutualAidPartners(client.AgencyID, client.Channel)
	if len(partners) > 0 {
		log.Printf("Audio mutual aid: %s [%s] ch %d has partners %v", packet.Username, client.AgencyID, client.Channel, partners)
	}
	for _, partnerAgency := range partners {
		partnerKey := channelKey(partnerAgency, client.Channel)
		partnerChannel := s.channels[partnerKey]
		if partnerChannel != nil {
			partnerChannel.mu.RLock()
			partnerCount := len(partnerChannel.Clients)
			for _, otherClient := range partnerChannel.Clients {
				s.sendPacket(audioData, otherClient.Addr)
				relayCount++
			}
			partnerChannel.mu.RUnlock()
			log.Printf("Audio mutual aid: relayed to %d clients in %s", partnerCount, partnerKey)
		} else {
			log.Printf("Audio mutual aid: channel %s NOT FOUND", partnerKey)
		}
	}

	log.Printf("Relayed audio from %s [%s] to %d clients in channel %d (%d bytes)",
		packet.Username, client.AgencyID, relayCount, client.Channel, len(packet.AudioData))
}

func (s *Server) handleDirectAudioPacket(packet *DirectAudioPacket, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}

	s.mu.RLock()
	sender, _ := s.findClientByUsername(packet.Sender)
	recipient, _ := s.findClientByUsername(packet.Recipient)
	s.mu.RUnlock()

	if sender == nil {
		log.Printf("Direct audio from unknown sender: %s (%s)", packet.Sender, addr)
		return
	}

	if recipient == nil {
		log.Printf("Direct audio to unknown recipient: %s from %s", packet.Recipient, packet.Sender)
		return
	}

	// Enforce same-agency isolation (unless mutual aid link is active)
	if sender.AgencyID != recipient.AgencyID {
		if !s.hasMutualAidLink(sender.AgencyID, recipient.AgencyID) {
			log.Printf("Direct audio blocked: %s [%s] -> %s [%s] (cross-agency)", packet.Sender, sender.AgencyID, packet.Recipient, recipient.AgencyID)
			return
		}
	}

	// Update sender's last heartbeat
	sender.LastHeartbeat = time.Now()

	// Create packet with type prepended
	audioData, err := CreateDirectAudioPacket(packet)
	if err != nil {
		log.Printf("Error creating direct audio packet: %v", err)
		return
	}

	// Send only to the specific recipient
	s.sendPacket(audioData, recipient.Addr)

	log.Printf("Relayed direct audio from %s to %s [%s] (%d bytes)",
		packet.Sender, packet.Recipient, sender.AgencyID, len(packet.AudioData))
}

func (s *Server) handleTextPacket(packet *TextMessagePacket, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}

	s.mu.RLock()
	client, _ := s.findClientByUsername(packet.Sender)
	s.mu.RUnlock()

	if client == nil {
		log.Printf("Text message from unknown client: %s (%s)", packet.Sender, addr)
		return
	}

	// Update client's last heartbeat
	client.LastHeartbeat = time.Now()

	// Get client's agency-scoped channel
	key := channelKey(client.AgencyID, client.Channel)
	channel := s.channels[key]
	if channel == nil {
		return
	}

	// Prepend packet type to the serialized data
	textData := append([]byte{PacketTypeText}, SerializeTextPacket(packet)...)

	// Broadcast message to all clients in the same channel
	channel.mu.RLock()
	relayCount := 0
	for _, otherClient := range channel.Clients {
		s.sendPacket(textData, otherClient.Addr)
		relayCount++
	}
	channel.mu.RUnlock()

	// Check for mutual aid partners — also relay to partner agencies' same channel
	partners := s.getMutualAidPartners(client.AgencyID, client.Channel)
	for _, partnerAgency := range partners {
		partnerKey := channelKey(partnerAgency, client.Channel)
		if partnerChannel := s.channels[partnerKey]; partnerChannel != nil {
			partnerChannel.mu.RLock()
			for _, otherClient := range partnerChannel.Clients {
				s.sendPacket(textData, otherClient.Addr)
				relayCount++
			}
			partnerChannel.mu.RUnlock()
		}
	}

	log.Printf("Relayed text message from %s [%s] to %d clients in channel %d",
		packet.Sender, client.AgencyID, relayCount-1, client.Channel)
}

func (s *Server) handleHeartbeat(packet *HeartbeatPacket, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}

	s.mu.RLock()
	client, _ := s.findClientByUsername(packet.Username)
	s.mu.RUnlock()

	if client == nil {
		// Client not registered (e.g. after server restart) — auto-register them
		// by treating this as a join on channel 0
		log.Printf("Heartbeat from unregistered client %s — auto-registering", packet.Username)
		s.handleJoin(&JoinPacket{Username: packet.Username, Channel: 0}, addr)
		return
	}

	// Update address in case it changed (e.g. mobile network switch)
	client.Addr = addr

	// Update client's last heartbeat
	client.LastHeartbeat = time.Now()

	// Get client's agency-scoped channel
	key := channelKey(client.AgencyID, client.Channel)
	channel := s.channels[key]
	if channel == nil {
		return
	}

	// Create heartbeat packet
	heartbeatData, err := CreateHeartbeatPacket(packet.Username)
	if err != nil {
		log.Printf("Error creating heartbeat packet: %v", err)
		return
	}

	// Relay heartbeat to all other clients in the same channel (not to sender)
	relayCount := 0
	for _, otherClient := range channel.Clients {
		if otherClient.Username != packet.Username {
			s.sendPacket(heartbeatData, otherClient.Addr)
			relayCount++
		}
	}

	// Check for mutual aid partners — also relay heartbeat to partner agencies' same channel
	partners := s.getMutualAidPartners(client.AgencyID, client.Channel)
	for _, partnerAgency := range partners {
		partnerKey := channelKey(partnerAgency, client.Channel)
		if partnerChannel := s.channels[partnerKey]; partnerChannel != nil {
			partnerChannel.mu.RLock()
			for _, otherClient := range partnerChannel.Clients {
				s.sendPacket(heartbeatData, otherClient.Addr)
				relayCount++
			}
			partnerChannel.mu.RUnlock()
		}
	}

	if relayCount > 0 {
		log.Printf("Relayed heartbeat from %s [%s] to %d clients in channel %d",
			packet.Username, client.AgencyID, relayCount, client.Channel)
	}
}

func (s *Server) handleJoin(packet *JoinPacket, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		log.Printf("JOIN rejected: %s from %s (not authenticated)", packet.Username, addr)
		return
	}

	// Collect packets to send AFTER releasing the lock
	type pendingPacket struct {
		data []byte
		addr *net.UDPAddr
		desc string
	}
	var toSend []pendingPacket

	s.mu.Lock()

	// Determine agency: prefer authenticated session, then user account lookup, then default
	agencyID := s.authenticatedAddrs[addr.String()]
	if agencyID == "" {
		if user := s.findUserByUsername(packet.Username); user != nil {
			agencyID = user.AgencyID
		} else {
			agencyID = "default"
			log.Printf("JOIN warning: user %s not found in users list, using agency 'default'", packet.Username)
		}
	}

	// Check if client already exists
	ck := userKey(agencyID, packet.Username)
	client, exists := s.clients[ck]
	var oldChannel int
	isChannelSwitch := false

	if exists {
		// Client is rejoining, possibly switching channels
		oldChannel = client.Channel
		if oldChannel != packet.Channel {
			isChannelSwitch = true
		}
		oldKey := channelKey(client.AgencyID, client.Channel)
		oldCh := s.channels[oldKey]
		if oldCh != nil {
			oldCh.mu.Lock()
			delete(oldCh.Clients, ck)
			oldCh.mu.Unlock()
		}

		client.Channel = packet.Channel
		client.AgencyID = agencyID
		client.Addr = addr
		client.LastHeartbeat = time.Now()
	} else {
		// New client
		client = &Client{
			Username:      packet.Username,
			AgencyID:      agencyID,
			Addr:          addr,
			Channel:       packet.Channel,
			LastHeartbeat: time.Now(),
			ConnectedAt:   time.Now(),
		}
		s.clients[ck] = client
	}

	// Add client to agency-scoped channel
	key := channelKey(agencyID, packet.Channel)
	channel := s.channels[key]
	if channel != nil {
		channel.mu.Lock()
		channel.Clients[ck] = client
		channel.mu.Unlock()
		log.Printf("Client %s [%s] joined channel %d from %s", packet.Username, agencyID, packet.Channel, addr)
	} else {
		log.Printf("JOIN warning: channel %s not found for client %s [%s]", key, packet.Username, agencyID)
	}

	// Collect mutual aid channel notifications to send after releasing the lock
	isMutualAidJoin := false
	for _, link := range s.mutualAidLinks {
		if link.Active && link.Channel == packet.Channel {
			isMutualAidJoin = true
			break
		}
	}
	if !isMutualAidJoin {
		for _, link := range s.mutualAidLinks {
			if !link.Active {
				continue
			}
			for _, a := range link.Agencies {
				if a == agencyID {
					pkt := SerializeChannelSwitch(&ChannelSwitchPacket{
						Channel:     link.Channel,
						ChannelName: link.ChannelName,
						Active:      true,
					})
					toSend = append(toSend, pendingPacket{pkt, addr, fmt.Sprintf("Sent mutual aid channel %d (%s) to newly joined client %s [%s]", link.Channel, link.ChannelName, packet.Username, agencyID)})
					break
				}
			}
		}
	}

	s.mu.Unlock()

	// Log activity events outside the lock
	if !exists {
		s.activityLog.Add("client_connect", packet.Username, fmt.Sprintf("Joined channel %d", packet.Channel), agencyID)
	} else if isChannelSwitch {
		s.activityLog.Add("channel_switch", packet.Username, fmt.Sprintf("Switched from channel %d to channel %d", oldChannel, packet.Channel), agencyID)
	}

	// Send packets outside the lock
	for _, p := range toSend {
		s.sendPacket(p.data, p.addr)
		log.Printf("%s", p.desc)
	}
}

func (s *Server) handleLeave(addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Find client by address
	var clientToRemove *Client
	for _, client := range s.clients {
		if client.Addr.String() == addr.String() {
			clientToRemove = client
			break
		}
	}

	if clientToRemove != nil {
		log.Printf("LEAVE: removing client %s [%s] from channel %d (addr: %s)", clientToRemove.Username, clientToRemove.AgencyID, clientToRemove.Channel, addr)
		s.removeClient(clientToRemove)
	}
}

func (s *Server) removeClient(client *Client) {
	ck := userKey(client.AgencyID, client.Username)

	// Remove from agency-scoped channel
	key := channelKey(client.AgencyID, client.Channel)
	channel := s.channels[key]
	if channel != nil {
		channel.mu.Lock()
		delete(channel.Clients, ck)
		channel.mu.Unlock()
	}

	// Remove from authenticated addresses
	if client.Addr != nil {
		delete(s.authenticatedAddrs, client.Addr.String())
	}

	// Remove from clients map
	delete(s.clients, ck)
	log.Printf("Client %s [%s] left channel %d", client.Username, client.AgencyID, client.Channel)
	s.activityLog.Add("client_disconnect", client.Username, fmt.Sprintf("Left channel %d", client.Channel), client.AgencyID)
}

func (s *Server) sendPacket(data []byte, addr *net.UDPAddr) {
	_, err := s.conn.WriteToUDP(data, addr)
	if err != nil {
		log.Printf("Error sending packet to %s: %v", addr, err)
		return
	}

	// Update stats
	s.stats.mu.Lock()
	s.stats.PacketsSent++
	s.stats.BytesSent += uint64(len(data))
	s.stats.mu.Unlock()
}

// relayRawPacket forwards a raw packet (key exchange, key announce) to all
// other clients in the sender's channel. The packet is sent as-is.
func (s *Server) relayRawPacket(data []byte, senderAddr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(senderAddr) {
		return
	}

	s.mu.RLock()
	sender, _ := s.findClientByAddr(senderAddr)
	if sender == nil {
		s.mu.RUnlock()
		return
	}

	key := channelKey(sender.AgencyID, sender.Channel)
	channel := s.channels[key]
	s.mu.RUnlock()

	if channel == nil {
		return
	}

	channel.mu.RLock()
	for _, other := range channel.Clients {
		if other.Addr.String() != senderAddr.String() {
			s.sendPacket(data, other.Addr)
		}
	}
	channel.mu.RUnlock()
}

func (s *Server) heartbeatChecker() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdown:
			return
		case <-ticker.C:
			s.checkHeartbeats()
		}
	}
}

func (s *Server) checkHeartbeats() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	timeout := 30 * time.Second

	var toRemove []*Client
	for _, client := range s.clients {
		if now.Sub(client.LastHeartbeat) > timeout {
			toRemove = append(toRemove, client)
		}
	}

	for _, client := range toRemove {
		log.Printf("Client %s timed out (no heartbeat for %v)", client.Username, timeout)
		s.removeClient(client)
	}
}

func (s *Server) isShuttingDown() bool {
	select {
	case <-s.shutdown:
		return true
	default:
		return false
	}
}

func (s *Server) Shutdown() {
	close(s.shutdown)
	if s.activityLog != nil {
		s.activityLog.Save()
	}
	if s.conn != nil {
		s.conn.Close()
	}
}

// GetStats returns current server statistics, optionally filtered by agency.
func (s *Server) GetStats(agencyFilter string) map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	s.stats.mu.RLock()
	defer s.stats.mu.RUnlock()

	uptime := time.Since(s.stats.StartTime)

	clientCount := 0
	if agencyFilter == "" {
		clientCount = len(s.clients)
	} else {
		for _, c := range s.clients {
			if c.AgencyID == agencyFilter {
				clientCount++
			}
		}
	}

	return map[string]interface{}{
		"total_clients":     clientCount,
		"packets_received":  s.stats.PacketsReceived,
		"packets_sent":      s.stats.PacketsSent,
		"bytes_received":    s.stats.BytesReceived,
		"bytes_sent":        s.stats.BytesSent,
		"uptime_seconds":    int(uptime.Seconds()),
		"start_time":        s.stats.StartTime,
		"version":           Version,
	}
}

// GetClients returns list of connected clients, optionally filtered by agency.
func (s *Server) GetClients(agencyFilter string) []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	clients := make([]map[string]interface{}, 0, len(s.clients))
	for _, client := range s.clients {
		if agencyFilter != "" && client.AgencyID != agencyFilter {
			continue
		}
		clients = append(clients, map[string]interface{}{
			"username":       client.Username,
			"agency_id":      client.AgencyID,
			"address":        client.Addr.String(),
			"channel":        client.Channel,
			"connected_at":   client.ConnectedAt,
			"last_heartbeat": client.LastHeartbeat,
		})
	}

	return clients
}

// GetChannels returns list of channels with client counts, optionally filtered by agency.
func (s *Server) GetChannels(agencyFilter string) []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	channels := make([]map[string]interface{}, 0)
	for key, channel := range s.channels {
		// Parse agencyID from key "agencyID:channelNum"
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			continue
		}
		chAgencyID := parts[0]
		if agencyFilter != "" && chAgencyID != agencyFilter {
			continue
		}
		channel.mu.RLock()
		channels = append(channels, map[string]interface{}{
			"id":           channel.ID,
			"name":         channel.Name,
			"agency_id":    chAgencyID,
			"client_count": len(channel.Clients),
		})
		channel.mu.RUnlock()
	}

	return channels
}

func (s *Server) handleGroupCreate(packet *GroupPacket, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}

	log.Printf("Group create from %s: %s (creator: %s, members: %v)", addr, packet.GroupName, packet.Creator, packet.Members)

	// Store group
	s.mu.Lock()
	s.groups[packet.GroupID] = packet
	s.mu.Unlock()

	// Determine creator's agency
	s.mu.RLock()
	creatorAgency := "default"
	if creator, _ := s.findClientByUsername(packet.Creator); creator != nil {
		creatorAgency = creator.AgencyID
	}

	relayData, err := CreateGroupPacket(PacketTypeGroupCreate, packet)
	if err != nil {
		log.Printf("Error creating group packet: %v", err)
		s.mu.RUnlock()
		return
	}

	// Relay only to clients in the same agency
	relayCount := 0
	for _, client := range s.clients {
		if client.AgencyID == creatorAgency {
			s.sendPacket(relayData, client.Addr)
			relayCount++
		}
	}
	s.mu.RUnlock()

	log.Printf("Relayed group create to %d clients [%s]", relayCount, creatorAgency)
}

func (s *Server) handleGroupUpdate(packet *GroupPacket, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}

	log.Printf("Group update from %s: %s (members: %v)", addr, packet.GroupName, packet.Members)

	// Update stored group
	s.mu.Lock()
	s.groups[packet.GroupID] = packet
	s.mu.Unlock()

	// Determine creator's agency
	s.mu.RLock()
	creatorAgency := "default"
	if creator, _ := s.findClientByUsername(packet.Creator); creator != nil {
		creatorAgency = creator.AgencyID
	}

	relayData, err := CreateGroupPacket(PacketTypeGroupUpdate, packet)
	if err != nil {
		log.Printf("Error creating group update packet: %v", err)
		s.mu.RUnlock()
		return
	}

	// Relay only to clients in the same agency
	relayCount := 0
	for _, client := range s.clients {
		if client.AgencyID == creatorAgency {
			s.sendPacket(relayData, client.Addr)
			relayCount++
		}
	}
	s.mu.RUnlock()

	log.Printf("Relayed group update to %d clients [%s]", relayCount, creatorAgency)
}

func (s *Server) GetGroups() []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	groups := make([]map[string]interface{}, 0, len(s.groups))
	for _, group := range s.groups {
		groups = append(groups, map[string]interface{}{
			"id":           group.GroupID,
			"name":         group.GroupName,
			"creator":      group.Creator,
			"member_count": len(group.Members),
			"members":      group.Members,
		})
	}

	return groups
}

// --- Authentication ---

func (s *Server) isAuthenticated(addr *net.UDPAddr) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.authenticatedAddrs[addr.String()]
	return ok
}

// getAuthenticatedAgency returns the agency ID for an authenticated address.
func (s *Server) getAuthenticatedAgency(addr *net.UDPAddr) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authenticatedAddrs[addr.String()]
}

// findUserByUsername searches all users to find one matching the given username.
// Used as fallback when agency ID is not provided (backward compat with old PTT clients).
func (s *Server) findUserByUsername(username string) *UserAccount {
	for _, user := range s.users {
		if user.Username == username {
			return user
		}
	}
	return nil
}

// findClientByAddr searches connected clients by address.
func (s *Server) findClientByAddr(addr *net.UDPAddr) (*Client, string) {
	addrStr := addr.String()
	for key, client := range s.clients {
		if client.Addr != nil && client.Addr.String() == addrStr {
			return client, key
		}
	}
	return nil, ""
}

// findClientByUsername searches connected clients by username.
func (s *Server) findClientByUsername(username string) (*Client, string) {
	for key, client := range s.clients {
		if client.Username == username {
			return client, key
		}
	}
	return nil, ""
}

// handleAuthChallengeRequest handles the first step of challenge-response auth (Phase 4f).
// Client sends username, server generates a nonce and sends it back.
func (s *Server) handleAuthChallengeRequest(packet *AuthChallengeRequestPacket, addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Generate 32-byte random nonce
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		log.Printf("Failed to generate auth nonce: %v", err)
		return
	}

	// Store pending challenge (keyed by address)
	s.pendingChallenges[addr.String()] = nonce

	// Send challenge nonce to client
	resp := SerializeAuthChallenge(nonce)
	s.sendPacket(resp, addr)
	log.Printf("Auth challenge sent to %s for user %s", addr, packet.Username)
}

func (s *Server) handleAuthRequest(packet *AuthRequestPacket, addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Try agency-scoped lookup first, fall back to search by username
	var user *UserAccount
	if packet.AgencyID != "" {
		user = s.users[userKey(packet.AgencyID, packet.Username)]
	}
	if user == nil {
		user = s.findUserByUsername(packet.Username)
	}

	if user == nil {
		log.Printf("Auth failed: unknown user %s from %s", packet.Username, addr)
		resp := SerializeAuthResponse(&AuthResponsePacket{Success: false, Message: "Unknown user"})
		s.sendPacket(resp, addr)
		return
	}

	// Challenge-response auth is required — legacy static-hash auth has been removed.
	addrKey := addr.String()
	nonce, hasPendingChallenge := s.pendingChallenges[addrKey]

	if !hasPendingChallenge {
		// No challenge issued for this address — challenge-response is required.
		log.Printf("Auth rejected: no pending challenge for %s (client must use challenge-response)", addr)
		resp := SerializeAuthResponse(&AuthResponsePacket{
			Success: false,
			Message: "Authentication method not supported — please update the app",
		})
		s.sendPacket(resp, addr)
		return
	}

	// Challenge-response: client sends HMAC-SHA256(stored hash, nonce)
	delete(s.pendingChallenges, addrKey) // consume nonce (one-time use)
	expectedHmac := computeHmacSha256([]byte(user.PasswordHash), nonce[:])
	authOk := subtle.ConstantTimeCompare(
		[]byte(hex.EncodeToString(expectedHmac)),
		[]byte(packet.PasswordHash)) == 1

	// If real password didn't match, try enrollment tokens
	usedEnrollToken := ""
	if !authOk {
		for token, et := range s.enrollmentTokens {
			if et.Username == packet.Username && time.Now().Before(et.ExpiresAt) {
				// The client used the enrollment token as a password:
				// client computes SHA256(token), then HMAC(SHA256(token), nonce)
				tokenHash := hashPassword(et.Token)
				tokenHmac := computeHmacSha256([]byte(tokenHash), nonce[:])
				if subtle.ConstantTimeCompare(
					[]byte(hex.EncodeToString(tokenHmac)),
					[]byte(packet.PasswordHash)) == 1 {
					authOk = true
					usedEnrollToken = token
					break
				}
			}
		}
	}

	if !authOk {
		log.Printf("Auth failed: wrong password for user %s from %s", packet.Username, addr)
		resp := SerializeAuthResponse(&AuthResponsePacket{Success: false, Message: "Invalid password"})
		s.sendPacket(resp, addr)
		return
	}

	// Consume enrollment token after successful use
	if usedEnrollToken != "" {
		delete(s.enrollmentTokens, usedEnrollToken)
		log.Printf("Auth (enrollment token) success: user %s from %s — token consumed", packet.Username, addr)
	} else {
		log.Printf("Auth (challenge-response) success: user %s from %s", packet.Username, addr)
	}

	// Remove old authenticated address for this user (in case they reconnected from a new port)
	clientKey := userKey(user.AgencyID, user.Username)
	if existingClient, ok := s.clients[clientKey]; ok {
		oldAddr := existingClient.Addr.String()
		if oldAddr != addr.String() {
			delete(s.authenticatedAddrs, oldAddr)
			log.Printf("Cleaned up stale auth for user %s (old addr: %s)", packet.Username, oldAddr)
		}
	}

	s.authenticatedAddrs[addr.String()] = user.AgencyID
	resp := SerializeAuthResponse(&AuthResponsePacket{Success: true, Message: "Authenticated"})
	s.sendPacket(resp, addr)
}

// computeHmacSha256 computes HMAC-SHA256(key, data).
func computeHmacSha256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// --- Enrollment Tokens ---

// GenerateEnrollmentToken creates a random 16-byte hex token for QR enrollment.
// The token maps to the user's real password hash and expires after 15 minutes.
// Must be called while s.mu is held.
func (s *Server) GenerateEnrollmentToken(username, agencyID, passwordHash string) string {
	tokenBytes := make([]byte, 8)
	if _, err := rand.Read(tokenBytes); err != nil {
		log.Printf("Failed to generate enrollment token: %v", err)
		return ""
	}
	token := hex.EncodeToString(tokenBytes)

	s.enrollmentTokens[token] = &EnrollmentToken{
		Token:        token,
		Username:     username,
		AgencyID:     agencyID,
		PasswordHash: passwordHash,
		ExpiresAt:    time.Now().Add(15 * time.Minute),
	}

	log.Printf("Enrollment token created for user %s (agency: %s), expires in 15 minutes", username, agencyID)
	return token
}

// cleanupExpiredTokens removes expired enrollment tokens. Called periodically.
func (s *Server) cleanupExpiredTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for token, et := range s.enrollmentTokens {
		if now.After(et.ExpiresAt) {
			delete(s.enrollmentTokens, token)
			log.Printf("Enrollment token expired for user %s", et.Username)
		}
	}
}

// startTokenCleanup runs a goroutine that cleans up expired tokens every minute.
func (s *Server) startTokenCleanup() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.cleanupExpiredTokens()
			case <-s.shutdown:
				return
			}
		}
	}()
}

// --- User Management ---

func (s *Server) loadUsers() {
	file, err := os.Open(s.config.UsersFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("No users file found at %s, starting with empty user list", s.config.UsersFile)
			return
		}
		log.Printf("Error loading users file: %v", err)
		return
	}
	defer file.Close()

	var users []*UserAccount
	if err := json.NewDecoder(file).Decode(&users); err != nil {
		log.Printf("Error parsing users file: %v", err)
		return
	}

	for _, user := range users {
		key := userKey(user.AgencyID, user.Username)
		s.users[key] = user
	}
	log.Printf("Loaded %d user accounts", len(s.users))
}

func (s *Server) saveUsers() error {
	users := make([]*UserAccount, 0, len(s.users))
	for _, user := range s.users {
		users = append(users, user)
	}

	file, err := os.Create(s.config.UsersFile)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(users)
}

func hashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

// hashPasswordSecure hashes a password with bcrypt (cost 12).
func hashPasswordSecure(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// checkPassword verifies a password against either a bcrypt hash ($2a$/$2b$ prefix)
// or a legacy SHA-256 hex hash (64 chars). Returns true on match.
func checkPassword(password, storedHash string) bool {
	if strings.HasPrefix(storedHash, "$2a$") || strings.HasPrefix(storedHash, "$2b$") {
		return bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(password)) == nil
	}
	// Legacy SHA-256 path — constant-time comparison
	expected := []byte(hashPassword(password))
	got := []byte(storedHash)
	return subtle.ConstantTimeCompare(expected, got) == 1
}

func (s *Server) UpdatePassword(agencyID, username, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := userKey(agencyID, username)
	user, exists := s.users[key]
	if !exists {
		return fmt.Errorf("user %s not found", username)
	}

	user.PasswordHash = hashPassword(password) // PTT auth uses SHA-256 (Phase 4f will migrate to challenge-response)

	// Also update web admin password if they have one — use bcrypt for web
	if admin, ok := s.webAdmins[key]; ok {
		if bcryptHash, err := hashPasswordSecure(password); err == nil {
			admin.PasswordHash = bcryptHash
		} else {
			admin.PasswordHash = hashPassword(password) // fallback
		}
		s.saveWebAdmins()
	}

	if err := s.saveUsers(); err != nil {
		return err
	}

	log.Printf("Updated password for user: %s [%s]", username, agencyID)
	return nil
}

func (s *Server) UpdateUserRole(agencyID, username, newRole string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := userKey(agencyID, username)
	user, exists := s.users[key]
	if !exists {
		return fmt.Errorf("user %s not found", username)
	}

	oldRole := user.Role
	user.Role = newRole

	// Handle web admin entry changes
	if newRole == "admin" {
		s.webAdmins[key] = &WebAdmin{
			Username:           username,
			PasswordHash:       user.PasswordHash,
			Role:               "agency_admin",
			AgencyID:           user.AgencyID,
			MustChangePassword: false,
		}
		s.saveWebAdmins()
	} else if newRole == "super_admin" {
		s.webAdmins[key] = &WebAdmin{
			Username:           username,
			PasswordHash:       user.PasswordHash,
			Role:               "super_admin",
			MustChangePassword: false,
		}
		s.saveWebAdmins()
	} else if oldRole == "admin" || oldRole == "super_admin" {
		// Downgraded from admin/super_admin to user — remove web admin
		delete(s.webAdmins, key)
		s.saveWebAdmins()
	}

	if err := s.saveUsers(); err != nil {
		return err
	}

	log.Printf("Updated role for user %s [%s]: %s -> %s", username, agencyID, oldRole, newRole)
	return nil
}

func (s *Server) RemoveUser(agencyID, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := userKey(agencyID, username)
	user, exists := s.users[key]
	if !exists {
		return fmt.Errorf("user %s not found", username)
	}

	// If user had admin/super_admin role, also remove web admin entry
	if user.Role == "admin" || user.Role == "super_admin" {
		delete(s.webAdmins, key)
		if err := s.saveWebAdmins(); err != nil {
			log.Printf("Warning: failed to remove web admin for %s: %v", username, err)
		}
	}

	delete(s.users, key)

	if err := s.saveUsers(); err != nil {
		return err
	}

	log.Printf("Removed user account: %s [%s]", username, agencyID)
	s.activityLog.Add("user_deleted", username, fmt.Sprintf("Removed from agency %s", agencyID), agencyID)
	return nil
}

func (s *Server) GetUsers(agencyFilter, callerRole, callerAgency, callerUsername string) []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	users := make([]map[string]interface{}, 0, len(s.users))
	for _, user := range s.users {
		if agencyFilter != "" && user.AgencyID != agencyFilter {
			continue
		}

		role := user.Role
		if role == "" {
			role = "user"
		}

		// Role-based visibility:
		// - super_admin: sees everyone
		// - agency_admin: sees "user" and "admin" roles in own agency (not super_admin)
		// - user: sees only themselves
		if callerRole == "agency_admin" {
			if user.AgencyID != callerAgency {
				continue
			}
			if role == "super_admin" {
				continue
			}
		}

		users = append(users, map[string]interface{}{
			"username":   user.Username,
			"name":       user.Name,
			"agency_id":  user.AgencyID,
			"role":       role,
			"created_at": user.CreatedAt,
		})
	}
	return users
}

// ---- Web Admin / Dashboard Auth ----

type WebAdmin struct {
	Username           string `json:"username"`
	PasswordHash       string `json:"password_hash"`
	Role               string `json:"role"`                  // "super_admin" or "agency_admin"
	AgencyID           string `json:"agency_id,omitempty"`   // empty for super_admin
	MustChangePassword bool   `json:"must_change_password"`
}

func (s *Server) loadWebAdmins() {
	file, err := os.Open(s.config.WebAdminsFile)
	if err != nil {
		// File doesn't exist — create default admin account
		log.Printf("No web admins file found, creating default admin account")
		s.createDefaultAdmin()
		return
	}
	defer file.Close()

	var admins []WebAdmin
	if err := json.NewDecoder(file).Decode(&admins); err != nil {
		log.Printf("Error reading web admins file: %v, creating default", err)
		s.createDefaultAdmin()
		return
	}

	for i := range admins {
		key := userKey(admins[i].AgencyID, admins[i].Username)
		s.webAdmins[key] = &admins[i]
	}
	log.Printf("Loaded %d web admin account(s)", len(s.webAdmins))
}

func (s *Server) createDefaultAdmin() {
	// Generate a cryptographically random initial password
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("FATAL: cannot generate random password: %v", err)
	}
	initialPassword := hex.EncodeToString(buf) // 32-char hex string

	bcryptHash, err := hashPasswordSecure(initialPassword)
	if err != nil {
		log.Fatalf("FATAL: cannot hash initial password: %v", err)
	}

	s.webAdmins[userKey("", "admin")] = &WebAdmin{
		Username:           "admin",
		PasswordHash:       bcryptHash,
		Role:               "super_admin",
		MustChangePassword: true,
	}
	if err := s.saveWebAdmins(); err != nil {
		log.Printf("Error saving default web admin: %v", err)
	}

	log.Printf("==============================================================")
	log.Printf("INITIAL ADMIN PASSWORD (record this, it will not be shown again):")
	log.Printf("  admin / %s", initialPassword)
	log.Printf("==============================================================")
	log.Printf("You MUST change this password at first login.")
}

func (s *Server) saveWebAdmins() error {
	admins := make([]WebAdmin, 0, len(s.webAdmins))
	for _, admin := range s.webAdmins {
		admins = append(admins, *admin)
	}

	file, err := os.Create(s.config.WebAdminsFile)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(admins)
}

func (s *Server) generateSessionToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) validateWebSession(token string) bool {
	if token == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.webSessions[token]
	return ok
}

// --- Agency Management ---

func (s *Server) loadAgencies() {
	file, err := os.Open(s.config.AgenciesFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("No agencies file found, will run migration on first load")
			return
		}
		log.Printf("Error loading agencies file: %v", err)
		return
	}
	defer file.Close()

	var agencies []*Agency
	if err := json.NewDecoder(file).Decode(&agencies); err != nil {
		log.Printf("Error parsing agencies file: %v", err)
		return
	}

	for _, agency := range agencies {
		s.agencies[agency.ID] = agency
	}
	log.Printf("Loaded %d agencies", len(s.agencies))
}

func (s *Server) saveAgencies() error {
	agencies := make([]*Agency, 0, len(s.agencies))
	for _, agency := range s.agencies {
		agencies = append(agencies, agency)
	}

	file, err := os.Create(s.config.AgenciesFile)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(agencies)
}

// migrateToAgencies runs once if no agencies exist, creating a "default" agency
// and backfilling all existing users and web admins.
func (s *Server) migrateToAgencies() {
	// Rename "Default Agency" to "Watchtower" if it still has the old name
	if agency, ok := s.agencies["default"]; ok && agency.Name == "Default Agency" {
		agency.Name = "Watchtower"
		s.saveAgencies()
		log.Printf("Renamed default agency to 'Watchtower'")
	}

	if len(s.agencies) > 0 {
		return
	}

	log.Printf("Running agency migration: creating default agency...")

	// Create default agency
	s.agencies["default"] = &Agency{
		ID:          "default",
		Name:        "Watchtower",
		CreatedAt:   time.Now().Format(time.RFC3339),
		MaxUsers:    100,
		MaxChannels: 20,
	}

	// Backfill users with agency_id and role, and re-key map
	rekeyed := make(map[string]*UserAccount)
	for _, user := range s.users {
		if user.AgencyID == "" {
			user.AgencyID = "default"
		}
		if user.Role == "" {
			user.Role = "user"
		}
		rekeyed[userKey(user.AgencyID, user.Username)] = user
	}
	s.users = rekeyed
	if len(s.users) > 0 {
		if err := s.saveUsers(); err != nil {
			log.Printf("Error saving migrated users: %v", err)
		}
	}

	// Backfill web admins with role and re-key map
	rekeyedAdmins := make(map[string]*WebAdmin)
	for _, admin := range s.webAdmins {
		if admin.Role == "" {
			admin.Role = "super_admin"
		}
		rekeyedAdmins[userKey(admin.AgencyID, admin.Username)] = admin
	}
	s.webAdmins = rekeyedAdmins
	if len(s.webAdmins) > 0 {
		if err := s.saveWebAdmins(); err != nil {
			log.Printf("Error saving migrated web admins: %v", err)
		}
	}

	if err := s.saveAgencies(); err != nil {
		log.Printf("Error saving agencies: %v", err)
	}

	log.Printf("Agency migration complete: %d users and %d admins updated", len(s.users), len(s.webAdmins))
}

// AddUser creates a new user account and returns an enrollment token for QR enrollment.
func (s *Server) AddUser(username, name, password, agencyID, role string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if agencyID == "" {
		agencyID = "default"
	}

	key := userKey(agencyID, username)

	if _, exists := s.users[key]; exists {
		return "", fmt.Errorf("user %s already exists in agency %s", username, agencyID)
	}

	if _, exists := s.agencies[agencyID]; !exists {
		return "", fmt.Errorf("agency %s not found", agencyID)
	}

	if role == "" {
		role = "user"
	}

	pwHash := hashPassword(password)
	s.users[key] = &UserAccount{
		Username:     username,
		Name:         name,
		PasswordHash: pwHash,
		AgencyID:     agencyID,
		Role:         role,
		CreatedAt:    time.Now().Format(time.RFC3339),
	}

	// If the user is an admin, also create/update a web admin entry for dashboard access
	if role == "admin" {
		adminHash := hashPassword(password) // fallback to SHA-256
		if bh, err := hashPasswordSecure(password); err == nil {
			adminHash = bh // prefer bcrypt for web admin
		}
		s.webAdmins[key] = &WebAdmin{
			Username:           username,
			PasswordHash:       adminHash,
			Role:               "agency_admin",
			AgencyID:           agencyID,
			MustChangePassword: false,
		}
		if err := s.saveWebAdmins(); err != nil {
			log.Printf("Warning: failed to save web admin for %s: %v", username, err)
		}
	} else if role == "super_admin" {
		s.webAdmins[key] = &WebAdmin{
			Username:           username,
			PasswordHash:       hashPassword(password),
			Role:               "super_admin",
			MustChangePassword: false,
		}
		if err := s.saveWebAdmins(); err != nil {
			log.Printf("Warning: failed to save web admin for %s: %v", username, err)
		}
	}

	if err := s.saveUsers(); err != nil {
		delete(s.users, key)
		return "", err
	}

	// Generate enrollment token for QR code (expires in 15 minutes)
	enrollToken := s.GenerateEnrollmentToken(username, agencyID, pwHash)

	log.Printf("Created user account: %s (agency: %s, role: %s)", username, agencyID, role)
	s.activityLog.Add("user_created", username, fmt.Sprintf("Created in agency %s with role %s", agencyID, role), agencyID)
	return enrollToken, nil
}

// --- Mutual Aid ---

func (s *Server) loadMutualAid() {
	file, err := os.Open(s.config.MutualAidFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("No mutual aid file found, starting with empty links")
			return
		}
		log.Printf("Error loading mutual aid file: %v", err)
		return
	}
	defer file.Close()

	var links []*MutualAidLink
	if err := json.NewDecoder(file).Decode(&links); err != nil {
		log.Printf("Error parsing mutual aid file: %v", err)
		return
	}

	migrated := false
	for _, link := range links {
		// Migrate old AgencyA/AgencyB format to Agencies array
		if len(link.Agencies) == 0 && link.AgencyA != "" && link.AgencyB != "" {
			link.Agencies = []string{link.AgencyA, link.AgencyB}
			link.AgencyA = ""
			link.AgencyB = ""
			migrated = true
		}
		s.mutualAidLinks[link.ID] = link
	}
	if migrated {
		s.saveMutualAid()
		log.Printf("Migrated mutual aid links to multi-agency format")
	}
	log.Printf("Loaded %d mutual aid links", len(s.mutualAidLinks))
}

func (s *Server) saveMutualAid() error {
	links := make([]*MutualAidLink, 0, len(s.mutualAidLinks))
	for _, link := range s.mutualAidLinks {
		links = append(links, link)
	}

	file, err := os.Create(s.config.MutualAidFile)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(links)
}

// getMutualAidPartners returns all partner agency IDs if an active mutual aid link
// exists for the given agency and channel. Returns nil if no link.
func (s *Server) getMutualAidPartners(agencyID string, channel int) []string {
	for _, link := range s.mutualAidLinks {
		if !link.Active {
			continue
		}
		if link.Channel != channel {
			continue
		}
		// Check if this agency is part of this link
		found := false
		for _, a := range link.Agencies {
			if a == agencyID {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		// Return all other agencies in the link
		var partners []string
		for _, a := range link.Agencies {
			if a != agencyID {
				partners = append(partners, a)
			}
		}
		return partners
	}
	return nil
}

// hasMutualAidLink checks if two agencies share an active mutual aid link on any channel.
func (s *Server) hasMutualAidLink(agencyA, agencyB string) bool {
	for _, link := range s.mutualAidLinks {
		if !link.Active {
			continue
		}
		foundA, foundB := false, false
		for _, a := range link.Agencies {
			if a == agencyA {
				foundA = true
			}
			if a == agencyB {
				foundB = true
			}
		}
		if foundA && foundB {
			return true
		}
	}
	return false
}

// notifyMutualAidChannel sends a ChannelSwitchPacket to all connected clients in the
// given agencies, telling the PTT app to add or remove a mutual aid channel from its list.
// Must be called with s.mu held.
func (s *Server) notifyMutualAidChannel(agencies []string, channel int, channelName string, active bool) {
	pkt := SerializeChannelSwitch(&ChannelSwitchPacket{
		Channel:     channel,
		ChannelName: channelName,
		Active:      active,
	})

	agencySet := make(map[string]bool, len(agencies))
	for _, a := range agencies {
		agencySet[a] = true
	}

	// Collect targets under read lock, then send outside the lock
	type target struct {
		username string
		agencyID string
		addr     *net.UDPAddr
	}
	var targets []target

	s.mu.RLock()
	for _, client := range s.clients {
		if !agencySet[client.AgencyID] {
			continue
		}
		targets = append(targets, target{client.Username, client.AgencyID, client.Addr})
	}
	s.mu.RUnlock()

	for _, t := range targets {
		s.sendPacket(pkt, t.addr)
		log.Printf("Mutual aid notify: sent to %s [%s]", t.username, t.agencyID)
	}

	action := "added"
	if !active {
		action = "removed"
	}
	log.Printf("Mutual aid channel %s: notified %d clients in %v about channel %d (%s)", action, len(targets), agencies, channel, channelName)
}

// pickMutualAidChannel picks the highest unused channel number (starting from 19 down)
// that has no active mutual aid link. Must be called with s.mu held.
func (s *Server) pickMutualAidChannel() int {
	used := make(map[int]bool)
	for _, link := range s.mutualAidLinks {
		if link.Active {
			used[link.Channel] = true
		}
	}
	for ch := 19; ch >= 0; ch-- {
		if !used[ch] {
			return ch
		}
	}
	return 19 // fallback
}
