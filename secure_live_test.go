// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

//go:build live

package main

// A live check of the secure protocol against a running server:
//
//	MRS_LIVE_HOST=<server address> MRS_LIVE_USER=u MRS_LIVE_PASS=p MRS_LIVE_AGENCY=default \
//	  go test -tags live -run TestLiveSecure -v .
//
// It signs in over real UDP, joins as MRS_LIVE_CALLSIGN (default "mrs-live"),
// and checks that the server answers only inside the session, ignores a
// plaintext packet from the same socket, and follows the phone to a new
// source port.

import (
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"
)

type liveConn struct {
	t        *testing.T
	c        *net.UDPConn
	sid      uint32
	c2s, s2c cipher.AEAD
	send     uint32
}

func liveDial(t *testing.T, host string) *net.UDPConn {
	ra, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, "5000"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialUDP("udp", nil, ra)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (l *liveConn) read(c *net.UDPConn, wait time.Duration) []byte {
	buf := make([]byte, 65535)
	c.SetReadDeadline(time.Now().Add(wait))
	n, err := c.Read(buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

func (l *liveConn) seal(inner []byte) []byte {
	l.send++
	out := make([]byte, 9)
	out[0] = PacketTypeSecData
	binary.BigEndian.PutUint32(out[1:5], l.sid)
	binary.BigEndian.PutUint32(out[5:9], l.send)
	return l.c2s.Seal(out, secNonce(l.send), inner, out[:9])
}

// drain reads for d and returns decrypted inner packets and how many packets
// arrived outside the session.
func (l *liveConn) drain(c *net.UDPConn, d time.Duration) (inner [][]byte, plain int) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		p := l.read(c, time.Until(end))
		if p == nil {
			break
		}
		if p[0] != PacketTypeSecData || len(p) < 25 || binary.BigEndian.Uint32(p[1:5]) != l.sid {
			plain++
			continue
		}
		b, err := l.s2c.Open(nil, secNonce(binary.BigEndian.Uint32(p[5:9])), p[9:], p[:9])
		if err != nil {
			l.t.Fatalf("server packet failed its check: %v", err)
		}
		inner = append(inner, b)
	}
	return
}

func TestLiveSecure(t *testing.T) {
	host, user, pass, agency := os.Getenv("MRS_LIVE_HOST"), os.Getenv("MRS_LIVE_USER"), os.Getenv("MRS_LIVE_PASS"), os.Getenv("MRS_LIVE_AGENCY")
	if host == "" || user == "" || pass == "" {
		t.Skip("set MRS_LIVE_HOST, MRS_LIVE_USER, MRS_LIVE_PASS")
	}
	callsign := os.Getenv("MRS_LIVE_CALLSIGN")
	if callsign == "" {
		callsign = "mrs-live"
	}
	l := &liveConn{t: t, c: liveDial(t, host)}
	defer l.c.Close()

	// 1-2: hello
	eph, _ := ecdh.X25519().GenerateKey(rand.Reader)
	cNonce := make([]byte, 16)
	rand.Read(cNonce)
	hello := append([]byte{PacketTypeSecHello, secVersion}, eph.PublicKey().Bytes()...)
	hello = append(hello, cNonce...)
	var rep []byte
	for i := 0; i < 3 && rep == nil; i++ {
		l.c.Write(hello)
		if p := l.read(l.c, 1500*time.Millisecond); p != nil && p[0] == PacketTypeSecHelloReply {
			rep = p
		}
	}
	if len(rep) != 115 {
		t.Fatalf("no hello reply from %s (got %d bytes): server without the secure protocol?", host, len(rep))
	}
	slot, eS, S, sNonce, tag := rep[2], rep[3:35], rep[35:67], rep[67:83], rep[83:115]
	sPub, _ := ecdh.X25519().NewPublicKey(S)
	ePub, _ := ecdh.X25519().NewPublicKey(eS)
	dhEE, _ := eph.ECDH(ePub)
	dhES, _ := eph.ECDH(sPub)
	th1 := secHash([]byte("MRS1-th1"), eph.PublicKey().Bytes(), cNonce, eS, S, sNonce)
	prk := hkdf.Extract(sha256.New, append(dhEE, dhES...), []byte("MRS1"))
	if !hmac.Equal(tag, computeHmacSha256(secExpand(prk, "MRS1 hs tag", th1[:]), th1[:])) {
		t.Fatal("server hello tag does not verify")
	}
	fp := sha256.Sum256(S)
	t.Logf("server key fingerprint %x", fp[:8])

	// 3-4: auth
	clientKey, v := scramDerive(hashPassword(pass), S, user)
	am := secHash([]byte("MRS1-auth"), th1[:], []byte(user))
	sig := computeHmacSha256(v.storedKey[:], am[:])
	proof := make([]byte, 32)
	for i := range proof {
		proof[i] = clientKey[i] ^ sig[i]
	}
	put := func(b []byte, s string) []byte {
		b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
		return append(b, s...)
	}
	var plain []byte
	plain = put(plain, user)
	plain = put(plain, agency)
	plain = append(plain, 0)
	plain = append(plain, proof...)
	plain = put(plain, "live-test")
	n := make([]byte, 12)
	rand.Read(n)
	ct := secAEAD(secExpand(prk, "MRS1 hs c2s", th1[:])).Seal(nil, n, plain, th1[:])
	auth := append([]byte{PacketTypeSecAuth, secVersion, slot}, eph.PublicKey().Bytes()...)
	auth = append(auth, cNonce...)
	auth = append(auth, n...)
	auth = append(auth, ct...)
	l.c.Write(auth)
	ar := l.read(l.c, 5*time.Second)
	if ar == nil || ar[0] != PacketTypeSecAuthReply {
		t.Fatal("no auth reply")
	}
	ctHash := sha256.Sum256(ct)
	th3 := secHash([]byte("MRS1-th3"), th1[:], n, ctHash[:])
	body, err := secAEAD(secExpand(prk, "MRS1 hs s2c", th1[:])).Open(nil, ar[2:14], ar[14:], th3[:])
	if err != nil {
		t.Fatalf("auth reply does not decrypt: %v", err)
	}
	ml := int(binary.BigEndian.Uint16(body[37:39]))
	msg := string(body[39 : 39+ml])
	if body[0] != 1 {
		t.Fatalf("sign-in refused: %q", msg)
	}
	if !hmac.Equal(body[5:37], computeHmacSha256(v.serverKey[:], am[:])) {
		t.Fatal("server signature does not verify")
	}
	// The same auth again (as if the reply were lost): the same answer.
	l.c.Write(auth)
	if again := l.read(l.c, 3*time.Second); again == nil || !bytes.Equal(again, ar) {
		t.Fatal("a repeated auth did not get the same reply")
	}
	l.sid = binary.BigEndian.Uint32(body[1:5])
	l.c2s = secAEAD(secExpand(prk, "MRS1 data c2s", th3[:]))
	l.s2c = secAEAD(secExpand(prk, "MRS1 data s2c", th3[:]))
	t.Logf("signed in: %q, session %08x", msg, l.sid)

	// JOIN channel 0 inside the session.
	join := put([]byte{PacketTypeJoin}, callsign)
	join = binary.BigEndian.AppendUint32(join, 0)
	l.c.Write(l.seal(join))
	in, outside := l.drain(l.c, 2*time.Second)
	if len(in) == 0 || outside != 0 {
		t.Fatalf("after JOIN: %d packets in the session, %d outside it", len(in), outside)
	}
	types := map[byte]int{}
	for _, b := range in {
		types[b[0]]++
	}
	t.Logf("JOIN answered with %d encrypted packets, types %v", len(in), types)

	// A plaintext LEAVE from this very socket must be ignored: a heartbeat in
	// the session afterwards is still echoed.
	l.c.Write([]byte{PacketTypeLeave})
	hb := put([]byte{PacketTypeHeartbeat}, callsign)
	l.c.Write(l.seal(hb))
	in, _ = l.drain(l.c, 2*time.Second)
	echoed := false
	for _, b := range in {
		echoed = echoed || b[0] == PacketTypeHeartbeat
	}
	if !echoed {
		t.Fatal("after a plaintext LEAVE the session no longer answers: the forged packet was acted on")
	}

	// The phone changes network: same session from a new socket.
	c2 := liveDial(t, host)
	defer c2.Close()
	c2.Write(l.seal(hb))
	in, outside = l.drain(c2, 2*time.Second)
	echoed = false
	for _, b := range in {
		echoed = echoed || b[0] == PacketTypeHeartbeat
	}
	if !echoed || outside != 0 {
		t.Fatalf("the session did not follow to a new address (echo=%v, outside=%d)", echoed, outside)
	}
	if os.Getenv("MRS_LIVE_HOLD") != "" {
		// Stay signed in so the dashboard can be inspected, heartbeating.
		d, _ := time.ParseDuration(os.Getenv("MRS_LIVE_HOLD"))
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			c2.Write(l.seal(hb))
			in, _ := l.drain(c2, 3*time.Second)
			for _, b := range in {
				if b[0] == PacketTypeAuthResponse && b[1] == AuthStatusRevoked {
					n := int(binary.BigEndian.Uint16(b[2:4]))
					t.Logf("server ended the session (encrypted): %q", strings.TrimSpace(string(b[4:4+n])))
					return
				}
			}
		}
	}
	leave := []byte{PacketTypeLeave}
	c2.Write(l.seal(leave))
}
