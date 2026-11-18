package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type parsedTranscripts struct {
	channel int32
	flags   byte
	entries []phoneTranscript
}

func parseTranscriptsPacket(t *testing.T, p []byte) parsedTranscripts {
	t.Helper()
	if p[0] != PacketTypeTranscripts {
		t.Fatalf("packet type %#x", p[0])
	}
	r := bytes.NewReader(p[1:])
	var out parsedTranscripts
	binary.Read(r, binary.BigEndian, &out.channel)
	out.flags, _ = r.ReadByte()
	var n uint16
	binary.Read(r, binary.BigEndian, &n)
	for i := 0; i < int(n); i++ {
		var e phoneTranscript
		e.ID, _ = getStr(r)
		e.User, _ = getStr(r)
		binary.Read(r, binary.BigEndian, &e.Start)
		var d uint32
		binary.Read(r, binary.BigEndian, &d)
		e.DurationMs = int64(d)
		e.State, _ = r.ReadByte()
		e.Text, _ = getStr(r)
		out.entries = append(out.entries, e)
	}
	if r.Len() != 0 {
		t.Fatalf("%d trailing bytes", r.Len())
	}
	return out
}

// phoneSink captures packets per phone address.
func phoneSink(s *Server) (func(addr string) [][]byte, func()) {
	var mu sync.Mutex
	got := map[string][][]byte{}
	s.testFloorSink = func(a *net.UDPAddr, p []byte) {
		mu.Lock()
		got[a.String()] = append(got[a.String()], append([]byte(nil), p...))
		mu.Unlock()
	}
	return func(addr string) [][]byte {
			mu.Lock()
			defer mu.Unlock()
			return got[addr]
		}, func() {
			mu.Lock()
			got = map[string][][]byte{}
			mu.Unlock()
		}
}

func addPhone(s *Server, agency, user string, port int) *net.UDPAddr {
	a := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	s.clients[userKey(agency, user)] = &Client{Username: user, AgencyID: agency, Addr: a}
	return a
}

func TestPhoneTranscriptHistoryAndLive(t *testing.T) {
	trSubsMu.Lock()
	trSubs = map[string]trSub{}
	trSubsMu.Unlock()
	s, consoles := newTranscribeTestServer(t, fakeHelper)
	sent, reset := phoneSink(s)
	phone := addPhone(s, "ag", "medic3", 41000)
	outsider := addPhone(s, "other", "spy", 41001)

	// One finished transcript on channel 0 already.
	first := recordFor(t, s, "ag", "engine41", 50)
	s.queueTranscription(first)
	waitTranscript(t, consoles)

	// History.
	s.handleTranscriptRequest(&TranscriptRequest{Channel: 0, Subscribe: true, Limit: 20}, phone)
	pk := sent(phone.String())
	if len(pk) != 1 {
		t.Fatalf("history: %d packets", len(pk))
	}
	h := parseTranscriptsPacket(t, pk[0])
	if h.flags&trFlagEnabled == 0 || h.flags&trFlagLast == 0 || len(h.entries) != 1 ||
		h.entries[0].ID != first.ID || h.entries[0].User != "engine41" || !strings.HasPrefix(h.entries[0].Text, "heard ") {
		t.Fatalf("history = %+v", h)
	}

	// Another agency asking for channel 0 sees nothing of ours.
	s.handleTranscriptRequest(&TranscriptRequest{Channel: 0, Subscribe: true, Limit: 20}, outsider)
	if o := parseTranscriptsPacket(t, sent(outsider.String())[0]); len(o.entries) != 0 || o.flags&trFlagEnabled != 0 {
		t.Fatalf("other agency got %+v", o)
	}

	// Live: a new transmission is announced as pending, then its text.
	reset()
	second := recordFor(t, s, "ag", "engine41", 50)
	s.pushTranscriptToPhones(second, trStatePending, "") // what the recorder does when it queues
	s.queueTranscription(second)
	waitTranscript(t, consoles)
	pk = sent(phone.String())
	if len(pk) != 2 {
		t.Fatalf("live: %d packets, want pending + text", len(pk))
	}
	p1, p2 := parseTranscriptsPacket(t, pk[0]), parseTranscriptsPacket(t, pk[1])
	if p1.flags&trFlagLive == 0 || p1.entries[0].State != trStatePending ||
		p2.entries[0].State != trStateDone || p2.entries[0].ID != second.ID {
		t.Fatalf("live packets %+v / %+v", p1, p2)
	}
	if len(sent(outsider.String())) != 0 {
		t.Fatal("the other agency's phone got a live transcript")
	}

	// Unsubscribe: no more live updates.
	reset()
	s.handleTranscriptRequest(&TranscriptRequest{Channel: 0, Subscribe: false}, phone)
	s.pushTranscriptToPhones(second, trStateDone, "x")
	if len(sent(phone.String())) != 0 {
		t.Fatal("sent after unsubscribe")
	}
}

