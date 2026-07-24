package main

import (
	"bytes"
	"encoding/binary"
	"log"
	"net"
	"sync"
	"time"
)

// MCX Step 4 — MCX group types, carried by floor-controlled channels. A channel's
// type changes its floor behavior (built on the Step 3 floor controller):
//
//   preconfigured — standard floor-controlled channel (default; today's behavior)
//   broadcast     — only the designated transmitter may hold the floor; everyone
//                   else is denied (one talker, all listen — e.g. IC to all units)
//   emergency     — every floor request is treated as Priority 1 (always preempts)
//
// Ad-hoc groups are dynamically created broadcast/preconfigured channels with an
// optional auto-expire. Membership for pre-configured/emergency is the channel's
// members; broadcast adds a designated transmitter.

const (
	GroupTypePreconfigured = "preconfigured"
	GroupTypeBroadcast     = "broadcast"
	GroupTypeEmergency     = "emergency"
	GroupTypeAdhoc         = "adhoc" // dynamically created; behaves like preconfigured for floor
)

// adhocChannel tracks a dynamically created group so it can auto-expire and
// dissolve when empty.
type adhocChannel struct {
	key       string
	channel   int
	name      string
	groupType string
	createdBy string
	createdAt time.Time
	expireAt  time.Time       // zero = no expiry
	members   map[string]bool // callsigns who accepted (for dissolve-on-empty)
}

// GroupManager holds runtime (non-config) group state: ad-hoc channels and their
// expiry. Config-defined types live in Config (ChannelGroupTypes/BroadcastUsers).
type GroupManager struct {
	mu     sync.Mutex
	adhoc  map[string]*adhocChannel // key "agencyID:channel" -> ad-hoc info
	server *Server
}

func NewGroupManager(s *Server) *GroupManager {
	return &GroupManager{adhoc: make(map[string]*adhocChannel), server: s}
}

// GroupType returns the MCX group type for a channel key, checking ad-hoc runtime
// state first, then config, defaulting to preconfigured.
func (s *Server) GroupType(key string) string {
	if s.groupMgr != nil {
		s.groupMgr.mu.Lock()
		a := s.groupMgr.adhoc[key]
		s.groupMgr.mu.Unlock()
		if a != nil {
			return a.groupType
		}
	}
	if t, ok := s.config.ChannelGroupTypes[key]; ok && t != "" {
		return t
	}
	return GroupTypePreconfigured
}

// BroadcastUser returns the designated transmitter callsign for a broadcast
// channel, or "" if none configured.
func (s *Server) BroadcastUser(key string) string {
	// Ad-hoc broadcast stores its transmitter as createdBy.
	if s.groupMgr != nil {
		s.groupMgr.mu.Lock()
		a := s.groupMgr.adhoc[key]
		s.groupMgr.mu.Unlock()
		if a != nil && a.groupType == GroupTypeBroadcast {
			return a.createdBy
		}
	}
	return s.config.BroadcastUsers[key]
}

// canRequestFloor applies MCX group-type rules to a floor request. Returns
// (allowed, forcedPriority). For broadcast, only the designated transmitter is
// allowed. For emergency, everyone is forced to Priority 1. forcedPriority of 0
// means "use the user's provisioned priority".
func (s *Server) groupFloorRules(key, user string, userPriority uint8) (bool, uint8) {
	switch s.GroupType(key) {
	case GroupTypeBroadcast:
		bu := s.BroadcastUser(key)
		if bu == "" || bu == user {
			return true, userPriority
		}
		return false, 0 // non-transmitter cannot take the floor on a broadcast channel
	case GroupTypeEmergency:
		return true, PriorityEmergency // emergency channels: always top priority
	default:
		return true, userPriority
	}
}

