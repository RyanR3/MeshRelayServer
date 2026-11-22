// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type Client struct {
	Username string
	AgencyID string
	// Account: userKey of the login account that JOINed this callsign (the
	// callsign can differ from the login). Only packets from an address that
	// authenticated as this account may move the client to a new address or
	// take the callsign over. "" for records not created by a UDP JOIN
	// (dispatch consoles) or when auth is disabled.
	Account       string
	Addr          *net.UDPAddr
	Channel       int
	LastHeartbeat time.Time
	ConnectedAt   time.Time
	// LowPower: the client asked for the low-power stream (HeartbeatFlagLowPower).
	LowPower bool
	// EmergencyAware: the client knows the EMERGENCY channel
	// (HeartbeatFlagEmergencyChannel), so its audio arrives channel-tagged.
	EmergencyAware bool
	// lastPing: when the server last sent this low-power client a keepalive.
	lastPing time.Time
	// MonitoredChannels: extra channels this client always LISTENS to (mutual-aid
	// / Zello monitor toggle), in addition to its active Channel.
	MonitoredChannels []int
	// Loc: last position from a LOCATION_REPORT (dispatch_command.go). Memory only.
	Loc *UnitLocation
	// Dispatch consoles only: the agency of the transmit channel (super admin
	// picks one; "" = every agency) and the extra channels monitored,
	// as channel keys "agency:channel".
	DispatchTxAgency string
	DispatchMonitor  map[string]bool
	DispatchAllCall  bool // transmit to every talkgroup of the TX agency
}

type Channel struct {
	ID      int
	Name    string
	Clients map[string]*Client
	mu      sync.RWMutex
}

// Channel map. Every category gets its own block so allocators can never
// collide, and a channel number alone says what kind of channel it is.
//
//	  0 -  49  agency talkgroups (the only ones an agency configures)
//	 50 -  98  mutual-aid patches and ad-hoc groups
//	100 - 149  LMR bridges
//	150 - 199  Zello bridges
//
// The gap at 99 is deliberate padding between the shared block and the bridge
// blocks. These are wire values; the UI shows talkgroups 1-based.
const (
	// MaxAgencyTalkgroups is how many ordinary channels an agency owns.
	MaxAgencyTalkgroups = 50

	SharedChannelMin = 50 // mutual aid + ad-hoc groups
	SharedChannelMax = 98

	ZelloChannelMin = 150
	ZelloChannelMax = 199
)

type Agency struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CreatedAt   string `json:"created_at"`
	MaxUsers    int    `json:"max_users"`
	MaxChannels int    `json:"max_channels"`

	// Discoverable opts this agency into the server-wide directory: every other
	// agency admin can see its NAME and send it a mutual-aid request. Nothing
	// else is exposed — no users, channels, or activity. Off by default, so an
	// agency stays invisible until it deliberately opts in.
	Discoverable bool `json:"discoverable"`

	// AutoAcceptFrom lists agency IDs whose mutual-aid requests are accepted
	// without approval — a standing agreement with a trusted partner.
	//
	// It is deliberately one-directional: it governs what THIS agency accepts,
	// not what it may send. So a link auto-joins only when the RECEIVING side
	// already trusts the sender, and "both agreed" means both added each other.
	AutoAcceptFrom []string `json:"auto_accept_from,omitempty"`

	// FloorControl turns on one-talker-per-channel arbitration for this agency
	// (MCX floor control): a second PTT press while someone is talking is
	// denied with a busy tone and queued, and only the floor holder's audio is
	// relayed. Off = open PTT, where simultaneous talkers are heard mixed.
	// Set from the dashboard; the legacy config list floor_control_agencies
	// still works and is OR-ed with this flag.
	FloorControl bool `json:"floor_control"`

	// QueueWhenBusy (with FloorControl on) replaces the busy tone with
	// store-and-forward: a second talker's transmission is accepted, recorded
	// on the server and replayed to the channel once the current talker is
	// done, in the order received.
	QueueWhenBusy bool `json:"queue_when_busy"`

	// NoGuestInvites stops this agency's members inviting guests from the app
	// (guest_invites.go). Off = invites allowed, the default.
	NoGuestInvites bool `json:"no_guest_invites,omitempty"`

	// NoRecording stops recording this agency's radio traffic (recorder.go).
	NoRecording bool `json:"no_recording,omitempty"`
	// Transcribe turns this agency's recordings into text (transcriber.go);
	// TranscribeWords are names and terms given to the model as a hint.
	Transcribe      bool   `json:"transcribe,omitempty"`
	TranscribeWords string `json:"transcribe_words,omitempty"`
	// RecordingRetentionDays: how long this agency keeps its recordings;
	// 0 = the server's recording_retention_days (recorder.go).
	RecordingRetentionDays int `json:"recording_retention_days,omitempty"`

	// ChannelCount is how many talkgroups the agency uses (0 = the default,
	// DefaultAgencyChannels) and ChannelNames what they are called, by wire
	// channel number. Synced to every phone and console (channel_names.go).
	ChannelCount int            `json:"channel_count,omitempty"`
	ChannelNames map[int]string `json:"channel_names,omitempty"`

	// The EMERGENCY channel (emergency_channel.go): its name ("" = EMERGENCY)
	// and the LMR bridge its audio is also sent out on ("" = none).
	EmergencyChannelName string `json:"emergency_channel_name,omitempty"`
	EmergencyLMRBridge   string `json:"emergency_lmr_bridge,omitempty"`
}

// Floor modes kept in Server.floorOn (lock-free, read per audio packet).
const (
	floorModeOff   = 0
	floorModeBusy  = 1 // one talker; others get a busy tone and a live queue
	floorModeQueue = 2 // one talker; others are recorded and replayed after
)

func (a *Agency) floorMode() int {
	switch {
	case !a.FloorControl:
		return floorModeOff
	case a.QueueWhenBusy:
		return floorModeQueue
	default:
		return floorModeBusy
	}
}

// trustsAgency reports whether this agency auto-accepts requests from another.
func (a *Agency) trustsAgency(agencyID string) bool {
	for _, id := range a.AutoAcceptFrom {
		if id == agencyID {
			return true
		}
	}
	return false
}

type UserAccount struct {
	Username     string `json:"username"`
	Name         string `json:"name,omitempty"`
	PasswordHash string `json:"password_hash"`
	AgencyID     string `json:"agency_id"`
	Role         string `json:"role"` // "user", "admin", or "super_admin"
	CreatedAt    string `json:"created_at"`
	// MCX Step 3: floor-control priority (1=EMERGENCY,2=COMMAND,3=RESPONDER,
	// 4=MONITOR). 0/absent = default RESPONDER. Assigned at provisioning; clients
	// cannot self-elevate (except Emergency alert).
	Priority int `json:"priority,omitempty"`
	// Guest invited from the app (guest_invites.go): who invited them, and
	// when the account is deleted. Nil for every other account.
	InvitedBy      string     `json:"invited_by,omitempty"`
	GuestExpiresAt *time.Time `json:"guest_expires_at,omitempty"`
	// LastLogin: when this account last signed in to PTT (RFC 3339).
	LastLogin string `json:"last_login,omitempty"`
	// Devices: per-phone keys issued by enrollment QR codes
	// (account_security.go). The password above is never replaced by a QR.
	Devices []*DeviceKey `json:"devices,omitempty"`
	// MCX Step 2 note: the derived MCX identity (<callsign>@<agency>.<mcptt_domain>)
	// is NOT stored — it's computed on demand (see userMCXID / GetUsers) and added
	// to API responses. The plain Username remains the display name + routing key.
}

// effectivePriority returns the user's floor priority, defaulting to RESPONDER.
func (u *UserAccount) effectivePriority() uint8 {
	if u == nil || u.Priority < PriorityEmergency || u.Priority > PriorityMonitor {
		return PriorityResponder
	}
	return uint8(u.Priority)
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

// WebSession is one dashboard / console login. Role and AgencyID are
// re-resolved from webAdmins on every request (see sessionFor in web.go);
// the copies here are the values at login, used to detect a role change.
type WebSession struct {
	Username string
	Role     string
	AgencyID string

	adminKey  string       // webAdmins key of the account (canonical case)
	pwHash    string       // dashboard hash at login; a change revokes the session
	createdAt time.Time    // absolute lifetime starts here
	lastSeen  atomic.Int64 // unix nanos of the last authenticated activity
}

// DispatchConn represents an active dispatch browser WebSocket connection.
// It wraps the WebSocket connection with a send channel to avoid concurrent writes.
type DispatchConn struct {
	Send chan []byte // buffered channel for outbound binary frames
	Text chan []byte // JSON messages (activity, emergencies) — dispatch_command.go
	tx   dispatchTX  // transmit (PTT) state, dispatch_audio.go
}

// EnrollmentToken is a temporary token that can be used in place of a password
// during QR code enrollment. It expires after 15 minutes.
// Guest event tokens have IsGuest=true and are NOT deleted after use.
type EnrollmentToken struct {
	Token        string    `json:"token"`
	Username     string    `json:"username"`
	AgencyID     string    `json:"agency_id"`
	PasswordHash string    `json:"-"` // the real password hash this token maps to
	ExpiresAt    time.Time `json:"expires_at"`
	IsGuest      bool      `json:"is_guest,omitempty"` // guest event tokens: multi-use, long-lived
	// One-time QR (account_security.go): when and from where it was used,
	// and the device key that use issued.
	UsedAt   time.Time `json:"-"`
	UsedFrom string    `json:"-"`
	DeviceID string    `json:"-"`
	// The device key's secret, kept in memory only for the retry grace so a
	// retry (or the app's second connection at startup) gets the SAME key.
	pendingSecret []byte
}

// GuestEvent is a temporary shared account for mutual aid / pop-up events.
// Multiple clients can scan the same QR code simultaneously.
type GuestEvent struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	AgencyID         string     `json:"agency_id"`
	Channel          int        `json:"channel"`
	Username         string     `json:"username"`             // auto-generated guest username
	Token            string     `json:"token"`                // enrollment token (multi-use)
	ExpiresAt        *time.Time `json:"expires_at,omitempty"` // nil = no expiry
	CreatedAt        string     `json:"created_at"`
	CreatedBy        string     `json:"created_by"`
	OwnsAgency       bool       `json:"owns_agency,omitempty"`        // true if AgencyID was auto-created for this event and should be deleted with it
	OwnerAgencyID    string     `json:"owner_agency_id,omitempty"`    // the real agency that created this event; "" = created by super_admin
	IncludedAgencies []string   `json:"included_agencies,omitempty"`  // agencies bridged into this event's channel
	MutualAidLinkID  string     `json:"mutual_aid_link_id,omitempty"` // the auto-created mutual-aid link (removed with the event)
}

