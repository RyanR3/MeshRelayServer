// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// Auth-response status byte values. This is a SEPARATE namespace from the
// packet types below — it is the first body byte of PacketTypeAuthResponse,
// not a packet type — so 0x02 here does not collide with PacketTypeHeartbeat.
const (
	AuthStatusFailed  byte = 0x00
	AuthStatusSuccess byte = 0x01
	// AuthStatusRevoked: the server ended an ESTABLISHED session on purpose
	// (bridge deleted, guest event ended, account removed) rather than
	// rejecting a login. Lets clients react without string-matching the
	// human-readable message. Older clients test for == 0x01 and so still
	// treat this as a failure.
	AuthStatusRevoked byte = 0x02
)

// Packet types matching Android app
const (
	PacketTypeAudio         byte = 0x00
	PacketTypeText          byte = 0x01
	PacketTypeHeartbeat     byte = 0x02
	PacketTypeJoin          byte = 0x03
	PacketTypeLeave         byte = 0x04
	PacketTypeGroupCreate   byte = 0x05
	PacketTypeGroupUpdate   byte = 0x06
	PacketTypeDirectAudio   byte = 0x07
	PacketTypeAuthRequest   byte = 0x08
	PacketTypeAuthResponse  byte = 0x09
	PacketTypeChannelSwitch byte = 0x0A

	// Phase 4 key management packet types
	PacketTypeKeyJoinRequest   byte = 0x0B
	PacketTypeKeyJoinResponse  byte = 0x0C
	PacketTypeAuthChallengeReq byte = 0x0D // Client requests challenge nonce
	PacketTypeAuthChallenge    byte = 0x0E // Server sends challenge nonce
	PacketTypeKeyAnnounce      byte = 0x0F // Wrapped key announcement for group propagation

	// Opus-compressed audio. Body layout is identical to PacketTypeAudio
	// (u16 usernameLen | username | u32 audioLen | audio) but the audio payload
	// is Opus rather than raw PCM. A distinct type keeps legacy PCM-only clients
	// safe: they hit the "unknown type" default branch and ignore the frame
	// instead of mis-parsing a flag byte and playing noise.
	PacketTypeAudioOpus byte = 0x10

	// Roster: server -> client authoritative list of connected usernames in the
	// client's agency. Lets clients show an accurate online-users list in server
	// mode (where peer heartbeats aren't broadcast). Body:
	//   u16 count | { u16 nameLen | name } * count
	PacketTypeRoster byte = 0x11

	// PacketTypeMonitorChannels: client -> server, the set of channels the client
	// wants to always LISTEN to (in addition to its active channel), so it hears
	// mutual-aid/Zello channels regardless of which channel it's tuned to talk on.
	// Body: u16 count | (i32 channel)*
	PacketTypeMonitorChannels byte = 0x12

	// PacketTypeAudioRtp: RTP-aligned audio (MCX Step 1). A standard 12-byte RTP
	// header (RFC 3550) is prepended to the normal Opus audio body, so a future
	// MCX gateway can strip the RTP header and forward Opus to an MCX media plane,
	// and the packets decode as valid RTP in Wireshark. Body layout:
	//   [12-byte RTP header][u16 usernameLen][username][i32 audioLen][Opus payload]
	// The type byte itself is the legacy-vs-RTP discriminator: peers that only
	// understand 0x00/0x10 ignore 0x13, and RTP-aware peers accept all three.
	// The payload is always Opus (RTP alignment is only defined for Opus audio).
	PacketTypeAudioRtp byte = 0x13

	// PacketTypeAudioRtpChan: channel-tagged RTP audio. Layout is a big-endian
	// int32 channel id prepended to the exact 0x13 body:
	//   [i32 channel][12-byte RTP header][u16 usernameLen][username][i32 audioLen][Opus]
	// Server -> client only, and sent ONLY to clients receiving the audio via
	// channel MONITORING (0x12 always-listen) — active-channel members keep the
	// plain 0x13 framing. Untagged monitor audio was attributed to the
	// receiver's active channel ("talking on Channel N" showed the wrong N).
	// Backward-safe: only monitoring clients ever see 0x14, and monitoring is
	// only implemented by the Android standalone app (which parses it).
	PacketTypeAudioRtpChan byte = 0x14

	// Floor control (MCX Step 3, mirrors 3GPP TS 24.380). Only used on channels
	// with floor control enabled; otherwise PTT is open as before.
	PacketTypeFloorRequest   byte = 0x20 // client -> server: I want the floor
	PacketTypeFloorGranted   byte = 0x21 // server -> client: you have the floor (or queued)
	PacketTypeFloorDenied    byte = 0x22 // server -> client: denied (busy/no perm), + queue pos
	PacketTypeFloorPreempted byte = 0x23 // server -> old holder: you were preempted
	PacketTypeFloorIdle      byte = 0x24 // server -> channel: floor is now free
	PacketTypeFloorTaken     byte = 0x25 // server -> joining client: floor status
	PacketTypeEmergencyAlert byte = 0x26 // client -> server / server -> channel: emergency

	// MCX Step 4: group info for a channel. Server -> client on join / on change so
	// the UI can show broadcast lock / emergency indicator.
	// Body: i32 channel | str groupType | str broadcastUser
	PacketTypeGroupInfo byte = 0x27

	// MCX Step 4 ad-hoc groups (invite/accept flow).
	// GroupInvite (server -> invited client): i32 channel | str groupName |
	//   str groupType | str createdBy
	PacketTypeGroupInvite byte = 0x28
	// GroupInviteResponse (client -> server): i32 channel | str user | u8 accept
	PacketTypeGroupInviteResponse byte = 0x29
	// GroupCreateAdhoc (client -> server): creates an ad-hoc group and invites
	// members. Body: str creator | str groupName | str groupType |
	//   u16 memberCount | (str member)*
	PacketTypeGroupCreateAdhoc byte = 0x2A

	// PacketTypeGroupDisband: client -> server, the creator explicitly deletes
	// an ad-hoc group. Body: [i32 channel][u16 userLen][user]. Only honored
	// when the sender is the group's creator; the server then dissolves the
	// channel for all members (ChannelSwitch active=false), same as expiry.
	PacketTypeGroupDisband byte = 0x2B
)

