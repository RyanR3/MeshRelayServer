// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// clientView is a 0x14 frame read the way the apps read it: [i32 channel]
// [12-byte RTP header][u16 userLen][user][i32 audioLen][audio], audio
// decoded as Opus.
type clientView struct {
	channel int
	marker  bool
	pt      uint8
	seq     uint16
	ssrc    uint32
	sender  string
	audio   []byte
}

func readTagged(t *testing.T, p []byte) clientView {
	t.Helper()
	if len(p) < 1+4+12 || p[0] != PacketTypeAudioRtpChan {
		t.Fatalf("not a 0x14 frame: % x", p)
	}
	v := clientView{channel: int(int32(binary.BigEndian.Uint32(p[1:5])))}
	hdr, rest, err := parseRTPHeader(p[5:])
	if err != nil {
		t.Fatalf("RTP header: %v", err)
	}
	v.marker, v.pt, v.seq, v.ssrc = hdr.Marker, hdr.PayloadType, hdr.SequenceNumber, hdr.SSRC
	body, err := ParseAudioPacket(rest)
	if err != nil {
		t.Fatalf("audio body: %v", err)
	}
	v.sender, v.audio = body.Username, body.AudioData
	return v
}

// The bytes a tuned client got before this change, built independently of
// the relay code.
func tunedBytes(p *AudioPacket, wireType byte) []byte {
	if wireType == PacketTypeAudioRtp {
		return append([]byte{PacketTypeAudioRtp}, SerializeAudioRtpPacket(p)...)
	}
	return append([]byte{wireType}, SerializeAudioPacket(p)...)
}

// An end-to-end encrypted payload: opaque to the server.
var sealedPayload = []byte{0xE2, 0xEE, 0x01, 0x9C, 0x00, 0x7F, 0x13, 0x14, 0xAA, 0x55}

func inbound(wireType byte, user string, seq uint16) *AudioPacket {
	p := &AudioPacket{Username: user, AudioData: append([]byte(nil), sealedPayload...)}
	switch wireType {
	case PacketTypeAudioOpus:
		p.Codec = CodecOpus
	case PacketTypeAudioRtp:
		p.Codec = CodecOpus
		p.RTP = &RTPHeader{Marker: seq == 1, PayloadType: 111, SequenceNumber: seq, Timestamp: 1000 + 320*uint32(seq), SSRC: 0xABCDEF01}
	}
	return p
}

// monitorTestServer: talker, tuned and both on channel 0; mon on channel 1.
// mon and both monitor channel 0 (both is also tuned to it).
func monitorTestServer(t *testing.T) (*Server, map[string][][]byte) {
	s, sent := newFloorTestServer(t, "talker", "tuned", "both", "mon")
	s.ssrcToUser = map[uint32]string{}
	s.channels["ag:1"] = &Channel{Clients: make(map[string]*Client)}
	mon := s.clients[userKey("ag", "mon")]
	delete(s.channels["ag:0"].Clients, userKey("ag", "mon"))
	mon.Channel = 1
	s.channels["ag:1"].Clients[userKey("ag", "mon")] = mon
	mon.MonitoredChannels = []int{0}
	s.clients[userKey("ag", "both")].MonitoredChannels = []int{0}
	return s, sent
}

func onlyPacket(t *testing.T, sent map[string][][]byte, user string) []byte {
	t.Helper()
	if n := len(sent[user]); n != 1 {
		t.Fatalf("%s got %d packets, want 1", user, n)
	}
	return sent[user][0]
}

