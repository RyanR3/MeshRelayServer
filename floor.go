// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"encoding/binary"
	"log"
	"math"
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
//                          [| f64 lat | f64 lon]  (server→client, set only, when
//                          the unit's position is known; older clients ignore it)

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
	b, err := readN(buf, int(n))
	if err != nil {
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

// emergencyAlertPacket builds the 0x26 broadcast for an emergency on the
// channel at key, with the alerting unit's last reported position appended
// when it is set and known, so phones can show where the unit is.
func (s *Server) emergencyAlertPacket(key string, channel int, user string, active bool) []byte {
	pkt := buildEmergencyAlert(channel, user, active)
	if !active || s == nil {
		return pkt
	}
	agency := key
	if i := strings.LastIndexByte(key, ':'); i >= 0 {
		agency = key[:i]
	}
	s.mu.RLock()
	var loc *UnitLocation
	if c := s.clients[userKey(agency, user)]; c != nil && c.Loc != nil {
		l := *c.Loc
		loc = &l
	}
	s.mu.RUnlock()
	if loc == nil {
		return pkt
	}
	pkt = binary.BigEndian.AppendUint64(pkt, math.Float64bits(loc.Lat))
	return binary.BigEndian.AppendUint64(pkt, math.Float64bits(loc.Lon))
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
	// playback is set while the server itself holds the floor to replay a
	// recorded (queued) transmission on the user's behalf. cancel stops it.
	playback bool
	cancel   chan struct{}
}

type queued struct {
	user     string
	priority uint8
}

// deferredTx is a transmission accepted while the channel was busy (queue
// mode): recorded frame by frame and replayed once the floor is free.
type deferredTx struct {
	user     string
	priority uint8
	frames   []deferredFrame
	complete bool // sender has released PTT
	started  time.Time
}

type deferredFrame struct {
	packet    *AudioPacket
	audioData []byte
}

const (
	deferredMaxFrames = 3000 // ~60 s at 20 ms frames
	deferredFrameMs   = 20   // replay pacing
)

// floorChannel is the floor state for one agency-scoped channel key.
type floorChannel struct {
	holder    *floorHolder
	queue     []queued
	emergency bool
	emergBy   string
	emergAt   time.Time
	// Queue mode: transmissions recorded while busy, in arrival order, and the
	// ones still being recorded (sender has not released yet) by user.
	deferred  []*deferredTx
	recording map[string]*deferredTx
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
		fc.server.sendFloorToUserIn(key, req.User, buildFloorDenied(req.Channel, FloorDenyNoPermission, 0, holderName(c)))
		return
	}

	// Idle -> grant immediately.
	if c.holder == nil {
		fc.grant(key, req.Channel, req.User, effectivePriority)
		return
	}

	// Already the holder? Re-grant (idempotent).
	if c.holder.user == req.User {
		fc.server.sendFloorToUserIn(key, req.User, buildFloorGranted(req.Channel, req.User, 0, 0))
		return
	}

	// Higher priority (lower number) preempts.
	if effectivePriority < c.holder.priority {
		old := c.holder.user
		oldPri := c.holder.priority
		fc.interruptPlayback(c)
		if c.holder != nil {
			fc.server.sendFloorToUserIn(key, old, buildFloorPreempted(req.Channel, req.User, effectivePriority))
		}
		fc.grant(key, req.Channel, req.User, effectivePriority)
		log.Printf("Floor %s: %s (pri %d) PREEMPTED %s (pri %d)", key, req.User, effectivePriority, old, oldPri)
		return
	}

	// Queue mode: accept the transmission, record it, replay it after the
	// current talker. The client sees FLOOR_GRANTED with a queue position > 0
	// and keeps sending; those frames land in RecordDeferred.
	if fc.server.floorQueueForKey(key) {
		if d := c.recording[req.User]; d != nil {
			fc.server.sendFloorToUserIn(key, req.User, buildFloorGranted(req.Channel, req.User, 0, uint8(fc.deferredPos(c, d))))
			return
		}
		if len(c.deferred) >= floorMaxQueue {
			fc.server.sendFloorToUserIn(key, req.User, buildFloorDenied(req.Channel, FloorDenyChannelFull, 0, c.holder.user))
			return
		}
		d := &deferredTx{user: req.User, priority: effectivePriority, started: time.Now()}
		c.deferred = append(c.deferred, d)
		if c.recording == nil {
			c.recording = map[string]*deferredTx{}
		}
		c.recording[req.User] = d
		pos := fc.deferredPos(c, d)
		fc.server.sendFloorToUserIn(key, req.User, buildFloorGranted(req.Channel, req.User, 0, uint8(pos)))
		log.Printf("Floor %s: %s queued for replay (#%d) behind %s", key, req.User, pos, c.holder.user)
		return
	}

	// Equal/lower priority -> deny + queue.
	pos := fc.enqueue(c, req.User, effectivePriority)
	if pos == 0 {
		fc.server.sendFloorToUserIn(key, req.User, buildFloorDenied(req.Channel, FloorDenyChannelFull, 0, c.holder.user))
		return
	}
	fc.server.sendFloorToUserIn(key, req.User, buildFloorDenied(req.Channel, FloorDenyBusy, uint8(pos), c.holder.user))
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
	fc.server.sendFloorToUserIn(key, user, buildFloorGranted(channel, user, 0, 0))
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

// ReleaseAgency drops every held floor, queue and recording for an agency's
// channels — used when floor control is switched off for the agency.
func (fc *FloorController) ReleaseAgency(agencyID string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	prefix := agencyID + ":"
	for key, c := range fc.channels {
		if strings.HasPrefix(key, prefix) {
			fc.interruptPlayback(c)
			delete(fc.channels, key)
		}
	}
}

// Release frees the floor held by `user` (PTT release / disconnect). Grants the
// next queued user, or broadcasts FLOOR_IDLE. Caller must NOT hold fc.mu.
func (fc *FloorController) Release(key string, channel int, user string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	c := fc.ch(key)
	if d := c.recording[user]; d != nil {
		// Queue mode: the sender finished; the recording stays queued for replay.
		d.complete = true
		delete(c.recording, user)
		return
	}
	if c.holder == nil || c.holder.user != user || c.holder.playback {
		// Not the (live) holder — maybe queued; drop from queue.
		fc.dequeueUser(c, user)
		return
	}
	fc.releaseAndAdvance(key, channel, c)
}

// RecordDeferred stores a frame from a user who was granted a queued slot
// (queue mode). Frames from anyone else are dropped. Caller must NOT hold fc.mu.
func (fc *FloorController) RecordDeferred(key, user string, packet *AudioPacket, audioData []byte) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	c := fc.channels[key]
	if c == nil {
		return
	}
	d := c.recording[user]
	limit := deferredMaxFrames
	if isEmergencyKey(key) {
		limit = emergencyDeferredMaxFrames
	}
	if d == nil || len(d.frames) >= limit {
		return
	}
	d.frames = append(d.frames, deferredFrame{packet: packet, audioData: audioData})
}