// Floor priority levels (higher-priority number preempts LOWER; EMERGENCY=1 wins).
const (
	PriorityEmergency = 1 // always preempts
	PriorityCommand   = 2 // supervisor / IC
	PriorityResponder = 3 // default standard user
	PriorityMonitor   = 4 // listen-only, cannot request floor
)

// FloorDenied reason codes.
const (
	FloorDenyBusy         = 0 // someone equal/higher holds the floor
	FloorDenyNoPermission = 1 // e.g. MONITOR priority, or broadcast group non-transmitter
	FloorDenyChannelFull  = 2 // queue full
)

// RTPHeader is the fixed 12-byte RTP header (RFC 3550), no CSRC/extensions.
type RTPHeader struct {
	Marker         bool   // set on the first packet of a talk burst
	PayloadType    uint8  // dynamic PT; 111 by convention for Opus
	SequenceNumber uint16 // increments per packet, wraps at 65535
	Timestamp      uint32 // +320 per 20ms frame at 16 kHz
	SSRC           uint32 // unique per transmitter session
}

// rtpVersion is always 2 for RFC 3550.
const rtpVersion = 2

// parseRTPHeader reads the fixed 12-byte RTP header from the front of data.
// Returns the header, the remaining bytes (the MeshRelay audio body), or error.
func parseRTPHeader(data []byte) (RTPHeader, []byte, error) {
	if len(data) < 12 {
		return RTPHeader{}, nil, errors.New("short RTP header")
	}
	b0 := data[0]
	if (b0 >> 6) != rtpVersion {
		return RTPHeader{}, nil, errors.New("bad RTP version")
	}
	h := RTPHeader{
		Marker:         (data[1] & 0x80) != 0,
		PayloadType:    data[1] & 0x7F,
		SequenceNumber: binary.BigEndian.Uint16(data[2:4]),
		Timestamp:      binary.BigEndian.Uint32(data[4:8]),
		SSRC:           binary.BigEndian.Uint32(data[8:12]),
	}
	return h, data[12:], nil
}

// serializeRTPHeader writes the 12-byte RTP header. CC=0, no padding/extension.
func serializeRTPHeader(h RTPHeader) []byte {
	out := make([]byte, 12)
	out[0] = byte(rtpVersion << 6) // V=2, P=0, X=0, CC=0
	out[1] = h.PayloadType & 0x7F
	if h.Marker {
		out[1] |= 0x80
	}
	binary.BigEndian.PutUint16(out[2:4], h.SequenceNumber)
	binary.BigEndian.PutUint32(out[4:8], h.Timestamp)
	binary.BigEndian.PutUint32(out[8:12], h.SSRC)
	return out
}

