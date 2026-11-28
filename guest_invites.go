// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Guest invites from the app.
//
// Any signed-in member of an agency can hand someone a temporary login on the
// spot: the app asks the server for a guest account, and shows the reply as
// the same QR code the dashboard prints for enrollment. The guest scans it
// with MeshRelay and is in the inviter's agency, as an ordinary user, until
// the invite expires — then the account is deleted and any connected session
// is ended.
//
// Limits: guests (and guest-event or gateway logins) cannot invite; an agency
// admin can switch the feature off for their agency (Agency.NoGuestInvites);
// each member can hold at most guestInvitesPerInviter live invites; an invite
// lasts at most a week. Invited guests show in the dashboard's user list,
// where an admin can delete them early like any other user.

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// client -> server: [u16 hours][u16 len|guest name (optional)]
	PacketTypeGuestInviteRequest byte = 0x2C
	// server -> client: [u8 ok][u16 len|username][u16 len|password]
	//                   [u16 len|agency id][i64 expires, unix s][u16 len|message]
	PacketTypeGuestInviteResponse byte = 0x2D

	guestInviteDefaultHours = 24
	guestInviteMaxHours     = 7 * 24
	guestInvitesPerInviter  = 10
	guestNameMaxLen         = 20
)

type GuestInviteRequest struct {
	Hours int
	Name  string
}

func ParseGuestInviteRequest(data []byte) (*GuestInviteRequest, error) {
	r := bytes.NewReader(data)
	var hours uint16
	if err := binary.Read(r, binary.BigEndian, &hours); err != nil {
		return nil, err
	}
	name, _ := getStr(r) // optional; an empty string at the end reads as EOF
	return &GuestInviteRequest{Hours: int(hours), Name: name}, nil
}

type GuestInviteResponse struct {
	OK        bool
	Username  string
	Password  string
	AgencyID  string
	ExpiresAt time.Time
	Message   string
}

