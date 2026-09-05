# Changelog

## Unreleased

- `internal/tunnel`: an ECHO message that times the whole chain instead of one leg
  of it. A node passes it along while it has hops, and the node with none left —
  the exit — turns it around, so it measures as far as our own infrastructure goes
  and no further. It carries no stream and opens none, which is the point: the exit
  dials the backend when a stream opens, so measuring with real traffic would mean a
  connection to Hypixel per measurement. Only the entry originates one, on the link
  ping interval; the first reply for a nonce wins and `Node.ChainRTT` smooths it the
  way a leg smooths its own round trip.
- `internal/tunnel`: a stream counts what it has actually put on the legs — every
  copy a leg's `duplicate` asks for, and every re-send — so what a session cost can
  be reported beside what it carried. `Stream.Wire`.
- `proxyd`: a handshake naming a next state other than status, login or transfer is
  rejected instead of parsed. Encode re-emits whatever was read, so an unknown one
  was forwarded to the backend verbatim: eight bytes from anyone at all, turned into
  a malformed handshake arriving from our egress address.
- `proxyd`: a Login Start that does not end where its protocol version says it
  should now falls back to matching on the name, rather than trusting sixteen bytes
  read out of the middle of some other field as a UUID. Snapshot clients report
  `0x40000000|n`, which is past every version threshold, so a snapshot of anything
  before 1.20.2 was parsed on the newest layout and denied for a UUID it never sent.
- `proxyd`: a whitelist line may end in `# tag`, kept through renames, and the list
  can be added to and removed from in place, reloading first so an edit made on the
  node between two operations survives. The tag is how the Discord bot will know
  whose line is whose; a UUID already listed is refused with the tag it carries.- `internal/mojang`: ungated name and UUID lookups for the control link, which only
  a key holder can reach, answering "no such player" apart from "Mojang did not
  answer" so a member who mistyped a name is told so rather than told to retry.- `proxyd`: a control link, `config.control`, through which the whitelist is
  managed from outside the process. TCP, one request per connection, every frame
  sealed with the node's control key and bound to a challenge the node picked for
  that connection, so a recording replays into nothing; a frame that does not open
  gets no reply at all. Loopback may always connect, `allow_from` says who else
  may. `list`, `add` and `remove`; an add given only a name or only a uuid has the
  other half resolved against Mojang on the node. `proxyd ctl` is the client for an
  operator on the node, which is how proxyctl will drive it over ssh. proxyd stays
  the only writer of its file.- `proxyctl`: every entry with a whitelist gets a control link, on the port after
  the hops, with a key of its own minted into `tunnel-keys.json` as `ctl|<node>`.
  `proxyctl whitelist list | add | remove` drives every such entry over ssh
  through `proxyd ctl`: an add or remove goes to all of them, so a player is
  whitelisted on the chain rather than on one node, and a list shows the entries
  side by side with the lines that are not on all of them marked.- `proxybot`: the Discord bot. Runs on one node, reaches every whitelisted entry
  over its control link, and keeps them carrying one membership: an op goes to
  the primary first and stops if refused, then to the others, and every five
  minutes the others are brought level with the primary by uuid and tag. Guild
  slash commands, private replies: `/whitelist add`, `remove`, `list`, and for
  managers `purge`, adding on a member's behalf and seeing everyone's lines. What
  a role grants comes from the topology: an account cap, or manage. A member's
  lines carry their Discord id as the tag, which is the whole ownership model, and
  are dropped once they leave the server or hold no configured role any more,
  checked with one Get Guild Member call each per pass so no privileged intent is
  needed. Every change is logged, and posted to an audit channel when one is set.
  The bot holds no state of its own. It depends on `github.com/bwmarrin/discordgo`;
  proxyd's import graph stays stdlib.- `proxyctl`: a `discord` block in the topology deploys the bot. Its node may
  connect to every entry's control link, and `deploy` checks its way to each
  entry it does not live on and offers the firewall rule, as for a hop. The bot
  gets `/etc/proxyd/bot.json` with every entry's control address and key, its own
  unprivileged account and hardened unit, and the token from `DISCORD_BOT_TOKEN`
  in the operator's environment, carried inside the install script over ssh into
  a root-only `/etc/proxyd/bot.env` the unit reads: never on a command line, never
  through `/tmp`. A redeploy without the token keeps the file. `status` and
  `uninstall` cover the bot. `topology.example.json` carries a `discord` block
  with placeholder ids; delete it to run without the bot.- `proxyd`: config-driven relay node. Handshake rewrite (Forge markers preserved,
  BungeeCord identity stripped), source-IP allowlist, `splice(2)` relay with
  per-direction half-close, legacy `0xFE` ping rejected.- `proxyctl`: topology-driven deployer over ssh/scp. Derives every node config from
  one file, cross-compiles per node arch, installs a hardened systemd unit running as
  an unprivileged account.- `proxyctl deploy` verifies every link after installing, probes the target's
  firewall when one is blocked, and offers to open exactly that port. Never edits a
  firewall without being asked.- `proxyd`: optional UUID whitelist on the ingress. Reads Login Start — the last
  plaintext packet naming the player — and drops anyone not on an `ign:uuid` list,
  with a login Disconnect rather than a silent close. Checked before dialing, so a
  stranger never costs the chain a connection or reaches Hypixel from our egress IP.
  Matches on UUID where the client sends one, on IGN for clients before 1.19 that
  send none, and rewrites the stored IGN when a player renames. The list reloads on
  change, so edits need no restart. Status pings are not gated.- `proxyd`: the whitelist follows renames instead of trusting a recorded name
  forever. Names are released when a player renames and can be claimed by someone
  else, so a stale name would otherwise admit a stranger — worst on 1.8.9, where a
  name is all the client sends. A daily refresh re-reads every entry's current name
  from Mojang and writes back what changed, which is well inside the ~37 day
  (unverified) window before a released name can be re-registered; the last run is
  stamped beside the list so a restart loop cannot burst. On top of that, a login with
  no UUID whose name is not listed triggers one lookup of who owns that name now, and
  is admitted only if the answer is a listed UUID — letting a just-renamed player back
  in on an old client without letting a stranger in under their old name. Lookups are
  the only attacker-reachable outbound requests, so they are rate limited per name,
  per source IP (3/5min, escalating to 2/hr then 1/hr, easing back after an idle
  window) and overall (50/5min). These calls go to Mojang, never Hypixel.
