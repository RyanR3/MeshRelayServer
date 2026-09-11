package main

import "testing"

func newMARTestServer() *Server {
	return &Server{
		agencies:          map[string]*Agency{},
		mutualAidLinks:    map[string]*MutualAidLink{},
		mutualAidRequests: map[string]*MutualAidRequest{},
		zelloBridges:      map[string]*ZelloBridge{},
		lmrBridges:        map[string]*LMRBridge{},
		channels:          map[string]*Channel{},
		clients:           map[string]*Client{},
	}
}

// A standing agreement is one-directional: it governs what an agency ACCEPTS.
// Fire trusting Sheriff must not let Fire's request to Sheriff skip approval —
// only Sheriff trusting Fire does that.
func TestAutoAcceptIsReceiverSideOnly(t *testing.T) {
	s := newMARTestServer()
	s.agencies["fire"] = &Agency{ID: "fire", Discoverable: true,
		AutoAcceptFrom: []string{"sheriff"}}
	s.agencies["sheriff"] = &Agency{ID: "sheriff", Discoverable: true}

	// Sheriff -> Fire: Fire trusts Sheriff, so Fire auto-accepts.
	if !s.agencies["fire"].trustsAgency("sheriff") {
		t.Fatal("fire should auto-accept sheriff")
	}
	// Fire -> Sheriff: Sheriff has trusted nobody, so this needs approval.
	if s.agencies["sheriff"].trustsAgency("fire") {
		t.Fatal("sheriff must NOT auto-accept fire — it never trusted them")
	}
}

func TestTrustsAgencyIgnoresUnknown(t *testing.T) {
	a := &Agency{ID: "fire", AutoAcceptFrom: []string{"ems", "sheriff"}}
	for _, id := range []string{"ems", "sheriff"} {
		if !a.trustsAgency(id) {
			t.Fatalf("expected %s to be trusted", id)
		}
	}
	if a.trustsAgency("dpw") {
		t.Fatal("dpw is not on the list and must not be trusted")
	}
	if (&Agency{ID: "x"}).trustsAgency("anyone") {
		t.Fatal("an empty trust list must trust nobody")
	}
}

// Only one pending request may exist between two agencies, in either
// direction, so both sides cannot stack duplicates while one is open.
func TestPendingRequestBetweenIsDirectionAgnostic(t *testing.T) {
	s := newMARTestServer()
	s.mutualAidRequests["r1"] = &MutualAidRequest{
		ID: "r1", FromAgency: "fire", ToAgency: "ems", Status: MARequestPending,
	}
	if s.pendingRequestBetween("fire", "ems") == nil {
		t.Fatal("should find the request in the sent direction")
	}
	if s.pendingRequestBetween("ems", "fire") == nil {
		t.Fatal("should find the same request in the reverse direction")
	}
	if s.pendingRequestBetween("fire", "dpw") != nil {
		t.Fatal("unrelated agencies must not match")
	}

	// A resolved request no longer blocks a new one.
	s.mutualAidRequests["r1"].Status = MARequestApproved
	if s.pendingRequestBetween("fire", "ems") != nil {
		t.Fatal("an approved request must not count as pending")
	}
}

func TestLinkBetweenFindsActiveLinksOnly(t *testing.T) {
	s := newMARTestServer()
	s.mutualAidLinks["l1"] = &MutualAidLink{
		ID: "l1", Agencies: []string{"fire", "ems"}, Active: true, Channel: 50,
	}
	if s.linkBetween("fire", "ems") == nil {
		t.Fatal("active link should be found")
	}
	if s.linkBetween("ems", "fire") == nil {
		t.Fatal("link lookup must be order-independent")
	}
	if s.linkBetween("fire", "dpw") != nil {
		t.Fatal("non-member agency must not match")
	}

	s.mutualAidLinks["l1"].Active = false
	if s.linkBetween("fire", "ems") != nil {
		t.Fatal("an inactive link must not be reported as a live patch")
	}
}

