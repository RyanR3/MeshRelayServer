package main

import (
	"bytes"
	"encoding/binary"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MCX Step 3 — server-enforced floor control with priority preemption
// (mirrors 3GPP TS 24.380). Opt-in per agency; when disabled a channel is open
// PTT exactly as before. Full-duplex bypasses floor control (it's a PTT-only
// concept).
//
// Wire format for floor packets (all big-endian, length-prefixed strings as
// u16 len | bytes), body after the 1-byte packet type:
//   FloorRequest   (0x20): i32 channel | str user | u8 priority
//   FloorGranted   (0x21): i32 channel | str user | u16 durationSec | u8 queuePos
//   FloorDenied    (0x22): i32 channel | u8 reason | u8 queuePos | str currentHolder
//   FloorPreempted (0x23): i32 channel | str preemptedBy | u8 priority
//   FloorIdle      (0x24): i32 channel
//   FloorTaken     (0x25): i32 channel | str holder | u8 priority
//   EmergencyAlert (0x26): i32 channel | str user | u8 active(1=set,0=cancel)

// ---- wire helpers ----

func putStr(buf *bytes.Buffer, s string) {
	b := []byte(s)
	binary.Write(buf, binary.BigEndian, uint16(len(b)))
	buf.Write(b)
}

func getStr(buf *bytes.Reader) (string, error) {
	var n uint16
	if err := binary.Read(buf, binary.BigEndian, &n); err != nil {
		return "", err
	}
	b := make([]byte, n)
	if _, err := buf.Read(b); err != nil {
		return "", err
	}
	return string(b), nil
}

// FloorRequest parsed from a client.
type FloorRequest struct {
	Channel  int
	User     string
	Priority uint8
}

func ParseFloorRequest(data []byte) (*FloorRequest, error) {
	r := bytes.NewReader(data)
	var ch int32
	if err := binary.Read(r, binary.BigEndian, &ch); err != nil {
		return nil, err
	}
	user, err := getStr(r)
	if err != nil {
		return nil, err
	}
	pri, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	return &FloorRequest{Channel: int(ch), User: user, Priority: pri}, nil
}

// EmergencyAlert parsed from a client.
type EmergencyAlert struct {
	Channel int
	User    string
	Active  bool
}

func ParseEmergencyAlert(data []byte) (*EmergencyAlert, error) {
	r := bytes.NewReader(data)
	var ch int32
	if err := binary.Read(r, binary.BigEndian, &ch); err != nil {
		return nil, err
	}
	user, err := getStr(r)
	if err != nil {
		return nil, err
	}
	active, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	return &EmergencyAlert{Channel: int(ch), User: user, Active: active == 1}, nil
}

// ---- server -> client builders ----

func buildFloorGranted(channel int, user string, durationSec uint16, queuePos uint8) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeFloorGranted)
	binary.Write(buf, binary.BigEndian, int32(channel))
	putStr(buf, user)
	binary.Write(buf, binary.BigEndian, durationSec)
	buf.WriteByte(queuePos)
	return buf.Bytes()
}

func buildFloorDenied(channel int, reason, queuePos uint8, holder string) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeFloorDenied)
	binary.Write(buf, binary.BigEndian, int32(channel))
	buf.WriteByte(reason)
	buf.WriteByte(queuePos)
	putStr(buf, holder)
	return buf.Bytes()
}

func buildFloorPreempted(channel int, preemptedBy string, priority uint8) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeFloorPreempted)
	binary.Write(buf, binary.BigEndian, int32(channel))
	putStr(buf, preemptedBy)
	buf.WriteByte(priority)
	return buf.Bytes()
}

func buildFloorIdle(channel int) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeFloorIdle)
	binary.Write(buf, binary.BigEndian, int32(channel))
	return buf.Bytes()
}

func buildFloorTaken(channel int, holder string, priority uint8) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeFloorTaken)
	binary.Write(buf, binary.BigEndian, int32(channel))
	putStr(buf, holder)
	buf.WriteByte(priority)
	return buf.Bytes()
}

