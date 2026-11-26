// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test server: two agencies, auth ON, every outgoing packet captured per
// destination address. Logins go through the real challenge-response path.
// ---------------------------------------------------------------------------

type secSink struct {
	mu   sync.Mutex
	sent map[string][][]byte // addr string -> packets
}

func (k *secSink) add(addr *net.UDPAddr, data []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.sent[addr.String()] = append(k.sent[addr.String()], append([]byte(nil), data...))
}

func (k *secSink) take(addr *net.UDPAddr) [][]byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := k.sent[addr.String()]
	delete(k.sent, addr.String())
	return out
}

func (k *secSink) reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.sent = map[string][][]byte{}
}

// countType returns how many packets of type t were sent to addr (and clears).
func (k *secSink) countType(addr *net.UDPAddr, t byte) int {
	n := 0
	for _, p := range k.take(addr) {
		if len(p) > 0 && p[0] == t {
			n++
		}
	}
	return n
}

func newSecTestServer(t *testing.T) (*Server, *secSink) {
	t.Helper()
	dir := t.TempDir()
	s := &Server{
		config: &Config{
			UsersFile:     filepath.Join(dir, "users.json"),
			AgenciesFile:  filepath.Join(dir, "agencies.json"),
			WebAdminsFile: filepath.Join(dir, "web_admins.json"),
		},
		clients:            map[string]*Client{},
		channels:           map[string]*Channel{},
		groups:             map[string]*GroupPacket{},
		users:              map[string]*UserAccount{},
		agencies:           map[string]*Agency{},
		authenticatedAddrs: map[string]string{},
		authAccounts:       map[string]string{},
		lastChannel:        map[string]int{},
		enrollmentTokens:   map[string]*EnrollmentToken{},
		guestEvents:        map[string]*GuestEvent{},
		webAdmins:          map[string]*WebAdmin{},
		dispatchConns:      map[string]*DispatchConn{},
		mutualAidLinks:     map[string]*MutualAidLink{},
		lmrBridges:         map[string]*LMRBridge{},
		zelloBridges:       map[string]*ZelloBridge{},
		ssrcToUser:         map[uint32]string{},
		shutdown:           make(chan struct{}),
		stats:              &Stats{StartTime: time.Now()},
		activityLog:        NewActivityLog(filepath.Join(dir, "activity.json"), 1000),
	}
	s.lastChannelSavePending = true // keep the 2 s last_channels writer out of the test dir
	s.floor = NewFloorController(s)
	s.groupMgr = NewGroupManager(s)
	for _, a := range []string{"a1", "a2"} {
		s.agencies[a] = &Agency{ID: a, Name: "Agency " + a, MaxUsers: 100}
		s.ensureAgencyChannels(a)
	}
	sink := &secSink{sent: map[string][][]byte{}}
	s.testFloorSink = sink.add
	return s, sink
}

func addUser(s *Server, agency, name, password string) {
	s.users[userKey(agency, name)] = &UserAccount{Username: name, AgencyID: agency, Role: "user", PasswordHash: hashPassword(password)}
}

func udp(port int) *net.UDPAddr { return &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: port} }

// login runs challenge-response for username/agency with secret (a password
// or an enrollment token: either way the client keys the HMAC with its
// SHA-256). Reports whether the server answered success.
func login(t *testing.T, s *Server, sink *secSink, addr *net.UDPAddr, username, agency, secret string) bool {
	t.Helper()
	sink.take(addr)
	s.handleAuthChallengeRequest(&AuthChallengeRequestPacket{Username: username, AgencyID: agency}, addr)
	var nonce [32]byte
	got := false
	for _, p := range sink.take(addr) {
		if p[0] == PacketTypeAuthChallenge && len(p) == 33 {
			copy(nonce[:], p[1:])
			got = true
		}
	}
	if !got {
		t.Fatalf("no auth challenge sent to %v", addr)
	}
	mac := hex.EncodeToString(computeHmacSha256([]byte(hashPassword(secret)), nonce[:]))
	s.handleAuthRequest(&AuthRequestPacket{Username: username, PasswordHash: mac, AgencyID: agency}, addr)
	for _, p := range sink.take(addr) {
		if p[0] == PacketTypeAuthResponse {
			return p[1] == 0x01
		}
	}
	t.Fatalf("no auth response sent to %v", addr)
	return false
}

