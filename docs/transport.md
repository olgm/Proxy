# Transport

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

## How the tunnel works

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
reading from the player's socket when the exit cannot drain into the backend fast
enough.

A chunk that falls out of every buffer before it can be replaced ends the session:
ordered delivery cannot continue past it, so both ends close and the player
reconnects. TCP has the same failure; it only hides it for longer.

## Duplication

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

## Racing

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

## Tuning

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
| `probe_min_ms`, `probe_max_ms` | 100, 1000 | clamp on the entry's blind re-send of its highest chunk, the backstop behind the horizon: end-to-end `RTT + 4·mdev`, doubling per try. The session ends after eight unanswered, and no sooner than `repair_ms` |

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

## Keys

A UDP relay cannot rely on `allow_from`. Over TCP an address has to complete a
handshake before it can be used as a source; over UDP anyone can write it on a
datagram, which would make a relay an open reflector and let a stranger inject bytes
into a live session. So every leg is sealed with its own AES-256-GCM key, with a
replay window behind it.

`deploy` mints those keys into `tunnel-keys.json` beside `topology.json` — gitignored,
mode 600 — and reuses them afterwards, so redeploying does not cut the chain. Keep
that file. Without it the next deploy generates new keys, which only works if every
node is redeployed together.

## Measure

`tools/tcpping` times the SYN → SYN-ACK exchange. A network can rate-limit or
deprioritise ICMP independently of TCP, so an ICMP number is not the number proxyd
sees — on one of our legs the two differ by 3 ms, and only on one address family.

```sh
go run ./tools/tcpping -listen :19999          # target, on the far node
go run ./tools/tcpping -c 100 <host>:19999     # probe, from the near node
```

Point it at a mesh address to time a tunnel or a public address to time the raw path;
the destination picks the route, `-4` / `-6` pins the family. Same caveat as
`mcping` (docs/deploy.md#verify): never aim it at the backend from a node.
