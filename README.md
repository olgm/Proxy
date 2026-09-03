# proxy

Minecraft TCP accelerator. Chains relay nodes between player and server so a lost
packet is replaced by the node before it rather than by the far end.

Legs run over plain TCP or over a UDP tunnel, per route. The tunnel can send every
packet more than once, and down more than one path at a time.

Targets Hypixel. Deployed today as HK ingress → Tokyo relay → Chicago egress, over
UDP with every packet doubled.

## Design

One binary (`proxyd`) on every node. A node has no role field: it is a list of
listeners, each with one next leg. A listener with a `minecraft` block is an ingress;
one that dials the target is an exit; anything else passes bytes along. A second
ingress is one more listener, not a code change.

The ingress parses exactly one packet — the handshake — rewrites the address to what
Hypixel expects, then relays raw bytes: `splice(2)` on a TCP route, numbered chunks
on a UDP one. It cannot do more: the client encrypts from Encryption Response
onward.

## Deploy

Needs Go and ssh locally; root or passwordless sudo on each node.

```sh
cp topology.example.json topology.json     # edit
cp whitelist.example.txt whitelist.txt     # edit, or drop `whitelist` from the route
go run ./cmd/proxyctl config               # preview, changes nothing
go run ./cmd/proxyctl deploy
go run ./cmd/proxyctl status
```

Commands: `config`, `deploy`, `status`, `uninstall`.

## topology.json

| field | meaning |
| --- | --- |
| `user` | unprivileged account proxyd runs as. Never root. |
| `base_port` | first auto-allocated hop port |
| `nodes.<n>.ssh` | ssh target or `~/.ssh/config` alias (control plane) |
| `nodes.<n>.addr` | address the previous hop dials (data plane) |
| `nodes.<n>.bind_addr` | optional; bind listeners to one local address only |
| `routes[].entry` | node clients connect to |
| `routes[].port` | public port on the entry node |
| `routes[].via` | ordered relay chain after the entry, ending at the exit |
| `routes[].transport` | `tcp` (default) or `udp` — see below |
| `routes[].duplicate` | UDP only; copies of every packet on every leg. Default 2 |
| `routes[].legs` | UDP only; `{from, to, duplicate}` per leg that should differ |
| `routes[].exit` | node every path converges on. Implied by the end of `via` |
| `routes[].paths` | UDP only; race several ways to the exit instead of one `via` |
| `routes[].tunnel` | UDP only; sizes, deadlines and every timer — see below |
| `routes[].target` | final `addr`, plus `rewrite_host` / `rewrite_port` |
| `routes[].whitelist` | optional; local `ign:uuid` file seeded onto the entry node |

Hop ports are allocated automatically. `config` prints the map.

## Transport

`transport` picks how the hops after the entry talk to each other. Players always
arrive over TCP either way, and the ingress is identical in both: it parses the
handshake, checks the whitelist, rewrites the address. Only the legs change.

| | `tcp` | `udp` |
| --- | --- | --- |
| Loss repair | TCP's own, per leg: a round trip plus a retransmit timeout | a NACK to the previous node, one leg RTT |
| Head-of-line blocking | at every hop | only at the exit |
| Duplication | no | `duplicate` copies of every packet, per leg |
| Racing | no | any number of paths into one exit |
| Relay cost | `splice(2)`; the payload never enters userspace | bytes must be numbered, so no zero copy |
| If a leg filters or polices UDP | not applicable | nothing gets through, and `deploy` says which leg |

### How the tunnel works

The entry cuts the byte stream into chunks and numbers them. Every chunk goes down
every configured path, `duplicate` times on each leg. Every node keeps the first
copy of each number it sees, drops the rest, and sends what it kept on with the
count set for the leg after. Nodes in between never reorder, so a hole does not
stall the hops behind it; only the exit puts the chunks back in order, on their way
into one TCP connection to the target.

Every leg watches its own numbers. 100 and 102 arrive and 101 does not, so the
receiver asks the node it came from for 101, and asks again every `RTT + 4·mdev` as
measured on that leg by its own ping. A node asked for a chunk it never held wants
it too, so the request walks back one leg at a time until it reaches somebody
holding it. Gap detection is blind past the last number that arrived, and Minecraft
is bursty enough that the next number may be a whole tick away, so a sender that
goes quiet with chunks unacknowledged tells the next hop how far it got, 10 ms after
its last send. That turns a lost tail into ordinary holes on that leg. Behind that,
the entry re-sends its highest chunk end to end when nothing is being acknowledged.

