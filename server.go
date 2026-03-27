package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
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
	PasswordHash string `json:"password_hash"`
	AgencyID     string `json:"agency_id"`
	Role         string `json:"role"` // "user", "admin", or "super_admin"
	CreatedAt    string `json:"created_at"`
}

type MutualAidLink struct {
	ID          string `json:"id"`
	AgencyA     string `json:"agency_a"`
	AgencyB     string `json:"agency_b"`
	Channel     int    `json:"channel"`
	ChannelName string `json:"channel_name"`
	CreatedAt   string `json:"created_at"`
	CreatedBy   string `json:"created_by"`
	Active      bool   `json:"active"`
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

type Server struct {
	config            *Config
	configFile        string
	conn              *net.UDPConn
	clients           map[string]*Client        // username -> client
	channels          map[string]*Channel       // "agencyID:channelNum" -> channel
	groups            map[string]*GroupPacket    // groupID -> group
	users             map[string]*UserAccount    // username -> account
	agencies          map[string]*Agency         // agencyID -> agency
	authenticatedAddrs map[string]bool           // addr string -> authenticated
	webAdmins          map[string]*WebAdmin      // username -> admin account (web dashboard)
	webSessions        map[string]*WebSession    // session token -> session
	mutualAidLinks     map[string]*MutualAidLink // linkID -> link
	mu                sync.RWMutex
	shutdown          chan struct{}
	stats             *Stats
	updateStatus      *UpdateStatus
}

type Stats struct {
	PacketsReceived uint64
	PacketsSent     uint64
	BytesReceived   uint64
	BytesSent       uint64
	StartTime       time.Time
	mu              sync.RWMutex
}

// channelKey builds the map key for agency-scoped channels.
func channelKey(agencyID string, channelNum int) string {
	return fmt.Sprintf("%s:%d", agencyID, channelNum)
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
		authenticatedAddrs: make(map[string]bool),
		webAdmins:          make(map[string]*WebAdmin),
		webSessions:        make(map[string]*WebSession),
		mutualAidLinks:     make(map[string]*MutualAidLink),
		shutdown:          make(chan struct{}),
		stats: &Stats{
			StartTime: time.Now(),
		},
		updateStatus: &UpdateStatus{},
	}

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
	}
}

