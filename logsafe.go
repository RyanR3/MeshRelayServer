// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

// Log injection guard.
//
// Names that arrive from the network or that an admin typed (usernames,
// callsigns, agency text in a request, bridge and channel names, TAK and
// Zello names, header values, file names) are printed with %q: a name with
// spaces or quotes stays one delimited token ("Engine 7", "O'Brien",
// "say \"hi\"") and a line break shows as \n instead of starting a new line.
// logsafe_test.go checks the source for log calls that print such a value
// with %s or %v.
//
// As a backstop for everything else (error texts that quote input, values
// not on that list), every log entry is written as exactly one line: the
// writer below escapes CR, LF and other control characters inside an entry.
// One entry can therefore never pose as two, whatever it prints.

import (
	"io"
	"log"
	"os"
	"strconv"
	"sync"
	"unicode/utf8"
)

func init() {
	log.SetOutput(&oneLineWriter{w: os.Stderr})
}

// oneLineWriter receives one log entry per Write (the log package guarantees
// that) and writes it with its inner control characters escaped.
type oneLineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (o *oneLineWriter) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, err := o.w.Write(oneLine(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// oneLine returns entry with every control character except the final
// newline escaped (\r, \n, \t, \x1b,   ...). Clean entries are returned
// as they are.
func oneLine(entry []byte) []byte {
	body := entry
	nl := len(body) > 0 && body[len(body)-1] == '\n'
	if nl {
		body = body[:len(body)-1]
	}
	if !needsEscape(body) {
		return entry
	}
	out := make([]byte, 0, len(body)+16)
	for i := 0; i < len(body); {
		r, size := utf8.DecodeRune(body[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			out = append(out, `\x`...)
			out = append(out, hexByte(body[i])...)
		case unsafeRune(r):
			q := strconv.QuoteRune(r) // '\n', '\x1b', ' '
			out = append(out, q[1:len(q)-1]...)
		default:
			out = append(out, body[i:i+size]...)
		}
		i += size
	}
	if nl {
		out = append(out, '\n')
	}
	return out
}

func needsEscape(b []byte) bool {
	for i := 0; i < len(b); {
		c := b[i]
		if c < utf8.RuneSelf {
			if c < 0x20 || c == 0x7f {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if (r == utf8.RuneError && size == 1) || unsafeRune(r) {
			return true
		}
		i += size
	}
	return false
}

// unsafeRune: characters a log viewer may treat as a line break or a
// terminal control: C0/C1 controls, DEL, and the Unicode line/paragraph
// separators.
func unsafeRune(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029
}

func hexByte(b byte) []byte {
	const digits = "0123456789abcdef"
	return []byte{digits[b>>4], digits[b&0xf]}
}
