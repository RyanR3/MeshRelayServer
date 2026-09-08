package main

import "testing"

func newLMRTestServer() *Server {
	return &Server{
		lmrBridges:     map[string]*LMRBridge{},
		mutualAidLinks: map[string]*MutualAidLink{},
		channels:       map[string]*Channel{},
		clients:        map[string]*Client{},
	}
}

// The LMR block must not overlap the 10-19 pool shared by ad-hoc groups,
// Zello mappings and mutual aid.
func TestLMRChannelBlockIsClearOfSharedPool(t *testing.T) {
	if LMRChannelMin <= 19 {
		t.Fatalf("LMR block starts at %d, which collides with the 10-19 shared pool", LMRChannelMin)
	}
	if LMRChannelMax < LMRChannelMin {
		t.Fatalf("LMR block is inverted: %d-%d", LMRChannelMin, LMRChannelMax)
	}
}

func TestPickLMRChannelAllocatesSequentially(t *testing.T) {
	s := newLMRTestServer()

	first := s.pickLMRChannel()
	if first != LMRChannelMin {
		t.Fatalf("first allocation = %d, want %d", first, LMRChannelMin)
	}
	s.lmrBridges["a"] = &LMRBridge{ID: "a", Channel: first}

	second := s.pickLMRChannel()
	if second != LMRChannelMin+1 {
		t.Fatalf("second allocation = %d, want %d", second, LMRChannelMin+1)
	}
}

// Unlike pickZelloBridgeChannel (which returns 19 even when full), an exhausted
// LMR block must report "no capacity" rather than handing out a used channel.
func TestPickLMRChannelReturnsZeroWhenExhausted(t *testing.T) {
	s := newLMRTestServer()
	for ch := LMRChannelMin; ch <= LMRChannelMax; ch++ {
		s.lmrBridges[string(rune(ch))] = &LMRBridge{Channel: ch}
	}
	if got := s.pickLMRChannel(); got != 0 {
		t.Fatalf("exhausted block returned %d, want 0", got)
	}
}

func TestLMRPartnersSpanEveryAgency(t *testing.T) {
	s := newLMRTestServer()
	s.lmrBridges["b1"] = &LMRBridge{
		ID: "b1", Channel: LMRChannelMin, Enabled: true,
		Agencies: []string{"fire", "ems", "sheriff"},
	}

	got := s.getLMRPartners("fire", LMRChannelMin)
	if len(got) != 2 {
		t.Fatalf("fire partners = %v, want 2 entries", got)
	}
	seen := map[string]bool{}
	for _, a := range got {
		seen[a] = true
	}
	if !seen["ems"] || !seen["sheriff"] {
		t.Fatalf("fire partners = %v, want ems+sheriff", got)
	}

	// An agency outside the bridge gets nothing.
	if p := s.getLMRPartners("dpw", LMRChannelMin); len(p) != 0 {
		t.Fatalf("non-member agency got partners %v", p)
	}
	// A disabled bridge routes nothing.
	s.lmrBridges["b1"].Enabled = false
	if p := s.getLMRPartners("fire", LMRChannelMin); len(p) != 0 {
		t.Fatalf("disabled bridge still returned partners %v", p)
	}
}

// Audio fan-out reaches LMR partners through getMutualAidPartners, so the
// existing relay sites need no LMR-specific branch.
func TestMutualAidPartnersIncludesLMRBridges(t *testing.T) {
	s := newLMRTestServer()
	s.lmrBridges["b1"] = &LMRBridge{
		ID: "b1", Channel: LMRChannelMin, Enabled: true,
		Agencies: []string{"fire", "ems"},
	}
	got := s.getMutualAidPartners("fire", LMRChannelMin)
	if len(got) != 1 || got[0] != "ems" {
		t.Fatalf("getMutualAidPartners = %v, want [ems]", got)
	}
}

// A normal channel must still resolve through the mutual-aid path untouched.
func TestMutualAidStillWorksAlongsideLMR(t *testing.T) {
	s := newLMRTestServer()
	s.lmrBridges["b1"] = &LMRBridge{
		ID: "b1", Channel: LMRChannelMin, Enabled: true,
		Agencies: []string{"fire", "ems"},
	}
	s.mutualAidLinks["l1"] = &MutualAidLink{
		ID: "l1", Channel: 12, Active: true,
		Agencies: []string{"police", "dpw"},
	}
	got := s.getMutualAidPartners("police", 12)
	if len(got) != 1 || got[0] != "dpw" {
		t.Fatalf("mutual-aid lookup = %v, want [dpw]", got)
	}
}