type Server struct {
	proxyNetsOnce sync.Once
	proxyNets     []*net.IPNet
	loginLimit    *signInGuard  // dashboard logins (ratelimit.go)
	oidc          *oidcProvider // dashboard single sign-on (oidc.go); nil = off
	pttAuthLimit  *signInGuard  // phone sign-ins
	// rosterSent: last roster sent per agency to low-power clients.
	rosterMu   sync.Mutex
	rosterSent map[string]rosterState

	config                 *Config
	configFile             string
	conn                   *net.UDPConn
	clients                map[string]*Client      // username -> client
	channels               map[string]*Channel     // "agencyID:channelNum" -> channel
	groups                 map[string]*GroupPacket // groupID -> group
	users                  map[string]*UserAccount // "agencyID:username" -> account
	agencies               map[string]*Agency      // agencyID -> agency
	authenticatedAddrs     map[string]string       // addr string -> agencyID
	authAccounts           map[string]string       // addr string -> userKey of the LOGIN account (the joined callsign can differ)
	dispatchOps            *dispatchOps            // unit status, dispatch messages (dispatch_ops.go)
	opsOnce                sync.Once
	recorder               *Recorder    // radio traffic recording (recorder.go)
	transcriber            *Transcriber // speech-to-text of recordings (transcriber.go)
	takMgr                 *takManager  // TAK Server connectors per agency (tak.go)
	takOnce                sync.Once
	lastChannel            map[string]int // userKey -> channel of the last JOIN, for re-registration
	lastChannelSavePending bool
	authSessions           map[string]authSession // addr string -> how it signed in (account_security.go)
	bridgeTx               bridgeTxGate           // transmit limits on bridged channels (bridge_tx.go)
	bridgeGuestGate        noteGate
	// Secure protocol (secure.go): the server's identity key, live sessions,
	// and remembered password verifiers.
	identity          *ecdh.PrivateKey
	sec               secTable
	dispatch          packetDispatcher
	verifierMu        sync.Mutex
	verifiers         map[string]scramVerifier
	enrollmentTokens  map[string]*EnrollmentToken // token string -> enrollment token
	guestEvents       map[string]*GuestEvent      // eventID -> guest event
	webAdmins         map[string]*WebAdmin        // "agencyID:username" -> admin account (web dashboard)
	dispatchConns     map[string]*DispatchConn    // "agencyID:username" -> active dispatch WebSocket
	webSessions       map[string]*WebSession      // session token -> session
	mutualAidLinks    map[string]*MutualAidLink   // linkID -> link
	emergMu           sync.Mutex
	emergCalls        map[string]map[string]*emergencyCall                         // agency -> user -> active emergency (emergency_channel.go)
	lmrBridges        map[string]*LMRBridge                                        // bridgeID -> LMR/VHF radio bridge
	mutualAidRequests map[string]*MutualAidRequest                                 // requestID -> agency-to-agency ask
	zelloBridges      map[string]*ZelloBridge                                      // bridgeID -> Zello Work channel bridge
	ssrcToUser        map[uint32]string                                            // RTP SSRC -> username (MCX Step 1)
	ssrcMu            sync.RWMutex                                                 // guards ssrcToUser
	floor             *FloorController                                             // MCX Step 3 floor control (per channel)
	floorOn           sync.Map                                                     // agencyID -> floorMode* (lock-free for the audio path)
	groupMgr          *GroupManager                                                // MCX Step 4 group types / ad-hoc
	sipGateway        *SipGateway                                                  // MCX Step 5 SIP gateway (nil = disabled)
	testFloorSink     func(*net.UDPAddr, []byte)                                   // test-only: capture floor packets
	testRelaySink     func(key string, channel int, user string, audioData []byte) // test-only: capture replayed audio
	testFloorUserSink func(user string, data []byte)                               // test-only: capture per-user floor packets
	autoFloorMu       sync.Mutex
	autoFloorLast     map[string]time.Time // gateway username -> last automatic floor request
	pausedGateways    sync.Map             // userKey of a disabled LMR bridge\'s gateway: relay nothing to it (lock-free, read per packet)
	mu                sync.RWMutex
	shutdown          chan struct{}
	stats             *Stats
	updateStatus      *UpdateStatus
	activityLog       *ActivityLog
	networkTimeSeries *NetworkTimeSeries
	wsConnCount       int64 // atomic counter for active WebSocket connections

	monTag monitorTagger // RTP numbering for channel-tagged monitor copies (monitor_tag.go)
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

	return writeJSONFileAtomic(al.filePath, events)
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

// GetRecentForAgency returns up to n newest events scoped to a single agency.
// An empty agencyID returns all events (super_admin view).
func (al *ActivityLog) GetRecentForAgency(n int, agencyID string) []*ActivityEvent {
	if agencyID == "" {
		return al.GetRecent(n)
	}

	al.mu.RLock()
	defer al.mu.RUnlock()

	result := make([]*ActivityEvent, 0, n)
	// Walk newest-first, collecting only this agency's events up to n.
	for i := len(al.Events) - 1; i >= 0 && len(result) < n; i-- {
		if al.Events[i].AgencyID == agencyID {
			result = append(result, al.Events[i])
		}
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
	for _, f := range []string{s.config.UsersFile, s.config.WebAdminsFile, s.config.AgenciesFile, s.config.MutualAidFile, s.lmrBridgesFile(), s.mutualAidRequestsFile(), "guest_events.json", "activity_log.json"} {
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

	// 6. Zello Bridge — only shown when at least one bridge is enabled. Healthy
	// if any bridge is connected; warning if enabled but none connected.
	s.mu.RLock()
	var enabledBridges, connectedBridges int
	var firstDetail string
	for _, b := range s.zelloBridges {
		if !b.Enabled {
			continue
		}
		enabledBridges++
		st, _ := func() (string, string) { b.statusMu.Lock(); defer b.statusMu.Unlock(); return b.Status, b.LastError }()
		if st == "connected" {
			connectedBridges++
			if firstDetail == "" {
				firstDetail = fmt.Sprintf("%s ↔ %s", b.ZelloChannel, b.Name)
			}
		}
	}
	s.mu.RUnlock()
	if enabledBridges > 0 {
		if connectedBridges > 0 {
			d := fmt.Sprintf("%d connected", connectedBridges)
			if firstDetail != "" {
				d = firstDetail
				if connectedBridges > 1 {
					d = fmt.Sprintf("%s +%d", firstDetail, connectedBridges-1)
				}
			}
			services = append(services, ServiceHealth{Name: "Zello Bridge", Status: "healthy", Details: d})
			healthyCount++
		} else {
			services = append(services, ServiceHealth{Name: "Zello Bridge", Status: "warning", Details: "Enabled but not connected"})
		}
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
	// Talkgroups plus the shared mutual-aid/ad-hoc block. Bridge channels
	// (LMR, Zello) are created on demand by their own code, since a bridge
	// only exists in the agencies it serves.
	for i := 0; i <= EmergencyChannel; i++ { // talkgroups, shared block, EMERGENCY
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
		config:             config,
		configFile:         configFile,
		clients:            make(map[string]*Client),
		channels:           make(map[string]*Channel),
		groups:             make(map[string]*GroupPacket),
		users:              make(map[string]*UserAccount),
		agencies:           make(map[string]*Agency),
		authenticatedAddrs: make(map[string]string),
		authAccounts:       make(map[string]string),
		authSessions:       make(map[string]authSession),
		// 10 wrong passwords per address, then that address waits; an account
		// is guarded only when 50 failures arrive from many addresses.
		loginLimit:        newSignInGuard(10, 50, 15*time.Minute, 15*time.Minute),
		pttAuthLimit:      newSignInGuard(10, 50, 5*time.Minute, 5*time.Minute),
		enrollmentTokens:  make(map[string]*EnrollmentToken),
		guestEvents:       make(map[string]*GuestEvent),
		webAdmins:         make(map[string]*WebAdmin),
		dispatchConns:     make(map[string]*DispatchConn),
		webSessions:       make(map[string]*WebSession),
		mutualAidLinks:    make(map[string]*MutualAidLink),
		lmrBridges:        make(map[string]*LMRBridge),
		mutualAidRequests: make(map[string]*MutualAidRequest),
		zelloBridges:      make(map[string]*ZelloBridge),
		ssrcToUser:        make(map[uint32]string),
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
	s.loadLastChannels()
	s.loadUsers()
	if err := s.loadOrCreateIdentity(); err != nil {
		log.Printf("WARNING: secure protocol disabled: %v", err)
	}
	s.loadWebAdmins()
	s.loadMutualAid()
	s.loadLMRBridges()
	s.loadMutualAidRequests()
	s.loadGuestEvents()
	s.loadZelloBridges()
	s.loadMapLayers()

	// Migrate to agency model if needed (creates default agency, backfills users/admins)
	s.migrateToAgencies()

	// Ensure channels exist for all agencies
	for agencyID := range s.agencies {
		s.ensureAgencyChannels(agencyID)
	}

	// MCX Step 3 floor controller + Step 4 group manager, sharing one periodic
	// tick (floor dead-man/emergency expiry + ad-hoc group expiry).
	s.floor = NewFloorController(s)
	s.groupMgr = NewGroupManager(s)
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-s.shutdown:
				return
			case <-t.C:
				s.floor.Tick()
				s.groupMgr.Tick()
				s.emergencyTick()
			}
		}
	}()

	// MCX Step 5: SIP gateway (disabled unless config.SipGateway.Enabled). The
	// server implements MeshRelayCallback so the gateway can inject MCX-side
	// events. Zero impact when off.
	if config.SipGateway != nil && config.SipGateway.Enabled {
		s.sipGateway = NewSipGateway(config.SipGateway, s)
		if err := s.sipGateway.Start(); err != nil {
			log.Printf("[SIP-GW] failed to start: %v", err)
			s.sipGateway = nil
		}
	}

	return s
}

// ---- MCX Step 5: MeshRelayCallback (gateway -> MeshRelay) ----

// GrantFloor lets the gateway grant the floor to an external MCX user on a
// bridged channel (so their audio relays to MeshRelay members).
func (s *Server) GrantFloor(channelKey, userMCXID string) error {
	if s.floor == nil {
		return nil
	}
	ch := channelNumFromKey(channelKey)
	// Treat the external user as a Command-priority floor holder on the bridge.
	s.floor.Request(channelKey, &FloorRequest{Channel: ch, User: userMCXID, Priority: PriorityCommand}, PriorityCommand)
	return nil
}

// DenyFloor / release from the gateway side.
func (s *Server) DenyFloor(channelKey, userMCXID string, reason uint8) error {
	if s.floor == nil {
		return nil
	}
	s.floor.Release(channelKey, channelNumFromKey(channelKey), userMCXID)
	return nil
}

// DeliverAudio injects an RTP audio packet from the MCX side into a MeshRelay
// channel (relays to all members).
func (s *Server) DeliverAudio(channelKey string, rtpPacket []byte) error {
	agencyID := channelKey
	if i := indexByteLast(channelKey, ':'); i >= 0 {
		agencyID = channelKey[:i]
	}
	pkt := rtpPacket
	// Ensure it carries our type byte for clients.
	if len(pkt) == 0 || pkt[0] != PacketTypeAudioRtp {
		pkt = append([]byte{PacketTypeAudioRtp}, rtpPacket...)
	}
	s.broadcastFloorToChannel(channelKey, pkt)
	_ = agencyID
	return nil
}

// BroadcastEmergency injects an emergency alert from the MCX side.
func (s *Server) BroadcastEmergency(channelKey, userMCXID string) error {
	ch := channelNumFromKey(channelKey)
	s.broadcastFloorToChannel(channelKey, buildEmergencyAlert(ch, userMCXID, true))
	return nil
}

// floorEnabledForAgency reports whether floor control is on for an agency:
// the agency's dashboard setting, or the legacy config list. Read on every
// audio packet, so it uses the lock-free map rather than s.agencies.
func (s *Server) floorEnabledForAgency(agencyID string) bool {
	return s.floorModeForAgency(agencyID) != floorModeOff
}

// floorQueueForKey: queue mode for one channel key. The EMERGENCY channel
// always queues (a second emergency is recorded and replayed after the first).
func (s *Server) floorQueueForKey(key string) bool {
	if isEmergencyKey(key) {
		return true
	}
	return s.floorQueueForAgency(agencyFromKey(key))
}

// floorQueueForAgency reports whether a busy channel records and replays a
// second talker (queue mode) rather than refusing them.
func (s *Server) floorQueueForAgency(agencyID string) bool {
	return s.floorModeForAgency(agencyID) == floorModeQueue
}

func (s *Server) floorModeForAgency(agencyID string) int {
	if m, ok := s.floorOn.Load(agencyID); ok && m.(int) != floorModeOff {
		return m.(int)
	}
	for _, a := range s.config.FloorControlAgencies {
		if a == agencyID {
			return floorModeBusy
		}
	}
	return floorModeOff
}

// agencyFromKey recovers the agency ID from a channel key ("agency:channel").
func agencyFromKey(key string) string {
	if i := strings.LastIndex(key, ":"); i >= 0 {
		return key[:i]
	}
	return key
}

// setFloorControl sets an agency's floor policy and persists it.
// Caller must NOT hold s.mu.
func (s *Server) setFloorControl(agencyID string, on bool, queue bool) error {
	s.mu.Lock()
	a := s.agencies[agencyID]
	if a == nil {
		s.mu.Unlock()
		return fmt.Errorf("agency not found")
	}
	a.FloorControl = on
	a.QueueWhenBusy = queue
	mode := a.floorMode()
	s.mu.Unlock()
	s.floorOn.Store(agencyID, mode)
	if mode == floorModeOff && s.floor != nil {
		// Nobody should stay "holder" of a channel that is now open.
		s.floor.ReleaseAgency(agencyID)
	}
	return s.saveAgencies()
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
	s.recorder = NewRecorder(s, s.config.RecordingDir, s.config.RecordingRetentionDays)
	log.Printf("Recording radio traffic to %s/ (kept %d days)", s.recorder.dir, s.recorder.retention)
	s.transcriber = NewTranscriber(s, s.config.TranscribeDir)
	if ok, m := s.transcriber.ready(); ok {
		log.Printf("Transcription available (model %s)", m.Name)
	}
	s.loadTakConnections()

	// Start heartbeat checker
	go s.heartbeatChecker()

	// Start network time-series recorder, activity log auto-save, and token cleanup
	go s.startNetworkRecorder()
	go s.activityLog.startAutoSave(s.shutdown)
	s.startTokenCleanup()
	go s.StartEnabledZelloBridges()

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

			// Process packet in goroutine, on its own copy: the next
			// ReadFromUDP reuses buffer, and overwrote packets still being
			// parsed ("Error parsing packet ... unexpected EOF", lost or
			// garbled audio — constant at video-call packet rates).
			pkt := make([]byte, n)
			copy(pkt, buffer[:n])
			// Handled in arrival order per source (ordered_dispatch.go).
			s.dispatchPacket(pkt, addr)
		}
	}
}

func (s *Server) handlePacket(data []byte, addr *net.UDPAddr) {
	if len(data) == 0 {
		return
	}
	if data[0] >= PacketTypeSecHello && data[0] <= PacketTypeSecRetry {
		s.handleSecurePacket(data, addr) // secure.go
		return
	}
	// An address with a secure session speaks ONLY through it. A plaintext
	// packet that claims to come from that address is forged (a spoofed
	// "leave", a replayed emergency, injected audio) and is dropped.
	if s.sec.live() && s.addrIsSecure(addr.String()) {
		return
	}
	s.handlePlainPacket(data, addr)
}

