// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Dispatch operations: unit status, dispatch messages and all-call.
//
// UNIT STATUS (0x2F, both directions): [u8 code][u16 len|note]
//   A unit sets its own status from the app; a console can set it too, and the
//   server then tells the unit. Kept in memory per callsign (survives
//   reconnects, not a server restart).
//
// DISPATCH MESSAGE (0x30):
//   server -> app: [u16 len|from][u16 len|text][i64 unix ms]
//   app -> server: [u16 len|text][i64 unix ms]   (a reply to dispatch)
//   Phones message each other end-to-end encrypted; this is a separate,
//   server-visible message between a console and units. Messages to a unit
//   that is offline wait (24 h, 20 per unit) and go out when it joins.
//
// ALL-CALL: a console transmits to every talkgroup of its agency at once
//   ({"type":"channel","agency":A,"allcall":true}). It bypasses floor control:
//   an all-call is a dispatcher override.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	PacketTypeUnitStatus      byte = 0x2F
	PacketTypeDispatchMessage byte = 0x30
	// Receipt to a unit for a message it sent dispatch:
	//   [u8 kind 1=delivered (server has it) 2=read (a console showed it)][i64 the unit's message ms]
	PacketTypeDispatchReceipt byte = 0x31

	maxMessageLen     = 2000
	messageHistoryLen = 300
	pendingPerUnit    = 20
	pendingMessageTTL = 24 * time.Hour
)

var unitStatusNames = map[uint8]string{
	0: "", 1: "Available", 2: "Busy", 3: "En route", 4: "On scene", 5: "Out of service",
}

type UnitStatusPacket struct {
	Code uint8
	Note string
}

func ParseUnitStatus(data []byte) (*UnitStatusPacket, error) {
	r := bytes.NewReader(data)
	code, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	note, _ := getStr(r)
	return &UnitStatusPacket{Code: code, Note: note}, nil
}

func buildUnitStatus(code uint8, note string) []byte {
	var b bytes.Buffer
	b.WriteByte(PacketTypeUnitStatus)
	b.WriteByte(code)
	putStr(&b, note)
	return b.Bytes()
}

type DispatchMessagePacket struct {
	Text string
	At   int64
}

func ParseDispatchMessage(data []byte) (*DispatchMessagePacket, error) {
	r := bytes.NewReader(data)
	text, err := getStr(r)
	if err != nil {
		return nil, err
	}
	var at int64
	binary.Read(r, binary.BigEndian, &at)
	return &DispatchMessagePacket{Text: text, At: at}, nil
}

func buildDispatchMessage(from, text string, at time.Time) []byte {
	var b bytes.Buffer
	b.WriteByte(PacketTypeDispatchMessage)
	putStr(&b, from)
	putStr(&b, text)
	binary.Write(&b, binary.BigEndian, at.UnixMilli())
	return b.Bytes()
}

// --- state ------------------------------------------------------------------

type unitStatus struct {
	Code  uint8
	Note  string
	At    time.Time
	SetBy string // "" = the unit itself, else the console operator
}

type dispatchMsg struct {
	ID     string `json:"id"`
	At     int64  `json:"at"`
	Agency string `json:"agency"`
	Dir    string `json:"dir"`  // "out" (console -> units) | "in" (unit -> dispatch)
	From   string `json:"from"` // operator or unit callsign
	To     string `json:"to"`   // unit callsign, "Channel: X" or "All units"
	Text   string `json:"text"`
	UnitAt int64  `json:"unit_at,omitempty"` // "in": the unit's own timestamp (its receipts key)
	Read   bool   `json:"read,omitempty"`    // "in": a console has shown it
	TakUID string `json:"tak_uid,omitempty"` // TAK user (GeoChat) on the other end
	Sent   int    `json:"sent,omitempty"`    // units it reached now
	Queued int    `json:"queued,omitempty"`  // offline units it will reach on join
}

type pendingMsg struct {
	data []byte
	at   time.Time
}

type dispatchOps struct {
	mu       sync.Mutex
	status   map[string]*unitStatus   // userKey(agency, callsign)
	history  map[string][]dispatchMsg // agency -> newest last
	pending  map[string][]pendingMsg  // userKey -> messages for an offline unit
	sequence int
}

func (s *Server) ops() *dispatchOps {
	s.opsOnce.Do(func() {
		s.dispatchOps = &dispatchOps{
			status:  map[string]*unitStatus{},
			history: map[string][]dispatchMsg{},
			pending: map[string][]pendingMsg{},
		}
	})
	return s.dispatchOps
}

// statusFor returns a unit's status for the console client list (nil if none).
func (s *Server) statusFor(agency, user string) map[string]interface{} {
	o := s.ops()
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.status[userKey(agency, user)]
	if st == nil || st.Code == 0 {
		return nil
	}
	return map[string]interface{}{
		"code": st.Code, "name": unitStatusNames[st.Code], "note": st.Note,
		"at": st.At.Format(time.RFC3339), "by": st.SetBy,
	}
}

