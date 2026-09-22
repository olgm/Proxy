# agents

Notes for agents working on this repo.

- [operational-safety.md](operational-safety.md) — **read first.** What must never be
  sent at the backend, and the hard rules that keep the egress address clean.
- [minecraft-protocol.md](minecraft-protocol.md) — Minecraft login/auth wire protocol;
  what a passthrough proxy can and cannot touch, and where the stream goes opaque.
- [zbproxy-behavior.md](zbproxy-behavior.md) — what ZBProxy implements and why;
  minimum set for a single fixed-backend accelerator vs deferred.
- [link-latency.md](link-latency.md) — measured per-leg latency, and the node-level
  firewall rule that pins the Tailscale underlay to IPv4.
- [udp-tunnel.md](udp-tunnel.md) — the UDP transport: what is on the wire, who holds
  what state, and the invariants that make de-duplication and repair correct.
- [control-plane.md](control-plane.md) — the operator CLI and the Discord bot: where
  each runs, the keyed control link into every entry, the role model, and how one
  bot keeps several entries' whitelists level.
- [probe.md](probe.md) — the measurement service: why it is outside proxyd, what the
  routes decide is measured, and how loss is split by direction.
- [trial.md](trial.md) — the bake-off harness: why a candidate leg cannot be probed
  by probed, why every hop echoes, and what no clock here can measure. Temporary.

## Shape

| path | role |
| --- | --- |
| `internal/mc` | handshake, Login Start and the status exchange: parse / rewrite / encode. The only packets we parse. Also the MOTD document and its render. |
| `internal/whitelist` | `ign:uuid` list: match, hot reload, daily name refresh, miss-path lookup. |
| `internal/mojang` | profile API client and the rate limiters guarding it. |
| `internal/tunnel` | UDP transport: framing, per-leg AEAD, NACK repair, duplication, racing, ordering at the exit. |
| `internal/sealed` | the sealed TCP exchange both links put on the wire: a challenge, then one AEAD frame each way. Was control's; shared the moment probed needed the same thing. |
| `internal/control` | the control link: sealed request/reply over TCP, `list`/`add`/`remove` against a whitelist. Server in proxyd, client in proxyctl and the bot. |
| `internal/botcfg` | shape of `/etc/proxyd/bot.json`: written by proxyctl, read by the bot. |
| `internal/probe` | the measurement service: probe classes per leg and per chain, duplication, per-direction loss, windowed percentiles, the JSONL dataset, and the feed that posts a closed window. |
| `internal/jsonl` | the rotating record file and its tail read: probed's dataset and proxyd's session log. |
| `internal/window` | windowed aggregation and the percentiles over it. Was probed's stats.go; shared the moment a second service needed numbers comparable to probed's. |
| `internal/webhook` | one Discord webhook client: content limit, 429s, mention suppression, one image attachment, delete, and a queue that coalesces and drops rather than blocking. stdlib only. |
| `internal/version` | the one version every binary here reports, and the commit `go build` stamps beside it. |
| `internal/proxy` | listeners, allowlist, whitelist gate, relay over TCP or tunnel, the control server. |
| `cmd/proxyd` | node runtime. Same binary on every node. `proxyd ctl` is the local control client. |
| `cmd/proxyctl` | deployer, and `whitelist` verbs over ssh. Operator machine only, never installed on a node. |
| `cmd/proxybot` | the Discord bot. One node; reaches every whitelisted entry over its control link. `card.go` draws the status PNG; it is the only thing here that links `x/image`. |
| `cmd/probed` | the measurement runtime. Optional, its own unit and user, on every node a production path touches. |
| `internal/trial` | the bake-off mesh: a flood over candidate legs, every copy kept with the route it took, per-leg round trips echoed at each hop. Temporary. |
| `cmd/triald` | the bake-off runtime. Beside probed, never instead of it. Temporary. |
| `tools/mcping` | status-ping client. Deliberately outside the deploy path. |
| `tools/tcpping` | TCP round-trip timing. Node-to-node only. |

## Rules

