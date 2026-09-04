package main

import "testing"

// An ad-hoc group must NOT be allocated a channel that a Zello bridge mapping
// already occupies. Reproduces the reported bug: Zello on 11 and 12, and a new
// group landing on 11.
func TestPickAdhocChannelAvoidsZello(t *testing.T) {
	s := &Server{
		zelloBridges: map[string]*ZelloBridge{
			"b1": {
				ID: "b1", AgencyID: "default",
				Mappings: []ZelloMapping{
					{ZelloChannel: "Ops", MeshChannel: 11, AgencyID: "default"},
					{ZelloChannel: "Fire", MeshChannel: 12, AgencyID: "default"},
				},
			},
		},
		groupMgr: nil,
	}

	ch := s.pickAdhocChannel("default")
	if ch == 11 || ch == 12 {
		t.Fatalf("ad-hoc group allocated channel %d, which Zello already uses", ch)
	}
	if ch != 10 {
		t.Errorf("expected the first free slot (10), got %d", ch)
	}
	t.Logf("OK: ad-hoc group got channel %d, avoiding Zello's 11/12", ch)
}

// And the reverse: a Zello mapping must not land on a live ad-hoc group.
func TestPickZelloChannelAvoidsAdhoc(t *testing.T) {
	s := &Server{
		zelloBridges: map[string]*ZelloBridge{},
	}
	s.groupMgr = NewGroupManager(s)
	s.groupMgr.CreateAdhoc(channelKey("default", 10), 10, "Squad", GroupTypePreconfigured, "medic1", 0)

	ch := s.pickZelloBridgeChannel()
	if ch == 10 {
		t.Fatalf("Zello mapping allocated channel 10, which an ad-hoc group already uses")
	}
	t.Logf("OK: Zello got channel %d, avoiding the ad-hoc group on 10", ch)
}
