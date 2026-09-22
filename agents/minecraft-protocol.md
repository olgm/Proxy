# Minecraft login/auth protocol: notes for a passthrough proxy

Observed behavior from `reference/ZBProxy` (`protocol/minecraft/{sniff,outbound}.go`, `common/mcprotocol/*`, `service/service.go`, `adapter/metadata.go`); the rest is the vanilla wire spec. **(unverified)** = inference, not confirmed from source.

## 1. State machine

| From | Trigger | To |
|---|---|---|
| (TCP open) | first byte | Handshaking |
| Handshaking | Handshake `0x00`, intent=1 | Status |
| Handshaking | Handshake `0x00`, intent=2 | Login |
| Handshaking | Handshake `0x00`, intent=3 (1.20.5+, "Transfer") | Login |
| Status | Status Request → Response → Ping → Pong | close |
| Login | (online mode) Encryption Req/Resp → session check → Set Compression → Login Success | Configuration (1.20.2+) or Play |
| Configuration | Finish Configuration + ack | Play |

Handshake is one packet, once, never repeated. Clients coalesce Handshake + next packet into one TCP write — frame by length, never by segment.

## 2. Packets a proxy must parse (all pre-encryption)

Handshaking, serverbound `0x00`:

| Field | Type | Notes |
|---|---|---|
| Protocol version | VarInt | client's version; snapshots = `0x40000000 \| n` |
| Server address | VarInt len + UTF-8 | ≤255 chars per spec; may carry NUL-delimited extras |
| Server port | Unsigned Short (BE) | |
| Intent | VarInt (always 1 byte in practice) | 1=Status, 2=Login, 3=Transfer |

ZBProxy caps this whole packet at 264 bytes and rejects protocol≤0, empty hostname, port==0, unknown intent.

Status: C→S `0x00` empty (wire bytes `01 00`); S→C `0x00` String(JSON); C→S `0x01` Long; S→C `0x01` Long (echo).

Login Start, serverbound `0x00` — layout varies by protocol version:

| Proto | Layout after String(16) name |
|---|---|
| <759 (pre-1.19) | nothing |
| 759–760 (1.19–1.19.2) | Bool hasSig {Long ts, VarInt+pubkey, VarInt+sig}, then Bool hasUUID {UUID} |
| 761–763 (1.19.3–1.20.1) | Bool hasUUID {UUID 16B} |
| ≥764 (1.20.2+) | UUID 16B, always present |

## 3. Framing / encoding

| Item | Rule |
|---|---|
| Frame | VarInt packet length, then that many bytes = VarInt packet ID + payload |
| VarInt | 7 bits/byte LE, MSB = continuation, max **5** bytes, signed int32 |
| Negative VarInt | always 5 bytes (`-1` = `FF FF FF FF 0F`) |
| String | VarInt **byte** length + UTF-8; spec limit is in *chars*, so byte cap = 3–4× the char cap |
| Numbers | big-endian, unlike VarInt |

Hazards:
- Reject VarInt at the 6th byte; reject negative/zero packet lengths; cap packet length (vanilla ceiling 2097151, handshake far smaller).
- A malicious 5-byte length can request ~2GB — always bound the read before allocating.
- **Legacy `0xFE` ping** (pre-1.7 server list ping): first byte `0xFE` parses as VarInt length 126 and desyncs the parser. Special-case first byte `0xFE` (and `0x02` = legacy handshake). Reply is a `0xFF` kick packet with a UTF-16BE string, or just drop the connection.
- Client name field: 16 chars max; a spec-legal string length of 16 is enforced by ZBProxy as 16 *bytes* — fine for ASCII names, would truncate exotic ones.
- Reads must be deadline-bounded (ZBProxy: 10s per handshake read) or a half-open client pins a goroutine.
- Buffered peek must hold the full handshake + Login Start for replay. ZBProxy caches 4096 B and cannot grow; 1.19 signature-bearing Login Start (~600 B) fits, but this is a hard ceiling.

## 4. Online-mode auth: who holds what

