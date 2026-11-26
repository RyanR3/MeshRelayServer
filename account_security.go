// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Phone sign-in security: one-time enrollment QR codes, per-phone device
// keys, and ending sessions the moment an account changes.
//
// Enrollment QR (dashboard → Users → QR):
//   - The QR's token works ONCE, for 15 minutes. The first phone to sign in
//     with it gets its own random device key, sent encrypted under the token,
//     and the token is destroyed. A photo or screenshot of the QR is useless
//     afterwards. The account's password is never touched, so the password an
//     admin set keeps working in the app and on the dashboard.
//   - An app too old to take a device key (it doesn't set AuthFlagDeviceKey)
//     keeps the token as its password: the token is bound as a "legacy"
//     device key. That phone keeps working, but so would a copy of its QR —
//     the dashboard marks those, and "Sign out phones" removes them.
//
// Sessions: every signed-in address records which account and which
// credential (password or a device key) it used. Deleting or renaming an
// account, resetting its password, signing its phones out, or ending a guest
// event takes the matching sessions off the server immediately, whatever
// callsign they joined under.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// AuthFlagDeviceKey (auth request flags byte): the app can store a
	// server-issued device key after a QR enrollment.
	AuthFlagDeviceKey byte = 0x01

	enrollTokenLife = 15 * time.Minute
	// A phone whose reply carrying the device key was lost (UDP), or whose
	// app opens two connections at startup, may use the same QR again from
	// the same address for this long and gets the same key. Kept short:
	// during it, someone else holding the QR behind the same public address
	// would get that key too.
	enrollRetryGrace = 10 * time.Second
	// A sign-in that never joins a channel is dropped after this.
	unjoinedAuthLife = 2 * time.Minute

	// MinPasswordLength applies to every password an admin sets.
	MinPasswordLength = 12

	credPassword = "pw"
	credGuest    = "guest"
)

// DeviceKey is one phone's credential for an account, issued by a QR.
type DeviceKey struct {
	ID        string `json:"id"`
	Hash      string `json:"hash"` // SHA-256 hex of the secret, the HMAC key, like a password hash
	Legacy    bool   `json:"legacy,omitempty"`
	CreatedAt string `json:"created_at"`
	LastUsed  string `json:"last_used,omitempty"`
	// IP the key was issued to, for the dashboard.
	EnrolledFrom string `json:"enrolled_from,omitempty"`
}

// authSession records how an address signed in.
type authSession struct {
	at   time.Time
	cred string // credPassword, credGuest, or "dev:<id>"
}

func deviceCred(id string) string { return "dev:" + id }

// validateNewPassword: the rule for every password an admin sets.
func validateNewPassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	if len(pw) > 256 {
		return errors.New("password is too long")
	}
	return nil
}

func randomHexBytes(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// deviceKeyWrap is the one-time pad that carries a new device key to the
// phone: HMAC-SHA256(SHA-256(token), "meshrelay-device-key|" + nonce). Only
// someone who holds the QR token can unwrap it.
func deviceKeyWrap(token string, nonce [32]byte) []byte {
	msg := append([]byte("meshrelay-device-key|"), nonce[:]...)
	return computeHmacSha256([]byte(hashPassword(token)), msg)
}

// deviceKeyTag proves a "|devkey=" reply came from a server that holds the QR
// token: HMAC-SHA256(SHA-256(token), "meshrelay-device-key-tag|" + nonce +
// key). The app checks it before storing the key, so a forged reply cannot
// replace a phone's credential with junk.
func deviceKeyTag(token string, nonce [32]byte, secret []byte) string {
	msg := append([]byte("meshrelay-device-key-tag|"), nonce[:]...)
	msg = append(msg, secret...)
	return hex.EncodeToString(computeHmacSha256([]byte(hashPassword(token)), msg))
}

// issueDeviceKeyLocked finishes a QR sign-in: destroys the token's
// usefulness, stores the phone's credential and returns the "|devkey=" reply
// suffix ("" for a legacy app). Caller holds s.mu.
//
// plain: the reply travels inside the secure handshake's encryption, so the
// key is sent as it is ("|devkey=<hex>") instead of wrapped under the token.
func (s *Server) issueDeviceKeyLocked(user *UserAccount, et *EnrollmentToken, nonce [32]byte, flags byte, addr *net.UDPAddr, plain bool) (cred, suffix string, err error) {
	now := time.Now()
	// A retry within the grace window (lost reply, or the app opening two
	// connections at once) gets the SAME key again, so every connection of
	// that phone ends up holding the one key the server keeps.
	if et.DeviceID != "" {
		for _, d := range user.Devices {
			if d.ID != et.DeviceID {
				continue
			}
			if flags&AuthFlagDeviceKey != 0 && et.pendingSecret != nil {
				if plain {
					return deviceCred(d.ID), "|devkey=" + hex.EncodeToString(et.pendingSecret), nil
				}
				pad := deviceKeyWrap(et.Token, nonce)
				wrapped := make([]byte, 32)
				for i := range et.pendingSecret {
					wrapped[i] = et.pendingSecret[i] ^ pad[i]
				}
				return deviceCred(d.ID), "|devkey=" + hex.EncodeToString(wrapped) +
					"|devtag=" + deviceKeyTag(et.Token, nonce, et.pendingSecret), nil
			}
			if flags&AuthFlagDeviceKey == 0 && d.Legacy {
				return deviceCred(d.ID), "", nil
			}
		}
		// Different kind of app on the retry: replace the first key.
		removeDeviceKey(user, et.DeviceID)
	}
	id, err := randomHexBytes(8)
	if err != nil {
		return "", "", err
	}
	dk := &DeviceKey{ID: id, CreatedAt: now.Format(time.RFC3339), EnrolledFrom: addr.IP.String()}
	if flags&AuthFlagDeviceKey != 0 {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return "", "", err
		}
		secretHex := hex.EncodeToString(secret)
		dk.Hash = hashPassword(secretHex)
		et.pendingSecret = secret
		if plain {
			suffix = "|devkey=" + secretHex
		} else {
			pad := deviceKeyWrap(et.Token, nonce)
			wrapped := make([]byte, 32)
			for i := range secret {
				wrapped[i] = secret[i] ^ pad[i]
			}
			suffix = "|devkey=" + hex.EncodeToString(wrapped) + "|devtag=" + deviceKeyTag(et.Token, nonce, secret)
		}
	} else {
		dk.Hash = hashPassword(et.Token)
		dk.Legacy = true
	}
	user.Devices = append(user.Devices, dk)
	if et.UsedAt.IsZero() {
		et.UsedAt = now
		et.UsedFrom = addr.IP.String()
	}
	et.DeviceID = id
	return deviceCred(id), suffix, nil
}

