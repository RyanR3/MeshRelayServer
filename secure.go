// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// MeshRelay secure protocol, version 1 ("MRS1"). The wire format is specified
// in docs/SECURE_PROTOCOL.md; every client implements the same thing and is
// checked against the vectors in testdata/mrs1_vectors.json.
//
// What it gives, that the old challenge-response sign-in did not:
//   - The phone checks it is talking to THIS server (a permanent identity key,
//     carried in the enrollment QR or remembered on first connect) before it
//     proves anything about itself.
//   - Nothing useful is visible on the wire: the handshake is under a fresh
//     key agreement, and every packet after it is encrypted and authenticated
//     with a per-session key and a counter. A forged or replayed packet fails.
//   - A session is its key and number, not an ip:port: a phone that changes
//     network keeps its session, and nobody else can speak for its address.
//   - The proof of the password uses a salted, slow-to-guess verifier, so what
//     the server holds for the secure sign-in is not itself a login.
//
// The old protocol keeps working beside it until an agency requires the
// secure one.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

const (
	PacketTypeSecHello      byte = 0x40 // phone -> server
	PacketTypeSecHelloReply byte = 0x41 // server -> phone
	PacketTypeSecAuth       byte = 0x42 // phone -> server
	PacketTypeSecAuthReply  byte = 0x43 // server -> phone
	PacketTypeSecData       byte = 0x44 // both ways: an encrypted packet
	PacketTypeSecRetry      byte = 0x45 // server -> phone: come back with this cookie

	secVersion      byte = 1
	scramIterations      = 100000
	secHelloSlot         = 10 * time.Second // lifetime of the server's stateless hello
	secReplayWindow      = 64
	secClosingLife       = 5 * time.Second
	// More hellos per second than this and newcomers must return a cookie
	// first, so forged-source floods cannot make the server do key agreement.
	secHelloBurst = 300

	identityKeyFile = "server_identity.key"
)

// --- identity ----------------------------------------------------------------

// loadOrCreateIdentity reads the server's permanent X25519 key, creating it on
// first run. The public half goes in enrollment QR codes; phones remember it.
func (s *Server) loadOrCreateIdentity() error {
	path := identityKeyFile
	if b, err := os.ReadFile(path); err == nil {
		raw, derr := hex.DecodeString(strings.TrimSpace(string(b)))
		if derr == nil && len(raw) == 32 {
			k, kerr := ecdh.X25519().NewPrivateKey(raw)
			if kerr == nil {
				s.identity = k
				return nil
			}
		}
		return errors.New("server identity key file is damaged: " + path)
	} else if !os.IsNotExist(err) {
		return err
	}
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, []byte(hex.EncodeToString(k.Bytes())+"\n")); err != nil { // 0600
		return err
	}
	s.identity = k
	log.Printf("Created the server identity key (%s). Fingerprint %s", path, s.identityFingerprint())
	return nil
}

func (s *Server) identityPublic() []byte {
	if s.identity == nil {
		return nil
	}
	return s.identity.PublicKey().Bytes()
}

