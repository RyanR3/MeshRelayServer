// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// TAK Server connector — one per agency, configured in the dashboard.
//
// The MeshRelay server enrolls with the agency's TAK Server the way ATAK does
// (username/password -> client certificate on the enrollment port, 8446) and
// keeps a TLS CoT stream open (8089). Then:
//
//   - TAK users' positions (PLI) show on the agency's dispatch consoles as TAK
//     units. MeshRelay phones also report into TAK themselves; those copies are
//     hidden when the phone is already on the MeshRelay server.
//   - The connector appears in TAK as a contact ("MeshRelay Dispatch" by
//     default). GeoChat sent to it lands in the console's Messages panel, and a
//     console can message any TAK user (GeoChat).
//   - TAK emergencies (911, ring the bell, TIC…) appear in the console's
//     Emergencies panel; MeshRelay emergencies are sent into TAK as 911 alerts.
//
// Stored in tak_connections.json (0600): the client certificate and key the
// enrollment returned — never the password.

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const takConnectionsFile = "tak_connections.json"

type TakConfig struct {
	AgencyID   string `json:"agency_id"`
	Host       string `json:"host"`
	StreamPort int    `json:"stream_port"` // TLS CoT, usually 8089
	EnrollPort int    `json:"enroll_port"` // certificate enrollment, usually 8446
	APIPort    int    `json:"api_port"`    // HTTPS API with client cert, usually 8443
	// Groups: TAK groups ("channels" in ATAK) dispatch is active in. Empty =
	// leave TAK's default (every group the account belongs to).
	Groups     []string `json:"groups,omitempty"`
	Username   string   `json:"username"`
	Callsign   string   `json:"callsign"` // how dispatch appears in TAK
	UID        string   `json:"uid"`
	Enabled    bool     `json:"enabled"`
	CertPEM    string   `json:"cert_pem,omitempty"`
	KeyPEM     string   `json:"key_pem,omitempty"`
	CAPEMs     []string `json:"ca_pems,omitempty"`
	EnrolledAt string   `json:"enrolled_at,omitempty"`
	// Managed: set up by the managing platform (infra-TAK), which renews it.
	// Saving the connection from the dashboard hands it back to the operator.
	Managed bool `json:"managed,omitempty"`
}

// TakUnit is a TAK user seen on the stream.
type TakUnit struct {
	UID      string    `json:"uid"`
	Callsign string    `json:"callsign"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	Speed    float64   `json:"speed,omitempty"`
	Course   float64   `json:"course,omitempty"`
	Battery  int       `json:"battery,omitempty"`
	Group    string    `json:"group,omitempty"`
	Role     string    `json:"role,omitempty"`
	Device   string    `json:"device,omitempty"`
	At       time.Time `json:"at"`
	Stale    time.Time `json:"stale"`
}

type takConn struct {
	s      *Server
	cfg    TakConfig
	mu     sync.Mutex
	conn   net.Conn
	status string // connecting | connected | error | disabled
	errMsg string
	units  map[string]*TakUnit
	stop   chan struct{}
	since  time.Time
}

type takManager struct {
	mu    sync.Mutex
	conns map[string]*takConn // agency -> connection
	cfgs  map[string]TakConfig
}

func (s *Server) tak() *takManager {
	s.takOnce.Do(func() {
		s.takMgr = &takManager{conns: map[string]*takConn{}, cfgs: map[string]TakConfig{}}
	})
	return s.takMgr
}

// --- persistence ------------------------------------------------------------

func (s *Server) loadTakConnections() {
	data, err := os.ReadFile(takConnectionsFile)
	if err != nil {
		return
	}
	var cfgs []TakConfig
	if json.Unmarshal(data, &cfgs) != nil {
		log.Printf("TAK: could not parse %s", takConnectionsFile)
		return
	}
	m := s.tak()
	m.mu.Lock()
	for _, c := range cfgs {
		m.cfgs[c.AgencyID] = c
	}
	m.mu.Unlock()
	for _, c := range cfgs {
		if c.Enabled && c.CertPEM != "" {
			s.startTak(c)
		}
	}
	log.Printf("TAK: loaded %d connection(s)", len(cfgs))
}

func (s *Server) saveTakConnections() error {
	m := s.tak()
	m.mu.Lock()
	var list []TakConfig
	for _, c := range m.cfgs {
		list = append(list, c)
	}
	m.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].AgencyID < list[j].AgencyID })
	// TAK credentials: owner-only, written atomically (os.WriteFile kept the
	// mode of a file created 0644 by an older version).
	return writeJSONFileAtomic(takConnectionsFile, list)
}

// --- enrollment -------------------------------------------------------------

var nameEntryRe = regexp.MustCompile(`nameEntry[^>]*name="([A-Z]+)"[^>]*value="([^"]*)"`)