func removeDeviceKey(user *UserAccount, id string) {
	out := user.Devices[:0]
	for _, d := range user.Devices {
		if d.ID != id {
			out = append(out, d)
		}
	}
	user.Devices = out
}

// enrollTokenUsable: unused and unexpired, or within the retry grace from
// the address that used it.
func enrollTokenUsable(et *EnrollmentToken, addr *net.UDPAddr, now time.Time) bool {
	if et.IsGuest {
		return now.Before(et.ExpiresAt)
	}
	if et.UsedAt.IsZero() {
		return now.Before(et.ExpiresAt)
	}
	return now.Sub(et.UsedAt) < enrollRetryGrace && addr != nil && addr.IP.String() == et.UsedFrom
}

// recordAuthLocked marks addr as signed in to account key with cred.
func (s *Server) recordAuthLocked(addr string, agencyID, key, cred string) {
	if s.authenticatedAddrs == nil {
		s.authenticatedAddrs = make(map[string]string)
	}
	s.authenticatedAddrs[addr] = agencyID
	if s.authAccounts == nil {
		s.authAccounts = make(map[string]string)
	}
	s.authAccounts[addr] = key
	if s.authSessions == nil {
		s.authSessions = make(map[string]authSession)
	}
	s.authSessions[addr] = authSession{at: time.Now(), cred: cred}
}

// forgetAddrAuthLocked removes every sign-in record for addr.
func (s *Server) forgetAddrAuthLocked(addr string) {
	s.secCloseAddr(addr) // its secure session ends with the sign-in
	delete(s.authenticatedAddrs, addr)
	delete(s.authAccounts, addr)
	delete(s.authSessions, addr)
}

// dropAddrAuthLocked forgets addr's sign-in unless another client (not
// except) is still using that address. Caller holds s.mu.
func (s *Server) dropAddrAuthLocked(addr string, except *Client) {
	for _, c := range s.clients {
		if c != except && c.Addr != nil && c.Addr.String() == addr {
			return
		}
	}
	s.forgetAddrAuthLocked(addr)
}

// takeSessionsLocked ends, under s.mu, the sessions signed in with account
// key whose credential passes match (nil = every credential) — whatever
// callsign they joined under — and addresses that signed in but never
// joined. Finish the returned kicks with finishSessionKicks AFTER unlocking.
func (s *Server) takeSessionsLocked(key string, match func(cred string) bool) []expiredGuestKick {
	ok := func(addr string) bool {
		if s.authAccounts[addr] != key {
			return false
		}
		return match == nil || match(s.authSessions[addr].cred)
	}
	var kicks []expiredGuestKick
	for ck, c := range s.clients {
		if c.Addr == nil {
			continue
		}
		a := c.Addr.String()
		if ok(a) || (match == nil && s.authAccounts[a] == "" && c.Account == "" && ck == key) {
			s.removeClient(c)
			s.forgetAddrAuthLocked(a)
			kicks = append(kicks, expiredGuestKick{client: c, addr: c.Addr})
		}
	}
	for a := range s.authAccounts {
		if !ok(a) {
			continue
		}
		s.forgetAddrAuthLocked(a)
		if ua, err := net.ResolveUDPAddr("udp", a); err == nil {
			kicks = append(kicks, expiredGuestKick{addr: ua})
		}
	}
	return kicks
}

