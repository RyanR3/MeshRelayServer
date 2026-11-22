# Channel key announcements, version 2

Phones share channel keys by announcing them, encrypted ("wrapped") under a
Key Group key. In version 1 only the wrapped key was protected: the channel
id, the key epoch and the group name travelled beside it in clear and could be
changed. A captured announcement could be replayed with another channel id or
a huge epoch, which froze or mis-keyed a channel, and a damaged one could lock
a channel out. Version 2 authenticates the whole announcement with the group
key and makes every client apply the same acceptance rules.

The server is not involved: it relays these packets unchanged.

All integers are big-endian. `||` is concatenation. Test vectors:
`testdata/keyannounce_v2_vectors.json`.

## 1. Keys and tags

    K_ann = HMAC-SHA256(key = groupKey, msg = ASCII "MeshRelay key announce v2")

    modeCode: 1 = local (the apps' "mesh", the ATAK plugin's "multicast")
              2 = "server"

    tag = HMAC-SHA256(K_ann,
            ASCII "MRKA2" || u8 modeCode ||
            u16 len(channelId) || channelId ||
            i32 epoch ||
            u16 len(groupId) || groupId ||
            i32 groupEpoch ||
            u16 len(wrappedKey) || wrappedKey)

`channelId` and `groupId` are the UTF-8 bytes sent in the packet. `wrappedKey`
is the version 1 blob, unchanged (`0x02 | IV(12) | AES-256-GCM ct+tag`).
`groupEpoch` is the Key Group's own epoch.

## 2. On the wire

Binary `0x0F` (server relay, channel multicast, Nearby): the version 1 packet,
followed by a 38-byte trailer. Version 1 parsers ignore bytes after
`wrappedKey`.

    0x0F | u16 chLen | channelId | i32 epoch | u16 gLen | groupId |
           u16 wLen | wrappedKey |
           0xA2 | u8 modeCode | i32 groupEpoch | tag[32]

JSON (multicast 239.255.0.100:5101): the version 1 object plus

    "v": 2, "m": modeCode, "ge": groupEpoch, "tag": base64(tag)

A sender always adds the trailer / fields. A packet whose trailer is present
but malformed (wrong marker, short) is treated as a failed version 2
announcement, not as version 1.

Bytes after `wrappedKey`: none = version 1. Otherwise the first byte must be
`0xA2` and at least 38 bytes must be present; anything else is malformed.
Bytes beyond the 38 are ignored (room for later additions) and are not
covered by the tag. In JSON, `"v"` >= 2 with `m`, `ge` and `tag` present is
version 2; `"v"` present without a usable `m`/`ge`/`tag` is malformed.

The `groupId` in the packet is covered by the tag but need not equal the
name under which the receiver stores the matching group; the key is filed
under the receiver's own group record. The retired-key check in section 4 is
applied before the same-key check, in table order.

## 3. Receiving

1. Parse. Reject `channelId` or `groupId` longer than 64 bytes or empty,
   `wrappedKey` outside 1..1024 bytes, `epoch` outside 0..1,000,000.
2. Version 2 (trailer present): for each held, unexpired group G, compute the
   tag with G's key. The first G whose tag matches (constant-time compare) is
   the announcing group. Then require `modeCode` = code of G's mode scope and
   `groupEpoch` = G's epoch; unwrap with G's key; the result must be 32 bytes.
   Mark G "signed only" (section 5). If no group matches, or any check fails:
   ignore the packet. Nothing is stored, no channel is locked out.
3. Apply the acceptance rules (section 4) for (mode of G, channelId).
4. Version 1 (no trailer): only groups that are not "signed only" are tried;
   the pre-version-2 rules apply (anti-rollback, +1000 epoch window). A
   version 1 announcement that fails never locks a channel out either.

## 4. Acceptance rules (version 2, authenticated)

Held = the channel's current key `Kh` at epoch `Eh` for that mode, if any.
Incoming = key `K` at epoch `E`.

| Case | Result |
|---|---|
| `K` is the current key of a different channel in the same mode | ignore |
| `K` is in this channel's retired-key list | ignore |
| no key held | store `K` at `E` |
| `K` = `Kh`, `E` > `Eh` | keep the key, raise the stored epoch to `E` |
| `K` = `Kh`, `E` <= `Eh` | nothing to do |
| `K` != `Kh`, `E` > `Eh` | replace: store `K` at `E`, retire `Kh` |
| `K` != `Kh`, `E` = `Eh` | two phones changed the key at once: keep whichever key has the lower SHA-256 (compared as unsigned bytes, first byte most significant). Replace only if SHA-256(`K`) < SHA-256(`Kh`) |
| `K` != `Kh`, `E` < `Eh` | ignore |

There is no "+1000" window for authenticated announcements. Storing a key
never lowers a stored epoch.

## 5. "Signed only" groups

A Key Group is marked "signed only" (stored with the group) when it is created
by a version 2 client, imported from a group QR that carries `"a": 2`, or when
a valid version 2 announcement for it is received. From then on version 1
announcements for that group are ignored. A version 2 client always puts
`"a": 2` in the group QR it shows.
`"a"` >= 2 counts. The mark is never cleared.

Consequence: a phone still on an old app that changes a channel key cannot
pass it to updated phones by announcement; it is shared by QR, or the phone is
updated.

## 6. Key join response (0x0C)

The response's `epoch` was not covered by its proof. A version 2 approver
appends

    0xA3 | tag2[32]
    tag2 = HMAC-SHA256(K_ann,
             ASCII "MRKJ2" || joinerEphPub[32] || approverEphPub[32] ||
             u16 len(channelId) || channelId || i32 epoch ||
             u16 len(groupId) || groupId)

after the version 1 fields. A joiner that finds a valid `tag2` uses the epoch.
Without it (old approver) the epoch in the packet is not trusted: the key is
stored at the epoch already held for that channel, or 0 when none is held; a
later authenticated announcement of the same key raises it (section 4).

Bytes after the version 1 fields: none = old approver. `0xA3` followed by at
least 32 bytes = `tag2` (later bytes ignored). A `tag2` that is malformed or
does not verify is treated as absent: the key itself is still authenticated
by the version 1 proof, only the epoch is not trusted. The joined key is
stored through the normal store (never lowering an epoch).

## 7. Not covered

Anyone who holds a group key can announce keys for that group. Telling
members apart needs per-device identity keys and a member list, which is a
separate piece of work.