// deferredPos is the 1-based position of a recording in the replay queue.
func (fc *FloorController) deferredPos(c *floorChannel, d *deferredTx) int {
	for i, x := range c.deferred {
		if x == d {
			return i + 1
		}
	}
	return 0
}

// interruptPlayback stops a replay in progress and puts what is left of it
// back at the front of the replay queue. Caller holds fc.mu.
func (fc *FloorController) interruptPlayback(c *floorChannel) {
	if c.holder == nil || !c.holder.playback {
		return
	}
	if c.holder.cancel != nil {
		close(c.holder.cancel)
	}
	c.holder = nil
}

// releaseAndAdvance clears the holder and gives the floor to the next waiting
// party: a recorded transmission (replayed by the server), then a live queued
// user, else idle. Caller holds fc.mu.
func (fc *FloorController) releaseAndAdvance(key string, channel int, c *floorChannel) {
	c.holder = nil
	if len(c.deferred) > 0 {
		d := c.deferred[0]
		c.deferred = c.deferred[1:]
		if len(d.frames) == 0 && d.complete {
			// Nothing was recorded (released before any audio) — skip it.
			fc.releaseAndAdvance(key, channel, c)
			return
		}
		h := &floorHolder{user: d.user, priority: d.priority, lastAudio: time.Now(),
			grantedAt: time.Now(), playback: true, cancel: make(chan struct{})}
		c.holder = h
		fc.server.broadcastFloorToChannel(key, buildFloorTaken(channel, d.user, d.priority))
		log.Printf("Floor %s: replaying queued transmission from %s (%d frames%s)",
			key, d.user, len(d.frames), map[bool]string{true: "", false: ", still recording"}[d.complete])
		go fc.replay(key, channel, c, h, d)
		return
	}
	if len(c.queue) > 0 {
		next := c.queue[0]
		c.queue = c.queue[1:]
		fc.grant(key, channel, next.user, next.priority)
		return
	}
	fc.server.broadcastFloorToChannel(key, buildFloorIdle(channel))
}