// identityFingerprint is what the dashboard shows: SHA-256 of the public key.
func (s *Server) identityFingerprint() string {
	pub := s.identityPublic()
	if pub == nil {
		return ""
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// --- verifier (SCRAM-style) ----------------------------------------------------
//
// The credential a phone holds is a password or a device key. Both sides start
// from P = lowercase hex SHA-256 of it (what users.json already stores), then:
//
//	salt           = SHA-256("MRS1-salt" | server public key | username)
//	SaltedPassword = PBKDF2-HMAC-SHA256(P, salt, 100000, 32)
//	ClientKey      = HMAC(SaltedPassword, "Client Key")
//	StoredKey      = SHA-256(ClientKey)
//	ServerKey      = HMAC(SaltedPassword, "Server Key")
//
// The phone proves it holds ClientKey; the server proves it holds ServerKey.
// The server needs only StoredKey and ServerKey.

type scramVerifier struct {
	storedKey [32]byte
	serverKey [32]byte
}

func scramSalt(serverPub []byte, username string) []byte {
	h := sha256.New()
	h.Write([]byte("MRS1-salt"))
	h.Write(serverPub)
	h.Write([]byte(username))
	return h.Sum(nil)
}

func scramDerive(hashHex string, serverPub []byte, username string) (clientKey [32]byte, v scramVerifier) {
	salted := pbkdf2.Key([]byte(hashHex), scramSalt(serverPub, username), scramIterations, 32, sha256.New)
	copy(clientKey[:], computeHmacSha256(salted, []byte("Client Key")))
	v.storedKey = sha256.Sum256(clientKey[:])
	copy(v.serverKey[:], computeHmacSha256(salted, []byte("Server Key")))
	return
}

// verifierFor returns the verifier for a stored credential hash, computing it
// once (the derivation is deliberately slow) and remembering it. Never call
// it with s.mu held.
func (s *Server) verifierFor(hashHex, username string) scramVerifier {
	key := hashHex + "|" + username
	s.verifierMu.Lock()
	if v, ok := s.verifiers[key]; ok {
		s.verifierMu.Unlock()
		return v
	}
	s.verifierMu.Unlock()
	_, v := scramDerive(hashHex, s.identityPublic(), username)
	s.verifierMu.Lock()
	if s.verifiers == nil {
		s.verifiers = map[string]scramVerifier{}
	}
	if len(s.verifiers) > 100000 {
		s.verifiers = map[string]scramVerifier{} // bounded; rebuilt on demand
	}
	s.verifiers[key] = v
	s.verifierMu.Unlock()
	return v
}

// scramCheck: does proof show knowledge of the ClientKey behind v, for this
// handshake (authMessage)?  proof = ClientKey XOR HMAC(StoredKey, authMessage).
func scramCheck(v scramVerifier, authMessage, proof []byte) bool {
	if len(proof) != 32 {
		return false
	}
	sig := computeHmacSha256(v.storedKey[:], authMessage)
	var ck [32]byte
	for i := range ck {
		ck[i] = proof[i] ^ sig[i]
	}
	got := sha256.Sum256(ck[:])
	return subtle.ConstantTimeCompare(got[:], v.storedKey[:]) == 1
}

// --- handshake keys -----------------------------------------------------------

func secHash(parts ...[]byte) [32]byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func secExpand(prk []byte, label string, th []byte) []byte {
	out := make([]byte, 32)
	r := hkdf.Expand(sha256.New, prk, append([]byte(label), th...))
	if _, err := io.ReadFull(r, out); err != nil {
		panic(err)
	}
	return out
}

func secAEAD(key []byte) cipher.AEAD {
	b, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		panic(err)
	}
	return g
}

// secHelloKeys is the server's side of a hello. It keeps NO state: the
// ephemeral key and nonce are derived from a server secret, the phone's hello,
// its address and a time slot, and derived again when the phone's auth
// arrives. A flood of hellos therefore stores nothing.
type secHelloKeys struct {
	slot   byte
	eS     *ecdh.PrivateKey
	sNonce [16]byte
	th1    [32]byte
	prk    []byte
	hsC2S  []byte
	hsS2C  []byte
	tagKey []byte
}

func (s *Server) secDeriveHello(eC, cNonce []byte, addr string, slot int64) (*secHelloKeys, error) {
	mac := hmac.New(sha256.New, challengeKey())
	mac.Write([]byte("MRS1-hello"))
	mac.Write(eC)
	mac.Write(cNonce)
	mac.Write([]byte(addr))
	var sb [8]byte
	binary.BigEndian.PutUint64(sb[:], uint64(slot))
	mac.Write(sb[:])
	seed := mac.Sum(nil)

	eS, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		return nil, err
	}
	clientEph, err := ecdh.X25519().NewPublicKey(eC)
	if err != nil {
		return nil, err
	}
	dhEE, err := eS.ECDH(clientEph)
	if err != nil {
		return nil, err
	}
	dhSE, err := s.identity.ECDH(clientEph)
	if err != nil {
		return nil, err
	}
	k := &secHelloKeys{slot: byte(slot), eS: eS}
	copy(k.sNonce[:], computeHmacSha256(challengeKey(), append([]byte("MRS1-snonce"), seed...)))
	k.th1 = secHash([]byte("MRS1-th1"), eC, cNonce, eS.PublicKey().Bytes(), s.identityPublic(), k.sNonce[:])
	k.prk = hkdf.Extract(sha256.New, append(dhEE, dhSE...), []byte("MRS1"))
	k.hsC2S = secExpand(k.prk, "MRS1 hs c2s", k.th1[:])
	k.hsS2C = secExpand(k.prk, "MRS1 hs s2c", k.th1[:])
	k.tagKey = secExpand(k.prk, "MRS1 hs tag", k.th1[:])
	return k, nil
}

