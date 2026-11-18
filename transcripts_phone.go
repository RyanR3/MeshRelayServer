// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Channel history on the phones (channel ⋮ menu → History): the server's
// recordings of a channel, with their transcripts, so a unit can catch up on
// traffic it missed (offline, muted while Busy, or not listening).
//
// A phone asks for a channel's recent recordings and, while its screen is
// open, is sent new ones as they finish and are transcribed:
//
//	TranscriptRequest (0x33, phone→server): i32 channel | u8 subscribe | u16 limit
//	    subscribe 1 = also send me new transcripts on this channel for the next
//	    90 s (the phone repeats the request while the screen is open);
//	    0 = stop. limit 0 = only (un)subscribe, no history.
//
//	Transcripts (0x34, server→phone): i32 channel | u8 flags | u16 n | n × entry
//	    flags: 1 = transcription is on for the agency, 2 = live update (not
//	    history), 4 = last packet of the history answer, 8 = recording is on
//	    entry: str id | str user | i64 start ms | u32 duration ms | u8 state | str text
//	    state: 0 = transcript, 1 = being transcribed, 2 = no speech (drop it),
//	    3 = recorded, not transcribed
//
//	RecordingAudioRequest (0x35, phone→server): str id | u16 first | u16 count
//	    the recording's audio, chunks first..first+count-1 (count 0 = all)
//	RecordingAudio (0x36, server→phone): str id | u16 seq | u16 total | bytes
//	    one chunk of the .mrr file ([u32 ms][u16 len][Opus] frames); total 0 =
//	    not found / not allowed. The phone asks again for chunks it missed.
//
// History is newest first, sent in packets of ~1 KB. Only the phone's own
// agency's recordings on that channel are visible — the same people who
// heard the traffic. Older phones never ask.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	PacketTypeTranscriptRequest   byte = 0x33
	PacketTypeTranscripts         byte = 0x34
	PacketTypeRecordingAudioReq   byte = 0x35
	PacketTypeRecordingAudioChunk byte = 0x36

	trFlagEnabled = 1
	trFlagLive    = 2
	trFlagLast    = 4
	trFlagRecords = 8

	trStateDone    = 0
	trStatePending = 1
	trStateEmpty   = 2
	trStatePlain   = 3

	trAudioChunk = 1000

	trPhoneMaxLimit = 100
	trSubscription  = 90 * time.Second
	trPacketBudget  = 1000
)

type TranscriptRequest struct {
	Channel   int32
	Subscribe bool
	Limit     int
}

func ParseTranscriptRequest(data []byte) (*TranscriptRequest, error) {
	if len(data) < 7 {
		return nil, errors.New("transcript request too short")
	}
	return &TranscriptRequest{
		Channel:   int32(binary.BigEndian.Uint32(data[0:4])),
		Subscribe: data[4] == 1,
		Limit:     int(binary.BigEndian.Uint16(data[5:7])),
	}, nil
}

type phoneTranscript struct {
	ID, User, Text string
	Start          int64
	DurationMs     int64
	State          byte
}

func putTranscriptEntry(b *bytes.Buffer, e phoneTranscript) {
	putStr(b, e.ID)
	putStr(b, e.User)
	binary.Write(b, binary.BigEndian, e.Start)
	binary.Write(b, binary.BigEndian, uint32(e.DurationMs))
	b.WriteByte(e.State)
	putStr(b, e.Text)
}

// buildTranscriptPackets splits entries into packets of about trPacketBudget
// bytes. At least one packet is returned (an empty history says "none").
func buildTranscriptPackets(channel int32, flags byte, entries []phoneTranscript, history bool) [][]byte {
	var out [][]byte
	var body bytes.Buffer
	n := 0
	flush := func(last bool) {
		var p bytes.Buffer
		p.WriteByte(PacketTypeTranscripts)
		binary.Write(&p, binary.BigEndian, channel)
		f := flags
		if last && history {
			f |= trFlagLast
		}
		p.WriteByte(f)
		binary.Write(&p, binary.BigEndian, uint16(n))
		p.Write(body.Bytes())
		out = append(out, p.Bytes())
		body.Reset()
		n = 0
	}
	for _, e := range entries {
		if len(e.Text) > 900 {
			e.Text = e.Text[:900] + "…"
		}
		var one bytes.Buffer
		putTranscriptEntry(&one, e)
		if n > 0 && body.Len()+one.Len() > trPacketBudget {
			flush(false)
		}
		body.Write(one.Bytes())
		n++
	}
	flush(true)
	return out
}

// Subscriptions: which phones want live transcripts of which channel.
type trSub struct {
	agency  string
	channel int32
	addr    *net.UDPAddr
	until   time.Time
}

var (
	trSubsMu sync.Mutex
	trSubs   = map[string]trSub{} // userKey → subscription
)

