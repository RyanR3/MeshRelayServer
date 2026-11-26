// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	mrand "math/rand"
	"net"
	"testing"
	"time"
)

// challengeReqBytes is what every app sends today: 0x0D | str name | [str agency].
func challengeReqBytes(name, agency string) []byte {
	b := []byte{PacketTypeAuthChallengeReq}
	b = binary.BigEndian.AppendUint16(b, uint16(len(name)))
	b = append(b, name...)
	if agency != "" {
		b = binary.BigEndian.AppendUint16(b, uint16(len(agency)))
		b = append(b, agency...)
	}
	return b
}

// paddedChallengeReq is what a future app should send: name, agency (may be
// empty) and zero padding to at least the 33-byte reply.
func paddedChallengeReq(name, agency string) []byte {
	b := []byte{PacketTypeAuthChallengeReq}
	b = binary.BigEndian.AppendUint16(b, uint16(len(name)))
	b = append(b, name...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(agency)))
	b = append(b, agency...)
	for len(b) < challengeReplyLen {
		b = append(b, 0)
	}
	return b
}

func shortHello() []byte {
	b := make([]byte, 2+32+16)
	b[0], b[1] = PacketTypeSecHello, secVersion
	eph, _ := ecdh.X25519().GenerateKey(rand.Reader)
	copy(b[2:34], eph.PublicKey().Bytes())
	rand.Read(b[34:50])
	return b
}

// fakeClock drives the reply budget in simulated time.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func sentBytes(pkts [][]byte) (n, count int) {
	for _, p := range pkts {
		n += len(p)
	}
	return n, len(pkts)
}

// A forged flood of short challenge requests "from" one victim: the victim
// receives at most the per-source budget, not one reply per request.
func TestForgedChallengeFloodAtOneVictimIsBounded(t *testing.T) {
	s, sink := newSecTestServer(t)
	clk := &fakeClock{t: time.Now()}
	s.smallReplies.now = clk.now
	victim := net.IPv4(192, 0, 2, 77)
	req := challengeReqBytes("a", "") // 4 bytes, the reviewer's example
	sentIn, out, replies := 0, 0, 0
	const seconds, perSecond = 10, 1000
	for sec := 0; sec < seconds; sec++ {
		for i := 0; i < perSecond; i++ {
			a := &net.UDPAddr{IP: victim, Port: 1024 + i}
			s.handlePacket(req, a)
			sentIn += len(req)
			n, c := sentBytes(sink.take(a))
			out += n
			replies += c
		}
		clk.advance(time.Second)
	}
	maxReplies := smallReplyPerSrcBurst + int(smallReplyPerSrcRate*seconds)
	if replies > maxReplies {
		t.Fatalf("victim got %d replies to %d forged requests; budget is %d", replies, seconds*perSecond, maxReplies)
	}
	t.Logf("forged %d bytes in, %d bytes reflected at the victim (%d replies)", sentIn, out, replies)
	if out > sentIn/10 {
		t.Fatalf("reflected %d bytes for %d forged: still an amplifier", out, sentIn)
	}
}

// Forged requests from random sources: the whole server's reply volume is
// bounded by the global budget.
func TestForgedChallengeFloodFromRandomSourcesIsBounded(t *testing.T) {
	s, sink := newSecTestServer(t)
	clk := &fakeClock{t: time.Now()}
	s.smallReplies.now = clk.now
	rng := mrand.New(mrand.NewSource(1))
	req := challengeReqBytes("a", "")
	const seconds, perSecond = 5, 5000
	replies := 0
	for sec := 0; sec < seconds; sec++ {
		for i := 0; i < perSecond; i++ {
			ip := net.IPv4(byte(rng.Intn(223)+1), byte(rng.Intn(256)), byte(rng.Intn(256)), byte(rng.Intn(256)))
			a := &net.UDPAddr{IP: ip, Port: 40000}
			s.handlePacket(req, a)
			replies += len(sink.take(a))
		}
		clk.advance(time.Second)
	}
	maxReplies := smallReplyGlobalBurst + int(smallReplyGlobalRate*seconds)
	if replies > maxReplies {
		t.Fatalf("server sent %d replies to %d forged requests; global budget is %d", replies, seconds*perSecond, maxReplies)
	}
	t.Logf("%d forged requests from random sources, %d answered (budget %d)", seconds*perSecond, replies, maxReplies)
}

// A forged hello flood at one victim: past the budget the victim only gets
// the 18-byte retry, smaller than the 50-byte hello.
func TestForgedHelloFloodIsBoundedAndRetryIsSmaller(t *testing.T) {
	s, sink := newSecServer(t)
	clk := &fakeClock{t: time.Now()}
	s.smallReplies.now = clk.now
	victim := net.IPv4(192, 0, 2, 78)
	full, retries := 0, 0
	for i := 0; i < 200; i++ {
		a := &net.UDPAddr{IP: victim, Port: 2000 + i}
		h := shortHello()
		s.handlePacket(h, a)
		for _, p := range sink.take(a) {
			switch p[0] {
			case PacketTypeSecHelloReply:
				full++
			case PacketTypeSecRetry:
				retries++
				if len(p) > len(h) {
					t.Fatalf("retry (%d bytes) larger than the hello (%d)", len(p), len(h))
				}
			}
		}
	}
	if full > smallReplyPerSrcBurst {
		t.Fatalf("%d full hello replies to one victim; budget %d", full, smallReplyPerSrcBurst)
	}
	if full+retries != 200 || retries == 0 {
		t.Fatalf("full=%d retries=%d", full, retries)
	}
}

