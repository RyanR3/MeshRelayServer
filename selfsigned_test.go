// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
)

func TestSelfSignedTLS(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)

	// Off unless asked for; a configured certificate always wins.
	c := &Config{}
	if ensureSelfSignedTLS(c); c.TLSCertFile != "" {
		t.Fatal("made a certificate without MESHRELAY_TLS_SELF_SIGNED")
	}
	c = &Config{TLSSelfSigned: true, TLSCertFile: "/etc/real.crt", TLSKeyFile: "/etc/real.key"}
	if ensureSelfSignedTLS(c); c.TLSCertFile != "/etc/real.crt" {
		t.Fatal("replaced a configured certificate")
	}

	c = &Config{TLSSelfSigned: true, TLSHosts: []string{"192.0.2.28", "meshrelay.example.org"}}
	if err := ensureSelfSignedTLS(c); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile); err != nil {
		t.Fatalf("unusable pair: %v", err)
	}
	if fi, _ := os.Stat(c.TLSKeyFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(c.TLSCertFile)
	b, _ := pem.Decode(raw)
	cert, _ := x509.ParseCertificate(b.Bytes)
	if err := cert.VerifyHostname("192.0.2.28"); err != nil {
		t.Fatalf("IP not in certificate: %v", err)
	}
	if err := cert.VerifyHostname("meshrelay.example.org"); err != nil {
		t.Fatalf("name not in certificate: %v", err)
	}

	// Same hosts: kept. New address: replaced.
	before := cert.SerialNumber.String()
	c2 := &Config{TLSSelfSigned: true, TLSHosts: []string{"meshrelay.example.org", "192.0.2.28"}}
	ensureSelfSignedTLS(c2)
	raw, _ = os.ReadFile(c2.TLSCertFile)
	b, _ = pem.Decode(raw)
	same, _ := x509.ParseCertificate(b.Bytes)
	if same.SerialNumber.String() != before {
		t.Fatal("re-issued for the same hosts")
	}
	c3 := &Config{TLSSelfSigned: true, TLSHosts: []string{"10.0.0.9"}}
	ensureSelfSignedTLS(c3)
	raw, _ = os.ReadFile(c3.TLSCertFile)
	b, _ = pem.Decode(raw)
	moved, _ := x509.ParseCertificate(b.Bytes)
	if moved.SerialNumber.String() == before || moved.VerifyHostname("10.0.0.9") != nil {
		t.Fatal("not re-issued for a new address")
	}
}

func TestSelfSignedFromEnv(t *testing.T) {
	t.Setenv("MESHRELAY_TLS_SELF_SIGNED", "1")
	t.Setenv("MESHRELAY_TLS_HOSTS", "192.0.2.28,meshrelay.example.org")
	c := &Config{}
	applyEnvOverrides(c)
	if !c.TLSSelfSigned || len(c.TLSHosts) != 2 {
		t.Fatalf("got %v %v", c.TLSSelfSigned, c.TLSHosts)
	}
}