// Audio codec identifiers for AudioPacket.Codec.
const (
	CodecPCM  = 0 // raw PCM int16, 32kHz mono (legacy default)
	CodecOpus = 1 // Opus, 32kHz mono, 640-sample/20ms frames
)

// AudioPacket represents an audio packet. Codec indicates how AudioData is
// encoded; it is derived from the packet type on the wire (PacketTypeAudio =>
// PCM, PacketTypeAudioOpus => Opus) and is not itself serialized into the body.
type AudioPacket struct {
	Username  string
	AudioData []byte
	Codec     int
	// RTP carries the sender's RTP header when the packet arrived as
	// PacketTypeAudioRtp (0x13). It is preserved through the relay so receivers
	// see the original transmitter's SSRC / sequence / timestamp / marker. Nil
	// for legacy 0x00/0x10 packets.
	RTP *RTPHeader
}

// DirectAudioPacket represents a direct audio packet for group voice
type DirectAudioPacket struct {
	Sender    string
	Recipient string
	AudioData []byte
}

// TextMessagePacket represents a text message packet
type TextMessagePacket struct {
	Sender    string
	Recipient string
	Content   string
	Timestamp int64
}

// HeartbeatPacket represents a client heartbeat
type HeartbeatPacket struct {
	Username string
	// Flags is an optional byte after the username. Older clients do not send
	// it (and older servers ignore it). Bit 0 = HeartbeatFlagLowPower.
	Flags byte
}

// HeartbeatFlagLowPower: the client wants the low-power stream — no relayed
// heartbeats, the roster only when it changes, and a server keepalive ping it
// answers instead of a fast heartbeat timer (so the phone can sleep between
// packets). The server echoes the flag back to confirm.
const HeartbeatFlagLowPower byte = 0x01

// HeartbeatFlagEmergencyChannel: the client knows the EMERGENCY channel
// (0x37) and plays its channel-tagged audio (emergency_channel.go).
const HeartbeatFlagEmergencyChannel byte = 0x02

// JoinPacket represents a channel join request
type JoinPacket struct {
	Username string
	Channel  int
}

// AuthRequestPacket represents a client authentication request
type AuthRequestPacket struct {
	Username     string
	PasswordHash string // Legacy: SHA-256(password); Challenge-response: HMAC-SHA256(password, nonce)
	AgencyID     string // optional, empty for older clients
}

// AuthChallengeRequestPacket - client requests a challenge nonce (Phase 4f)
type AuthChallengeRequestPacket struct {
	Username string
	AgencyID string
}

// AuthChallengePacket - server sends a challenge nonce (Phase 4f)
type AuthChallengePacket struct {
	Nonce [32]byte
}

// AuthResponsePacket represents a server authentication response
type AuthResponsePacket struct {
	Success bool
	Message string
	// Revoked marks a session the SERVER ended deliberately (its bridge was
	// deleted, its guest event ended, its account removed) as opposed to a
	// login that failed. It rides in the success byte as a third value so
	// clients can react correctly without parsing the human-readable message.
	Revoked bool
}

// ChannelSwitchPacket notifies a PTT client about a mutual aid channel
type ChannelSwitchPacket struct {
	Channel     int
	ChannelName string
	Active      bool // true = add channel, false = remove channel
}

// GroupPacket represents a group create/update packet
type GroupPacket struct {
	GroupID   string
	GroupName string
	Creator   string
	Members   []string
	Timestamp int64
}

// errLengthExceedsPacket is returned when a length field claims more bytes
// than the packet actually carries.
var errLengthExceedsPacket = errors.New("length field exceeds packet size")