func secCookie(addr string, slot int64) []byte {
	mac := hmac.New(sha256.New, challengeKey())
	mac.Write([]byte("MRS1-cookie"))
	mac.Write([]byte(addr))
	var sb [8]byte
	binary.BigEndian.PutUint64(sb[:], uint64(slot))
	mac.Write(sb[:])
	return mac.Sum(nil)[:16]
}

// --- sessions -----------------------------------------------------------------

type secSession struct {
	id   uint32
	c2s  cipher.AEAD
	s2c  cipher.AEAD
	send uint32 // counter of the last packet sent (atomic)

	mu       sync.Mutex
	addr     *net.UDPAddr
	recvHigh uint32
	recvMask uint64
	closing  time.Time // set when the session was ended; kept briefly to deliver the goodbye
	hsHash   [32]byte
	reply    []byte // the auth reply, re-sent if the phone repeats its auth
	created  time.Time
}

type secTable struct {
	mu     sync.RWMutex
	byID   map[uint32]*secSession
	byAddr map[string]*secSession
	byHs   map[[32]byte]*secSession

	count      int64 // sessions in the table (atomic)
	helloCount int64 // hellos in the current second (atomic)
	helloSec   int64
}

// live reports whether any secure session exists (a cheap check for the
// packet paths, which run for every audio frame).
func (t *secTable) live() bool { return atomic.LoadInt64(&t.count) > 0 }

func (t *secTable) init() {
	if t.byID == nil {
		t.byID = map[uint32]*secSession{}
		t.byAddr = map[string]*secSession{}
		t.byHs = map[[32]byte]*secSession{}
	}
}

func secNonce(ctr uint32) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint32(n[8:], ctr)
	return n
}

// seal wraps one packet for this session.
func (ss *secSession) seal(inner []byte) []byte {
	ctr := atomic.AddUint32(&ss.send, 1)
	out := make([]byte, 9, 9+len(inner)+16)
	out[0] = PacketTypeSecData
	binary.BigEndian.PutUint32(out[1:5], ss.id)
	binary.BigEndian.PutUint32(out[5:9], ctr)
	return ss.s2c.Seal(out, secNonce(ctr), inner, out[:9])
}

// open checks and decrypts one packet; nil when it is forged, replayed or old.
func (ss *secSession) open(pkt []byte) []byte {
	if len(pkt) < 9+16 {
		return nil
	}
	ctr := binary.BigEndian.Uint32(pkt[5:9])
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if !ss.closing.IsZero() || ctr == 0 {
		return nil
	}
	if ctr <= ss.recvHigh {
		d := ss.recvHigh - ctr
		if d >= secReplayWindow || ss.recvMask&(1<<d) != 0 {
			return nil // too old, or seen before
		}
	}
	inner, err := ss.c2s.Open(nil, secNonce(ctr), pkt[9:], pkt[:9])
	if err != nil {
		return nil
	}
	if ctr > ss.recvHigh {
		shift := ctr - ss.recvHigh
		if shift >= secReplayWindow {
			ss.recvMask = 0
		} else {
			ss.recvMask <<= shift
		}
		ss.recvMask |= 1
		ss.recvHigh = ctr
	} else {
		ss.recvMask |= 1 << (ss.recvHigh - ctr)
	}
	return inner
}

// secSessionForAddr: the live or closing session bound to addr, if any.
func (s *Server) secSessionForAddr(addr string) *secSession {
	s.sec.mu.RLock()
	ss := s.sec.byAddr[addr]
	s.sec.mu.RUnlock()
	return ss
}

// addrIsSecure: packets to and from this address must use the secure layer.
func (s *Server) addrIsSecure(addr string) bool { return s.secSessionForAddr(addr) != nil }

// secCloseAddr ends the session at addr. It stays in the table for a few
// seconds so the goodbye (a revoke) still goes out encrypted.
func (s *Server) secCloseAddr(addr string) {
	if ss := s.secSessionForAddr(addr); ss != nil {
		ss.mu.Lock()
		if ss.closing.IsZero() {
			ss.closing = time.Now()
		}
		ss.mu.Unlock()
	}
}

// secDropAddr removes the session at addr at once (the address switched to
// the old protocol).
func (s *Server) secDropAddr(addr string) {
	s.sec.mu.Lock()
	if ss := s.sec.byAddr[addr]; ss != nil {
		delete(s.sec.byAddr, addr)
		delete(s.sec.byID, ss.id)
		delete(s.sec.byHs, ss.hsHash)
		atomic.StoreInt64(&s.sec.count, int64(len(s.sec.byID)))
	}
	s.sec.mu.Unlock()
}

