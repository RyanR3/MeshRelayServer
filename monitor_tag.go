// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"crypto/rand"
	"encoding/binary"
	"strconv"
	"sync"
	"time"
)

// Channel tagging for monitor copies.
//
// A client that MONITORS a channel (0x12 always-listen) without being tuned to
// it needs that channel's audio in the channel-tagged form (0x14), or it plays
// the audio as if it came from its own selected channel (wrong key, wrong
// history, wrong "talking on" label). The only tagged form the apps understand
// is 0x14 = [i32 channel][12-byte RTP header][0x10/0x13 body], and every app
// decodes a 0x14 payload as Opus. So:
//
//   - RTP audio (0x13) is tagged by putting the channel in front of it, as is.
//   - Opus audio without an RTP header (0x10 from older senders, dispatch
//     console talk, the Zello bridge) is tagged with an RTP header the server
//     makes up for the stream: a fresh SSRC per talk burst and a sequence
//     number that steps by one per relayed frame. The apps use the header only
//     to drop duplicates and late frames and to keep frames in order, which a
//     header numbered in relay order satisfies; the payload bytes are copied
//     unchanged, so an end-to-end encrypted payload stays as it was.
//   - Raw PCM (0x00) has no tagged form (0x14 is always decoded as Opus), so
//     monitors keep getting it untagged, as before.

const (
	// monitorBurstGap: a pause longer than this starts a new talk burst (new
	// SSRC, marker set). Senders send a frame every 20 ms while keyed.
	monitorBurstGap = 500 * time.Millisecond
	// monitorStreamIdle: made-up streams unused this long are forgotten.
	monitorStreamIdle = 30 * time.Second
	// monitorRTPPayloadType is the dynamic payload type the apps use for Opus.
	monitorRTPPayloadType = 111
	// monitorRTPTimestampStep: 20 ms at 16 kHz.
	monitorRTPTimestampStep = 320
)

type monitorRTPStream struct {
	ssrc uint32
	seq  uint16
	ts   uint32
	last time.Time
}

// monitorTagger numbers the made-up RTP streams. The zero value is ready.
type monitorTagger struct {
	mu      sync.Mutex
	streams map[string]*monitorRTPStream
	now     func() time.Time // test hook; nil = time.Now
}

func randUint32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(b[:])
}

// next returns the RTP header for the next frame of a stream.
func (t *monitorTagger) next(stream string) RTPHeader {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.now != nil {
		now = t.now()
	}
	st := t.streams[stream]
	if st != nil && now.Sub(st.last) <= monitorBurstGap {
		st.seq++
		st.ts += monitorRTPTimestampStep
		st.last = now
		return RTPHeader{PayloadType: monitorRTPPayloadType, SequenceNumber: st.seq, Timestamp: st.ts, SSRC: st.ssrc}
	}
	if t.streams == nil {
		t.streams = make(map[string]*monitorRTPStream)
	}
	for k, old := range t.streams {
		if now.Sub(old.last) > monitorStreamIdle {
			delete(t.streams, k)
		}
	}
	ssrc := randUint32()
	if st != nil && ssrc == st.ssrc {
		ssrc++
	}
	r := randUint32()
	st = &monitorRTPStream{ssrc: ssrc, seq: uint16(r), ts: randUint32(), last: now}
	t.streams[stream] = st
	return RTPHeader{Marker: true, PayloadType: monitorRTPPayloadType, SequenceNumber: st.seq, Timestamp: st.ts, SSRC: st.ssrc}
}

// monitorStreamKey names the stream one sender produces on one channel.
func monitorStreamKey(agency string, channel int, sender string) string {
	return agency + "\x00" + strconv.Itoa(channel) + "\x00" + sender
}

// tagRTPAudio returns the 0x14 form of a relay-ready 0x13 frame.
func tagRTPAudio(channel int, audioData []byte) []byte {
	tagged := make([]byte, 5, 5+len(audioData)-1)
	tagged[0] = PacketTypeAudioRtpChan
	binary.BigEndian.PutUint32(tagged[1:5], uint32(int32(channel)))
	return append(tagged, audioData[1:]...)
}

// monitorAudio returns the copy of a relay-ready audio frame (type byte +
// body, as tuned clients get it) for a client that hears it by monitoring
// the channel: the 0x14 form when one exists, else audioData unchanged.
// stream identifies the sender's stream for made-up RTP numbering.
func (s *Server) monitorAudio(stream string, channel int, audioData []byte) []byte {
	if len(audioData) < 1 {
		return audioData
	}
	switch audioData[0] {
	case PacketTypeAudioRtp:
		return tagRTPAudio(channel, audioData)
	case PacketTypeAudioOpus:
		hdr := serializeRTPHeader(s.monTag.next(stream))
		tagged := make([]byte, 5, 5+len(hdr)+len(audioData)-1)
		tagged[0] = PacketTypeAudioRtpChan
		binary.BigEndian.PutUint32(tagged[1:5], uint32(int32(channel)))
		tagged = append(tagged, hdr...)
		return append(tagged, audioData[1:]...)
	}
	return audioData // PCM: no tagged form
}