// readN reads exactly n bytes from r. The length comes from the packet itself,
// so it is checked against the bytes actually left BEFORE anything is
// allocated: a 7-byte packet claiming 10 MB of audio used to allocate the
// full 10 MB (pre-auth, from anyone on the internet) before failing.
func readN(r *bytes.Reader, n int) ([]byte, error) {
	if n < 0 || n > r.Len() {
		return nil, errLengthExceedsPacket
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// ParsePacket parses a raw packet and returns its type and data
func ParsePacket(data []byte) (byte, interface{}, error) {
	if len(data) < 1 {
		return 0, nil, errors.New("packet too short")
	}

	packetType := data[0]
	payload := data[1:]

	switch packetType {
	case PacketTypeAudio:
		pkt, err := ParseAudioPacket(payload)
		return packetType, pkt, err
	case PacketTypeAudioOpus:
		// Identical body to PCM audio; only the payload encoding differs.
		pkt, err := ParseAudioPacket(payload)
		if pkt != nil {
			pkt.Codec = CodecOpus
		}
		return packetType, pkt, err
	case PacketTypeAudioRtp:
		// RTP-aligned Opus audio (MCX Step 1): 12-byte RTP header + audio body.
		pkt, err := ParseAudioRtpPacket(payload)
		return packetType, pkt, err
	case PacketTypeFloorRequest:
		pkt, err := ParseFloorRequest(payload)
		return packetType, pkt, err
	case PacketTypeEmergencyAlert:
		pkt, err := ParseEmergencyAlert(payload)
		return packetType, pkt, err
	case PacketTypeGroupCreateAdhoc:
		pkt, err := ParseGroupCreateAdhoc(payload)
		return packetType, pkt, err
	case PacketTypeGroupInviteResponse:
		pkt, err := ParseGroupInviteResponse(payload)
		return packetType, pkt, err
	case PacketTypeGroupDisband:
		pkt, err := ParseGroupDisband(payload)
		return packetType, pkt, err
	case PacketTypeGuestInviteRequest:
		pkt, err := ParseGuestInviteRequest(payload)
		return packetType, pkt, err
	case PacketTypeLocationReport:
		pkt, err := ParseLocationReport(payload)
		return packetType, pkt, err
	case PacketTypeUnitStatus:
		pkt, err := ParseUnitStatus(payload)
		return packetType, pkt, err
	case PacketTypeTranscriptRequest:
		pkt, err := ParseTranscriptRequest(payload)
		return packetType, pkt, err
	case PacketTypeRecordingAudioReq:
		pkt, err := ParseRecordingAudioRequest(payload)
		return packetType, pkt, err
	case PacketTypeDispatchMessage:
		pkt, err := ParseDispatchMessage(payload)
		return packetType, pkt, err
	case PacketTypeDirectAudio:
		pkt, err := ParseDirectAudioPacket(payload)
		return packetType, pkt, err
	case PacketTypeText:
		pkt, err := ParseTextPacket(payload)
		return packetType, pkt, err
	case PacketTypeHeartbeat:
		pkt, err := ParseHeartbeatPacket(payload)
		return packetType, pkt, err
	case PacketTypeJoin:
		pkt, err := ParseJoinPacket(payload)
		return packetType, pkt, err
	case PacketTypeLeave:
		return packetType, nil, nil
	case PacketTypeGroupCreate, PacketTypeGroupUpdate:
		pkt, err := ParseGroupPacket(payload)
		return packetType, pkt, err
	case PacketTypeAuthRequest:
		pkt, err := ParseAuthRequestPacket(payload)
		return packetType, pkt, err
	case PacketTypeAuthChallengeReq:
		pkt, err := ParseAuthChallengeRequestPacket(payload)
		return packetType, pkt, err
	case PacketTypeKeyJoinRequest, PacketTypeKeyJoinResponse, PacketTypeKeyAnnounce:
		// Pass through — relayed to channel peers
		return packetType, payload, nil
	case PacketTypeMonitorChannels:
		pkt, err := ParseMonitorChannelsPacket(payload)
		return packetType, pkt, err
	default:
		return packetType, nil, errors.New("unknown packet type")
	}
}

// MonitorChannelsPacket carries the channels a client wants to always listen to.
type MonitorChannelsPacket struct {
	Channels []int
}

// ParseMonitorChannelsPacket: u16 count | (i32 channel)*
func ParseMonitorChannelsPacket(data []byte) (*MonitorChannelsPacket, error) {
	buf := bytes.NewReader(data)
	var count uint16
	if err := binary.Read(buf, binary.BigEndian, &count); err != nil {
		return nil, err
	}
	if count > 64 { // sanity cap
		return nil, errors.New("too many monitor channels")
	}
	chs := make([]int, 0, count)
	for i := 0; i < int(count); i++ {
		var ch int32
		if err := binary.Read(buf, binary.BigEndian, &ch); err != nil {
			return nil, err
		}
		chs = append(chs, int(ch))
	}
	return &MonitorChannelsPacket{Channels: chs}, nil
}

// ParseAudioPacket parses an audio packet (matches Android AudioPacket.fromBytes)
func ParseAudioPacket(data []byte) (*AudioPacket, error) {
	buf := bytes.NewReader(data)

	// Read username length
	var usernameLen int16
	if err := binary.Read(buf, binary.BigEndian, &usernameLen); err != nil {
		return nil, err
	}

	// Validate username length
	if usernameLen < 0 || usernameLen > 256 {
		return nil, errors.New("invalid username length")
	}

	// Read username
	usernameBytes, err := readN(buf, int(usernameLen))
	if err != nil {
		return nil, err
	}

	// Read audio data length
	var audioLen int32
	if err := binary.Read(buf, binary.BigEndian, &audioLen); err != nil {
		return nil, err
	}

	// Validate audio length
	if audioLen < 0 || audioLen > 10*1024*1024 {
		return nil, errors.New("invalid audio length")
	}

	// Read audio data
	audioData, err := readN(buf, int(audioLen))
	if err != nil {
		return nil, err
	}

	return &AudioPacket{
		Username:  string(usernameBytes),
		AudioData: audioData,
	}, nil
}

// ParseDirectAudioPacket parses a direct audio packet for group voice
func ParseDirectAudioPacket(data []byte) (*DirectAudioPacket, error) {
	buf := bytes.NewReader(data)

	// Read sender length
	var senderLen int16
	if err := binary.Read(buf, binary.BigEndian, &senderLen); err != nil {
		return nil, err
	}

	// Validate sender length
	if senderLen < 0 || senderLen > 256 {
		return nil, errors.New("invalid sender length")
	}

	// Read sender
	senderBytes, err := readN(buf, int(senderLen))
	if err != nil {
		return nil, err
	}

	// Read recipient length
	var recipientLen int16
	if err := binary.Read(buf, binary.BigEndian, &recipientLen); err != nil {
		return nil, err
	}

	// Validate recipient length
	if recipientLen < 0 || recipientLen > 256 {
		return nil, errors.New("invalid recipient length")
	}

	// Read recipient
	recipientBytes, err := readN(buf, int(recipientLen))
	if err != nil {
		return nil, err
	}

	// Read audio data length
	var audioLen int32
	if err := binary.Read(buf, binary.BigEndian, &audioLen); err != nil {
		return nil, err
	}

	// Validate audio length
	if audioLen < 0 || audioLen > 10*1024*1024 {
		return nil, errors.New("invalid audio length")
	}

	// Read audio data
	audioData, err := readN(buf, int(audioLen))
	if err != nil {
		return nil, err
	}

	return &DirectAudioPacket{
		Sender:    string(senderBytes),
		Recipient: string(recipientBytes),
		AudioData: audioData,
	}, nil
}

// ParseTextPacket parses a text message packet (matches Android TextMessagePacket.fromBytes)
func ParseTextPacket(data []byte) (*TextMessagePacket, error) {
	buf := bytes.NewReader(data)

	// Read sender
	var senderLen int16
	if err := binary.Read(buf, binary.BigEndian, &senderLen); err != nil {
		return nil, err
	}

	if senderLen < 0 || senderLen > 256 {
		return nil, errors.New("invalid sender length")
	}

	senderBytes, err := readN(buf, int(senderLen))
	if err != nil {
		return nil, err
	}

	// Read recipient
	var recipientLen int16
	if err := binary.Read(buf, binary.BigEndian, &recipientLen); err != nil {
		return nil, err
	}

	if recipientLen < 0 || recipientLen > 256 {
		return nil, errors.New("invalid recipient length")
	}

	recipientBytes, err := readN(buf, int(recipientLen))
	if err != nil {
		return nil, err
	}

	// Read content
	var contentLen int16
	if err := binary.Read(buf, binary.BigEndian, &contentLen); err != nil {
		return nil, err
	}

	if contentLen < 0 || contentLen > 10000 {
		return nil, errors.New("invalid content length")
	}

	contentBytes, err := readN(buf, int(contentLen))
	if err != nil {
		return nil, err
	}

	// Read timestamp
	var timestamp int64
	if err := binary.Read(buf, binary.BigEndian, &timestamp); err != nil {
		return nil, err
	}

	return &TextMessagePacket{
		Sender:    string(senderBytes),
		Recipient: string(recipientBytes),
		Content:   string(contentBytes),
		Timestamp: timestamp,
	}, nil
}

// ParseHeartbeatPacket parses a heartbeat packet
func ParseHeartbeatPacket(data []byte) (*HeartbeatPacket, error) {
	buf := bytes.NewReader(data)

	// Read username length
	var usernameLen int16
	if err := binary.Read(buf, binary.BigEndian, &usernameLen); err != nil {
		return nil, err
	}

	if usernameLen < 0 || usernameLen > 256 {
		return nil, errors.New("invalid username length")
	}

	// Read username
	usernameBytes, err := readN(buf, int(usernameLen))
	if err != nil {
		return nil, err
	}

	hb := &HeartbeatPacket{Username: string(usernameBytes)}
	if f, err := buf.ReadByte(); err == nil {
		hb.Flags = f
	}
	return hb, nil
}

// ParseJoinPacket parses a channel join packet
func ParseJoinPacket(data []byte) (*JoinPacket, error) {
	buf := bytes.NewReader(data)

	// Read username length
	var usernameLen int16
	if err := binary.Read(buf, binary.BigEndian, &usernameLen); err != nil {
		return nil, err
	}

	if usernameLen < 0 || usernameLen > 256 {
		return nil, errors.New("invalid username length")
	}

	// Read username
	usernameBytes, err := readN(buf, int(usernameLen))
	if err != nil {
		return nil, err
	}

	// Read channel ID
	var channel int32
	if err := binary.Read(buf, binary.BigEndian, &channel); err != nil {
		return nil, err
	}

	return &JoinPacket{
		Username: string(usernameBytes),
		Channel:  int(channel),
	}, nil
}

// SerializeAudioPacket serializes an audio packet for transmission
func SerializeAudioPacket(packet *AudioPacket) []byte {
	buf := new(bytes.Buffer)

	// Write username length
	binary.Write(buf, binary.BigEndian, int16(len(packet.Username)))
	// Write username
	buf.WriteString(packet.Username)
	// Write audio length
	binary.Write(buf, binary.BigEndian, int32(len(packet.AudioData)))
	// Write audio data
	buf.Write(packet.AudioData)

	return buf.Bytes()
}

// ParseAudioRtpPacket parses a PacketTypeAudioRtp (0x13) body: a 12-byte RTP
// header followed by the standard [u16 userLen|user|i32 audioLen|audio] body.
// Returns an AudioPacket with RTP set and Codec=Opus.
func ParseAudioRtpPacket(data []byte) (*AudioPacket, error) {
	hdr, rest, err := parseRTPHeader(data)
	if err != nil {
		return nil, err
	}
	pkt, err := ParseAudioPacket(rest)
	if err != nil {
		return nil, err
	}
	pkt.Codec = CodecOpus // RTP-aligned audio is always Opus
	pkt.RTP = &hdr
	return pkt, nil
}

// SerializeAudioRtpPacket writes [12-byte RTP header][audio body]. If the packet
// has no RTP header (shouldn't happen on this path) a zero header is written.
func SerializeAudioRtpPacket(packet *AudioPacket) []byte {
	var hdr RTPHeader
	if packet.RTP != nil {
		hdr = *packet.RTP
	}
	return append(serializeRTPHeader(hdr), SerializeAudioPacket(packet)...)
}

// SerializeTextPacket serializes a text packet for transmission
// SerializeRosterPacket builds a roster packet: the authoritative list of
// connected usernames for an agency. Wire body: u16 count | (u16 len | name)*.
func SerializeRosterPacket(usernames []string) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeRoster)
	binary.Write(buf, binary.BigEndian, int16(len(usernames)))
	for _, name := range usernames {
		nb := []byte(name)
		binary.Write(buf, binary.BigEndian, int16(len(nb)))
		buf.Write(nb)
	}
	return buf.Bytes()
}

