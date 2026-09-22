# Trial

`internal/trial`, `cmd/triald`, `cmd/proxyctl/trial.go`. A bake-off harness for
candidate nodes. **Temporary: delete it when it has answered its question.**

**DONE 2026-09-13T08:04Z.** The mesh is live as the triangle below: hk and ty-d
are out, ch/ty-b/ch2 are in, all six directed legs carrying. First windows:
`ty-b>ch` 121.5, `ty-b>ch2` 122.2, `ch2>ch` 1.8, `ch>ch2` 2.1,
`ch>ty-b` 121.3, `ch2>ty-b` 122.1.

**Exclude 2026-09-13T08:03:02Z-08:04:07Z from any analysis** — the teardown, a
correlated outage on every leg at once, exactly like the re-IP and redeploy windows
below. The windows either side of it are partial (n=56-57, not 60) and read 5-6.7%
loss on the ch legs; that is the artifact, not a fault.

**Observing this mesh perturbs it, and the first hour showed how.** In the
08:08-08:09 window every leg *touching* ch2 carried one ~1.2 s outlier (mdev
153.5 / 95.4 / 50.2 / 25.5 against p50 unmoved and 0% loss) while every leg that did
not was flat at 0.3. It was **not** the box — `probed`'s `ty2>ch2` in the same
minute read mdev 0.1, and probed runs on that host too. It was **not** the ufw rule
added for `ch` — `/etc/ufw/user.rules` mtime is 08:09:56, 54 s after that window
closed, and the window containing it is flat. What is left is a pause inside
triald's own process or its scheduling, on a 1 vCPU box that was simultaneously
serving several `journalctl` reads over ssh. Unproven, but the obvious candidate is
the monitoring itself: do not read a single spiky minute here without checking
whether somebody was shelled into the node during it.

Final datasets from the five-node mesh were pulled first: 6.1M records, ~858 MB, in
`trial-data/`. `/var/lib/triald` is left behind on hk and ty-d (205 MB and 121 MB)
because uninstall does not delete it; both nodes have ample disk and the data is
already local, so it is cleanup nobody needs to hurry.

**2026-09-20: uninstalled everywhere.** Seven nights of the race were enough, and
the night v1 was sunset was the night to take it down: `trial uninstall` on ty2, ch
and ch2, datasets pulled compressed into `trial-data/` (the Sep 13 five-node
pulls moved to `trial-data/mesh5-2026-09-13/` first, because `pull` overwrites),
and `trial.json` retired to `trial.json.bak`. Two things bit: `trial pull` cats a
457 MB file over ssh from Tokyo and does not finish inside ten minutes, so pull with
`gzip -1 -c` on the node instead; and ch2's `ssh` in `trial.json` was its public
address, which its `ufw` denies — the topology's alias `ch2-ty-a-probe` reaches it.
`/var/lib/triald` is left on all three nodes. Nothing here runs anywhere now; the
code stays until someone deletes `cmd/triald` and `internal/trial`.