func (s *Server) handleAudioPacket(packet *AudioPacket, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}

	s.mu.RLock()
	client, exists := s.clients[packet.Username]
	s.mu.RUnlock()

	if !exists {
		log.Printf("Audio from unknown client: %s (%s)", packet.Username, addr)
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

	// Check for mutual aid partner — also relay to partner agency's same channel
	partnerAgency := s.getMutualAidPartner(client.AgencyID, client.Channel)
	if partnerAgency != "" {
		partnerKey := channelKey(partnerAgency, client.Channel)
		if partnerChannel := s.channels[partnerKey]; partnerChannel != nil {
			partnerChannel.mu.RLock()
			for _, otherClient := range partnerChannel.Clients {
				s.sendPacket(audioData, otherClient.Addr)
				relayCount++
			}
			partnerChannel.mu.RUnlock()
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
	sender, senderExists := s.clients[packet.Sender]
	recipient, recipientExists := s.clients[packet.Recipient]
	s.mu.RUnlock()

	if !senderExists {
		log.Printf("Direct audio from unknown sender: %s (%s)", packet.Sender, addr)
		return
	}

	if !recipientExists {
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
	client, exists := s.clients[packet.Sender]
	s.mu.RUnlock()

	if !exists {
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

	// Check for mutual aid partner — also relay to partner agency's same channel
	partnerAgency := s.getMutualAidPartner(client.AgencyID, client.Channel)
	if partnerAgency != "" {
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
	client, exists := s.clients[packet.Username]
	s.mu.RUnlock()

	if !exists {
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

	// Check for mutual aid partner — also relay heartbeat to partner agency's same channel
	partnerAgency := s.getMutualAidPartner(client.AgencyID, client.Channel)
	if partnerAgency != "" {
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
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Determine agency from user account (default to "default" if not found)
	agencyID := "default"
	if user, ok := s.users[packet.Username]; ok {
		agencyID = user.AgencyID
	}

	// Check if client already exists
	client, exists := s.clients[packet.Username]

	if exists {
		// Client is rejoining, possibly switching channels
		oldKey := channelKey(client.AgencyID, client.Channel)
		oldChannel := s.channels[oldKey]
		if oldChannel != nil {
			oldChannel.mu.Lock()
			delete(oldChannel.Clients, client.Username)
			oldChannel.mu.Unlock()
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
		s.clients[packet.Username] = client
	}

	// Add client to agency-scoped channel
	key := channelKey(agencyID, packet.Channel)
	channel := s.channels[key]
	if channel != nil {
		channel.mu.Lock()
		channel.Clients[client.Username] = client
		channel.mu.Unlock()
		log.Printf("Client %s [%s] joined channel %d from %s", packet.Username, agencyID, packet.Channel, addr)
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
		s.removeClient(clientToRemove)
	}
}

func (s *Server) removeClient(client *Client) {
	// Remove from agency-scoped channel
	key := channelKey(client.AgencyID, client.Channel)
	channel := s.channels[key]
	if channel != nil {
		channel.mu.Lock()
		delete(channel.Clients, client.Username)
		channel.mu.Unlock()
	}

	// Remove from authenticated addresses
	if client.Addr != nil {
		delete(s.authenticatedAddrs, client.Addr.String())
	}

	// Remove from clients map
	delete(s.clients, client.Username)
	log.Printf("Client %s [%s] left channel %d", client.Username, client.AgencyID, client.Channel)
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
	if creator, ok := s.clients[packet.Creator]; ok {
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
	if creator, ok := s.clients[packet.Creator]; ok {
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
	return s.authenticatedAddrs[addr.String()]
}

func (s *Server) handleAuthRequest(packet *AuthRequestPacket, addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, exists := s.users[packet.Username]
	if !exists {
		log.Printf("Auth failed: unknown user %s from %s", packet.Username, addr)
		resp := SerializeAuthResponse(&AuthResponsePacket{Success: false, Message: "Unknown user"})
		s.sendPacket(resp, addr)
		return
	}

	if user.PasswordHash != packet.PasswordHash {
		log.Printf("Auth failed: wrong password for user %s from %s", packet.Username, addr)
		resp := SerializeAuthResponse(&AuthResponsePacket{Success: false, Message: "Invalid password"})
		s.sendPacket(resp, addr)
		return
	}

	// Remove old authenticated address for this user (in case they reconnected from a new port)
	if existingClient, ok := s.clients[packet.Username]; ok {
		oldAddr := existingClient.Addr.String()
		if oldAddr != addr.String() {
			delete(s.authenticatedAddrs, oldAddr)
			log.Printf("Cleaned up stale auth for user %s (old addr: %s)", packet.Username, oldAddr)
		}
	}

	s.authenticatedAddrs[addr.String()] = true
	log.Printf("Auth success: user %s from %s", packet.Username, addr)
	resp := SerializeAuthResponse(&AuthResponsePacket{Success: true, Message: "Authenticated"})
	s.sendPacket(resp, addr)
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
		s.users[user.Username] = user
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

func (s *Server) UpdatePassword(username, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, exists := s.users[username]
	if !exists {
		return fmt.Errorf("user %s not found", username)
	}

	user.PasswordHash = hashPassword(password)

	// Also update web admin password if they have one
	if admin, ok := s.webAdmins[username]; ok {
		admin.PasswordHash = hashPassword(password)
		s.saveWebAdmins()
	}

	if err := s.saveUsers(); err != nil {
		return err
	}

	log.Printf("Updated password for user: %s", username)
	return nil
}

func (s *Server) UpdateUserRole(username, newRole string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, exists := s.users[username]
	if !exists {
		return fmt.Errorf("user %s not found", username)
	}

	oldRole := user.Role
	user.Role = newRole

	// Handle web admin entry changes
	if newRole == "admin" {
		s.webAdmins[username] = &WebAdmin{
			Username:           username,
			PasswordHash:       user.PasswordHash,
			Role:               "agency_admin",
			AgencyID:           user.AgencyID,
			MustChangePassword: false,
		}
		s.saveWebAdmins()
	} else if newRole == "super_admin" {
		s.webAdmins[username] = &WebAdmin{
			Username:           username,
			PasswordHash:       user.PasswordHash,
			Role:               "super_admin",
			MustChangePassword: false,
		}
		s.saveWebAdmins()
	} else if oldRole == "admin" || oldRole == "super_admin" {
		// Downgraded from admin/super_admin to user — remove web admin
		delete(s.webAdmins, username)
		s.saveWebAdmins()
	}

	if err := s.saveUsers(); err != nil {
		return err
	}

	log.Printf("Updated role for user %s: %s -> %s", username, oldRole, newRole)
	return nil
}

func (s *Server) RemoveUser(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, exists := s.users[username]
	if !exists {
		return fmt.Errorf("user %s not found", username)
	}

	// If user had admin/super_admin role, also remove web admin entry
	if user.Role == "admin" || user.Role == "super_admin" {
		delete(s.webAdmins, username)
		if err := s.saveWebAdmins(); err != nil {
			log.Printf("Warning: failed to remove web admin for %s: %v", username, err)
		}
	}

	delete(s.users, username)

	if err := s.saveUsers(); err != nil {
		return err
	}

	log.Printf("Removed user account: %s", username)
	return nil
}

func (s *Server) GetUsers(agencyFilter string) []map[string]interface{} {
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
		users = append(users, map[string]interface{}{
			"username":   user.Username,
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
		s.webAdmins[admins[i].Username] = &admins[i]
	}
	log.Printf("Loaded %d web admin account(s)", len(s.webAdmins))
}

func (s *Server) createDefaultAdmin() {
	s.webAdmins["admin"] = &WebAdmin{
		Username:           "admin",
		PasswordHash:       hashPassword("admin"),
		Role:               "super_admin",
		MustChangePassword: true,
	}
	if err := s.saveWebAdmins(); err != nil {
		log.Printf("Error saving default web admin: %v", err)
	}
	log.Printf("Created default web admin account: admin (password: admin)")
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

	// Backfill users with agency_id and role
	for _, user := range s.users {
		if user.AgencyID == "" {
			user.AgencyID = "default"
		}
		if user.Role == "" {
			user.Role = "user"
		}
	}
	if len(s.users) > 0 {
		if err := s.saveUsers(); err != nil {
			log.Printf("Error saving migrated users: %v", err)
		}
	}

	// Backfill web admins with role
	for _, admin := range s.webAdmins {
		if admin.Role == "" {
			admin.Role = "super_admin"
		}
	}
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

func (s *Server) AddUser(username, password, agencyID, role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[username]; exists {
		return fmt.Errorf("user %s already exists", username)
	}

	if agencyID == "" {
		agencyID = "default"
	}

	if _, exists := s.agencies[agencyID]; !exists {
		return fmt.Errorf("agency %s not found", agencyID)
	}

	if role == "" {
		role = "user"
	}

	s.users[username] = &UserAccount{
		Username:     username,
		PasswordHash: hashPassword(password),
		AgencyID:     agencyID,
		Role:         role,
		CreatedAt:    time.Now().Format(time.RFC3339),
	}

	// If the user is an admin, also create/update a web admin entry for dashboard access
	if role == "admin" {
		s.webAdmins[username] = &WebAdmin{
			Username:           username,
			PasswordHash:       hashPassword(password),
			Role:               "agency_admin",
			AgencyID:           agencyID,
			MustChangePassword: false,
		}
		if err := s.saveWebAdmins(); err != nil {
			log.Printf("Warning: failed to save web admin for %s: %v", username, err)
		}
	} else if role == "super_admin" {
		s.webAdmins[username] = &WebAdmin{
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
		delete(s.users, username)
		return err
	}

	log.Printf("Created user account: %s (agency: %s, role: %s)", username, agencyID, role)
	return nil
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

	for _, link := range links {
		s.mutualAidLinks[link.ID] = link
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

// getMutualAidPartner returns the partner agencyID if an active mutual aid link
// exists for the given agency and channel. Returns "" if no link.
func (s *Server) getMutualAidPartner(agencyID string, channel int) string {
	for _, link := range s.mutualAidLinks {
		if !link.Active {
			continue
		}
		if link.Channel != channel {
			continue
		}
		if link.AgencyA == agencyID {
			return link.AgencyB
		}
		if link.AgencyB == agencyID {
			return link.AgencyA
		}
	}
	return ""
}

// hasMutualAidLink checks if two agencies have an active mutual aid link on any channel.
func (s *Server) hasMutualAidLink(agencyA, agencyB string) bool {
	for _, link := range s.mutualAidLinks {
		if !link.Active {
			continue
		}
		if (link.AgencyA == agencyA && link.AgencyB == agencyB) ||
			(link.AgencyA == agencyB && link.AgencyB == agencyA) {
			return true
		}
	}
	return false
}

// notifyMutualAidChannel sends a ChannelSwitchPacket to all connected clients in both
// agencies, telling the PTT app to add or remove a mutual aid channel from its list.
// Must be called with s.mu held.
func (s *Server) notifyMutualAidChannel(agencyA, agencyB string, channel int, channelName string, active bool) {
	pkt := SerializeChannelSwitch(&ChannelSwitchPacket{
		Channel:     channel,
		ChannelName: channelName,
		Active:      active,
	})

	count := 0
	for _, client := range s.clients {
		if client.AgencyID != agencyA && client.AgencyID != agencyB {
			continue
		}
		s.sendPacket(pkt, client.Addr)
		count++
	}

	action := "added"
	if !active {
		action = "removed"
	}
	log.Printf("Mutual aid channel %s: notified %d clients in [%s, %s] about channel %d (%s)", action, count, agencyA, agencyB, channel, channelName)
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