func SerializeTextPacket(packet *TextMessagePacket) []byte {
	buf := new(bytes.Buffer)

	// Write packet type
	buf.WriteByte(PacketTypeText)
	// Write sender length
	binary.Write(buf, binary.BigEndian, int16(len(packet.Sender)))
	// Write sender
	buf.WriteString(packet.Sender)
	// Write recipient length
	binary.Write(buf, binary.BigEndian, int16(len(packet.Recipient)))
	// Write recipient
	buf.WriteString(packet.Recipient)
	// Write content length
	binary.Write(buf, binary.BigEndian, int16(len(packet.Content)))
	// Write content
	buf.WriteString(packet.Content)
	// Write timestamp
	binary.Write(buf, binary.BigEndian, packet.Timestamp)

	return buf.Bytes()
}

// CreateHeartbeatPacket creates a heartbeat packet for transmission
func CreateHeartbeatPacket(username string) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write packet type
	buf.WriteByte(PacketTypeHeartbeat)
	// Write username length
	binary.Write(buf, binary.BigEndian, int16(len(username)))
	// Write username
	buf.WriteString(username)

	return buf.Bytes(), nil
}

// CreateHeartbeatPacketFlags is CreateHeartbeatPacket with the trailing flags
// byte, sent only to clients that sent the flag themselves.
func CreateHeartbeatPacketFlags(username string, flags byte) []byte {
	b, _ := CreateHeartbeatPacket(username)
	return append(b, flags)
}

