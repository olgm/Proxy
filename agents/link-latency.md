# Link latency

Measured facts about the core links, and the one node-level change that isn't in this
repo. Re-measure with `tools/tcpping` before trusting any number here.

## Underlay is pinned to IPv4

Tailscale picks its own underlay and picked IPv6 for HK↔TY, which is the slower path
there. There is no supported "prefer IPv4" flag (`tailscale set`, `tailscaled --help`,
and the `TS_DEBUG_*` knobs all lack one), so it is a firewall rule on each end:

| node | `/etc/ufw/before6.rules` |
| --- | --- |
| HK | `-A ufw6-before-output -p udp -d 2407:b9c0:1:1d9::/64 -j DROP` |
| TY | `-A ufw6-before-output -p udp -d 2401:e9e0::/112 -j DROP` |

Scoped to the peer's subnet, so other peers keep IPv6. Must sit **before**
`ufw6-before-output`'s `--ctstate RELATED,ESTABLISHED -j ACCEPT`; a plain
`ufw deny out` lands after it in `ufw6-user-output` and never matches an already
established flow. `before6.rules` survives `ufw reload` and reboot. To undo, delete
the line and `ufw reload`.

TY↔CH needed no rule against the old Chicago box, which had no public IPv6. The one
that replaced it on 2026-09-05 does have one, and so does AU — but neither leg runs
over Tailscale: `topology.json` gives every node its public IPv4 as `addr`, so the
data plane is plain IPv4 UDP and Tailscale carries only ssh. Re-check this if a node
is ever moved onto its mesh address.

Confirm with `tailscale status --peers` — the peer line must read `direct <IPv4>:41641`.

## Numbers (2026-09-01, 100 TCP probes per leg)

| leg | path | avg | p50 | mdev |
| --- | --- | --- | --- | --- |
| HK→TY | tailscale (IPv4 underlay) | 44.72 | 44.54 | 0.50 |
| HK→TY | public IPv4 | 44.22 | 44.00 | 1.00 |
| TY→CH | tailscale | 123.11 | 122.99 | 0.43 |
| TY→CH | public IPv4 | 123.30 | 123.93 | 0.99 |

Chain p50: tailscale 167.5 ms, public IPv4 167.9 ms. A wash, with tailscale steadier.
Before the pin, HK→TY over tailscale was 48.12 avg / 46.20 p50 — the pin bought
~2.5 ms avg and cut mdev from 8.3 to 0.5.

## Mojang profile API (2026-09-02)

What a whitelist miss costs the player on the login screen. The operator machine is
not a useful proxy for this — HK is 7x further from Mojang than this laptop is.

### From HK, the node that actually runs the lookup

Mojang is on Azure Front Door. Both endpoints resolve to the same infrastructure and
measure the same:

| endpoint | probes | avg | p50 | p90 | mdev |
| --- | --- | --- | --- | --- | --- |
| `api.mojang.com:443` | 30 | 218.00 | 216.42 | 223.60 | 4.46 |
| `sessionserver.mojang.com:443` | 15 | 215.36 | 216.15 | 222.01 | 5.06 |

0% loss on both, and steady. But 216 ms puts Mojang *further from HK than Chicago is*
(~167 ms end of chain). Azure's anycast is not giving this VPS an Asian edge.

Cost of one lookup, `curl` from HK:

| case | latency | shape |
| --- | --- | --- |
| cold connection | 900–1000 ms | TCP + TLS(2 RTT) + request ≈ 4 x 216 |
| reused connection | 257–278 ms | one round trip |
| outliers | 1.15 s (v4), 3.07 s (v6) | seen once each in ~25 requests |

proxyd should do better cold than curl does: the node's curl predates TLS 1.3
(`--write-out ssl_version` is unknown to it, so <7.61) and spends 2 RTTs on the
handshake. Go negotiates 1.3, which is 1 RTT, so expect **~650 ms cold** — derived
from the measured RTT, not measured directly, because nodes carry `proxyd` and
`tcpping` and nothing else.

**IPv4 and IPv6 are the same here** — 222.7 vs 223.0 ms TCP connect. Unlike the
Tailscale leg above, there is nothing to pin. Do not re-litigate this.

### What it means

A miss costs a player ~650 ms, or ~950 ms worst case. Noticeable, not painful, paid
once per rename because the resolved name is then written to the list, and only by
someone who missed. Still far inside the 10 s handshake deadline, and it happens
before we dial, so it never touches the backend's keepalive budget. The 5 s client
timeout is ~23 RTTs of headroom, which the 3.07 s outlier says is not excessive.

A Method A refresh walk is sequential and pools its connection: ~940 ms for the first
entry, ~260 ms for each one after. A 50-name list is **~14 s** of background work,
once a day. Fine, but an order of magnitude off what the same walk costs from a
well-connected machine — do not size this from a laptop measurement.