// takeAccountSessionsLocked ends every session of the account.
func (s *Server) takeAccountSessionsLocked(key string) []expiredGuestKick {
	return s.takeSessionsLocked(key, nil)
}

// pruneAuthLocked drops sign-ins that never joined a channel, and session
// records whose address is no longer signed in. Caller holds s.mu.
func (s *Server) pruneAuthLocked(now time.Time) {
	inUse := make(map[string]bool, len(s.clients))
	for _, c := range s.clients {
		if c.Addr != nil {
			inUse[c.Addr.String()] = true
		}
	}
	for a := range s.authenticatedAddrs {
		if inUse[a] {
			continue
		}
		sess, ok := s.authSessions[a]
		if !ok || now.Sub(sess.at) > unjoinedAuthLife {
			s.forgetAddrAuthLocked(a)
		}
	}
	for a := range s.authSessions {
		if _, ok := s.authenticatedAddrs[a]; !ok {
			delete(s.authSessions, a)
		}
	}
	// Used enrollment tokens past their retry grace are gone for good.
	for t, et := range s.enrollmentTokens {
		if !et.IsGuest && !et.UsedAt.IsZero() && now.Sub(et.UsedAt) >= enrollRetryGrace {
			for i := range et.pendingSecret {
				et.pendingSecret[i] = 0
			}
			delete(s.enrollmentTokens, t)
		}
	}
}

// SignOutDevices removes every phone key of an account and ends all of its
// phone sessions. The account's password still works.
func (s *Server) SignOutDevices(agencyID, username string) (int, error) {
	key := userKey(agencyID, username)
	s.mu.Lock()
	user, ok := s.users[key]
	if !ok {
		s.mu.Unlock()
		return 0, fmt.Errorf("user %s not found", username)
	}
	n := len(user.Devices)
	user.Devices = nil
	for t, et := range s.enrollmentTokens {
		if !et.IsGuest && et.Username == user.Username && et.AgencyID == user.AgencyID {
			delete(s.enrollmentTokens, t)
		}
	}
	kicks := s.takeAccountSessionsLocked(key)
	err := s.saveUsers()
	s.mu.Unlock()
	s.finishSessionKicks(kicks, "Signed out by an administrator")
	log.Printf("Signed out phones for %q [%s]: %d device key(s) removed, %d session(s) ended", username, agencyID, n, len(kicks))
	s.activityLog.Add("devices_signed_out", username, fmt.Sprintf("%d phone key(s) removed, %d session(s) ended", n, len(kicks)), agencyID)
	return n, err
}

// deviceSummary is the dashboard's view of an account's phones.
func deviceSummary(u *UserAccount) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(u.Devices))
	for _, d := range u.Devices {
		out = append(out, map[string]interface{}{
			"id": d.ID, "legacy": d.Legacy, "created_at": d.CreatedAt,
			"last_used": d.LastUsed, "enrolled_from": d.EnrolledFrom,
		})
	}
	return out
}

// devkeySuffixFromMessage is used by tests.
func devkeySuffixFromMessage(msg string) string {
	for _, part := range strings.Split(msg, "|") {
		if strings.HasPrefix(part, "devkey=") {
			return strings.TrimPrefix(part, "devkey=")
		}
	}
	return ""
}

// validCallsign: what a phone may JOIN as. The callsign is shown on every
// phone, the dashboard and the console, so markup and control characters are
// refused at the door (the pages escape it as well).
func validCallsign(name string) bool {
	if name == "" || len(name) > 64 || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '<' || r == '>' || r == '"' || r == '`' || r == '\\' {
			return false
		}
	}
	return true
}

// callsignOwnerLocked returns the account key that owns callsign in agencyID
// ("" when it is nobody's account name): a PTT account with that username
// (case-insensitive), which includes every LMR bridge gateway account.
// Caller holds s.mu.
func (s *Server) callsignOwnerLocked(agencyID, callsign string) string {
	if u := s.users[userKey(agencyID, callsign)]; u != nil {
		return userKey(agencyID, u.Username)
	}
	for _, u := range s.users {
		if u.AgencyID == agencyID && strings.EqualFold(u.Username, callsign) {
			return userKey(agencyID, u.Username)
		}
	}
	return ""
}