// CreateDirectAudioPacket creates a direct audio packet for group voice
func CreateDirectAudioPacket(packet *DirectAudioPacket) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write packet type
	buf.WriteByte(PacketTypeDirectAudio)
	// Write sender length
	binary.Write(buf, binary.BigEndian, int16(len(packet.Sender)))
	// Write sender
	buf.WriteString(packet.Sender)
	// Write recipient length
	binary.Write(buf, binary.BigEndian, int16(len(packet.Recipient)))
	// Write recipient
	buf.WriteString(packet.Recipient)
	// Write audio data length
	binary.Write(buf, binary.BigEndian, int32(len(packet.AudioData)))
	// Write audio data
	buf.Write(packet.AudioData)

	return buf.Bytes(), nil
}

// ParseGroupPacket parses a group create/update packet
func ParseGroupPacket(data []byte) (*GroupPacket, error) {
	buf := bytes.NewReader(data)

	// Read group ID length
	var groupIDLen int16
	if err := binary.Read(buf, binary.BigEndian, &groupIDLen); err != nil {
		return nil, err
	}
	if groupIDLen < 0 || groupIDLen > 512 {
		return nil, errors.New("invalid group ID length")
	}
	groupIDBytes, err := readN(buf, int(groupIDLen))
	if err != nil {
		return nil, err
	}

	// Read group name length
	var groupNameLen int16
	if err := binary.Read(buf, binary.BigEndian, &groupNameLen); err != nil {
		return nil, err
	}
	if groupNameLen < 0 || groupNameLen > 256 {
		return nil, errors.New("invalid group name length")
	}
	groupNameBytes, err := readN(buf, int(groupNameLen))
	if err != nil {
		return nil, err
	}

	// Read creator length
	var creatorLen int16
	if err := binary.Read(buf, binary.BigEndian, &creatorLen); err != nil {
		return nil, err
	}
	if creatorLen < 0 || creatorLen > 256 {
		return nil, errors.New("invalid creator length")
	}
	creatorBytes, err := readN(buf, int(creatorLen))
	if err != nil {
		return nil, err
	}

	// Read member count
	var memberCount int16
	if err := binary.Read(buf, binary.BigEndian, &memberCount); err != nil {
		return nil, err
	}
	if memberCount < 0 || memberCount > 100 {
		return nil, errors.New("invalid member count")
	}

	// Read members
	members := make([]string, memberCount)
	for i := int16(0); i < memberCount; i++ {
		var memberLen int16
		if err := binary.Read(buf, binary.BigEndian, &memberLen); err != nil {
			return nil, err
		}
		if memberLen < 0 || memberLen > 256 {
			return nil, errors.New("invalid member length")
		}
		memberBytes, err := readN(buf, int(memberLen))
		if err != nil {
			return nil, err
		}
		members[i] = string(memberBytes)
	}

	// Read timestamp
	var timestamp int64
	if err := binary.Read(buf, binary.BigEndian, &timestamp); err != nil {
		return nil, err
	}

	return &GroupPacket{
		GroupID:   string(groupIDBytes),
		GroupName: string(groupNameBytes),
		Creator:   string(creatorBytes),
		Members:   members,
		Timestamp: timestamp,
	}, nil
}