func (s *Server) setUnitStatus(agency, user string, code uint8, note, by string) {
	if _, ok := unitStatusNames[code]; !ok {
		return
	}
	if len(note) > 200 {
		note = note[:200]
	}
	o := s.ops()
	o.mu.Lock()
	o.status[userKey(agency, user)] = &unitStatus{Code: code, Note: note, At: time.Now(), SetBy: by}
	o.mu.Unlock()
	log.Printf("Unit status: %s [%s] -> %q (by %s)", user, agency, unitStatusNames[code], nonEmpty(by, "unit"))
	s.sendToConsoles(agency, map[string]interface{}{
		"type": "status", "agency": agency, "user": user, "code": code,
		"name": unitStatusNames[code], "note": note, "by": by, "at": time.Now().Format(time.RFC3339),
	})
}

// handleUnitStatus: a unit set its own status.
func (s *Server) handleUnitStatus(p *UnitStatusPacket, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}
	s.mu.RLock()
	c, _ := s.findClientByAddr(addr)
	var agency, user string
	if c != nil {
		agency, user = c.AgencyID, c.Username
	}
	s.mu.RUnlock()
	if c == nil {
		return
	}
	s.setUnitStatus(agency, user, p.Code, p.Note, "")
}

// consoleSetStatus: an operator set a unit's status; the unit is told.
func (s *Server) consoleSetStatus(console *Client, agency, user string, code uint8) {
	if console.AgencyID != "" && console.AgencyID != agency {
		return
	}
	s.setUnitStatus(agency, user, code, "", console.Username)
	s.mu.RLock()
	c := s.clients[userKey(agency, user)]
	var addr *net.UDPAddr
	if c != nil {
		addr = c.Addr
	}
	s.mu.RUnlock()
	if addr != nil {
		s.sendPacket(buildUnitStatus(code, "Set by dispatch"), addr)
	}
}

// --- messages ---------------------------------------------------------------

type messageTarget struct {
	Agency string `json:"agency"`
	User   string `json:"user"`
}

// consoleSendMessage delivers an operator's message to one unit, every unit on
// a channel ("agency:channel"), or every unit in an agency.
func (s *Server) consoleSendMessage(console *Client, text string, unit *messageTarget, channel, allAgency string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len(text) > maxMessageLen {
		text = text[:maxMessageLen]
	}
	now := time.Now()
	type rcpt struct {
		key  string
		addr *net.UDPAddr
	}
	var rcpts []rcpt
	var agency, label string

	s.mu.RLock()
	allowed := func(a string) bool { return console.AgencyID == "" || console.AgencyID == a }
	switch {
	case unit != nil && allowed(unit.Agency):
		agency, label = unit.Agency, unit.User
		k := userKey(unit.Agency, unit.User)
		var addr *net.UDPAddr
		if c := s.clients[k]; c != nil {
			addr = c.Addr
		}
		rcpts = append(rcpts, rcpt{k, addr})
	case channel != "" && allowed(agencyFromKey(channel)):
		agency = agencyFromKey(channel)
		_, chNum := splitChannelKey(channel)
		label = fmt.Sprintf("Channel %d", chNum+1)
		if a := s.agencies[agency]; a != nil && chNum < MaxAgencyTalkgroups {
			label = a.talkgroupName(chNum)
		}
		for k, c := range s.clients {
			if c.AgencyID == agency && c.Channel == chNum && !isConsoleAddr(c.Addr) {
				rcpts = append(rcpts, rcpt{k, c.Addr})
			}
		}
	case allAgency != "" && allowed(allAgency):
		agency, label = allAgency, "All units"
		for k, c := range s.clients {
			if c.AgencyID == allAgency && !isConsoleAddr(c.Addr) {
				rcpts = append(rcpts, rcpt{k, c.Addr})
			}
		}
	}
	s.mu.RUnlock()
	if agency == "" {
		return
	}

	data := buildDispatchMessage("Dispatch ("+console.Username+")", text, now)
	sent, queued := 0, 0
	o := s.ops()
	for _, r := range rcpts {
		if r.addr != nil {
			s.sendPacket(data, r.addr)
			sent++
			continue
		}
		o.mu.Lock()
		q := append(o.pending[r.key], pendingMsg{data, now})
		if len(q) > pendingPerUnit {
			q = q[len(q)-pendingPerUnit:]
		}
		o.pending[r.key] = q
		o.mu.Unlock()
		queued++
	}
	s.recordMessage(dispatchMsg{At: now.UnixMilli(), Agency: agency, Dir: "out", From: console.Username,
		To: label, Text: text, Sent: sent, Queued: queued})
}