func buildEmergencyAlert(channel int, user string, active bool) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeEmergencyAlert)
	binary.Write(buf, binary.BigEndian, int32(channel))
	putStr(buf, user)
	if active {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

// ---- floor state machine ----

const (
	floorDeadManTimeout = 3 * time.Second   // release floor after this much audio silence
	floorMaxQueue       = 5                 // per-channel queue depth
	emergencyAutoExpire = 300 * time.Second // emergency auto-cancel
	floorGraceMs        = 200               // preemption grace period
)

type floorHolder struct {
	user      string
	priority  uint8
	lastAudio time.Time
	grantedAt time.Time
}

type queued struct {
	user     string
	priority uint8
}

// floorChannel is the floor state for one agency-scoped channel key.
type floorChannel struct {
	holder    *floorHolder
	queue     []queued
	emergency bool
	emergBy   string
	emergAt   time.Time
}

// FloorController arbitrates the talk floor per channel key ("agencyID:channel").
type FloorController struct {
	mu       sync.Mutex
	channels map[string]*floorChannel
	server   *Server
}

func NewFloorController(s *Server) *FloorController {
	return &FloorController{channels: make(map[string]*floorChannel), server: s}
}

func (fc *FloorController) ch(key string) *floorChannel {
	c := fc.channels[key]
	if c == nil {
		c = &floorChannel{}
		fc.channels[key] = c
	}
	return c
}

// Request handles a FLOOR_REQUEST. Returns nothing; sends grant/denied/preempt
// packets to the relevant clients. `sendTo` sends bytes to a user's addr.
func (fc *FloorController) Request(key string, req *FloorRequest, effectivePriority uint8) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	c := fc.ch(key)

	// Monitor priority cannot request the floor.
	if effectivePriority >= PriorityMonitor {
		fc.server.sendFloorToUser(req.User, buildFloorDenied(req.Channel, FloorDenyNoPermission, 0, holderName(c)))
		return
	}

	// Idle -> grant immediately.
	if c.holder == nil {
		fc.grant(key, req.Channel, req.User, effectivePriority)
		return
	}

	// Already the holder? Re-grant (idempotent).
	if c.holder.user == req.User {
		fc.server.sendFloorToUser(req.User, buildFloorGranted(req.Channel, req.User, 0, 0))
		return
	}

	// Higher priority (lower number) preempts.
	if effectivePriority < c.holder.priority {
		old := c.holder.user
		fc.server.sendFloorToUser(old, buildFloorPreempted(req.Channel, req.User, effectivePriority))
		fc.grant(key, req.Channel, req.User, effectivePriority)
		log.Printf("Floor %s: %s (pri %d) PREEMPTED %s (pri %d)", key, req.User, effectivePriority, old, c.holderPriorityBefore(old))
		return
	}

	// Equal/lower priority -> deny + queue.
	pos := fc.enqueue(c, req.User, effectivePriority)
	if pos == 0 {
		fc.server.sendFloorToUser(req.User, buildFloorDenied(req.Channel, FloorDenyChannelFull, 0, c.holder.user))
		return
	}
	fc.server.sendFloorToUser(req.User, buildFloorDenied(req.Channel, FloorDenyBusy, uint8(pos), c.holder.user))
}

func (c *floorChannel) holderPriorityBefore(user string) uint8 {
	if c.holder != nil && c.holder.user == user {
		return c.holder.priority
	}
	return 0
}

// enqueue adds a user to the FIFO queue; returns 1-based position, or 0 if full
// or already queued (already-queued returns its existing position).
func (fc *FloorController) enqueue(c *floorChannel, user string, pri uint8) int {
	for i, q := range c.queue {
		if q.user == user {
			return i + 1
		}
	}
	if len(c.queue) >= floorMaxQueue {
		return 0
	}
	c.queue = append(c.queue, queued{user: user, priority: pri})
	return len(c.queue)
}

// grant sets a new floor holder and notifies them. Caller holds fc.mu.
func (fc *FloorController) grant(key string, channel int, user string, priority uint8) {
	c := fc.ch(key)
	c.holder = &floorHolder{user: user, priority: priority, lastAudio: time.Now(), grantedAt: time.Now()}
	// remove from queue if present
	fc.dequeueUser(c, user)
	fc.server.sendFloorToUser(user, buildFloorGranted(channel, user, 0, 0))
}

func (fc *FloorController) dequeueUser(c *floorChannel, user string) {
	out := c.queue[:0]
	for _, q := range c.queue {
		if q.user != user {
			out = append(out, q)
		}
	}
	c.queue = out
}

// Release frees the floor held by `user` (PTT release / disconnect). Grants the
// next queued user, or broadcasts FLOOR_IDLE. Caller must NOT hold fc.mu.
// ReleaseAgency drops every held floor and queue for an agency's channels —
// used when floor control is switched off for the agency.
func (fc *FloorController) ReleaseAgency(agencyID string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	prefix := agencyID + ":"
	for key := range fc.channels {
		if strings.HasPrefix(key, prefix) {
			delete(fc.channels, key)
		}
	}
}