// Past the budget, a hello that returns the cookie (the sender receives at
// that address) and a padded hello are still answered in full; so is a
// padded challenge request.
func TestPaddedRequestsAndCookiesBypassTheBudget(t *testing.T) {
	s, sink := newSecServer(t)
	clk := &fakeClock{t: time.Now()}
	s.smallReplies.now = clk.now
	ip := net.IPv4(198, 51, 100, 9)
	a := &net.UDPAddr{IP: ip, Port: 5555}
	// Spend this address's budget.
	for i := 0; i < smallReplyPerSrcBurst*2; i++ {
		s.handlePacket(challengeReqBytes("medic", "a1"), a)
	}
	sink.take(a)
	s.handlePacket(challengeReqBytes("medic", "a1"), a)
	if n := len(sink.take(a)); n != 0 {
		t.Fatal("short request answered past the budget")
	}
	s.handlePacket(paddedChallengeReq("medic", "a1"), a)
	got := sink.take(a)
	if len(got) != 1 || got[0][0] != PacketTypeAuthChallenge || len(got[0]) != challengeReplyLen {
		t.Fatalf("padded challenge request not answered: %v", got)
	}

	// Hello: short -> retry; same hello with the cookie -> full reply.
	h := shortHello()
	s.handlePacket(h, a)
	var cookie []byte
	for _, p := range sink.take(a) {
		if p[0] == PacketTypeSecRetry {
			cookie = p[2:18]
		}
	}
	if cookie == nil {
		t.Fatal("no retry past the budget")
	}
	s.handlePacket(append(append([]byte(nil), h...), cookie...), a)
	if n := countFirst(sink.take(a), PacketTypeSecHelloReply); n != 1 {
		t.Fatal("hello with a valid cookie not answered")
	}
	// Padded hello: zero cookie field, padded to the reply size.
	ph := append(shortHello(), make([]byte, secHelloReplyLen-50)...)
	s.handlePacket(ph, a)
	if n := countFirst(sink.take(a), PacketTypeSecHelloReply); n != 1 {
		t.Fatal("padded hello not answered")
	}
	// A wrong cookie is no proof.
	bad := append(shortHello(), make([]byte, 16)...)
	s.handlePacket(bad, a)
	if n := countFirst(sink.take(a), PacketTypeSecHelloReply); n != 0 {
		t.Fatal("hello with a zero cookie treated as proven")
	}
}

func countFirst(pkts [][]byte, t byte) int {
	n := 0
	for _, p := range pkts {
		if len(p) > 0 && p[0] == t {
			n++
		}
	}
	return n
}

// A reconnect storm (every phone at once after a restart) is not slowed: 400
// phones from different addresses, each asking twice, all get a challenge and
// sign in; and 60 phones behind one carrier NAT address all sign in when they
// retry a few seconds apart, as the apps do.
func TestReconnectStormSignsInUnhindered(t *testing.T) {
	s, sink := newSecTestServer(t)
	addUser(s, "a1", "medic", "medic-password-1")
	for i := 0; i < 400; i++ {
		a := &net.UDPAddr{IP: net.IPv4(100, 64, byte(i/250), byte(i%250+1)), Port: 30000}
		s.handlePacket(challengeReqBytes("medic", "a1"), a) // the app's second connection
		sink.take(a)
		if !login(t, s, sink, a, "medic", "a1", "medic-password-1") {
			t.Fatalf("phone %d refused during the storm", i)
		}
	}
	// Behind one NAT address: the first ones go straight in, the rest when
	// they retry (the app retries the challenge every 2 s).
	clk := &fakeClock{t: time.Now()}
	s.smallReplies.now = clk.now
	nat := net.IPv4(100, 70, 0, 1)
	waiting := map[int]bool{}
	for i := 0; i < 60; i++ {
		waiting[i] = true
	}
	for round := 0; round < 30 && len(waiting) > 0; round++ {
		for i := range waiting {
			a := &net.UDPAddr{IP: nat, Port: 20000 + i}
			sink.take(a)
			s.handlePacket(challengeReqBytes("medic", "a1"), a)
			if countFirst(sink.take(a), PacketTypeAuthChallenge) == 1 {
				delete(waiting, i)
			}
		}
		clk.advance(2 * time.Second)
	}
	if len(waiting) > 0 {
		t.Fatalf("%d phones behind one NAT never got a challenge", len(waiting))
	}
}

// Every packet type from an address that has not signed in: no reply larger
// than the request, except the two budgeted sign-in replies.
func TestNoUnauthenticatedReplyLargerThanRequest(t *testing.T) {
	s, sink := newSecServer(t)
	addUser(s, "a1", "medic", "medic-password-1")
	rng := mrand.New(mrand.NewSource(7))
	sizes := []int{1, 2, 3, 4, 6, 8, 12, 16, 24, 40, 64}
	for typ := 0; typ < 256; typ++ {
		for _, n := range sizes {
			for trial := 0; trial < 4; trial++ {
				pkt := make([]byte, n)
				rng.Read(pkt)
				pkt[0] = byte(typ)
				a := &net.UDPAddr{IP: net.IPv4(203, 0, 113, byte(rng.Intn(250)+1)), Port: 10000 + typ}
				s.handlePacket(pkt, a)
				for addr, pk := range sinkAll(sink) {
					for _, p := range pk {
						if len(p) <= len(pkt) {
							continue
						}
						if (typ == int(PacketTypeAuthChallengeReq) && p[0] == PacketTypeAuthChallenge) ||
							(typ == int(PacketTypeSecHello) && p[0] == PacketTypeSecHelloReply) {
							continue // budgeted
						}
						t.Fatalf("type 0x%02x, %d-byte request from %v: %d-byte reply 0x%02x to %s",
							typ, len(pkt), a, len(p), p[0], addr)
					}
				}
			}
		}
	}
}

func sinkAll(k *secSink) map[string][][]byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := k.sent
	k.sent = map[string][][]byte{}
	return out
}