// replay paces a recorded transmission out to the channel. It runs while h is
// the channel's holder; preemption closes h.cancel and the remainder goes back
// to the front of the queue.
func (fc *FloorController) replay(key string, channel int, c *floorChannel, h *floorHolder, d *deferredTx) {
	i := 0
	waited := time.Duration(0)
	for {
		select {
		case <-h.cancel:
			fc.mu.Lock()
			if i < len(d.frames) {
				d.frames = d.frames[i:]
				c.deferred = append([]*deferredTx{d}, c.deferred...)
			}
			fc.mu.Unlock()
			return
		default:
		}
		fc.mu.Lock()
		if c.holder != h {
			fc.mu.Unlock()
			return
		}
		var f deferredFrame
		have := i < len(d.frames)
		done := !have && d.complete
		if have {
			f = d.frames[i]
			i++
			h.lastAudio = time.Now()
			waited = 0
		}
		if !have && !done {
			// Caught up with a sender who is still talking; wait for more, but
			// not forever — the dead-man window ends a stalled recording.
			waited += deferredFrameMs * time.Millisecond
			if waited > floorDeadManTimeout {
				d.complete = true
				delete(c.recording, d.user)
				done = true
			}
		}
		if done {
			fc.releaseAndAdvance(key, channel, c)
			fc.mu.Unlock()
			return
		}
		fc.mu.Unlock()
		if have {
			fc.server.relayDeferred(key, channel, d.user, f.packet, f.audioData)
		}
		time.Sleep(deferredFrameMs * time.Millisecond)
	}
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

// IsRecording reports whether `user` is being recorded for a queued replay.
func (fc *FloorController) IsRecording(key, user string) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	c := fc.channels[key]
	return c != nil && c.recording[user] != nil
}

