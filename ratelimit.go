// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Limits on wrong passwords, for the dashboard login and the phones' sign-in.
//
// A key (a client address, or an account) that collects `max` failures within
// `window` is refused for `lock`. A success clears that key's count. Only
// failures count, so a phone that reconnects a hundred times an hour with the
// right password is never slowed down.
//
// Dashboard: 10 wrong passwords per account or 30 per address in 15 minutes
// lock that account/address for 15 minutes (HTTP 429).
// Phones: 10 per account or 30 per address in 5 minutes lock for 5 minutes.
// Pending phone challenges expire after 30 s and are capped, so a flood of
// challenge requests cannot grow memory without bound.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

type failEntry struct {
	first  time.Time // start of the current window
	count  int
	locked time.Time // refused until then
}

type failLimiter struct {
	mu      sync.Mutex
	max     int // per address ("ip:" keys and anything else)
	maxAcct int // per account ("acct:" keys): stricter
	window  time.Duration
	lock    time.Duration
	maxKeys int
	entries map[string]*failEntry
	now     func() time.Time
}

func newFailLimiter(maxAddr, maxAcct int, window, lock time.Duration) *failLimiter {
	return &failLimiter{max: maxAddr, maxAcct: maxAcct, window: window, lock: lock, maxKeys: 50000,
		entries: map[string]*failEntry{}, now: time.Now}
}

// blocked reports whether key is locked out, and for how long still.
func (l *failLimiter) blocked(key string) (bool, time.Duration) {
	if l == nil || key == "" {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return false, 0
	}
	if left := e.locked.Sub(l.now()); left > 0 {
		return true, left
	}
	return false, 0
}

// fail records a failure; true when this one locked the key.
func (l *failLimiter) fail(key string) bool {
	if l == nil || key == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil {
		if len(l.entries) >= l.maxKeys {
			l.pruneLocked(now)
		}
		if len(l.entries) >= l.maxKeys {
			// Still full of live entries: someone is spraying from many
			// addresses. Drop the oldest windows rather than grow forever.
			l.evictOldestLocked(l.maxKeys / 10)
		}
		e = &failEntry{first: now}
		l.entries[key] = e
	}
	if now.Sub(e.first) > l.window {
		e.first, e.count = now, 0
	}
	e.count++
	limit := l.max
	if strings.HasPrefix(key, "acct:") && l.maxAcct > 0 {
		limit = l.maxAcct
	}
	if e.count >= limit {
		e.locked = now.Add(l.lock)
		e.first, e.count = now, 0
		return true
	}
	return false
}