// CreateGroupPacket creates a group packet for transmission
func CreateGroupPacket(packetType byte, packet *GroupPacket) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write packet type
	buf.WriteByte(packetType)
	// Write group ID length
	binary.Write(buf, binary.BigEndian, int16(len(packet.GroupID)))
	// Write group ID
	buf.WriteString(packet.GroupID)
	// Write group name length
	binary.Write(buf, binary.BigEndian, int16(len(packet.GroupName)))
	// Write group name
	buf.WriteString(packet.GroupName)
	// Write creator length
	binary.Write(buf, binary.BigEndian, int16(len(packet.Creator)))
	// Write creator
	buf.WriteString(packet.Creator)
	// Write member count
	binary.Write(buf, binary.BigEndian, int16(len(packet.Members)))
	// Write members
	for _, member := range packet.Members {
		binary.Write(buf, binary.BigEndian, int16(len(member)))
		buf.WriteString(member)
	}
	// Write timestamp
	binary.Write(buf, binary.BigEndian, packet.Timestamp)

	return buf.Bytes(), nil
}

// ParseAuthRequestPacket parses an auth request packet
func ParseAuthRequestPacket(data []byte) (*AuthRequestPacket, error) {
	buf := bytes.NewReader(data)

	// Read username length
	var usernameLen int16
	if err := binary.Read(buf, binary.BigEndian, &usernameLen); err != nil {
		return nil, err
	}
	if usernameLen < 0 || usernameLen > 256 {
		return nil, errors.New("invalid username length")
	}
	usernameBytes, err := readN(buf, int(usernameLen))
	if err != nil {
		return nil, err
	}

	// Read password hash length
	var hashLen int16
	if err := binary.Read(buf, binary.BigEndian, &hashLen); err != nil {
		return nil, err
	}
	if hashLen < 0 || hashLen > 256 {
		return nil, errors.New("invalid password hash length")
	}
	hashBytes, err := readN(buf, int(hashLen))
	if err != nil {
		return nil, err
	}

	// Optionally read agency ID (new clients send it, old clients don't)
	var agencyID string
	if buf.Len() >= 2 {
		var agencyLen int16
		if err := binary.Read(buf, binary.BigEndian, &agencyLen); err == nil && agencyLen > 0 && agencyLen <= 256 {
			if agencyBytes, err := readN(buf, int(agencyLen)); err == nil {
				agencyID = string(agencyBytes)
			}
		}
	}

	return &AuthRequestPacket{
		Username:     string(usernameBytes),
		PasswordHash: string(hashBytes),
		AgencyID:     agencyID,
	}, nil
}

