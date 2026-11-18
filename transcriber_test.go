// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCleanTranscript(t *testing.T) {
	cases := []struct {
		in   string
		ms   int64
		want string
	}{
		{" Engine 41 on scene. ", 4000, "Engine 41 on scene."},
		{"[BLANK_AUDIO]", 1000, ""},
		{"you", 1200, ""},
		{"Thank you.", 900, ""},
		{"Thank you.", 5000, "Thank you."}, // a long transmission really said it
		{"Copy (static) that", 3000, "Copy that"},
		{"*radio chatter*", 2000, ""},
		{"10-4", 800, "10-4"},
	}
	for _, c := range cases {
		if got := cleanTranscript(c.in, c.ms); got != c.want {
			t.Errorf("cleanTranscript(%q, %d) = %q, want %q", c.in, c.ms, got, c.want)
		}
	}
}

// newTranscribeTestServer: a server with a recorder in a temp dir, agency
// "ag" (transcription on), a console of that agency, and a stand-in helper
// script that answers like mrtranscribe.
func newTranscribeTestServer(t *testing.T, helper string) (*Server, chan []byte) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell helper")
	}
	tmp := t.TempDir()
	s := &Server{
		clients:       map[string]*Client{},
		channels:      map[string]*Channel{},
		agencies:      map[string]*Agency{"ag": {ID: "ag", Name: "Agency", Transcribe: true, TranscribeWords: "Engine 41\tMedic 3"}, "other": {ID: "other", Name: "Other"}},
		dispatchConns: map[string]*DispatchConn{},
		config:        &Config{},
	}
	s.recorder = &Recorder{s: s, dir: filepath.Join(tmp, "rec"), retention: 30, active: map[string]*activeRec{}}
	trDir := filepath.Join(tmp, "tr")
	os.MkdirAll(trDir, 0o755)
	for _, f := range []string{"ggml-base.en.bin", vadModel.File} {
		os.WriteFile(filepath.Join(trDir, f), []byte("x"), 0o644)
	}
	os.WriteFile(filepath.Join(trDir, trHelperFile), []byte(helper), 0o755)
	os.WriteFile(filepath.Join(trDir, "settings.json"), []byte(`{"model":"base.en"}`), 0o644)
	s.transcriber = NewTranscriber(s, trDir)

	text := make(chan []byte, 16)
	s.clients["ag:console"] = &Client{Username: "console", AgencyID: "ag"}
	s.dispatchConns["ag:console"] = &DispatchConn{Text: text}
	return s, text
}

// The stand-in echoes the file name and the prompt so the test can check both.
const fakeHelper = `#!/bin/sh
echo '{"ready":true}'
while IFS='	' read -r file prompt; do
  name=$(basename "$file" .mrr)
  echo "{\"ok\":true,\"text\":\"heard $name with $prompt\",\"speech_ms\":900,\"audio_ms\":1500,\"ms\":5}"
done
`

func recordFor(t *testing.T, s *Server, agency, user string, frames int) RecordingInfo {
	t.Helper()
	r := s.recorder
	now := time.Now()
	a := r.open(agency, 0, user, now)
	for i := 0; i < frames; i++ {
		a.w.Write([]byte{0, 0, 0, byte(i * 20), 0, 1, 0xF8})
	}
	a.info.Frames = frames
	a.last = now.Add(time.Duration(frames*20) * time.Millisecond)
	a.w.Flush()
	a.f.Close()
	a.info.DurationMs = a.last.Sub(a.start).Milliseconds() + 20
	r.appendIndex(a.info)
	return a.info
}

func waitTranscript(t *testing.T, text chan []byte) map[string]interface{} {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case b := <-text:
			var m map[string]interface{}
			json.Unmarshal(b, &m)
			if m["type"] == "transcript" {
				return m
			}
		case <-deadline:
			t.Fatal("no transcript message")
		}
	}
}