func TestMonitorCopyIsTaggedForEachInboundType(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wireType byte
		tagged   bool
	}{
		{"0x00 raw PCM (no tagged form)", PacketTypeAudio, false},
		{"0x10 Opus", PacketTypeAudioOpus, true},
		{"0x13 RTP Opus", PacketTypeAudioRtp, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sent := monitorTestServer(t)
			talker := s.clients[userKey("ag", "talker")]
			pkt := inbound(tc.wireType, "talker", 1)
			want := tunedBytes(pkt, tc.wireType)
			s.handleAudioPacket(pkt, talker.Addr)

			// Tuned clients: byte-identical to before, one copy, also when
			// they monitor the channel too.
			for _, u := range []string{"tuned", "both"} {
				if got := onlyPacket(t, sent, u); !bytes.Equal(got, want) {
					t.Fatalf("%s (tuned) got % x, want % x", u, got, want)
				}
			}
			if len(sent["talker"]) != 0 {
				t.Fatal("talker got its own audio back")
			}

			got := onlyPacket(t, sent, "mon")
			if !tc.tagged {
				if !bytes.Equal(got, want) {
					t.Fatalf("PCM monitor copy changed: % x", got)
				}
				return
			}
			v := readTagged(t, got)
			if v.channel != 0 {
				t.Fatalf("tagged channel %d, want 0", v.channel)
			}
			if v.sender != "talker" || !bytes.Equal(v.audio, sealedPayload) {
				t.Fatalf("body changed: sender %q audio % x", v.sender, v.audio)
			}
			// Apart from the channel and header, the frame is the tuned frame.
			if !bytes.Equal(got[1+4+12:], want[len(want)-len(SerializeAudioPacket(pkt)):]) {
				t.Fatal("monitor body is not the tuned body")
			}
			if tc.wireType == PacketTypeAudioRtp {
				// The sender's own RTP header is kept.
				if !bytes.Equal(got[5:], want[1:]) || v.ssrc != 0xABCDEF01 || v.seq != 1 {
					t.Fatalf("RTP header not preserved: % x", got[5:17])
				}
			} else if !v.marker || v.pt != 111 {
				t.Fatalf("made-up header: marker=%v pt=%d, want first frame of a burst, PT 111", v.marker, v.pt)
			}
		})
	}
}

func TestMonitorCopyOfPCMTaggedNeverAsOpus(t *testing.T) {
	s, _ := newFloorTestServer(t)
	pcm := tunedBytes(inbound(PacketTypeAudio, "x", 0), PacketTypeAudio)
	if got := s.monitorAudio("k", 5, pcm); !bytes.Equal(got, pcm) {
		t.Fatal("raw PCM must not be put in 0x14 (the apps decode 0x14 as Opus)")
	}
}

// Made-up RTP numbering for 0x10: one SSRC per talk burst, sequence numbers
// stepping by one, the marker on the first frame only — what the apps' duplicate,
// stale-frame and reorder checks expect of a sender.
func TestMonitorOpusRTPNumbering(t *testing.T) {
	s, sent := monitorTestServer(t)
	clock := time.Unix(1_800_000_000, 0)
	s.monTag.now = func() time.Time { return clock }
	talker := s.clients[userKey("ag", "talker")]

	send := func() clientView {
		reset(sent)
		s.handleAudioPacket(inbound(PacketTypeAudioOpus, "talker", 0), talker.Addr)
		return readTagged(t, onlyPacket(t, sent, "mon"))
	}
	first := send()
	if !first.marker {
		t.Fatal("first frame of a burst should carry the marker")
	}
	prev := first
	for i := 0; i < 5; i++ {
		clock = clock.Add(20 * time.Millisecond)
		v := send()
		if v.ssrc != first.ssrc || v.seq != prev.seq+1 || v.marker {
			t.Fatalf("frame %d: ssrc %x seq %d marker %v after seq %d", i+2, v.ssrc, v.seq, v.marker, prev.seq)
		}
		prev = v
	}
	// Unkey, key again: a new burst.
	clock = clock.Add(2 * time.Second)
	next := send()
	if next.ssrc == first.ssrc || !next.marker {
		t.Fatalf("new burst: ssrc %x (was %x) marker %v", next.ssrc, first.ssrc, next.marker)
	}

	// Another talker on the same channel is a separate stream.
	other := s.clients[userKey("ag", "tuned")]
	reset(sent)
	s.handleAudioPacket(inbound(PacketTypeAudioOpus, "tuned", 0), other.Addr)
	if v := readTagged(t, onlyPacket(t, sent, "mon")); v.ssrc == next.ssrc || v.sender != "tuned" {
		t.Fatal("two talkers share a made-up stream")
	}
}