// handlePlainPacket dispatches one MeshRelay packet: straight off the wire in
// the old protocol, or after decryption in the secure one.
func (s *Server) handlePlainPacket(data []byte, addr *net.UDPAddr) {
	packetType, payload, err := ParsePacket(data)
	if err != nil {
		log.Printf("Error parsing packet from %s: %v", addr, err)
		return
	}

	switch packetType {
	case PacketTypeAudio, PacketTypeAudioOpus, PacketTypeAudioRtp:
		s.handleAudioPacket(payload.(*AudioPacket), addr)
	case PacketTypeFloorRequest:
		s.handleFloorRequest(payload.(*FloorRequest), addr)
	case PacketTypeEmergencyAlert:
		s.handleEmergencyAlert(payload.(*EmergencyAlert), addr)
	case PacketTypeGroupCreateAdhoc:
		s.handleGroupCreateAdhoc(payload.(*GroupCreateAdhoc), addr)
	case PacketTypeGroupInviteResponse:
		s.handleGroupInviteResponse(payload.(*GroupInviteResponse), addr)
	case PacketTypeGroupDisband:
		s.handleGroupDisband(payload.(*GroupDisband), addr)
	case PacketTypeGuestInviteRequest:
		s.handleGuestInviteRequest(payload.(*GuestInviteRequest), addr)
	case PacketTypeLocationReport:
		s.handleLocationReport(payload.(*LocationReport), addr)
	case PacketTypeUnitStatus:
		s.handleUnitStatus(payload.(*UnitStatusPacket), addr)
	case PacketTypeTranscriptRequest:
		s.handleTranscriptRequest(payload.(*TranscriptRequest), addr)
	case PacketTypeRecordingAudioReq:
		s.handleRecordingAudioRequest(payload.(*RecordingAudioRequest), addr)
	case PacketTypeDispatchMessage:
		s.handleDispatchMessage(payload.(*DispatchMessagePacket), addr)
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
	case PacketTypeMonitorChannels:
		s.handleMonitorChannels(payload.(*MonitorChannelsPacket), addr)
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

// handleMonitorChannels updates which extra channels a client always listens to.
func (s *Server) handleMonitorChannels(pkt *MonitorChannelsPacket, addr *net.UDPAddr) {
	if !s.isAuthenticated(addr) {
		return
	}
	s.mu.Lock()
	for _, c := range s.clients {
		if c.Addr.String() == addr.String() {
			c.MonitoredChannels = pkt.Channels
			s.mu.Unlock()
			log.Printf("Client %s [%s] monitoring channels %v", c.Username, c.AgencyID, pkt.Channels)
			return
		}
	}
	s.mu.Unlock()
}

func (s *Server) handleAudioPacket(packet *AudioPacket, addr *net.UDPAddr) {
	if !s.isAuthenticated(addr) {
		return
	}

	// Resolve the sender from the authenticated source address: the name in
	// the packet must be a callsign this address is entitled to. A look-up by
	// name alone let another agency's user take this client over.
	s.mu.Lock()
	client := s.senderClient(packet.Username, addr)
	var snap Client
	if client != nil {
		// Any authenticated packet keeps the client alive.
		client.LastHeartbeat = time.Now()
		// Learn the client's current address from its audio, not only from its
		// 10-second heartbeats. Production log, 2026-09-09 01:42:41 UTC: a
		// gateway's path changed mid-transmission and for the next ~10 s the
		// server kept relaying to the stale address — 50 pkt/s "to 1 client"
		// that never arrived — until the next heartbeat refreshed it.
		// senderClient only returns a record at another address when this
		// address authenticated as the record's own login account.
		s.adoptSenderAddr(client, addr, "audio")
		// Relay from a snapshot: JOINs change Channel under s.mu.
		snap = *client
	}
	s.mu.Unlock()

	if client == nil {
		log.Printf("Audio from unknown client: %s (%s)", packet.Username, addr)
		return
	}
	client = &snap

	// Prepend packet type to the serialized data, preserving both the codec and
	// the RTP framing. An RTP-aligned packet (0x13) is relayed as RTP with the
	// sender's original RTP header intact, so receivers and any MCX gateway see
	// the true transmitter SSRC / sequence / timestamp / marker. Otherwise an
	// Opus frame relays as 0x10 and PCM as 0x00.
	var audioData []byte
	if packet.RTP != nil {
		audioData = append([]byte{PacketTypeAudioRtp}, SerializeAudioRtpPacket(packet)...)
	} else {
		relayType := PacketTypeAudio
		if packet.Codec == CodecOpus {
			relayType = PacketTypeAudioOpus
		}
		audioData = append([]byte{relayType}, SerializeAudioPacket(packet)...)
	}

	// A unit with an active emergency talks on the EMERGENCY channel, whatever
	// channel it is on (emergency_channel.go).
	if s.routeEmergencyAudio(client, packet, audioData) {
		return
	}
	// Nobody else talks on the EMERGENCY channel.
	if client.Channel == EmergencyChannel {
		return
	}
	// Bridged channels (LMR, Zello): no guests unless the bridge allows them,
	// and a transmit time limit (bridge_tx.go).
	if !s.bridgeTxAllowed(client, time.Now()) {
		return
	}

	// MCX Step 3: when floor control is enabled for this agency, only the current
	// floor holder's audio is relayed. A non-holder is either recorded for replay
	// (queue mode, if the server granted them a queued slot) or dropped. The
	// holder's audio resets the dead-man timer. Floor control is skipped when
	// disabled — open PTT as before.
	if s.floorEnabledForAgency(client.AgencyID) {
		fkey := channelKey(client.AgencyID, client.Channel)
		if !s.floor.HasFloor(fkey, packet.Username) && !s.autoFloorForGateway(client, fkey) {
			s.floor.RecordDeferred(fkey, packet.Username, packet, audioData)
			return
		}
		s.floor.NoteAudio(fkey, packet.Username)
	}

	// MCX Step 1: remember which user an RTP SSRC belongs to, so a future gateway
	// (and diagnostics) can map RTP streams back to a MeshRelay identity without
	// re-parsing the audio body.
	if packet.RTP != nil {
		s.ssrcMu.Lock()
		s.ssrcToUser[packet.RTP.SSRC] = packet.Username
		s.ssrcMu.Unlock()
	}

	s.relayAudioData(client, packet, audioData)
}

// relayDeferred delivers one recorded frame of a queued transmission to the
// channel as if the user had just sent it (floor replay, queue mode). The
// user's live client may be gone by now, so the relay is addressed by agency
// and channel rather than by connection.
func (s *Server) relayDeferred(key string, channelNum int, user string, packet *AudioPacket, audioData []byte) {
	if s.testRelaySink != nil {
		s.testRelaySink(key, channelNum, user, audioData)
		return
	}
	// The recording belongs to the channel's agency: never resolve the user
	// by name in another agency.
	agency := agencyFromKey(key)
	if isEmergencyKey(key) {
		// A queued emergency: the EMERGENCY channel and its LMR bridge.
		s.relayEmergencyAudio(agency, user, nil, packet, audioData)
		return
	}
	s.mu.RLock()
	var client *Client
	if c := s.clientInAgency(agency, user); c != nil {
		cp := *c
		client = &cp
	}
	s.mu.RUnlock()
	if client == nil {
		client = &Client{Username: user, AgencyID: agency, Channel: channelNum}
	}
	s.relayAudioData(client, packet, audioData)
}

// relayAudioData fans one relay-ready audio frame out to everyone who should
// hear it: the sender's channel, always-listen monitors, ad-hoc group members,
// mutual-aid partners, bridges and the dispatch console.
func (s *Server) relayAudioData(client *Client, packet *AudioPacket, audioData []byte) {
	// Get client's agency-scoped channel, and the mutual-aid partners'
	// channels, under s.mu: the channel map and the link list are written by
	// the dashboard while packets are being relayed.
	key := channelKey(client.AgencyID, client.Channel)
	s.mu.RLock()
	channel := s.channels[key]
	partners := s.getMutualAidPartners(client.AgencyID, client.Channel)
	partnerChannels := make(map[string]*Channel, len(partners))
	for _, p := range partners {
		pk := channelKey(p, client.Channel)
		partnerChannels[pk] = s.channels[pk]
	}
	s.mu.RUnlock()
	if channel == nil {
		return
	}

	// Channel-tagged variant (0x14) for MONITOR recipients only: without the
	// tag, always-listen audio is attributed to the receiver's active channel.
	// RTP and Opus audio have a tagged form (monitor_tag.go); raw PCM has none
	// and monitors keep getting it untagged. Built once, on first use, so the
	// made-up RTP numbering only advances for frames a monitor receives.
	var monitorTagged []byte
	monitorAudioData := func() []byte {
		if monitorTagged == nil {
			monitorTagged = s.monitorAudio(monitorStreamKey(client.AgencyID, client.Channel, packet.Username), client.Channel, audioData)
		}
		return monitorTagged
	}
	// EMERGENCY-channel listeners keep the tagging they had: RTP audio only.
	emergencyAudioData := audioData
	if packet.RTP != nil {
		emergencyAudioData = tagRTPAudio(client.Channel, audioData)
	}

	// Broadcast audio to all clients in the same channel (except sender).
	// Track who already received it so monitors aren't double-sent.
	sentTo := make(map[string]bool)
	channel.mu.RLock()
	relayCount := 0
	for _, otherClient := range channel.Clients {
		if otherClient.Username != client.Username && !s.gatewayPaused(otherClient) {
			s.sendPacket(audioData, otherClient.Addr)
			sentTo[otherClient.Addr.String()] = true
			relayCount++
		}
	}
	channel.mu.RUnlock()

	// Also relay to clients in this agency who are MONITORING this channel
	// (always-listen) but aren't tuned to it — so mutual-aid/Zello channels are
	// heard regardless of the client's active channel.
	s.mu.RLock()
	emergency := client.Channel == EmergencyChannel
	for _, mc := range s.clients {
		if mc.AgencyID != client.AgencyID || mc.Username == client.Username || s.gatewayPaused(mc) {
			continue
		}
		if sentTo[mc.Addr.String()] {
			continue // already got it as an active-channel member
		}
		if emergency {
			// Everyone in the agency always hears the EMERGENCY channel.
			// Apps that don't know it get it untagged, so it still plays.
			if s.emergencyListener(mc) {
				if mc.EmergencyAware {
					s.sendPacket(emergencyAudioData, mc.Addr)
				} else {
					s.sendPacket(audioData, mc.Addr)
				}
				sentTo[mc.Addr.String()] = true
				relayCount++
			}
			continue
		}
		for _, ch := range mc.MonitoredChannels {
			if ch == client.Channel {
				s.sendPacket(monitorAudioData(), mc.Addr)
				sentTo[mc.Addr.String()] = true
				relayCount++
				break
			}
		}
	}
	s.mu.RUnlock()

	// Ad-hoc group members invited from a mutual-aid partner agency sit on this
	// channel under the CREATOR's agency key, so the agency-scoped fan-out above
	// misses them. Deliver by group membership instead.
	// Find the group by MEMBERSHIP, not by the speaker's agency key: the group
	// lives under the creator's key, so an invited partner talking would
	// otherwise match nothing and their audio would never leave their agency.
	if gkey := s.findAdhocForSpeaker(packet.Username, client.AgencyID, client.Channel); gkey != "" {
		members := s.groupMgr.adhocMembersOf(gkey)
		creator := ""
		if a := s.groupMgr.adhocInfo(gkey); a != nil {
			creator = a.createdBy
		}
		// Members are callsigns; resolve them only in the group's agency and
		// the agencies linked to it, never a same-named user elsewhere.
		groupAgency := agencyFromKey(gkey)
		reach := s.mutualAidAgencies(groupAgency)
		s.mu.RLock()
		for _, u := range append(members, creator) {
			if u == "" || u == packet.Username {
				continue
			}
			mc := s.findClientInAgencies(u, reach, groupAgency)
			if mc == nil || sentTo[mc.Addr.String()] || s.gatewayPaused(mc) {
				continue
			}
			s.sendPacket(audioData, mc.Addr)
			sentTo[mc.Addr.String()] = true
			relayCount++
		}
		s.mu.RUnlock()
	}

	// Mutual aid partners (looked up above) — also relay to partner agencies' same channel
	if len(partners) > 0 {
		log.Printf("Audio mutual aid: %s [%s] ch %d has partners %v", packet.Username, client.AgencyID, client.Channel, partners)
	}
	for _, partnerAgency := range partners {
		partnerKey := channelKey(partnerAgency, client.Channel)
		partnerChannel := partnerChannels[partnerKey]
		if partnerChannel != nil {
			partnerChannel.mu.RLock()
			partnerCount := 0
			for _, otherClient := range partnerChannel.Clients {
				if sentTo[otherClient.Addr.String()] || s.gatewayPaused(otherClient) {
					continue // already received it
				}
				s.sendPacket(audioData, otherClient.Addr)
				sentTo[otherClient.Addr.String()] = true
				partnerCount++
				relayCount++
			}
			partnerChannel.mu.RUnlock()
			log.Printf("Audio mutual aid: relayed to %d clients in %s", partnerCount, partnerKey)
		} else {
			log.Printf("Audio mutual aid: channel %s NOT FOUND", partnerKey)
		}

		// Mutual aid is inherently CROSS-agency, so the same-agency monitor
		// fan-out above never reaches the partner side. Deliver to partner-agency
		// clients who MONITOR this channel but aren't tuned to it — otherwise
		// always-listen on a mutual-aid channel hears nothing from the partner.
		monitorCount := 0
		s.mu.RLock()
		for _, mc := range s.clients {
			if mc.AgencyID != partnerAgency || sentTo[mc.Addr.String()] {
				continue
			}
			for _, ch := range mc.MonitoredChannels {
				if ch == client.Channel {
					s.sendPacket(monitorAudioData(), mc.Addr)
					sentTo[mc.Addr.String()] = true
					monitorCount++
					relayCount++
					break
				}
			}
		}
		s.mu.RUnlock()
		if monitorCount > 0 {
			log.Printf("Audio mutual aid: relayed to %d monitor(s) in %s", monitorCount, partnerAgency)
		}
	}

	log.Printf("Relayed audio from %s [%s] to %d clients in channel %d (%d bytes)",
		packet.Username, client.AgencyID, relayCount, client.Channel, len(packet.AudioData))

	// Forward to a Zello bridge on this channel (Opus passthrough).
	s.forwardMeshAudioToZello(client.AgencyID, client.Channel, packet.Username, packet.Codec, packet.AudioData)

	// MCX Step 5: forward to the SIP gateway if this is the bridged channel
	// (RTP passthrough to the external MCX media plane).
	if s.sipGateway != nil {
		s.sipGateway.OnAudioPacket(channelKey(client.AgencyID, client.Channel), audioData)
	}

	// Dispatch consoles tuned to this channel (they decode Opus in the
	// browser; see dispatch_audio.go). Legacy PCM frames are not sent.
	if packet.Codec == CodecOpus {
		s.fanOutToDispatch(client.AgencyID, client.Channel, client.Username, packet.AudioData)
		s.recordAudio(client.AgencyID, client.Channel, client.Username, packet.AudioData)
	}
	s.noteDispatchTalk(client.AgencyID, client.Channel, client.Username)
}

func (s *Server) handleDirectAudioPacket(packet *DirectAudioPacket, addr *net.UDPAddr) {
	if !s.isAuthenticated(addr) {
		return
	}

	// The sender is whoever this address authenticated as; the recipient is
	// looked up in the sender's agency or one linked to it by mutual aid.
	s.mu.Lock()
	sender := s.senderClient(packet.Sender, addr)
	var recipient *Client
	var senderAgency, recipientAgency string
	var recipientAddr *net.UDPAddr
	if sender != nil {
		sender.LastHeartbeat = time.Now()
		senderAgency = sender.AgencyID
		recipient = s.peerClient(sender.AgencyID, packet.Recipient)
		if recipient != nil {
			recipientAgency = recipient.AgencyID
			recipientAddr = recipient.Addr
		}
	}
	s.mu.Unlock()

	if sender == nil {
		log.Printf("Direct audio from unknown sender: %s (%s)", packet.Sender, addr)
		return
	}

	if recipient == nil {
		log.Printf("Direct audio to unknown recipient: %s from %s [%s] (offline, or another agency without mutual aid)", packet.Recipient, packet.Sender, senderAgency)
		return
	}

	// Create packet with type prepended
	audioData, err := CreateDirectAudioPacket(packet)
	if err != nil {
		log.Printf("Error creating direct audio packet: %v", err)
		return
	}

	// Send only to the specific recipient
	s.sendPacket(audioData, recipientAddr)

	log.Printf("Relayed direct audio from %s [%s] to %s [%s] (%d bytes)",
		packet.Sender, senderAgency, packet.Recipient, recipientAgency, len(packet.AudioData))
}

func (s *Server) handleTextPacket(packet *TextMessagePacket, addr *net.UDPAddr) {
	if !s.isAuthenticated(addr) {
		return
	}

	// SerializeTextPacket already starts with the type byte. This used to
	// prepend a second one, so every phone parsed a garbage sender length and
	// dropped the message: text over the server never arrived.
	textData := SerializeTextPacket(packet)

	// Resolve the sender from the authenticated address (never by the name in
	// the packet alone), and collect every recipient address under the lock:
	// the channel map and mutual-aid links change while messages are relayed.
	s.mu.Lock()
	client := s.senderClient(packet.Sender, addr)
	if client == nil {
		s.mu.Unlock()
		log.Printf("Text message from unknown client: %s (%s)", packet.Sender, addr)
		return
	}
	client.LastHeartbeat = time.Now()
	agencyID, chNum := client.AgencyID, client.Channel

	// A private message goes to its recipient only. It used to be relayed to
	// everyone on the sender's channel (and mutual-aid partners), and clients
	// did not filter, so other members saw it as if it were sent to them.
	if packet.Recipient != "" {
		var raddr *net.UDPAddr
		if rc := s.peerClient(agencyID, packet.Recipient); rc != nil {
			raddr = rc.Addr
		}
		s.mu.Unlock()
		if raddr != nil {
			s.sendPacket(textData, raddr)
			log.Printf("Relayed private text message from %s [%s] to %s", packet.Sender, agencyID, packet.Recipient)
		} else {
			log.Printf("Private text from %s to %s not delivered (recipient offline or other agency)", packet.Sender, packet.Recipient)
		}
		return
	}

	channel := s.channels[channelKey(agencyID, chNum)]
	if channel == nil {
		s.mu.Unlock()
		return
	}
	var addrs []*net.UDPAddr
	channel.mu.RLock()
	for _, otherClient := range channel.Clients {
		addrs = append(addrs, otherClient.Addr)
	}
	channel.mu.RUnlock()

	// Mutual aid partners — also relay to partner agencies' same channel
	for _, partnerAgency := range s.getMutualAidPartners(agencyID, chNum) {
		if partnerChannel := s.channels[channelKey(partnerAgency, chNum)]; partnerChannel != nil {
			partnerChannel.mu.RLock()
			for _, otherClient := range partnerChannel.Clients {
				addrs = append(addrs, otherClient.Addr)
			}
			partnerChannel.mu.RUnlock()
		}
	}
	s.mu.Unlock()

	for _, a := range addrs {
		s.sendPacket(textData, a)
	}

	log.Printf("Relayed text message from %s [%s] to %d clients in channel %d",
		packet.Sender, agencyID, len(addrs)-1, chNum)
}

func (s *Server) handleHeartbeat(packet *HeartbeatPacket, addr *net.UDPAddr) {
	// An address that has not logged in (a new NAT mapping, a network switch,
	// a server restart) gets no answer at all. That silence is exactly what
	// every client's liveness watchdog treats as "link dead": after 3.5
	// heartbeat intervals it reconnects and logs in again (Android
	// ServerManager.handleConnectionLost -> reconnectLoop; a network-change
	// callback does the same at once). An unsolicited failed AUTH_RESPONSE
	// would NOT trigger that — the app only shows an "Authentication failed"
	// toast — so silence is the correct "re-login needed" signal.
	if !s.isAuthenticated(addr) {
		return
	}

	lowPower := packet.Flags&HeartbeatFlagLowPower != 0

	s.mu.Lock()
	client := s.senderClient(packet.Username, addr)
	if client == nil {
		blocked := s.senderBlocked(packet.Username, addr)
		s.mu.Unlock()
		if blocked {
			// The callsign is online under another login account (or is a
			// dispatch console): not this address's to refresh or re-home.
			log.Printf("Heartbeat for %s from %s dropped: callsign belongs to another session", packet.Username, addr)
			return
		}
		// Client not registered (record expired, or the server restarted while
		// the client stayed authenticated) — re-register it on the channel it
		// was last on. A heartbeat carries only the username, so the old code
		// put it on channel 0: the client believed it was still transmitting
		// on its channel while the server relayed its audio to channel 0, and
		// nothing on either side flagged it.
		ch := s.lastChannelFor(packet.Username, addr)
		log.Printf("Heartbeat from unregistered client %s — auto-registering on channel %d", packet.Username, ch)
		s.handleJoin(&JoinPacket{Username: packet.Username, Channel: ch}, addr)
		if lowPower {
			s.mu.Lock()
			c := s.senderClient(packet.Username, addr)
			if c != nil {
				c.LowPower = true
			}
			s.mu.Unlock()
			if c != nil {
				s.sendPacket(CreateHeartbeatPacketFlags(packet.Username, HeartbeatFlagLowPower), addr)
			}
		}
		return
	}

	// Update address in case it changed (e.g. mobile network switch).
	// senderClient only returns a record at another address when this
	// address authenticated as that record's own login account.
	s.adoptSenderAddr(client, addr, "heartbeat")
	client.LastHeartbeat = time.Now()
	client.LowPower = lowPower
	client.EmergencyAware = packet.Flags&HeartbeatFlagEmergencyChannel != 0
	agencyID, chNum, username := client.AgencyID, client.Channel, client.Username

	// Everyone the heartbeat is relayed to, collected under the locks: the
	// channel's member map is written by JOIN/LEAVE concurrently, and reading
	// it unlocked was a fatal "concurrent map iteration and map write".
	channel := s.channels[channelKey(agencyID, chNum)]
	if channel == nil {
		s.mu.Unlock()
		return
	}
	var relayTo []*net.UDPAddr
	channel.mu.RLock()
	for _, otherClient := range channel.Clients {
		// Low-power clients take presence from the roster instead: relaying
		// every heartbeat to them kept their phone's cellular radio from ever
		// idling.
		if otherClient.Username != username && !otherClient.LowPower {
			relayTo = append(relayTo, otherClient.Addr)
		}
	}
	channel.mu.RUnlock()
	// Mutual aid partners — also relay heartbeat to partner agencies' same channel
	for _, partnerAgency := range s.getMutualAidPartners(agencyID, chNum) {
		if partnerChannel := s.channels[channelKey(partnerAgency, chNum)]; partnerChannel != nil {
			partnerChannel.mu.RLock()
			for _, otherClient := range partnerChannel.Clients {
				if !otherClient.LowPower {
					relayTo = append(relayTo, otherClient.Addr)
				}
			}
			partnerChannel.mu.RUnlock()
		}
	}
	s.mu.Unlock()

	// Create heartbeat packet
	heartbeatData, err := CreateHeartbeatPacket(packet.Username)
	if err != nil {
		log.Printf("Error creating heartbeat packet: %v", err)
		return
	}

	// Ack the sender's own heartbeat. Without this a LONE client (the only one
	// connected) receives nothing back on a quiet server, so its "no packets
	// received in 3.5 intervals" liveness check false-trips → disconnect →
	// reconnect loop (and a repeating disconnect tone). Echoing the heartbeat
	// keeps lastServerPacketTime fresh for every client, alone or not.
	// A low-power client gets the flag back, which tells it this server
	// supports the low-power stream.
	if lowPower {
		s.sendPacket(CreateHeartbeatPacketFlags(packet.Username, HeartbeatFlagLowPower), addr)
	} else {
		s.sendPacket(heartbeatData, addr)
	}

	for _, a := range relayTo {
		s.sendPacket(heartbeatData, a)
	}

	if len(relayTo) > 0 {
		log.Printf("Relayed heartbeat from %s [%s] to %d clients in channel %d",
			packet.Username, agencyID, len(relayTo), chNum)
	}
}

func (s *Server) handleJoin(packet *JoinPacket, addr *net.UDPAddr) {
	if !s.isAuthenticated(addr) {
		log.Printf("JOIN rejected: %q from %s (not authenticated)", packet.Username, addr)
		return
	}
	if !validCallsign(packet.Username) {
		log.Printf("JOIN rejected: callsign %q from %s has characters that are not allowed", packet.Username, addr)
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

	// The agency is the signed-in session's, never guessed from the name.
	agencyID := s.authenticatedAddrs[addr.String()]
	if agencyID == "" {
		s.mu.Unlock()
		log.Printf("JOIN rejected: %s from %s (session ended)", packet.Username, addr)
		return
	}

	// The login account this address authenticated as.
	account := s.authAccounts[addr.String()]
	// A callsign that is the NAME of an account in this agency — a unit's
	// login, or an LMR bridge's gateway — belongs to that account. Anyone
	// could JOIN as an offline unit or gateway before: the squatter received
	// its private messages, was treated as the gateway, and the real device
	// was then refused for as long as the squatter stayed.
	if owner := s.callsignOwnerLocked(agencyID, packet.Username); owner != "" && owner != account {
		s.mu.Unlock()
		if !s.bridgeGuestGate.note("join#"+addr.String(), time.Now()) {
			return // already said so a moment ago
		}
		log.Printf("JOIN refused: %q [%s] from %s — that callsign is the name of another account (signed in as %s)",
			packet.Username, agencyID, addr, account)
		s.activityLog.Add("join_refused", packet.Username,
			"Callsign belongs to another account; signed in as "+strings.TrimPrefix(account, agencyID+":"), agencyID)
		s.sendPacket(SerializeTextPacket(&TextMessagePacket{Sender: "Server", Recipient: packet.Username,
			Content:   "The callsign \"" + packet.Username + "\" belongs to another account. Change your callsign (Profile) or sign in with that account.",
			Timestamp: time.Now().UnixMilli()}), addr)
		return
	}

	// Check if client already exists
	ck := userKey(agencyID, packet.Username)
	client, exists := s.clients[ck]
	if exists && !s.mayActAs(client, addr) {
		// The callsign is online under a different login account (or is a
		// dispatch console). Taking it over would steal its audio, its queued
		// dispatch messages and — for an LMR gateway — the radio bridge. The
		// SAME account re-joining from a new address (network switch, stale
		// old session) passes mayActAs and moves the record as before.
		s.mu.Unlock()
		log.Printf("JOIN refused: %s [%s] from %s — callsign is online under another account (%s at %v)",
			packet.Username, agencyID, addr, client.Account, client.Addr)
		return
	}
	if s.lastChannel == nil {
		s.lastChannel = make(map[string]int)
	}
	if s.lastChannel[ck] != packet.Channel {
		s.lastChannel[ck] = packet.Channel
		s.scheduleSaveLastChannels()
	}
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

		// Moved to a new address (network switch, new port): the old one
		// must not stay signed in — another device can be handed that
		// ip:port by the same carrier NAT.
		if client.Addr != nil && client.Addr.String() != addr.String() {
			s.dropAddrAuthLocked(client.Addr.String(), client)
		}
		client.Channel = packet.Channel
		client.AgencyID = agencyID
		client.Addr = addr
		client.LastHeartbeat = time.Now()
		if account != "" {
			client.Account = account
		}
	} else {
		// New client
		client = &Client{
			Username:      packet.Username,
			AgencyID:      agencyID,
			Account:       account,
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

	// The agency's channel count and names (phones lock and show these).
	if a := s.agencies[agencyID]; a != nil {
		toSend = append(toSend, pendingPacket{buildChannelNames(a), addr,
			fmt.Sprintf("Sent channel names (%d) to %s [%s]", a.talkgroupCount(), packet.Username, agencyID)})
	}

	// The EMERGENCY channel (always heard, listen-only).
	if a := s.agencies[agencyID]; a != nil && s.lmrBridgeByUsername(agencyID, packet.Username) == nil {
		toSend = append(toSend, pendingPacket{buildEmergencyChannel(a), addr,
			fmt.Sprintf("Sent emergency channel to %s [%s]", packet.Username, agencyID)})
	}

	// Channels to announce to the joining client (mutual aid, Zello, LMR).
	for _, a := range s.joinAnnouncements(agencyID, packet.Username, packet.Channel) {
		pkt := SerializeChannelSwitch(&ChannelSwitchPacket{Channel: a.channel, ChannelName: a.name, Active: true})
		toSend = append(toSend, pendingPacket{pkt, addr, fmt.Sprintf("Sent %s channel %d (%s) to newly joined %s [%s]", a.kind, a.channel, a.name, packet.Username, agencyID)})
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
	s.deliverPending(agencyID, packet.Username, addr)

	// MCX Step 3: if floor control is on and the floor is taken on the joined
	// channel, tell the joining client so its UI reflects the current holder.
	if s.floor != nil && s.floorEnabledForAgency(agencyID) {
		if taken := s.floor.FloorStatusFor(channelKey(agencyID, packet.Channel), packet.Channel); taken != nil {
			s.sendPacket(taken, addr)
		}
	}
	// MCX Step 4: tell the joining client the channel's MCX group type (broadcast/
	// emergency) so its UI shows the right indicator. No-op for normal channels.
	s.sendGroupInfo(addr, agencyID, packet.Channel)

	// Push an updated roster so everyone in the agency sees the new client.
	if !exists {
		s.broadcastRoster(agencyID)
	}
}

func (s *Server) handleLeave(addr *net.UDPAddr) {
	if !s.isAuthenticated(addr) {
		return
	}

	s.mu.Lock()

	// Find client by address
	var clientToRemove *Client
	for _, client := range s.clients {
		if client.Addr.String() == addr.String() {
			clientToRemove = client
			break
		}
	}

	leftAgency := ""
	if clientToRemove != nil {
		leftAgency = clientToRemove.AgencyID
		log.Printf("LEAVE: removing client %s [%s] from channel %d (addr: %s)", clientToRemove.Username, clientToRemove.AgencyID, clientToRemove.Channel, addr)
		s.removeClient(clientToRemove)
	}
	s.mu.Unlock()

	if clientToRemove != nil {
		s.clientRemovedCleanup(clientToRemove)
	}

	// Push an updated roster so everyone in the agency sees the departure.
	if leftAgency != "" {
		s.broadcastRoster(leftAgency)
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
		s.forgetAddrAuthLocked(client.Addr.String())
	}

	// Remove from clients map
	delete(s.clients, ck)
	log.Printf("Client %s [%s] left channel %d", client.Username, client.AgencyID, client.Channel)
	s.activityLog.Add("client_disconnect", client.Username, fmt.Sprintf("Left channel %d", client.Channel), client.AgencyID)
}

// clientRemovedCleanup finishes a removeClient. Call it AFTER releasing s.mu:
// handing the floor on and dissolving an ad-hoc group both take s.mu to reach
// the channel's clients, and sync.RWMutex is not reentrant. Run under the lock
// they froze the whole server (AWS, 2026-11-06 04:27 UTC: the last member left
// an ad-hoc group; logins, audio and heartbeats all hung until a restart).
func (s *Server) clientRemovedCleanup(client *Client) {
	// MCX Step 3: release any floor/queue this user held (prevents a stuck floor).
	if s.floor != nil {
		s.floor.DropUser(client.AgencyID, client.Username)
	}
	// MCX Step 4: leaving an ad-hoc group channel dissolves it when empty.
	if s.groupMgr != nil {
		s.adhocMemberLeft(client.AgencyID, client.Channel, client.Username)
	}
}

// broadcastRoster sends the authoritative list of connected usernames in an
// agency to every client in that agency. Called on join/leave and periodically
// so clients can show an accurate online-users list in server mode. Must be
// called WITHOUT s.mu held (it takes an RLock internally, then sends after).
// mutualAidAgencies returns every agency reachable from agencyID over an ACTIVE
// mutual-aid link (on any channel), including agencyID itself. Two agencies
// sharing a link are working the same incident, so their users need to see each
// other — for the roster, and for building an ad-hoc group across both.
// Caller must NOT hold s.mu (it takes RLock itself).
func (s *Server) mutualAidAgencies(agencyID string) map[string]bool {
	out := map[string]bool{agencyID: true}
	if agencyID == "" {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mutualAidAgenciesLocked(agencyID)
}

// mutualAidAgenciesLocked is mutualAidAgencies for callers already holding s.mu.
func (s *Server) mutualAidAgenciesLocked(agencyID string) map[string]bool {
	out := map[string]bool{agencyID: true}
	if agencyID == "" {
		return out
	}
	for _, link := range s.mutualAidLinks {
		if !link.Active {
			continue
		}
		member := false
		for _, a := range link.Agencies {
			if a == agencyID {
				member = true
				break
			}
		}
		if !member {
			continue
		}
		for _, a := range link.Agencies {
			if a != "" {
				out[a] = true
			}
		}
	}
	return out
}

// Heartbeat timing. Legacy clients heartbeat every 10 s. Low-power clients
// answer a server ping sent once they have been quiet for lowPowerPingAfter
// (15-25 s with the 10 s checker), so their NAT mapping is refreshed well
// inside common 30 s carrier UDP timeouts.
const (
	heartbeatTimeout         = 30 * time.Second
	lowPowerHeartbeatTimeout = 75 * time.Second
	lowPowerPingAfter        = 15 * time.Second
	lowPowerRosterRefresh    = 5 * time.Minute
)

// broadcastRoster sends the roster to every client of the agency (a join,
// leave or link change).
func (s *Server) broadcastRoster(agencyID string) {
	s.sendRoster(agencyID, true)
}

// broadcastAllRostersPeriodic is the 10 s safety-net refresh: legacy clients
// get it every time (their presence expires after 30 s without it); low-power
// clients only when the list changed or lowPowerRosterRefresh has passed.
func (s *Server) broadcastAllRostersPeriodic() {
	s.mu.RLock()
	agencies := make(map[string]bool)
	for _, c := range s.clients {
		if c.AgencyID != "" {
			agencies[c.AgencyID] = true
		}
	}
	s.mu.RUnlock()
	for agencyID := range agencies {
		s.sendRoster(agencyID, false)
	}
}

func (s *Server) sendRoster(agencyID string, all bool) {
	if agencyID == "" {
		return
	}

	s.mu.RLock()
	// Include mutual-aid partners: linked agencies are working together, so
	// their users belong in each other's roster (otherwise they're invisible
	// for direct calls and ad-hoc group building).
	visible := s.mutualAidAgenciesLocked(agencyID)
	names := make([]string, 0, len(s.clients))
	addrs := make([]*net.UDPAddr, 0, len(s.clients))
	lowPower := make([]bool, 0, len(s.clients))
	for _, c := range s.clients {
		if !visible[c.AgencyID] {
			continue
		}
		// A /command console is not a unit: phones reach it through their
		// "Dispatch" conversation (packet 0x30). Listing it here showed the
		// dispatcher's login as a second contact that could not be messaged
		// (private texts are channel-encrypted; the console can't read them).
		if isConsoleAddr(c.Addr) {
			continue
		}
		// Tag users from a PARTNER agency with that agency's name, so a group
		// builder can tell whose people are whose. Own-agency users stay bare.
		name := c.Username
		if c.AgencyID != agencyID {
			label := c.AgencyID
			if ag, ok := s.agencies[c.AgencyID]; ok && ag.Name != "" {
				label = ag.Name
			}
			name = c.Username + rosterAgencySuffix + label + ")"
		}
		names = append(names, name)
		// The roster packet only goes to OUR agency's clients; partners get
		// their own roster built from their side of the link.
		if c.AgencyID == agencyID {
			addrs = append(addrs, c.Addr)
			lowPower = append(lowPower, c.LowPower)
		}
	}
	s.mu.RUnlock()

	if len(addrs) == 0 {
		return
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	sig := strings.Join(sorted, "\n")
	s.rosterMu.Lock()
	if s.rosterSent == nil {
		s.rosterSent = make(map[string]rosterState)
	}
	prev := s.rosterSent[agencyID]
	toLowPower := all || prev.sig != sig || time.Since(prev.at) >= lowPowerRosterRefresh
	if toLowPower {
		s.rosterSent[agencyID] = rosterState{sig: sig, at: time.Now()}
	}
	s.rosterMu.Unlock()

	packet := SerializeRosterPacket(names)
	for i, a := range addrs {
		if lowPower[i] && !toLowPower {
			continue
		}
		s.sendPacket(packet, a)
	}
}

// rosterState is the last roster sent to an agency's low-power clients.
type rosterState struct {
	sig string
	at  time.Time
}

func (s *Server) sendPacket(data []byte, addr *net.UDPAddr) {
	// An address with a secure session gets everything encrypted and
	// authenticated for that session (secure.go).
	if addr != nil && s.sec.live() {
		if ss := s.secSessionForAddr(addr.String()); ss != nil {
			data = ss.seal(data)
		}
	}
	s.sendRaw(data, addr)
}

// sendRaw writes bytes to an address as they are (handshake packets, and
// packets sendPacket already wrapped).
func (s *Server) sendRaw(data []byte, addr *net.UDPAddr) {
	// Test hook / no-conn guard: capture instead of writing to the socket.
	if s.testFloorSink != nil {
		s.testFloorSink(addr, data)
		return
	}
	// A dispatch console's loopback address: deliver over its WebSocket.
	if deliverToDispatch(data, addr) {
		return
	}
	if s.conn == nil {
		return
	}
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
	if !s.isAuthenticated(senderAddr) {
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
			// Periodic roster refresh: pushes the authoritative online list to
			// all agencies (safety net for any missed join/leave/timeout event).
			// Low-power clients only get it when it changed (or every
			// lowPowerRosterRefresh), not every tick.
			s.broadcastAllRostersPeriodic()
		}
	}
}

func (s *Server) checkHeartbeats() {
	s.mu.Lock()

	now := time.Now()

	var toRemove []*Client
	var toPing []*Client
	for _, client := range s.clients {
		timeout := heartbeatTimeout
		if client.LowPower {
			timeout = lowPowerHeartbeatTimeout
		}
		silent := now.Sub(client.LastHeartbeat)
		if silent > timeout {
			toRemove = append(toRemove, client)
		} else if client.LowPower && silent > lowPowerPingAfter && now.Sub(client.lastPing) >= lowPowerPingAfter {
			client.lastPing = now
			toPing = append(toPing, client)
		}
	}

	for _, client := range toRemove {
		log.Printf("Client %s timed out (no heartbeat for %v)", client.Username, now.Sub(client.LastHeartbeat).Round(time.Second))
		s.removeClient(client)
	}
	s.mu.Unlock()

	for _, client := range toRemove {
		s.clientRemovedCleanup(client)
	}

	// Keepalive for low-power clients: a phone that lets its CPU sleep cannot
	// run a heartbeat timer, so the server pings it and the incoming packet
	// wakes it to answer. The answer also keeps the client's NAT mapping open.
	for _, client := range toPing {
		s.sendPacket(CreateHeartbeatPacketFlags(client.Username, HeartbeatFlagLowPower), client.Addr)
	}

	// The periodic broadcastAllRostersPeriodic() in heartbeatChecker will push the
	// updated list; no per-agency broadcast needed here.
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
	if s.sipGateway != nil {
		s.sipGateway.Stop()
	}
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
		"total_clients":    clientCount,
		"packets_received": s.stats.PacketsReceived,
		"packets_sent":     s.stats.PacketsSent,
		"bytes_received":   s.stats.BytesReceived,
		"bytes_sent":       s.stats.BytesSent,
		"uptime_seconds":   int(uptime.Seconds()),
		"start_time":       s.stats.StartTime,
		"version":          Version,
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
		// The login account can differ from the callsign shown on the air.
		account := client.Username
		if client.Addr != nil {
			if k := s.authAccounts[client.Addr.String()]; k != "" {
				account = k[strings.LastIndex(k, ":")+1:]
			}
		}
		clients = append(clients, map[string]interface{}{
			"username": client.Username,
			"account":  account,
			// true = this phone uses the secure protocol (secure.go).
			"secure":    client.Addr != nil && s.sec.live() && s.addrIsSecure(client.Addr.String()),
			"agency_id": client.AgencyID,
			// No IP address: the list reaches every admin's and dispatcher's
			// browser, and a phone's public IP is nobody else's business.
			"channel":        client.Channel,
			"connected_at":   client.ConnectedAt,
			"last_heartbeat": client.LastHeartbeat,
		})
		if client.Loc != nil {
			clients[len(clients)-1]["location"] = client.Loc.json()
		}
		if client.Addr != nil && client.Addr.IP.Equal(net.IPv4(127, 0, 0, 2)) {
			clients[len(clients)-1]["console"] = true // a dispatch console, not a unit
		}
		if st := s.statusFor(client.AgencyID, client.Username); st != nil {
			clients[len(clients)-1]["status"] = st
		}
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
		label := channel.Name
		if channel.ID >= 0 && channel.ID < MaxAgencyTalkgroups {
			label = s.agencies[chAgencyID].talkgroupName(channel.ID) // nil-safe
		}
		channel.mu.RLock()
		channels = append(channels, map[string]interface{}{
			"id":           channel.ID,
			"name":         channel.Name,
			"label":        label,
			"agency_id":    chAgencyID,
			"client_count": len(channel.Clients),
		})
		channel.mu.RUnlock()
	}

	return channels
}

func (s *Server) handleGroupCreate(packet *GroupPacket, addr *net.UDPAddr) {
	s.relayGroupPacket(PacketTypeGroupCreate, packet, addr)
}

func (s *Server) handleGroupUpdate(packet *GroupPacket, addr *net.UDPAddr) {
	s.relayGroupPacket(PacketTypeGroupUpdate, packet, addr)
}

// relayGroupPacket stores a legacy group create/update and relays it to the
// creator's agency. The creator must be the client this address is signed in
// as: the agency used to come from a by-name search for packet.Creator, so
// anyone could push group packets into another agency.
func (s *Server) relayGroupPacket(packetType byte, packet *GroupPacket, addr *net.UDPAddr) {
	if !s.isAuthenticated(addr) {
		return
	}

	relayData, err := CreateGroupPacket(packetType, packet)
	if err != nil {
		log.Printf("Error creating group packet: %v", err)
		return
	}

	s.mu.Lock()
	creatorAgency := "default"
	if creator := s.senderClient(packet.Creator, addr); creator != nil {
		creatorAgency = creator.AgencyID
	} else {
		s.mu.Unlock()
		log.Printf("Group packet from %s dropped: creator %q is not this session's client", addr, packet.Creator)
		return
	}
	s.groups[packet.GroupID] = packet

	// Relay only to clients in the same agency
	var addrs []*net.UDPAddr
	for _, client := range s.clients {
		if client.AgencyID == creatorAgency {
			addrs = append(addrs, client.Addr)
		}
	}
	s.mu.Unlock()

	for _, a := range addrs {
		s.sendPacket(relayData, a)
	}
	log.Printf("Relayed group packet 0x%02X %q (creator %s) to %d clients [%s]", packetType, packet.GroupName, packet.Creator, len(addrs), creatorAgency)
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
// rosterAgencySuffix separates a username from the agency label the server adds
// for mutual-aid partners in the roster (e.g. "medic1 (Fire Dept)"). Clients
// echo the displayed string back when inviting or calling, so lookups must
// tolerate it. The username itself is unchanged on the wire.
const rosterAgencySuffix = " ("

// stripRosterLabel removes a trailing " (Agency)" decoration, if present.
func stripRosterLabel(name string) string {
	i := strings.LastIndex(name, rosterAgencySuffix)
	if i > 0 && strings.HasSuffix(name, ")") {
		return name[:i]
	}
	return name
}

// findClientForSender resolves the client record for a packet from addr.
//
// Client records are keyed per agency (userKey), but the old lookup matched on
// USERNAME alone across the whole map — and Go map iteration is random, so the
// same username in two agencies resolved to a different record from one packet
// to the next, and its audio was relayed to whichever agency/channel that record
// happened to be in. When the sender's address is authenticated we know its
// agency, so the record is looked up directly; the username-only search is only
// the fallback for unauthenticated (auth-disabled) servers.
func (s *Server) findClientForSender(username string, addr *net.UDPAddr) (*Client, string) {
	if addr != nil {
		if agencyID := s.authenticatedAddrs[addr.String()]; agencyID != "" {
			ck := userKey(agencyID, username)
			if c := s.clients[ck]; c != nil {
				return c, ck
			}
			if bare := stripRosterLabel(username); bare != username {
				ck = userKey(agencyID, bare)
				if c := s.clients[ck]; c != nil {
					return c, ck
				}
			}
		}
	}
	return s.findClientByUsername(username)
}

// lastChannelFor returns the channel a user last joined (per agency when the
// sender is authenticated), or 0 when unknown.
func (s *Server) lastChannelFor(username string, addr *net.UDPAddr) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var user *UserAccount
	if addr != nil {
		if agencyID := s.authenticatedAddrs[addr.String()]; agencyID != "" {
			if ch, ok := s.lastChannel[userKey(agencyID, username)]; ok {
				return ch
			}
			// Stay in the session's agency: a same-named account elsewhere
			// says nothing about this client.
			user = s.users[userKey(agencyID, username)]
			if user == nil {
				return 0
			}
		}
	}
	if user == nil {
		user = s.findUserByUsername(username)
	}
	if user != nil {
		if ch, ok := s.lastChannel[userKey(user.AgencyID, username)]; ok {
			return ch
		}
		// A bridge gateway belongs on its bridge's channel, always.
		if own := s.ownLMRBridge(&Client{Username: username, AgencyID: user.AgencyID}); own != nil {
			return own.Channel
		}
	}
	return 0
}

// --- last-channel persistence --------------------------------------------
//
// The channel each user last joined is kept across restarts. Without it, a
// client that never noticed the restart (UDP: its heartbeats were simply
// answered again) was auto-registered on channel 0 and its audio went nowhere
// — the Pi bridge carried radio traffic to channel 0 for ten minutes after
// the v4.2.10 update (scny, 2026-10-22 21:16 UTC).

func (s *Server) lastChannelsFile() string {
	return "last_channels.json"
}

func (s *Server) loadLastChannels() {
	data, err := os.ReadFile(s.lastChannelsFile())
	if err != nil {
		return
	}
	m := map[string]int{}
	if err := json.Unmarshal(data, &m); err != nil {
		log.Printf("last_channels.json unreadable: %v", err)
		return
	}
	s.mu.Lock()
	if s.lastChannel == nil {
		s.lastChannel = make(map[string]int)
	}
	for k, v := range m {
		s.lastChannel[k] = v
	}
	s.mu.Unlock()
	log.Printf("Loaded last channels for %d users", len(m))
}

// scheduleSaveLastChannels writes the map shortly after a change, coalescing
// bursts. Caller holds s.mu (write).
func (s *Server) scheduleSaveLastChannels() {
	if s.lastChannelSavePending {
		return
	}
	s.lastChannelSavePending = true
	snapshotFn := func() map[string]int {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lastChannelSavePending = false
		m := make(map[string]int, len(s.lastChannel))
		for k, v := range s.lastChannel {
			m[k] = v
		}
		return m
	}
	go func() {
		time.Sleep(2 * time.Second)
		m := snapshotFn()
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return
		}
		writeFileAtomic(s.lastChannelsFile(), data)
	}()
}

func (s *Server) findClientByUsername(username string) (*Client, string) {
	for key, client := range s.clients {
		if client.Username == username {
			return client, key
		}
	}
	// Retry without a roster agency label, so a client echoing the displayed
	// name ("medic1 (Fire Dept)") still resolves to the real user.
	if bare := stripRosterLabel(username); bare != username {
		for key, client := range s.clients {
			if client.Username == bare {
				return client, key
			}
		}
	}
	return nil, ""
}

// handleAuthChallengeRequest handles the first step of challenge-response auth (Phase 4f).
// Client sends username, server generates a nonce and sends it back.
func (s *Server) handleAuthChallengeRequest(packet *AuthChallengeRequestPacket, addr *net.UDPAddr) {
	// Stateless (ratelimit.go): nothing is stored, and the server lock is not
	// taken, so a flood of these cannot block sign-ins or slow the server.
	// Refused while the address is locked out for wrong passwords.
	if locked, _, _ := s.pttAuthLimit.blocked(addr.IP.String()); locked {
		return
	}
	s.sendPacket(SerializeAuthChallenge(challengeNonce(addr.String(), time.Now())), addr)
}

func (s *Server) handleAuthRequest(packet *AuthRequestPacket, addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Candidate accounts: the agency-scoped one when the client names its
	// agency, otherwise every account with that username (older clients send
	// no agency; the same username can exist in several agencies). Each
	// candidate is checked against ITS OWN password and enrollment tokens, so
	// the agency is decided by the credential, never by map order.
	var candidates []*UserAccount
	if packet.AgencyID != "" {
		if u := s.users[userKey(packet.AgencyID, packet.Username)]; u != nil {
			candidates = append(candidates, u)
		}
	}
	if len(candidates) == 0 {
		candidates = s.findUsersByUsername(packet.Username)
	}

	// Wrong-password limits (ratelimit.go). They count against the address,
	// and against the account(s) this request RESOLVES to — never the agency
	// text in the packet. Unknown user and wrong password get the same
	// answer, so the reply does not tell a stranger which usernames exist.
	ip := addr.IP.String()
	var accts []string
	for _, c := range candidates {
		accts = append(accts, strings.ToLower(userKey(c.AgencyID, c.Username)))
	}
	if len(accts) == 0 {
		accts = []string{"?:" + strings.ToLower(strings.TrimSpace(packet.Username))}
	}
	if locked, left, what := s.pttAuthLimit.blocked(ip, accts...); locked {
		log.Printf("Auth refused: %s locked for %s more (too many wrong passwords) — user %s from %s",
			what, left.Round(time.Second), packet.Username, addr)
		resp := SerializeAuthResponse(&AuthResponsePacket{Success: false, Message: "Too many attempts — try again in a few minutes"})
		s.sendPacket(resp, addr)
		return
	}
	authFailed := func() {
		if s.pttAuthLimit.failed(ip, accts...) {
			log.Printf("Auth lockout: too many wrong passwords (user %s from %s)", packet.Username, addr)
		}
		resp := SerializeAuthResponse(&AuthResponsePacket{Success: false, Message: "Invalid username or password"})
		s.sendPacket(resp, addr)
	}

	if len(candidates) == 0 {
		log.Printf("Auth failed: unknown user %s from %s", packet.Username, addr)
		authFailed()
		return
	}

	// Challenge-response: the client sends HMAC-SHA256(stored hash, nonce) for
	// the nonce this address was given (current or previous time slot).
	var (
		user            *UserAccount
		usedEnrollToken string
		deviceID        string
		authOk          bool
		nonce           [32]byte
	)
	for _, n := range challengeNonces(addr.String(), time.Now()) {
		if user, usedEnrollToken, deviceID, authOk = s.matchCredential(candidates, n, packet.PasswordHash, addr); authOk {
			nonce = n
			break
		}
	}
	if user == nil {
		user = candidates[0] // for the log line only
	}

	if !authOk {
		log.Printf("Auth failed: wrong password for user %s from %s", packet.Username, addr)
		authFailed()
		return
	}
	s.pttAuthLimit.succeeded(ip, strings.ToLower(userKey(user.AgencyID, user.Username)))

	msg, err := s.completeSignInLocked(user, usedEnrollToken, deviceID, addr, &legacyKeyWrap{nonce: nonce, flags: packet.Flags})
	if err != nil {
		s.sendPacket(SerializeAuthResponse(&AuthResponsePacket{Success: false, Message: "Server error — try again"}), addr)
		return
	}
	// The address now speaks the old protocol: any secure session it had is over.
	s.secDropAddr(addr.String())
	resp := SerializeAuthResponse(&AuthResponsePacket{Success: true, Message: msg})
	s.sendPacket(resp, addr)
}

// legacyKeyWrap carries what the OLD handshake needs to deliver a device key
// (wrapped under the QR token). The secure handshake passes nil: its reply is
// already encrypted, so the key goes as it is.
type legacyKeyWrap struct {
	nonce [32]byte
	flags byte
}

// completeSignInLocked finishes a sign-in that proved a credential, for both
// handshakes: device-key issue for a QR, session bookkeeping, last login. It
// returns the success message ("Authenticated|domain=…|devkey=…"). Caller
// holds s.mu.
func (s *Server) completeSignInLocked(user *UserAccount, usedEnrollToken, deviceID string, addr *net.UDPAddr, legacy *legacyKeyWrap) (string, error) {
	// Which credential this session used: ends with it (account_security.go).
	cred := credPassword
	devSuffix := ""
	now := time.Now()
	how := "challenge-response"
	if legacy == nil {
		how = "secure"
	}
	switch {
	case usedEnrollToken != "" && s.enrollmentTokens[usedEnrollToken].IsGuest:
		// Guest event QR: shared by everyone at the event, ends with it.
		cred = credGuest
		log.Printf("Auth (guest token, %s) success: user %s from %s — guest event login", how, user.Username, addr)
	case usedEnrollToken != "":
		// One-time QR: this phone gets its own key and the token is spent.
		et := s.enrollmentTokens[usedEnrollToken]
		var nonce [32]byte
		flags := AuthFlagDeviceKey // the secure handshake always takes a device key
		if legacy != nil {
			nonce, flags = legacy.nonce, legacy.flags
		}
		c, suffix, err := s.issueDeviceKeyLocked(user, et, nonce, flags, addr, legacy == nil)
		if err != nil {
			log.Printf("Auth (enrollment token) for %s from %s: could not issue a device key: %v", user.Username, addr, err)
			return "", err
		}
		cred, devSuffix = c, suffix
		s.saveUsers()
		kind := "device key issued"
		if suffix == "" {
			kind = "older app: token kept as this phone's key (update the app for one-time QR codes)"
		}
		log.Printf("Auth (enrollment token, %s) success: user %s [%s] from %s — QR used, %s", how, user.Username, user.AgencyID, addr, kind)
		s.activityLog.Add("qr_enrolled", user.Username, fmt.Sprintf("QR code used from %s (%s)", addr.IP, kind), user.AgencyID)
	case deviceID != "":
		cred = deviceCred(deviceID)
		for _, d := range user.Devices {
			if d.ID == deviceID {
				d.LastUsed = now.Format(time.RFC3339)
			}
		}
		log.Printf("Auth (device key, %s) success: user %s from %s", how, user.Username, addr)
	default:
		log.Printf("Auth (%s) success: user %s from %s", how, user.Username, addr)
	}

	// A phone that moved addresses re-JOINs and the JOIN clears its old
	// address; nothing here may touch another device's session.
	s.recordAuthLocked(addr.String(), user.AgencyID, userKey(user.AgencyID, user.Username), cred)
	// Last login, for the dashboard. Reconnects are frequent (network
	// switches, heartbeats lost), so the file is rewritten at most every few
	// minutes per account; the in-memory value is always current.
	prev, _ := time.Parse(time.RFC3339, user.LastLogin)
	user.LastLogin = now.Format(time.RFC3339)
	if now.Sub(prev) > 5*time.Minute {
		s.saveUsers()
	}
	// MCX Step 2: hand the client its MCX domain so it can derive its own MCX id
	// (<callsign>@<domain>). Appended in a backward-compatible way — old clients
	// read the "Authenticated" prefix and ignore the "|domain=" suffix.
	msg := "Authenticated"
	if dom := agencyDomain(user.AgencyID, s.config.McpttDomain); dom != "" {
		msg = "Authenticated|domain=" + dom
	}
	return msg + devSuffix, nil // "|devkey=…" after a one-time QR
}

// matchCredential finds the account (among candidates) whose password, or one
// of whose enrollment / guest tokens, produced the client's HMAC over nonce.
// A token only ever authenticates the account it was issued for: same
// username AND same agency. Matching on username alone let agency A's QR
// code log in as (and rebind the password of) a same-named user in agency B.
// Caller holds s.mu.
func (s *Server) matchCredential(candidates []*UserAccount, nonce [32]byte, clientHmac string, addr *net.UDPAddr) (user *UserAccount, token, deviceID string, ok bool) {
	matches := func(hash string) bool {
		if hash == "" {
			return false
		}
		expected := computeHmacSha256([]byte(hash), nonce[:])
		return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(expected)), []byte(clientHmac)) == 1
	}
	for _, u := range candidates {
		if matches(u.PasswordHash) {
			return u, "", "", true
		}
		for _, d := range u.Devices {
			if matches(d.Hash) {
				return u, "", d.ID, true
			}
		}
	}
	now := time.Now()
	for _, u := range candidates {
		for t, et := range s.enrollmentTokens {
			if et.Username != u.Username || et.AgencyID != u.AgencyID || !enrollTokenUsable(et, addr, now) {
				continue
			}
			// The client used the enrollment token as a password:
			// client computes SHA256(token), then HMAC(SHA256(token), nonce)
			if matches(hashPassword(et.Token)) {
				return u, t, "", true
			}
		}
	}
	return nil, "", "", false
}

// findUsersByUsername returns every account with this username, in a stable
// (agency) order. Caller holds s.mu.
func (s *Server) findUsersByUsername(username string) []*UserAccount {
	var out []*UserAccount
	for _, u := range s.users {
		if u.Username == username {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgencyID < out[j].AgencyID })
	return out
}

// computeHmacSha256 computes HMAC-SHA256(key, data).
func computeHmacSha256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// --- Enrollment Tokens ---

// GenerateEnrollmentToken creates a one-time QR token (128-bit) for the
// account. It works for one sign-in within 15 minutes (account_security.go).
// Showing the QR again within that window returns the same unused token; it
// never extends the window. Must be called while s.mu is held.
func (s *Server) GenerateEnrollmentToken(username, agencyID, passwordHash string) string {
	now := time.Now()
	for _, et := range s.enrollmentTokens {
		if et.Username == username && et.AgencyID == agencyID && !et.IsGuest &&
			et.UsedAt.IsZero() && now.Before(et.ExpiresAt) {
			return et.Token
		}
	}
	token, err := randomHexBytes(16)
	if err != nil {
		log.Printf("Failed to generate enrollment token: %v", err)
		return ""
	}
	s.enrollmentTokens[token] = &EnrollmentToken{
		Token:        token,
		Username:     username,
		AgencyID:     agencyID,
		PasswordHash: passwordHash,
		ExpiresAt:    now.Add(enrollTokenLife),
	}
	log.Printf("Enrollment QR created for user %s (agency: %s): one use, expires in 15 minutes", username, agencyID)
	return token
}

// cleanupExpiredTokens removes expired enrollment tokens and guest events. Called periodically.
func (s *Server) cleanupExpiredTokens() {
	s.mu.Lock()

	now := time.Now()
	for token, et := range s.enrollmentTokens {
		if now.After(et.ExpiresAt) && !et.IsGuest {
			delete(s.enrollmentTokens, token)
			log.Printf("Enrollment token expired for user %s", et.Username)
		}
	}

	// Check for expired guest events and remove them (releases and re-acquires lock internally)
	s.cleanupExpiredGuestEvents()

	// Sign-ins that never joined, and spent one-time QR tokens.
	s.pruneAuthLocked(now)
	defer s.secReap(now) // after the lock is released

	// Guests invited from the app whose time is up.
	kicks := s.removeExpiredInvitedGuests()

	s.mu.Unlock()
	s.finishExpiredGuestKicks(kicks)
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

// saveUsers writes users.json (owner-only, atomic). Caller holds s.mu (read or
// write): it iterates s.users. Callers that have already released s.mu use
// saveUsersUnlocked — iterating the map unlocked raced with logins and edits
// ("concurrent map iteration and map write" is fatal in Go).
func (s *Server) saveUsers() error {
	users := make([]*UserAccount, 0, len(s.users))
	for _, user := range s.users {
		users = append(users, user)
	}

	return writeJSONFileAtomic(s.config.UsersFile, users)
}

// saveUsersUnlocked is saveUsers for callers that do NOT hold s.mu.
func (s *Server) saveUsersUnlocked() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveUsers()
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
	kicks, err := s.updatePasswordLocked(agencyID, username, password)
	s.mu.Unlock()
	// Phones signed in with the OLD password are signed out now; phones
	// enrolled by QR keep their own keys ("Sign out phones" ends those).
	s.finishSessionKicks(kicks, "Your password was changed — sign in again")
	return err
}

func (s *Server) updatePasswordLocked(agencyID, username, password string) ([]expiredGuestKick, error) {

	key := userKey(agencyID, username)
	user, userExists := s.users[key]
	admin, adminExists := s.webAdmins[key]

	// An account can exist in either store, or both: a PTT user has a
	// users.json entry, a dashboard login has a web_admins.json entry, and an
	// agency_admin normally has both. Updating only one leaves the other stale,
	// which shows up as a password that works in the app but not the dashboard
	// (or vice versa), and appears to "revert" after a restart because the
	// stale file is what gets reloaded.
	if !userExists && !adminExists {
		return nil, fmt.Errorf("user %s not found", username)
	}

	var kicks []expiredGuestKick
	if userExists {
		// PTT auth uses SHA-256 (challenge-response HMACs over this hash).
		user.PasswordHash = hashPassword(password)
		kicks = s.takeSessionsLocked(key, func(c string) bool { return c == credPassword })
		if err := s.saveUsers(); err != nil {
			return kicks, err
		}
	}

	if adminExists {
		// The dashboard prefers bcrypt; checkPassword still accepts SHA-256.
		if bcryptHash, err := hashPasswordSecure(password); err == nil {
			admin.PasswordHash = bcryptHash
		} else {
			admin.PasswordHash = hashPassword(password) // fallback
		}
		if err := s.saveWebAdmins(); err != nil {
			return kicks, err
		}
	}

	log.Printf("Updated password for %s [%s] (ptt=%v dashboard=%v)",
		username, agencyID, userExists, adminExists)
	return kicks, nil
}

// ErrNeedsDashboardPassword: promoting an account that has no dashboard
// login needs a dashboard password typed by the admin.
var ErrNeedsDashboardPassword = errors.New("needs_dashboard_password")

func (s *Server) UpdateUserRole(agencyID, username, newRole string, dashboardPassword ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := userKey(agencyID, username)
	user, exists := s.users[key]
	if !exists {
		return fmt.Errorf("user %s not found", username)
	}

	oldRole := user.Role
	user.Role = newRole

	// Handle web admin entry changes.
	//
	// NEVER rebuild an existing dashboard entry from the PTT hash. The two
	// stores hold different things once an account has been used: the
	// dashboard keeps the password the admin actually types (bcrypt), while
	// users.json holds whatever the app authenticates with — after a QR
	// enrollment that is the enrollment token, not a password anyone knows.
	// Re-saving the role (even to the same role) used to copy that token hash
	// over the dashboard password, so the admin could no longer log in until
	// a super admin reset it. Keep the existing hash; only the role changes.
	if newRole == "admin" || newRole == "super_admin" {
		webRole := "agency_admin"
		webAgency := user.AgencyID
		if newRole == "super_admin" {
			webRole = "super_admin"
			webAgency = ""
		}
		if existing, ok := s.webAdmins[key]; ok {
			existing.Role = webRole
			existing.AgencyID = webAgency
		} else {
			// First dashboard login for this account. Never copy the PTT
			// hash: it may be an old QR token, and the dashboard password
			// must be one the admin chose (bcrypt).
			pw := ""
			if len(dashboardPassword) > 0 {
				pw = dashboardPassword[0]
			}
			if pw == "" {
				user.Role = oldRole
				return ErrNeedsDashboardPassword
			}
			if err := validateNewPassword(pw); err != nil {
				user.Role = oldRole
				return err
			}
			bh, err := hashPasswordSecure(pw)
			if err != nil {
				user.Role = oldRole
				return err
			}
			s.webAdmins[key] = &WebAdmin{
				Username:           username,
				PasswordHash:       bh,
				Role:               webRole,
				AgencyID:           webAgency,
				MustChangePassword: false,
			}
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
	kicks, err := s.removeUserLocked(agencyID, username)
	s.mu.Unlock()
	if len(kicks) > 0 {
		log.Printf("Removed user %s [%s]: ended %d live session(s)", username, agencyID, len(kicks))
		s.finishSessionKicks(kicks, "Your account was removed")
	}
	return err
}

// removeUserLocked deletes the account and takes its live sessions off the
// server. Caller holds s.mu; the kicks are finished after unlocking.
func (s *Server) removeUserLocked(agencyID, username string) ([]expiredGuestKick, error) {
	key := userKey(agencyID, username)
	user, exists := s.users[key]

	// Handle dispatch users (web-only — no PTT account)
	if !exists {
		admin, adminExists := s.webAdmins[key]
		if adminExists && admin.Role == "dispatch" {
			delete(s.webAdmins, key)
			if err := s.saveWebAdmins(); err != nil {
				return nil, fmt.Errorf("failed to remove dispatch user: %w", err)
			}
			log.Printf("Removed dispatch user: %s [%s]", username, agencyID)
			s.activityLog.Add("dispatch_user_deleted", username, fmt.Sprintf("Removed from agency %s", agencyID), agencyID)
			return nil, nil
		}
		return nil, fmt.Errorf("user %s not found", username)
	}

	// If user had admin/super_admin role, also remove web admin entry
	if user.Role == "admin" || user.Role == "super_admin" {
		delete(s.webAdmins, key)
		if err := s.saveWebAdmins(); err != nil {
			log.Printf("Warning: failed to remove web admin for %s: %v", username, err)
		}
	}

	delete(s.users, key)

	kicks := s.takeAccountSessionsLocked(key)

	if err := s.saveUsers(); err != nil {
		return kicks, err
	}

	log.Printf("Removed user account: %s [%s]", username, agencyID)
	s.activityLog.Add("user_deleted", username, fmt.Sprintf("Removed from agency %s", agencyID), agencyID)
	return kicks, nil
}

func (s *Server) GetUsers(agencyFilter, callerRole, callerAgency, callerUsername string) []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	users := make([]map[string]interface{}, 0, len(s.users)+4)

	// Accounts signed in right now: by the account each connection logged in
	// with, plus callsign matches for connections without that record.
	online := make(map[string]bool, len(s.clients))
	for key, c := range s.clients {
		online[key] = true
		if c.Addr != nil {
			if k := s.authAccounts[c.Addr.String()]; k != "" {
				online[k] = true
			}
		}
	}
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

		pri := user.Priority
		if pri == 0 {
			pri = PriorityResponder
		}
		users = append(users, map[string]interface{}{
			"username":   user.Username,
			"name":       user.Name,
			"agency_id":  user.AgencyID,
			"role":       role,
			"created_at": user.CreatedAt,
			// MCX Step 2: derived MCX identity (additive; username stays the
			// display name and routing key).
			"mcx_id": userMCXID(user.Username, user.AgencyID, s.config.McpttDomain),
			// MCX Step 3: provisioned floor priority.
			"priority":   pri,
			"online":     online[userKey(user.AgencyID, user.Username)],
			"last_login": user.LastLogin,
			// Phones enrolled by QR (account_security.go), no secrets.
			"devices": deviceSummary(user),
		})
		if user.GuestExpiresAt != nil {
			u := users[len(users)-1]
			u["invited_by"] = user.InvitedBy
			u["guest_expires_at"] = user.GuestExpiresAt.Format(time.RFC3339)
		}
	}

	// Also include dispatch users (web-admin only, no PTT account)
	for _, admin := range s.webAdmins {
		if admin.Role != "dispatch" {
			continue
		}
		if agencyFilter != "" && admin.AgencyID != agencyFilter {
			continue
		}
		if callerRole == "agency_admin" && admin.AgencyID != callerAgency {
			continue
		}
		users = append(users, map[string]interface{}{
			"username":   admin.Username,
			"name":       admin.Username,
			"agency_id":  admin.AgencyID,
			"role":       "dispatch",
			"created_at": "",
			"mcx_id":     userMCXID(admin.Username, admin.AgencyID, s.config.McpttDomain),
		})
	}

	return users
}

// ---- Web Admin / Dashboard Auth ----

type WebAdmin struct {
	Username           string `json:"username"`
	PasswordHash       string `json:"password_hash"`
	Role               string `json:"role"`                // "super_admin" or "agency_admin"
	AgencyID           string `json:"agency_id,omitempty"` // empty for super_admin
	MustChangePassword bool   `json:"must_change_password"`
	// SSO accounts (oidc.go) sign in through the identity provider only;
	// Subject is the provider's stable id for the person.
	SSO     bool   `json:"sso,omitempty"`
	Subject string `json:"sso_subject,omitempty"`
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
	// A platform (infra-TAK) generates the initial password itself, keeps it
	// in its own protected settings and shows it to its operator, so it is
	// never written to the log here.
	if pw := s.config.AdminPassword; pw != "" {
		bcryptHash, err := hashPasswordSecure(pw)
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
		log.Printf("Initial super admin 'admin' created with the password provided by the installer (change it at first login)")
		return
	}
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

	return writeJSONFileAtomic(s.config.WebAdminsFile, admins)
}

func (s *Server) generateSessionToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) validateWebSession(token string) bool {
	return s.sessionFor(token) != nil
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
		if m := agency.floorMode(); m != floorModeOff {
			s.floorOn.Store(agency.ID, m)
		}
	}
	log.Printf("Loaded %d agencies", len(s.agencies))
}

