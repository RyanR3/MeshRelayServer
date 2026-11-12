package main

import (
	"encoding/binary"
	"testing"
)

// buildClientRtpPacket mirrors exactly what the Android/iOS clients put on the
// wire for a PacketTypeAudioRtp frame:
//
//	[0x13][12B RTP header][u16 userLen|user][i32 audioLen|opus]
func buildClientRtpPacket(user string, opus []byte, marker bool, seq uint16, ts, ssrc uint32) []byte {
	out := []byte{PacketTypeAudioRtp}
	hdr := make([]byte, 12)
	hdr[0] = 2 << 6 // V=2
	hdr[1] = 111    // PT
	if marker {
		hdr[1] |= 0x80
	}
	binary.BigEndian.PutUint16(hdr[2:4], seq)
	binary.BigEndian.PutUint32(hdr[4:8], ts)
	binary.BigEndian.PutUint32(hdr[8:12], ssrc)
	out = append(out, hdr...)
	ub := []byte(user)
	lenU := make([]byte, 2)
	binary.BigEndian.PutUint16(lenU, uint16(len(ub)))
	out = append(out, lenU...)
	out = append(out, ub...)
	lenA := make([]byte, 4)
	binary.BigEndian.PutUint32(lenA, uint32(len(opus)))
	out = append(out, lenA...)
	out = append(out, opus...)
	return out
}

func TestParseRtpAudioPacket(t *testing.T) {
	opus := []byte{0x01, 0x02, 0x03, 0x04}
	pkt := buildClientRtpPacket("raptor7", opus, true, 42, 320*3, 0xDEADBEEF)

	ptype, payload, err := ParsePacket(pkt)
	if err != nil {
		t.Fatalf("ParsePacket error: %v", err)
	}
	if ptype != PacketTypeAudioRtp {
		t.Fatalf("expected type 0x13, got 0x%02X", ptype)
	}
	ap, ok := payload.(*AudioPacket)
	if !ok {
		t.Fatalf("payload is not *AudioPacket")
	}
	if ap.Username != "raptor7" {
		t.Errorf("username = %q, want raptor7", ap.Username)
	}
	if string(ap.AudioData) != string(opus) {
		t.Errorf("audio data mismatch")
	}
	if ap.Codec != CodecOpus {
		t.Errorf("codec = %d, want Opus(%d)", ap.Codec, CodecOpus)
	}
	if ap.RTP == nil {
		t.Fatalf("RTP header not parsed")
	}
	if !ap.RTP.Marker {
		t.Errorf("marker not set")
	}
	if ap.RTP.PayloadType != 111 {
		t.Errorf("PT = %d, want 111", ap.RTP.PayloadType)
	}
	if ap.RTP.SequenceNumber != 42 {
		t.Errorf("seq = %d, want 42", ap.RTP.SequenceNumber)
	}
	if ap.RTP.Timestamp != 320*3 {
		t.Errorf("timestamp = %d, want %d", ap.RTP.Timestamp, 320*3)
	}
	if ap.RTP.SSRC != 0xDEADBEEF {
		t.Errorf("SSRC = 0x%X, want 0xDEADBEEF", ap.RTP.SSRC)
	}
}

// The relay must preserve the sender's RTP header byte-for-byte so receivers
// (and a future MCX gateway) see the true transmitter stream identity.
func TestRtpRelayPreservesHeader(t *testing.T) {
	opus := []byte{0xAA, 0xBB, 0xCC}
	orig := buildClientRtpPacket("ic1", opus, false, 1000, 320*10, 0x11223344)

	_, payload, err := ParsePacket(orig)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ap := payload.(*AudioPacket)

	// Reserialize the way the server relays it.
	relayed := append([]byte{PacketTypeAudioRtp}, SerializeAudioRtpPacket(ap)...)

	// Round-trip: re-parse the relayed bytes and confirm the RTP fields survived.
	_, p2, err := ParsePacket(relayed)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	ap2 := p2.(*AudioPacket)
	if ap2.RTP.SequenceNumber != 1000 || ap2.RTP.Timestamp != 320*10 || ap2.RTP.SSRC != 0x11223344 || ap2.RTP.Marker {
		t.Errorf("relay altered RTP header: %+v", ap2.RTP)
	}
	if string(ap2.AudioData) != string(opus) || ap2.Username != "ic1" {
		t.Errorf("relay altered audio body")
	}
}

// Legacy 0x00/0x10 packets must still parse with no RTP header.
func TestLegacyAudioStillParses(t *testing.T) {
	body := SerializeAudioPacket(&AudioPacket{Username: "legacy", AudioData: []byte{1, 2, 3}})
	// Opus legacy
	opusPkt := append([]byte{PacketTypeAudioOpus}, body...)
	ptype, payload, err := ParsePacket(opusPkt)
	if err != nil || ptype != PacketTypeAudioOpus {
		t.Fatalf("legacy opus parse failed: %v", err)
	}
	ap := payload.(*AudioPacket)
	if ap.RTP != nil {
		t.Errorf("legacy packet should have no RTP header")
	}
	if ap.Codec != CodecOpus || ap.Username != "legacy" {
		t.Errorf("legacy opus fields wrong")
	}
	// PCM legacy
	pcmPkt := append([]byte{PacketTypeAudio}, body...)
	_, p2, err := ParsePacket(pcmPkt)
	if err != nil {
		t.Fatalf("legacy pcm parse failed: %v", err)
	}
	if p2.(*AudioPacket).RTP != nil {
		t.Errorf("legacy pcm should have no RTP header")
	}
}

// Simulate a client's per-frame header advance and confirm seq/timestamp/marker
// behave per RFC 3550 across a talk burst.
func TestRtpBurstSequencing(t *testing.T) {
	seq := uint16(500)
	ts := uint32(1_000_000)
	for frame := 0; frame < 5; frame++ {
		marker := frame == 0
		pkt := buildClientRtpPacket("u", []byte{0x1}, marker, seq, ts, 0xABCD)
		_, payload, _ := ParsePacket(pkt)
		h := payload.(*AudioPacket).RTP
		if frame == 0 && !h.Marker {
			t.Errorf("frame 0 must have marker")
		}
		if frame > 0 && h.Marker {
			t.Errorf("frame %d must NOT have marker", frame)
		}
		if h.SequenceNumber != seq {
			t.Errorf("frame %d seq = %d, want %d", frame, h.SequenceNumber, seq)
		}
		if h.Timestamp != ts {
			t.Errorf("frame %d ts = %d, want %d", frame, h.Timestamp, ts)
		}
		seq++
		ts += 320 // 20ms @ 16kHz
	}
}