// CreateAdhoc creates a dynamically-created group channel with optional expiry.
// groupType is preconfigured or broadcast; broadcast designates `createdBy` as
// the transmitter. autoExpire of 0 = no expiry. The creator is auto-joined.
func (gm *GroupManager) CreateAdhoc(key string, channel int, name, groupType, createdBy string, autoExpire time.Duration) {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	a := &adhocChannel{
		key: key, channel: channel, name: name, groupType: groupType,
		createdBy: createdBy, createdAt: time.Now(),
		members: map[string]bool{createdBy: true},
	}
	if autoExpire > 0 {
		a.expireAt = time.Now().Add(autoExpire)
	}
	gm.adhoc[key] = a
}

// adhocInfo returns the ad-hoc channel for a key (or nil).
func (gm *GroupManager) adhocInfo(key string) *adhocChannel {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	return gm.adhoc[key]
}

// addMember records an accepted member. Returns false if the group is gone.
func (gm *GroupManager) addMember(key, user string) bool {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	a := gm.adhoc[key]
	if a == nil {
		return false
	}
	a.members[user] = true
	return true
}

// removeMember drops a member; returns (channel, name, nowEmpty) so the caller
// can dissolve an empty group.
func (gm *GroupManager) removeMember(key, user string) (int, string, bool) {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	a := gm.adhoc[key]
	if a == nil {
		return 0, "", false
	}
	delete(a.members, user)
	empty := len(a.members) == 0
	if empty {
		delete(gm.adhoc, key)
	}
	return a.channel, a.name, empty
}

// buildGroupInfo builds a GROUP_INFO packet (0x27) describing a channel's MCX
// group type and (for broadcast) the designated transmitter.
func buildGroupInfo(channel int, groupType, broadcastUser string) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeGroupInfo)
	binary.Write(buf, binary.BigEndian, int32(channel))
	putStr(buf, groupType)
	putStr(buf, broadcastUser)
	return buf.Bytes()
}

// sendGroupInfo sends the current group info for a channel to a client addr.
func (s *Server) sendGroupInfo(addr *net.UDPAddr, agencyID string, channel int) {
	key := channelKey(agencyID, channel)
	gt := s.GroupType(key)
	// Only send for non-default types (keeps normal channels quiet on the wire).
	if gt == GroupTypePreconfigured {
		return
	}
	s.sendPacket(buildGroupInfo(channel, gt, s.BroadcastUser(key)), addr)
}

// broadcastGroupInfo pushes the current group info to every client on a channel
// (e.g. after an admin changes the type). Always sends, including the default
// type, so clients that were on a broadcast/emergency channel clear their UI.
func (s *Server) broadcastGroupInfo(agencyID string, channel int) {
	key := channelKey(agencyID, channel)
	pkt := buildGroupInfo(channel, s.GroupType(key), s.BroadcastUser(key))
	s.broadcastFloorToChannel(key, pkt)
}

// Tick expires ad-hoc groups whose timer elapsed. Called from the server ticker.
func (gm *GroupManager) Tick() {
	gm.mu.Lock()
	toDissolve := []adhocDissolve{}
	now := time.Now()
	for key, a := range gm.adhoc {
		if !a.expireAt.IsZero() && now.After(a.expireAt) {
			toDissolve = append(toDissolve, adhocDissolve{key, a.channel, a.name})
			delete(gm.adhoc, key)
		}
	}
	gm.mu.Unlock()
	// Remove the dissolved channel from members' lists (ChannelSwitch active=false).
	for _, d := range toDissolve {
		gm.server.dissolveAdhocChannel(d.key, d.channel, d.name)
	}
}

type adhocDissolve struct {
	key     string
	channel int
	name    string
}

// ---- ad-hoc invite/accept wire format ----

func buildGroupInvite(channel int, groupName, groupType, createdBy string) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeGroupInvite)
	binary.Write(buf, binary.BigEndian, int32(channel))
	putStr(buf, groupName)
	putStr(buf, groupType)
	putStr(buf, createdBy)
	return buf.Bytes()
}

// GroupInviteResponse parsed from a client.
type GroupInviteResponse struct {
	Channel int
	User    string
	Accept  bool
}

