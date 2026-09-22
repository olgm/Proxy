# Trial

A bake-off harness for nodes we are thinking about, in `trial.json` and deployed by
`proxyctl trial`. **Temporary — delete it, and `cmd/triald`, once it has answered.**

It exists because probed may not be pointed off the production path, which is the
right rule and also means there is no way to ask whether a node we do not run yet
would be better. Every leg here is one no route uses. So it is a separate service
in every way probed is separate from proxyd: own binary, unit, user, port and keys,
and it can be stopped without touching anything that carries a player.

```sh
cp trial.example.json trial.json     # edit: nodes, legs, and the runs over them
go run ./cmd/proxyctl trial config   # preview, changes nothing
go run ./cmd/proxyctl trial deploy
go run ./cmd/proxyctl trial status   # which legs are carrying, and the ufw rule if not
go run ./cmd/proxyctl trial pull     # fetch every dataset, and probed's beside it
go run ./cmd/proxyctl trial report
```

A `run` is a directed flood named for the node it starts at. Every node passes an
arrival on down each of its onward edges, never to a node already in the packet's
route, so seven edges enumerate five distinct paths from one end to the other and
the flood terminates without anybody counting hops. `reverse_of` turns another
run's edges around, which is how the return direction is described without
restating them and getting one wrong.

Nothing is de-duplicated. probed collapses the copies racing into an exit, because
a player's packet only has to arrive once; here every copy is logged with the route
it took, because which route won and by how much is the measurement.

**Every hop echoes what it forwards, and that is what makes the numbers real.** A
chain that only forwards gives arrival times taken on different clocks, so a leg
reads as one-way delay plus clock offset — and against legs whose one-way delay is
about a millisecond, that offset is larger than the thing being measured. Echoing
puts both timestamps of a round trip on one machine. So does the racing margin:
competing copies land on the *same* node and are compared against that node's *one*
clock. One-way latency split by direction is the thing this cannot measure, and it
is not reported rather than reported wrongly.

`expect_ms` on a leg drives one thing only: a closed window landing more than
`over_ms` above it gets a traceroute, at most one per leg per `cooldown_s`. Set it
from a measurement. A value below what the leg really costs traces every window
forever.

The dataset is JSONL at `/var/lib/triald/trial.jsonl` on **every** node, not only
the originators, since an arrival is a measurement in its own right:

```json
{"t":"2026-09-07T14:31:00.104217Z","k":"rx","leg":"ty-c>ch","tick":1788783629,
 "path":"hk>ty-a>ty-c>ch","lseq":9912,"onward":0}
{"t":"2026-09-07T14:31:00Z","k":"win","w":"1m","leg":"hk>ty-a","n":60,"sent":60,
 "got":60,"fwd":60,"loss":0,"loss_fwd":0,"loss_rev":0,"min":44.6,"max":45.9,
 "mean":44.9,"p50":44.9,"p90":45.1,"p99":45.6,"mdev":0.22,"expect":45}
```

The `win` fields are spelled exactly as probed spells them, and both are computed
by `internal/window`, which is the only reason `trial report` can print a candidate
leg and the leg it would replace in one table. Sample counts per leg are **not**
uniform — a leg's samples come from the runs crossing it, so `hk>ty-a` gets one a
second and `ty-c>ch` gets three. Read `n` before reading a p99.

`trial report` answers the two questions: how each candidate leg compares with the
incumbent, which route wins a race and by what margin, and whether two routes lose
the same ticks. That last one is what decides whether a second node earns its keep:
two routes a millisecond apart on a calm day are worth nothing as a race if they
fail together. When nothing has failed yet it says so, rather than reporting
independence it has no evidence for.

The data plane is public IPv4, never the Tailscale addresses — production runs on
public IPv4, and the overlay would hide the carrier diversity the trial is about.
The unit needs `AmbientCapabilities=CAP_NET_RAW`: `NoNewPrivileges` disables
`mtr-packet`'s file capability, and without it every traceroute fails.