// handleDispatchMessage: a unit replied to dispatch.
func (s *Server) handleDispatchMessage(p *DispatchMessagePacket, addr *net.UDPAddr) {
	if s.config.AuthEnabled && !s.isAuthenticated(addr) {
		return
	}
	text := strings.TrimSpace(p.Text)
	if text == "" {
		return
	}
	if len(text) > maxMessageLen {
		text = text[:maxMessageLen]
	}
	s.mu.RLock()
	c, _ := s.findClientByAddr(addr)
	var agency, user string
	if c != nil {
		agency, user = c.AgencyID, c.Username
	}
	s.mu.RUnlock()
	if c == nil {
		return
	}
	s.recordMessage(dispatchMsg{At: time.Now().UnixMilli(), Agency: agency, Dir: "in", From: user, To: "Dispatch",
		Text: text, UnitAt: p.At})
	s.sendPacket(buildDispatchReceipt(1, p.At), addr) // delivered
}

func buildDispatchReceipt(kind uint8, at int64) []byte {
	var b bytes.Buffer
	b.WriteByte(PacketTypeDispatchReceipt)
	b.WriteByte(kind)
	binary.Write(&b, binary.BigEndian, at)
	return b.Bytes()
}

// consoleMarkRead: a console showed these incoming messages. The first time a
// message is read, its sender is told and every console updates.
func (s *Server) consoleMarkRead(console *Client, ids []string) {
	type receipt struct {
		agency, user string
		at           int64
		id           string
	}
	var rs []receipt
	o := s.ops()
	o.mu.Lock()
	for agency, h := range o.history {
		if console.AgencyID != "" && console.AgencyID != agency {
			continue
		}
		for i := range h {
			m := &h[i]
			if m.Dir != "in" || m.Read {
				continue
			}
			for _, id := range ids {
				if id == m.ID {
					m.Read = true
					rs = append(rs, receipt{agency, m.From, m.UnitAt, m.ID})
					break
				}
			}
		}
	}
	o.mu.Unlock()
	for _, r := range rs {
		s.mu.RLock()
		var addr *net.UDPAddr
		if c := s.clients[userKey(r.agency, r.user)]; c != nil {
			addr = c.Addr
		}
		s.mu.RUnlock()
		if addr != nil && r.at > 0 {
			s.sendPacket(buildDispatchReceipt(2, r.at), addr)
		}
		s.sendToConsoles(r.agency, map[string]interface{}{"type": "message_read", "id": r.id, "by": console.Username})
	}
}

func (s *Server) recordMessage(m dispatchMsg) {
	o := s.ops()
	o.mu.Lock()
	o.sequence++
	m.ID = fmt.Sprintf("%d-%d", m.At, o.sequence)
	h := append(o.history[m.Agency], m)
	if len(h) > messageHistoryLen {
		h = h[len(h)-messageHistoryLen:]
	}
	o.history[m.Agency] = h
	o.mu.Unlock()
	log.Printf("Dispatch message %s [%s] %s -> %s (%d chars)", m.Dir, m.Agency, m.From, m.To, len(m.Text))
	msg := map[string]interface{}{"type": "message"}
	b, _ := json.Marshal(m)
	var fields map[string]interface{}
	json.Unmarshal(b, &fields)
	for k, v := range fields {
		msg[k] = v
	}
	s.sendToConsoles(m.Agency, msg)
}

// messageHistory: recent messages a console may see (oldest first).
func (s *Server) messageHistory(consoleAgency string) []dispatchMsg {
	o := s.ops()
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []dispatchMsg
	for agency, h := range o.history {
		if consoleAgency == "" || consoleAgency == agency {
			out = append(out, h...)
		}
	}
	// Merge order across agencies.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].At < out[j-1].At; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// deliverPending sends messages that waited for a unit that was offline.
func (s *Server) deliverPending(agency, user string, addr *net.UDPAddr) {
	o := s.ops()
	k := userKey(agency, user)
	o.mu.Lock()
	q := o.pending[k]
	delete(o.pending, k)
	o.mu.Unlock()
	for _, m := range q {
		if time.Since(m.at) < pendingMessageTTL {
			s.sendPacket(m.data, addr)
		}
	}
	if len(q) > 0 {
		log.Printf("Delivered %d waiting dispatch message(s) to %s [%s]", len(q), user, agency)
	}
}

// --- all-call -----------------------------------------------------------------

// allCallChannels: every talkgroup of the agencies a console transmits to.
// Caller holds s.mu (read).
func (s *Server) allCallChannels(agencies []string) [][2]interface{} {
	var out [][2]interface{}
	for _, a := range agencies {
		ag := s.agencies[a]
		if ag == nil {
			continue
		}
		n := ag.talkgroupCount()
		for ch := 0; ch < n; ch++ {
			out = append(out, [2]interface{}{a, ch})
		}
	}
	return out
}

// --- helpers --------------------------------------------------------------------

func isConsoleAddr(addr *net.UDPAddr) bool {
	return addr != nil && addr.IP.Equal(net.IPv4(127, 0, 0, 2))
}

func splitChannelKey(k string) (string, int) {
	i := strings.LastIndex(k, ":")
	if i < 0 {
		return k, 0
	}
	var n int
	fmt.Sscanf(k[i+1:], "%d", &n)
	return k[:i], n
}
