package main

import (
	"testing"
	"time"
)

// A removed client's cleanup must not need s.mu while the removal holds it.
// Each case below froze the whole server before the cleanup moved out of the
// lock, so a failure here is a hang, caught by the timeout.
func finishesInTime(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s deadlocked", what)
	}
}

func TestLeaveLastAdhocMemberDoesNotDeadlock(t *testing.T) {
	s, _ := groupTestServer(t, "creator")
	s.activityLog = NewActivityLog(t.TempDir()+"/activity.json", 100)
	s.groupMgr.CreateAdhoc("ag:0", 0, "Temp", GroupTypePreconfigured, "creator", 0)

	addr := s.clients[userKey("ag", "creator")].Addr
	finishesInTime(t, "last member leaving an ad-hoc group", func() { s.handleLeave(addr) })
	if s.groupMgr.adhocInfo("ag:0") != nil {
		t.Fatalf("empty ad-hoc group should be dissolved")
	}
}

func TestLeaveWhileHoldingFloorDoesNotDeadlock(t *testing.T) {
	s, sent := newFloorTestServer(t, "alice", "bob")
	s.activityLog = NewActivityLog(t.TempDir()+"/activity.json", 100)
	s.floor.Request("ag:0", req(0, "alice"), PriorityResponder)

	addr := s.clients[userKey("ag", "alice")].Addr
	finishesInTime(t, "floor holder leaving", func() { s.handleLeave(addr) })
	if s.floor.HasFloor("ag:0", "alice") {
		t.Fatalf("a departed client must not keep the floor")
	}
	toldIdle := false
	for _, p := range sent["bob"] {
		toldIdle = toldIdle || p[0] == PacketTypeFloorIdle
	}
	if !toldIdle {
		t.Fatalf("bob should be told the floor is idle")
	}
}

func TestTimeoutWhileHoldingFloorDoesNotDeadlock(t *testing.T) {
	s, _ := newFloorTestServer(t, "alice")
	s.activityLog = NewActivityLog(t.TempDir()+"/activity.json", 100)
	s.floor.Request("ag:0", req(0, "alice"), PriorityResponder)
	s.clients[userKey("ag", "alice")].LastHeartbeat = time.Now().Add(-time.Minute)

	finishesInTime(t, "floor holder timing out", s.checkHeartbeats)
	if s.floor.HasFloor("ag:0", "alice") {
		t.Fatalf("a timed-out client must not keep the floor")
	}
}

func TestTimeoutOfLastAdhocMemberDoesNotDeadlock(t *testing.T) {
	s, _ := groupTestServer(t, "creator")
	s.activityLog = NewActivityLog(t.TempDir()+"/activity.json", 100)
	s.groupMgr.CreateAdhoc("ag:0", 0, "Temp", GroupTypePreconfigured, "creator", 0)
	s.clients[userKey("ag", "creator")].LastHeartbeat = time.Now().Add(-time.Minute)

	finishesInTime(t, "last ad-hoc member timing out", s.checkHeartbeats)
	if s.groupMgr.adhocInfo("ag:0") != nil {
		t.Fatalf("empty ad-hoc group should be dissolved")
	}
}