// secReap drops sessions that were ended, and sessions whose address is no
// longer signed in. Called from the cleanup tick.
func (s *Server) secReap(now time.Time) {
	s.mu.RLock()
	signedIn := make(map[string]bool, len(s.authenticatedAddrs))
	for a := range s.authenticatedAddrs {
		signedIn[a] = true
	}
	s.mu.RUnlock()
	s.sec.mu.Lock()
	for id, ss := range s.sec.byID {
		ss.mu.Lock()
		addr := ss.addr.String()
		gone := (!ss.closing.IsZero() && now.Sub(ss.closing) > secClosingLife) ||
			(ss.closing.IsZero() && !signedIn[addr] && now.Sub(ss.created) > time.Minute)
		ss.mu.Unlock()
		if gone {
			delete(s.sec.byID, id)
			delete(s.sec.byHs, ss.hsHash)
			if s.sec.byAddr[addr] == ss {
				delete(s.sec.byAddr, addr)
			}
		}
	}
	atomic.StoreInt64(&s.sec.count, int64(len(s.sec.byID)))
	s.sec.mu.Unlock()
}

// --- packet entry points --------------------------------------------------------

// handleSecurePacket takes every 0x40-0x45 packet.
func (s *Server) handleSecurePacket(data []byte, addr *net.UDPAddr) {
	if s.identity == nil {
		return
	}
	switch data[0] {
	case PacketTypeSecHello:
		s.handleSecHello(data, addr)
	case PacketTypeSecAuth:
		s.handleSecAuth(data, addr)
	case PacketTypeSecData:
		s.handleSecData(data, addr)
	}
}

// hello: 0x40 | ver | eC[32] | cNonce[16] | [cookie[16]]
func (s *Server) handleSecHello(data []byte, addr *net.UDPAddr) {
	if len(data) < 2+32+16 || data[1] != secVersion {
		return
	}
	if locked, _, _ := s.pttAuthLimit.blocked(addr.IP.String()); locked {
		return
	}
	now := time.Now()
	slot := now.UnixNano() / int64(secHelloSlot)
	// Under a flood, key agreement only for addresses that proved they can
	// receive (returned the cookie).
	sec := now.Unix()
	if atomic.SwapInt64(&s.sec.helloSec, sec) != sec {
		atomic.StoreInt64(&s.sec.helloCount, 0)
	}
	if atomic.AddInt64(&s.sec.helloCount, 1) > secHelloBurst {
		ok := false
		if len(data) >= 2+32+16+16 {
			got := data[50:66]
			ok = hmac.Equal(got, secCookie(addr.String(), slot)) || hmac.Equal(got, secCookie(addr.String(), slot-1))
		}
		if !ok {
			out := append([]byte{PacketTypeSecRetry, secVersion}, secCookie(addr.String(), slot)...)
			s.sendRaw(out, addr)
			return
		}
	}
	k, err := s.secDeriveHello(data[2:34], data[34:50], addr.String(), slot)
	if err != nil {
		return
	}
	// reply: 0x41 | ver | slot | eS[32] | S[32] | sNonce[16] | tag[32]
	out := make([]byte, 0, 3+32+32+16+32)
	out = append(out, PacketTypeSecHelloReply, secVersion, k.slot)
	out = append(out, k.eS.PublicKey().Bytes()...)
	out = append(out, s.identityPublic()...)
	out = append(out, k.sNonce[:]...)
	out = append(out, computeHmacSha256(k.tagKey, k.th1[:])...)
	s.sendRaw(out, addr)
}

