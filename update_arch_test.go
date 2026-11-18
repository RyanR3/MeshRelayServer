// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"runtime"
	"testing"
)

// The GUI updater used to always download "ptt-server-linux" (amd64), which on
// an aarch64 Raspberry Pi installed a binary that could not execute — and the
// checksum fallback verified it happily, so the failure surfaced only as a
// dead service after restart.
func TestArchBinaryAssetMatchesThisMachine(t *testing.T) {
	got := archBinaryAsset()
	want := "ptt-server-linux"
	if runtime.GOARCH == "arm64" {
		want = "ptt-server-linux-arm64"
	}
	if got != want {
		t.Fatalf("archBinaryAsset() = %q on GOARCH=%s, want %q", got, runtime.GOARCH, want)
	}
}

// Both release asset names must be reachable, so a release that ships only one
// of them is still resolvable rather than silently yielding an empty name.
func TestArchBinaryAssetIsAKnownReleaseAsset(t *testing.T) {
	known := map[string]bool{
		"ptt-server-linux":       true,
		"ptt-server-linux-arm64": true,
	}
	if !known[archBinaryAsset()] {
		t.Fatalf("archBinaryAsset() = %q, which is not a published release asset", archBinaryAsset())
	}
}
