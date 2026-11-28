// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 R3 Systems

package main

import (
	"bytes"
	"testing"
)

// Ad-hoc group audio delivered by membership: a member joined to the group
// channel gets the frame exactly as before. A member on another channel gets
// it untagged by default (adhoc_group_audio_tag off, the v4.9.1 behaviour),
// or, with adhoc_group_audio_tag on, channel-tagged (0x14) with the group's
// channel, exactly like a monitor copy (raw PCM has no tagged form and stays
// as it was).
func TestAdhocGroupAudioTaggedForMembersOffTheGroupChannel(t *testing.T) {
	const groupCh = 20
	for _, tagOn := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			wireType byte
		}{
			{"0x00 raw PCM", PacketTypeAudio},
			{"0x10 Opus", PacketTypeAudioOpus},
			{"0x13 RTP Opus", PacketTypeAudioRtp},
		} {
			name := tc.name + "/tag off"
			if tagOn {
				name = tc.name + "/tag on"
			}
			t.Run(name, func(t *testing.T) {
				s, sent := groupTestServer(t, "x")
				s.config.FloorControlAgencies = nil
				s.config.AdhocGroupAudioTag = tagOn
				s.ssrcToUser = map[uint32]string{}
				// A link on another channel puts "pa" within the group's reach
				// without making channel 20 a mutual-aid channel.
				s.mutualAidLinks = map[string]*MutualAidLink{"l1": {ID: "l1", Agencies: []string{"ag", "pa"}, Channel: 7, Active: true}}
				talker := addAgencyClient(s, sent, "ag", "talker", groupCh, 42000)
				addAgencyClient(s, sent, "ag", "onch", groupCh, 42001)     // member, on the group channel
				addAgencyClient(s, sent, "ag", "away", 3, 42002)           // member, switched away
				addAgencyClient(s, sent, "pa", "pon", groupCh, 42003)      // partner member, on it
				addAgencyClient(s, sent, "pa", "paway", 3, 42004)          // partner member, away
				addAgencyClient(s, sent, "ag", "bystander", 3, 42005)      // not a member
				pm := addAgencyClient(s, sent, "pa", "pmonaway", 4, 42006) // partner member, away, monitoring the group
				pm.MonitoredChannels = []int{groupCh}
				s.groupMgr.CreateAdhoc(channelKey("ag", groupCh), groupCh, "Team", GroupTypePreconfigured, "talker", 0)
				for _, m := range []string{"onch", "away", "pon", "paway", "pmonaway"} {
					s.groupMgr.addMember(channelKey("ag", groupCh), m)
				}

				pkt := inbound(tc.wireType, "talker", 1)
				want := tunedBytes(pkt, tc.wireType)
				s.relayAudioData(talker, pkt, want)

				for _, u := range []string{"ag/onch", "pa/pon"} {
					if got := onlyPacket(t, sent, u); !bytes.Equal(got, want) {
						t.Fatalf("%s (on the group channel) got % x, want % x", u, got, want)
					}
				}
				if len(sent["ag/bystander"]) != 0 || len(sent["ag/talker"]) != 0 {
					t.Fatal("audio went to a non-member or back to the talker")
				}
				for _, u := range []string{"ag/away", "pa/paway", "pa/pmonaway"} {
					got := onlyPacket(t, sent, u)
					if !tagOn || tc.wireType == PacketTypeAudio {
						if !bytes.Equal(got, want) {
							t.Fatalf("%s: untagged copy changed: % x", u, got)
						}
						continue
					}
					v := readTagged(t, got)
					if v.channel != groupCh || v.sender != "talker" || !bytes.Equal(v.audio, sealedPayload) {
						t.Fatalf("%s: tagged ch %d sender %q audio % x", u, v.channel, v.sender, v.audio)
					}
					if tc.wireType == PacketTypeAudioRtp && !bytes.Equal(got[5:], want[1:]) {
						t.Fatalf("%s: sender's RTP header not kept", u)
					}
				}
				// Every off-channel copy of one frame is the same frame (one
				// made-up RTP stream per sender, as for monitors).
				if a, b := onlyPacket(t, sent, "ag/away"), onlyPacket(t, sent, "pa/paway"); !bytes.Equal(a, b) {
					t.Fatal("off-channel members got different copies")
				}
			})
		}
	}
}