A cumulative "delivered through N" travels the other way a few times a second. It
frees the retransmit buffer at every hop it passes, and it is what stops the entry
reading from the player's socket when the exit cannot drain into Hypixel fast
enough.

A chunk that falls out of every buffer before it can be replaced ends the session:
ordered delivery cannot continue past it, so both ends close and the player
reconnects. TCP has the same failure; it only hides it for longer.

### Duplication

`duplicate` is a property of a leg, applied at both of its ends. Every node drops
what it receives on a leg to one copy of each chunk, then sends it on with the count
of the leg after, so the counts never multiply along the chain: a relay fed two
copies from a lossy leg puts one on a clean one. Retransmissions and the tail probe
carry the same count as everything else on that leg.

The route-level number is every leg's default, a path's `duplicate` overrides it for
that path's legs, and `legs` overrides it for one leg by name. Name a leg by the two
nodes it joins, in the direction data travels toward the exit; the count applies
both ways across it.

```json
"duplicate": 2,
"legs": [
  {"from": "ty", "to": "chi", "duplicate": 1}
]
```

That keeps two copies on HK → Tokyo and drops to one on Tokyo → Chicago, where the
link is clean. Two paths that share a leg share its packets, so they have to agree
on its count; `config` refuses the topology if they don't, unless `legs` settles it.

Duplication replaces a lost packet without waiting a round trip for anyone to ask
for it. It does nothing for a leg that is dropping because it is full — there it
makes things worse — so measure the loss before raising it, one leg at a time: the
link lines in the log say which leg is losing.

### Racing

`paths` sends the same chunks toward the exit several ways at once. Every path
starts at the entry and ends at the exit; `via` lists only what lies between.

```json
"exit": "chi",
"paths": [
  {"via": ["ty"]},
  {"via": [], "duplicate": 1}
]
```

That races HK → Tokyo → Chicago against HK → Chicago direct. The exit takes whichever
copy of each chunk arrives first, so the session gets the better of the two paths per
packet rather than on average, and survives either failing outright.

Paths that share a leg share its packets — the shape is a graph and a node forwards
to all of its successors — so two paths through the same leg must agree on its
`duplicate`, or name it under `legs`. `config` refuses the topology if they don't,
and refuses a set of paths whose edges form a loop.

### Tuning

`tunnel` is passed to every node on the route. Everything has a working default;
the fields exist because the right value depends on the path, not on Minecraft.

| field | default | meaning |
| --- | --- | --- |
| `max_datagram` | 1200 | whole datagram before IP and UDP headers |
| `window` | 1 MiB | unacknowledged bytes one direction may hold before the entry stops reading the player |
| `repair_ms` | 5000 | how long a hole may go unfilled before the session ends |
| `idle_ms` | 120000 | a relay forgets a stream after this long without a datagram |
| `tick_ms` | 5 | granularity of every timer below |
| `ping_ms` | 1000 | per link; a link is called down after five unanswered |
| `nack_min_ms`, `nack_max_ms` | 10, 1000 | clamp on how often a hole is asked for again, and on how often a quiet sender repeats its horizon: `RTT + 4·mdev` on that leg, held between these. Equal values fix it |
| `head_quiet_ms` | 10 | how long a sender with chunks unacknowledged stays silent before saying how far it got |
| `ack_ms`, `ack_repeat_ms` | 20, 250 | how often the exit reports what it has read when that moved, and when it has not |
| `probe_min_ms`, `probe_max_ms` | 100, 1000 | clamp on the entry's blind re-send of its highest chunk, the backstop behind the horizon: end-to-end `RTT + 4·mdev`, doubling per try |

Minecraft traffic is light — a few tens of KB/s at most — so all of these can be
made a good deal more aggressive than the defaults without the extra packets
costing anything a leg would notice. The defaults lean the other way. A setting to
start from, for latency over thrift:

```json
"tunnel": {
  "tick_ms": 1, "nack_min_ms": 5, "nack_max_ms": 50,
  "head_quiet_ms": 3, "ack_ms": 5, "ack_repeat_ms": 50,
  "probe_min_ms": 30, "probe_max_ms": 200
}
```

`nack_max_ms` below a leg's RTT means asking again before the answer could have
arrived, which costs a duplicate retransmit each time and buys back only the case
where the request or the answer was lost. With `duplicate` on the leg that case is
already rare, so leave that one near the RTT of the longest leg.