- No code copied from `reference/ZBProxy`. Protocol understanding only.
- Roles are emergent: a listener with a `minecraft` block is an ingress. Do not add a
  role field. A route may name its entry as its own exit, which is one node and no
  leg — that is how the egress is also an ingress, not a special case in proxyd.
- A status ping is answered on the ingress and never forwarded. It is the one packet
  a stranger can make us send at the backend without an account, and every client's
  multiplayer screen sends it on a loop. Never add a pass-through mode; there is no
  config for one on purpose. The chain is timed with the tunnel's own ECHO instead,
  which stops at the exit.
- Nothing in the status exchange carries a latency: the client times the pong. The
  only way to report a number is to answer that late. Do not look for a field.
- The whitelist gates on what the client claims in Login Start. That claim is
  unverifiable here and always will be — see minecraft-protocol.md §4. It is a door
  lock, not authentication, and it is not a rate limit. Do not describe it as either.
- Only an ingress can hold a whitelist; a relay never sees a Login Start. Only the
  ingress sees a real client IP, so per-source limits have to live there too.
- `allow_from` is a TCP-only control. Over UDP a source address is forgeable, so a
  hop authenticates every datagram with its per-leg key instead. Never add a UDP path
  that trusts an address, and never let a UDP listener act on an unopened datagram.
- Duplication is per leg, set at both ends of it. Every node drops what arrives to
  one copy of each chunk, then sends on with the next leg's own count, so counts
  never compound. A re-send is flagged on the wire and is not a copy: a relay must
  pass one on even when it holds the chunk, or a tail lost on the leg after it can
  never be found. Ordering is restored only at the exit; do not add it to a relay.
- Chunk numbers are the only identity in the tunnel. Duplicates carry the same number
  on purpose — that is what makes a copy distinguishable from a gap — so nothing may
  renumber a chunk as it passes through.
- The protocol version in a handshake is the client's word like everything else. A
  Login Start that does not end where that version says it should was parsed on the
  wrong layout: drop the UUID and match the name. Never trust bytes read by a branch
  the packet disagreed with.
- The UUID column is the identity; the name column is a cache of what that UUID is
  called today. Anything that trusts a stored name without the refresh behind it
  re-opens the released-name hole. Never add a code path that matches a name the
  refresh does not keep current.
- Mojang is not the backend. Profile lookups are fine from a node and are not covered
  by the probe rule; nothing in this repo may talk to the backend from a node.
- `proxyd` stays dependency-free — stdlib only, which is why the tunnel uses AES-GCM
  and not something from `x/crypto`. The module is not: `cmd/proxybot` depends on
  discordgo. Check with `go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}'
  ./cmd/proxyd ./cmd/probed`. SSH credentials never land on a node. What does: the per-leg tunnel
  keys and the node's own control key in `/etc/proxyd/config.json`, 0640 root:proxyd;
  on the bot's node also every entry's control key in `bot.json`, 0640 root:proxybot,
  and the token in `bot.env`, 0600 root:root, read by systemd.
- The control link is TCP, so it may trust an address as well as its key; the
  address rule above is about UDP. Loopback always may connect. proxyd stays the only
  writer of its whitelist file: everything else goes through the link.
- A line's tag is the whole ownership model. The bot identifies a member's lines by
  `discord:<id>` and nothing else; never infer ownership from a name.
- Every whitelisted entry carries the same membership. The primary is the tie-break
  and the others are reconciled to it by uuid and tag, never by name. Do not add a
  second source of truth beside the whitelist files: the bot keeps no state on
  purpose.
- Nodes carry `proxyd`, and the bot's node also `proxybot`. Neither may contact
  the backend; see operational-safety.md. A node on a probed topology also carries
  `probed`, which reaches its own peers and, when a probe feed is configured, one
  Discord webhook.
- What `probed` measures follows from the routes and is not configurable: every
  production leg at one copy and at its real count, and every route longer than one
  leg end to end the same way. A pair no route puts traffic between is never
  probed. Do not add a knob that lets the measurement drift off the production
  path — a probe on a path players do not use answers a question nobody asked.