// Approval reuses the same allocator as the super_admin path, so a request
// approved when the shared block is full must fail rather than collide.
func TestCreateLinkFailsWhenChannelBlockFull(t *testing.T) {
	s := newMARTestServer()
	for ch := SharedChannelMin; ch <= SharedChannelMax; ch++ {
		s.mutualAidLinks[string(rune(ch))] = &MutualAidLink{Channel: ch, Active: true}
	}
	if link := s.createMutualAidLinkLocked([]string{"fire", "ems"}, "Ops", "tester"); link != nil {
		t.Fatalf("expected nil when the block is full, got channel %d", link.Channel)
	}
}

func TestCreateLinkAllocatesFromSharedBlock(t *testing.T) {
	s := newMARTestServer()
	link := s.createMutualAidLinkLocked([]string{"fire", "ems"}, "County Ops", "tester")
	if link == nil {
		t.Fatal("expected a link")
	}
	if link.Channel < SharedChannelMin || link.Channel > SharedChannelMax {
		t.Fatalf("channel %d is outside the shared block %d-%d",
			link.Channel, SharedChannelMin, SharedChannelMax)
	}
	if !link.Active || link.ChannelName != "County Ops" {
		t.Fatalf("link not initialised correctly: %+v", link)
	}
	if _, ok := s.mutualAidLinks[link.ID]; !ok {
		t.Fatal("link was not stored")
	}
}

// The revision must change when anything on this agency's Mutual Aid page
// changes, and stay put otherwise — it is what decides whether the open page
// reloads itself.
func TestMutualAidRevisionTracksChanges(t *testing.T) {
	s := newMARTestServer()
	base := s.mutualAidRevision("fire")

	// A request involving someone else must not disturb us.
	s.mutualAidRequests["other"] = &MutualAidRequest{
		ID: "other", FromAgency: "ems", ToAgency: "dpw", Status: MARequestPending}
	if s.mutualAidRevision("fire") != base {
		t.Fatal("an unrelated agency's request changed our revision")
	}

	// An inbound request must change it.
	s.mutualAidRequests["r1"] = &MutualAidRequest{
		ID: "r1", FromAgency: "ems", ToAgency: "fire", Status: MARequestPending}
	withReq := s.mutualAidRevision("fire")
	if withReq == base {
		t.Fatal("an inbound request did not change the revision")
	}

	// Approving it must change it again — the page shows a different thing.
	s.mutualAidRequests["r1"].Status = MARequestApproved
	if s.mutualAidRevision("fire") == withReq {
		t.Fatal("resolving a request did not change the revision")
	}

	// A live patch must count too.
	afterResolve := s.mutualAidRevision("fire")
	s.mutualAidLinks["l1"] = &MutualAidLink{
		ID: "l1", Agencies: []string{"fire", "ems"}, Active: true, Channel: 50}
	if s.mutualAidRevision("fire") == afterResolve {
		t.Fatal("a new patch did not change the revision")
	}
}

// Repeated calls with no underlying change must return the same value, or the
// page would reload on every socket tick.
func TestMutualAidRevisionIsStable(t *testing.T) {
	s := newMARTestServer()
	for i := 0; i < 20; i++ {
		s.mutualAidRequests[string(rune('a'+i))] = &MutualAidRequest{
			ID: string(rune('a' + i)), FromAgency: "fire", ToAgency: "ems",
			Status: MARequestPending}
	}
	first := s.mutualAidRevision("fire")
	for i := 0; i < 10; i++ {
		if s.mutualAidRevision("fire") != first {
			t.Fatal("revision changed without any state change (map ordering?)")
		}
	}
}

func TestMutualAidRevisionZeroForSuperAdmin(t *testing.T) {
	s := newMARTestServer()
	s.mutualAidLinks["l1"] = &MutualAidLink{
		ID: "l1", Agencies: []string{"fire", "ems"}, Active: true}
	if s.mutualAidRevision("") != 0 {
		t.Fatal("super_admin has no per-agency view; revision should be 0")
	}
}