// takEnroll gets a client certificate from the TAK Server's enrollment API.
// The enrollment port's own certificate is not verified (as in ATAK: the
// server's CA comes back with the signed certificate and is pinned after).
func takEnroll(cfg *TakConfig, password string) error {
	return takEnrollVerified(cfg, password, nil)
}

// takEnrollVerified enrolls like takEnroll; with trustPEMs set (the TAK CA
// bundle a co-located platform reads from the TAK box) the enrollment port's
// certificate chain must verify against those CAs or the public roots.
func takEnrollVerified(cfg *TakConfig, password string, trustPEMs []string) error {
	tlsCfg := &tls.Config{InsecureSkipVerify: true}
	if len(trustPEMs) > 0 {
		tlsCfg = enrollTLSConfig(trustPEMs)
	}
	client := &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	base := fmt.Sprintf("https://%s:%d", cfg.Host, cfg.EnrollPort)
	do := func(method, url string, body io.Reader, ctype string) ([]byte, error) {
		req, _ := http.NewRequest(method, url, body)
		req.SetBasicAuth(cfg.Username, password)
		req.Header.Set("Accept", "application/json")
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, errors.New("TAK server rejected the username or password")
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("TAK server returned %d from %s", resp.StatusCode, url)
		}
		return b, nil
	}

	cfgXML, err := do("GET", base+"/Marti/api/tls/config", nil, "")
	if err != nil {
		return fmt.Errorf("enrollment config: %w", err)
	}
	subj := pkix.Name{CommonName: cfg.Username, Organization: []string{"TAK"}, OrganizationalUnit: []string{"TAK"}}
	for _, m := range nameEntryRe.FindAllStringSubmatch(string(cfgXML), -1) {
		switch m[1] {
		case "O":
			subj.Organization = []string{m[2]}
		case "OU":
			subj.OrganizationalUnit = []string{m[2]}
		}
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: subj}, key)
	if err != nil {
		return err
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	// TAK expects the base64 body without the PEM armour.
	csrBody := strings.TrimSpace(string(csrPEM))
	csrBody = strings.TrimPrefix(csrBody, "-----BEGIN CERTIFICATE REQUEST-----")
	csrBody = strings.TrimSuffix(csrBody, "-----END CERTIFICATE REQUEST-----")

	signURL := fmt.Sprintf("%s/Marti/api/tls/signClient/v2?clientUid=%s&version=3", base, urlQueryEscape(cfg.UID))
	resp, err := do("POST", signURL, strings.NewReader(strings.TrimSpace(csrBody)), "text/plain")
	if err != nil {
		return fmt.Errorf("certificate signing: %w", err)
	}
	var signed map[string]string
	if err := json.Unmarshal(resp, &signed); err != nil || signed["signedCert"] == "" {
		return errors.New("TAK server did not return a signed certificate")
	}
	certDER, err := base64.StdEncoding.DecodeString(signed["signedCert"])
	if err != nil {
		return errors.New("TAK server returned an unreadable certificate")
	}
	cfg.CertPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	cfg.KeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	cfg.CAPEMs = nil
	for i := 0; i < 10; i++ {
		v, ok := signed["ca"+strconv.Itoa(i)]
		if !ok {
			break
		}
		if der, err := base64.StdEncoding.DecodeString(v); err == nil {
			cfg.CAPEMs = append(cfg.CAPEMs, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
		}
	}
	cfg.EnrolledAt = time.Now().Format(time.RFC3339)
	return nil
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// tlsConfig: client certificate + the TAK CA as the only trusted root. TAK
// servers are usually reached by IP with certificates for a hostname, so the
// chain is verified but the name is not.
func (c *TakConfig) tlsConfig() (*tls.Config, error) {
	cert, err := tls.X509KeyPair([]byte(c.CertPEM), []byte(c.KeyPEM))
	if err != nil {
		return nil, fmt.Errorf("stored TAK certificate is invalid: %w", err)
	}
	pool := x509.NewCertPool()
	for _, p := range c.CAPEMs {
		pool.AppendCertsFromPEM([]byte(p))
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // replaced by VerifyPeerCertificate below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(c.CAPEMs) == 0 {
				return nil // no CA from enrollment: accept (logged at connect)
			}
			if len(raw) == 0 {
				return errors.New("TAK server sent no certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			inter := x509.NewCertPool()
			for _, r := range raw[1:] {
				if ic, err := x509.ParseCertificate(r); err == nil {
					inter.AddCert(ic)
				}
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
			if err != nil {
				return fmt.Errorf("TAK server certificate not signed by its enrollment CA: %w", err)
			}
			return nil
		},
		MinVersion: tls.VersionTLS12,
	}, nil
}

// --- connection ----------------------------------------------------------------

func (s *Server) startTak(cfg TakConfig) {
	m := s.tak()
	m.mu.Lock()
	if old := m.conns[cfg.AgencyID]; old != nil {
		old.close()
	}
	tc := &takConn{s: s, cfg: cfg, status: "connecting", units: map[string]*TakUnit{}, stop: make(chan struct{})}
	m.conns[cfg.AgencyID] = tc
	m.cfgs[cfg.AgencyID] = cfg
	m.mu.Unlock()
	go tc.run()
}

func (s *Server) stopTak(agency string) {
	m := s.tak()
	m.mu.Lock()
	if tc := m.conns[agency]; tc != nil {
		tc.close()
		delete(m.conns, agency)
	}
	m.mu.Unlock()
}

func (t *takConn) close() {
	t.mu.Lock()
	select {
	case <-t.stop:
	default:
		close(t.stop)
	}
	if t.conn != nil {
		t.conn.Close()
	}
	t.mu.Unlock()
}

func (t *takConn) setStatus(status, msg string) {
	t.mu.Lock()
	t.status, t.errMsg = status, msg
	t.mu.Unlock()
}

func (t *takConn) run() {
	backoff := 2 * time.Second
	for {
		select {
		case <-t.stop:
			return
		default:
		}
		err := t.session()
		select {
		case <-t.stop:
			return
		default:
		}
		msg := "connection closed"
		if err != nil {
			msg = err.Error()
		}
		t.setStatus("error", msg)
		log.Printf("TAK [%s]: %s — retrying in %v", t.cfg.AgencyID, msg, backoff)
		select {
		case <-t.stop:
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (t *takConn) session() error {
	tlsCfg, err := t.cfg.tlsConfig()
	if err != nil {
		return err
	}
	t.setStatus("connecting", "")
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort(t.cfg.Host, strconv.Itoa(t.cfg.StreamPort)), tlsCfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	t.mu.Lock()
	t.conn = conn
	t.since = time.Now()
	t.mu.Unlock()
	defer conn.Close()
	t.setStatus("connected", "")
	log.Printf("TAK [%s]: connected to %s:%d as %q", t.cfg.AgencyID, t.cfg.Host, t.cfg.StreamPort, t.cfg.Callsign)
	t.mu.Lock()
	current := t.cfg
	t.mu.Unlock()
	if len(current.Groups) > 0 {
		// TAK ties group choices to the streaming client by UID, which it
		// learns from our first presence message — so apply a moment later.
		go func(cfg TakConfig) {
			time.Sleep(3 * time.Second)
			if err := takApplyGroups(&cfg); err != nil {
				log.Printf("TAK [%s]: could not set groups: %v", cfg.AgencyID, err)
			} else {
				log.Printf("TAK [%s]: active groups %v", cfg.AgencyID, cfg.Groups)
			}
		}(current)
	}

	// Presence every 30 s (also the keepalive).
	done := make(chan struct{})
	defer close(done)
	go func() {
		t.send(t.presenceCoT())
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				t.send(t.presenceCoT())
				t.send(t.pingCoT())
				t.pruneStale()
			}
		}
	}()

	dec := xml.NewDecoder(conn)
	dec.Strict = false
	for {
		conn.SetReadDeadline(time.Now().Add(3 * time.Minute)) // pings keep a quiet server talking
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "event" {
			continue
		}
		var ev cotEvent
		if err := dec.DecodeElement(&ev, &se); err != nil {
			continue
		}
		t.handle(&ev)
	}
}

func (t *takConn) send(data string) {
	t.mu.Lock()
	c := t.conn
	t.mu.Unlock()
	if c == nil {
		return
	}
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(data)); err != nil {
		log.Printf("TAK [%s]: write failed: %v", t.cfg.AgencyID, err)
		c.Close()
	}
}

func (t *takConn) pruneStale() {
	now := time.Now()
	t.mu.Lock()
	for uid, u := range t.units {
		if now.After(u.Stale.Add(5 * time.Minute)) {
			delete(t.units, uid)
		}
	}
	t.mu.Unlock()
}

// --- CoT ----------------------------------------------------------------------

type cotEvent struct {
	Type  string `xml:"type,attr"`
	UID   string `xml:"uid,attr"`
	Time  string `xml:"time,attr"`
	Stale string `xml:"stale,attr"`
	Point struct {
		Lat float64 `xml:"lat,attr"`
		Lon float64 `xml:"lon,attr"`
	} `xml:"point"`
	Detail struct {
		Contact *struct {
			Callsign string `xml:"callsign,attr"`
		} `xml:"contact"`
		Group *struct {
			Name string `xml:"name,attr"`
			Role string `xml:"role,attr"`
		} `xml:"__group"`
		Status *struct {
			Battery string `xml:"battery,attr"`
		} `xml:"status"`
		Track *struct {
			Course string `xml:"course,attr"`
			Speed  string `xml:"speed,attr"`
		} `xml:"track"`
		Takv *struct {
			Device string `xml:"device,attr"`
		} `xml:"takv"`
		Chat *struct {
			ID             string `xml:"id,attr"`
			Chatroom       string `xml:"chatroom,attr"`
			SenderCallsign string `xml:"senderCallsign,attr"`
			Chatgrp        *struct {
				UID0 string `xml:"uid0,attr"`
				UID1 string `xml:"uid1,attr"`
			} `xml:"chatgrp"`
		} `xml:"__chat"`
		Remarks   string `xml:"remarks"`
		Emergency *struct {
			Type   string `xml:"type,attr"`
			Cancel string `xml:"cancel,attr"`
			Text   string `xml:",chardata"`
		} `xml:"emergency"`
		Link []struct {
			UID string `xml:"uid,attr"`
		} `xml:"link"`
	} `xml:"detail"`
}

func parseCotTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Now()
	}
	return t
}

func (t *takConn) handle(ev *cotEvent) {
	agency := t.cfg.AgencyID
	switch {
	case ev.UID == t.cfg.UID:
		return // our own presence echoed back
	case ev.Type == "b-t-f": // GeoChat
		t.handleChat(ev)
	case strings.HasPrefix(ev.Type, "b-a-o-") || ev.Detail.Emergency != nil:
		t.handleEmergency(ev)
	case ev.Type == "t-x-d-d": // delete
		for _, l := range ev.Detail.Link {
			t.mu.Lock()
			delete(t.units, l.UID)
			t.mu.Unlock()
		}
	case strings.HasPrefix(ev.Type, "a-") && ev.Detail.Contact != nil && ev.Detail.Contact.Callsign != "":
		if !validFix(ev.Point.Lat, ev.Point.Lon) {
			return
		}
		u := &TakUnit{UID: ev.UID, Callsign: ev.Detail.Contact.Callsign, Lat: ev.Point.Lat, Lon: ev.Point.Lon,
			At: parseCotTime(ev.Time), Stale: parseCotTime(ev.Stale)}
		if g := ev.Detail.Group; g != nil {
			u.Group, u.Role = g.Name, g.Role
		}
		if st := ev.Detail.Status; st != nil {
			u.Battery, _ = strconv.Atoi(st.Battery)
		}
		if tr := ev.Detail.Track; tr != nil {
			u.Speed, _ = strconv.ParseFloat(tr.Speed, 64)
			u.Course, _ = strconv.ParseFloat(tr.Course, 64)
		}
		if tv := ev.Detail.Takv; tv != nil {
			u.Device = tv.Device
		}
		t.mu.Lock()
		t.units[ev.UID] = u
		t.mu.Unlock()
	}
	_ = agency
}

func (t *takConn) handleChat(ev *cotEvent) {
	ch := ev.Detail.Chat
	if ch == nil {
		return
	}
	to := ch.ID
	from := ""
	if ch.Chatgrp != nil {
		to = ch.Chatgrp.UID1
		from = ch.Chatgrp.UID0
	}
	if to != t.cfg.UID && ch.Chatroom != t.cfg.Callsign {
		return // not for dispatch (team chats, All Chat Rooms)
	}
	text := strings.TrimSpace(ev.Detail.Remarks)
	if text == "" {
		return
	}
	if len(text) > maxMessageLen {
		text = text[:maxMessageLen]
	}
	name := nonEmpty(ch.SenderCallsign, from)
	t.s.recordMessage(dispatchMsg{At: time.Now().UnixMilli(), Agency: t.cfg.AgencyID, Dir: "in",
		From: name, To: "Dispatch", Text: text, TakUID: from})
}

// TAK alert types (ATAK's emergency button).
var takAlertNames = map[string]string{
	"b-a-o-tbl": "911 Alert", "b-a-o-pan": "Ring The Bell", "b-a-o-opn": "Troops In Contact",
	"b-a-o-c": "Custom Alert", "b-a-g": "Geo-fence Breached",
}

func (t *takConn) handleEmergency(ev *cotEvent) {
	active := ev.Type != "b-a-o-can"
	name := ""
	if e := ev.Detail.Emergency; e != nil {
		if strings.EqualFold(e.Cancel, "true") {
			active = false
		}
		name = strings.TrimSpace(e.Text)
	}
	if name == "" && ev.Detail.Contact != nil {
		name = ev.Detail.Contact.Callsign
	}
	if name == "" {
		name = ev.UID
	}
	// MeshRelay's own emergencies come back from TAK too: not a new alert.
	if strings.HasPrefix(ev.UID, "MeshRelay-911-") {
		return
	}
	msg := map[string]interface{}{
		"type": "emergency", "agency": t.cfg.AgencyID, "channel": -1, "user": name + " (TAK)",
		"active": active, "tak": true, "alert": takAlertNames[ev.Type],
	}
	if validFix(ev.Point.Lat, ev.Point.Lon) {
		msg["lat"], msg["lon"] = ev.Point.Lat, ev.Point.Lon
	}
	log.Printf("TAK [%s]: emergency %v from %s (%s)", t.cfg.AgencyID, active, name, ev.Type)
	t.s.sendToConsoles(t.cfg.AgencyID, msg)
}

func xmlEsc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return strings.ReplaceAll(b.String(), `"`, "&quot;")
}