func (s *Server) saveAgencies() error {
	agencies := make([]*Agency, 0, len(s.agencies))
	for _, agency := range s.agencies {
		agencies = append(agencies, agency)
	}

	return writeJSONFileAtomic(s.config.AgenciesFile, agencies)
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
		MaxChannels: MaxAgencyTalkgroups,
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

// AddDispatchUser creates a web-only dispatch user (WebAdmin entry, no PTT account).
func (s *Server) AddDispatchUser(username, name, password, agencyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if agencyID == "" {
		agencyID = "default"
	}
	if _, ok := s.agencies[agencyID]; !ok {
		return fmt.Errorf("agency %s not found", agencyID)
	}

	key := userKey(agencyID, username)
	if _, exists := s.webAdmins[key]; exists {
		return fmt.Errorf("dispatch user %s already exists in agency %s", username, agencyID)
	}

	pwHash, err := hashPasswordSecure(password)
	if err != nil {
		pwHash = hashPassword(password)
	}

	s.webAdmins[key] = &WebAdmin{
		Username:           username,
		PasswordHash:       pwHash,
		Role:               "dispatch",
		AgencyID:           agencyID,
		MustChangePassword: false,
	}

	if err := s.saveWebAdmins(); err != nil {
		delete(s.webAdmins, key)
		return err
	}

	log.Printf("Created dispatch user: %s (agency: %s)", username, agencyID)
	s.activityLog.Add("dispatch_user_created", username, fmt.Sprintf("Dispatch user created in agency %s", agencyID), agencyID)
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

	return writeJSONFileAtomic(s.config.MutualAidFile, links)
}

// getMutualAidPartners returns all partner agency IDs if an active mutual aid link
// exists for the given agency and channel. Returns nil if no link.
// getMutualAidPartners returns the other agencies that share a channel with
// this one, so audio/text/heartbeat fan out across the patch.
//
// LMR bridge channels (the 100-119 block) answer here too: a bridge patches
// several agencies onto one radio channel, which is the same relationship a
// mutual-aid link describes. Routing both through this one function means the
// three relay sites that call it need no LMR-specific branch.
// Caller holds s.mu (read is enough): it walks the bridge and link maps.
func (s *Server) getMutualAidPartners(agencyID string, channel int) []string {
	if p := s.getLMRPartners(agencyID, channel); len(p) > 0 {
		return p
	}
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
// Caller holds s.mu (read is enough).
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
		if s.isGatewayLogin(client) {
			continue // a gateway hears only its own LMR bridge
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

// pickMutualAidChannel picks the highest free channel in the shared 10-19 block,
// or 0 when the block is full. Caller must hold s.mu.
func (s *Server) pickMutualAidChannel() int {
	// Two things this must respect, and originally didn't:
	//
	//   - The 10-19 block is SHARED with ad-hoc groups and Zello mappings, so
	//     the free-channel test has to be usedHighChannels(), not just the
	//     mutual-aid links. Looking at links alone handed out numbers already
	//     serving a Zello bridge or an ad-hoc group.
	//   - Channels 0-9 are ordinary agency talkgroups. Scanning down to 0 could
	//     put a patch on someone's Channel 1.
	used := s.usedHighChannels()
	for ch := SharedChannelMax; ch >= SharedChannelMin; ch-- {
		if !used[ch] {
			return ch
		}
	}
	return 0 // block full — caller must treat 0 as "no capacity"
}

// --- Guest Events ---

func (s *Server) loadGuestEvents() {
	file, err := os.Open("guest_events.json")
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Printf("Error loading guest events: %v", err)
		return
	}
	defer file.Close()

	var events []*GuestEvent
	if err := json.NewDecoder(file).Decode(&events); err != nil {
		log.Printf("Error parsing guest events: %v", err)
		return
	}

	now := time.Now()
	for _, ev := range events {
		// Skip already-expired events on load
		if ev.ExpiresAt != nil && now.After(*ev.ExpiresAt) {
			log.Printf("Guest event %s (%s) expired, skipping on load", ev.ID, ev.Name)
			continue
		}
		s.guestEvents[ev.ID] = ev
		// Re-register multi-use enrollment token in memory
		s.enrollmentTokens[ev.Token] = &EnrollmentToken{
			Token:     ev.Token,
			Username:  ev.Username,
			AgencyID:  ev.AgencyID,
			ExpiresAt: guestTokenExpiry(ev.ExpiresAt),
			IsGuest:   true,
		}
	}
	log.Printf("Loaded %d guest events", len(s.guestEvents))
}

// guestTokenExpiry returns the expiry time for the in-memory token.
// If the event has no expiry, the token gets a far-future expiry so it's never cleaned up.
func guestTokenExpiry(eventExpiry *time.Time) time.Time {
	if eventExpiry != nil {
		return *eventExpiry
	}
	return time.Now().Add(365 * 24 * time.Hour) // 1 year; refreshed on every load
}

func (s *Server) saveGuestEvents() error {
	events := make([]*GuestEvent, 0, len(s.guestEvents))
	for _, ev := range s.guestEvents {
		events = append(events, ev)
	}

	return writeJSONFileAtomic("guest_events.json", events)
}

// CreateGuestEvent creates a guest event with a shared user account and multi-use token.
// Must be called without s.mu held (it acquires it internally).
// Each event gets its own temporary agency named after the event; the agency
// (and all its users) is deleted when the event ends. The ownerAgencyID records
// which real agency created the event, for agency-scoped visibility; "" means it
// was created by a super_admin and is visible to super_admins only.
func (s *Server) CreateGuestEvent(name, ownerAgencyID string, channel int, expiresAt *time.Time, createdBy string, includeAgencies []string) (*GuestEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The channel is the server's choice whenever other agencies are patched
	// in: it must be a FREE channel in the shared block. Taking the number
	// from the request let an event land on a partner agency's own
	// talkgroup (or an LMR/Zello channel), putting anonymous QR guests on it.
	patched := false
	for _, a := range includeAgencies {
		patched = patched || a != ""
	}
	if patched {
		if channel < SharedChannelMin || channel > SharedChannelMax || s.usedHighChannels()[channel] {
			channel = s.pickMutualAidChannel()
			if channel == 0 {
				return nil, fmt.Errorf("no free shared channel for the event")
			}
		}
	} else if channel < 0 || channel > SharedChannelMax {
		return nil, fmt.Errorf("invalid event channel")
	}

	// Generate unique event ID and guest username
	idBytes := make([]byte, 8)
	rand.Read(idBytes)
	eventID := hex.EncodeToString(idBytes)

	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)

	// Create a temporary agency dedicated to this event.
	eventAgencyID := "event-" + eventID[:8]
	s.agencies[eventAgencyID] = &Agency{
		ID:          eventAgencyID,
		Name:        name,
		CreatedAt:   time.Now().Format(time.RFC3339),
		MaxUsers:    1000,
		MaxChannels: MaxAgencyTalkgroups,
	}
	s.ensureAgencyChannels(eventAgencyID)
	if err := s.saveAgencies(); err != nil {
		delete(s.agencies, eventAgencyID)
		return nil, fmt.Errorf("failed to save agency: %w", err)
	}

	// Guest username: "guest-<eventID[:8]>"
	username := "guest-" + eventID[:8]

	// Create the PTT user account with token as the password
	pwHash := hashPassword(token)
	key := userKey(eventAgencyID, username)
	s.users[key] = &UserAccount{
		Username:     username,
		Name:         name + " (Guest)",
		PasswordHash: pwHash,
		AgencyID:     eventAgencyID,
		Role:         "user",
		CreatedAt:    time.Now().Format(time.RFC3339),
	}
	if err := s.saveUsers(); err != nil {
		delete(s.users, key)
		delete(s.agencies, eventAgencyID)
		s.saveAgencies()
		return nil, fmt.Errorf("failed to save user: %w", err)
	}

	// Register multi-use enrollment token
	s.enrollmentTokens[token] = &EnrollmentToken{
		Token:     token,
		Username:  username,
		AgencyID:  eventAgencyID,
		ExpiresAt: guestTokenExpiry(expiresAt),
		IsGuest:   true,
	}

	ev := &GuestEvent{
		ID:            eventID,
		Name:          name,
		AgencyID:      eventAgencyID,
		Channel:       channel,
		Username:      username,
		Token:         token,
		ExpiresAt:     expiresAt,
		CreatedAt:     time.Now().Format(time.RFC3339),
		CreatedBy:     createdBy,
		OwnsAgency:    true,
		OwnerAgencyID: ownerAgencyID,
	}

	// Bridge any selected existing agencies into the event's channel via a
	// mutual-aid link, so every member of those agencies is patched into the
	// event automatically (no per-user enrollment needed). The event's own
	// temporary agency is included so guests (enrolled via QR) and the selected
	// agencies all share the channel.
	if len(includeAgencies) > 0 {
		linkAgencies := []string{eventAgencyID}
		seen := map[string]bool{eventAgencyID: true}
		for _, aid := range includeAgencies {
			if aid == "" || seen[aid] {
				continue
			}
			if _, ok := s.agencies[aid]; !ok {
				continue
			} // skip unknown agencies
			linkAgencies = append(linkAgencies, aid)
			seen[aid] = true
		}
		if len(linkAgencies) >= 2 {
			linkIDBytes := make([]byte, 8)
			rand.Read(linkIDBytes)
			linkID := hex.EncodeToString(linkIDBytes)
			s.mutualAidLinks[linkID] = &MutualAidLink{
				ID:          linkID,
				Agencies:    linkAgencies,
				Channel:     channel,
				ChannelName: name,
				CreatedAt:   time.Now().Format(time.RFC3339),
				CreatedBy:   createdBy,
				Active:      true,
			}
			ev.MutualAidLinkID = linkID
			ev.IncludedAgencies = linkAgencies[1:] // exclude the event's own temp agency
			if err := s.saveMutualAid(); err != nil {
				log.Printf("Warning: failed to save guest-event mutual aid link: %v", err)
			}
			// Notify connected clients of the bridged channel (background).
			go s.notifyMutualAidChannel(linkAgencies, channel, name, true)
		}
	}

	s.guestEvents[eventID] = ev

	if err := s.saveGuestEvents(); err != nil {
		log.Printf("Warning: failed to save guest events: %v", err)
	}

	log.Printf("Created guest event: %s (%s) agency=%s channel=%d by %s", eventID, name, eventAgencyID, channel, createdBy)
	s.activityLog.Add("guest_event_created", createdBy, fmt.Sprintf("Guest event '%s' created (user: %s, agency: %s)", name, username, eventAgencyID), eventAgencyID)
	return ev, nil
}

// DeleteGuestEvent removes a guest event, its user account, and kicks connected clients.
// Must be called without s.mu held.
func (s *Server) DeleteGuestEvent(eventID string) error {
	s.mu.Lock()

	ev, ok := s.guestEvents[eventID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("guest event not found")
	}

	// Remove enrollment token
	delete(s.enrollmentTokens, ev.Token)

	// Remove user account
	key := userKey(ev.AgencyID, ev.Username)
	delete(s.users, key)

	// End every session of the event: the shared account's (whatever
	// callsign each phone joined as) and anyone else in the event's agency.
	// These leave the channels too — only dropping them from s.clients left
	// them in the channel member lists, still hearing and transmitting.
	kicks := s.takeAccountSessionsLocked(key)
	for _, c := range s.clients {
		if c.AgencyID == ev.AgencyID {
			s.removeClient(c)
			kicks = append(kicks, expiredGuestKick{client: c, addr: c.Addr})
		}
	}
	for a, ag := range s.authenticatedAddrs {
		if ag == ev.AgencyID {
			s.forgetAddrAuthLocked(a)
		}
	}

	// Cascade-delete the temporary agency this event owns: its users,
	// channels, and any enrollment tokens scoped to it.
	if ev.OwnsAgency {
		for uKey, u := range s.users {
			if u.AgencyID == ev.AgencyID {
				delete(s.users, uKey)
			}
		}
		for tok, et := range s.enrollmentTokens {
			if et.AgencyID == ev.AgencyID {
				delete(s.enrollmentTokens, tok)
			}
		}
		for chKey := range s.channels {
			if strings.HasPrefix(chKey, ev.AgencyID+":") {
				delete(s.channels, chKey)
			}
		}
		for adminUser, admin := range s.webAdmins {
			if admin.AgencyID == ev.AgencyID {
				delete(s.webAdmins, adminUser)
			}
		}
		delete(s.agencies, ev.AgencyID)
	}

	// Remove the auto-created mutual-aid link (agency bridge) for this event so
	// the selected agencies are un-patched when the event ends.
	if ev.MutualAidLinkID != "" {
		if link, ok := s.mutualAidLinks[ev.MutualAidLinkID]; ok {
			go s.notifyMutualAidChannel(link.Agencies, link.Channel, link.ChannelName, false)
			delete(s.mutualAidLinks, ev.MutualAidLinkID)
			s.saveMutualAid()
		}
	}

	s.saveUsers()

	delete(s.guestEvents, eventID)
	s.mu.Unlock()

	// Kick connected clients (outside lock)
	s.finishSessionKicks(kicks, "Guest event ended")

	// The savers iterate server maps: hold s.mu (read) while they run.
	s.mu.RLock()
	if ev.OwnsAgency {
		s.saveAgencies()
		s.saveWebAdmins()
	}
	err := s.saveGuestEvents()
	s.mu.RUnlock()
	if err != nil {
		log.Printf("Warning: failed to save guest events: %v", err)
	}

	log.Printf("Deleted guest event: %s (%s), removed agency %s, kicked %d clients", eventID, ev.Name, ev.AgencyID, len(kicks))
	s.activityLog.Add("guest_event_deleted", "", fmt.Sprintf("Guest event '%s' deleted (agency %s removed), kicked %d clients", ev.Name, ev.AgencyID, len(kicks)), ev.AgencyID)
	return nil
}

// cleanupExpiredGuestEvents removes guest events that have passed their expiry time.
// Called from cleanupExpiredTokens.
func (s *Server) cleanupExpiredGuestEvents() {
	now := time.Now()
	var expired []string
	for id, ev := range s.guestEvents {
		if ev.ExpiresAt != nil && now.After(*ev.ExpiresAt) {
			expired = append(expired, id)
		}
	}
	s.mu.Unlock()
	for _, id := range expired {
		log.Printf("Guest event %s expired, auto-deleting", id)
		s.DeleteGuestEvent(id)
	}
	s.mu.Lock()
}

// channelAnnouncement is one named channel a joining client is told about.
type channelAnnouncement struct {
	channel int
	name    string
	kind    string // "mutual aid", "Zello bridge", "LMR bridge"
}

// joinAnnouncements lists the bridged / shared channels to announce to a client
// that just joined. Caller holds s.mu.
//
// A GATEWAY login (the username of an LMR bridge it owns) hears ONLY its own
// bridge. A gateway adopts whatever channel it is told about, and every JOIN
// re-triggers this announcement, so anything else — a second LMR bridge
// (join storm, 2026-09-22), Zello bridge channels or a mutual-aid patch
// (2026-10-22: a Pi bridge flipped onto two live police Zello channels) — would
// pull the radio onto a channel it was never meant to bridge. Phones still
// hear everything.
func (s *Server) joinAnnouncements(agencyID, username string, joinedChannel int) []channelAnnouncement {
	var out []channelAnnouncement

	var ownBridge *LMRBridge
	for _, b := range s.lmrBridges {
		if b.Username == username && b.ownerAgency() == agencyID {
			ownBridge = b
			break
		}
	}
	if ownBridge != nil {
		if ownBridge.Enabled && ownBridge.servesAgency(agencyID) {
			out = append(out, channelAnnouncement{ownBridge.Channel, ownBridge.displayName(), "LMR bridge"})
		}
		return out
	}

	// Mutual aid: every active link this agency is part of, unless the client
	// joined the link's channel itself.
	isMutualAidJoin := false
	for _, link := range s.mutualAidLinks {
		if link.Active && link.Channel == joinedChannel {
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
					out = append(out, channelAnnouncement{link.Channel, link.ChannelName, "mutual aid"})
					break
				}
			}
		}
	}

	// All mapped channels of any active Zello bridge for this agency, so
	// bridged channels appear as selectable named channels.
	for _, b := range s.zelloBridges {
		if b.Enabled && b.AgencyID == agencyID && b.announced {
			for _, m := range b.effectiveMappings() {
				if m.ZelloChannel == "" {
					continue
				}
				out = append(out, channelAnnouncement{m.MeshChannel, m.ZelloChannel, "Zello bridge"})
			}
		}
	}

	// Every enabled LMR bridge serving this agency.
	for _, b := range s.lmrBridges {
		if b.Enabled && b.servesAgency(agencyID) {
			out = append(out, channelAnnouncement{b.Channel, b.displayName(), "LMR bridge"})
		}
	}
	return out
}

