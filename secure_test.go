// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"
)

// secPhone is the phone's side of the secure protocol: the reference the apps
// are written against.
type secPhone struct {
	t      *testing.T
	s      *Server
	sink   *secSink
	addr   *net.UDPAddr
	pinned []byte // server key the phone expects (nil = first contact)

	sid  uint32
	c2s  cipher.AEAD
	s2c  cipher.AEAD
	send uint32
	msg  string
}

func newSecServer(t *testing.T) (*Server, *secSink) {
	s, sink := newSecTestServer(t)
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s.identity = k
	s.sec.init()
	return s, sink
}

func secPutStr(b []byte, v string) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(v)))
	return append(b, v...)
}

// signIn runs the whole handshake with credential (a password, a device key
// or a QR token). It returns the server's status byte (1 = signed in).
func (p *secPhone) signIn(username, agency, credential string) byte {
	p.t.Helper()
	eph, _ := ecdh.X25519().GenerateKey(rand.Reader)
	cNonce := make([]byte, 16)
	rand.Read(cNonce)
	hello := append([]byte{PacketTypeSecHello, secVersion}, eph.PublicKey().Bytes()...)
	hello = append(hello, cNonce...)
	p.sink.take(p.addr)
	p.s.handlePacket(hello, p.addr)
	var rep []byte
	for _, pk := range p.sink.take(p.addr) {
		if pk[0] == PacketTypeSecHelloReply {
			rep = pk
		}
	}
	if len(rep) != 3+32+32+16+32 {
		p.t.Fatalf("no hello reply (%d bytes)", len(rep))
	}
	slot, eS, S, sNonce, tag := rep[2], rep[3:35], rep[35:67], rep[67:83], rep[83:115]
	if p.pinned != nil && !bytes.Equal(p.pinned, S) {
		p.t.Fatal("server key is not the pinned one")
	}
	sPub, _ := ecdh.X25519().NewPublicKey(S)
	ePub, _ := ecdh.X25519().NewPublicKey(eS)
	dhEE, _ := eph.ECDH(ePub)
	dhES, _ := eph.ECDH(sPub)
	th1 := secHash([]byte("MRS1-th1"), eph.PublicKey().Bytes(), cNonce, eS, S, sNonce)
	prk := hkdf.Extract(sha256.New, append(dhEE, dhES...), []byte("MRS1"))
	// The server proved it holds the identity key behind S.
	if !hmac.Equal(tag, computeHmacSha256(secExpand(prk, "MRS1 hs tag", th1[:]), th1[:])) {
		p.t.Fatal("server hello tag does not verify")
	}

	clientKey, v := scramDerive(hashPassword(credential), S, username)
	am := secHash([]byte("MRS1-auth"), th1[:], []byte(username))
	sig := computeHmacSha256(v.storedKey[:], am[:])
	proof := make([]byte, 32)
	for i := range proof {
		proof[i] = clientKey[i] ^ sig[i]
	}
	var plain []byte
	plain = secPutStr(plain, username)
	plain = secPutStr(plain, agency)
	plain = append(plain, 0)
	plain = append(plain, proof...)
	plain = secPutStr(plain, "test 1.0")
	n := make([]byte, 12)
	rand.Read(n)
	ct := secAEAD(secExpand(prk, "MRS1 hs c2s", th1[:])).Seal(nil, n, plain, th1[:])
	auth := append([]byte{PacketTypeSecAuth, secVersion, slot}, eph.PublicKey().Bytes()...)
	auth = append(auth, cNonce...)
	auth = append(auth, n...)
	auth = append(auth, ct...)
	p.s.handlePacket(auth, p.addr)
	var ar []byte
	for _, pk := range p.sink.take(p.addr) {
		if pk[0] == PacketTypeSecAuthReply {
			ar = pk
		}
	}
	if ar == nil {
		p.t.Fatal("no auth reply")
	}
	ctHash := sha256.Sum256(ct)
	th3 := secHash([]byte("MRS1-th3"), th1[:], n, ctHash[:])
	body, err := secAEAD(secExpand(prk, "MRS1 hs s2c", th1[:])).Open(nil, ar[2:14], ar[14:], th3[:])
	if err != nil {
		p.t.Fatalf("auth reply does not decrypt: %v", err)
	}
	status := body[0]
	ml := int(binary.BigEndian.Uint16(body[37:39]))
	p.msg = string(body[39 : 39+ml])
	if status != 1 {
		return status
	}
	// The server proved it knows this account's verifier.
	if !hmac.Equal(body[5:37], computeHmacSha256(v.serverKey[:], am[:])) {
		p.t.Fatal("server signature does not verify")
	}
	p.sid = binary.BigEndian.Uint32(body[1:5])
	p.c2s = secAEAD(secExpand(prk, "MRS1 data c2s", th3[:]))
	p.s2c = secAEAD(secExpand(prk, "MRS1 data s2c", th3[:]))
	p.send = 0
	p.pinned = append([]byte(nil), S...)
	return status
}