// auth: 0x42 | ver | slot | eC[32] | cNonce[16] | nonce[12] | ciphertext
// plaintext: str username | str agency | flags | proof[32] | str client
func (s *Server) handleSecAuth(data []byte, addr *net.UDPAddr) {
	const head = 3 + 32 + 16 + 12
	if len(data) < head+16 || data[1] != secVersion {
		return
	}
	now := time.Now()
	cur := now.UnixNano() / int64(secHelloSlot)
	var slot int64 = -1
	for _, c := range []int64{cur, cur - 1} {
		if byte(c) == data[2] {
			slot = c
		}
	}
	if slot < 0 {
		return // hello too old: the phone starts again
	}
	eC, cNonce, nonce, ct := data[3:35], data[35:51], data[51:63], data[63:]
	k, err := s.secDeriveHello(eC, cNonce, addr.String(), slot)
	if err != nil {
		return
	}
	plain, err := secAEAD(k.hsC2S).Open(nil, nonce, ct, k.th1[:])
	if err != nil {
		return // not made for this hello: silently dropped
	}
	ctHash := sha256.Sum256(ct)
	th3 := secHash([]byte("MRS1-th3"), k.th1[:], nonce, ctHash[:])

	// The phone repeated its auth (our reply was lost): same answer again.
	s.sec.mu.RLock()
	prev := s.sec.byHs[th3]
	s.sec.mu.RUnlock()
	if prev != nil {
		s.sendRaw(prev.reply, addr)
		return
	}

	reply := func(status byte, sid uint32, serverSig []byte, msg string) []byte {
		body := make([]byte, 0, 1+4+32+2+len(msg))
		body = append(body, status)
		body = binary.BigEndian.AppendUint32(body, sid)
		if serverSig == nil {
			serverSig = make([]byte, 32)
		}
		body = append(body, serverSig...)
		body = binary.BigEndian.AppendUint16(body, uint16(len(msg)))
		body = append(body, msg...)
		n := make([]byte, 12)
		rand.Read(n)
		out := append([]byte{PacketTypeSecAuthReply, secVersion}, n...)
		return secAEAD(k.hsS2C).Seal(out, n, body, th3[:])
	}
	fail := func(msg string) { s.sendRaw(reply(0, 0, nil, msg), addr) }

	r := bytes.NewReader(plain)
	username, e1 := secReadStr(r)
	agency, e2 := secReadStr(r)
	flags, e3 := r.ReadByte()
	proof := make([]byte, 32)
	_, e4 := io.ReadFull(r, proof)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || username == "" {
		return
	}
	_ = flags
	authMessage := secHash([]byte("MRS1-auth"), k.th1[:], []byte(username))

	// Who could this be, and what could they hold? (Read under the lock;
	// the slow verifier work happens outside it.)
	type candCred struct {
		user     *UserAccount
		hash     string
		token    string
		deviceID string
	}
	var creds []candCred
	var accts []string
	s.mu.RLock()
	var candidates []*UserAccount
	if agency != "" {
		if u := s.users[userKey(agency, username)]; u != nil {
			candidates = append(candidates, u)
		}
	}
	if len(candidates) == 0 {
		candidates = s.findUsersByUsername(username)
	}
	for _, u := range candidates {
		accts = append(accts, strings.ToLower(userKey(u.AgencyID, u.Username)))
		if u.PasswordHash != "" {
			creds = append(creds, candCred{user: u, hash: u.PasswordHash})
		}
		for _, d := range u.Devices {
			creds = append(creds, candCred{user: u, hash: d.Hash, deviceID: d.ID})
		}
		for t, et := range s.enrollmentTokens {
			if et.Username == u.Username && et.AgencyID == u.AgencyID && enrollTokenUsable(et, addr, now) {
				creds = append(creds, candCred{user: u, hash: hashPassword(et.Token), token: t})
			}
		}
	}
	s.mu.RUnlock()
	if len(accts) == 0 {
		accts = []string{"?:" + strings.ToLower(strings.TrimSpace(username))}
	}
	ip := addr.IP.String()
	if locked, left, what := s.pttAuthLimit.blocked(ip, accts...); locked {
		log.Printf("Auth refused: %s locked for %s more (too many wrong passwords) — user %s from %s",
			what, left.Round(time.Second), username, addr)
		fail("Too many attempts — try again in a few minutes")
		return
	}

	var match *candCred
	var matchV scramVerifier
	for i := range creds {
		v := s.verifierFor(creds[i].hash, username)
		if scramCheck(v, authMessage[:], proof) {
			match, matchV = &creds[i], v
			break
		}
	}
	if match == nil {
		if len(candidates) == 0 {
			log.Printf("Auth failed: unknown user %s from %s (secure)", username, addr)
		} else {
			log.Printf("Auth failed: wrong password for user %s from %s (secure)", username, addr)
		}
		if s.pttAuthLimit.failed(ip, accts...) {
			log.Printf("Auth lockout: too many wrong passwords (user %s from %s)", username, addr)
		}
		fail("Invalid username or password")
		return
	}

	s.mu.Lock()
	// The account may have changed while the lock was released.
	user := s.users[userKey(match.user.AgencyID, match.user.Username)]
	stillGood := user == match.user
	if stillGood && match.token != "" {
		et := s.enrollmentTokens[match.token]
		stillGood = et != nil && enrollTokenUsable(et, addr, now)
	}
	if stillGood && match.deviceID != "" {
		stillGood = false
		for _, d := range user.Devices {
			stillGood = stillGood || (d.ID == match.deviceID && d.Hash == match.hash)
		}
	}
	if stillGood && match.token == "" && match.deviceID == "" {
		stillGood = user.PasswordHash == match.hash
	}
	if !stillGood {
		s.mu.Unlock()
		fail("Invalid username or password")
		return
	}
	msg, err := s.completeSignInLocked(user, match.token, match.deviceID, addr, nil)
	s.mu.Unlock()
	if err != nil {
		fail("Server error — try again")
		return
	}
	s.pttAuthLimit.succeeded(ip, strings.ToLower(userKey(user.AgencyID, user.Username)))

	// The session: keys from the same agreement, bound to this exact exchange.
	ss := &secSession{
		c2s:     secAEAD(secExpand(k.prk, "MRS1 data c2s", th3[:])),
		s2c:     secAEAD(secExpand(k.prk, "MRS1 data s2c", th3[:])),
		addr:    addr,
		hsHash:  th3,
		created: now,
	}
	s.sec.mu.Lock()
	s.sec.init()
	for {
		var b [4]byte
		rand.Read(b[:])
		ss.id = binary.BigEndian.Uint32(b[:])
		if _, taken := s.sec.byID[ss.id]; ss.id != 0 && !taken {
			break
		}
	}
	ss.reply = reply(1, ss.id, computeHmacSha256(matchV.serverKey[:], authMessage[:]), msg)
	if old := s.sec.byAddr[addr.String()]; old != nil {
		delete(s.sec.byID, old.id)
		delete(s.sec.byHs, old.hsHash)
	}
	s.sec.byID[ss.id] = ss
	s.sec.byAddr[addr.String()] = ss
	s.sec.byHs[th3] = ss
	atomic.StoreInt64(&s.sec.count, int64(len(s.sec.byID)))
	s.sec.mu.Unlock()
	s.sendRaw(ss.reply, addr)
}