- **The whitelist is not an authentication boundary.** Both fields are the client's
  unverified word: proxyd never terminates Minecraft's encryption, so it can never
  ask Mojang whether a UUID really belongs to that player. Anyone who knows a listed
  UUID — they are public, resolvable from any IGN — passes the gate, fails Mojang's
  check at Hypixel, and can repeat that at any rate they like. Failed session checks
  from one datacenter IP is the signature Hypixel bans egress addresses for, and the
  whitelist does nothing to slow it. **The control for that is a per-source-IP
  connection rate limit and concurrent cap on the ingress**, which is the only hop
  that sees a real client IP; not implemented.- `proxyd`: `routes[].transport` chooses how the hops after the entry talk. `tcp` is
  the original chain, unchanged byte for byte. `udp` replaces it with a tunnel that
  numbers the byte stream at the entry, forwards each datagram on arrival at every
  hop in between — no reordering, so a hole never stalls the hops behind it — and
  puts it back in order once, at the exit, on its way into one TCP connection to the
  backend. Players still arrive over TCP and the ingress is unchanged: handshake
  rewrite, whitelist, then the bytes go into the tunnel instead of a socket.- `proxyd`: loss on a UDP leg is repaired by the node before it, not the far end.
  A receiver that sees 100 and 102 asks its previous hop for 101 and asks again every
  `RTT + 4·mdev` measured on that leg by its own ping; a node asked for something it
  never held wants it too, so the request walks back one leg at a time. Gap detection
  cannot see past the last number that arrived, so a sender with nothing being
  acknowledged re-sends its highest chunk, which turns a lost tail into a hole that
  can be asked for. A cumulative "delivered through N" runs the other way, frees the
  retransmit buffer at every hop it passes, and is what closes the window on the
  player's socket when the exit cannot drain into Hypixel fast enough. A chunk that
  falls out of every buffer before it can be replaced ends the session rather than
  hanging it.- `proxyd`: `duplicate` sends every packet more than once, per leg. Every node keeps
  the first copy of each chunk that reaches it, drops the rest, and sends what it
  kept on with the count set for the leg after, so counts never multiply along the
  chain and a clean leg can carry one copy while a lossy one carries three. Default
  2 everywhere; `routes[].legs` sets one leg by name. Ordering is still restored
  once, at the exit: putting chunks back in order at a relay would stall the hops
  behind it on every hole, which is the head-of-line blocking the tunnel exists to
  avoid. Duplication buys back a lost packet without waiting for anyone to ask, and
  buys nothing against a leg that is dropping because it is full.- `proxyd`: a retransmission is marked as one on the wire, so a relay that already
  holds the chunk passes it on instead of dropping it as another copy. The
  originator's probe of its highest chunk depends on this: it is the only thing that
  can reveal a tail lost on the leg *after* a relay, and a relay that swallowed it
  would leave the exit's horizon short of the tail until the stream was given up.
  This replaced an earlier scheme that duplicated only where a chunk entered the
  tunnel and de-duplicated only at the exit; it was wrong for a chain, because the
  count had to be the worst leg's count on every leg, and a relay could do nothing
  for the leg after it.- `proxyd`: a lost tail is found on the leg that lost it. Gap detection cannot see
  past the last number that arrived, and Minecraft is bursty enough that the next
  number may be a whole tick away, so a sender that has been quiet for 10 ms with
  chunks unacknowledged tells the next hop its horizon; everything under it the
  receiver has not seen becomes an ordinary hole, asked for on that leg and repaired
  in one leg round trip. Repeated every leg RTO until acknowledged, so a lost advert
  costs an RTO rather than the tail. Every sender does this, relays included. The
  end-to-end probe stays as the backstop, and now fires only when the adverts are
  lost too. An end-of-burst flag would have been the wrong tool: it rides on the
  chunk that was lost.- `proxyd`: every timer in the tunnel is configurable from `routes[].tunnel`, per
  route and therefore on every node of it: the tick, the ping, the clamp on how
  often a hole is asked for again, how long a sender stays quiet before announcing
  its horizon, how often the exit reports what it read, and the clamp on the
  end-to-end probe. The defaults are unchanged and lean toward thrift; Minecraft
  traffic is light enough that all of them can be made far more aggressive without
  a leg noticing, and the README gives a setting to start from.- `proxyd`: `paths` races several ways to one exit. Every path starts at the entry
  and ends at the exit; the exit keeps the first copy of each number and drops the
  rest, so a session gets the better path per packet rather than on average and
  survives one path failing outright. Paths that share a leg share its packets, so
  the shape is a graph; `proxyctl` rejects two paths that disagree about how many
  copies a shared leg carries, and rejects a set whose edges loop.
