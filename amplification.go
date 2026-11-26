// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Reflection / amplification guard for the UDP port.
//
// A UDP source address can be forged. Anything the server sends to an address
// that has not signed in may therefore land on a third party, so:
//
//  1. A reply to an unauthenticated request is never larger than the request,
//     with two exceptions that today's apps need: the sign-in challenge (0x0E,
//     33 bytes) and the secure hello reply (0x41, 115 bytes). Every app sends
//     a shorter request for those (0x0D is 3 + name + agency bytes, the hello
//     is 50 bytes).
//  2. Those two larger replies come out of a budget: per source address
//     (IPv4, or an IPv6 /64) and for the whole server. Inside the budget a
//     short request is answered as before. Past it, a short challenge request
//     gets no answer and a short hello gets the 18-byte retry cookie (0x45),
//     which is smaller than the hello; an app that returns the cookie has
//     proved it receives at that address and is answered in full.
//  3. A request padded to at least the size of its reply is not amplifying
//     and is always answered (docs/SECURE_PROTOCOL.md, "Padding"). Apps that
//     pad close the amplification factor completely.
//
// The budget is sized so that sign-ins are never slowed in normal use: a few
// hundred phones reconnecting at once after a server restart fit in the burst.
// What a forger can reflect at one victim is bounded by the per-source budget,
// and what the whole server can emit by the global one.

import (
	"crypto/hmac"
	"hash/maphash"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Per source address: a burst, then a steady rate. One phone sign-in is
	// at most a few short requests (the app may open two connections, and
	// retries a lost challenge up to three times); a carrier NAT can put
	// several phones behind one address.
	smallReplyPerSrcBurst = 32
	smallReplyPerSrcRate  = 2.0 // per second

	// Whole server. 2000 covers a reconnect storm of several hundred phones;
	// the steady rate bounds what a forged flood can ever make the server
	// send: 500/s x 115 bytes is under 0.5 Mbit/s, spread over all victims.
	smallReplyGlobalBurst = 2000
	smallReplyGlobalRate  = 500.0 // per second

	// Per-source buckets live in a fixed table indexed by a keyed hash of the
	// address, so a flood from forged random sources cannot grow memory.
	// Two addresses that share a bucket share its budget, which only makes
	// the limit stricter for them.
	smallReplyBuckets = 8192

	challengeReplyLen = 1 + 32                // 0x0E | nonce
	secHelloReplyLen  = 3 + 32 + 32 + 16 + 32 // 0x41 | ver | slot | eS | S | sNonce | tag
)

type tokenBucket struct {
	tokens float64
	last   int64 // unix nanos of the last refill; 0 = never used (full)
}

// take spends one token if there is one.
func (b *tokenBucket) take(now int64, rate, burst float64) bool {
	if b.last == 0 {
		b.tokens, b.last = burst, now
	}
	if el := now - b.last; el > 0 {
		b.tokens += float64(el) / 1e9 * rate
		if b.tokens > burst {
			b.tokens = burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// smallReplyGate is the budget for replies larger than their request.
type smallReplyGate struct {
	once   sync.Once
	seed   maphash.Seed
	mu     sync.Mutex
	src    [smallReplyBuckets]tokenBucket
	global tokenBucket

	refused    atomic.Int64 // refused since the last report
	lastReport atomic.Int64
	now        func() time.Time // tests
}

func (g *smallReplyGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// allow reports whether a reply larger than its request may go to ip now.
func (g *smallReplyGate) allow(ip net.IP) bool {
	g.once.Do(func() { g.seed = maphash.MakeSeed() })
	now := g.clock()
	idx := g.bucketFor(ip)
	g.mu.Lock()
	ns := now.UnixNano()
	ok := g.src[idx].take(ns, smallReplyPerSrcRate, smallReplyPerSrcBurst)
	if ok {
		ok = g.global.take(ns, smallReplyGlobalRate, smallReplyGlobalBurst)
		if !ok {
			// Not this source's fault: give its token back.
			g.src[idx].tokens++
		}
	}
	g.mu.Unlock()
	if !ok {
		g.noteRefused(now)
	}
	return ok
}

func (g *smallReplyGate) bucketFor(ip net.IP) int {
	var h maphash.Hash
	h.SetSeed(g.seed)
	if v4 := ip.To4(); v4 != nil {
		h.Write(v4)
	} else if len(ip) == net.IPv6len {
		h.Write(ip[:8]) // one subscriber holds a whole /64
	} else {
		h.Write(ip)
	}
	return int(h.Sum64() % smallReplyBuckets)
}

// noteRefused counts a refused reply and logs a summary at most once a
// minute, so a flood cannot flood the log as well.
func (g *smallReplyGate) noteRefused(now time.Time) {
	n := g.refused.Add(1)
	last := g.lastReport.Load()
	if now.UnixNano()-last < int64(time.Minute) {
		return
	}
	if g.lastReport.CompareAndSwap(last, now.UnixNano()) {
		g.refused.Add(-n)
		log.Printf("Sign-in replies limited: %d short unauthenticated request(s) not answered in full in the last minute (possible forged-source flood)", n)
	}
}

// unauthReplyAllowed decides whether reply may be sent to addr, which has not
// signed in, in answer to a request of reqLen bytes. A reply no larger than
// the request always may; a larger one only within the budget.
func (s *Server) unauthReplyAllowed(reqLen, replyLen int, addr *net.UDPAddr) bool {
	if replyLen <= reqLen {
		return true
	}
	return s.smallReplies.allow(addr.IP)
}

// sendNoLarger sends reply only when it is no larger than the request it
// answers. For replies to addresses that have not signed in.
func (s *Server) sendNoLarger(reqLen int, reply []byte, addr *net.UDPAddr) bool {
	if len(reply) > reqLen {
		return false
	}
	s.sendPacket(reply, addr)
	return true
}

// wireLenChallengeReq is how many bytes a challenge request took on the wire:
// what was received (WireLen, set by the packet parser) and never less than
// its fields need (direct calls in tests leave WireLen 0).
func wireLenChallengeReq(p *AuthChallengeRequestPacket) int {
	n := 1 + 2 + len(p.Username)
	if p.AgencyID != "" {
		n += 2 + len(p.AgencyID)
	}
	if p.WireLen > n {
		return p.WireLen
	}
	return n
}

// wireLenAuthReq: the same for an auth request.
func wireLenAuthReq(p *AuthRequestPacket) int {
	n := 1 + 2 + len(p.Username) + 2 + len(p.PasswordHash)
	if p.AgencyID != "" || p.Flags != 0 {
		n += 2 + len(p.AgencyID)
	}
	if p.Flags != 0 {
		n++
	}
	if p.WireLen > n {
		return p.WireLen
	}
	return n
}

// helloCookieOK: the hello carries the retry cookie for addr (current or
// previous slot), i.e. the sender has received our earlier retry there.
func helloCookieOK(data []byte, addr *net.UDPAddr, slot int64) bool {
	if len(data) < 2+32+16+16 {
		return false
	}
	got := data[50:66]
	return hmac.Equal(got, secCookie(addr.String(), slot)) || hmac.Equal(got, secCookie(addr.String(), slot-1))
}