### Keys

A UDP relay cannot rely on `allow_from`. Over TCP an address has to complete a
handshake before it can be used as a source; over UDP anyone can write it on a
datagram, which would make a relay an open reflector and let a stranger inject bytes
into a live session. So every leg is sealed with its own AES-256-GCM key, with a
replay window behind it.

`deploy` mints those keys into `tunnel-keys.json` beside `topology.json` — gitignored,
mode 600 — and reuses them afterwards, so redeploying does not cut the chain. Keep
that file. Without it the next deploy generates new keys, which only works if every
node is redeployed together.

## Whitelist

Optional, and only on the entry node: it is the only hop that sees a Login Start.
One player per line.

```
# friends
Notch:069a79f4-44e9-4726-a5be-fca90e38aaf5
```

Point `routes[].whitelist` at the file and `deploy` seeds it to
`/var/lib/proxyd/whitelist.txt`, **once**. After that the node owns it: proxyd
rewrites the IGN column when a player renames, so later deploys leave it alone. Edit
it there to add or remove people; the change is picked up on the next login, without
a restart that would drop everyone mid-session. A file that fails to parse leaves the
last good list in place.

Which field is checked depends on how old the client is — the UUID only exists in
Login Start from 1.19, and is only mandatory from 1.20.2:

| Client | Matched on |
| --- | --- |
| 1.20.2+ | UUID; the IGN column is corrected if the player renamed |
| 1.19–1.20.1 | UUID when sent, otherwise the IGN |
| 1.8.9–1.18.2 | IGN — those clients send no UUID at all |

Anyone not on the list gets a login Disconnect and is dropped before proxyd dials, so
they never cost the chain a connection.

### Names that moved on

The UUID column is the identity. The IGN column is only a cache of what that UUID is
called today, because names are released when a player renames and can then be
claimed by someone else. Left alone, a name written here months ago would keep
working for whoever holds it now — which matters most for 1.8.9 clients, since a name
is the only thing they send.

Two mechanisms keep that from happening, both talking to Mojang and never to Hypixel,
so the probe rule in `agents/operational-safety.md` does not apply to them.

**A daily refresh** re-reads every entry's current name and writes back the ones that
changed. A released name cannot be re-registered by anyone else for far longer than a
day (~37 days, **unverified**), so the cache is never stale enough for a recycled name
to be honoured. The last run is recorded in `whitelist.txt.refreshed` beside the list,
so a restart loop cannot turn into a burst of Mojang traffic. Because of this, a name
that matches the file is trusted without a lookup — logins stay local and fast.

**A lookup on a miss** covers the gap. If a client sends no UUID and its name is not
in the file, proxyd asks who owns that name right now and admits them only if the
answer is a listed UUID — then records the new name, so it is never asked again. That
is what lets a player who renamed and logged straight back in on 1.8.9 through, while
a stranger who claimed their old name resolves to their own UUID and is refused. A
login that carries a UUID never triggers a lookup: it already named an identity.

Those lookups are the only attacker-reachable outbound requests here, so they are
rate limited three ways:

| Limiter | Budget | On exhaustion |
| --- | --- | --- |
| per name | 3 / 5 min | drops to 2/hr, then 1/hr, each rung starting spent |
| per source IP | 3 / 5 min | same ladder; catches one address cycling names |
| overall | 50 / 5 min | no ladder; every lookup waits for the window |

A rung that passes with no attempt at all eases back up one, so someone who renamed
and retried a few times is not pinned for the day while sustained pressure stays
throttled. A refused lookup always denies the login — never admits it. Players who
match the list locally are unaffected by any of this.

Cost to the player when a lookup does happen, measured from the HK ingress: **~650 ms
on a cold connection, ~260 ms on a warm one**, and up to the 5 s client timeout if
Mojang is unreachable. It is paid once, because the resolved name is written to the
list, and only by someone who missed. Anyone the list already matches never waits.

The ingress is a bad place to ask from — Chicago completes the same lookup in 68 ms —
but relaying it through another node needs a control-plane protocol we do not have,
to save 400 ms on something that happens a few times a year. The measurements and the
arithmetic are in `agents/link-latency.md` if that tradeoff ever changes.

### What it does not do

