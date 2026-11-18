// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two agencies, one keeping 7 days and one using the server's 30: a 10-day
// old day loses only the 7-day agency's recordings (files, index lines and
// transcripts); a 40-day old day goes entirely.
func TestPerAgencyRetention(t *testing.T) {
	dir := t.TempDir()
	s := &Server{agencies: map[string]*Agency{
		"short": {ID: "short", RecordingRetentionDays: 7},
		"long":  {ID: "long"},
	}}
	r := &Recorder{s: s, dir: dir, retention: 30, active: map[string]*activeRec{}}
	now := time.Now()

	mk := func(age int, agency, id string) {
		day := now.AddDate(0, 0, -age).Format(recDayFmt)
		os.MkdirAll(filepath.Join(dir, day, agency), 0o755)
		os.WriteFile(filepath.Join(dir, day, agency, id+".mrr"), []byte("x"), 0o644)
		info := RecordingInfo{ID: id, Agency: agency, Start: now.AddDate(0, 0, -age).UnixMilli(), File: filepath.Join(day, agency, id+".mrr")}
		r.appendIndex(info)
		r.appendTranscript(info, transcriptLine{ID: id, Text: "said " + id})
	}
	mk(10, "short", "s10")
	mk(10, "long", "l10")
	mk(3, "short", "s3")
	mk(40, "long", "l40")

	r.prune(now)

	day10 := now.AddDate(0, 0, -10).Format(recDayFmt)
	if _, err := os.Stat(filepath.Join(dir, day10, "short")); !os.IsNotExist(err) {
		t.Fatal("7-day agency's 10-day-old recordings were kept")
	}
	if _, err := os.Stat(filepath.Join(dir, day10, "long", "l10.mrr")); err != nil {
		t.Fatal("30-day agency's 10-day-old recording was deleted")
	}
	idx, _ := os.ReadFile(filepath.Join(dir, day10, "index.jsonl"))
	trs, _ := os.ReadFile(filepath.Join(dir, day10, "transcripts.jsonl"))
	if strings.Contains(string(idx), "s10") || !strings.Contains(string(idx), "l10") ||
		strings.Contains(string(trs), "s10") || !strings.Contains(string(trs), "l10") {
		t.Fatalf("index/transcripts not rewritten:\n%s\n%s", idx, trs)
	}
	if got := r.Query(day10, "", "", -1, "", 0); len(got) != 1 || got[0].ID != "l10" || got[0].Text != "said l10" {
		b, _ := json.Marshal(got)
		t.Fatalf("Query after prune = %s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, now.AddDate(0, 0, -3).Format(recDayFmt), "short", "s3.mrr")); err != nil {
		t.Fatal("recent recording deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, now.AddDate(0, 0, -40).Format(recDayFmt))); !os.IsNotExist(err) {
		t.Fatal("40-day-old day folder kept")
	}
}
