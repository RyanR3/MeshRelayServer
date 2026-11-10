package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func consoleJSON(dc *DispatchConn) []map[string]interface{} {
	var out []map[string]interface{}
	for len(dc.Text) > 0 {
		var m map[string]interface{}
		json.Unmarshal(<-dc.Text, &m)
		out = append(out, m)
	}
	return out
}

func TestRecorderWritesIndexesAndFinds(t *testing.T) {
	dir := t.TempDir()
	r := &Recorder{dir: dir, retention: 30, active: map[string]*activeRec{}}
	r.Record("ag", 2, "alice", []byte{1, 2, 3})
	r.Record("ag", 2, "alice", []byte{4, 5})
	r.mu.Lock()
	for k, a := range r.active {
		a.last = a.last.Add(100 * time.Millisecond)
		r.finish(k, a)
	}
	r.mu.Unlock()
	time.Sleep(100 * time.Millisecond) // index append runs in a goroutine

	day := time.Now().Format(recDayFmt)
	list := r.Query(day, "ag", "", -1, "", 0)
	if len(list) != 1 || list[0].Frames != 2 || list[0].Channel != 2 || list[0].User != "alice" {
		t.Fatalf("index: %+v", list)
	}
	if other := r.Query(day, "other", "", -1, "", 0); len(other) != 0 {
		t.Fatalf("another agency must not see this recording")
	}
	info := r.Find(list[0].ID)
	if info == nil {
		t.Fatalf("Find failed for %s", list[0].ID)
	}
	data, _ := os.ReadFile(filepath.Join(dir, info.File))
	rd := bytes.NewReader(data)
	var off uint32
	var n uint16
	binary.Read(rd, binary.BigEndian, &off)
	binary.Read(rd, binary.BigEndian, &n)
	frame := make([]byte, n)
	rd.Read(frame)
	if n != 3 || !bytes.Equal(frame, []byte{1, 2, 3}) {
		t.Fatalf("first frame on disk: len %d %v", n, frame)
	}
}

func TestUnitStatusFromUnitAndConsole(t *testing.T) {
	s, dc, key, sent := dispatchTestServer(t)
	dc.Text = make(chan []byte, 16)
	s.authenticatedAddrs = map[string]string{}
	alice := s.clients[userKey("ag", "alice")]

	s.handleUnitStatus(&UnitStatusPacket{Code: 3}, alice.Addr)
	if st := s.statusFor("ag", "alice"); st == nil || st["name"] != "En route" {
		t.Fatalf("unit status not stored: %v", st)
	}
	s.consoleSetStatus(s.clients[key], "ag", "alice", 4)
	got := sent["alice"][len(sent["alice"])-1]
	if got[0] != PacketTypeUnitStatus || got[1] != 4 {
		t.Fatalf("alice should be told her new status, got %v", got)
	}
	msgs := consoleJSON(dc)
	if len(msgs) != 2 || msgs[1]["name"] != "On scene" || msgs[1]["by"] != "dispatcher" {
		t.Fatalf("console updates: %v", msgs)
	}
}

func TestDispatchMessagesBothWaysAndOffline(t *testing.T) {
	s, dc, key, sent := dispatchTestServer(t)
	dc.Text = make(chan []byte, 16)
	s.authenticatedAddrs = map[string]string{}
	console := s.clients[key]

	s.consoleSendMessage(console, "Respond to 12 Main St", &messageTarget{Agency: "ag", User: "alice"}, "", "")
	got := sent["alice"]
	if len(got) != 1 || got[0][0] != PacketTypeDispatchMessage {
		t.Fatalf("alice should get the message, got %d packets", len(got))
	}
	r := bytes.NewReader(got[0][1:])
	from, _ := getStr(r)
	text, _ := getStr(r)
	if from != "Dispatch (dispatcher)" || text != "Respond to 12 Main St" {
		t.Fatalf("message decoded as %q / %q", from, text)
	}

	// Reply from the unit reaches the console; the unit gets "delivered".
	s.handleDispatchMessage(&DispatchMessagePacket{Text: "Copy, en route", At: 777}, s.clients[userKey("ag", "alice")].Addr)
	msgs := consoleJSON(dc)
	if len(msgs) != 2 || msgs[1]["dir"] != "in" || msgs[1]["from"] != "alice" {
		t.Fatalf("console message feed: %v", msgs)
	}
	last := sent["alice"][len(sent["alice"])-1]
	if last[0] != PacketTypeDispatchReceipt || last[1] != 1 || binary.BigEndian.Uint64(last[2:]) != 777 {
		t.Fatalf("alice should get a delivered receipt for her message 777, got %v", last)
	}
	// The console shows it: alice gets "read", once.
	s.consoleMarkRead(console, []string{msgs[1]["id"].(string)})
	s.consoleMarkRead(console, []string{msgs[1]["id"].(string)})
	reads := 0
	for _, p := range sent["alice"] {
		if p[0] == PacketTypeDispatchReceipt && p[1] == 2 {
			reads++
		}
	}
	if reads != 1 {
		t.Fatalf("alice should get exactly one read receipt, got %d", reads)
	}
	consoleJSON(dc)
	if h := s.messageHistory("ag"); len(h) != 2 {
		t.Fatalf("history has %d", len(h))
	}

	// Offline unit: queued, delivered on join.
	s.consoleSendMessage(console, "Call me", &messageTarget{Agency: "ag", User: "bob"}, "", "")
	bobAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 45000}
	var bobGot []byte
	capture := s.testFloorSink
	s.testFloorSink = func(a *net.UDPAddr, d []byte) {
		if a.String() == bobAddr.String() {
			bobGot = d
			return
		}
		capture(a, d)
	}
	s.deliverPending("ag", "bob", bobAddr)
	if bobGot == nil || bobGot[0] != PacketTypeDispatchMessage {
		t.Fatalf("bob should get the waiting message when he joins")
	}
}

func TestAllCallReachesEveryTalkgroup(t *testing.T) {
	s, dc, key, sent := dispatchTestServer(t)
	s.agencies = map[string]*Agency{"ag": {ID: "ag", MaxChannels: 3}}
	// alice is on channel 0; add carol on channel 2.
	carolAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 46000}
	carol := &Client{Username: "carol", AgencyID: "ag", Channel: 2, Addr: carolAddr}
	s.clients[userKey("ag", "carol")] = carol
	s.channels["ag:2"] = &Channel{Clients: map[string]*Client{userKey("ag", "carol"): carol}}
	var carolGot int
	capture := s.testFloorSink
	s.testFloorSink = func(a *net.UDPAddr, d []byte) {
		if a.String() == carolAddr.String() && d[0] == PacketTypeAudioOpus {
			carolGot++
			return
		}
		capture(a, d)
	}

	s.clients[key].DispatchAllCall = true
	s.dispatchPTT(key, &dc.tx, true)
	s.dispatchAudioIn(key, &dc.tx, []byte{0xF8, 1})
	if carolGot != 1 || len(sent["alice"]) == 0 || sent["alice"][len(sent["alice"])-1][0] != PacketTypeAudioOpus {
		t.Fatalf("all-call should reach channel 0 (alice) and channel 2 (carol): carol=%d", carolGot)
	}
}
