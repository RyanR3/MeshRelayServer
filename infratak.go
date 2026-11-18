package main

// Running under a platform that installs the server for you (infra-TAK) or
// behind any reverse proxy.
//
// infra-TAK runs the server as a Docker container: settings come from the
// environment, the dashboard listens on loopback only and Caddy terminates
// HTTPS in front of it, and updates arrive as a new image, never through the
// dashboard's own updater.
//
// Environment (each overrides config.json):
//
//	MESHRELAY_PTT_PORT          UDP port for the phones (5000)
//	MESHRELAY_WEB_PORT          dashboard port (8080)
//	MESHRELAY_WEB_BIND          dashboard listen address, e.g. 127.0.0.1
//	MESHRELAY_TRUST_PROXY       1 = honour X-Forwarded-For/-Proto from a trusted proxy
//	MESHRELAY_TRUSTED_PROXIES   addresses/CIDRs the proxy connects from
//	                            (default loopback; in Docker the network gateway)
//	MESHRELAY_MANAGED           platform name ("infratak"): no self-update
//	MESHRELAY_WEB_DIR           dashboard files (./web)
//	MESHRELAY_RECORDING_DIR     radio recordings
//	MESHRELAY_TRANSCRIBE_DIR    speech-to-text models and settings
//	MESHRELAY_TRANSCRIBE_HELPER_DIR bundled speech-to-text helper builds
//	MESHRELAY_ADMIN_PASSWORD    initial super admin password (first start only)

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

func applyEnvOverrides(c *Config) {
	str := func(name string, dst *string) {
		if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
			*dst = strings.TrimSpace(v)
		}
	}
	num := func(name string, dst *int) {
		if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n <= 0 || n > 65535 {
				log.Printf("Ignoring %s=%q: not a port number", name, v)
				return
			}
			*dst = n
		}
	}
	num("MESHRELAY_PTT_PORT", &c.PTTPort)
	num("MESHRELAY_WEB_PORT", &c.WebPort)
	str("MESHRELAY_WEB_BIND", &c.WebBind)
	str("MESHRELAY_MANAGED", &c.Managed)
	str("MESHRELAY_WEB_DIR", &c.WebDir)
	str("MESHRELAY_RECORDING_DIR", &c.RecordingDir)
	str("MESHRELAY_TRANSCRIBE_DIR", &c.TranscribeDir)
	str("MESHRELAY_TRANSCRIBE_HELPER_DIR", &c.TranscribeHelperDir)
	if v := strings.TrimSpace(os.Getenv("MESHRELAY_TRUSTED_PROXIES")); v != "" {
		c.TrustedProxies = nil
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.TrustedProxies = append(c.TrustedProxies, p)
			}
		}
	}
	if v, ok := os.LookupEnv("MESHRELAY_TRUST_PROXY"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.TrustProxy = true
		case "0", "false", "no", "off":
			c.TrustProxy = false
		}
	}
	if v := os.Getenv("MESHRELAY_ADMIN_PASSWORD"); v != "" {
		c.AdminPassword = v
		// Not left in the environment for anything this process starts.
		os.Unsetenv("MESHRELAY_ADMIN_PASSWORD")
	}
}

// isManaged: a platform installs and updates this server.
func (c *Config) isManaged() bool { return c != nil && c.Managed != "" }

func (c *Config) webDir() string {
	if c != nil && c.WebDir != "" {
		return c.WebDir
	}
	return "./web"
}

// trustedProxyNets parses TrustedProxies (addresses or CIDRs); empty means
// loopback only. Bad entries are logged and skipped.
func (c *Config) trustedProxyNets() []*net.IPNet {
	list := c.TrustedProxies
	if len(list) == 0 {
		list = []string{"127.0.0.0/8", "::1/128"}
	}
	var out []*net.IPNet
	for _, e := range list {
		if !strings.Contains(e, "/") {
			if ip := net.ParseIP(e); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				e = fmt.Sprintf("%s/%d", ip.String(), bits)
			}
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			log.Printf("Ignoring trusted proxy %q: %v", e, err)
			continue
		}
		out = append(out, n)
	}
	return out
}

func remoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return net.ParseIP(host)
}

// fromTrustedProxy: forwarded headers count only when proxy mode is on AND
// the connection itself comes from the proxy's address. Anyone else could
// send the headers themselves.
//
// Docker: the dashboard port is published on the HOST's 127.0.0.1 only, so
// the only way in is from the host; inside the container those requests
// arrive from the compose network's gateway, which is what the compose file
// lists in MESHRELAY_TRUSTED_PROXIES. (Host networking would keep the real
// loopback address, but infra-TAK does not allow host namespaces.)
func (s *Server) fromTrustedProxy(r *http.Request) bool {
	if s.config == nil || !s.config.TrustProxy {
		return false
	}
	ip := remoteIP(r.RemoteAddr)
	if ip == nil {
		return false
	}
	s.proxyNetsOnce.Do(func() { s.proxyNets = s.config.trustedProxyNets() })
	for _, n := range s.proxyNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is the address of the person at the browser: the proxy's
// X-Forwarded-For when the request came through a trusted proxy, otherwise
// the connection's own address.
func (s *Server) clientIP(r *http.Request) string {
	if s.fromTrustedProxy(r) {
		// Caddy appends the address it saw to whatever the client sent, so
		// the LAST entry is the one the proxy vouches for.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requestIsHTTPS: the browser is on HTTPS, either to us directly or to the
// proxy in front of us. Decides the cookie's Secure flag.
func (s *Server) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.fromTrustedProxy(r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// handleHealth: unauthenticated liveness for the platform and the proxy.
// Says nothing about the server beyond "up".
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(`{"status":"ok"}`))
}

func managedUpdateMessage(platform string) string {
	if strings.EqualFold(platform, "infratak") {
		return "This server is managed by infra-TAK. Update it from the infra-TAK console (MeshRelay page, Update Now)."
	}
	return "This server is managed by " + platform + ", which installs its updates."
}
