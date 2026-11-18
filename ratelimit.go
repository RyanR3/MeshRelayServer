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
	"fmt"
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

func (l *failLimiter) evictOldestLocked(n int) {
	for ; n > 0 && len(l.entries) > 0; n-- {
		var oldK string
		var oldT time.Time
		for k, e := range l.entries {
			if oldK == "" || e.first.Before(oldT) {
				oldK, oldT = k, e.first
			}
		}
		delete(l.entries, oldK)
	}
}

func (l *failLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// --- phone challenges --------------------------------------------------------

const (
	challengeTTL = 30 * time.Second
	maxChallenge = 20000
)

type pendingChallenge struct {
	nonce [32]byte
	at    time.Time
}

// storeChallengeLocked keeps a nonce for addr; false when the table is full of
// live challenges (a flood) and this one was refused. Caller holds s.mu.
func (s *Server) storeChallengeLocked(addr string, nonce [32]byte, now time.Time) bool {
	if _, exists := s.pendingChallenges[addr]; !exists && len(s.pendingChallenges) >= maxChallenge {
		for k, c := range s.pendingChallenges {
			if now.Sub(c.at) > challengeTTL {
				delete(s.pendingChallenges, k)
			}
		}
		if len(s.pendingChallenges) >= maxChallenge {
			return false
		}
	}
	s.pendingChallenges[addr] = pendingChallenge{nonce: nonce, at: now}
	return true
}

// takeChallengeLocked returns and removes addr's nonce if it is still fresh.
func (s *Server) takeChallengeLocked(addr string, now time.Time) ([32]byte, bool) {
	c, ok := s.pendingChallenges[addr]
	if !ok {
		return [32]byte{}, false
	}
	delete(s.pendingChallenges, addr)
	if now.Sub(c.at) > challengeTTL {
		return [32]byte{}, false
	}
	return c.nonce, true
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
