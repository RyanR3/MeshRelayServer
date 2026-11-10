package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// A console "dispatcher" in agency ag, tuned to channel 0, beside field user alice.
func dispatchTestServer(t *testing.T) (*Server, *DispatchConn, string, map[string][][]byte) {
	t.Helper()
	s, sent := newFloorTestServer(t, "alice")
	s.activityLog = NewActivityLog(t.TempDir()+"/activity.json", 100)
	s.dispatchConns = map[string]*DispatchConn{}
	dc := &DispatchConn{Send: make(chan []byte, 64)}
	addr := newDispatchAddr()
	dispatchByAddr.Store(addr.String(), dc)
	t.Cleanup(func() { dispatchByAddr.Delete(addr.String()) })
	key := userKey("ag", "dispatcher")
	s.clients[key] = &Client{Username: "dispatcher", AgencyID: "ag", Channel: 0, Addr: addr, LastHeartbeat: time.Now()}
	s.dispatchConns[key] = dc
	// Packets for the console go to its WebSocket, as in sendPacket.
	capture := s.testFloorSink
	s.testFloorSink = func(a *net.UDPAddr, data []byte) {
		if !deliverToDispatch(data, a) {
			capture(a, data)
		}
	}
	return s, dc, key, sent
}

func drain(dc *DispatchConn) [][]byte {
	var out [][]byte
	for {
		select {
		case f := <-dc.Send:
			out = append(out, f)
		default:
			return out
		}
	}
}

func TestFieldOpusReachesConsole(t *testing.T) {
	s, dc, _, _ := dispatchTestServer(t)
	opus := []byte{0xF8, 1, 2, 3}

	if n := s.fanOutToDispatch("ag", 0, "alice", opus); n != 1 {
		t.Fatalf("sent to %d consoles, want 1", n)
	}
	f := drain(dc)
	if len(f) != 1 || f[0][0] != dispatchFrameAudioOut {
		t.Fatalf("console got %v", f)
	}
	r := bytes.NewReader(f[0][1:])
	var ch uint32
	binary.Read(r, binary.BigEndian, &ch)
	sender, _ := getStr(r)
	agency, _ := getStr(r)
	rest := make([]byte, r.Len())
	r.Read(rest)
	if ch != 0 || sender != "alice" || agency != "ag" || !bytes.Equal(rest, opus) {
		t.Fatalf("frame decoded as ch=%d sender=%q agency=%q opus=%v", ch, sender, agency, rest)
	}

	// Other channel, other agency, and the console's own audio: not heard.
	s.fanOutToDispatch("ag", 3, "alice", opus)
	s.fanOutToDispatch("other", 0, "bob", opus)
	s.fanOutToDispatch("ag", 0, "dispatcher", opus)
	if f := drain(dc); len(f) != 0 {
		t.Fatalf("console should not hear these, got %d frames", len(f))
	}
}

func TestConsoleAudioOnlyWhileKeyed(t *testing.T) {
	s, dc, key, sent := dispatchTestServer(t)
	opus := []byte{0xF8, 9, 9}

	s.dispatchAudioIn(key, &dc.tx, opus)
	if len(sent["alice"]) != 0 {
		t.Fatalf("unkeyed console audio must not be relayed")
	}

	s.config.AuthEnabled = false
	s.dispatchPTT(key, &dc.tx, true)
	if f := drain(dc); len(f) != 1 || f[0][0] != PacketTypeFloorGranted {
		t.Fatalf("open channel: console should be granted, got %v", f)
	}
	s.dispatchAudioIn(key, &dc.tx, opus)
	got := sent["alice"]
	if len(got) != 1 || got[0][0] != PacketTypeAudioOpus {
		t.Fatalf("alice should get one Opus packet, got %d", len(got))
	}
	pkt, err := ParseAudioPacket(got[0][1:])
	if err != nil || pkt.Username != "dispatcher" || !bytes.Equal(pkt.AudioData, opus) {
		t.Fatalf("relayed packet %+v err %v", pkt, err)
	}

	s.dispatchPTT(key, &dc.tx, false)
	s.dispatchAudioIn(key, &dc.tx, opus)
	if len(sent["alice"]) != 1 {
		t.Fatalf("audio after unkey must not be relayed")
	}
}

func TestConsoleUsesFloorControl(t *testing.T) {
	s, dc, key, sent := dispatchTestServer(t)
	s.agencies = map[string]*Agency{"ag": {ID: "ag", FloorControl: true}}
	s.floorOn.Store("ag", floorModeBusy)

	// alice holds the floor; the console (COMMAND priority) preempts her.
	s.floor.Request("ag:0", req(0, "alice"), PriorityResponder)
	s.dispatchPTT(key, &dc.tx, true)
	if !s.floor.HasFloor("ag:0", "dispatcher") {
		t.Fatalf("console at COMMAND priority should take the floor from a responder")
	}
	granted := false
	for _, f := range drain(dc) {
		granted = granted || f[0] == PacketTypeFloorGranted
	}
	if !granted {
		t.Fatalf("console should be told it has the floor")
	}
	if lastPacketType(sent, "alice") != PacketTypeFloorPreempted {
		t.Fatalf("alice should be told she was preempted")
	}

	s.dispatchPTT(key, &dc.tx, false)
	if s.floor.HasFloor("ag:0", "dispatcher") {
		t.Fatalf("unkey must release the floor")
	}
}
