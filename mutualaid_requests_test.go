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