func cotTimes(staleAfter time.Duration) (string, string) {
	now := time.Now().UTC()
	return now.Format("2006-01-02T15:04:05.000Z"), now.Add(staleAfter).Format("2006-01-02T15:04:05.000Z")
}

// presenceCoT makes dispatch a contact TAK users can chat with. It sits at the
// middle of the agency's MeshRelay units when any have a position.
func (t *takConn) presenceCoT() string {
	lat, lon := t.s.agencyCentroid(t.cfg.AgencyID)
	now, stale := cotTimes(2 * time.Minute)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><event version="2.0" type="a-f-G-U-C" uid="%s" how="h-g-i-g-o" time="%s" start="%s" stale="%s"><point lat="%f" lon="%f" hae="9999999" ce="9999999" le="9999999"/><detail><contact callsign="%s" endpoint="*:-1:stcp"/><__group name="Cyan" role="HQ"/><takv device="MeshRelay Server" platform="MeshRelay Dispatch" os="" version="%s"/><uid Droid="%s"/></detail></event>`,
		xmlEsc(t.cfg.UID), now, now, stale, lat, lon, xmlEsc(t.cfg.Callsign), xmlEsc(Version), xmlEsc(t.cfg.Callsign))
}

// pingCoT: TAK's keepalive (the server answers t-x-c-t-r), so a quiet server
// still sends something before the read deadline.
func (t *takConn) pingCoT() string {
	now, stale := cotTimes(20 * time.Second)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><event version="2.0" type="t-x-c-t" uid="%s-ping" how="m-g" time="%s" start="%s" stale="%s"><point lat="0" lon="0" hae="0" ce="9999999" le="9999999"/><detail/></event>`,
		xmlEsc(t.cfg.UID), now, now, stale)
}