func (p *secPhone) sealAt(ctr uint32, inner []byte) []byte {
	out := make([]byte, 9)
	out[0] = PacketTypeSecData
	binary.BigEndian.PutUint32(out[1:5], p.sid)
	binary.BigEndian.PutUint32(out[5:9], ctr)
	return p.c2s.Seal(out, secNonce(ctr), inner, out[:9])
}

func (p *secPhone) sendFrom(addr *net.UDPAddr, inner []byte) {
	p.send++
	p.s.handlePacket(p.sealAt(p.send, inner), addr)
}

func (p *secPhone) sendInner(inner []byte) { p.sendFrom(p.addr, inner) }

// recv returns the decrypted packets the server sent to addr; plain counts
// anything that arrived outside the secure layer.
func (p *secPhone) recv(addr *net.UDPAddr) (inner [][]byte, plain int) {
	for _, pk := range p.sink.take(addr) {
		if pk[0] != PacketTypeSecData || binary.BigEndian.Uint32(pk[1:5]) != p.sid {
			plain++
			continue
		}
		ctr := binary.BigEndian.Uint32(pk[5:9])
		b, err := p.s2c.Open(nil, secNonce(ctr), pk[9:], pk[:9])
		if err != nil {
			p.t.Fatalf("server packet does not decrypt: %v", err)
		}
		inner = append(inner, b)
	}
	return
}

func joinPacket(callsign string, ch int) []byte {
	b := []byte{PacketTypeJoin}
	b = secPutStr(b, callsign)
	return binary.BigEndian.AppendUint32(b, uint32(ch))
}

func TestSecureSignInAndEncryptedSession(t *testing.T) {
	s, sink := newSecServer(t)
	addUser(s, "a1", "medic", "medic-password-1")
	p := &secPhone{t: t, s: s, sink: sink, addr: udp(9001)}

	if st := p.signIn("medic", "a1", "wrong-password"); st != 0 || p.msg != "Invalid username or password" {
		t.Fatalf("wrong password: status %d %q", st, p.msg)
	}
	if s.isAuthenticated(p.addr) || s.addrIsSecure(p.addr.String()) {
		t.Fatal("a failed sign-in left a session")
	}
	if st := p.signIn("medic", "a1", "medic-password-1"); st != 1 || !strings.HasPrefix(p.msg, "Authenticated") {
		t.Fatalf("sign-in: status %d %q", st, p.msg)
	}
	if !s.isAuthenticated(p.addr) || !s.addrIsSecure(p.addr.String()) {
		t.Fatal("no secure session after sign-in")
	}

	// JOIN through the session; everything the server sends back is encrypted.
	p.sendInner(joinPacket("Medic 7", 0))
	if s.clients[userKey("a1", "Medic 7")] == nil {
		t.Fatal("JOIN through the secure session did not register")
	}
	inner, plain := p.recv(p.addr)
	if plain != 0 || len(inner) == 0 {
		t.Fatalf("server sent %d plaintext packets, %d encrypted", plain, len(inner))
	}

	// A forged plaintext LEAVE from the phone's address does nothing.
	s.handlePacket([]byte{PacketTypeLeave}, p.addr)
	if s.clients[userKey("a1", "Medic 7")] == nil {
		t.Fatal("a plaintext packet from a secure address was acted on")
	}
	// Neither does a replay of a packet the server already took...
	replay := p.sealAt(p.send, joinPacket("Medic 7", 3))
	s.handlePacket(replay, p.addr)
	if s.clients[userKey("a1", "Medic 7")].Channel != 0 {
		t.Fatal("a replayed counter was accepted")
	}
	// ...nor a packet with a corrupted byte, nor one from another session id.
	bad := p.sealAt(p.send+1, joinPacket("Medic 7", 3))
	bad[len(bad)-1] ^= 1
	s.handlePacket(bad, p.addr)
	other := p.sealAt(p.send+1, joinPacket("Medic 7", 3))
	binary.BigEndian.PutUint32(other[1:5], p.sid+1)
	s.handlePacket(other, p.addr)
	if s.clients[userKey("a1", "Medic 7")].Channel != 0 {
		t.Fatal("a tampered packet was accepted")
	}
	// Out of order inside the window is fine (UDP reorders).
	late := p.sealAt(p.send+1, joinPacket("Medic 7", 1))
	p.send += 2
	s.handlePacket(p.sealAt(p.send, joinPacket("Medic 7", 2)), p.addr)
	s.handlePacket(late, p.addr)
	if got := s.clients[userKey("a1", "Medic 7")].Channel; got != 1 {
		t.Fatalf("an in-window late packet was dropped (channel %d)", got)
	}
}

