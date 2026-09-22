# Probe

`proxyd`'s link line says what a leg is doing right now. It does not say what the
leg did all week, it smooths its numbers rather than keeping them, and it cannot
answer the question the `duplicate` setting exists for: what does sending every
packet twice actually buy on this leg?

`probed` answers that. It is a separate service — its own binary, unit, user, port
and keys — deployed beside proxyd on every node a production path touches, and
entirely optional: delete the `probe` block and nothing about the chain changes.

```json
"probe": {
  "hz": 1,
  "windows": ["1m", "10m"]
}
```

| field | default | meaning |
| --- | --- | --- |
| `hz` | 1 | probes per second, per class |
| `windows` | `["1m","10m"]` | aggregation periods; every one is written for every class |
| `timeout_ms` | 2000 | how long an unanswered probe waits before it is called lost |
| `max_log_mb` | 256 | rotates the dataset at this size, keeping one previous file |

## What it measures

The routes decide, not the config. Every leg a production path really uses is
probed twice — once at a single copy, and once at the count that leg actually
carries — and the difference between the two is what duplication buys, measured on
that leg, in the same window. Every route longer than one leg is probed the same
way end to end, where the copies are also raced across every path into the exit.

A pair of nodes no route puts traffic between is never probed. Sydney and Hong Kong
are both ingresses and never talk to each other, so that pair is not a leg and does
not become one.

Two things fall out of that and are not oversights:

- **A leg or a chain already carrying one copy is measured once.** The baseline and
  the production class would be the same measurement under two names. Since the
  fleet went to `duplicate: 1` that is every class it has.
- **A route of exactly one leg gets no end-to-end class.** It would be its leg
  class again. Today Sydney and the incumbent Tokyo are one leg each; Hong Kong
  and the second Tokyo each race two paths into the exit, so both have one.

`proxyctl config` prints the whole map:

```
hk (198.51.100.10)
    udp (dials only)              hk>ty          leg    x1   origin
    udp                           hk>ty          leg    x2   origin
    udp                           hk>ch          chain  x2   origin
    udp                           hk>ch          chain  x1   origin
```

The probe is not ICMP and not a separate protocol: it is the tunnel's own framing,
byte for byte, sealed by the same `tunnel.Sealer` under a key of the leg's own. A
probe of a different size or shape would be measuring a different path. What it is
not is proxyd's socket — probed is a separate process, so it measures the link
rather than the queue in front of it.

Nothing it sends reaches the backend. A chain probe stops at the exit exactly as
the tunnel's ECHO does, and for the same reason: one hop further is the backend.

## The dataset

JSONL at `/var/lib/probed/probe.jsonl`, one object per class per window. Only
originators write one — Chicago answers everything and logs nothing.

```json
{"t":"2026-09-05T14:31:00Z","w":"1m","class":"hk>ty","kind":"leg","dup":2,
 "n":60,"sent":60,"got":60,"fwd":60,"loss":0,"loss_fwd":0,"loss_rev":0,
 "min":43.8,"max":45.9,"mean":44.1,"p50":44.02,"p90":44.31,"p99":45.1,"mdev":0.31}
```

`loss_fwd` and `loss_rev` split the round trip into the direction that actually
dropped it. Nothing extra is sent to get them: the answer carries the responder's own
count of probes it has accepted, and differencing that across a window says how
many arrived. The first window of a series has no split and reports `null` rather
than zero — the count that arrived is the difference of two counters, and the
earlier one comes from the window before.

`mdev` is the population standard deviation, as `ping(8)` and `tools/tcpping`
report it. proxyd's own link line reports a *smoothed* mean deviation instead, so
the two are different numbers over the same leg. Do not compare them directly.

`n` is in every line on purpose. A percentile is only as good as its sample count,
and p99 of a 60-sample window is the second-worst of sixty rather than a real p99.
That is why there are two windows and not one: the short one is what a graph needs
to show an incident while it is happening, and the long one is the only one whose
p99 means anything. At 1 Hz they are 60 and 600 samples.

Raising `hz` sharpens both, and costs bandwidth linearly while leaving the log the
same size — the windows aggregate whatever arrives. That matters most for the
number hardest to measure: if a leg drops 1% of single copies, it drops both copies
about 0.01% of the time, so a duplicated class reads 0.000% for hours at 1 Hz. Turn
the rate up when that is the question being asked.

## What it costs

A request is 66 bytes on the wire and an answer 82, IP and UDP headers included.
At the default 1 Hz on a six-node topology, every class at one copy. A leg
carries one pair of datagrams a second for each class that crosses it, and a
raced chain crosses every path it races:

| leg | datagrams/s | per day | per 30 days |
| --- | --- | --- | --- |
| HK→TY2 | 4 | 26 MB | 0.8 GB |
| TY2→CH | 6 | 38 MB | 1.2 GB |
| TY2→CH2 | 6 | 38 MB | 1.2 GB |
| CH2→CH | 6 | 38 MB | 1.2 GB |
| TY→CH | 2 | 13 MB | 0.4 GB |
| AU→CH | 2 | 13 MB | 0.4 GB |
| **all** | **26** | **166 MB** | **5.0 GB** |

That is what crosses the wire; each end bills what it sends and what it receives,
so budget roughly double across the six nodes. For scale, one Minecraft session is
tens of KB/s, so the whole measurement is a few percent of a single player.

**28,800 probes an hour** on this example fleet: 10,800 from the node originating
three classes, 7,200 from the one originating two, and 3,600 each from the three
originating one.

The log is far smaller, because a window is one line however many probes went into
it. A line is about 210 bytes, and a class writes 66 a hour (60 short windows and
6 long ones):

| node | classes | per day | per 30 days |
| --- | --- | --- | --- |
| TY2 | 3 | 1.0 MB | 30 MB |
| HK | 2 | 0.7 MB | 20 MB |
| TY | 1 | 0.3 MB | 10 MB |
| AU | 1 | 0.3 MB | 10 MB |
| CH2 | 1 | 0.3 MB | 10 MB |
| CH | 0 — answers only | — | — |
| **all** | | **2.7 MB** | **80 MB** |

`max_log_mb` bounds it regardless: the file rotates at that size and one previous
file is kept, so the dataset never occupies more than twice it however long a node
runs.

## Verify

`proxyctl status` prints the newest line per class beside proxyd's own:

```
== hk probe
   hk: probe hk>ty2 leg dup=1 w=1m rtt=44.0ms mdev=0.2ms loss=0.0% n=60
   hk: probe hk>ch chain dup=1 w=1m rtt=165.4ms mdev=0.5ms loss=0.0% n=60
```

`deploy` opens one UDP port per node anything is probed toward and verifies it the
same way it verifies a hop: an ingress that only originates dials out and answers
come back on the same socket, so it needs no inbound rule of its own.