func (l *failLimiter) success(key string) {
	if l == nil || key == "" {
		return
	}
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

func (l *failLimiter) pruneLocked(now time.Time) {
	for k, e := range l.entries {
		if now.Sub(e.first) > l.window && !now.Before(e.locked) {
			delete(l.entries, k)
		}
	}
}

// evictOldestLocked drops the n oldest windows in one pass. It used to scan
// the whole table once per eviction (5,000 scans of 50,000 entries) while the
// server lock was held: a spray of bad sign-ins froze the server for seconds.
func (l *failLimiter) evictOldestLocked(n int) {
	if n <= 0 || len(l.entries) == 0 {
		return
	}
	type kt struct {
		k string
		t time.Time
	}
	all := make([]kt, 0, len(l.entries))
	for k, e := range l.entries {
		all = append(all, kt{k, e.first})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	if n > len(all) {
		n = len(all)
	}
	for _, x := range all[:n] {
		delete(l.entries, x.k)
	}
}

func (l *failLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// --- sign-in guard ----------------------------------------------------------
//
// Wrong passwords are limited PER ADDRESS: a guesser gets ipMax tries per
// window and is then refused for the lock time. Before, ten wrong passwords
// for an ACCOUNT locked that account for everyone, so anyone could keep a
// unit, a gateway or the admin locked out with a few packets.
//
// An account is only "guarded" when failures arrive from many addresses at
// once (a distributed guess, acctMax in a window). While guarded, sign-in for
// that account is accepted only from addresses that signed in to it
// successfully before — so the people who use the account keep working, and
// the correct password from a known address always gets through.
//
// Counting uses the account the request RESOLVES to, never the agency text in
// the packet: a junk agency name used to give a fresh counter per guess. A
// success does not clear the address counter (one valid guest login used to
// reset it).

type signInGuard struct {
	ips  *failLimiter
	acct *failLimiter

	mu   sync.Mutex
	good map[string][]string // account -> addresses that signed in (newest last)
}

const (
	goodAddrsKept  = 8
	goodAccountCap = 20000
)

func newSignInGuard(ipMax, acctMax int, window, lock time.Duration) *signInGuard {
	return &signInGuard{
		ips:  newFailLimiter(ipMax, ipMax, window, lock),
		acct: newFailLimiter(acctMax, acctMax, window, lock),
		good: map[string][]string{},
	}
}

// guardAddr is what an address counts as: an IPv4 address, or the /64 of an
// IPv6 address (one subscriber controls a whole /64).
func guardAddr(ip string) string {
	p := net.ParseIP(strings.TrimSpace(ip))
	if p == nil {
		return ip
	}
	if v4 := p.To4(); v4 != nil {
		return v4.String()
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func (g *signInGuard) knownGood(account, addr string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.good[account] {
		if a == addr {
			return true
		}
	}
	return false
}

// blocked: may this address try to sign in to these accounts now?
func (g *signInGuard) blocked(ip string, accounts ...string) (bool, time.Duration, string) {
	if g == nil {
		return false, 0, ""
	}
	addr := guardAddr(ip)
	if locked, left := g.ips.blocked("ip:" + addr); locked {
		return true, left, "address"
	}
	for _, a := range accounts {
		if locked, left := g.acct.blocked("acct:" + a); locked && !g.knownGood(a, addr) {
			return true, left, "account"
		}
	}
	return false, 0, ""
}

// failed records a wrong password; true when it locked the address or put an
// account under guard.
func (g *signInGuard) failed(ip string, accounts ...string) bool {
	if g == nil {
		return false
	}
	locked := g.ips.fail("ip:" + guardAddr(ip))
	for _, a := range accounts {
		if g.acct.fail("acct:" + a) {
			locked = true
		}
	}
	return locked
}

// succeeded remembers the address as one that uses this account.
func (g *signInGuard) succeeded(ip, account string) {
	if g == nil || account == "" {
		return
	}
	addr := guardAddr(ip)
	g.mu.Lock()
	defer g.mu.Unlock()
	list := g.good[account]
	for i, a := range list {
		if a == addr {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	list = append(list, addr)
	if len(list) > goodAddrsKept {
		list = list[len(list)-goodAddrsKept:]
	}
	if _, ok := g.good[account]; !ok && len(g.good) >= goodAccountCap {
		for k := range g.good { // bounded: drop an arbitrary old entry
			delete(g.good, k)
			break
		}
	}
	g.good[account] = list
}

// --- phone challenges --------------------------------------------------------
//
// Stateless. The nonce for an address is HMAC(server secret, address, time
// slot); nothing is stored per request. The old table of pending challenges
// (20,000 entries, no per-address cap) could be kept full by one host cycling
// source ports, which blocked every new sign-in, and each refused request
// scanned the whole table under the server lock.
//
// A sign-in is checked against the current and the previous slot, so a nonce
// is good for challengeSlot to 2 x challengeSlot.

const challengeSlot = 10 * time.Second

var (
	challengeSecretOnce sync.Once
	challengeSecret     [32]byte
)

func challengeKey() []byte {
	challengeSecretOnce.Do(func() {
		if _, err := rand.Read(challengeSecret[:]); err != nil {
			panic("no randomness for the challenge secret: " + err.Error())
		}
	})
	return challengeSecret[:]
}

func challengeNonceAt(addr string, slot int64) [32]byte {
	mac := hmac.New(sha256.New, challengeKey())
	mac.Write([]byte(addr))
	mac.Write([]byte{0})
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(slot))
	mac.Write(b[:])
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// challengeNonce is the nonce to send addr now.
func challengeNonce(addr string, now time.Time) [32]byte {
	return challengeNonceAt(addr, now.UnixNano()/int64(challengeSlot))
}

// challengeNonces are the nonces a sign-in from addr may answer now.
func challengeNonces(addr string, now time.Time) [][32]byte {
	slot := now.UnixNano() / int64(challengeSlot)
	return [][32]byte{challengeNonceAt(addr, slot), challengeNonceAt(addr, slot-1)}
}

var (
	dummyHashOnce sync.Once
	dummyHash     string
)

// dummyPasswordHash is a bcrypt hash of a random password, compared against
// when a login names an account that does not exist.
func dummyPasswordHash() string {
	dummyHashOnce.Do(func() {
		h, err := hashPasswordSecure(fmt.Sprintf("%d-no-such-account", time.Now().UnixNano()))
		if err == nil {
			dummyHash = h
		}
	})
	return dummyHash
}