// SerializeChannelSwitch serializes a channel switch packet for transmission
func SerializeChannelSwitch(packet *ChannelSwitchPacket) []byte {
	buf := new(bytes.Buffer)

	// Write packet type
	buf.WriteByte(PacketTypeChannelSwitch)
	// Write active flag (1 = add, 0 = remove)
	if packet.Active {
		buf.WriteByte(0x01)
	} else {
		buf.WriteByte(0x00)
	}
	// Write channel number
	binary.Write(buf, binary.BigEndian, int32(packet.Channel))
	// Write channel name length
	binary.Write(buf, binary.BigEndian, int16(len(packet.ChannelName)))
	// Write channel name
	buf.WriteString(packet.ChannelName)

	return buf.Bytes()
}

// SerializeAuthResponse serializes an auth response for transmission
func SerializeAuthResponse(packet *AuthResponsePacket) []byte {
	buf := new(bytes.Buffer)

	// Write packet type
	buf.WriteByte(PacketTypeAuthResponse)

	// Write success byte. 0x02 means "session revoked by the server" — a
	// deliberate teardown rather than a rejected login. Older clients test for
	// == 0x01, so they read 0x02 as a plain failure and still disconnect;
	// nothing breaks by adding it.
	switch {
	case packet.Success:
		buf.WriteByte(0x01)
	case packet.Revoked:
		buf.WriteByte(AuthStatusRevoked)
	default:
		buf.WriteByte(0x00)
	}

	// Write message
	msgBytes := []byte(packet.Message)
	binary.Write(buf, binary.BigEndian, int16(len(msgBytes)))
	buf.Write(msgBytes)

	return buf.Bytes()
}

// ParseAuthChallengeRequestPacket parses a challenge request from the client
func ParseAuthChallengeRequestPacket(data []byte) (*AuthChallengeRequestPacket, error) {
	buf := bytes.NewReader(data)

	// Username
	var usernameLen int16
	if err := binary.Read(buf, binary.BigEndian, &usernameLen); err != nil {
		return nil, err
	}
	if usernameLen <= 0 || usernameLen > 256 {
		return nil, errors.New("invalid username length")
	}
	usernameBytes, err := readN(buf, int(usernameLen))
	if err != nil {
		return nil, err
	}

	// AgencyID (optional — remaining bytes)
	var agencyID string
	if buf.Len() >= 2 {
		var agencyLen int16
		if err := binary.Read(buf, binary.BigEndian, &agencyLen); err == nil && agencyLen > 0 && agencyLen <= 256 {
			if agencyBytes, err := readN(buf, int(agencyLen)); err == nil {
				agencyID = string(agencyBytes)
			}
		}
	}

	return &AuthChallengeRequestPacket{
		Username: string(usernameBytes),
		AgencyID: agencyID,
	}, nil
}

// SerializeAuthChallenge serializes a challenge nonce for the client
func SerializeAuthChallenge(nonce [32]byte) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(PacketTypeAuthChallenge)
	buf.Write(nonce[:])
	return buf.Bytes()
}