// A frame nobody monitors does not advance the numbering, so a monitor never
// sees a gap it would wait for.
func TestMonitorNumberingOnlyAdvancesForMonitoredFrames(t *testing.T) {
	s, sent := monitorTestServer(t)
	talker := s.clients[userKey("ag", "talker")]
	mon := s.clients[userKey("ag", "mon")]
	s.handleAudioPacket(inbound(PacketTypeAudioOpus, "talker", 0), talker.Addr)
	a := readTagged(t, onlyPacket(t, sent, "mon"))

	s.mu.Lock()
	mon.MonitoredChannels = nil
	s.clients[userKey("ag", "both")].MonitoredChannels = nil
	s.mu.Unlock()
	s.handleAudioPacket(inbound(PacketTypeAudioOpus, "talker", 0), talker.Addr)

	s.mu.Lock()
	mon.MonitoredChannels = []int{0}
	s.mu.Unlock()
	reset(sent)
	s.handleAudioPacket(inbound(PacketTypeAudioOpus, "talker", 0), talker.Addr)
	b := readTagged(t, onlyPacket(t, sent, "mon"))
	if b.ssrc != a.ssrc || b.seq != a.seq+1 {
		t.Fatalf("seq %d after %d (ssrc %x/%x)", b.seq, a.seq, b.ssrc, a.ssrc)
	}
}

func addAgencyClient(s *Server, sent map[string][][]byte, agency, user string, channel, port int) *Client {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: port}
	c := &Client{Username: user, AgencyID: agency, Channel: channel, Addr: addr}
	s.clients[userKey(agency, user)] = c
	key := channelKey(agency, channel)
	if s.channels[key] == nil {
		s.channels[key] = &Channel{Clients: make(map[string]*Client)}
	}
	s.channels[key].Clients[userKey(agency, user)] = c
	prev := s.testFloorSink
	s.testFloorSink = func(a *net.UDPAddr, data []byte) {
		if a.String() == addr.String() {
			sent[agency+"/"+user] = append(sent[agency+"/"+user], data)
			return
		}
		prev(a, data)
	}
	return c
}

// Mutual aid and LMR bridge partners: the partner agency's tuned clients get
// the untagged frame as before, its monitors get the same tagged frame as the
// talker's own agency.
func TestMonitorCopyTaggedAcrossAgencies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		channel int
		link    func(s *Server, ch int)
	}{
		{"mutual aid", 7, func(s *Server, ch int) {
			s.mutualAidLinks = map[string]*MutualAidLink{"l1": {ID: "l1", Agencies: []string{"ag", "pa"}, Channel: ch, Active: true}}
		}},
		{"LMR bridge partner", 100, func(s *Server, ch int) {
			s.lmrBridges = map[string]*LMRBridge{"b1": {ID: "b1", Channel: ch, Enabled: true, Agencies: []string{"ag", "pa"}}}
		}},
	} {
		for _, wireType := range []byte{PacketTypeAudio, PacketTypeAudioOpus, PacketTypeAudioRtp} {
			t.Run(tc.name, func(t *testing.T) {
				s, sent := newFloorTestServer(t, "x")
				gw := addAgencyClient(s, sent, "ag", "gateway", tc.channel, 41000)
				addAgencyClient(s, sent, "ag", "onch", tc.channel, 41001)
				m := addAgencyClient(s, sent, "ag", "mon", 3, 41002)
				m.MonitoredChannels = []int{tc.channel}
				addAgencyClient(s, sent, "pa", "ptuned", tc.channel, 41003)
				pm := addAgencyClient(s, sent, "pa", "pmon", 3, 41004)
				pm.MonitoredChannels = []int{tc.channel}
				pb := addAgencyClient(s, sent, "pa", "pboth", tc.channel, 41005)
				pb.MonitoredChannels = []int{tc.channel}
				tc.link(s, tc.channel)

				pkt := inbound(wireType, "gateway", 1)
				want := tunedBytes(pkt, wireType)
				s.relayAudioData(gw, pkt, want)

				for _, u := range []string{"ag/onch", "pa/ptuned", "pa/pboth"} {
					if got := onlyPacket(t, sent, u); !bytes.Equal(got, want) {
						t.Fatalf("type %#x: %s (tuned) got % x", wireType, u, got)
					}
				}
				own := onlyPacket(t, sent, "ag/mon")
				partner := onlyPacket(t, sent, "pa/pmon")
				if !bytes.Equal(own, partner) {
					t.Fatalf("type %#x: partner monitor copy differs from own-agency copy", wireType)
				}
				if wireType == PacketTypeAudio {
					if !bytes.Equal(partner, want) {
						t.Fatal("PCM monitor copy changed")
					}
					return
				}
				if v := readTagged(t, partner); v.channel != tc.channel || !bytes.Equal(v.audio, sealedPayload) {
					t.Fatalf("type %#x: partner monitor got ch %d audio % x", wireType, v.channel, v.audio)
				}
			})
		}
	}
}