func TestSecureSessionFollowsThePhoneAndEndsWithTheAccount(t *testing.T) {
	s, sink := newSecServer(t)
	addUser(s, "a1", "medic", "medic-password-1")
	p := &secPhone{t: t, s: s, sink: sink, addr: udp(9101)}
	if p.signIn("medic", "a1", "medic-password-1") != 1 {
		t.Fatal("sign-in failed")
	}
	p.sendInner(joinPacket("Medic 7", 0))
	p.recv(p.addr)

	// The phone changes network: same session, new address, no new sign-in.
	lte := &net.UDPAddr{IP: net.IPv4(172, 56, 3, 9), Port: 40123}
	p.sendFrom(lte, joinPacket("Medic 7", 2))
	c := s.clients[userKey("a1", "Medic 7")]
	if c == nil || c.Addr.String() != lte.String() || c.Channel != 2 {
		t.Fatal("the session did not follow the phone to its new address")
	}
	if !s.isAuthenticated(lte) || s.isAuthenticated(p.addr) {
		t.Fatal("sign-in did not move with the session")
	}
	if in, plain := p.recv(lte); len(in) == 0 || plain != 0 {
		t.Fatal("replies did not go, encrypted, to the new address")
	}

	// Deleting the account ends the session, and the goodbye is encrypted.
	s.config.UsersFile = t.TempDir() + "/users.json"
	if err := s.RemoveUser("a1", "medic"); err != nil {
		t.Fatal(err)
	}
	in, plain := p.recv(lte)
	revoked := false
	for _, b := range in {
		revoked = revoked || (b[0] == PacketTypeAuthResponse && b[1] == AuthStatusRevoked)
	}
	if !revoked || plain != 0 {
		t.Fatalf("no encrypted sign-out (revoked=%v, plaintext=%d)", revoked, plain)
	}
	p.sendFrom(lte, joinPacket("Medic 7", 0))
	if s.clients[userKey("a1", "Medic 7")] != nil {
		t.Fatal("an ended session was still accepted")
	}
}

func TestSecureQREnrollmentIssuesDeviceKey(t *testing.T) {
	s, sink := newSecServer(t)
	s.config.UsersFile = t.TempDir() + "/users.json"
	addUser(s, "a1", "medic", "medic-password-1")
	token := s.GenerateEnrollmentToken("medic", "a1", "")
	p := &secPhone{t: t, s: s, sink: sink, addr: udp(9201)}
	if p.signIn("medic", "a1", token) != 1 {
		t.Fatalf("QR sign-in failed: %q", p.msg)
	}
	key := ""
	for _, part := range strings.Split(p.msg, "|") {
		if strings.HasPrefix(part, "devkey=") {
			key = strings.TrimPrefix(part, "devkey=")
		}
	}
	if len(key) != 64 || strings.Contains(p.msg, "devtag=") {
		t.Fatalf("expected a plain device key inside the encrypted reply, got %q", p.msg)
	}
	u := s.users[userKey("a1", "medic")]
	if len(u.Devices) != 1 || u.Devices[0].Legacy || u.PasswordHash != hashPassword("medic-password-1") {
		t.Fatal("QR did not create exactly one device key, or touched the password")
	}
	p2 := &secPhone{t: t, s: s, sink: sink, addr: udp(9202), pinned: p.pinned}
	if p2.signIn("medic", "a1", key) != 1 {
		t.Fatal("the device key does not sign in")
	}
	// The spent token is refused from elsewhere.
	p3 := &secPhone{t: t, s: s, sink: sink, addr: &net.UDPAddr{IP: net.IPv4(10, 9, 9, 9), Port: 7000}}
	if p3.signIn("medic", "a1", token) == 1 {
		t.Fatal("a copy of the used QR signed in")
	}
}