### Every node is a different distance from Mojang, and not the way you would guess

| node | TCP RTT to edge (p50) | full lookup (p50) |
| --- | --- | --- |
| HK | 216.4 ms | ~940 ms |
| Tokyo | 2.4 ms | **663 ms** |
| Chicago | 4.2 ms | **68 ms** |

Tokyo is 2.4 ms from an Azure Front Door edge and still takes 663 ms to complete a
lookup. Chicago is *further* from its edge and finishes 10x faster. The reason is that
a profile lookup is a dynamic API call, not a cacheable object: the edge terminates
TLS locally and then fetches from Mojang's origin, which is evidently in North
America. Node→edge distance buys you the handshake and nothing else; the origin fetch
dominates, and it is trans-Pacific from Tokyo.

So the useful measurement is total lookup time, never ping to the edge. HK's TCP RTT
made it look 90x worse than Tokyo; by the number that matters it is only 1.4x worse.

### Relaying the lookup through Chicago

Asking a nearer-to-Mojang node to do the lookup and return the answer is arithmetic
that works, with one measured leg to add: HK→Chicago direct is 184.7 ms p50
(20 probes, 0% loss, mdev 4.1). Chicago is the only candidate — Tokyo is 44.5 ms away
but 663 ms from an answer, so relaying through it is *slower* than HK asking directly.

| approach | cost of a miss |
| --- | --- |
| HK asks Mojang (what we do; Go, TLS 1.3) | ~650 ms |
| HK asks Mojang (measured, node curl, TLS 1.2) | ~940 ms |
| **HK asks Chicago over a held-open connection** | **~253 ms** (184.7 + 68) |
| HK asks Chicago, new TCP connection each time | ~438 ms |
| HK asks Chicago over TLS, new connection | ~808 ms — worse than direct |
| HK asks Tokyo | ~708 ms — worse than direct |

The 253 ms figure needs both: a connection HK holds open to Chicago, and TLS
terminated *at* Chicago. A `CONNECT` tunnel does not work — TLS stays end to end, so
every handshake round trip still pays 189 ms and the total lands at ~563 ms, barely
better than direct.

Not built. It buys 400 ms on a path that fires when a listed player renames and logs
in on a pre-1.19 client before the next refresh — a few times a year — and costs a
control-plane protocol between nodes, a fourth firewall rule, reconnect logic, and a
new way for logins to fail when Chicago is down. Recorded because the numbers are
real and the tradeoff may look different later.

### From the operator machine, for contrast

20–35 ms steady, ~160 ms for the first lookup while DNS is cold, 24–28 ms for an
unknown name. Recorded only to show how misleading it is: 7x optimistic.

## UDP tunnel, measured on the deployed chain (2026-09-03)

The transport was switched to `transport: udp`, `duplicate: 2`, single path. UDP is
**not** penalised on either leg, which was the open question — China-route transit
often polices it, and this one does not.

| leg | TCP p50 (tcpping, 2026-09-01) | tunnel ping | mdev |
| --- | --- | --- | --- |
| HK→TY | 44.00 | 43.8–44.1 | 0.1–0.4 |
| TY→CH | 123.93 | 122.0–122.5 | 0.1–0.3 |

Chain ~166.2 ms against 167.5 (tailscale) / 167.9 (public IPv4) over TCP, and
steadier: mdev on the tunnel is 0.1–0.4 where tcpping measured 0.99–1.00 on public
IPv4. Ping loss is 0.0% on both legs over hours, with single-digit retransmits.

Do not read the loss column from before this date: pings sent and pongs received
were counted in separate 30 s windows, so any ping in flight at the boundary was
written off. It reported a steady 3.2% on a chain that was dropping nothing.

### End to end, 4 MiB through the real chain

Verified against a throwaway TCP source on the exit — never the backend, so this does not
break the probe rule and can be repeated. HK ingress → TY → CH → a socket on CH:
**4194304 bytes, byte-exact (md5 matched), 2.10 s.**

That is 2.0 MB/s of payload, or 4 MB/s on the wire with `duplicate: 2`. The ceiling
is the window, not the path: 1 MiB over a 166 ms round trip is ~6.3 MB/s of chunks,
halved by duplication to ~3.2 MB/s. Raise `tunnel.window` if a large transfer ever
matters. It does not for Minecraft — a session is tens of KB/s — but a resource pack
download is the case that would notice.

Two things to know before repeating this. The source has to read what the proxy
forwards to it: a socket closed with unread data sends RST and the kernel throws away
whatever was still queued, which looks exactly like the tunnel truncating the stream.
And `sendall` returning proves only that the bytes reached a kernel buffer — on
loopback that can be several MB — so it is not evidence the proxy read them.

## Duplication turned off (2026-09-09)

Every tunnelled route went from `duplicate: 2` to `1`. Over ~23 h of 10m windows,
aggregated out of each node's `probe.jsonl` as `(sent-got)/sent`:

| class | at 1 copy | at 2 copies |
| --- | --- | --- |
| `hk>ty` leg | 0.416% | 0.330% |
| `hk>ch` chain | 0.515% | 0.342% |
| `ty>ch` leg | 0.149% | 0.151% |
| `au>ch` leg | 0.010% | 0.007% |

The second copy does something on the HK legs and nothing on the other two: a fifth
off `hk>ty`, a third off `hk>ch`, from a rate already under 1%. It buys no latency —
p99 is identical to two decimals in every pair — and it does not help where it would
matter most. Median window loss is 0.000% on every class, and p90 and the worst
window are the same at one copy or two (24.17% against 24.00% on `hk>ty`), because an
outage takes both copies with it. Isolated loss at a few tenths of a percent is what
the tunnel's NACK already repairs in one leg RTT. So the copy was costing twice the
tunnel traffic to pre-empt a repair that works.

**`loss` in the dataset is already a percentage.** `round(rt)` in
`internal/probe/log.go`, where `rt` is `100*(sent-got)/sent`. Multiplying it by 100
again in a throwaway query reports impossible rates — 750% — and looks exactly like a
counting bug in probed. There is no such bug; this one cost an afternoon on
2026-09-09. Aggregate from `sent` and `got` for a weighted rate across windows;
averaging the `loss` column unweighted is wrong too when windows differ in `n`.

## The four-node chain, measured by the tunnel itself (2026-09-05)

Chicago's address changed on 2026-09-05 and Sydney was added. These are `proxyd`'s
own link lines — smoothed RTT over a leg's ping, read out of
`proxyctl status` a few minutes after the deploy, so they are the number the chain
actually runs on rather than a separate probe.

| leg | RTT | mdev | loss |
| --- | --- | --- | --- |
| HK→TY | 44.0–44.5 | 0.2–0.9 | 0.0% |
| TY→CH | 121.2–121.9 | 0.1–0.6 | 0.0% |
| AU→CH | 176.5–177.0 | 0.1–0.7 | 0.0% |

Against the old Chicago, TY→CH measured 122.0–122.5 on the same instrument on
2026-09-03 and 123.93 p50 by tcpping on 2026-09-01, so the swap bought roughly
1–2 ms on every path that ends in Chicago. Small, and it was free.

What each ingress owes to the backend's doorstep, being the sum of its legs:

| ingress | path | to the exit |
| --- | --- | --- |
| HK | TY, CH | ~166.3 ms |
| TY | CH | ~121.9 ms |
| AU | CH | ~176.5 ms |
| CH | none | 0 — it dials the backend itself |

AU→CH at 176.5 ms is worse than HK's whole two-leg chain. That is the path, not the
proxy: Sydney to Chicago is a long way and there is no node in between to shorten
it. A relay on the US west coast is the thing that would, if a Sydney player ever
justifies one.

## The second Chicago, and the leg the race rests on (2026-09-13)

`ch2` (a second provider's `ord` region, 198.51.100.31) becomes a pure relay in v2.3.2, so `ty2` can
race `ty2>ch` against `ty2>ch2>ch`. The hop between the two Chicago boxes had
never been measured — every earlier number was Tokyo-to-Chicago.

| leg | probes | min | avg | max | mdev | loss |
| --- | --- | --- | --- | --- | --- | --- |
| ch → ch2, public IPv4, ICMP | 30 | 1.531 | 1.769 | 5.187 | 0.639 | 0.0% |

ICMP, so read it as approximate — on IPv4 this repo has measured no protocol gap
(43.82 ICMP against 44.08 TCP on HK→TY), which is why it is good enough to decide
with and why probed re-measures it properly once deployed.

What it makes the raced path cost:

| path | normal | during the nightly NTT window |
| --- | --- | --- |
| `ty2 → ch` direct | **121.4** | 126–130 |
| `ty2 → ch2 → ch` | 124.2 (122.45 + 1.8) | **124.2** |
| what the chain gets, being the first copy | 121.4 | ~124.2 |

So the race does not touch the median and caps the overnight tail about 6 ms lower
than it is today. The 5.19 ms max in 30 packets is the Hyper-V stall showing up
small; mdev 0.64 against ty-b's 0.14-0.21 says the same. Neither matters on a
raced path: that box was rejected as an exit for its nightly reroute and is fine as
one of two paths.

## Protocol matters, and it is not uniform

Same leg, same 30-sample run, HK→TY:

| path | ICMP | TCP |
| --- | --- | --- |
| public IPv4 | 43.82 | 44.08 |
| public IPv6 | 51.51 | 48.63 |

IPv4 shows no protocol gap. IPv6 penalises ICMP by ~2.9 ms relative to TCP, and is
~7.7 ms slower than IPv4 outright. So an ICMP number is not a TCP number, and the
error is path-dependent — measure with the protocol you actually carry.