// isGatewayLogin reports whether a connected client is the gateway user of an
// LMR bridge it owns. Such a client adopts any channel it is announced, so the
// runtime announcements (a Zello bridge coming up, a mutual-aid patch being
// created, another LMR bridge) must skip it — only its own bridge may reach it.
// Caller holds s.mu (read).
func (s *Server) isGatewayLogin(c *Client) bool {
	return s.ownLMRBridge(c) != nil
}

func (s *Server) ownLMRBridge(c *Client) *LMRBridge {
	for _, b := range s.lmrBridges {
		if b.Username == c.Username && b.ownerAgency() == c.AgencyID {
			// The gateway is the bridge's OWN ACCOUNT, not anyone using its
			// callsign. (JOIN also reserves the name; this is the second lock.)
			if c.Account != "" && c.Account != userKey(b.ownerAgency(), b.Username) {
				return nil
			}
			return b
		}
	}
	return nil
}

// autoFloorForGateway asks for the floor on behalf of an LMR bridge gateway
// whose radio just started sending audio. A radio cannot press PTT the way a
// phone does: the gateway streams whatever the radio receives and never sends
// a FLOOR_REQUEST, so with floor control on, every radio transmission was
// dropped as "not the holder" (2026-10-22, scny). The request goes through the
// normal arbiter — an idle channel grants it, a busy one applies the agency's
// policy (busy: dropped, queue: recorded and replayed) — and the dead-man
// timer releases it when the radio goes quiet. Throttled to one request per
// second per gateway so a refused radio does not hammer the arbiter.
// Returns true when the gateway now holds the floor.
func (s *Server) autoFloorForGateway(client *Client, fkey string) bool {
	s.mu.RLock()
	own := s.ownLMRBridge(client)
	acct := s.accountFor(client)
	s.mu.RUnlock()
	if own == nil || s.floor == nil {
		return false
	}
	s.autoFloorMu.Lock()
	if s.autoFloorLast == nil {
		s.autoFloorLast = map[string]time.Time{}
	}
	last := s.autoFloorLast[client.Username]
	if time.Since(last) < time.Second {
		s.autoFloorMu.Unlock()
		return false
	}
	s.autoFloorLast[client.Username] = time.Now()
	s.autoFloorMu.Unlock()

	pri := acct.effectivePriority()
	s.floor.Request(fkey, &FloorRequest{Channel: client.Channel, User: client.Username, Priority: pri}, pri)
	granted := s.floor.HasFloor(fkey, client.Username)
	if granted {
		log.Printf("Floor %s: auto-granted to gateway %s (radio audio)", fkey, client.Username)
	}
	return granted
}

// gatewayPaused reports whether a client is the gateway of an LMR bridge that
// is currently disabled on the dashboard. Nothing is relayed to such a client,
// so the radio behind it cannot be keyed — the remote kill switch for a
// bridge that sits unattended on a public-safety channel. Lock-free; the
// set is maintained by refreshPausedGateways.
func (s *Server) gatewayPaused(c *Client) bool {
	if c == nil {
		return false
	}
	_, paused := s.pausedGateways.Load(userKey(c.AgencyID, c.Username))
	return paused
}

// refreshPausedGateways rebuilds the paused set from the bridge list. Caller
// holds s.mu (read or write).
func (s *Server) refreshPausedGateways() {
	s.pausedGateways.Range(func(k, _ interface{}) bool { s.pausedGateways.Delete(k); return true })
	for _, b := range s.lmrBridges {
		if !b.Enabled && b.Username != "" {
			s.pausedGateways.Store(userKey(b.ownerAgency(), b.Username), true)
		}
	}
}