func SerializeGuestInviteResponse(p *GuestInviteResponse) []byte {
	var buf bytes.Buffer
	buf.WriteByte(PacketTypeGuestInviteResponse)
	if p.OK {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
	putStr(&buf, p.Username)
	putStr(&buf, p.Password)
	putStr(&buf, p.AgencyID)
	var exp int64
	if !p.ExpiresAt.IsZero() {
		exp = p.ExpiresAt.Unix()
	}
	binary.Write(&buf, binary.BigEndian, exp)
	putStr(&buf, p.Message)
	return buf.Bytes()
}

// guestUsernameBase turns the name the inviter typed into a callsign:
// lowercase letters, digits, '-' and '_', prefixed "guest-".
func guestUsernameBase(name string) string {
	var b strings.Builder
	lastDash := false
	for _, c := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
			b.WriteRune(c)
			lastDash = false
		case c == '-' || c == ' ' || c == '.':
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= guestNameMaxLen {
			break
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		return ""
	}
	return "guest-" + base
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleGuestInviteRequest(req *GuestInviteRequest, addr *net.UDPAddr) {
	// An address that has not signed in gets no answer: the refusal (45
	// bytes) was 15 times the 3-byte request, an amplifier for a forged
	// source (amplification.go).
	if !s.isAuthenticated(addr) {
		return
	}
	resp := s.createInvitedGuest(req, addr)
	s.sendPacket(SerializeGuestInviteResponse(resp), addr)
}

func (s *Server) createInvitedGuest(req *GuestInviteRequest, addr *net.UDPAddr) *GuestInviteResponse {
	fail := func(msg string) *GuestInviteResponse {
		return &GuestInviteResponse{Message: msg}
	}
	if !s.isAuthenticated(addr) {
		return fail("Sign in to the server first")
	}

	hours := req.Hours
	if hours <= 0 {
		hours = guestInviteDefaultHours
	}
	if hours > guestInviteMaxHours {
		hours = guestInviteMaxHours
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	client, _ := s.findClientByAddr(addr)
	if client == nil {
		return fail("Sign in to the server first")
	}
	if s.isGatewayLogin(client) {
		return fail("Gateways can't invite guests")
	}
	// The account this connection signed in with — not the callsign it
	// joined under, which can differ (login "Unit1", callsign "Medic4").
	accountKey := s.authAccounts[addr.String()]
	if accountKey == "" {
		accountKey = userKey(client.AgencyID, client.Username)
	}
	inviter := s.users[accountKey]
	if inviter == nil {
		return fail("Your account wasn't found on the server")
	}
	if inviter.GuestExpiresAt != nil || strings.HasPrefix(inviter.AgencyID, "event-") {
		return fail("Guests can't invite other guests")
	}
	agency := s.agencies[inviter.AgencyID]
	if agency == nil {
		return fail("Your agency wasn't found on the server")
	}
	if agency.NoGuestInvites {
		return fail("Your agency has turned off guest invites")
	}

	now := time.Now()
	live, inAgency := 0, 0
	for _, u := range s.users {
		if u.AgencyID != inviter.AgencyID {
			continue
		}
		inAgency++
		if u.GuestExpiresAt != nil && u.InvitedBy == inviter.Username && now.Before(*u.GuestExpiresAt) {
			live++
		}
	}
	if live >= guestInvitesPerInviter {
		return fail(fmt.Sprintf("You already have %d active guests — an admin can remove some in the dashboard", live))
	}
	if agency.MaxUsers > 0 && inAgency >= agency.MaxUsers {
		return fail("Your agency is at its user limit")
	}

	// Callsign: the name the inviter typed, else a random one. Unique in the
	// agency; "-2", "-3"… on a clash.
	base := guestUsernameBase(req.Name)
	if base == "" {
		base = "guest-" + randomHex(2)
	}
	username := base
	for i := 2; ; i++ {
		_, taken := s.users[userKey(inviter.AgencyID, username)]
		if !taken {
			if c, _ := s.findClientByUsername(username); c == nil {
				break
			}
		}
		if i > 99 {
			username = "guest-" + randomHex(3)
			if _, taken := s.users[userKey(inviter.AgencyID, username)]; !taken {
				break
			}
			continue
		}
		username = fmt.Sprintf("%s-%d", base, i)
	}

	// The QR carries this password; the account stores only its hash, the
	// same way a dashboard enrollment QR binds an account.
	password := randomHex(16)
	expires := now.Add(time.Duration(hours) * time.Hour)
	display := strings.TrimSpace(req.Name)
	if display == "" {
		display = "Guest"
	}
	key := userKey(inviter.AgencyID, username)
	s.users[key] = &UserAccount{
		Username:       username,
		Name:           display + " (guest of " + inviter.Username + ")",
		PasswordHash:   hashPassword(password),
		AgencyID:       inviter.AgencyID,
		Role:           "user",
		CreatedAt:      now.Format(time.RFC3339),
		InvitedBy:      inviter.Username,
		GuestExpiresAt: &expires,
	}
	if err := s.saveUsers(); err != nil {
		delete(s.users, key)
		log.Printf("Guest invite: failed to save users: %v", err)
		return fail("The server couldn't save the guest account")
	}

	log.Printf("Guest invite: %q [%s] invited %q for %dh (expires %s)",
		inviter.Username, inviter.AgencyID, username, hours, expires.Format(time.RFC3339))
	s.activityLog.Add("guest_invited", inviter.Username,
		fmt.Sprintf("Invited guest %s for %d h", username, hours), inviter.AgencyID)

	return &GuestInviteResponse{
		OK:        true,
		Username:  username,
		Password:  password,
		AgencyID:  inviter.AgencyID,
		ExpiresAt: expires,
		Message:   "Guest invited",
	}
}

// expiredGuestKick is a session to end after the lock is released.
type expiredGuestKick struct {
	client *Client
	addr   *net.UDPAddr
}

// removeExpiredInvitedGuests deletes invited guests whose time is up and
// takes their sessions off the server. Caller holds s.mu; the returned
// sessions must be finished with finishExpiredGuestKicks AFTER unlocking.
func (s *Server) removeExpiredInvitedGuests() []expiredGuestKick {
	now := time.Now()
	var kicks []expiredGuestKick
	removed := 0
	for key, u := range s.users {
		if u.GuestExpiresAt == nil || now.Before(*u.GuestExpiresAt) {
			continue
		}
		delete(s.users, key)
		removed++
		log.Printf("Guest %q [%s] (invited by %q) expired, removed", u.Username, u.AgencyID, u.InvitedBy)
		s.activityLog.Add("guest_expired", u.Username,
			fmt.Sprintf("Guest invited by %s expired", u.InvitedBy), u.AgencyID)
		kicks = append(kicks, s.takeAccountSessionsLocked(key)...)
	}
	if removed > 0 {
		if err := s.saveUsers(); err != nil {
			log.Printf("Guest expiry: failed to save users: %v", err)
		}
	}
	return kicks
}

func (s *Server) finishExpiredGuestKicks(kicks []expiredGuestKick) {
	s.finishSessionKicks(kicks, "Guest access expired")
}

// finishSessionKicks tells each ended session why (a revoked, not failed,
// sign-in: the app stops instead of retrying) and finishes the removal.
// Call without s.mu held.
func (s *Server) finishSessionKicks(kicks []expiredGuestKick, reason string) {
	agencies := map[string]bool{}
	for _, k := range kicks {
		if k.addr != nil {
			s.sendPacket(SerializeAuthResponse(&AuthResponsePacket{
				Success: false,
				Revoked: true,
				Message: reason,
			}), k.addr)
		}
		if k.client == nil {
			continue
		}
		s.clientRemovedCleanup(k.client)
		agencies[k.client.AgencyID] = true
	}
	for a := range agencies {
		s.broadcastRoster(a)
	}
}

// --- dashboard switch (Settings) --------------------------------------------
// Same scoping as floor control: an agency admin sets their own agency; a
// super_admin names one with ?agency= / "agency_id".

func (s *Server) handleGetGuestInvites(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	if isSuper {
		if q := r.URL.Query().Get("agency"); q != "" {
			agencyID = q
		}
	}
	s.mu.RLock()
	a := s.agencies[agencyID]
	var enabled bool
	active := 0
	if a != nil {
		enabled = !a.NoGuestInvites
		now := time.Now()
		for _, u := range s.users {
			if u.AgencyID == agencyID && u.GuestExpiresAt != nil && now.Before(*u.GuestExpiresAt) {
				active++
			}
		}
	}
	s.mu.RUnlock()
	if a == nil {
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"agency_id":     agencyID,
		"enabled":       enabled,
		"active_guests": active,
	})
}

func (s *Server) handleUpdateGuestInvites(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var req struct {
		AgencyID *string `json:"agency_id"` // super_admin only
		Enabled  *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if isSuper && req.AgencyID != nil && *req.AgencyID != "" {
		agencyID = *req.AgencyID
	}
	s.mu.Lock()
	a := s.agencies[agencyID]
	if a != nil {
		a.NoGuestInvites = !*req.Enabled
	}
	s.mu.Unlock()
	if a == nil {
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	if err := s.saveAgencies(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("Guest invites for agency %s: enabled=%v", agencyID, *req.Enabled)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "agency_id": agencyID, "enabled": *req.Enabled})
}
