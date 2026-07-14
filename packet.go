package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// Packet types matching Android app
const (
	PacketTypeAudio       byte = 0x00
	PacketTypeText        byte = 0x01
	PacketTypeHeartbeat   byte = 0x02
	PacketTypeJoin        byte = 0x03
	PacketTypeLeave       byte = 0x04
	PacketTypeGroupCreate byte = 0x05
	PacketTypeGroupUpdate byte = 0x06
	PacketTypeDirectAudio byte = 0x07
	PacketTypeAuthRequest   byte = 0x08
	PacketTypeAuthResponse  byte = 0x09
	PacketTypeChannelSwitch byte = 0x0A

	// Phase 4 key management packet types
	PacketTypeKeyJoinRequest  byte = 0x0B
	PacketTypeKeyJoinResponse byte = 0x0C
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
)

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
}

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
	usernameBytes := make([]byte, usernameLen)
	if _, err := io.ReadFull(buf, usernameBytes); err != nil {
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
	audioData := make([]byte, audioLen)
	if _, err := io.ReadFull(buf, audioData); err != nil {
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
	senderBytes := make([]byte, senderLen)
	if _, err := io.ReadFull(buf, senderBytes); err != nil {
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
	recipientBytes := make([]byte, recipientLen)
	if _, err := io.ReadFull(buf, recipientBytes); err != nil {
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
	audioData := make([]byte, audioLen)
	if _, err := io.ReadFull(buf, audioData); err != nil {
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

	senderBytes := make([]byte, senderLen)
	if _, err := io.ReadFull(buf, senderBytes); err != nil {
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

	recipientBytes := make([]byte, recipientLen)
	if _, err := io.ReadFull(buf, recipientBytes); err != nil {
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

	contentBytes := make([]byte, contentLen)
	if _, err := io.ReadFull(buf, contentBytes); err != nil {
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
	usernameBytes := make([]byte, usernameLen)
	if _, err := io.ReadFull(buf, usernameBytes); err != nil {
		return nil, err
	}

	return &HeartbeatPacket{
		Username: string(usernameBytes),
	}, nil
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
	usernameBytes := make([]byte, usernameLen)
	if _, err := io.ReadFull(buf, usernameBytes); err != nil {
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
	groupIDBytes := make([]byte, groupIDLen)
	if _, err := io.ReadFull(buf, groupIDBytes); err != nil {
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
	groupNameBytes := make([]byte, groupNameLen)
	if _, err := io.ReadFull(buf, groupNameBytes); err != nil {
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
	creatorBytes := make([]byte, creatorLen)
	if _, err := io.ReadFull(buf, creatorBytes); err != nil {
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
		memberBytes := make([]byte, memberLen)
		if _, err := io.ReadFull(buf, memberBytes); err != nil {
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
	usernameBytes := make([]byte, usernameLen)
	if _, err := io.ReadFull(buf, usernameBytes); err != nil {
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
	hashBytes := make([]byte, hashLen)
	if _, err := io.ReadFull(buf, hashBytes); err != nil {
		return nil, err
	}

	// Optionally read agency ID (new clients send it, old clients don't)
	var agencyID string
	if buf.Len() >= 2 {
		var agencyLen int16
		if err := binary.Read(buf, binary.BigEndian, &agencyLen); err == nil && agencyLen > 0 && agencyLen <= 256 {
			agencyBytes := make([]byte, agencyLen)
			if _, err := io.ReadFull(buf, agencyBytes); err == nil {
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

	// Write success byte
	if packet.Success {
		buf.WriteByte(0x01)
	} else {
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
	usernameBytes := make([]byte, usernameLen)
	if _, err := io.ReadFull(buf, usernameBytes); err != nil {
		return nil, err
	}

	// AgencyID (optional — remaining bytes)
	var agencyID string
	if buf.Len() >= 2 {
		var agencyLen int16
		if err := binary.Read(buf, binary.BigEndian, &agencyLen); err == nil && agencyLen > 0 && agencyLen <= 256 {
			agencyBytes := make([]byte, agencyLen)
			if _, err := io.ReadFull(buf, agencyBytes); err == nil {
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
