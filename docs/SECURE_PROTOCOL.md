# MeshRelay secure protocol, version 1 (MRS1)

Client-to-server protocol on UDP 5000 that replaces the challenge-response
sign-in (`0x0D`/`0x0E`/`0x08`/`0x09`) and the plaintext packets after it.
The server speaks both until an agency requires this one.

Reference implementation: `secure.go` (server) and the `secPhone` type in
`secure_test.go` (client). Check a new client against
`testdata/mrs1_vectors.json`.

## Primitives

| Use | Primitive |
|---|---|
| Key agreement | X25519 |
| Key derivation | HKDF-SHA256 (`Extract` salt = ASCII `MRS1`) |
| Hash / MAC | SHA-256 / HMAC-SHA256 |
| Packet encryption | AES-256-GCM, 12-byte nonce, 16-byte tag |
| Password stretching | PBKDF2-HMAC-SHA256, 100000 iterations, 32 bytes |

All integers are big-endian. `str` is a 2-byte length followed by UTF-8 bytes.
`|` is concatenation. Labels are ASCII with no terminator.

## Server identity

The server has a permanent X25519 key pair; `S` is its 32-byte public key.
A client pins `S`:

- from the enrollment QR (`"spk"`: 64 hex characters), or
- on its first successful sign-in to that server (after step 4 verifies).

A client that has a pinned `S` for a server MUST abort when the hello reply
carries a different `S`, and MUST NOT fall back to the old protocol for that
server.

## Credential and verifier

`P` = lowercase hex SHA-256 of the credential string (password, device key or
QR token), as 64 ASCII bytes.

```
salt           = SHA-256("MRS1-salt" | S | username)
SaltedPassword = PBKDF2(P, salt, 100000, 32)
ClientKey      = HMAC(SaltedPassword, "Client Key")
StoredKey      = SHA-256(ClientKey)
ServerKey      = HMAC(SaltedPassword, "Server Key")
```

`username` is the login account name exactly as typed. A client may cache
`ClientKey`, `StoredKey` and `ServerKey` for a (server, username, credential).

## Handshake

```
1. client -> server  0x40 | 0x01 | eC[32] | cNonce[16] | [cookie[16]]
2. server -> client  0x41 | 0x01 | slot[1] | eS[32] | S[32] | sNonce[16] | tag[32]
3. client -> server  0x42 | 0x01 | slot[1] | eC[32] | cNonce[16] | nonce[12] | ct
4. server -> client  0x43 | 0x01 | nonce[12] | ct
```

`eC`, `eS` are fresh X25519 public keys. `cNonce` is 16 random bytes.
The server may answer step 1 with `0x45 | 0x01 | cookie[16]` when it is
busy; the client repeats step 1 with the cookie appended.

After step 2 both sides compute:

```
th1   = SHA-256("MRS1-th1" | eC | cNonce | eS | S | sNonce)
prk   = HKDF-Extract(salt = "MRS1", ikm = X25519(e, e) | X25519(eC, S))
kC2S  = HKDF-Expand(prk, "MRS1 hs c2s" | th1, 32)
kS2C  = HKDF-Expand(prk, "MRS1 hs s2c" | th1, 32)
kTag  = HKDF-Expand(prk, "MRS1 hs tag" | th1, 32)
```

The client MUST check `tag == HMAC(kTag, th1)` (the server holds the private
key of `S`) and that `S` is the pinned key, before sending step 3.

Step 3 ciphertext: AES-256-GCM(kC2S, random `nonce`, aad = `th1`) over

```
str username | str agency | flags[1] | proof[32] | str client_name
authMessage = SHA-256("MRS1-auth" | th1 | username)
proof       = ClientKey XOR HMAC(StoredKey, authMessage)
```

`slot` is echoed from step 2. `flags` is reserved (0).

```
th3 = SHA-256("MRS1-th3" | th1 | nonce(step 3) | SHA-256(ct(step 3)))
```

Step 4 ciphertext: AES-256-GCM(kS2C, random `nonce`, aad = `th3`) over

```
status[1] | session[4] | serverSig[32] | str message
```

`status` 1 = signed in; anything else = refused (`message` says why,
`session` and `serverSig` are zero). On success the client MUST check
`serverSig == HMAC(ServerKey, authMessage)`; only then does it pin `S` on a
first contact. `message` is `Authenticated[|domain=<d>][|devkey=<64 hex>]`.
`devkey` is a new device key after a QR sign-in: store it in place of the
token; it is the credential from then on.

The server keeps no state before step 3. If step 4 is lost, the client
repeats step 3 unchanged and gets the same step 4 again. A hello is good
for 10 to 20 seconds.

## Session

```
kDataC2S = HKDF-Expand(prk, "MRS1 data c2s" | th3, 32)
kDataS2C = HKDF-Expand(prk, "MRS1 data s2c" | th3, 32)
```

Every packet in both directions:

```
0x44 | session[4] | counter[4] | AES-256-GCM(key, nonce, aad, inner)
nonce = 8 zero bytes | counter[4]
aad   = the 9 header bytes
inner = an ordinary MeshRelay packet (type byte first), unchanged
```

Each direction has its own key and its own counter, starting at 1 and
increasing by 1 per packet. The receiver accepts a counter once, within a
window of 64 below the highest seen.

Rules:

- After step 4 the client sends and accepts ONLY `0x44` packets for that
  server (and handshake packets when it signs in again). Plaintext packets
  from the server's address are forged: drop them.
- The server finds the session by `session`, not by address. A valid packet
  from a new address moves the session there; no new sign-in is needed.
- JOIN, heartbeats, audio and everything else are sent as `inner`.
- A sign-out from the server is the usual `0x09 0x02 <message>`, inside the
  session.

## Falling back

A client that has never pinned a key for a server sends step 1 and, with no
`0x41` or `0x45` after two tries of about 1.5 s, uses the old protocol
(the server predates MRS1).