func ParseGroupInviteResponse(data []byte) (*GroupInviteResponse, error) {
	r := bytes.NewReader(data)
	var ch int32
	if err := binary.Read(r, binary.BigEndian, &ch); err != nil {
		return nil, err
	}
	user, err := getStr(r)
	if err != nil {
		return nil, err
	}
	accept, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	return &GroupInviteResponse{Channel: int(ch), User: user, Accept: accept == 1}, nil
}

// GroupDisband parsed from a client: creator explicitly deletes an ad-hoc group.
type GroupDisband struct {
	Channel int
	User    string
}

// ParseGroupDisband: [i32 channel][u16 userLen][user]
func ParseGroupDisband(data []byte) (*GroupDisband, error) {
	r := bytes.NewReader(data)
	var ch int32
	if err := binary.Read(r, binary.BigEndian, &ch); err != nil {
		return nil, err
	}
	user, err := getStr(r)
	if err != nil {
		return nil, err
	}
	return &GroupDisband{Channel: int(ch), User: user}, nil
}

// GroupCreateAdhoc parsed from a client.
type GroupCreateAdhoc struct {
	Creator   string
	GroupName string
	GroupType string
	Members   []string
}

func ParseGroupCreateAdhoc(data []byte) (*GroupCreateAdhoc, error) {
	r := bytes.NewReader(data)
	creator, err := getStr(r)
	if err != nil {
		return nil, err
	}
	name, err := getStr(r)
	if err != nil {
		return nil, err
	}
	gtype, err := getStr(r)
	if err != nil {
		return nil, err
	}
	var count uint16
	if err := binary.Read(r, binary.BigEndian, &count); err != nil {
		return nil, err
	}
	members := make([]string, 0, count)
	for i := 0; i < int(count); i++ {
		m, err := getStr(r)
		if err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return &GroupCreateAdhoc{Creator: creator, GroupName: name, GroupType: gtype, Members: members}, nil
}

// ---- server handlers ----

// adhocAutoExpire is how long an ad-hoc group lives with no members before the
// tick reaps it (also dissolves immediately when the last member leaves).
const adhocAutoExpire = 4 * time.Hour

// handleGroupCreateAdhoc creates an ad-hoc group channel and invites members.
func (s *Server) handleGroupCreateAdhoc(pkt *GroupCreateAdhoc, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}
	s.mu.RLock()
	creator, _ := s.findClientByUsername(pkt.Creator)
	s.mu.RUnlock()
	if creator == nil {
		return
	}
	agencyID := creator.AgencyID

	// Allocate a free high channel (10-19), like Zello/mutual-aid.
	s.mu.Lock()
	channel := s.pickAdhocChannel(agencyID)
	s.mu.Unlock()
	if channel < 0 {
		return // no free slot
	}
	key := channelKey(agencyID, channel)

	gtype := pkt.GroupType
	if gtype != GroupTypeBroadcast {
		gtype = GroupTypePreconfigured // ad-hoc is standard unless broadcast requested
	}
	s.groupMgr.CreateAdhoc(key, channel, pkt.GroupName, gtype, pkt.Creator, adhocAutoExpire)

	// Push the channel to the creator immediately (they're auto-joined).
	s.sendPacket(SerializeChannelSwitch(&ChannelSwitchPacket{Channel: channel, ChannelName: pkt.GroupName, Active: true}), creator.Addr)

	// Invite each requested member (skip the creator).
	invite := buildGroupInvite(channel, pkt.GroupName, gtype, pkt.Creator)
	for _, m := range pkt.Members {
		if m == pkt.Creator {
			continue
		}
		s.mu.RLock()
		mc, _ := s.findClientByUsername(m)
		s.mu.RUnlock()
		if mc != nil && mc.AgencyID == agencyID {
			s.sendPacket(invite, mc.Addr)
		}
	}
	log.Printf("Ad-hoc group %q (ch %d, %s) created by %s [%s], invited %d", pkt.GroupName, channel, gtype, pkt.Creator, agencyID, len(pkt.Members))
}