// Dispatch console talk is Opus without an RTP header: monitors get it tagged,
// tuned phones exactly as before.
func TestConsoleTalkTaggedForMonitors(t *testing.T) {
	s, sent := monitorTestServer(t)
	consoleAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 9), Port: 49000}
	s.clients["console-1"] = &Client{Username: "Dispatch", AgencyID: "ag", Channel: 0, Addr: consoleAddr}
	opus := []byte{0x48, 0x01, 0x02, 0x03}

	s.dispatchAudioIn("console-1", &dispatchTX{keyed: true}, opus)

	want := tunedBytes(&AudioPacket{Username: "Dispatch", AudioData: opus, Codec: CodecOpus}, PacketTypeAudioOpus)
	for _, u := range []string{"tuned", "both", "talker"} {
		if got := onlyPacket(t, sent, u); !bytes.Equal(got, want) {
			t.Fatalf("%s (tuned) got % x", u, got)
		}
	}
	v := readTagged(t, onlyPacket(t, sent, "mon"))
	if v.channel != 0 || v.sender != "Dispatch" || !bytes.Equal(v.audio, opus) || !v.marker {
		t.Fatalf("monitor got %+v", v)
	}
}

// Zello bridge audio is Opus without an RTP header: monitors get it tagged,
// tuned phones exactly as before.
func TestZelloAudioTaggedForMonitors(t *testing.T) {
	s, sent := monitorTestServer(t)
	b := &ZelloBridge{ID: "z1", AgencyID: "ag",
		Mappings:  []ZelloMapping{{ZelloChannel: "Ops", MeshChannel: 0}},
		inStreams: map[int64]string{42: "Ops"}}
	opus := []byte{0x78, 0x9A, 0xBC}
	frame := append([]byte{0x01, 0, 0, 0, 42, 0, 0, 0, 7}, opus...)

	s.handleZelloBinaryAudio(b, frame)

	want := tunedBytes(&AudioPacket{Username: "Zello:Ops", AudioData: opus, Codec: CodecOpus}, PacketTypeAudioOpus)
	for _, u := range []string{"tuned", "both", "talker"} {
		if got := onlyPacket(t, sent, u); !bytes.Equal(got, want) {
			t.Fatalf("%s (tuned) got % x", u, got)
		}
	}
	v := readTagged(t, onlyPacket(t, sent, "mon"))
	if v.channel != 0 || v.sender != "Zello:Ops" || !bytes.Equal(v.audio, opus) {
		t.Fatalf("monitor got %+v", v)
	}
}

// The EMERGENCY channel keeps its own rule: only RTP audio is tagged for
// listeners that know the channel; Opus without RTP still arrives untagged.
func TestEmergencyListenersUnchangedForOpus(t *testing.T) {
	s, sent := emergencyTestServer(t)
	medic := s.clients[userKey("ag", "medic")]
	s.handleEmergencyAlert(&EmergencyAlert{Channel: 0, User: "medic", Active: true}, medic.Addr)
	reset(sent)
	pkt := inbound(PacketTypeAudioOpus, "medic", 0)
	want := tunedBytes(pkt, PacketTypeAudioOpus)
	s.handleAudioPacket(pkt, medic.Addr)
	for _, u := range []string{"alice", "bob", "old"} {
		if got := onlyPacket(t, sent, u); !bytes.Equal(got, want) {
			t.Fatalf("%s got % x, want the untagged Opus frame", u, got)
		}
	}
}