| # | Dir | Packet | Contents | Proxy visibility |
|---|---|---|---|---|
| 1 | C→S | Login Start `0x00` | name, UUID | **plaintext, readable/editable** |
| 2 | S→C | Encryption Request `0x01` | String(20) serverID (empty on modern), VarInt+RSA-1024 DER pubkey, VarInt+4B verify token, +Bool shouldAuthenticate (1.20.5+) | **plaintext, readable** |
| 3 | C→S | Encryption Response `0x01` | VarInt+RSA(sharedSecret 16B AES), VarInt+RSA(verifyToken) (1.19.x variant: Bool, else Long salt + signature) | **plaintext but opaque** — RSA-sealed to server's key |
| 4 | C→Mojang | HTTPS `sessionserver/session/minecraft/join` | accessToken, profile, serverHash = SHA1(serverID ‖ sharedSecret ‖ serverPubKey), twos-complement hex | **out of band, not on our TCP stream** |
| 5 | S→Mojang | HTTPS `hasJoined?username=&serverId=[&ip=]` | returns profile + skin properties | out of band |
| 6 | both | AES/CFB8 enabled, key = IV = the 16B shared secret | | — |
| 7 | S→C | Set Compression `0x03` | VarInt threshold | **encrypted, invisible** |
| 8 | S→C | Login Success `0x02` | UUID, name, properties (1.19+) | **encrypted, invisible** |
| 9 | C→S | Login Acknowledged `0x03` (1.20.2+) | → Configuration state | encrypted |

Custody: RSA private key = server only. Shared secret = client + server only (client generates it, seals it to the server pubkey). Mojang access token = client + Mojang only.

So a passthrough proxy can **never** decrypt post-step-3 traffic. MITM means substituting our own RSA keypair, which changes `serverPubKey` and thus the join hash — the real server's `hasJoined` then fails. MITM works only if the proxy holds a real Minecraft account and re-authenticates as that account, i.e. becomes the player. Not an option.

`hasJoined`'s optional `ip=` param (`prevent-proxy-connections`): if the server sends the IP it sees, Mojang requires it to match the IP the client used for `join`. A relay breaks that match. ZBProxy works against Hypixel, so Hypixel evidently omits it — but this one setting would kill every relay overnight against a backend that turns it on. **(unverified)**

## 5. Opacity cut point

- **C→S:** last plaintext byte = last byte of Encryption Response (`0x01`). Client encrypts starting with the very next byte it writes.
- **S→C:** last plaintext byte = last byte of Encryption Request (`0x01`). Server encrypts starting with the next byte.
- Practical design cut point is **earlier**: stop parsing at the end of the Handshake packet. Everything we need (version, hostname, port, intent) is there; the only later value of interest is the player name in Login Start, which is optional.
- ZBProxy's model: sniff handshake (+ Login Start for the name), rewrite the handshake, `writev` [rewritten handshake][cached original remainder] to upstream, then pure bidirectional byte relay (`splice(2)` on Linux) forever. Byte counts differ between the two sides of the rewrite, so zero-copy can only start after our own handshake write.

## 6. Compression

- Set Compression is **already encrypted**, so a passthrough proxy never sees the threshold and must not care.
- After it, framing becomes: VarInt total length, VarInt uncompressed length (0 = stored), zlib payload. Threshold typically 256.
- So there is no point at which a passthrough proxy can "resume" parsing — blocked by encryption *and* by an unknown threshold. Relay raw bytes; no packet-boundary buffering after the handshake.

## 7. Hostname handling

Some backends validate the handshake hostname (Hypixel among them, since ~2020). A raw
`proxy_pass`-style relay sends the proxy's own hostname and gets rejected there;
rewriting the handshake to the backend's own hostname (`rewrite_host`) is what
makes this class of proxy work against a backend that checks it.

| Suffix form | Meaning | Action |
|---|---|---|
| `host` | plain | rewrite host → the backend's hostname, port → the backend's port |
| `host\0FML\0` | Forge 1.7–1.12 | split at first NUL, rewrite host, re-append suffix |
| `host\0FML2\0` | Forge 1.13–1.16 | same |
| `host\0FML3\0` | Forge/NeoForge 1.20+ **(unverified exact string)** | same |
| `host\0ip\0uuid\0propsJSON` | BungeeCord legacy forwarding | **strip entirely** — never forward client-supplied identity fields upstream |
| `host.` | trailing FQDN root dot | strip before comparing |

