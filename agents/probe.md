# Probe

`internal/probe`, `cmd/probed`, `cmd/proxyctl/probe.go`. The measurement service.
Optional, separate from proxyd in every way that matters, and scoped by the routes.

## Why it is not in proxyd

It measures the link, not proxyd's socket. Two consequences, and both were the
point of putting it outside:

- It cannot see contention on proxyd's socket or its timer goroutine. A number
  from here is what the *path* costs, not what a loaded proxyd costs.
- It can be stopped, restarted and redeployed without touching a live session.

It has its own binary, unit, user (`probed`), port and per-leg keys. Holding
probed's keys is not a way into a session; that is why they are not proxyd's.

## What is measured, and why that set

The routes decide. Nothing here is configurable except how fast and how it is
aggregated, because the interesting question is about the production path and a
knob that let it drift off the production path would only produce wrong answers.

| class | on | says |
| --- | --- | --- |
| leg, 1 copy | every leg a production path uses | what the raw link costs |
| leg, N copies | the same leg at its real count | what duplication buys, on that leg |
| chain, 1 copy | every route longer than one leg | the raw path end to end |
| chain, N copies | the same route at its real counts, raced | what a player's packet sees |

Two rules follow from that table and are worth not re-deriving:

- **A leg or a chain carrying one copy is measured once.** The baseline and the
  production class would be the same measurement under two names. `baselines` in
  `cmd/proxyctl/probe.go` is the chain half of that; `counts` is the leg half.
  Since 2026-09-09 the fleet is at `duplicate: 1`, so every class is a single one.
- **A route of exactly one leg gets no chain class.** It would be its leg class
  again. Today that is both Tokyos and Sydney; only Hong Kong has a chain.

**A class name does not identify a class.** From v2.3.2 both raced routes produce a
chain named for its ends, and one such chain's ends are `ty2` and `ch` — which is
also one of its own legs. Two classes are called `ty2>ch`, one leg and one chain,
and only `kind` tells them apart. The dataset and the feed always carried it;
probed's journal line and `proxyctl status`'s key did not until v2.3.2, and the
status view silently showed whichever flushed last.

From v2.3.1 the Hong Kong chain relays through `ty2`, so the class set moves with
it: `hk>ty` is gone, `hk>ty2` and `ty2>ch` are new, and `hk>ch` keeps its name
while changing which Tokyo it crosses. A reader joining `probe.jsonl` across the
cutover has to cut on the deploy timestamp; nothing in the file says the path
changed.

A pair of nodes no route puts traffic between is never probed. AU↔HK is not a leg
and never becomes one by adding a class.

## Wire

The tunnel's framing, byte for byte: `[4B epoch][8B counter][AES-256-GCM + 16B tag]`
via `tunnel.Sealer`. Plaintext is one byte of type then:

| type | body | sealed | on the wire (IPv4) |
| --- | --- | --- | --- |
| REQ | class u8, seq u64 | 38 B | 66 B |
| RESP | class u8, seq u64, recv u64, high u64 | 54 B | 82 B |

Sharing the framing is deliberate. A probe of a different size or shape measures a
different path, and sharing the code is what stops the two drifting apart.

## Invariants

- **Roles are emergent, again.** A class with down links and no up ones
  originates, one with no down links answers, one with both relays. No role field.
- **Every node drops what it receives to one copy, then sends on with its own
  leg's count.** Counts never compound: a relay fed two copies puts the configured
  number on the leg after, not four. `TestChainCountsDoNotCompound`.
- **The count is the hop's, not the class's.** Two paths into one exit may carry
  different counts, and a node racing both honours each. The class-level
  `duplicate` is only the default, and the label the dataset is written under.
- **A sample is filed under the time it was sent**, never the time it came back.
  Filing by arrival moves every slow sample one window right, which attributes a
  latency spike to the calm minute after it.
- **A window is flushed one probe timeout after it closes.** proxyd learned this
  the hard way: counting sends and replies in separate windows reported a steady
  3.2% loss on a chain dropping nothing.
- **The answer carries the responder's counters**, so round-trip loss splits into
  the direction that dropped it without any extra packet. `recv` counts distinct
  probes *after* de-duplication, which is what makes differencing it across a
  window a count of probes that arrived rather than of packets.
- **The first window of a series has no direction split and cannot.** The count
  that arrived is the difference of two counters and the earlier one comes from the
  window before. `fwd` is null there, not zero.
- **Nothing reaches the backend.** A chain probe stops at the exit exactly as the
  tunnel's ECHO does. There is no forwarding past a node with no down links.

## The feed

`feed.go`. When `PROBED_PROBE_WEBHOOK` is set, a closed window is also posted to
a Discord webhook, one line per class. Off when it is unset, which is the normal
state and the whole of the check.

- **Only an originator posts**, which falls out of where `post` is called from:
  the same place the dataset is written. Chicago holds no measurement of its own.
- **The window filter is not a measurement setting.** `feed_windows` picks what
  reaches the channel; every window is still measured and still logged. The
  default — the longest configured — lives in `FeedWindows` here rather than in
  proxyctl, because both need the answer and only one of them may own it.
- **This is the one thing probed talks to outside our own infrastructure.** It
  was true that it spoke only to its peers, and that line has moved: peers, and a
  webhook the operator configured. The rule that matters is unchanged — nothing
  here reaches the backend, and a chain probe still stops at the exit.
- **The feed cannot slow the measurement.** Posting goes through a queue that
  drops rather than blocks, so a webhook that is down or rate-limited costs a
  window nothing.

## Reading the dataset

JSONL at `/var/lib/probed/probe.jsonl`, one object per class per window, rotated
at `max_log_mb` keeping one previous file. Only originators write: Chicago answers
everything and logs nothing.

`mdev` is the population standard deviation, as `ping(8)` and `tools/tcpping`
report it. **proxyd's own link line reports a smoothed mean deviation instead**, so
the two are different numbers over the same leg. Do not put them side by side.

`n` is in every line because a percentile is only as good as its sample count: p99
of a 60-sample window is the second-worst of sixty. That is the whole reason there
are two windows rather than one.

## Testing

`probe_test.go` puts a real UDP relay between real sockets and drops datagrams on
demand, the same way `tunnel_test.go` does, so loss is injected on the wire rather
than through a seam. Tests never index the log by position: the first window a node
writes is a partial one opened before the probes started, so how many samples it
holds is a race with startup. Use `awaitOne`.