- **A UDP hop cannot use `allow_from`.** Over TCP an address has to complete a
  handshake before it can be used as a source; over UDP anyone can write it on a
  datagram, which would make a relay an open reflector and let a stranger inject
  bytes into a live session. Every leg is therefore sealed with its own AES-256-GCM
  key and a replay window. `proxyctl deploy` mints them into `tunnel-keys.json`
  beside the topology — gitignored, mode 600 — and reuses them, so redeploying does
  not cut the chain.- `proxyd`: one log line when a tunnel link starts or stops answering, and one per
  link every 30 s with round-trip time, jitter, ping loss and retransmit counts. It
  is the only view from outside a node of whether a leg carries UDP at all — a
  filtered UDP port is indistinguishable from an open one until something replies —
  so `proxyctl deploy` verifies UDP legs by reading it rather than by connecting, and
  `proxyctl status` prints the latest line per link.- `tools/mcping`: status-ping client for checking a chain end to end. Outside the
  deploy path; build and copy it by hand.- `tools/tcpping`: TCP round-trip timing, with a `-listen` mode so any node can be a
  target. Measures what proxyd carries instead of what ICMP reports. Outside the
  deploy path.
- Measured a candidate second Tokyo node (ty-a `nrt`, `vc2-1c-1gb`): 20 ICMP echoes
  from each existing node, 0% loss, HK 44.6 ms avg and Chicago 136.1 ms avg. Node was
  provisioned and destroyed on 2026-09-01; no topology refers to it and its IP is back
  in ty-a's pool.
- Pinned the Tailscale underlay to IPv4 on the HK↔Tokyo link, which had negotiated an
  IPv6 path costing 2.5 ms avg and 8.3 ms mdev against 0.5 ms on IPv4. Tokyo↔Chicago
  needed nothing: Chicago has no public IPv6. With both legs on IPv4 the chain is
  167.5 ms p50 over Tailscale against 167.9 ms over public IPv4, so transport is now a
  wash and the choice is about dependencies, not latency.
- Verified HK → Tokyo → Chicago → Hypixel: status ping returns Hypixel's MOTD while
  the client claims an unrelated hostname, and all three hops appear in the socket
  table. Real client login through the chain confirmed working.
- Switched the live HK → Tokyo → Chicago chain to `transport: udp` with every packet
  duplicated, single path. Racing is implemented and tested but not deployed: with
  three nodes the only second path is HK → Chicago direct, which shares HK's uplink
  with the relayed path. `transport: tcp` is one edit and one deploy away.
- UDP is not penalised on either leg, which was the open question: HK→TY 43.8–44.1 ms
  against 44.00 over TCP, TY→CHI 122.0–122.5 against 123.93, and mdev 0.1–0.4 where
  TCP measured ~1.0. Ping loss 0.0% on both. Verified end to end by carrying 4 MiB
  through the deployed chain to a throwaway socket on the exit — never Hypixel —
  byte-exact in 2.10 s, which is the 1 MiB window over a 166 ms round trip rather
  than anything the path is doing. Numbers and the two ways to mis-measure this are
  in `agents/link-latency.md`.