func TestTranscriptPacketsSplitAndExpire(t *testing.T) {
	var entries []phoneTranscript
	for i := 0; i < 30; i++ {
		entries = append(entries, phoneTranscript{ID: "id", User: "u", Text: strings.Repeat("word ", 40), Start: 1})
	}
	pkts := buildTranscriptPackets(3, trFlagEnabled, entries, true)
	total := 0
	for i, p := range pkts {
		if len(p) > trPacketBudget+300 {
			t.Fatalf("packet %d is %d bytes", i, len(p))
		}
		pt := parseTranscriptsPacket(t, p)
		total += len(pt.entries)
		if last := pt.flags&trFlagLast != 0; last != (i == len(pkts)-1) {
			t.Fatalf("last flag wrong on packet %d", i)
		}
	}
	if total != 30 || len(pkts) < 5 {
		t.Fatalf("%d entries in %d packets", total, len(pkts))
	}
	// An empty history is still one packet, marked last.
	if p := buildTranscriptPackets(3, 0, nil, true); len(p) != 1 || parseTranscriptsPacket(t, p[0]).flags&trFlagLast == 0 {
		t.Fatal("empty history")
	}

	// Expired subscriptions are dropped.
	trSubsMu.Lock()
	trSubs = map[string]trSub{"ag:x": {agency: "ag", channel: 0, addr: &net.UDPAddr{Port: 1}, until: time.Now().Add(-time.Second)}}
	trSubsMu.Unlock()
	s := &Server{}
	called := false
	s.testFloorSink = func(*net.UDPAddr, []byte) { called = true }
	s.pushTranscriptToPhones(RecordingInfo{Agency: "ag"}, trStateDone, "x")
	trSubsMu.Lock()
	left := len(trSubs)
	trSubsMu.Unlock()
	if called || left != 0 {
		t.Fatal("expired subscription still served")
	}
}

func TestParseTranscriptRequest(t *testing.T) {
	r, err := ParseTranscriptRequest([]byte{0, 0, 0, 7, 1, 0, 25})
	if err != nil || r.Channel != 7 || !r.Subscribe || r.Limit != 25 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := ParseTranscriptRequest([]byte{0, 0}); err == nil {
		t.Fatal("short packet accepted")
	}
}

func TestPhoneRecordingAudio(t *testing.T) {
	s, _ := newTranscribeTestServer(t, fakeHelper)
	sent, _ := phoneSink(s)
	phone := addPhone(s, "ag", "medic3", 42000)
	outsider := addPhone(s, "other", "spy", 42001)
	info := recordFor(t, s, "ag", "engine41", 600) // ~4.2 KB → 5 chunks
	want, _ := os.ReadFile(filepath.Join(s.recorder.dir, info.File))

	parse := func(p []byte) (id string, seq, total int, data []byte) {
		r := bytes.NewReader(p[1:])
		id, _ = getStr(r)
		var a, b uint16
		binary.Read(r, binary.BigEndian, &a)
		binary.Read(r, binary.BigEndian, &b)
		rest := make([]byte, r.Len())
		r.Read(rest)
		return id, int(a), int(b), rest
	}
	wait := func(addr string, n int) [][]byte {
		deadline := time.Now().Add(3 * time.Second)
		for len(sent(addr)) < n && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		return sent(addr)
	}

	s.handleRecordingAudioRequest(&RecordingAudioRequest{ID: info.ID}, phone)
	total := (len(want) + trAudioChunk - 1) / trAudioChunk
	pk := wait(phone.String(), total)
	got := make([][]byte, total)
	for _, p := range pk {
		id, seq, tot, data := parse(p)
		if id != info.ID || tot != total {
			t.Fatalf("chunk id %q total %d", id, tot)
		}
		got[seq] = data
	}
	if !bytes.Equal(bytes.Join(got, nil), want) {
		t.Fatal("reassembled audio differs from the recording")
	}

	// Re-ask for one missed chunk only.
	before := len(sent(phone.String()))
	s.handleRecordingAudioRequest(&RecordingAudioRequest{ID: info.ID, First: 2, Count: 1}, phone)
	pk = wait(phone.String(), before+1)
	if _, seq, _, _ := parse(pk[len(pk)-1]); len(pk) != before+1 || seq != 2 {
		t.Fatalf("re-ask sent %d packets, seq %d", len(pk)-before, seq)
	}

	// Another agency is told "not found".
	s.handleRecordingAudioRequest(&RecordingAudioRequest{ID: info.ID}, outsider)
	if _, _, tot, _ := parse(wait(outsider.String(), 1)[0]); tot != 0 {
		t.Fatal("another agency got the audio")
	}
}