func mustLogin(t *testing.T, s *Server, sink *secSink, addr *net.UDPAddr, username, agency, secret string) {
	t.Helper()
	if !login(t, s, sink, addr, username, agency, secret) {
		t.Fatalf("login %s [%s] from %v failed", username, agency, addr)
	}
}

func opus(user string) *AudioPacket {
	return &AudioPacket{Username: user, AudioData: []byte{1, 2, 3, 4}, Codec: CodecOpus}
}

// ---------------------------------------------------------------------------
// S1: an enrollment token authenticates only the account it was issued for.
// ---------------------------------------------------------------------------

func TestEnrollmentTokenIsBoundToItsAgency(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "bob", "a1-password")
	addUser(s, "a2", "bob", "a2-password")
	a2Hash := s.users[userKey("a2", "bob")].PasswordHash
	token := s.GenerateEnrollmentToken("bob", "a1", s.users[userKey("a1", "bob")].PasswordHash)

	// Agency A's QR presented as agency B's bob: refused, B's password untouched.
	if login(t, s, sink, udp(5001), "bob", "a2", token) {
		t.Fatal("a1's enrollment token logged in as a2's bob")
	}
	if s.users[userKey("a2", "bob")].PasswordHash != a2Hash {
		t.Fatal("a1's enrollment token rebound a2's bob's password")
	}
	if s.authenticatedAddrs[udp(5001).String()] != "" {
		t.Fatal("refused login left the address authenticated")
	}

	// An old client that sends no agency: the token picks ITS account (a1).
	mustLogin(t, s, sink, udp(5002), "bob", "", token)
	if got := s.authenticatedAddrs[udp(5002).String()]; got != "a1" {
		t.Fatalf("token login landed in agency %q, want a1", got)
	}
	if s.users[userKey("a2", "bob")].PasswordHash != a2Hash {
		t.Fatal("a2's bob's password changed by a1's token")
	}
	// The QR never replaces the password; this (older-app) login bound the
	// token as a legacy phone key instead.
	if b := s.users[userKey("a1", "bob")]; b.PasswordHash != hashPassword("a1-password") || len(b.Devices) != 1 || !b.Devices[0].Legacy {
		t.Fatal("a1's bob: password must be unchanged and the token kept as one legacy phone key")
	}

	// No agency + a2's own password: the password picks a2, not map order.
	mustLogin(t, s, sink, udp(5003), "bob", "", "a2-password")
	if got := s.authenticatedAddrs[udp(5003).String()]; got != "a2" {
		t.Fatalf("password login landed in agency %q, want a2", got)
	}
	// The older app's phone keeps working with the token (its legacy key).
	mustLogin(t, s, sink, udp(5004), "bob", "a1", token)
}

// Guest event tokens are bound the same way.
func TestGuestTokenIsBoundToItsAgency(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a2", "guest", "pw2")
	s.users[userKey("a1", "guest")] = &UserAccount{Username: "guest", AgencyID: "a1", Role: "user", PasswordHash: "x"}
	s.enrollmentTokens["tok"] = &EnrollmentToken{Token: "tok", Username: "guest", AgencyID: "a1", ExpiresAt: time.Now().Add(time.Hour), IsGuest: true}
	if login(t, s, sink, udp(5101), "guest", "a2", "tok") {
		t.Fatal("a1's guest token logged in as a2's same-named user")
	}
	mustLogin(t, s, sink, udp(5102), "guest", "a1", "tok")
}

// ---------------------------------------------------------------------------
// S2 / S5: packets act as the client signed in at the source address only.
// ---------------------------------------------------------------------------