func (s *Server) handleTranscriptRequest(req *TranscriptRequest, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}
	s.mu.RLock()
	c, _ := s.findClientByAddr(addr)
	var agency, user string
	enabled, recording := false, false
	if c != nil {
		agency, user = c.AgencyID, c.Username
		if a := s.agencies[agency]; a != nil {
			enabled = a.Transcribe && !a.NoRecording
			recording = !a.NoRecording && s.recorder != nil
		}
	}
	s.mu.RUnlock()
	if c == nil {
		return
	}
	if ok, _ := s.transcriber.ready(); !ok {
		enabled = false
	}

	key := userKey(agency, user)
	trSubsMu.Lock()
	if req.Subscribe {
		trSubs[key] = trSub{agency: agency, channel: req.Channel, addr: addr, until: time.Now().Add(trSubscription)}
	} else {
		delete(trSubs, key)
	}
	trSubsMu.Unlock()

	if req.Limit <= 0 {
		return
	}
	limit := req.Limit
	if limit > trPhoneMaxLimit {
		limit = trPhoneMaxLimit
	}
	var entries []phoneTranscript
	if s.recorder != nil {
		now := time.Now()
		for _, day := range []string{now.Format(recDayFmt), now.AddDate(0, 0, -1).Format(recDayFmt)} {
			for _, r := range s.recorder.Query(day, agency, "", int(req.Channel), "", 0) {
				state := byte(trStatePlain)
				if r.Transcribed {
					if r.Text == "" {
						continue // no speech: a key-up or squelch noise
					}
					state = trStateDone
				} else if enabled && time.Since(time.UnixMilli(r.Start)) < 2*time.Minute {
					state = trStatePending
				}
				entries = append(entries, phoneTranscript{ID: r.ID, User: r.User, Text: r.Text,
					Start: r.Start, DurationMs: r.DurationMs, State: state})
				if len(entries) >= limit {
					break
				}
			}
			if len(entries) >= limit {
				break
			}
		}
	}
	flags := byte(0)
	if enabled {
		flags |= trFlagEnabled
	}
	if recording {
		flags |= trFlagRecords
	}
	for _, p := range buildTranscriptPackets(req.Channel, flags, entries, true) {
		s.sendTranscriptPacket(p, addr)
	}
}

func (s *Server) sendTranscriptPacket(p []byte, addr *net.UDPAddr) {
	if s.testFloorSink != nil {
		s.testFloorSink(addr, p)
		return
	}
	s.sendPacket(p, addr)
}

// pushTranscriptToPhones sends one entry to the phones watching that channel.
func (s *Server) pushTranscriptToPhones(info RecordingInfo, state byte, text string) {
	now := time.Now()
	var addrs []*net.UDPAddr
	trSubsMu.Lock()
	for k, sub := range trSubs {
		if now.After(sub.until) {
			delete(trSubs, k)
			continue
		}
		if sub.agency == info.Agency && sub.channel == int32(info.Channel) {
			addrs = append(addrs, sub.addr)
		}
	}
	trSubsMu.Unlock()
	if len(addrs) == 0 {
		return
	}
	e := phoneTranscript{ID: info.ID, User: info.User, Text: text, Start: info.Start, DurationMs: info.DurationMs, State: state}
	flags := byte(trFlagLive | trFlagRecords)
	if state != trStatePlain {
		flags |= trFlagEnabled
	}
	pkts := buildTranscriptPackets(int32(info.Channel), flags, []phoneTranscript{e}, false)
	for _, a := range addrs {
		for _, p := range pkts {
			s.sendTranscriptPacket(p, a)
		}
	}
}

// --- recording audio for the phones ---------------------------------------------

type RecordingAudioRequest struct {
	ID           string
	First, Count int
}

func ParseRecordingAudioRequest(data []byte) (*RecordingAudioRequest, error) {
	r := bytes.NewReader(data)
	id, err := getStr(r)
	if err != nil {
		return nil, err
	}
	var first, count uint16
	if binary.Read(r, binary.BigEndian, &first) != nil || binary.Read(r, binary.BigEndian, &count) != nil {
		return nil, errors.New("recording audio request too short")
	}
	return &RecordingAudioRequest{ID: id, First: int(first), Count: int(count)}, nil
}

func recordingAudioChunk(id string, seq, total int, data []byte) []byte {
	var b bytes.Buffer
	b.WriteByte(PacketTypeRecordingAudioChunk)
	putStr(&b, id)
	binary.Write(&b, binary.BigEndian, uint16(seq))
	binary.Write(&b, binary.BigEndian, uint16(total))
	b.Write(data)
	return b.Bytes()
}

// handleRecordingAudioRequest sends (part of) a recording to a phone of the
// recording's agency.
func (s *Server) handleRecordingAudioRequest(req *RecordingAudioRequest, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}
	s.mu.RLock()
	c, _ := s.findClientByAddr(addr)
	agency := ""
	if c != nil {
		agency = c.AgencyID
	}
	s.mu.RUnlock()
	if c == nil || s.recorder == nil {
		return
	}
	info := s.recorder.Find(req.ID)
	var data []byte
	if info != nil && info.Agency == agency {
		data, _ = os.ReadFile(filepath.Join(s.recorder.dir, filepath.Clean(info.File)))
	}
	total := (len(data) + trAudioChunk - 1) / trAudioChunk
	if len(data) == 0 || total > 0xFFFF {
		s.sendTranscriptPacket(recordingAudioChunk(req.ID, 0, 0, nil), addr)
		return
	}
	first, count := req.First, req.Count
	if count <= 0 || first+count > total {
		count = total - first
	}
	if first < 0 || first >= total {
		return
	}
	go func() {
		for seq := first; seq < first+count; seq++ {
			end := (seq + 1) * trAudioChunk
			if end > len(data) {
				end = len(data)
			}
			s.sendTranscriptPacket(recordingAudioChunk(req.ID, seq, total, data[seq*trAudioChunk:end]), addr)
			if s.testFloorSink == nil && seq%8 == 7 {
				time.Sleep(2 * time.Millisecond) // don't burst a long recording into a phone's socket
			}
		}
	}()
}