- Split on the **first** NUL to get the comparable hostname; compare case-insensitively.
- ZBProxy keeps the post-NUL markup and re-appends it after rewriting, unless `IgnoreFMLSuffix` is set. Preserving it is right for modded clients; it is not needed against a vanilla backend.
- Rewriting the port as well as the host is what ZBProxy's shipped Hypixel preset does. Whether a given backend actually checks the port is **(unverified)** and backend-specific.
- SRV: ZBProxy resolves `_minecraft._tcp` for the upstream by default (`IgnoreSRVRedirect` disables). **We do not, and do not need to:** against Hypixel, `_minecraft._tcp.mc.hypixel.net` returns nothing (checked 2026-09-05), and the A record serves. A backend that does publish an SRV record would need this handled; check before assuming it doesn't.

## 8. Keepalive

| Item | Value |
|---|---|
| Initiator | server, in Play (and in Configuration on 1.20.2+) |
| Interval | ~15 s, random Long ID |
| Server-side kick | no matching response before the next one is due → "Timed out" (~15 s window) |
| Client-side kick | vanilla read-timeout ~30 s with no bytes at all → "Timed out" |
| ID mismatch | immediate kick |

Encrypted — the proxy cannot see, answer, or synthesize keepalives. Breaks it: stalling >15 s, buffering instead of forwarding, dropping/reordering bytes, closing one direction (half-close reads as reset), Nagle delay. Set `TCP_NODELAY`; TCP keepalive only stops NAT idle-eviction, it is not a substitute.

## 9. Protocol version

- Pass the client's version through **unmodified** into the rewritten handshake. Changing it desyncs the auth flow and the client.
- We must still branch on it to parse Login Start (see §2). Key thresholds: 759 (1.19), 761 (1.19.3), 764 (1.20.2).
- Snapshots report `0x40000000 | n`, which is past every threshold above, so a snapshot of anything before 1.20.2 branches onto the newest layout and reads a signature block where the UUID should be. We detect it by the body not ending where the branch expected, and fall back to the name. Hypixel does not accept snapshot clients, so this is robustness, not a live path.
- Mismatch in Login → server sends Disconnect (login) `0x00` with a JSON chat "Outdated client/server". In Status the client just shows the incompatible marker. Either way the proxy does nothing; it is already relaying.
- Common values: 47=1.8.9, 340=1.12.2, 754=1.16.5, 758=1.18.2, 763=1.20.1, 765=1.20.4, 767=1.21.

## 10. What a large backend does that a proxy has to survive

These are generic behaviors a large server may enforce; Hypixel is used below as a
concrete, checkable example of each.

| Behavior | Effect on us |
|---|---|
| Handshake hostname must match what the backend expects (Hypixel requires `mc.hypixel.net`, since ~2020) | mandatory rewrite when the backend checks it; this is the whole feature for such a backend |
| Online mode always on | encryption always happens; no plaintext Play stream, ever |
| Anti-VPN / datacenter-ASN IP blocking | **most likely practical failure mode** against a backend that does this — a cheap VPS IP gets kicked. Residential / clean-ASN egress matters more than any protocol detail |
| Per-IP connection throttle | all proxied players share one source IP; concurrent logins trip "connecting too fast" on a backend that throttles per IP. Vanilla default is 4000 ms/IP; a given backend's may be stricter **(unverified, backend-specific)** |
| Per-IP simultaneous connection cap | caps how many players one proxy IP can serve on a backend that enforces this **(unverified threshold, backend-specific)** |
| Status/ping flood limits | do not proxy MOTD pings 1:1. **Done:** the ingress synthesizes the response and the pong; nothing in the status path reaches the backend. The pong is held for the chain's own round trip so the number still means something — see README, "Server list". |
| "Failed to verify username" | session-check failure — see the `prevent-proxy-connections` risk in §4 |
| PROXY protocol | Hypixel does not accept it, and most public backends won't. ZBProxy's PROXY-protocol support is for *our own* chained hops only; never send it upstream |

## 11. Design implications

1. Parse exactly one packet (Handshake), optionally peek Login Start for the name, then relay bytes forever. Replay peeked bytes verbatim after the rewritten handshake in one vectored write.
2. Never inspect or reframe anything after that — you cannot, and trying adds latency that trips the 15 s keepalive.
3. Reject cheaply before touching upstream: bad VarInts, `0xFE`, oversized handshakes, per-IP rate limits.
4. Latency is the product: `TCP_NODELAY`, `splice(2)` where available, no per-packet processing.
5. Dominant reliability risk is not the protocol — it is the backend's IP reputation checks and throttling of our egress address. Large servers (Hypixel among them) rate-limit or blacklist datacenter addresses that probe them, and the egress address is the hardest thing in this system to replace — never send synthetic traffic at the backend from a node.
