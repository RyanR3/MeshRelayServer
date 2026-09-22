package main

import (
	"fmt"
	"testing"
)

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
	s.lmrBridges["b1"] = &LMRBridge{
		ID: "b1", Username: "gw-county", Enabled: true,
		Channel: LMRChannelMin, OwnerAgencyID: "fire", Agencies: []string{"fire"},
	}

	if b := s.lmrBridgeByUsername("fire", "gw-county"); b == nil || b.ID != "b1" {
		t.Fatalf("lookup by username failed: %v", b)
	}
	if b := s.lmrBridgeByUsername("fire", "nobody"); b != nil {
		t.Fatalf("unknown username resolved to %v", b)
	}
	// A disabled bridge still owns its gateway account, so the username stays
	// taken. This asserted the opposite until 2026-09-11; see
	// TestLMRUsernameHeldByDisabledBridge.
	s.lmrBridges["b1"].Enabled = false
	if b := s.lmrBridgeByUsername("fire", "gw-county"); b == nil {
		t.Fatalf("disabled bridge must still hold its username")
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

// pickMutualAidChannel shares the 10-19 block with ad-hoc groups and Zello
// mappings, so it must consult usedHighChannels() rather than only the
// mutual-aid links — and must never dip into 0-9, which are ordinary agency
// talkgroups.
func TestPickMutualAidChannelRespectsSharedPool(t *testing.T) {
	s := newLMRTestServer()
	s.zelloBridges = map[string]*ZelloBridge{}

	// usedHighChannels spans every server-allocated block, so a number claimed
	// anywhere is off-limits to mutual aid even though Zello now has its own
	// range.
	s.zelloBridges["z1"] = &ZelloBridge{
		AgencyID: "fire",
		Mappings: []ZelloMapping{{ZelloChannel: "dispatch", MeshChannel: SharedChannelMax, AgencyID: "fire"}},
	}
	if got := s.pickMutualAidChannel(); got == SharedChannelMax {
		t.Fatalf("handed out channel %d while a mapping occupies it", SharedChannelMax)
	}

	// Fill the whole block: the allocator must report no capacity, not fall
	// into the talkgroup range.
	for ch := SharedChannelMin; ch <= SharedChannelMax; ch++ {
		s.mutualAidLinks[fmt.Sprintf("l%d", ch)] = &MutualAidLink{Channel: ch, Active: true}
	}
	got := s.pickMutualAidChannel()
	if got != 0 {
		t.Fatalf("full block returned %d, want 0 (no capacity)", got)
	}
}

func TestPickMutualAidChannelNeverReturnsTalkgroup(t *testing.T) {
	s := newLMRTestServer()
	s.zelloBridges = map[string]*ZelloBridge{}
	for ch := SharedChannelMin; ch <= SharedChannelMax; ch++ {
		s.mutualAidLinks[fmt.Sprintf("l%d", ch)] = &MutualAidLink{Channel: ch, Active: true}
	}
	if got := s.pickMutualAidChannel(); got >= 1 && got < SharedChannelMin {
		t.Fatalf("allocated %d, which is an ordinary agency talkgroup", got)
	}
}

// The revoked status must ride in the success byte, so clients can tell a
// deliberate teardown from a rejected login without matching message text.
func TestAuthResponseRevokedStatusByte(t *testing.T) {
	cases := []struct {
		name string
		pkt  AuthResponsePacket
		want byte
	}{
		{"success", AuthResponsePacket{Success: true}, AuthStatusSuccess},
		{"failure", AuthResponsePacket{Success: false}, AuthStatusFailed},
		{"revoked", AuthResponsePacket{Success: false, Revoked: true}, AuthStatusRevoked},
		// Success wins if both are somehow set: a live session is not revoked.
		{"success beats revoked", AuthResponsePacket{Success: true, Revoked: true}, AuthStatusSuccess},
	}
	for _, c := range cases {
		b := SerializeAuthResponse(&c.pkt)
		if len(b) < 2 {
			t.Fatalf("%s: packet too short", c.name)
		}
		if b[0] != PacketTypeAuthResponse {
			t.Fatalf("%s: type = %#x, want %#x", c.name, b[0], PacketTypeAuthResponse)
		}
		if b[1] != c.want {
			t.Fatalf("%s: status byte = %#x, want %#x", c.name, b[1], c.want)
		}
	}
}

// Ownership must be explicit. Agencies[0] made it depend on slice order, so
// reordering the list moved the bridge to another agency — and orphaned the
// gateway account, which is keyed userKey(owner, username).
func TestLMROwnershipSurvivesAgencyReorder(t *testing.T) {
	b := &LMRBridge{
		ID: "b1", OwnerAgencyID: "fire", Username: "gw-vhf",
		Agencies: []string{"fire", "ems"}, Channel: LMRChannelMin,
	}
	b.Agencies[0], b.Agencies[1] = b.Agencies[1], b.Agencies[0]

	if b.ownerAgency() != "fire" {
		t.Fatalf("reordering agencies changed the owner to %q", b.ownerAgency())
	}
	if !b.ownedBy("fire") {
		t.Error("fire should still own it")
	}
	if b.ownedBy("ems") {
		t.Error("a granted agency became the owner")
	}
}

// A bridge is private: another agency sees it only once granted, and access is
// USE only — never ownership.
func TestLMRBridgeVisibility(t *testing.T) {
	b := &LMRBridge{
		ID: "b1", OwnerAgencyID: "fire", Username: "gw-vhf",
		Agencies: []string{"fire"}, Channel: LMRChannelMin,
	}
	if b.visibleTo("ems") {
		t.Error("an ungranted agency must not see the bridge")
	}
	if b.sharedWith("fire") {
		t.Error("the owner is an owner, not a share recipient")
	}

	b.Agencies = append(b.Agencies, "ems")
	if !b.sharedWith("ems") {
		t.Error("ems should now have access")
	}
	if !b.visibleTo("ems") {
		t.Error("ems should now see it")
	}
	if b.ownedBy("ems") {
		t.Error("access must never confer ownership")
	}
	if b.visibleTo("dpw") {
		t.Error("a third agency still must not see it")
	}
	if b.visibleTo("") {
		t.Error("an empty agency id must see nothing")
	}
}

// Bridges predating OwnerAgencyID fall back to the first agency, which is the
// account key they were created with.
func TestLMRLegacyBridgeFallsBackToFirstAgency(t *testing.T) {
	b := &LMRBridge{ID: "old", Username: "gw", Agencies: []string{"fire", "ems"}}
	if b.ownerAgency() != "fire" {
		t.Fatalf("legacy owner = %q, want fire", b.ownerAgency())
	}
	if !b.ownedBy("fire") {
		t.Error("legacy bridge should be owned by its first agency")
	}
	empty := &LMRBridge{ID: "x"}
	if empty.ownerAgency() != "" {
		t.Error("a bridge with no agencies has no owner")
	}
}

// A DISABLED bridge still owns its gateway account, so its username is still
// taken. Filtering the lookup on Enabled let creation past this check only to
// fail later inside AddUser with a message about users, not bridges.
func TestLMRUsernameHeldByDisabledBridge(t *testing.T) {
	s := newLMRTestServer()
	s.lmrBridges["b1"] = &LMRBridge{
		ID: "b1", Username: "gw-vhf", OwnerAgencyID: "fire",
		Agencies: []string{"fire"}, Channel: LMRChannelMin, Enabled: false,
	}
	if s.lmrBridgeByUsername("fire", "gw-vhf") == nil {
		t.Fatal("a disabled bridge must still hold its gateway username")
	}
	s.lmrBridges["b1"].Enabled = true
	if s.lmrBridgeByUsername("fire", "gw-vhf") == nil {
		t.Fatal("an enabled bridge must hold its username too")
	}
	if s.lmrBridgeByUsername("fire", "free-name") != nil {
		t.Fatal("an unused username must be free")
	}
}

// Deleting a bridge must release its username for reuse.
func TestLMRUsernameFreedAfterDelete(t *testing.T) {
	s := newLMRTestServer()
	s.lmrBridges["b1"] = &LMRBridge{
		ID: "b1", Username: "gw-vhf", OwnerAgencyID: "fire",
		Agencies: []string{"fire"}, Channel: LMRChannelMin, Enabled: true,
	}
	delete(s.lmrBridges, "b1")
	if s.lmrBridgeByUsername("fire", "gw-vhf") != nil {
		t.Fatal("the username should be free once the bridge is gone")
	}
}

// Gateway usernames are per-agency, exactly like the accounts behind them
// (keyed userKey(agency, username)). A super-admin bridge named "test" in
// default must NOT stop an agency from naming its own bridge "test" — the
// lookup used to scan every bridge server-wide and did exactly that.
func TestLMRUsernameIsSegregatedPerAgency(t *testing.T) {
	s := newLMRTestServer()
	s.lmrBridges["b1"] = &LMRBridge{
		ID: "b1", Username: "test", OwnerAgencyID: "default",
		Agencies: []string{"default"}, Channel: LMRChannelMin, Enabled: true,
	}

	if b := s.lmrBridgeByUsername("default", "test"); b == nil {
		t.Fatal("the owning agency must see its own username as taken")
	}
	if b := s.lmrBridgeByUsername("fire", "test"); b != nil {
		t.Fatalf("another agency must be free to use the same name, got %v", b.ID)
	}

	// Sharing does not transfer ownership: the account still lives in the
	// owner's agency, so a recipient may still use the name itself.
	s.lmrBridges["b1"].Agencies = []string{"default", "fire"}
	if b := s.lmrBridgeByUsername("fire", "test"); b != nil {
		t.Fatal("a sharing recipient must not inherit the username reservation")
	}
}

// Pre-ownership bridges have no OwnerAgencyID and fall back to Agencies[0],
// which is where their gateway account was created.
func TestLMRUsernameLegacyBridgeUsesFirstAgency(t *testing.T) {
	s := newLMRTestServer()
	s.lmrBridges["b1"] = &LMRBridge{
		ID: "b1", Username: "gw-legacy",
		Agencies: []string{"fire", "ems"}, Channel: LMRChannelMin, Enabled: true,
	}
	if s.lmrBridgeByUsername("fire", "gw-legacy") == nil {
		t.Fatal("legacy bridge must reserve its name under Agencies[0]")
	}
	if s.lmrBridgeByUsername("ems", "gw-legacy") != nil {
		t.Fatal("legacy bridge must not reserve the name for other agencies")
	}
}

// A gateway login is told about ITS bridge only. Announcing every bridge in
// the agency to a gateway made it adopt each in turn and re-join, and every
// join re-announced the bridges: a join storm (2026-09-22, two bridges in one
// agency). Phones still see all bridges.
func TestGatewayLoginHearsOnlyItsOwnBridge(t *testing.T) {
	s := newMARTestServer()
	s.agencies["a"] = &Agency{ID: "a", Name: "A"}
	s.lmrBridges["b1"] = &LMRBridge{ID: "b1", Name: "Pi bridge", Username: "pi", OwnerAgencyID: "a", Agencies: []string{"a"}, Channel: 100, Enabled: true}
	s.lmrBridges["b2"] = &LMRBridge{ID: "b2", Name: "Pixel Bridge", Username: "pixelgw", OwnerAgencyID: "a", Agencies: []string{"a"}, Channel: 101, Enabled: true}

	announced := func(username string) []int {
		var out []int
		var own *LMRBridge
		for _, b := range s.lmrBridges {
			if b.Username == username && b.ownerAgency() == "a" {
				own = b
			}
		}
		for _, b := range s.lmrBridges {
			if own != nil && b != own {
				continue
			}
			if b.Enabled && b.servesAgency("a") {
				out = append(out, b.Channel)
			}
		}
		return out
	}
	if got := announced("pixelgw"); len(got) != 1 || got[0] != 101 {
		t.Fatalf("gateway pixelgw should hear only ch 101, got %v", got)
	}
	if got := announced("pi"); len(got) != 1 || got[0] != 100 {
		t.Fatalf("gateway pi should hear only ch 100, got %v", got)
	}
	if got := announced("phoneuser"); len(got) != 2 {
		t.Fatalf("a phone should hear both bridges, got %v", got)
	}
}