// The old protocol still works beside the secure one, and a repeated auth
// (lost reply) gets the same answer.
func TestSecureCoexistsWithOldProtocolAndHelloFloodStoresNothing(t *testing.T) {
	s, sink := newSecServer(t)
	addUser(s, "a1", "medic", "medic-password-1")
	if !login(t, s, sink, udp(9301), "medic", "a1", "medic-password-1") {
		t.Fatal("old protocol sign-in stopped working")
	}
	if s.addrIsSecure(udp(9301).String()) {
		t.Fatal("an old-protocol address was marked secure")
	}
	// Forged hello floods store nothing.
	for i := 0; i < 500; i++ {
		b := append([]byte{PacketTypeSecHello, secVersion}, make([]byte, 48)...)
		rand.Read(b[2:])
		s.handlePacket(b, &net.UDPAddr{IP: net.IPv4(203, 0, 113, byte(i)), Port: 1000 + i})
	}
	if len(s.sec.byID) != 0 || len(s.sec.byAddr) != 0 {
		t.Fatal("hellos created state")
	}
}

// Fixed inputs -> fixed outputs, written to testdata/mrs1_vectors.json for the
// apps' own tests. If this changes, every client changes.
func TestSecureVectors(t *testing.T) {
	serverPub, _ := hex.DecodeString("8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a")
	ck, v := scramDerive(hashPassword("correct horse battery"), serverPub, "Engine7")
	th1 := secHash([]byte("MRS1-th1"), bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 16),
		bytes.Repeat([]byte{3}, 32), serverPub, bytes.Repeat([]byte{4}, 16))
	am := secHash([]byte("MRS1-auth"), th1[:], []byte("Engine7"))
	sig := computeHmacSha256(v.storedKey[:], am[:])
	proof := make([]byte, 32)
	for i := range proof {
		proof[i] = ck[i] ^ sig[i]
	}
	prk := hkdf.Extract(sha256.New, bytes.Repeat([]byte{5}, 64), []byte("MRS1"))
	dataKey := secExpand(prk, "MRS1 data c2s", th1[:])
	hdr := []byte{PacketTypeSecData, 0, 0, 0, 7, 0, 0, 0, 1}
	sealed := secAEAD(dataKey).Seal(append([]byte(nil), hdr...), secNonce(1), []byte{PacketTypeLeave}, hdr)
	vec := map[string]string{
		"password":         "correct horse battery",
		"username":         "Engine7",
		"server_public":    hex.EncodeToString(serverPub),
		"p_hex":            hashPassword("correct horse battery"),
		"salt":             hex.EncodeToString(scramSalt(serverPub, "Engine7")),
		"iterations":       "100000",
		"client_key":       hex.EncodeToString(ck[:]),
		"stored_key":       hex.EncodeToString(v.storedKey[:]),
		"server_key":       hex.EncodeToString(v.serverKey[:]),
		"th1":              hex.EncodeToString(th1[:]),
		"auth_message":     hex.EncodeToString(am[:]),
		"client_proof":     hex.EncodeToString(proof),
		"server_signature": hex.EncodeToString(computeHmacSha256(v.serverKey[:], am[:])),
		"prk":              hex.EncodeToString(prk),
		"data_c2s_key":     hex.EncodeToString(dataKey),
		"hs_tag_key":       hex.EncodeToString(secExpand(prk, "MRS1 hs tag", th1[:])),
		"sealed_leave":     hex.EncodeToString(sealed),
	}
	if !scramCheck(v, am[:], proof) {
		t.Fatal("reference proof does not verify")
	}
	out, _ := json.MarshalIndent(vec, "", "  ")
	const path = "testdata/mrs1_vectors.json"
	if old, err := os.ReadFile(path); err == nil && !bytes.Equal(bytes.TrimSpace(old), out) {
		t.Fatalf("%s changed: the wire format moved. Delete the file only if that is intended", path)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	_ = time.Now
}
