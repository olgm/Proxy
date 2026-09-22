# ZBProxy behavioural notes

Source: `reference/ZBProxy` (v3). Behaviour only, no code.

## Pipeline

`accept → IP ACL → [PROXY hdr] → sniff MC handshake → route rules → outbound → rewrite handshake → relay`.
Sniff reads into a rewindable 4 KiB cache (>4 KiB handshakes unsupported), then rewinds past the handshake: the
original handshake bytes are dropped, a freshly built one sent instead, Login Start onward relayed verbatim.

## Minecraft mechanisms

| Mechanism | Problem solved | Needed for a single fixed backend? |
|---|---|---|
| Handshake sniff (proto ver, host, port, intent) | Everything downstream keys off it | Yes |
| Handshake regeneration + hostname rewrite | Hypixel rejects wrong login address | Yes |
| Port rewrite | Backend vhost keyed on port | Yes (25565) |
| FML/Forge suffix preserve | Host field carries `\0FML\0…`; naive rewrite breaks Forge | Yes if Forge users |
| `IgnoreFMLSuffix` | Strip Forge markup when backend dislikes it | No |
| Status/MOTD synthesis | Answer server-list pings locally | **Required for us** — see operational-safety.md. Implemented; not optional and not configurable. |
| Status pass-through mode | Show backend's real MOTD | **Never.** Every listing refresh would be a status request at Hypixel from the egress. |
| Ping modes: echo / `0ms` / `disconnect` | Control displayed latency | We do neither: the pong is held for the chain's measured round trip, so the number is the trip to our last node. |
| Login Start parse (name, UUID) | ACLs, logging | Yes on a whitelisted ingress — it is both the gate and the only name a session log can carry. |
| Name + hostname allow/deny, kick packet | Private proxy, reject wrong vhost | No |
| Live online counter + max-player cap, kick packet | MOTD `online` field, capacity control | Counter yes, it is the MOTD's `players.online`. Cap and kick no. |
| SRV lookup `_minecraft._tcp` (+ `IgnoreSRVRedirect` opt-out) | Backend advertises SRV | **No — Hypixel has no SRV record** (checked 2026-09-05). Earlier note here was wrong. |
| Intent=Transfer (1.20.5+) | Parsed and matchable, **unimplemented** → reset | No |
| Router rules (hostname/name/IP/port/status/transfer/and/or/invert) | Multi-backend fan-out | No |
| Hot config reload (fsnotify) | Edit lists/MOTD without restart | No |
| SOCKS4/4a/5 chained dialer | Egress via another proxy | No |
| Plain TCP relay + TLS SNI sniff (stubbed) | Generic relay | No |
| Legacy pre-1.7 ping (0xFE) | **Not handled** — rejected as bad packet | No |

## Hostname / handshake rewriting

- Rewrites only the `server address` and `port` fields of the serverbound Handshake. Login and Status paths both.
- Two config paths: outbound `EnableHostnameRewrite` + `RewrittenHostname` (falls back to outbound target), or a
  router rule's Minecraft rewrite (hostname/port/intent). Rule wins.
- Forge: host field is `host\0FML markup`; code splits on `\0`, rewrites the host, re-appends markup.
- Why the server cares: Hypixel authenticates the login address. Dumb passthrough (nginx `proxy_pass`) forwards
  whatever the client typed — your proxy's address — and Hypixel refuses. Old fix was a client `hosts` edit.
- Everything after Login Start is encrypted end-to-end: this is the only mutable point in the stream.

## MOTD / status ping

- **No caching.** Mode chosen by whether favicon+description are both empty:
  - Empty → pass-through: dial backend, send rewritten handshake + regenerated Status Request in one write, relay.
    One backend round-trip per server-list refresh.
  - Non-empty → synthesize in-process, backend never touched.
- Why synthesize: kill the per-refresh round-trip (clients ping constantly), survive backend downtime, brand the
  listing, hide the backend, report your own player count.
- Synthesized fields: version name, `version.protocol` **echoed from the client's handshake** (else the client
  renders a red "incompatible" badge), max, online, sample, description, favicon.
- Favicon is a literal base64 data URI; sentinel `{DEFAULT_MOTD}` expands to a built-in PNG. Description
  placeholders `{INFO} {NAME} {HOST} {PORT}` expand once at init, not per request.
- Online count: config value, or `-1` = live atomic count of currently-relayed logins. Sample (hover text):
  object form = explicit UUID→name; array form = bare names with generated signature-carrying UUIDs. Cosmetic.
- Ping/pong: default echoes the client's timestamp (honest RTT); `0ms` returns a zeroed timestamp (fake instant
  ping); `disconnect` sends nothing and drops.

## Name / UUID

- Name read from Login Start, capped at 16 chars. No charset/regex validation.
- UUID parsing is protocol-version-branched (≥764 raw, ≥761 optional flag, ≥759 behind a signature blob, older
  absent). Metadata only. **No UUID rewriting anywhere** — impossible, online-mode auth is client↔real server.
- Name and hostname allow/deny lists, optional case-folding, hot-reloadable, shared named-list registry.
  Rejection sends a chat-component Disconnect packet, not an RST.

## PROXY protocol

