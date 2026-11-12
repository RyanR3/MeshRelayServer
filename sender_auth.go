package main

import (
	"log"
	"net"
	"strings"
)

// Sender identity for UDP packets.
//
// A UDP session is the source address that completed challenge-response login
// (authenticatedAddrs / authAccounts). Most packets also carry a username, and
// the server used to look that name up across EVERY agency and act on
// whichever record it found — moving that record's address to the sender. A
// user of agency A could therefore take over agency B's same-named (or any
// named) user: their audio, text, floor and emergencies.
//
// The rule now: a packet acts only on the client record that its source
// address is entitled to —
//   - the record of the claimed name in the address's authenticated agency, AND
//   - that record either lives at this address, or belongs to the login
//     account this address authenticated as (the same account reconnecting
//     from a new address after a network switch).
//
// Anything else is dropped. The callsign can differ from the login account
// (login "Xcover", callsign "Dev1"): Client.Account records which login
// account owns a callsign, set when the callsign is JOINed.

// mayActAs reports whether a packet from addr may act as client c. Caller
// holds s.mu (read or write).
func (s *Server) mayActAs(c *Client, addr *net.UDPAddr) bool {
	if c == nil || addr == nil {
		return false
	}
	if c.Addr != nil && c.Addr.String() == addr.String() {
		return true
	}
	acct := s.authAccounts[addr.String()]
	return acct != "" && c.Account == acct
}

// senderClient resolves the client record a packet from addr speaks for,
// given the username written in the packet. Returns nil when the address may
// not act as that name (see above). Caller holds s.mu (read or write).
//
// With auth disabled there is no session to check against, so the old
// name-based lookup is kept for those (development) servers.
func (s *Server) senderClient(claimed string, addr *net.UDPAddr) *Client {
	if addr == nil {
		return nil
	}
	if !s.config.AuthEnabled {
		c, _ := s.findClientForSender(claimed, addr)
		return c
	}
	agencyID := s.authenticatedAddrs[addr.String()]
	if agencyID == "" {
		return nil
	}
	names := []string{claimed}
	if bare := stripRosterLabel(claimed); bare != claimed {
		names = append(names, bare)
	}
	for _, n := range names {
		if c := s.clients[userKey(agencyID, n)]; c != nil && s.mayActAs(c, addr) {
			return c
		}
	}
	return nil
}

// senderBlocked reports whether a packet from addr names a callsign that is
// online in the address's agency but belongs to a DIFFERENT session (another
// login account, a dispatch console). Used to tell an impersonation attempt
// apart from a client that simply isn't registered yet. Caller holds s.mu.
func (s *Server) senderBlocked(claimed string, addr *net.UDPAddr) bool {
	if !s.config.AuthEnabled || addr == nil {
		return false
	}
	agencyID := s.authenticatedAddrs[addr.String()]
	if agencyID == "" {
		return false
	}
	c := s.clients[userKey(agencyID, stripRosterLabel(claimed))]
	if c == nil {
		c = s.clients[userKey(agencyID, claimed)]
	}
	return c != nil && !s.mayActAs(c, addr)
}

// adoptSenderAddr moves c to addr when its packets now come from a new
// address. Only call it for a client returned by senderClient (which proved
// addr belongs to c's login account). Caller holds s.mu (write).
func (s *Server) adoptSenderAddr(c *Client, addr *net.UDPAddr, why string) {
	if addr == nil || (c.Addr != nil && c.Addr.String() == addr.String()) {
		return
	}
	log.Printf("Client %s [%s] address changed %v -> %v (%s)", c.Username, c.AgencyID, c.Addr, addr, why)
	c.Addr = addr
}

// findClientInAgencies finds an online client by callsign among the allowed
// agencies, preferring the record in prefer. A roster label ("medic1 (Fire
// Dept)") is stripped and, when it names one of the allowed agencies, picks
// that agency. Caller holds s.mu.
func (s *Server) findClientInAgencies(name string, allowed map[string]bool, prefer string) *Client {
	bare := stripRosterLabel(name)
	label := ""
	if bare != name {
		label = strings.TrimSuffix(name[len(bare)+len(rosterAgencySuffix):], ")")
	}
	if label == "" && allowed[prefer] {
		if c := s.clients[userKey(prefer, name)]; c != nil {
			return c
		}
	}
	var best *Client
	for _, c := range s.clients {
		if !allowed[c.AgencyID] || (c.Username != name && c.Username != bare) {
			continue
		}
		if label != "" {
			if a := s.agencies[c.AgencyID]; a != nil && a.Name == label {
				return c
			}
		}
		// Deterministic choice among the rest: the preferred agency, then
		// the lowest agency ID.
		if best == nil || c.AgencyID == prefer ||
			(best.AgencyID != prefer && c.AgencyID < best.AgencyID) {
			best = c
		}
	}
	return best
}

// peerClient resolves the recipient of a private message / direct call from a
// sender in senderAgency: their own agency, or one linked to it by an active
// mutual-aid link. Never a same-named user in an unrelated agency. Caller
// holds s.mu.
func (s *Server) peerClient(senderAgency, name string) *Client {
	return s.findClientInAgencies(name, s.mutualAidAgenciesLocked(senderAgency), senderAgency)
}

// clientInAgency returns the online client for a callsign in one agency. A
// super-admin dispatch console has no agency of its own ("" key) and talks
// into one agency's channel; it is matched as a fallback. Caller holds s.mu.
func (s *Server) clientInAgency(agencyID, name string) *Client {
	if c := s.clients[userKey(agencyID, name)]; c != nil {
		return c
	}
	if c := s.clients[userKey("", name)]; c != nil && c.AgencyID == "" {
		return c
	}
	return nil
}

// accountFor returns the user account behind an online client: the login
// account that JOINed the callsign, else the account named like the callsign
// in the client's own agency. Never a same-named account in another agency.
// Caller holds s.mu.
func (s *Server) accountFor(c *Client) *UserAccount {
	if c == nil {
		return nil
	}
	if c.Account != "" {
		if u := s.users[c.Account]; u != nil {
			return u
		}
	}
	return s.users[userKey(c.AgencyID, c.Username)]
}