- `probed` is a separate binary, unit, user and set of keys, and proxyd does not
  know it exists. Keep it that way: it must be stoppable without touching a live
  session, and holding its keys must not be a way into one.
- A leg no route uses is measured by `triald`, never by `probed`. The rule above
  that probed may not drift off the production path is the reason triald exists,
  not something triald gets around: it is a separate binary, unit, user, port and
  set of keys, its legs are written by hand because there is nothing in the routes
  to derive them from, and it is deleted when its question is answered. It carries
  node ids on the wire, which nothing else here does — the route is the
  measurement — and they are data only: the key still decides which leg a datagram
  belongs to. See trial.md.
- The status card is always the last message in its channel, and that is a
  lifecycle rather than a setting: Discord cannot move a message, so a quiet tick
  edits the card in place and a transition deletes it, posts the line and posts a
  new card below. It is the only message anything here deletes. A resolved
  incident's two lines are edited down to `> -# …` so that an alert that is over
  stops looking like one that is not. See control-plane.md.
- A feed is off unless its environment variable is set, and a feed may never cost
  the thing it reports on. proxyd posts a session off the relay path and probed
  posts a window off the flush; both queues drop rather than block, post early when
  they are filling, and report what they dropped even with nothing else to send.
  The client IP goes in the journal, in the session log and in `/watch`, never in a
  channel. `internal/webhook` is stdlib because two of its three callers are the
  dependency-free node binaries.
- A session's whole account of itself is written by the goroutine relaying it, in
  its tail: record, journal line, feed line. So proxyd must never be killed on top
  of a relay — it handles SIGTERM, ends each session and waits up to five seconds
  for them to be written down. Everything about shutdown ordering in `node.close`
  exists for that; the tunnel and the log come down after the sessions, not before.
- A session is keyed by uuid everywhere it is read back, and a client before 1.19
  sends none. The whitelist matched the login to an identity in order to allow it,
  so the login takes that identity onward. A record without one belongs to nobody.
- An account holds one session: a login ends whatever that identity already has
  open, new connection wins. Two at once is a retry over a dead attempt, and it
  made the feed and the roster both count one player twice.
- `chain` on a session is billed traffic across every node it crossed, not one
  node's spend: a leg is billed at both of its ends, the chain's two ends once
  each, so a direct exit costs exactly twice payload and that is the floor. Bytes
  only — no ratio is rendered anywhere, deliberately. The entry measures its
  own leg and is told by proxyctl (`chain_legs`) how many lie beyond it, because
  `hops` names the next node and nothing past it.
- `agents/` is tracked. Gitignored: `reference/`, `private/`, `CLAUDE.md`,
  `topology.json`, `tunnel-keys.json`.
- Docs here are one-line notes. Be complete in coverage, ruthless in word count.
- Nothing in a deploy script may assume GNU coreutils. Not every distro ships GNU
  coreutils (uutils is one example); uutils' `install` cannot overwrite an existing
  file from `/dev/stdin` and its errors name neither the file nor the operand. Prefer
  the form that works on a fresh node and an already-deployed one alike — remove,
  then create — and read a bare `install: No such file or directory` as uutils
  rather than as a missing upload.
- One version for the whole repo, in `internal/version`, and every binary reports it.
  Do not add a per-binary version: proxyd, proxybot, probed and triald share
  `internal/tunnel`, `internal/control`, `internal/probe` and `internal/window`, so
  four numbers cut from one commit could only ever agree. Bump `V` and tag the
  commit. The revision printed beside it is stamped by `go build` and is the half
  that says what actually shipped — `go run` does not stamp one, so a `go run`
  invocation prints the bare number and cannot be compared against a node.

## Verify

```sh
go test ./...                      # in-process 3-hop chains, TCP and UDP, with loss
go run ./cmd/proxyctl config       # expand topology, change nothing
go run ./cmd/proxyctl deploy       # installs, then verifies every link
```

Do not run `mcping` against the backend. Against the chain entry it is now fine: the
ingress answers a status ping itself and nothing reaches the backend. See
[operational-safety.md](operational-safety.md).