| Direction | Support | Notes |
|---|---|---|
| Inbound (read) | v1 + v2, auto-detected by signature | Overwrites source addr → feeds logs and IP ACL |
| Outbound (write) | v1 or v2, per-outbound | Sends real client IP to backend |

Only source address is consumed; destination and v2 TLVs parsed-and-discarded; v2 UNIX family unsupported.
**Hypixel does not consume PROXY protocol** — relevant only for self-hosted backends (Velocity/BungeeCord,
HAProxy, nginx, Spectrum).

## Connection lifecycle

- 10 s read deadline on every handshake / PROXY-header read step; cleared before relay.
- Relay phase: **no deadlines, no idle timeout.** TCP keepalive is the only liveness check, off unless configured.
  **No dial timeout** beyond OS default and process-lifetime context.
- **No half-close.** Two copy goroutines; first EOF either direction hard-closes both. A client that shuts down
  its write side gets disconnected. Fine for Minecraft, wrong for generic TCP.
- Give-up: bad handshake, deadline, ACL reject, dial failure, either-side EOF/error.
- `SO_LINGER 0` (immediate RST, no TIME_WAIT) for ACL rejects and REJECT/RESET pseudo-outbounds; `SO_LINGER 10`
  after a kick packet so the message flushes. Per-connection random coloured ID on every log line.
- Bug spotted: the plain (non-MC) route logs a dial failure but does not return, then relays a nil conn.

## Buffering and relay

- Relay buffer 16 KiB from a power-of-two `sync.Pool` allocator (64 B–64 KiB). Sniff cache 4 KiB, page-sized,
  rewindable, released once drained.
- Linux/Android TCP→TCP relay delegates to `io.Copy`, which the Go runtime turns into `splice(2)`: zero-copy,
  no userspace buffer, less CPU and latency. Elsewhere, a plain read/write loop.
- Handshake + cached Login Start go out as one vectored (`writev`) call; status response likewise. One syscall,
  one segment, no extra RTT-visible packet. Length prefixes go into reserved 5-byte front headroom, no copy.
- **`TCP_NODELAY` never set explicitly** — Go enables it by default. A reimplementation in another language must
  set it manually or Nagle adds ~40 ms to small packets. Single highest-leverage latency setting.
- Optional socket opts: SO_MARK, bind-to-device, TCP congestion algorithm, TCP Fast Open, MPTCP.

## Limits and access control

| Control | Present |
|---|---|
| IP allow/deny (exact string match, list-based) | Yes, at accept time |
| CIDR matching | Only as a router rule, not the fast-path ACL |
| Player-name / hostname allow-deny | Yes |
| Max concurrent players (per outbound) | Yes |
| Per-IP conn cap, conns/sec, bandwidth limit | **No** |
| Handshake flood protection | Only the 10 s deadline + 264-byte packet cap |

## Outbound dialing / DNS

- Source selection: optional `SendThrough` local IP, bind-to-interface, fwmark.
- SRV first unless disabled: `_minecraft._tcp.<host>`, tries results in order, falls back to plain host:port.
  No priority/weight handling, no caching.
- DNS: Go's default resolver on every dial. **No caching, no custom resolver, no pre-resolution.** A cold lookup
  on the login path adds directly to join latency — worth caching in our build.
- Happy eyeballs: Go dialer defaults only (dual-stack, ~300 ms fallback). No pooling or backend pre-warming.

## Hypixel-specific

- The reason the project exists: hostname rewrite to `mc.hypixel.net`, defeating the login-address check.
- Shipped default config is Hypixel: listen 25565, sniff minecraft, rewrite host/port, outbound
  `mc.hypixel.net:25565`, synthesized MOTD, default outbound RESET. SRV is on by default in ZBProxy, but Hypixel
  publishes no SRV record, so it resolves nothing and falls back to the A record.
- Nothing else is Hypixel-aware — no Hypixel API, no inspection past Login Start (encrypted).

## Minimum set for a Hypixel-only accelerator

1. TCP accept loop, one task per connection.
2. Parse Handshake: VarInt length, packet id 0, protocol version, host, port, intent. Reject malformed, cap
   packet size, 10 s read deadline.
3. Rewrite host → `mc.hypixel.net`, port → 25565, preserving any `\0` Forge markup.
4. Re-emit handshake, then forward buffered Login Start and everything after byte-for-byte.
5. Status intent: synthesize a response echoing the client's protocol version, and answer the ping/pong. Passing
   through is not an option for us — it aims the whole internet's server-list refreshes at Hypixel via our egress.
6. Bidirectional relay: `splice` on Linux, else a pooled ~16 KiB buffer. `TCP_NODELAY` on both sockets.
   Close both directions on first EOF/error.
7. Resolve the backend by A record. There is no SRV record to prefer.
8. Dial timeout, and per-connection IDs in logs.

## Deferred

PROXY protocol either direction · name/hostname ACLs and kick messages · max-player cap and kicking · MOTD
placeholder templating · router rule engine and multiple outbounds · hot config reload of the MOTD · SOCKS
chaining · TLS SNI sniffing · generic non-MC relay · MPTCP, TFO, congestion control, fwmark, bind-to-device ·
SRV (Hypixel has no record) · rate limiting.

Done since: status/MOTD synthesis with favicon and sample, live online count, legacy `0xFE` rejection, Transfer
intent passed through, IP ACLs with CIDR, per-session logging.