func TestTranscriptionEndToEnd(t *testing.T) {
	s, text := newTranscribeTestServer(t, fakeHelper)
	info := recordFor(t, s, "ag", "medic3", 50) // 1 s

	if !s.queueTranscription(info) {
		t.Fatal("recording was not queued for transcription")
	}
	m := waitTranscript(t, text)
	want := "heard " + info.ID + " with Engine 41 Medic 3"
	if m["id"] != info.ID || m["text"] != want {
		t.Fatalf("console got %v, want text %q", m, want)
	}

	// The transcript is stored with the recording and searchable.
	day := time.UnixMilli(info.Start).Format(recDayFmt)
	list := s.recorder.Query(day, "ag", "", -1, "", 0)
	if len(list) != 1 || list[0].Text != want || !list[0].Transcribed {
		t.Fatalf("Query = %+v", list)
	}
	if got := s.recorder.Query(day, "ag", "", -1, "", 0, "medic 3"); len(got) != 1 {
		t.Fatalf("text search found %d, want 1", len(got))
	}
	if got := s.recorder.Query(day, "ag", "", -1, "", 0, "ladder"); len(got) != 0 {
		t.Fatalf("text search for a missing word found %d", len(got))
	}
}

func TestTranscriptionOnlyForAgenciesThatTurnedItOn(t *testing.T) {
	s, _ := newTranscribeTestServer(t, fakeHelper)
	if s.queueTranscription(recordFor(t, s, "other", "u", 50)) {
		t.Fatal("an agency without transcription was transcribed")
	}
	s.agencies["ag"].NoRecording = true
	if s.queueTranscription(recordFor(t, s, "ag", "u", 50)) {
		t.Fatal("transcribed although recording is off")
	}
	s.agencies["ag"].NoRecording = false
	if s.queueTranscription(recordFor(t, s, "ag", "u", 5)) { // 0.1 s key-up
		t.Fatal("a key-up was transcribed")
	}
	// Not installed: nothing is queued.
	os.Remove(s.transcriber.helperPath())
	if s.queueTranscription(recordFor(t, s, "ag", "u", 50)) {
		t.Fatal("queued without a helper")
	}
}

// A helper that dies is restarted for the next recording.
func TestTranscriptionHelperCrashRecovers(t *testing.T) {
	crashOnce := `#!/bin/sh
marker="$(dirname "$0")/crashed"
echo '{"ready":true}'
if [ ! -f "$marker" ]; then touch "$marker"; read -r line; exit 1; fi
while read -r line; do echo '{"ok":true,"text":"second try","audio_ms":1000}'; done
`
	s, text := newTranscribeTestServer(t, crashOnce)
	s.queueTranscription(recordFor(t, s, "ag", "a", 50))
	second := recordFor(t, s, "ag", "b", 50)
	s.queueTranscription(second)
	m := waitTranscript(t, text)
	if m["id"] != second.ID || m["text"] != "second try" {
		t.Fatalf("got %v", m)
	}
	if s.transcriber.failed.Load() != 1 {
		t.Fatalf("failed = %d, want 1", s.transcriber.failed.Load())
	}
}

func TestTranscriptionRoutesAreSuperAdminOnly(t *testing.T) {
	for _, route := range []string{"GET /api/transcription", "POST /api/transcription/install", "POST /api/transcription/remove"} {
		if _, listed := apiRolePolicy[route]; listed {
			t.Errorf("%s is open to more than super admins", route)
		}
	}
	if !strings.Contains(vadModel.URL, "huggingface.co/") {
		t.Error("VAD model URL changed")
	}
}

// TestTranscriptionInstallLive runs the real install: downloads the tiny
// model and the speech detector from Hugging Face, verifies them, and
// self-tests the helper given in MR_HELPER. Run on a Linux box with
// MR_LIVE_INSTALL=1 (needs internet).
func TestTranscriptionInstallLive(t *testing.T) {
	if os.Getenv("MR_LIVE_INSTALL") == "" {
		t.Skip("set MR_LIVE_INSTALL=1 and MR_HELPER=/path/to/mrtranscribe")
	}
	helper, err := os.ReadFile(os.Getenv("MR_HELPER"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, trHelperFile), helper, 0o755)
	s := &Server{agencies: map[string]*Agency{}, config: &Config{}}
	tr := NewTranscriber(s, dir)
	s.transcriber = tr
	tr.install = trInstall{InProgress: true}
	start := time.Now()
	tr.runInstall("tiny.en")
	st := tr.status()
	inst := st["install"].(trInstall)
	t.Logf("install: %+v in %v", inst, time.Since(start).Round(time.Second))
	if inst.Stage != "done" || st["installed"] != true || st["model"] != "tiny.en" {
		t.Fatalf("install did not finish: %+v", st)
	}
	// A second install of the same model re-uses the verified files.
	tr.install = trInstall{InProgress: true}
	start = time.Now()
	tr.runInstall("tiny.en")
	if d := time.Since(start); d > 30*time.Second || tr.status()["installed"] != true {
		t.Fatalf("re-install took %v", d)
	}
}