**This is not authentication.** Both fields are the client's unverified word. proxyd
never terminates Minecraft's encryption, so unlike a real server it can never ask
Mojang whether a UUID belongs to the person presenting it — the session check happens
between the client and Hypixel, out of our sight (`agents/hypixel-protocol.md` §4). A
modified client can claim any UUID. The refresh and the miss-path lookup fix *stale*
identity, not *forged* identity: they establish which UUID owns a name today, never
that the client is that UUID.

It also does not protect the egress IP. UUIDs are public — resolvable from any IGN in
one API call — so anyone who knows that a listed player uses this proxy can get past
the gate. They cannot log in: Hypixel's session check fails. But they can fail it
again and again, and every attempt reaches Hypixel from the one egress address all the
real players share. Repeated failed session checks from a single datacenter IP is what
gets an egress address blocked, and the whitelist answers *who*, never *how often*.

The control for that is a per-source-IP connection rate limit and concurrent cap on
the ingress — the only hop that sees a real client IP, and the one thing in the whole
exchange a client cannot forge. Not implemented.

So: the whitelist keeps uninvited players off the chain. Treat it as a door lock, not
a security boundary.

## Firewall

After deploy, proxyctl tests every link from the side that will really dial it and
reports which are blocked. When a firewall is the cause it prints the exact rule and
asks before applying it; with no terminal attached it only prints. It never edits a
firewall unprompted.

The rules are one public port on the entry node, and each hop port opened to the
previous hop only:

```sh
ufw allow 25565/tcp                                       # entry node
ufw allow from <prev-hop-ip> to any port <hop> proto tcp  # every other node
ufw allow from <prev-hop-ip> to any port <hop> proto udp  # ... on a udp route
```

The entry needs nothing inbound for a UDP route: it dials out and answers come back
on the same socket. A relay or exit needs one UDP rule per path that reaches it.

A blocked UDP port cannot be found by connecting to it — that always succeeds — so
`deploy` asks the near node whether its link has been answered, and waits a few
seconds before believing it hasn't.

## Verify

`proxyd` logs one line the moment a tunnel link starts answering, and again if one
stops, plus a line per link every 30 s with round-trip time, jitter, ping loss and
retransmit counts. That log is the only view from outside a node of whether a leg is
carrying UDP at all, and at what cost. `proxyctl status` prints the most recent line
per link beside the service state.

```
:9000: link 203.0.113.30:9001 up rtt=123.1ms mdev=0.4ms loss=0.0% sent=812 recv=790 rtx=0 dropped=0
```

`tools/mcping` sends a status ping and prints the MOTD. It claims a wrong hostname by
default, so a reply proves the ingress rewrote the address rather than the client
having asked for the right thing:

```sh
go run ./tools/mcping <entry-ip>:25565
```

Not built or deployed by proxyctl. Copy it to a node by hand if you want it there.

## Measure

`tools/tcpping` times the SYN → SYN-ACK exchange. A network can rate-limit or
deprioritise ICMP independently of TCP, so an ICMP number is not the number proxyd
sees — on one of our legs the two differ by 3 ms, and only on one address family.

```sh
go run ./tools/tcpping -listen :19999          # target, on the far node
go run ./tools/tcpping -c 100 <host>:19999     # probe, from the near node
```

Point it at a mesh address to time a tunnel or a public address to time the raw path;
the destination picks the route, `-4` / `-6` pins the family. Same caveat as `mcping`:
never aim it at the backend from a node.

## Notes

- TCP relays enforce a source-IP allowlist in-binary. Without it a relay is an open
  proxy to Hypixel and the abuse lands on your egress IP. UDP hops cannot use one —
  the address is forgeable — so they authenticate every datagram instead.
- UDP is not uniformly welcome. China-route and other cheap transit commonly polices
  or deprioritises it, so a leg can be slower on the tunnel than on TCP even with no
  loss at all. `transport` is per route: run both and compare the link lines before
  committing.
- Moving a hop onto a mesh VPN is an address change in `topology.json`, not a code
  change. Measured on the HK→TY→CHI chain, Tailscale and public IPv4 land within
  0.5 ms of each other end to end, with Tailscale the steadier of the two. That holds
  only once the tunnel's underlay is pinned to IPv4: left to choose, it took an IPv6
  path on one leg that cost 2.5 ms and 16x the jitter. Default stays public IPv4, for
  one less dependency rather than for speed.
- Egress IP quality matters: Hypixel blocks flagged datacenter ranges. Don't probe the
  backend from an egress node either (pings, status queries, benchmark loops); test
  from your own machine, and treat the egress IP as something you can't easily
  replace.
