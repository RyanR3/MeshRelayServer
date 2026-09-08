package main

// LMR (land-mobile-radio) bridges — VHF/UHF/P25 interop.
//
// A MeshRelay Gateway box sits next to an LMR radio and logs in with the
// bridge's OWN gateway account. Everything the gateway carries lands on one
// dedicated channel, which the server announces to every agency in the bridge
// as a named channel ("LMR Bridge"), exactly the way a mutual-aid patch is
// announced. Any agency dragged into the bridge hears the radio and can key it.
//
// Two things distinguish this from the Zello bridge:
//
//   - Channel numbers come from their own block (100-119), NOT the 10-19 pool
//     shared by ad-hoc groups, Zello mappings and mutual-aid links. That pool is
//     already three-ways contended, so LMR stays clear of it entirely.
//   - The bridge owns its gateway login. One row is the whole configuration:
//     credentials, channel, and the agency list.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"time"
)

// LMR bridges get their own channel block, deliberately clear of the 10-19
// range shared by ad-hoc groups, Zello mappings and mutual aid.
const (
	LMRChannelMin = 100
	LMRChannelMax = 119
)

// DefaultLMRChannelName is what clients show for a bridge that has no name set.
const DefaultLMRChannelName = "LMR Bridge"

// LMRBridge is one radio gateway: a login, a channel, and the agencies that
// share it.
type LMRBridge struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Gateway login. The MeshRelay Gateway app authenticates with these. The
	// account is a real UserAccount (created alongside the bridge) so the
	// existing challenge-response auth path works unchanged; Username is kept
	// here as the link between bridge and account.
	Username string `json:"username"`

	// Agencies that can see and use this bridge. Order is not significant; the
	// first entry is treated as the owning agency for the gateway's own client
	// record (it has to belong to exactly one for channel bookkeeping).
	Agencies []string `json:"agencies"`

	// Channel is allocated from the LMR block and is stable for the bridge's
	// life. ChannelName is what clients display.
	Channel     int    `json:"channel"`
	ChannelName string `json:"channel_name"`

	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`
}

// ownerAgency is the agency the gateway's own client record belongs to.
func (b *LMRBridge) ownerAgency() string {
	if len(b.Agencies) == 0 {
		return ""
	}
	return b.Agencies[0]
}

// servesAgency reports whether this bridge reaches the given agency.
func (b *LMRBridge) servesAgency(agencyID string) bool {
	for _, a := range b.Agencies {
		if a == agencyID {
			return true
		}
	}
	return false
}

// displayName is the channel label pushed to clients.
func (b *LMRBridge) displayName() string {
	if b.ChannelName != "" {
		return b.ChannelName
	}
	if b.Name != "" {
		return b.Name
	}
	return DefaultLMRChannelName
}

// --- persistence -----------------------------------------------------------

func (s *Server) lmrBridgesFile() string {
	if s.config != nil && s.config.LMRBridgeFile != "" {
		return s.config.LMRBridgeFile
	}
	return "lmr_bridges.json"
}

// loadLMRBridges reads bridges from disk. A missing file is not an error.
func (s *Server) loadLMRBridges() {
	path := s.lmrBridgesFile()
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("LMR: failed to read %s: %v", path, err)
		}
		return
	}
	var list []*LMRBridge
	if err := json.Unmarshal(data, &list); err != nil {
		log.Printf("LMR: failed to parse %s: %v", path, err)
		return
	}
	for _, b := range list {
		if b.ID == "" {
			continue
		}
		if b.Channel < LMRChannelMin || b.Channel > LMRChannelMax {
			log.Printf("LMR: bridge %s has out-of-range channel %d, skipping", b.ID, b.Channel)
			continue
		}
		s.lmrBridges[b.ID] = b
		// The bridge's channel must exist in every agency it serves, or audio
		// has nowhere to land.
		for _, a := range b.Agencies {
			s.ensureLMRChannel(a, b.Channel, b.displayName())
		}
	}
	log.Printf("LMR: loaded %d bridge(s)", len(s.lmrBridges))
}

// saveLMRBridges writes bridges to disk. Caller must NOT hold s.mu.
func (s *Server) saveLMRBridges() error {
	s.mu.RLock()
	list := make([]*LMRBridge, 0, len(s.lmrBridges))
	for _, b := range s.lmrBridges {
		list = append(list, b)
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.lmrBridgesFile(), data, 0600)
}

// --- channel allocation ----------------------------------------------------

// usedLMRChannels returns the LMR-block channels already taken.
// Caller must hold s.mu.
func (s *Server) usedLMRChannels() map[int]bool {
	used := make(map[int]bool, len(s.lmrBridges))
	for _, b := range s.lmrBridges {
		used[b.Channel] = true
	}
	return used
}

// pickLMRChannel returns a free channel from the LMR block, or 0 if the block
// is exhausted (callers must treat 0 as "no capacity" — unlike the Zello
// allocator, this does not silently reuse the last slot).
// Caller must hold s.mu.
func (s *Server) pickLMRChannel() int {
	used := s.usedLMRChannels()
	for ch := LMRChannelMin; ch <= LMRChannelMax; ch++ {
		if !used[ch] {
			return ch
		}
	}
	return 0
}

// ensureLMRChannel creates the agency-scoped channel record for a bridge.
// ensureAgencyChannels only builds 0-19, so the LMR block needs this.
// Caller must hold s.mu.
func (s *Server) ensureLMRChannel(agencyID string, channel int, name string) {
	key := channelKey(agencyID, channel)
	if _, exists := s.channels[key]; exists {
		return
	}
	s.channels[key] = &Channel{
		ID:      channel,
		Name:    name,
		Clients: make(map[string]*Client),
	}
}

// --- lookup ----------------------------------------------------------------

// lmrBridgeForChannel returns the enabled bridge owning a channel, or nil.
// Caller must hold s.mu (read is enough).
func (s *Server) lmrBridgeForChannel(channel int) *LMRBridge {
	if channel < LMRChannelMin || channel > LMRChannelMax {
		return nil
	}
	for _, b := range s.lmrBridges {
		if b.Enabled && b.Channel == channel {
			return b
		}
	}
	return nil
}

// lmrBridgeByUsername returns the enabled bridge whose gateway account matches.
// Caller must hold s.mu (read is enough).
func (s *Server) lmrBridgeByUsername(username string) *LMRBridge {
	if username == "" {
		return nil
	}
	for _, b := range s.lmrBridges {
		if b.Enabled && b.Username == username {
			return b
		}
	}
	return nil
}

// getLMRPartners returns the OTHER agencies sharing a bridge channel, mirroring
// getMutualAidPartners so audio fan-out can reuse the same shape.
// Caller must hold s.mu (read is enough).
func (s *Server) getLMRPartners(agencyID string, channel int) []string {
	b := s.lmrBridgeForChannel(channel)
	if b == nil || !b.servesAgency(agencyID) {
		return nil
	}
	var partners []string
	for _, a := range b.Agencies {
		if a != agencyID {
			partners = append(partners, a)
		}
	}
	return partners
}

// --- client announcements --------------------------------------------------

// notifyLMRChannel announces (or withdraws) a bridge's channel to every client
// in its agencies. Same shape as notifyMutualAidChannel: collect under the read
// lock, send outside it.
func (s *Server) notifyLMRChannel(agencies []string, channel int, name string, active bool) {
	pkt := SerializeChannelSwitch(&ChannelSwitchPacket{
		Channel:     channel,
		ChannelName: name,
		Active:      active,
	})

	set := make(map[string]bool, len(agencies))
	for _, a := range agencies {
		set[a] = true
	}

	var addrs []*net.UDPAddr
	s.mu.RLock()
	for _, c := range s.clients {
		if set[c.AgencyID] {
			addrs = append(addrs, c.Addr)
		}
	}
	s.mu.RUnlock()

	for _, a := range addrs {
		s.sendPacket(pkt, a)
	}
	verb := "announced"
	if !active {
		verb = "withdrew"
	}
	log.Printf("LMR: %s channel %d (%s) to %d client(s) across %v", verb, channel, name, len(addrs), agencies)
}

// newLMRBridgeID mints a random bridge id.
func newLMRBridgeID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// nowRFC3339 is a small helper so handlers read cleanly.
func nowRFC3339() string { return time.Now().Format(time.RFC3339) }

// describeLMRBridge is used in logs and API responses.
func describeLMRBridge(b *LMRBridge) string {
	return fmt.Sprintf("%s (ch %d, %d agencies)", b.displayName(), b.Channel, len(b.Agencies))
}