// HasFloor reports whether `user` currently holds the floor on `key`. When floor
// control is enabled, only the holder's audio is relayed.
func (fc *FloorController) HasFloor(key, user string) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	c := fc.channels[key]
	// A replay holds the floor for the server, not for the user's live mic.
	return c != nil && c.holder != nil && c.holder.user == user && !c.holder.playback
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
		// Preempt to the emergency user (a replay in progress is paused and
		// resumes after).
		fc.interruptPlayback(c)
		if c.holder != nil && c.holder.user != user {
			fc.server.sendFloorToUserIn(key, c.holder.user, buildFloorPreempted(channel, user, PriorityEmergency))
		}
		fc.grant(key, channel, user, PriorityEmergency)
		fc.server.broadcastFloorToChannel(key, fc.server.emergencyAlertPacket(key, channel, user, true))
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
		if c.holder != nil && !c.holder.playback && now.Sub(c.holder.lastAudio) > floorDeadManTimeout {
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
		if d := c.recording[user]; d != nil {
			d.complete = true
			delete(c.recording, user)
		}
		if c.holder != nil && c.holder.user == user && !c.holder.playback {
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
	s.sendFloorToUserIn("", username, data)
}

// sendFloorToUserIn sends a floor packet to a user of the floor channel key's
// agency (key "" = any agency, the legacy by-name lookup). Floor state is
// tracked by callsign, and the same callsign can exist in several agencies:
// resolving by name alone delivered one agency's grants/denials to a
// same-named user in another.
func (s *Server) sendFloorToUserIn(key, username string, data []byte) {
	if s.testFloorUserSink != nil {
		s.testFloorUserSink(username, data)
		return
	}
	s.mu.RLock()
	var client *Client
	if key == "" {
		client, _ = s.findClientByUsername(username)
	} else {
		client = s.clientInAgency(agencyFromKey(key), username)
	}
	var addr *net.UDPAddr
	if client != nil {
		addr = client.Addr
	}
	s.mu.RUnlock()
	if addr != nil {
		if s.testFloorSink != nil {
			s.testFloorSink(addr, data)
			return
		}
		s.sendPacket(data, addr)
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
	// The requester is the client this address is signed in as; the name in
	// the request must be its callsign (another agency's user could otherwise
	// take or release this agency's floor).
	s.mu.RLock()
	var client *Client
	var acct *UserAccount
	if c := s.senderClient(req.User, addr); c != nil {
		cp := *c
		client = &cp
		// Server-authoritative priority from the login account (ignore the
		// client-claimed value).
		acct = s.accountFor(c)
	}
	s.mu.RUnlock()
	if client == nil {
		return
	}
	// A unit in emergency talks on the EMERGENCY channel: its PTT asks for
	// that channel's floor, whatever channel it is on.
	if s.emergencyActiveFor(client.AgencyID, req.User) {
		ekey := channelKey(client.AgencyID, EmergencyChannel)
		if req.Priority == 0 {
			s.floor.Release(ekey, EmergencyChannel, req.User)
		} else {
			s.touchEmergency(client.AgencyID, req.User)
			s.floor.Request(ekey, &FloorRequest{Channel: EmergencyChannel, User: req.User, Priority: PriorityEmergency}, PriorityEmergency)
		}
		return
	}
	// Nobody else talks on the EMERGENCY channel.
	if client.Channel == EmergencyChannel {
		if req.Priority != 0 {
			s.sendFloorToUserIn(channelKey(client.AgencyID, EmergencyChannel), req.User,
				buildFloorDenied(req.Channel, FloorDenyNoPermission, 0, ""))
		}
		return
	}
	if !s.floorEnabledForAgency(client.AgencyID) {
		// Floor control off for this agency: auto-grant so clients that gate on
		// grant still work (open-PTT semantics preserved).
		if req.Priority != 0 {
			s.sendFloorToUserIn(channelKey(client.AgencyID, client.Channel), req.User, buildFloorGranted(req.Channel, req.User, 0, 0))
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

	pri := acct.effectivePriority()

	// MCX Step 4: apply group-type rules. Broadcast channels deny the floor to
	// anyone but the designated transmitter; emergency channels force Priority 1.
	allowed, forced := s.groupFloorRules(key, req.User, pri)
	if !allowed {
		s.sendFloorToUserIn(key, req.User, buildFloorDenied(req.Channel, FloorDenyNoPermission, 0, s.BroadcastUser(key)))
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
	s.saveUsersUnlocked()
}

// handleEmergencyAlert processes an EMERGENCY_ALERT (0x26): set or cancel the
// channel emergency state. Any member can set; only the originator or a
// supervisor (admin/super_admin) can cancel.
func (s *Server) handleEmergencyAlert(alert *EmergencyAlert, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}
	// Only the signed-in client itself can raise or cancel its emergency.
	s.mu.RLock()
	var client *Client
	isSupervisor := false
	if c := s.senderClient(alert.User, addr); c != nil {
		cp := *c
		client = &cp
		acct := s.accountFor(c)
		isSupervisor = acct != nil && (acct.Role == "admin" || acct.Role == "super_admin")
	}
	s.mu.RUnlock()
	if client == nil {
		return
	}
	// The EMERGENCY channel (emergency_channel.go): the alert goes to the
	// whole agency and dispatch, and the unit's audio moves to that channel
	// until the emergency is cleared. The working channel is left alone.
	key := channelKey(client.AgencyID, client.Channel)
	if alert.Active {
		first := !s.emergencyActiveFor(client.AgencyID, alert.User)
		s.raiseEmergency(client.AgencyID, alert.User, client.Channel)
		// MCX Step 5: propagate an emergency set to the external MCX platform.
		if first && s.sipGateway != nil {
			s.sipGateway.OnEmergencyAlert(key, userMCXID(alert.User, client.AgencyID, s.config.McpttDomain))
		}
		return
	}
	if s.clearEmergency(client.AgencyID, alert.User, alert.User) {
		return
	}
	if isSupervisor {
		// A supervisor's cancel: the emergencies raised on their channel.
		s.clearEmergenciesOnChannel(client.AgencyID, client.Channel, alert.User)
		return
	}
	// Not active (already cleared, or the server restarted): confirm the
	// clear to the unit so its phone stops showing it.
	s.sendFloorToUserIn(key, alert.User, buildEmergencyAlert(client.Channel, alert.User, false))
}