func (fc *FloorController) Release(key string, channel int, user string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	c := fc.ch(key)
	if c.holder == nil || c.holder.user != user {
		// Not the holder — maybe queued; drop from queue.
		fc.dequeueUser(c, user)
		return
	}
	fc.releaseAndAdvance(key, channel, c)
}

// releaseAndAdvance clears the holder and grants the next in queue (or idle).
// Caller holds fc.mu.
func (fc *FloorController) releaseAndAdvance(key string, channel int, c *floorChannel) {
	c.holder = nil
	if len(c.queue) > 0 {
		next := c.queue[0]
		c.queue = c.queue[1:]
		fc.grant(key, channel, next.user, next.priority)
		return
	}
	fc.server.broadcastFloorToChannel(key, buildFloorIdle(channel))
}

// NoteAudio marks that the holder is still transmitting (resets dead-man timer).
func (fc *FloorController) NoteAudio(key, user string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	c := fc.channels[key]
	if c != nil && c.holder != nil && c.holder.user == user {
		c.holder.lastAudio = time.Now()
	}
}

// HasFloor reports whether `user` currently holds the floor on `key`. When floor
// control is enabled, only the holder's audio is relayed.
func (fc *FloorController) HasFloor(key, user string) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	c := fc.channels[key]
	return c != nil && c.holder != nil && c.holder.user == user
}

// Emergency sets or cancels the emergency state on a channel. Setting emergency
// preempts the floor to the alerting user at PriorityEmergency and broadcasts
// the alert. Cancel clears it (any supervisor / the originator).
func (fc *FloorController) Emergency(key string, channel int, user string, active bool, isSupervisor bool) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	c := fc.ch(key)
	if active {
		c.emergency = true
		c.emergBy = user
		c.emergAt = time.Now()
		// Preempt to the emergency user.
		if c.holder != nil && c.holder.user != user {
			fc.server.sendFloorToUser(c.holder.user, buildFloorPreempted(channel, user, PriorityEmergency))
		}
		fc.grant(key, channel, user, PriorityEmergency)
		fc.server.broadcastFloorToChannel(key, buildEmergencyAlert(channel, user, true))
		log.Printf("Floor %s: EMERGENCY activated by %s", key, user)
	} else {
		// Only the originator or a supervisor can cancel.
		if c.emergBy != user && !isSupervisor {
			return
		}
		c.emergency = false
		c.emergBy = ""
		fc.server.broadcastFloorToChannel(key, buildEmergencyAlert(channel, user, false))
		log.Printf("Floor %s: emergency cancelled by %s", key, user)
	}
}

// FloorStatusFor returns a FLOOR_TAKEN packet describing the current holder for a
// joining client, or nil if idle.
func (fc *FloorController) FloorStatusFor(key string, channel int) []byte {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	c := fc.channels[key]
	if c == nil || c.holder == nil {
		return nil
	}
	return buildFloorTaken(channel, c.holder.user, c.holder.priority)
}

// Tick runs periodically: dead-man timeout release + emergency auto-expire.
func (fc *FloorController) Tick() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	now := time.Now()
	for key, c := range fc.channels {
		if c.holder != nil && now.Sub(c.holder.lastAudio) > floorDeadManTimeout {
			// Dead-man: holder stopped sending audio.
			ch := channelNumFromKey(key)
			log.Printf("Floor %s: dead-man timeout, releasing %s", key, c.holder.user)
			fc.releaseAndAdvance(key, ch, c)
		}
		if c.emergency && now.Sub(c.emergAt) > emergencyAutoExpire {
			c.emergency = false
			ch := channelNumFromKey(key)
			fc.server.broadcastFloorToChannel(key, buildEmergencyAlert(ch, c.emergBy, false))
			c.emergBy = ""
			log.Printf("Floor %s: emergency auto-expired", key)
		}
	}
}

// DropUser removes a disconnected user from any floor/queue on all channels.
func (fc *FloorController) DropUser(user string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for key, c := range fc.channels {
		if c.holder != nil && c.holder.user == user {
			ch := channelNumFromKey(key)
			fc.releaseAndAdvance(key, ch, c)
		} else {
			fc.dequeueUser(c, user)
		}
	}
}

func holderName(c *floorChannel) string {
	if c.holder != nil {
		return c.holder.user
	}
	return ""
}

// channelNumFromKey extracts the channel number from an "agencyID:channel" key.
func channelNumFromKey(key string) int {
	i := strings.LastIndexByte(key, ':')
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(key[i+1:])
	return n
}

// ---- Server helpers for the floor controller ----

