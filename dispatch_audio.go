// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Dispatch console audio (browser <-> field), Opus both ways.
//
// Field units send 16 kHz Opus (0x10, or RTP-framed 0x13). The console decodes
// it in the browser (WebCodecs) and encodes its microphone the same way, so the
// server never transcodes: it only re-frames.
//
// WebSocket binary frames, server -> browser:
//   0xA0 audio:  [0xA0][u32 channel][u16 len|sender][u16 len|agency][Opus frame]
//   0x21-0x26:   floor / emergency packets addressed to the console, verbatim
//                (same layout the apps receive over UDP)
// browser -> server:
//   0xB0 audio:  [0xB0][Opus frame]  — only relayed while the console holds PTT
// JSON text frames, browser -> server:
//   {"type":"channel","channel":N}   tune the console
//   {"type":"ptt","down":true|false} key / unkey (floor request / release)

import (
	"encoding/binary"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dispatchFrameAudioOut byte = 0xA0 // server -> browser
	dispatchFrameAudioIn  byte = 0xB0 // browser -> server
)

// Each console gets its own loopback address so packets the server "sends" to
// it (floor grants, emergency alerts) can be routed to its WebSocket instead
// of the UDP socket. 127.0.0.2 is never a real client address here.
var (
	dispatchAddrPort uint32   = 1000
	dispatchByAddr   sync.Map // addr string -> *DispatchConn
)

func newDispatchAddr() *net.UDPAddr {
	p := atomic.AddUint32(&dispatchAddrPort, 1)
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: int(1000 + p%60000)}
}

// deliverToDispatch routes a packet addressed to a console's loopback address
// to its WebSocket. Reports whether addr belonged to a console. Only the
// packets the console understands are passed on.
func deliverToDispatch(data []byte, addr *net.UDPAddr) bool {
	if addr == nil || len(data) == 0 || !addr.IP.Equal(net.IPv4(127, 0, 0, 2)) {
		return false
	}
	v, ok := dispatchByAddr.Load(addr.String())
	if !ok {
		return true // a console that just left: drop, never hit the UDP socket
	}
	switch data[0] {
	case PacketTypeFloorGranted, PacketTypeFloorDenied, PacketTypeFloorPreempted,
		PacketTypeFloorIdle, PacketTypeFloorTaken, PacketTypeEmergencyAlert:
		frame := append([]byte(nil), data...)
		select {
		case v.(*DispatchConn).Send <- frame:
		default:
		}
	}
	return true
}

// dispatchAudioFrame builds the 0xA0 frame for one Opus packet.
func dispatchAudioFrame(channel int, sender, agency string, opus []byte) []byte {
	out := make([]byte, 0, 1+4+2+len(sender)+2+len(agency)+len(opus))
	out = append(out, dispatchFrameAudioOut)
	out = binary.BigEndian.AppendUint32(out, uint32(channel))
	out = binary.BigEndian.AppendUint16(out, uint16(len(sender)))
	out = append(out, sender...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(agency)))
	out = append(out, agency...)
	return append(out, opus...)
}

// fanOutToDispatch sends one Opus frame heard on agency/channel to every
// console tuned to it. A console with no agency (super_admin) hears that
// channel number in every agency. The console that sent it never hears itself.
func (s *Server) fanOutToDispatch(agencyID string, channel int, sender string, opus []byte) int {
	if len(opus) == 0 {
		return 0
	}
	var frame []byte
	sent := 0
	s.mu.RLock()
	for connKey, dc := range s.dispatchConns {
		dispClient, ok := s.clients[connKey]
		if !ok || dispClient.Username == sender || !consoleHears(dispClient, agencyID, channel) {
			continue
		}
		if frame == nil {
			frame = dispatchAudioFrame(channel, sender, agencyID, opus)
		}
		select {
		case dc.Send <- frame:
			sent++
		default: // console's send buffer is full — drop this frame
		}
	}
	s.mu.RUnlock()
	return sent
}

// dispatchTX is a console's transmit state.
type dispatchTX struct {
	mu       sync.Mutex
	keyed    bool
	frames   int
	keyedAt  time.Time
	lastLogF int
}

