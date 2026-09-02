# proxy

Minecraft TCP accelerator. Chains relay nodes between player and server so every hop
runs its own congestion control — loss on one leg doesn't stall the rest.

Targets Hypixel. Deployed today as HK ingress → Tokyo relay → Chicago egress.

## Design

One binary (`proxyd`) on every node. A node has no role field: it is a list of
listeners, each with one upstream. A listener with a `minecraft` block is an ingress;
one without is a relay. A second ingress is one more listener, not a code change.

The ingress parses exactly one packet — the handshake — rewrites the address to what
Hypixel expects, then relays raw bytes via `splice(2)`. It cannot do more: the client
encrypts from Encryption Response onward.

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
| `routes[].via` | ordered relay chain after the entry |
| `routes[].target` | final `addr`, plus `rewrite_host` / `rewrite_port` |
| `routes[].whitelist` | optional; local `ign:uuid` file seeded onto the entry node |

Hop ports are allocated automatically. `config` prints the map.

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
```

## Verify

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

- Relays enforce a source-IP allowlist in-binary. Without it a relay is an open proxy
  to Hypixel and the abuse lands on your egress IP.
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
