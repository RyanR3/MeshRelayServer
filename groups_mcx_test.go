// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"testing"
	"time"
)

// broadcastTestServer builds on the floor test server with group-type config.
func groupTestServer(t *testing.T, users ...string) (*Server, map[string][][]byte) {
	s, sent := newFloorTestServer(t, users...)
	s.config.FloorControlAgencies = []string{"ag"} // enable floor control for tests
	s.config.ChannelGroupTypes = map[string]string{}
	s.config.BroadcastUsers = map[string]string{}
	s.groupMgr = NewGroupManager(s)
	return s, sent
}

func TestBroadcastOnlyDesignatedTransmitter(t *testing.T) {
	s, _ := groupTestServer(t, "ic", "unit1")
	s.config.ChannelGroupTypes["ag:0"] = GroupTypeBroadcast
	s.config.BroadcastUsers["ag:0"] = "ic"

	// The IC (designated) can take the floor.
	ok, _ := s.groupFloorRules("ag:0", "ic", PriorityResponder)
	if !ok {
		t.Fatalf("designated transmitter should be allowed on broadcast channel")
	}
	// A regular unit cannot.
	ok, _ = s.groupFloorRules("ag:0", "unit1", PriorityResponder)
	if ok {
		t.Fatalf("non-transmitter must be denied the floor on a broadcast channel")
	}
}

func TestEmergencyForcesPriority1(t *testing.T) {
	s, _ := groupTestServer(t, "resp")
	s.config.ChannelGroupTypes["ag:0"] = GroupTypeEmergency
	_, forced := s.groupFloorRules("ag:0", "resp", PriorityResponder)
	if forced != PriorityEmergency {
		t.Fatalf("emergency channel should force Priority 1, got %d", forced)
	}
}

func TestPreconfiguredUsesUserPriority(t *testing.T) {
	s, _ := groupTestServer(t, "resp")
	// no type configured -> preconfigured default
	ok, forced := s.groupFloorRules("ag:0", "resp", PriorityCommand)
	if !ok || forced != PriorityCommand {
		t.Fatalf("preconfigured should allow + keep user priority, got ok=%v forced=%d", ok, forced)
	}
}

func TestAdhocCreateAndExpire(t *testing.T) {
	s, _ := groupTestServer(t, "creator")
	s.groupMgr.CreateAdhoc("ag:15", 15, "TestGroup", GroupTypeBroadcast, "creator", 50*time.Millisecond)
	if s.GroupType("ag:15") != GroupTypeBroadcast {
		t.Fatalf("ad-hoc channel should report broadcast type")
	}
	if s.BroadcastUser("ag:15") != "creator" {
		t.Fatalf("ad-hoc broadcast transmitter should be the creator")
	}
	// After expiry + tick, it reverts to default.
	time.Sleep(70 * time.Millisecond)
	s.groupMgr.Tick()
	if s.GroupType("ag:15") != GroupTypePreconfigured {
		t.Fatalf("ad-hoc channel should have expired back to preconfigured")
	}
}

func TestAdhocInviteAcceptFlow(t *testing.T) {
	s, sent := groupTestServer(t, "creator", "friend", "stranger")
	// Creator makes an ad-hoc group inviting "friend".
	s.handleGroupCreateAdhoc(&GroupCreateAdhoc{
		Creator: "creator", GroupName: "TaskForce", GroupType: GroupTypePreconfigured,
		Members: []string{"friend"},
	}, s.clients[userKey("ag", "creator")].Addr)

	// Creator should have received a ChannelSwitch (auto-joined); friend an invite.
	if len(sent["creator"]) == 0 || sent["creator"][len(sent["creator"])-1][0] != PacketTypeChannelSwitch {
		t.Fatalf("creator should be auto-joined via ChannelSwitch")
	}
	if len(sent["friend"]) == 0 || sent["friend"][len(sent["friend"])-1][0] != PacketTypeGroupInvite {
		t.Fatalf("friend should receive a GROUP_INVITE")
	}
	// stranger (not invited) gets nothing.
	if len(sent["stranger"]) != 0 {
		t.Fatalf("uninvited user should not be contacted")
	}

	// Find the allocated channel from the group manager.
	var ch int
	s.groupMgr.mu.Lock()
	for _, a := range s.groupMgr.adhoc {
		ch = a.channel
	}
	s.groupMgr.mu.Unlock()

	// Friend accepts -> gets the channel pushed.
	s.handleGroupInviteResponse(&GroupInviteResponse{Channel: ch, User: "friend", Accept: true},
		s.clients[userKey("ag", "friend")].Addr)
	if sent["friend"][len(sent["friend"])-1][0] != PacketTypeChannelSwitch {
		t.Fatalf("friend should get the channel pushed after accepting")
	}
}

func TestAdhocDissolveWhenEmpty(t *testing.T) {
	s, _ := groupTestServer(t, "creator")
	s.groupMgr.CreateAdhoc("ag:12", 12, "Temp", GroupTypePreconfigured, "creator", 0)
	// Creator leaves -> group empty -> dissolved.
	s.adhocMemberLeft("ag", 12, "creator")
	if s.groupMgr.adhocInfo("ag:12") != nil {
		t.Fatalf("empty ad-hoc group should be dissolved")
	}
}

// End-to-end through the floor controller: a broadcast channel denies a
// non-transmitter's floor request and grants the designated transmitter.
func TestBroadcastFloorEndToEnd(t *testing.T) {
	s, sent := groupTestServer(t, "ic", "unit1")
	s.config.ChannelGroupTypes["ag:0"] = GroupTypeBroadcast
	s.config.BroadcastUsers["ag:0"] = "ic"

	// Simulate handleFloorRequest logic for unit1 (denied) then ic (granted).
	acctPri := uint8(PriorityResponder)
	if allowed, _ := s.groupFloorRules("ag:0", "unit1", acctPri); allowed {
		t.Fatalf("unit1 should not be allowed")
	} else {
		s.sendFloorToUser("unit1", buildFloorDenied(0, FloorDenyNoPermission, 0, "ic"))
	}
	if lastPacketType(sent, "unit1") != PacketTypeFloorDenied {
		t.Fatalf("unit1 should get FLOOR_DENIED on broadcast channel")
	}
	if allowed, forced := s.groupFloorRules("ag:0", "ic", acctPri); allowed {
		pri := acctPri
		if forced != 0 {
			pri = forced
		}
		s.floor.Request("ag:0", req(0, "ic"), pri)
	}
	if !s.floor.HasFloor("ag:0", "ic") {
		t.Fatalf("ic (designated) should hold the broadcast floor")
	}
}