// handleGroupInviteResponse processes accept/decline of an ad-hoc invite.
func (s *Server) handleGroupInviteResponse(resp *GroupInviteResponse, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}
	s.mu.RLock()
	client, _ := s.findClientByUsername(resp.User)
	s.mu.RUnlock()
	if client == nil {
		return
	}
	key := channelKey(client.AgencyID, resp.Channel)
	a := s.groupMgr.adhocInfo(key)
	if a == nil {
		return // group gone/expired
	}
	if resp.Accept {
		if s.groupMgr.addMember(key, resp.User) {
			// Push the channel to the accepting member so it appears in their list.
			s.sendPacket(SerializeChannelSwitch(&ChannelSwitchPacket{Channel: resp.Channel, ChannelName: a.name, Active: true}), client.Addr)
		}
	}
	// Decline: nothing to do (they were never added).
}

// removeAdhoc deletes an ad-hoc entry, returning it (nil if unknown).
func (gm *GroupManager) removeAdhoc(key string) *adhocChannel {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	a := gm.adhoc[key]
	delete(gm.adhoc, key)
	return a
}

// handleGroupDisband: the creator explicitly deletes an ad-hoc group. Anyone
// else's request is ignored (they can simply leave; dissolve-on-empty handles
// the rest).
func (s *Server) handleGroupDisband(pkt *GroupDisband, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}
	s.mu.RLock()
	client, _ := s.findClientByUsername(pkt.User)
	s.mu.RUnlock()
	if client == nil {
		return
	}
	key := channelKey(client.AgencyID, pkt.Channel)
	a := s.groupMgr.adhocInfo(key)
	if a == nil {
		return // not an ad-hoc group (mutual aid / Zello rows can't be disbanded)
	}
	if a.createdBy != pkt.User {
		log.Printf("Group disband refused: %s is not creator of %q (ch %d)", pkt.User, a.name, pkt.Channel)
		return
	}
	s.groupMgr.removeAdhoc(key)
	s.dissolveAdhocChannel(key, a.channel, a.name)
	log.Printf("Ad-hoc group %q (ch %d) disbanded by creator %s", a.name, a.channel, pkt.User)
}

// AdhocLeave is called when a member leaves an ad-hoc channel (channel switch
// away / disconnect); dissolves the group when empty.
func (s *Server) adhocMemberLeft(agencyID string, channel int, user string) {
	key := channelKey(agencyID, channel)
	if s.groupMgr.adhocInfo(key) == nil {
		return
	}
	ch, name, empty := s.groupMgr.removeMember(key, user)
	if empty {
		s.dissolveAdhocChannel(key, ch, name)
	}
}

// dissolveAdhocChannel removes an ad-hoc channel from all members' lists.
func (s *Server) dissolveAdhocChannel(key string, channel int, name string) {
	// Extract agency from the key.
	agencyID := key
	if i := indexByteLast(key, ':'); i >= 0 {
		agencyID = key[:i]
	}
	pkt := SerializeChannelSwitch(&ChannelSwitchPacket{Channel: channel, ChannelName: name, Active: false})
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
	log.Printf("Ad-hoc group ch %d (%s) dissolved", channel, name)
}

// pickAdhocChannel finds a free high channel (10-19) not used by another ad-hoc,
// Zello bridge, or mutual-aid link. Caller holds s.mu.
func (s *Server) pickAdhocChannel(agencyID string) int {
	used := map[int]bool{}
	if s.groupMgr != nil {
		s.groupMgr.mu.Lock()
		for _, a := range s.groupMgr.adhoc {
			used[a.channel] = true
		}
		s.groupMgr.mu.Unlock()
	}
	for _, l := range s.mutualAidLinks {
		if l.Active {
			used[l.Channel] = true
		}
	}
	for ch := 10; ch <= 19; ch++ {
		if !used[ch] {
			return ch
		}
	}
	return -1
}

func indexByteLast(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