// Agency a2 has vic and peer on channel 1; agency a1 has mallory. Returns the
// three addresses.
func crossAgencySetup(t *testing.T) (*Server, *secSink, *net.UDPAddr, *net.UDPAddr, *net.UDPAddr) {
	s, sink := newSecTestServer(t)
	addUser(s, "a2", "vic", "pv")
	addUser(s, "a2", "peer", "pp")
	addUser(s, "a1", "mallory", "pm")
	vic, peer, mal := udp(6001), udp(6002), udp(6003)
	mustLogin(t, s, sink, vic, "vic", "a2", "pv")
	mustLogin(t, s, sink, peer, "peer", "a2", "pp")
	mustLogin(t, s, sink, mal, "mallory", "a1", "pm")
	s.handleJoin(&JoinPacket{Username: "vic", Channel: 1}, vic)
	s.handleJoin(&JoinPacket{Username: "peer", Channel: 1}, peer)
	s.handleJoin(&JoinPacket{Username: "mallory", Channel: 1}, mal)
	sink.reset()
	return s, sink, vic, peer, mal
}

func TestCrossAgencyHeartbeatCannotHijackSession(t *testing.T) {
	s, sink, vic, peer, mal := crossAgencySetup(t)

	s.handleHeartbeat(&HeartbeatPacket{Username: "vic"}, mal)
	s.handleAudioPacket(opus("vic"), mal)

	if got := s.clients[userKey("a2", "vic")].Addr.String(); got != vic.String() {
		t.Fatalf("a2's vic was moved to %s by a1's heartbeat/audio", got)
	}
	// vic still hears its own agency: peer's audio reaches vic's real address.
	sink.reset()
	s.handleAudioPacket(opus("peer"), peer)
	if sink.countType(vic, PacketTypeAudioOpus) != 1 {
		t.Fatal("vic went deaf after the hijack attempt")
	}
	if n := sink.countType(mal, PacketTypeAudioOpus); n != 0 {
		t.Fatalf("a1's mallory received %d of a2's audio frames", n)
	}
}