// sendFloorToUser sends a floor packet to a specific user's current address.
func (s *Server) sendFloorToUser(username string, data []byte) {
	s.mu.RLock()
	client, _ := s.findClientByUsername(username)
	s.mu.RUnlock()
	if client != nil {
		if s.testFloorSink != nil {
			s.testFloorSink(client.Addr, data)
			return
		}
		s.sendPacket(data, client.Addr)
	}
}

// broadcastFloorToChannel sends a floor packet to every client in a channel key.
func (s *Server) broadcastFloorToChannel(key string, data []byte) {
	s.mu.RLock()
	channel := s.channels[key]
	var addrs []*net.UDPAddr
	if channel != nil {
		channel.mu.RLock()
		for _, c := range channel.Clients {
			addrs = append(addrs, c.Addr)
		}
		channel.mu.RUnlock()
	}
	s.mu.RUnlock()
	for _, a := range addrs {
		if s.testFloorSink != nil {
			s.testFloorSink(a, data)
			continue
		}
		s.sendPacket(data, a)
	}
}

// handleFloorRequest processes a FLOOR_REQUEST (0x20). A request with priority 0
// is a RELEASE (PTT up). The server validates the client's priority against its
// provisioned account (clients can't self-elevate).
func (s *Server) handleFloorRequest(req *FloorRequest, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}
	s.mu.RLock()
	client, _ := s.findClientByUsername(req.User)
	s.mu.RUnlock()
	if client == nil {
		return
	}
	if !s.floorEnabledForAgency(client.AgencyID) {
		// Floor control off for this agency: auto-grant so clients that gate on
		// grant still work (open-PTT semantics preserved).
		if req.Priority != 0 {
			s.sendFloorToUser(req.User, buildFloorGranted(req.Channel, req.User, 0, 0))
		}
		return
	}
	key := channelKey(client.AgencyID, client.Channel)

	if req.Priority == 0 {
		// Release the floor (PTT up).
		s.floor.Release(key, client.Channel, req.User)
		// MCX Step 5: notify the SIP gateway of the release (bridged channel).
		if s.sipGateway != nil {
			s.sipGateway.OnFloorRelease(key, userMCXID(req.User, client.AgencyID, s.config.McpttDomain))
		}
		return
	}

	// Server-authoritative priority (ignore client-claimed value).
	acct := s.findUserByUsername(req.User)
	pri := acct.effectivePriority()

	// MCX Step 4: apply group-type rules. Broadcast channels deny the floor to
	// anyone but the designated transmitter; emergency channels force Priority 1.
	allowed, forced := s.groupFloorRules(key, req.User, pri)
	if !allowed {
		s.sendFloorToUser(req.User, buildFloorDenied(req.Channel, FloorDenyNoPermission, 0, s.BroadcastUser(key)))
		return
	}
	if forced != 0 {
		pri = forced
	}
	s.floor.Request(key, req, pri)
	// MCX Step 5: notify the SIP gateway that a MeshRelay user took the floor on
	// the bridged channel, so it can signal the external MCX platform.
	if s.sipGateway != nil {
		s.sipGateway.OnFloorRequest(key, userMCXID(req.User, client.AgencyID, s.config.McpttDomain), pri)
	}
}

// SetUserPriority sets a user's provisioned floor priority (1-4) and persists it.
// Priority 0 or out-of-range leaves the account's priority as default RESPONDER.
func (s *Server) SetUserPriority(agencyID, username string, priority int) {
	if priority < PriorityEmergency || priority > PriorityMonitor {
		return
	}
	s.mu.Lock()
	if u := s.users[userKey(agencyID, username)]; u != nil {
		u.Priority = priority
	}
	s.mu.Unlock()
	s.saveUsers()
}

// handleEmergencyAlert processes an EMERGENCY_ALERT (0x26): set or cancel the
// channel emergency state. Any member can set; only the originator or a
// supervisor (admin/super_admin) can cancel.
func (s *Server) handleEmergencyAlert(alert *EmergencyAlert, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}
	s.mu.RLock()
	client, _ := s.findClientByUsername(alert.User)
	s.mu.RUnlock()
	if client == nil {
		return
	}
	if !s.floorEnabledForAgency(client.AgencyID) {
		return
	}
	key := channelKey(client.AgencyID, client.Channel)
	acct := s.findUserByUsername(alert.User)
	isSupervisor := acct != nil && (acct.Role == "admin" || acct.Role == "super_admin")
	s.floor.Emergency(key, client.Channel, alert.User, alert.Active, isSupervisor)
	// MCX Step 5: propagate an emergency set to the external MCX platform.
	if s.sipGateway != nil && alert.Active {
		s.sipGateway.OnEmergencyAlert(key, userMCXID(alert.User, client.AgencyID, s.config.McpttDomain))
	}
}