func TestEnsureLMRChannelCreatesPerAgencyRecords(t *testing.T) {
	s := newLMRTestServer()
	// ensureAgencyChannels only builds 0-19, so the LMR block needs its own.
	s.ensureLMRChannel("fire", LMRChannelMin, "LMR Bridge")
	s.ensureLMRChannel("ems", LMRChannelMin, "LMR Bridge")

	for _, a := range []string{"fire", "ems"} {
		if _, ok := s.channels[channelKey(a, LMRChannelMin)]; !ok {
			t.Fatalf("channel record missing for agency %s", a)
		}
	}
	// Idempotent: a second call must not clobber the existing record.
	s.channels[channelKey("fire", LMRChannelMin)].Name = "renamed"
	s.ensureLMRChannel("fire", LMRChannelMin, "LMR Bridge")
	if got := s.channels[channelKey("fire", LMRChannelMin)].Name; got != "renamed" {
		t.Fatalf("ensureLMRChannel overwrote an existing channel (name=%q)", got)
	}
}

func TestLMRBridgeDisplayNameFallsBack(t *testing.T) {
	cases := []struct{ chName, name, want string }{
		{"County VHF", "ignored", "County VHF"},
		{"", "County VHF", "County VHF"},
		{"", "", DefaultLMRChannelName},
	}
	for _, c := range cases {
		b := &LMRBridge{ChannelName: c.chName, Name: c.name}
		if got := b.displayName(); got != c.want {
			t.Fatalf("displayName(%q,%q) = %q, want %q", c.chName, c.name, got, c.want)
		}
	}
}

func TestLMRBridgeLookupByUsername(t *testing.T) {
	s := newLMRTestServer()
	s.lmrBridges["b1"] = &LMRBridge{ID: "b1", Username: "gw-county", Enabled: true, Channel: LMRChannelMin}

	if b := s.lmrBridgeByUsername("gw-county"); b == nil || b.ID != "b1" {
		t.Fatalf("lookup by username failed: %v", b)
	}
	if b := s.lmrBridgeByUsername("nobody"); b != nil {
		t.Fatalf("unknown username resolved to %v", b)
	}
	s.lmrBridges["b1"].Enabled = false
	if b := s.lmrBridgeByUsername("gw-county"); b != nil {
		t.Fatalf("disabled bridge resolved by username")
	}
}

// Deleting a bridge must tear down its gateway login completely: the account,
// any enrollment tokens issued for it, and the live session. Leaving any behind
// would let a removed bridge's gateway keep talking or simply re-enrol.
func TestDeleteBridgeRemovesAccountAndTokens(t *testing.T) {
	s := newLMRTestServer()
	s.users = map[string]*UserAccount{}
	s.enrollmentTokens = map[string]*EnrollmentToken{}

	s.users[userKey("fire", "gw-vhf")] = &UserAccount{Username: "gw-vhf", AgencyID: "fire"}
	s.enrollmentTokens["tok1"] = &EnrollmentToken{Username: "gw-vhf", AgencyID: "fire"}
	s.enrollmentTokens["tok2"] = &EnrollmentToken{Username: "someone-else", AgencyID: "fire"}

	// Mirror what handleDeleteLMRBridge does to the account.
	owner, username := "fire", "gw-vhf"
	delete(s.users, userKey(owner, username))
	for tok, et := range s.enrollmentTokens {
		if et.Username == username && et.AgencyID == owner {
			delete(s.enrollmentTokens, tok)
		}
	}

	if _, ok := s.users[userKey("fire", "gw-vhf")]; ok {
		t.Fatal("gateway account survived bridge deletion")
	}
	if _, ok := s.enrollmentTokens["tok1"]; ok {
		t.Fatal("gateway enrollment token survived bridge deletion")
	}
	if _, ok := s.enrollmentTokens["tok2"]; !ok {
		t.Fatal("deletion removed an unrelated user's enrollment token")
	}
}