func TestCrossAgencyImpersonationIsDropped(t *testing.T) {
	s, sink, vic, peer, mal := crossAgencySetup(t)
	s.floorOn.Store("a2", floorModeBusy)

	// Audio claiming to be a2's vic: nothing reaches a2.
	s.handleAudioPacket(opus("vic"), mal)
	if n := sink.countType(peer, PacketTypeAudioOpus); n != 0 {
		t.Fatalf("impersonated audio relayed to a2 (%d frames)", n)
	}

	// Text as a2's vic: not relayed to a2, privately or on the channel.
	s.handleTextPacket(&TextMessagePacket{Sender: "vic", Content: "evacuate", Timestamp: 1}, mal)
	s.handleTextPacket(&TextMessagePacket{Sender: "vic", Recipient: "peer", Content: "meet me", Timestamp: 2}, mal)
	if n := sink.countType(peer, PacketTypeText); n != 0 {
		t.Fatalf("impersonated text delivered to a2 (%d)", n)
	}
	// Mallory's own private text to a2's peer (no mutual aid): not delivered.
	s.handleTextPacket(&TextMessagePacket{Sender: "mallory", Recipient: "peer", Content: "hi", Timestamp: 3}, mal)
	if n := sink.countType(peer, PacketTypeText); n != 0 {
		t.Fatal("private text crossed agencies without a mutual-aid link")
	}

	// Direct audio as vic, and to a2's peer from mallory: dropped.
	s.handleDirectAudioPacket(&DirectAudioPacket{Sender: "vic", Recipient: "peer", AudioData: []byte{1}}, mal)
	s.handleDirectAudioPacket(&DirectAudioPacket{Sender: "mallory", Recipient: "peer", AudioData: []byte{1}}, mal)
	if n := sink.countType(peer, PacketTypeDirectAudio); n != 0 {
		t.Fatal("direct audio crossed agencies")
	}

	// Floor and emergency as vic: a2's floor untouched.
	s.handleFloorRequest(&FloorRequest{Channel: 1, User: "vic", Priority: PriorityResponder}, mal)
	if s.floor.HasFloor("a2:1", "vic") {
		t.Fatal("a1 user took a2's floor as vic")
	}
	s.handleEmergencyAlert(&EmergencyAlert{Channel: 1, User: "vic", Active: true}, mal)
	if s.floor.HasFloor("a2:1", "vic") {
		t.Fatal("a1 user raised an emergency as a2's vic")
	}

	// Ad-hoc group as vic: not created.
	s.handleGroupCreateAdhoc(&GroupCreateAdhoc{Creator: "vic", GroupName: "x", Members: []string{"peer"}}, mal)
	if n := sink.countType(peer, PacketTypeGroupInvite); n != 0 {
		t.Fatal("ad-hoc invite sent to a2 on behalf of vic")
	}

	// The real vic still works.
	s.handleFloorRequest(&FloorRequest{Channel: 1, User: "vic", Priority: PriorityResponder}, vic)
	if !s.floor.HasFloor("a2:1", "vic") {
		t.Fatal("vic's own floor request was not granted")
	}
	s.handleAudioPacket(opus("vic"), vic)
	if n := sink.countType(peer, PacketTypeAudioOpus); n != 1 {
		t.Fatalf("vic's own audio relayed %d times to peer, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// S6 + reconnects: callsigns belong to the login account that joined them.
// ---------------------------------------------------------------------------

func TestJoinCannotTakeOverAnotherAccountsCallsign(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "xcover", "px") // login account; callsign differs
	addUser(s, "a1", "other", "po")
	first, other := udp(7001), udp(7002)
	mustLogin(t, s, sink, first, "xcover", "a1", "px")
	mustLogin(t, s, sink, other, "other", "a1", "po")
	s.handleJoin(&JoinPacket{Username: "Dev1", Channel: 2}, first)

	c := s.clients[userKey("a1", "Dev1")]
	if c == nil || c.Account != userKey("a1", "xcover") {
		t.Fatalf("Dev1 should be owned by login xcover, got %+v", c)
	}

	// Another account in the same agency cannot take the callsign.
	s.handleJoin(&JoinPacket{Username: "Dev1", Channel: 3}, other)
	s.handleHeartbeat(&HeartbeatPacket{Username: "Dev1"}, other)
	if c := s.clients[userKey("a1", "Dev1")]; c.Addr.String() != first.String() || c.Channel != 2 {
		t.Fatalf("other account took over Dev1: %+v", c)
	}

	// The SAME account after a network switch: new address, fresh login,
	// heartbeat alone re-homes the callsign (no JOIN needed) ...
	moved := udp(7003)
	mustLogin(t, s, sink, moved, "xcover", "a1", "px")
	s.handleHeartbeat(&HeartbeatPacket{Username: "Dev1"}, moved)
	if c := s.clients[userKey("a1", "Dev1")]; c.Addr.String() != moved.String() {
		t.Fatalf("same account's heartbeat from its new address did not move Dev1 (at %v)", c.Addr)
	}
	if sink.countType(moved, PacketTypeHeartbeat) != 1 {
		t.Fatal("heartbeat from the new address was not echoed")
	}
	// ... and so does a JOIN from yet another address.
	again := udp(7004)
	mustLogin(t, s, sink, again, "xcover", "a1", "px")
	s.handleJoin(&JoinPacket{Username: "Dev1", Channel: 4}, again)
	if c := s.clients[userKey("a1", "Dev1")]; c.Addr.String() != again.String() || c.Channel != 4 {
		t.Fatalf("same account re-join failed: %+v", c)
	}
}

// A dispatch console's record (loopback address, no login account) cannot be
// taken over by a phone using the same name.
func TestJoinCannotTakeOverDispatchConsole(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "phone", "pp")
	console := &Client{Username: "Dispatch", AgencyID: "a1", Addr: newDispatchAddr(), Channel: 0, LastHeartbeat: time.Now()}
	s.clients[userKey("a1", "Dispatch")] = console
	ph := udp(7101)
	mustLogin(t, s, sink, ph, "phone", "a1", "pp")
	s.handleJoin(&JoinPacket{Username: "Dispatch", Channel: 0}, ph)
	s.handleHeartbeat(&HeartbeatPacket{Username: "Dispatch"}, ph)
	if !s.clients[userKey("a1", "Dispatch")].Addr.IP.Equal(net.IPv4(127, 0, 0, 2)) {
		t.Fatal("a phone took over the dispatch console's session")
	}
}

// An address that never logged in (new NAT mapping) gets silence — which is
// what makes clients reconnect and log in again — and moves nothing.
func TestUnauthenticatedHeartbeatIsSilent(t *testing.T) {
	s, sink, vic, _, _ := crossAgencySetup(t)
	stranger := udp(6999)
	s.handleHeartbeat(&HeartbeatPacket{Username: "vic"}, stranger)
	if len(sink.take(stranger)) != 0 {
		t.Fatal("server answered an unauthenticated heartbeat")
	}
	if s.clients[userKey("a2", "vic")].Addr.String() != vic.String() {
		t.Fatal("unauthenticated heartbeat moved the client")
	}
}

// A heartbeat after the server forgot the client (restart / time-out, address
// still authenticated) re-registers it under the login account.
func TestHeartbeatAutoRegistersOwnCallsign(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "gw", "pg")
	a := udp(7201)
	mustLogin(t, s, sink, a, "gw", "a1", "pg")
	s.lastChannel[userKey("a1", "gw")] = 7
	s.handleHeartbeat(&HeartbeatPacket{Username: "gw"}, a)
	c := s.clients[userKey("a1", "gw")]
	if c == nil || c.Channel != 7 || c.Account != userKey("a1", "gw") {
		t.Fatalf("auto-register = %+v, want channel 7 owned by gw", c)
	}
}