// dispatchPTT keys or unkeys a console. With floor control on for its agency
// the console asks for the floor like a phone (at COMMAND priority); the
// answer reaches the browser through deliverToDispatch.
func (s *Server) dispatchPTT(key string, tx *dispatchTX, down bool) {
	s.mu.RLock()
	c := s.clients[key]
	var username, agency string
	var channel int
	if c != nil {
		username, agency, channel = c.Username, c.AgencyID, c.Channel
		if agency == "" {
			agency = c.DispatchTxAgency // super admin console tuned to one agency
		}
	}
	s.mu.RUnlock()
	if c == nil {
		return
	}

	tx.mu.Lock()
	wasKeyed := tx.keyed
	tx.keyed = down
	if down && !wasKeyed {
		tx.frames = 0
		tx.keyedAt = time.Now()
	}
	frames, since := tx.frames, time.Since(tx.keyedAt)
	tx.mu.Unlock()

	if !down && wasKeyed {
		log.Printf("Dispatch %s [%s] unkeyed ch %d: %d frames in %.1fs", username, agency, channel, frames, since.Seconds())
	}

	if channel == EmergencyChannel && !c.DispatchAllCall {
		// Listen-only: nobody talks back on the EMERGENCY channel.
		if down {
			deliverToDispatch(buildFloorDenied(channel, FloorDenyNoPermission, 0, ""), c.Addr)
		}
		return
	}
	floorOn := agency != "" && s.floor != nil && s.floorEnabledForAgency(agency) && !c.DispatchAllCall
	if !floorOn {
		if down {
			// Open PTT: tell the console it may talk, as the server does for apps.
			deliverToDispatch(buildFloorGranted(channel, username, 0, 0), c.Addr)
		}
		return
	}
	fkey := channelKey(agency, channel)
	if down {
		allowed, forced := s.groupFloorRules(fkey, username, PriorityCommand)
		if !allowed {
			deliverToDispatch(buildFloorDenied(channel, FloorDenyNoPermission, 0, s.BroadcastUser(fkey)), c.Addr)
			return
		}
		pri := uint8(PriorityCommand)
		if forced != 0 {
			pri = forced
		}
		s.floor.Request(fkey, &FloorRequest{Channel: channel, User: username, Priority: pri}, pri)
	} else {
		s.floor.Release(fkey, channel, username)
	}
}

// dispatchAudioIn relays one Opus frame from a console to its channel, the way
// a phone's audio is relayed (channel members, monitors, mutual-aid partners,
// Zello bridges, other consoles). Floor rules apply when the agency uses them.
func (s *Server) dispatchAudioIn(key string, tx *dispatchTX, opus []byte) {
	tx.mu.Lock()
	keyed := tx.keyed
	if keyed {
		tx.frames++
	}
	tx.mu.Unlock()
	if !keyed || len(opus) == 0 || len(opus) > 1500 {
		return
	}

	s.mu.RLock()
	c := s.clients[key]
	var username string
	var channel int
	var agencies []string
	var allCall [][2]interface{}
	if c != nil {
		username, channel = c.Username, c.Channel
		agencies = s.consoleTxAgencies(c)
		if c.DispatchAllCall {
			allCall = s.allCallChannels(agencies)
		}
	}
	s.mu.RUnlock()
	if c == nil || (channel == EmergencyChannel && allCall == nil) {
		return
	}
	if allCall != nil {
		// All-call: every talkgroup, no floor arbitration (dispatcher override).
		pkt := &AudioPacket{Username: username, AudioData: opus, Codec: CodecOpus}
		audioData := append([]byte{PacketTypeAudioOpus}, SerializeAudioPacket(pkt)...)
		for _, t := range allCall {
			s.relayAudioData(&Client{Username: username, AgencyID: t[0].(string), Channel: t[1].(int), Addr: c.Addr}, pkt, audioData)
		}
		return
	}

	pkt := &AudioPacket{Username: username, AudioData: opus, Codec: CodecOpus}
	audioData := append([]byte{PacketTypeAudioOpus}, SerializeAudioPacket(pkt)...)

	for _, a := range agencies {
		src := &Client{Username: username, AgencyID: a, Channel: channel, Addr: c.Addr}
		if len(agencies) == 1 && s.floor != nil && s.floorEnabledForAgency(a) {
			fkey := channelKey(a, channel)
			if !s.floor.HasFloor(fkey, username) {
				s.floor.RecordDeferred(fkey, username, pkt, audioData)
				continue
			}
			s.floor.NoteAudio(fkey, username)
		}
		s.relayAudioData(src, pkt, audioData)
	}
}