**2026-09-22T05:52Z: re-armed as au / ch / ch2, for a few days.** The question:
should AU's traffic to Chicago go via `ch2` (candidate ty-a's provider, ord region)
rather than `ch` (candidate ty-c's provider)? A 300-ping spot check from AU read
176.16 ms to one and 176.49 to the other, which is
too close to call. So the mesh is two echo-only legs, `au-ch` and `au-ch2`. There
are no runs, so each end sends its own echo, all four directions are measured, and
nothing is forwarded. `au>ch` is production and is here as the **control**, which is
the same justification v2.3.2 gave. ids: au **8** (new), ch 5, ch2 7, left as
they were so old `path` strings still resolve. `expect_ms` is 176 for both legs,
taken from the spot check, so a trace fires above 181.

How it was armed without touching production: four `ufw allow ... 9400 proto udp`
rules (au from ch and from ch2, ch from au, ch2 from au), then
`proxyctl trial deploy`. That only installs and restarts the `triald` unit. proxyd
and probed kept the PIDs and start times they had before, on all three nodes.
First 1m windows, all n=60 and 0% loss: `au>ch` 176.37, `au>ch2` 176.16,
`ch>au` 176.37, `ch2>au` 176.25.

**When pulling:** ch and ch2 *append* to their old `trial.jsonl` (381 MB and
179 MB of the race dataset, already copied locally), so a plain `trial pull` fetches
all of that again and, per the note above, will not finish from here. Grep the four
leg names on the node, or `gzip -1 -c`, instead. au's file holds only this mesh.
`trial uninstall` still has no node filter. It is correct for this mesh, since all
three nodes should lose triald together, but remember to also remove the four ufw
rules.

**2026-09-13, v2.3.2: it measures the race.** `ch2` went into production as a
raced second path, so the mesh is the three nodes that race — `ty-b`, `ch`,
`ch2` — flooding both paths from ty-b and back from ch. These are
production legs, which is a departure from the rule below that every leg here is
one no route uses; the justification is that **probed cannot answer this question
at all**. probed de-duplicates exactly as the tunnel does, so its chain class
reports the winner's latency and never which path won or by how much. That is the
one number that says whether racing is doing anything. Watch a few nights, then
delete the whole service.

**2026-09-13, v2.3.1 (superseded by the line above): reduced to one leg.** The Tokyo question is answered and
ty-b is production, so the mesh keeps only `ty-b <-> ch2` — the pair
that says whether a Chicago box on a different provider than `ch` removes the
nightly NTT reroute, and the only remaining leg no route uses. No runs: it is echo-only, which is how that leg
has been measured all along, so the numbers stay comparable across the cut. hk, ty
and ch leave the mesh. `trial uninstall` has no node filter, so the order is pull,
uninstall everywhere, reduce the file, redeploy the two — and the gap that leaves
is a correlated outage to exclude, like the re-IP window.

## Why it is not probed

probed measures the production path and may never be pointed off it — that rule is
in AGENTS.md and it is a good one. Every leg here is deliberately one no route
uses, because the question is which nodes the route *should* use. Same rule, other
side of it, so it is a different service: own binary, unit, user, port and keys,
and neither knows the other exists. Both run on the same node.

That is also why this config is written by hand while probed's is derived from the
routes. Do not "fix" that by generating it from `topology.json`: there is nothing
in the routes to generate it from.

## The three differences from probed

- **Nothing is de-duplicated.** probed collapses the copies racing into an exit,
  because a player's packet only has to arrive once. Here every copy is logged
  with the route it took: which route won, and by how much, is the measurement.
- **A route travels on the wire.** Nowhere else does a node identify itself in a
  datagram. A copy that cannot say where it has been answers nothing. The ids are
  data only — the key still decides which leg a datagram belongs to, so nothing
  acts on an unopened datagram and no address is trusted.
- **Every hop echoes what it forwards.** This is the load-bearing one. See below.

## Why the echo, and what cannot be measured without it

A chain that only forwards yields arrival times taken on *different clocks*, so a
leg's latency comes out as one-way delay plus clock offset. Measured 2026-09-07:
ty-d and ty-c run chrony (root dispersion 1.0 ms and 7.0 ms); hk, ty-b,
ty-a and ch run systemd-timesyncd, undisciplined. The intra-Tokyo legs are about
1 ms one way. The error is larger than the signal, and clock drift looks exactly
like the latency drift being hunted.

So each hop echoes the probe back over the leg it arrived on while also passing it
onward. Both timestamps of a round trip are then taken on one machine.

Needs no clock sync at all: per-leg RTT; per-leg forward vs reverse loss (the echo
carries the far end's arrival count, differenced across a window — probed's trick);
jitter; **racing margin**, because competing copies land on the *same* node and are
compared against that node's *one* clock; and cross-leg failure correlation, which
needs only the 1-second alignment timesyncd already gives.

**Not obtainable, and must not be claimed: one-way latency split by direction.**
That needs synchronised clocks. RTT plus a direction-split loss answers every
question actually asked.

## Invariants

- **Roles are emergent.** A run is named for the node that starts it; a node whose
  own name is that run's `from` originates it, one that lists it only says where
  to pass an arrival on, and one that does not list it is that run's terminus. No
  role field.
- **The loop guard is the route.** A copy is never forwarded to a node already in
  its path, and `maxPath` caps it at 8 regardless. A flood over a mesh with a
  cycle does not stop on its own; this is the only reason the full route rather
  than just the sender is carried.
- **`tick` is the wall second, taken independently on every node.** At 1 Hz every
  node stamps the same tick for the same second with nobody coordinating, so six
  nodes' datasets join on that column alone. Coarse NTP is enough for this and is
  not enough for anything else here.
- **`Names` is the whole mesh's table and every node gets all of it.** The exit
  sees copies through nodes it has no leg to and starts no run from; without the
  roster it writes bare ids, and the racing analysis happens at exactly that node.
- **A node that starts a run may not also set `echo` on a leg.** A probe says
  which run it belongs to by whose id opens its path, so its peers could not tell
  the two apart. `echo` is for a leg no run crosses — today the three ty-d legs,
  which are off both floods because ty-d is the incumbent rather than a candidate.
- **The trigger reads a closed window, never a single probe.** At 1 Hz a leg with
  a few ms of mdev would trip a per-sample threshold continuously. The cooldown is
  per leg: a node whose legs all degrade at once has that many paths to describe.
- **A sample is filed under the time it was sent**, and a window closes one probe
  timeout after it ends. Both live in `internal/window` with the arithmetic, and
  probed obeys the same two.
- **`loss_fwd` reads a little high when the reverse direction is the lossy one.**
  How many probes arrived is learned from a counter the far end carries back on its
  echoes, so a window whose last echo was dropped reports a slightly stale count.
  A probe or two in a hundred. probed has the same property; do not read a 1%
  `loss_fwd` beside a 33% `loss_rev` as two problems.

## Reading the dataset

JSONL at `/var/lib/triald/trial.jsonl`, on **every** node — unlike probed, where
only an originator has anything to say. Three kinds, told apart by `k`:

| `k` | one per | says |
| --- | --- | --- |
| `rx` | arrival | `leg` it came in on, `tick`, full `path`, `onward` copies sent |
| `win` | leg-direction per window | probed's fields, spelled the same way, plus `expect` |
| `trace` | traceroute | the window that triggered it and mtr's own JSON, unparsed |

`rx` timestamps are RFC3339**Nano**. Copies racing into Tokyo land within a
millisecond of each other and a second-resolution stamp reports every race a tie.

`win` uses probed's field names on purpose: goal one is setting a candidate's
numbers against the incumbent's, and that only means something if both were
computed the same way. They are — `internal/window`.

Sample counts per leg are **not uniform**, because a leg's samples come from the
runs crossing it: 1/s on `hk>ty-a`, 3/s on `ty-c>ch`. Read `n` before a p99.

## Operational notes

- The data plane is **public IPv4**, never the Tailscale 100.x addresses.
  Production is public IPv4 (link-latency.md), and the overlay would hide the
  carrier diversity the whole trial is about.
- `mtr --json` is what the tracer runs. `mtr -r` **segfaults** on ty-b
  (mtr-tiny 0.95, `buffer overflow detected`); `--json` is fine there.
- The unit needs `AmbientCapabilities=CAP_NET_RAW`. `NoNewPrivileges=true`
  disables `mtr-packet`'s file capability, so without it every trace fails.
- Nothing here reaches the backend: every leg ends on a machine we run.
- **2026-09-10: the mesh is down to four nodes, and `trial.json` is edited but NOT
  deployed.** `ty2-ty-a-probe` (198.51.100.21) and `ty2-ty-c-probe` (198.51.100.23)
  were destroyed at their providers once the bake-off answered both goals. `trial.json`
  now lists only hk(1), ty-b(3), ch(5) and ty-d(6) — **ids deliberately left
  unrenumbered** so the `path` strings in the three days of collected data still resolve
  to the same nodes. The four surviving nodes still hold the six-node config, so they
  will keep probing two dead addresses and logging 100% loss on
  `hk>ty-a`, `ty-a>*`, `*>ty-c` and `ty-c>*` until a deploy. That is the same
  edited-but-not-deployed state that caused the 2h20m dark hole above; here it is
  harmless because the peers are gone rather than moved, but **exclude those legs from
  any window after 2026-09-10T13:1xZ** rather than reading them as loss.
  The three stale `ufw` allow rules for the released addresses (9400/udp from
  198.51.100.21 on hk and ty-d, from 198.51.100.23 on ty-d) were deleted; both
  nodes now allow 9400 only from ty-b.
- **2026-09-12: a second Chicago endpoint was added to test the nightly reroute.**
  `ch2` (id 7, candidate ty-a's provider, `ord` region, 198.51.100.31) joins the
  mesh as **one extra leg, `ty-b-ch2`, crossed by no run** — so it is echo-only and
  changes nothing about how `hk>ty-b>ch` is measured. It exists to answer one
  question: the `ch>ty-b` return leaves GSL for NTT every evening (19:00-05:00Z,
  three nights for three, 308 -> 175 -> 49 minutes), and a second Chicago provider says
  whether that window is the Chicago end's transit engineering or something wider.
  `expect_ms` is 122, from a 100-packet measurement at 121.87 (the incumbent ty-c box
  measured 121.27 in the same minute). Note `trace.over_ms` is 5 mesh-wide, so this leg
  only traces above 127 and will miss the 126.4 ms state the incumbent leg catches.
  In the fast state both Chicago boxes take the **same GSL path hop for hop** from
  `203.0.113.7` (Seattle) onward; ty-a hands off to its own upstream, ty-c via a
  different upstream. It reports as a Hyper-V guest — the same host type that
  the destroyed `ty2-ty-a-probe` host-stalled on, so **read a mesh-wide spike minute
  with ch2 in it as a suspect host stall before believing the path.**
- **2026-09-12, 17h of ch2: the nightly reroute is the Chicago end's, and it is
  ty-c's alone.** Over 07:00-23:46Z, 1006 1m windows each, `ty-b>ch` spent
  19.9% of minutes at or above 123.5 ms (median 121.41, p95 128.59, worst 130.23)
  while `ty-b>ch2` spent **0.0%** there: its whole 17-hour p50 range is
  122.40-122.56, a 0.16 ms spread, straight through both of that day's episodes
  (18:07-18:38 and 20:58 onward). Manual `mtr` 30 seconds apart during the second
  one: `ch>ty-b` rode Akamai then NTT GIN (`a05.chcgil09` -> `r27.sttlwa01` ->
  `r34.tokyjp05`) at 126.26 ms; `ch2>ty-b` rode GSL hop for hop
  (`sea-drtsea10` -> `ty-eqxty2` -> `ty-eqxty8`) at 121.85 ms. The forward
  `ty-b>ch` direction stayed on GSL and read a normal 121 ms at its last GSL hop
  while the leg's RTT was 128, so the change is on the `ch>ty-b` return. This
  is path evidence, not a one-way latency split; do not turn it into one.
- **2026-09-13, correction to the line above: ty-b is NOT exonerated.** Adding the
  incumbent to the comparison changes whose flaw it is. probed's `ty>ch` measures ty-d
  to the *same* ty-c Chicago box, and over the same 82.8 h it sat at p95 121.55 /
  p99 121.60 with **4** elevated minutes in 4942. In all **767** minutes `ty-b>ch`
  is on NTT, `ty>ch` is unmoved at 121.42 median. The four minutes ty-d did rise
  (09-09T15:24-15:27, 137 ms) ty-b rose identically, so that one is a real shared
  GSL event and the only one. Same box, same second, two Tokyo nodes 0.69 ms apart:
  the reroute is a **destination-prefix decision**, not the Chicago box's egress in
  general. Traces at 00:18:10Z: both paths GSL hop for hop from `sea-drtsea10` through
  `ty-eqxty8-sw4`, diverging only at the last Tokyo handoff (ty-b direct, ty-d
  via `917.as` -> `ty-d.io`). Both framings hold and neither alone is honest — it
  happens only to ty-b, *and* a different Chicago provider does not do it.
- **The reroute is not shrinking after all.** Degraded minutes per 19:00-05:00Z night,
  post re-IP: 09-09 308, 09-10 175, 09-11 49, **09-12 169 by 23:46Z with five hours of
  the window still to run**, plus 32 daytime minutes at 18:07-18:38. Three points were
  a trend and the fourth broke it; do not plan on it healing itself.
- **ch2 host-stalls, and the p50 will not show it.** 29 of its first 1006 minutes
  (2.9%) carry second-scale spikes, worst 1791 ms, with the minute's p50 untouched.
  They are the box: each lands on **both** ends of the leg within ~20 ms
  (`ty-b>ch2` and `ch2>ty-b`) while `ty-b>hk` sits at 44 ms and
  `ty-b>ty-d` at 4 ms in the same minute. Read `max` and `mdev` on this leg,
  never `p50` alone. A 1.8 s freeze costs a player more than a steady 5 ms, so this
  box answers the routing question without being the box to ship.
- **ch2 has logged zero traces and cannot log one.** `trace.over_ms` is 5 mesh-wide
  and its `expect_ms` is 122, so it only fires above 127 and it has never been near
  127. Every path claim about it above comes from a hand-run `mtr`. Drop its
  `expect_ms` to 121 if it should document its own path.
- **2026-09-13, candidate against incumbent, and where the spikes are.** 82.8 h post
  re-IP, 4968 aligned 1m windows, trial for ty-b against probed for ty-d (same
  `internal/window`, both 1 Hz, n=60 — that is what makes them comparable):

  | | ty-b | ty-d/ty |
  | --- | --- | --- |
  | HK median / p99 | 44.05 / 44.18 | **43.94 / 44.07** |
  | HK worst minute, loss | **81.46**, **0.214%** | 101.21, 0.233% |
  | CH median | **121.42** | 121.44 |
  | CH p95 / p99 | 129.97 / 130.19 | **121.55 / 121.60** |
  | CH loss | **0.002%** | 0.019% |

  `ty-b>ty-d` is 0.69 ms median, so none of that spread is distance.
- **Transients and the sustained state are different phenomena; do not pool them.**
  Spike = a 1m window whose `max` exceeds its own `p50` by 50 ms. By that rule
  `hk>ty-b` 1.21%, `hk>ty` 1.31%, `ty-b>ch` 0.06%, `ty>ch` 0.45% — ty-b
  equal or better everywhere, 7x cleaner to Chicago. **Transients have no diurnal
  structure at all** on either node, flat across all 24 hourly buckets. Every bit of
  time-of-day signal is in the sustained state, which is 15.44% of minutes on
  `ty-b>ch` (18:00-04:00Z) against 0.08% on `ty>ch`.
- **HK transients are independent; HK sustained events are shared.** Only 7 of
  ty-b's 60 HK spike minutes coincide with a `hk>ty` spike (12%, phi +0.101) — two
  carriers into one ingress, spiking on different minutes. But 9 of the 10 *sustained*
  HK elevations land in the same minute at near-identical magnitude (78.57/76.23,
  81.46/81.64, 81.22/82.24). Those are HK-side; charge them to neither Tokyo node.
  Loss minutes sit between: phi +0.219, 27% coincidence, and both legs' loss is
  overwhelmingly reverse-direction (547 vs 568 packets rev, 92 vs 125 fwd).
  The 12% independence is only worth something to a chain that races copies, and the
  fleet has been at `duplicate: 1` since 2026-09-09, so today it buys nothing.
- **`trial deploy` aborts on the first node that fails, and the rest silently keep the
  old config.** On 2026-09-12 `trial deploy ch2 hk ty-b ty-d` failed at
  `ch2` and never reached the other three; `trial status` then showed them still
  probing the destroyed ty-a/ty-c addresses and `ch2>ty-b` at 100% loss,
  because ty-b had not been given the new leg's key. Deploy node by node, or read
  `trial status` afterwards and believe it over the deploy's exit code.
- **Correlated outage to exclude: 2026-09-12T06:33:45Z-06:44:00Z**, triald restarted on
  all five nodes across several passes. `ch2>ty-b` reads 100% loss for
  06:36-06:43Z for the key reason above, not for a network one.

- **A redeploy is a correlated outage.** Restarting several nodes drops ticks on
  every path through them at once, which is precisely the signature the
  independence table looks for. Record the window and exclude it; do not let a
  deploy get read as evidence that two nodes share fate. Known so far:
  2026-09-08T06:58:26Z–06:59:22Z, ty-b/ty-c/ty-d, adding the ty-d legs.
- **So is a re-address, and it is the worse one.** ty-b moved the node off
  198.51.100.22 at 2026-09-09T11:08:19Z. The five peers kept sending to the old
  address and the node kept answering from the new one, so every leg touching it
  read 100% loss in both directions for 2h20m, until 13:28:46Z. Nothing logged an
  error: each side was doing exactly what its config said. Exclude
  2026-09-09T11:08:19Z–13:28:46Z (ticks 1788952099–1788960526) — read as data it
  says ty-b fails alone and totally, which is the strongest possible claim
  about independence and is entirely an artifact of an edit that had not shipped.
  **A leg is configured on both ends and in a firewall.** An address change is
  three edits per peer, and `trial deploy` does one of them; the other two are
  `ufw allow` from the new address and `ufw delete` of the old. Leaving the old
  rule in place holds a port open to whoever the address belongs to next.