// chatCoT builds a direct GeoChat exactly the way ATAK does on this kind of
// server (compared against a real ATAK message): a UUID message ID, how
// "h-g-i-g-o", and no <marti><dest> — the TAK server hands it to the group
// and the recipient keeps it because chatgrp/id name its UID.
func (t *takConn) chatCoT(toUID, toCallsign, text string) string {
	now, stale := cotTimes(24 * time.Hour)
	msgID := newUUID()
	evUID := "GeoChat." + t.cfg.UID + "." + toUID + "." + msgID
	lat, lon := t.s.agencyCentroid(t.cfg.AgencyID)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><event version="2.0" uid="%s" type="b-t-f" how="h-g-i-g-o" time="%s" start="%s" stale="%s"><point lat="%f" lon="%f" hae="9999999.0" ce="9999999.0" le="9999999.0"/><detail><__chat parent="RootContactGroup" groupOwner="false" messageId="%s" chatroom="%s" id="%s" senderCallsign="%s"><chatgrp uid0="%s" uid1="%s" id="%s"/></__chat><link uid="%s" type="a-f-G-U-C" relation="p-p"/><remarks source="BAO.F.ATAK.%s" to="%s" time="%s">%s</remarks></detail></event>`,
		xmlEsc(evUID), now, now, stale, lat, lon, msgID, xmlEsc(toCallsign), xmlEsc(toUID), xmlEsc(t.cfg.Callsign),
		xmlEsc(t.cfg.UID), xmlEsc(toUID), xmlEsc(toUID), xmlEsc(t.cfg.UID), xmlEsc(t.cfg.UID), xmlEsc(toUID), now, xmlEsc(text))
}

// newUUID returns a random (version 4) UUID string.
func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (t *takConn) emergencyCoT(user string, lat, lon float64, active bool) string {
	now, stale := cotTimes(10 * time.Minute)
	uid := "MeshRelay-911-" + t.cfg.AgencyID + "-" + user
	typ, emerg := "b-a-o-tbl", `<emergency type="911 Alert">`+xmlEsc(user)+`</emergency>`
	if !active {
		typ, emerg = "b-a-o-can", `<emergency cancel="true">`+xmlEsc(user)+`</emergency>`
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><event version="2.0" type="%s" uid="%s" how="m-g" time="%s" start="%s" stale="%s"><point lat="%f" lon="%f" hae="9999999" ce="9999999" le="9999999"/><detail><contact callsign="%s-Alert"/><link uid="%s" type="a-f-G-U-C" relation="p-p"/>%s<remarks>MeshRelay emergency: %s</remarks></detail></event>`,
		typ, xmlEsc(uid), now, now, stale, lat, lon, xmlEsc(user), xmlEsc(t.cfg.UID), emerg, xmlEsc(user))
}

