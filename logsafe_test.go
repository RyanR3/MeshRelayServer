// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Names that come from the network or from what an admin typed. A log call
// that prints one of them must use %q (logsafe.go).
var (
	untrustedIdents = map[string]bool{
		"username": true, "user": true, "u": true, "callsign": true, "name": true, "who": true,
		"by": true, "createdBy": true, "channelName": true, "account": true, "newUsername": true,
		"old": true, "target": true, "gw": true, "host": true, "userMCXID": true, "msg": true,
		"errStr": true, "logMsg": true, "oldKey": true,
	}
	untrustedFields = map[string]bool{
		"Username": true, "User": true, "Sender": true, "Recipient": true, "Creator": true,
		"InvitedBy": true, "CreatedBy": true, "Name": true, "ChannelName": true, "ZelloChannel": true,
		"UID": true, "Type": true, "Host": true, "Network": true, "From": true, "To": true,
		"Account": true, "AgencyID": true, "ToAgency": true, "Agency": true, "Model": true,
		"CommonName": true, "user": true, "username": true, "sessionUser": true,
	}
	// Receivers whose AgencyID / Agency is the server's own, validated id
	// (an agency that exists), not text from a request.
	trustedAgencyOwners = map[string]bool{
		"client": true, "c": true, "u": true, "user": true, "b": true, "t.cfg": true, "cfg": true,
		"clientToRemove": true, "inviter": true, "ev": true, "m": true, "session": true, "mar": true,
		"et": true, "t": true, "sender": true, "link": true,
	}
)

func exprText(fs *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	_ = printer.Fprint(&b, fs, e)
	return b.String()
}

// untrustedIn reports whether expression e prints a name from the network.
func untrustedIn(fs *token.FileSet, e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.IndexExpr:
			// map[bool]string{...}[cond]: prints one of the constants.
			if _, ok := x.X.(*ast.CompositeLit); ok {
				return false
			}
		case *ast.Ident:
			if untrustedIdents[x.Name] {
				found = true
			}
		case *ast.SelectorExpr:
			f := x.Sel.Name
			if f == "AgencyID" || f == "Agency" {
				if trustedAgencyOwners[exprText(fs, x.X)] {
					return false
				}
			}
			if untrustedFields[f] {
				found = true
			}
			// Only the field name counts, not the receiver ("user.AgencyID").
			ast.Inspect(x.X, func(m ast.Node) bool {
				if c, ok := m.(*ast.CallExpr); ok {
					for _, a := range c.Args {
						if untrustedIn(fs, a) {
							found = true
						}
					}
					return false
				}
				return true
			})
			return false
		}
		return true
	})
	return found
}

type logFinding struct {
	pos  token.Position
	verb string
	arg  string
}

// unsafeLogArgs lists log calls in the package that print an untrusted name
// with a verb other than %q (or %d etc.).
func unsafeLogArgs(t *testing.T, dir string) []logFinding {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fs := token.NewFileSet()
	var out []logFinding
	for _, fn := range files {
		if strings.HasSuffix(fn, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fs, fn, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "log" || !strings.HasSuffix(sel.Sel.Name, "f") || len(c.Args) == 0 {
				return true
			}
			lit, ok := c.Args[0].(*ast.BasicLit)
			if !ok {
				return true // a built format: checked by hand (zellobridge dropLog)
			}
			format, _ := strconv.Unquote(lit.Value)
			for i, v := range formatVerbs(format) {
				if (v != 's' && v != 'v') || i+1 >= len(c.Args) {
					continue
				}
				if untrustedIn(fs, c.Args[i+1]) {
					out = append(out, logFinding{fs.Position(c.Pos()), "%" + string(v), exprText(fs, c.Args[i+1])})
				}
			}
			return true
		})
	}
	return out
}

// formatVerbs returns the verb letter of each argument-consuming directive.
func formatVerbs(f string) []byte {
	var out []byte
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			continue
		}
		i++
		for i < len(f) && strings.IndexByte("+-# 0123456789.", f[i]) >= 0 {
			i++
		}
		if i < len(f) && f[i] != '%' {
			out = append(out, f[i])
		}
	}
	return out
}

// Static check: no log line prints a network-derived name with %s / %v.
func TestLogCallsQuoteNetworkNames(t *testing.T) {
	for _, f := range unsafeLogArgs(t, ".") {
		t.Errorf("%s: %s prints %s with %s: use %%q (logsafe.go)", f.pos, f.verb, f.arg, f.verb)
	}
}

// The writer keeps every entry on one line.
func TestOneLineWriter(t *testing.T) {
	cases := map[string]string{
		"plain entry\n":                  "plain entry\n",
		"a\nb\n":                         `a\nb` + "\n",
		"cr\r\nfake line\n":              `cr\r\nfake line` + "\n",
		"esc \x1b[31mred\n":              `esc \x1b[31mred` + "\n",
		"sep \u2028 x\n":                 `sep \u2028 x` + "\n",
		"bad \xff utf8\n":                `bad \xff utf8` + "\n",
		"unicode stays: Engine 7 é 日本\n": "unicode stays: Engine 7 é 日本\n",
		"tab\there\n":                    `tab\there` + "\n",
		"no newline at end \n middle":    `no newline at end \n middle`,
	}
	for in, want := range cases {
		if got := string(oneLine([]byte(in))); got != want {
			t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// A sign-in with a CR/LF in the name writes exactly one log line, and the
// name appears quoted with the fixed text of the line unchanged.
func TestSignInWithLineBreakInNameLogsOneLine(t *testing.T) {
	s, sink := newSecTestServer(t)
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&oneLineWriter{w: &buf})
	log.SetFlags(log.LstdFlags)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	evil := "bob\r\n2026/11/26 01:00:00 Auth (challenge-response) success: user admin from 10.9.9.9:5000"
	a := udp(9555)
	// Through the real parser, as it arrives off the wire.
	pkt := []byte{PacketTypeAuthRequest}
	pkt = putStrBytes(pkt, evil)
	pkt = putStrBytes(pkt, strings.Repeat("0", 64))
	pkt = putStrBytes(pkt, "")
	s.handlePacket(pkt, a)
	sink.take(a)

	out := buf.String()
	if n := strings.Count(out, "\n"); n != 1 {
		t.Fatalf("one sign-in wrote %d log lines:\n%s", n, out)
	}
	if !strings.Contains(out, `Auth failed: unknown user "bob\r\n2026/11/26 01:00:00 Auth (challenge-response) success: user admin from 10.9.9.9:5000" from 10.0.0.1:9555`) {
		t.Fatalf("unexpected log line: %s", out)
	}

	// The same through the secure-protocol log lines and a JOIN: still one
	// line per event.
	buf.Reset()
	s.handlePacket(append([]byte{PacketTypeJoin}, putStrBytes(nil, "x\ny")...), a)
	if strings.Count(buf.String(), "\n") > 1 {
		t.Fatalf("JOIN wrote several lines: %q", buf.String())
	}
}

func putStrBytes(b []byte, v string) []byte {
	b = append(b, byte(len(v)>>8), byte(len(v)))
	return append(b, v...)
}