// data: 0x44 | session[4] | counter[4] | ciphertext
func (s *Server) handleSecData(data []byte, addr *net.UDPAddr) {
	if len(data) < 9+16 {
		return
	}
	s.sec.mu.RLock()
	ss := s.sec.byID[binary.BigEndian.Uint32(data[1:5])]
	s.sec.mu.RUnlock()
	if ss == nil {
		return
	}
	inner := ss.open(data)
	if inner == nil || len(inner) == 0 {
		return
	}
	// A genuine packet from a new address: the phone changed network. Its
	// sign-in and its client record follow it.
	ss.mu.Lock()
	old := ss.addr
	moved := old.String() != addr.String()
	if moved {
		ss.addr = addr
	}
	ss.mu.Unlock()
	if moved {
		s.secMoveAddr(ss, old, addr)
	}
	if inner[0] >= PacketTypeSecHello && inner[0] <= PacketTypeSecRetry {
		return // no nesting
	}
	s.handlePlainPacket(inner, addr)
}

// secMoveAddr carries a session's sign-in from one address to another.
func (s *Server) secMoveAddr(ss *secSession, from, to *net.UDPAddr) {
	f, t := from.String(), to.String()
	s.sec.mu.Lock()
	if s.sec.byAddr[f] == ss {
		delete(s.sec.byAddr, f)
	}
	s.sec.byAddr[t] = ss
	s.sec.mu.Unlock()

	s.mu.Lock()
	if ag, ok := s.authenticatedAddrs[f]; ok {
		s.authenticatedAddrs[t] = ag
		delete(s.authenticatedAddrs, f)
	}
	if a, ok := s.authAccounts[f]; ok {
		s.authAccounts[t] = a
		delete(s.authAccounts, f)
	}
	if a, ok := s.authSessions[f]; ok {
		s.authSessions[t] = a
		delete(s.authSessions, f)
	}
	for _, c := range s.clients {
		if c.Addr != nil && c.Addr.String() == f {
			c.Addr = to
		}
	}
	s.mu.Unlock()
	log.Printf("Secure session %08x moved %s -> %s", ss.id, f, t)
}

func secReadStr(r *bytes.Reader) (string, error) {
	var n uint16
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return "", err
	}
	if int(n) > r.Len() || n > 512 {
		return "", errors.New("bad length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}
