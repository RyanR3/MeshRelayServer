package main

import "testing"

func newZelloAgencyTestServer() *Server {
	return &Server{
		agencies:     map[string]*Agency{},
		zelloBridges: map[string]*ZelloBridge{},
	}
}

// A bridge is private to its owner. Another agency sees it only once granted.
func TestZelloBridgeVisibility(t *testing.T) {
	b := &ZelloBridge{
		ID: "b1", OwnerAgencyID: "fire", AgencyID: "fire",
		Mappings: []ZelloMapping{{ZelloChannel: "Dispatch", MeshChannel: 150, AgencyID: "fire"}},
	}

	if !b.ownedBy("fire") {
		t.Error("owner must own it")
	}
	if b.ownedBy("ems") {
		t.Error("a non-owner must not own it")
	}
	if b.visibleTo("ems") {
		t.Error("an ungranted agency must not see the bridge at all")
	}

	// Grant EMS access.
	b.Mappings[0].ExtraAgencies = []string{"ems"}
	if !b.sharedWith("ems") {
		t.Error("EMS should now have access")
	}
	if !b.visibleTo("ems") {
		t.Error("EMS should now see the bridge")
	}
	if b.ownedBy("ems") {
		t.Error("access is USE only — EMS must never become an owner")
	}
	if b.visibleTo("dpw") {
		t.Error("a third agency still must not see it")
	}
}

// Ownership must be explicit, not inferred from mapping order — reordering
// mappings must not hand the bridge to someone else.
func TestZelloOwnershipSurvivesMappingReorder(t *testing.T) {
	b := &ZelloBridge{
		ID: "b1", OwnerAgencyID: "fire", AgencyID: "fire",
		Mappings: []ZelloMapping{
			{ZelloChannel: "A", MeshChannel: 150, AgencyID: "fire"},
			{ZelloChannel: "B", MeshChannel: 151, AgencyID: "ems"},
		},
	}
	b.Mappings[0], b.Mappings[1] = b.Mappings[1], b.Mappings[0]
	if !b.ownedBy("fire") {
		t.Fatal("reordering mappings changed the owner")
	}
	if b.ownedBy("ems") {
		t.Fatal("a mapping target became the owner")
	}
}

// An empty owner is a legacy super_admin bridge: no agency owns it.
func TestZelloLegacyBridgeHasNoAgencyOwner(t *testing.T) {
	b := &ZelloBridge{ID: "old", AgencyID: "fire",
		Mappings: []ZelloMapping{{ZelloChannel: "X", MeshChannel: 150, AgencyID: "fire"}}}
	if b.ownedBy("fire") {
		t.Error("a bridge with no OwnerAgencyID must not be owned by an agency")
	}
	if b.ownedBy("") {
		t.Error("empty agency must never own anything")
	}
	// It is still USABLE by its mapped agency.
	if !b.sharedWith("fire") {
		t.Error("the mapped agency should still have access")
	}
}

func TestZelloSharedWithIgnoresEmptyAgency(t *testing.T) {
	b := &ZelloBridge{ID: "b1", OwnerAgencyID: "fire",
		Mappings: []ZelloMapping{{ZelloChannel: "A", MeshChannel: 150, AgencyID: "fire"}}}
	if b.sharedWith("") {
		t.Error("an empty agency id must not match")
	}
	if b.visibleTo("") {
		t.Error("an empty agency id must see nothing")
	}
}