// ---------------------------------------------------------------------------
// S3: parsers never allocate a claimed length the packet doesn't carry.
// ---------------------------------------------------------------------------

func TestParsersRejectLengthsBeyondPacket(t *testing.T) {
	be16 := func(v int) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, uint16(v)); return b }
	be32 := func(v int) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, uint32(v)); return b }
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	// 7 bytes on the wire claiming a 10 MB audio frame (the audit's PoC).
	audio := cat([]byte{PacketTypeAudioOpus}, be16(0), be32(10*1024*1024))
	direct := cat([]byte{PacketTypeDirectAudio}, be16(0), be16(0), be32(10*1024*1024))
	text := cat([]byte{PacketTypeText}, be16(0), be16(0), be16(10000))
	auth := cat([]byte{PacketTypeAuthRequest}, be16(256))
	group := cat([]byte{PacketTypeGroupCreate}, be16(512))
	adhoc := cat([]byte{PacketTypeGroupCreateAdhoc}, be16(0), be16(0), be16(0), be16(65535))
	unit := cat([]byte{PacketTypeDispatchMessage}, be16(65535))

	for name, pkt := range map[string][]byte{"audio": audio, "direct": direct, "text": text,
		"auth": auth, "group": group, "adhoc": adhoc, "dispatch": unit} {
		if _, _, err := ParsePacket(pkt); err == nil {
			t.Errorf("%s: oversized length accepted", name)
		}
	}

	// And it costs (almost) nothing: 1000 hostile packets must not allocate
	// anywhere near 1000 x 10 MB.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < 1000; i++ {
		ParsePacket(audio)
		ParsePacket(direct)
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Fatalf("parsing 2000 hostile packets allocated %d MB", grew>>20)
	}

	// Well-formed packets still parse.
	ok := cat([]byte{PacketTypeAudioOpus}, be16(3), []byte("bob"), be32(2), []byte{9, 9})
	if _, p, err := ParsePacket(ok); err != nil || p.(*AudioPacket).Username != "bob" {
		t.Fatalf("valid audio packet rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// S4: heartbeats, joins, channel switches, leaves and relays running together
// must not race (go test -race; the old heartbeat loop walked a channel's
// member map unlocked — a fatal "concurrent map iteration and map write").
// ---------------------------------------------------------------------------

func TestConcurrentTrafficNoRace(t *testing.T) {
	s, sink := newSecTestServer(t)
	const n = 6
	addrs := make([]*net.UDPAddr, n)
	for i := 0; i < n; i++ {
		name := "u" + string(rune('a'+i))
		addUser(s, "a1", name, "pw")
		addrs[i] = udp(8000 + i)
		mustLogin(t, s, sink, addrs[i], name, "a1", "pw")
		s.handleJoin(&JoinPacket{Username: name, Channel: 1}, addrs[i])
	}
	s.mutualAidLinks["l"] = &MutualAidLink{ID: "l", Agencies: []string{"a1", "a2"}, Channel: 1, Active: true}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		name := "u" + string(rune('a'+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; ; k++ {
				select {
				case <-stop:
					return
				default:
				}
				switch k % 5 {
				case 0:
					s.handleHeartbeat(&HeartbeatPacket{Username: name}, addrs[i])
				case 1:
					s.handleJoin(&JoinPacket{Username: name, Channel: 1 + k%3}, addrs[i])
				case 2:
					s.handleAudioPacket(opus(name), addrs[i])
				case 3:
					s.handleTextPacket(&TextMessagePacket{Sender: name, Content: "x"}, addrs[i])
				case 4:
					s.checkHeartbeats()
				}
			}
		}()
	}
	// Link list churn from "the dashboard" while packets flow.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.Lock()
			s.mutualAidLinks["l"].Active = k%2 == 0
			s.ensureAgencyChannels("a3")
			s.mu.Unlock()
		}
	}()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// ---------------------------------------------------------------------------
// S13: secret files are owner-only and replaced atomically.
// ---------------------------------------------------------------------------

func TestSecretFilesAreOwnerOnly(t *testing.T) {
	s, _ := newSecTestServer(t)
	addUser(s, "a1", "bob", "pw")
	// An existing file from an older version, world-readable.
	if err := os.WriteFile(s.config.UsersFile, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.saveUsersUnlocked(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.config.UsersFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("users.json mode %o, want 600", perm)
	}
	data, _ := os.ReadFile(s.config.UsersFile)
	if !bytes.Contains(data, []byte(`"bob"`)) {
		t.Fatalf("users.json content not saved: %s", data)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(s.config.UsersFile))
	for _, e := range entries {
		if e.Name() != "users.json" && e.Name() != "activity.json" {
			t.Errorf("leftover file %s", e.Name())
		}
	}

	// Config the same way.
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfgPath, []byte("{}"), 0o644)
	if err := (&Config{}).Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(cfgPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %o, want 600", fi.Mode().Perm())
	}
}

// A /command console is reached through the phone's "Dispatch" conversation;
// it must not also appear in the roster as a second, un-messageable contact.
func TestRosterExcludesDispatchConsoles(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "phone", "pp")
	s.clients[userKey("a1", "WCDRT900")] = &Client{Username: "WCDRT900", AgencyID: "a1",
		Addr: newDispatchAddr(), LastHeartbeat: time.Now()}
	ph := udp(7301)
	mustLogin(t, s, sink, ph, "phone", "a1", "pp")
	s.handleJoin(&JoinPacket{Username: "phone", Channel: 1}, ph)
	sink.reset()
	s.broadcastRoster("a1")
	var roster []byte
	for _, p := range sink.take(ph) {
		if len(p) > 0 && p[0] == PacketTypeRoster {
			roster = p
		}
	}
	if roster == nil {
		t.Fatal("phone got no roster")
	}
	if !bytes.Contains(roster, []byte("phone")) {
		t.Fatal("roster is missing the phone itself")
	}
	if bytes.Contains(roster, []byte("WCDRT900")) {
		t.Fatal("dispatch console listed in the phone roster")
	}
}

// The client list goes to every admin's and dispatcher's browser: it must not
// carry the phones' IP addresses.
func TestClientListHasNoIPAddresses(t *testing.T) {
	s, _ := newSecTestServer(t)
	s.clients[userKey("a1", "u1")] = &Client{Username: "u1", AgencyID: "a1", Addr: udp(7001)}
	for _, c := range s.GetClients("") {
		for k, v := range c {
			if str, ok := v.(string); ok && strings.Contains(str, "10.0.0.1") {
				t.Fatalf("client list field %q carries the IP address: %q", k, str)
			}
		}
		if _, ok := c["address"]; ok {
			t.Fatalf("client list still has an address field")
		}
	}
}