// --- server-side hooks ------------------------------------------------------------

func (s *Server) agencyCentroid(agency string) (float64, float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var lat, lon float64
	n := 0
	for _, c := range s.clients {
		if c.AgencyID == agency && c.Loc != nil {
			lat += c.Loc.Lat
			lon += c.Loc.Lon
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return lat / float64(n), lon / float64(n)
}

// takUnitsFor lists TAK users for consoles of an agency ("" = every agency).
// MeshRelay phones that also report into TAK are left out when they are on the
// MeshRelay server already (same callsign) — the console shows them once.
func (s *Server) takUnitsFor(agencyFilter string) []map[string]interface{} {
	m := s.tak()
	m.mu.Lock()
	conns := make([]*takConn, 0, len(m.conns))
	for a, c := range m.conns {
		if agencyFilter == "" || a == agencyFilter {
			conns = append(conns, c)
		}
	}
	m.mu.Unlock()

	onMesh := map[string]bool{}
	s.mu.RLock()
	for _, c := range s.clients {
		onMesh[c.AgencyID+"\x00"+strings.ToLower(c.Username)] = true
	}
	s.mu.RUnlock()

	out := []map[string]interface{}{}
	now := time.Now()
	for _, tc := range conns {
		tc.mu.Lock()
		for _, u := range tc.units {
			if onMesh[tc.cfg.AgencyID+"\x00"+strings.ToLower(u.Callsign)] {
				continue
			}
			out = append(out, map[string]interface{}{
				"uid": u.UID, "callsign": u.Callsign, "agency": tc.cfg.AgencyID,
				"lat": u.Lat, "lon": u.Lon, "at": u.At.Format(time.RFC3339), "stale": now.After(u.Stale),
				"battery": u.Battery, "speed": u.Speed, "course": u.Course, "group": u.Group, "role": u.Role,
				"device": u.Device,
			})
		}
		tc.mu.Unlock()
	}
	return out
}

func (s *Server) takConnFor(agency string) *takConn {
	m := s.tak()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[agency]
}

// consoleSendTakMessage sends an operator's message to a TAK user as GeoChat.
func (s *Server) consoleSendTakMessage(console *Client, agency, uid, text string) {
	text = strings.TrimSpace(text)
	if text == "" || (console.AgencyID != "" && console.AgencyID != agency) {
		return
	}
	if len(text) > maxMessageLen {
		text = text[:maxMessageLen]
	}
	tc := s.takConnFor(agency)
	callsign := uid
	sent := 0
	if tc != nil {
		tc.mu.Lock()
		if u := tc.units[uid]; u != nil {
			callsign = u.Callsign
		}
		connected := tc.status == "connected"
		tc.mu.Unlock()
		if connected {
			tc.send(tc.chatCoT(uid, callsign, text))
			sent = 1
		}
	}
	s.recordMessage(dispatchMsg{At: time.Now().UnixMilli(), Agency: agency, Dir: "out", From: console.Username,
		To: callsign + " (TAK)", Text: text, Sent: sent, TakUID: uid})
}

// takEmergency mirrors a MeshRelay emergency into the agency's TAK server.
func (s *Server) takEmergency(agency, user string, active bool) {
	tc := s.takConnFor(agency)
	if tc == nil {
		return
	}
	var lat, lon float64
	s.mu.RLock()
	if c := s.clients[userKey(agency, user)]; c != nil && c.Loc != nil {
		lat, lon = c.Loc.Lat, c.Loc.Lon
	}
	s.mu.RUnlock()
	tc.send(tc.emergencyCoT(user, lat, lon, active))
}

// --- dashboard API -------------------------------------------------------------------

func (s *Server) takStatusJSON(agency string) map[string]interface{} {
	m := s.tak()
	m.mu.Lock()
	cfg, ok := m.cfgs[agency]
	tc := m.conns[agency]
	m.mu.Unlock()
	out := map[string]interface{}{"agency_id": agency, "configured": ok}
	if !ok {
		out["stream_port"], out["enroll_port"], out["api_port"], out["callsign"] = 8089, 8446, 8443, "MeshRelay Dispatch"
		return out
	}
	out["host"], out["stream_port"], out["enroll_port"] = cfg.Host, cfg.StreamPort, cfg.EnrollPort
	out["api_port"], out["groups"] = cfg.APIPort, cfg.Groups
	if cfg.APIPort <= 0 {
		out["api_port"] = 8443
	}
	out["username"], out["callsign"], out["enabled"] = cfg.Username, cfg.Callsign, cfg.Enabled
	out["enrolled"] = cfg.CertPEM != ""
	out["enrolled_at"] = cfg.EnrolledAt
	out["managed"] = cfg.Managed
	if block, _ := pem.Decode([]byte(cfg.CertPEM)); block != nil {
		if c, err := x509.ParseCertificate(block.Bytes); err == nil {
			out["cert_expires"] = c.NotAfter.Format(time.RFC3339)
		}
	}
	out["status"] = "disabled"
	if tc != nil {
		tc.mu.Lock()
		out["status"], out["error"], out["tak_users"] = tc.status, tc.errMsg, len(tc.units)
		tc.mu.Unlock()
	}
	return out
}

func (s *Server) handleGetTak(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	if isSuper {
		if q := r.URL.Query().Get("agency"); q != "" {
			agencyID = q
		}
	}
	json.NewEncoder(w).Encode(s.takStatusJSON(agencyID))
}

func (s *Server) handleUpdateTak(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var req struct {
		AgencyID   string `json:"agency_id"`
		Host       string `json:"host"`
		StreamPort int    `json:"stream_port"`
		EnrollPort int    `json:"enroll_port"`
		APIPort    int    `json:"api_port"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		Callsign   string `json:"callsign"`
		Enabled    bool   `json:"enabled"`
		Remove     bool   `json:"remove"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if isSuper && req.AgencyID != "" {
		agencyID = req.AgencyID
	}
	s.mu.RLock()
	_, known := s.agencies[agencyID]
	s.mu.RUnlock()
	if !known {
		writeJSONError(w, http.StatusNotFound, "agency not found")
		return
	}
	m := s.tak()
	if req.Remove {
		s.stopTak(agencyID)
		m.mu.Lock()
		delete(m.cfgs, agencyID)
		m.mu.Unlock()
		s.saveTakConnections()
		log.Printf("TAK [%s]: connection removed", agencyID)
		json.NewEncoder(w).Encode(s.takStatusJSON(agencyID))
		return
	}

	m.mu.Lock()
	cfg, existed := m.cfgs[agencyID]
	m.mu.Unlock()
	host := strings.TrimSpace(req.Host)
	if host == "" {
		writeJSONError(w, http.StatusBadRequest, "TAK server address is required")
		return
	}
	reEnroll := req.Password != "" || !existed || host != cfg.Host || req.Username != cfg.Username || req.EnrollPort != cfg.EnrollPort
	cfg.AgencyID, cfg.Host, cfg.Enabled = agencyID, host, req.Enabled
	cfg.Managed = false
	cfg.StreamPort, cfg.EnrollPort = req.StreamPort, req.EnrollPort
	if cfg.StreamPort <= 0 {
		cfg.StreamPort = 8089
	}
	if cfg.EnrollPort <= 0 {
		cfg.EnrollPort = 8446
	}
	cfg.APIPort = req.APIPort
	if cfg.APIPort <= 0 {
		cfg.APIPort = 8443
	}
	cfg.Username = strings.TrimSpace(req.Username)
	cfg.Callsign = nonEmpty(strings.TrimSpace(req.Callsign), "MeshRelay Dispatch")
	if cfg.UID == "" {
		cfg.UID = "MeshRelay-Dispatch-" + safeName.ReplaceAllString(agencyID, "_") + "-" + randomHex(3)
	}
	if reEnroll {
		if req.Password == "" {
			writeJSONError(w, http.StatusBadRequest, "Enter the TAK password to enroll for a certificate")
			return
		}
		if err := takEnroll(&cfg, req.Password); err != nil {
			log.Printf("TAK [%s]: enrollment failed: %v", agencyID, err)
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		log.Printf("TAK [%s]: enrolled with %s as %s", agencyID, cfg.Host, cfg.Username)
	}
	m.mu.Lock()
	m.cfgs[agencyID] = cfg
	m.mu.Unlock()
	if err := s.saveTakConnections(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.Enabled {
		s.startTak(cfg)
		time.Sleep(1500 * time.Millisecond) // let the first connect attempt report
	} else {
		s.stopTak(agencyID)
	}
	json.NewEncoder(w).Encode(s.takStatusJSON(agencyID))
}

// --- TAK groups ("channels") ------------------------------------------------------

// TakGroup is one TAK group as the dashboard shows it (TAK lists each group
// twice, once per direction).
type TakGroup struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Active      bool   `json:"active"` // active for dispatch's connection
}

func (c *TakConfig) apiBase() string {
	port := c.APIPort
	if port <= 0 {
		port = 8443
	}
	return fmt.Sprintf("https://%s:%d", c.Host, port)
}

func (c *TakConfig) apiClient() (*http.Client, error) {
	tc, err := c.tlsConfig()
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}, nil
}

// takAllGroups returns TAK's raw group list for this certificate (both
// directions, every field — PUT back unchanged apart from "active").
func takAllGroups(c *TakConfig) ([]map[string]interface{}, error) {
	cl, err := c.apiClient()
	if err != nil {
		return nil, err
	}
	resp, err := cl.Get(c.apiBase() + "/Marti/api/groups/all?useCache=true")
	if err != nil {
		return nil, fmt.Errorf("TAK API: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TAK API returned %d listing groups", resp.StatusCode)
	}
	var wrap struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, errors.New("TAK API returned an unreadable group list")
	}
	return wrap.Data, nil
}

// takListGroups: one row per group name, active if dispatch is in it.
func takListGroups(c *TakConfig) ([]TakGroup, error) {
	raw, err := takAllGroups(c)
	if err != nil {
		return nil, err
	}
	byName := map[string]*TakGroup{}
	var order []string
	for _, g := range raw {
		name, _ := g["name"].(string)
		if name == "" {
			continue
		}
		tg := byName[name]
		if tg == nil {
			tg = &TakGroup{Name: name}
			byName[name] = tg
			order = append(order, name)
		}
		if d, _ := g["description"].(string); d != "" {
			tg.Description = d
		}
		if a, _ := g["active"].(bool); a {
			tg.Active = true
		}
	}
	sort.Strings(order)
	out := make([]TakGroup, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, nil
}

// takApplyGroups makes exactly cfg.Groups active (both directions) for
// dispatch's streaming connection.
func takApplyGroups(c *TakConfig) error {
	raw, err := takAllGroups(c)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, g := range c.Groups {
		want[g] = true
	}
	for _, g := range raw {
		name, _ := g["name"].(string)
		g["active"] = want[name]
	}
	body, _ := json.Marshal(raw)
	cl, err := c.apiClient()
	if err != nil {
		return err
	}
	req, _ := http.NewRequest("PUT", c.apiBase()+"/Marti/api/groups/active?clientUid="+urlQueryEscape(c.UID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("TAK API: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("TAK API returned %d setting groups", resp.StatusCode)
	}
	return nil
}

func (s *Server) handleGetTakGroups(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	if isSuper {
		if q := r.URL.Query().Get("agency"); q != "" {
			agencyID = q
		}
	}
	m := s.tak()
	m.mu.Lock()
	cfg, ok := m.cfgs[agencyID]
	m.mu.Unlock()
	if !ok || cfg.CertPEM == "" {
		writeJSONError(w, http.StatusBadRequest, "Connect to the TAK Server first")
		return
	}
	groups, err := takListGroups(&cfg)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"groups": groups, "selected": cfg.Groups})
}

func (s *Server) handleUpdateTakGroups(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	agencyID, isSuper := s.callerAgency(r)
	var req struct {
		AgencyID string   `json:"agency_id"`
		Groups   []string `json:"groups"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if isSuper && req.AgencyID != "" {
		agencyID = req.AgencyID
	}
	m := s.tak()
	m.mu.Lock()
	cfg, ok := m.cfgs[agencyID]
	if ok {
		cfg.Groups = req.Groups
		m.cfgs[agencyID] = cfg
		if tc := m.conns[agencyID]; tc != nil {
			tc.mu.Lock()
			tc.cfg.Groups = req.Groups
			tc.mu.Unlock()
		}
	}
	m.mu.Unlock()
	if !ok || cfg.CertPEM == "" {
		writeJSONError(w, http.StatusBadRequest, "Connect to the TAK Server first")
		return
	}
	if len(req.Groups) == 0 {
		writeJSONError(w, http.StatusBadRequest, "Pick at least one group")
		return
	}
	if err := takApplyGroups(&cfg); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.saveTakConnections()
	log.Printf("TAK [%s]: active groups set to %v", agencyID, req.Groups)
	groups, err := takListGroups(&cfg)
	if err != nil {
		groups = nil
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"groups": groups, "selected": cfg.Groups})
}
