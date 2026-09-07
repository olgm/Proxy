# Changelog

## Unreleased

- `proxyctl`: `trial report`'s worst column is the slowest single round trip a
  window saw, not the highest p99 it reported. p99 of a sixty-sample window is the
  second-worst of sixty, so one outlier sits above it and never appears — and then
  shows up only as an mdev larger than the p99 printed beside it, which reads as
  arithmetic that cannot be true rather than as the one spike it is. The first live
  report had a row saying p99 1.48 ms and mdev 32.92; the missing number was a
  single 806 ms answer to a probe sent before that leg's firewall was open.

- `proxyctl`: `trial report` now groups a race by the run a copy belongs to, and
  separates the median p99 from the worst one. Running it against the live mesh is
  what found both. It had been timing a copy that started at hk against one that
  started at ch and calling the gap a margin — at ty-a that read as an echo probe
  beating a chain packet by 79 ms, which is not a race and not a number. Two copies
  are only alternatives to each other if they started in the same place. The same
  fix applies to the independence table, where correlating a forward route with a
  return one would be asking whether two different questions failed together.

  And the p99 column held a maximum while p50 held a median, under headings that
  did not say so. The worst window is worth having — it is the one a reader is
  looking for — so it is its own column now rather than quietly displacing the
  typical one.

- `proxyctl`: a `trial` verb for the bake-off mesh — `config`, `deploy`, `status`,
  `pull`, `report`, `uninstall` — reading `trial.json` rather than the topology,
  because the nodes it asks about are the ones no route uses yet and requiring a
  valid topology to ask would be backwards. Keys come from a `trial|` namespace of
  their own: these are the set most likely to end up on a machine we have had for a
  day and are not sure about, and holding them must not be a way into anything else.

  Two things are worked out rather than written down, so they cannot disagree with
  the mesh. Which legs a node holds follows from the legs it appears in. And a leg
  no run crosses is probed on its own — everywhere else a run's own packets are the
  leg's measurement, which is the point of building it this way. The expansion is
  then run through triald's own validation before anything is uploaded, because the
  alternative is learning from six nodes that all failed to start at once.

  `status` reports which legs are carrying and prints the `ufw` rule for any that
  are not, without applying it. `pull` fetches every node's dataset and probed's
  beside it, since the first question is how a candidate compares with the leg it
  would replace and probed is the only thing measuring the latter. `report` prints
  the leg comparison, the race win rates with their margins, and whether two routes
  lose the same ticks — and says plainly when nothing has failed yet, rather than
  reporting an independence it has no evidence for.

- `internal/trial`, `triald`: a bake-off harness for candidate nodes, beside probed
  and never instead of it. probed measures the production path and may not be
  pointed off it; every leg here is one no route uses, because the question is
  which nodes a route should use. So: its own binary, unit, user, port and keys,
  legs written by hand since there is nothing in the routes to derive them from,
  and deletion when it has answered.

  Three things it does that probed must not. Nothing is de-duplicated — probed
  collapses the copies racing into an exit because a player's packet only has to
  arrive once, and here which copy won and by how much *is* the measurement. The
  whole route travels on the wire, the only place in this repo a node identifies
  itself in a datagram, because a copy that cannot say where it has been answers
  nothing; the ids are data and the key still decides which leg a datagram belongs
  to. And every hop echoes what it forwards.

  That last one is what makes the numbers real. A chain that only forwards gives
  arrival times taken on different clocks, so a leg reads as one-way delay plus
  clock offset — and four of the six nodes run undisciplined timesyncd against
  legs whose one-way delay is about a millisecond. The error would be larger than
  the signal and drift would look exactly like the thing being hunted. Echoing
  puts both timestamps of a round trip on one machine. What survives with no clock
  sync at all: per-leg round trips, loss split by direction, jitter, the racing
  margin — competing copies land on the same node and are compared against that
  node's one clock — and cross-leg failure correlation, which needs only the
  one-second alignment `tick` already provides. What does not, and is not claimed:
  one-way latency split by direction.

- `internal/window`: the windowed aggregation and its percentiles, moved out of
  `internal/probe` so that a second measurement service can put its numbers beside
  probed's and have the comparison mean something. Two copies of a percentile drift
  apart the moment one of them is touched, and the whole point of a bake-off is that
  the candidate and the incumbent were measured the same way. `probe.Report` keeps
  its flat shape and its fields are copied out of `window.Closed` rather than
  embedded, so the dataset on disk did not move by one byte — the existing probe
  tests passing unchanged is what says so. The two rules that were hard-won stay
  with the arithmetic: a sample is filed under the time it was sent, and a window is
  not closed until one probe timeout after it ends.

- `proxybot`: `/watch user:@x`, managers only and always private. It shows the
  accounts that member owns, the sessions they have open and on which entry, and a
  page of five finished sessions with a button for the next — each with the IGN,
  uuid, originating IP, how long it lasted and what it carried, payload and wire.
  This is the one place the client IP is reported, which is why it is
  managers-only and ephemeral: one manager reading a private reply is not the
  audience a channel feed is. Ownership is the whitelist tag as everywhere else and
  sessions are then matched by uuid, so the history follows the account rather than
  whoever used to hold it. The page number rides in the button's own id, so the bot
  remembers nothing between one press and the next, and the role is checked again
  on every press.

- `internal/jsonl`: the rotating record file, moved out of `internal/probe` so
  proxyd's session log and probed's dataset are one implementation rather than two.
  It also reads back now: `Tail` returns the newest records matching a filter,
  across the current file and the rotated one, holding only what it keeps — the
  right way round for a file bounded at tens of megabytes and read a few times a
  day. A line truncated by a crash mid-write is skipped rather than failing the
  read, because the records before it are still good.
- `proxyd`: an ingress writes down the sessions it finishes, at
  `/var/lib/proxyd/sessions.jsonl`, and a `history` op on the control link reads
  the newest of them back by uuid. It holds what the journal line already holds —
  same data, same machine, same operator — and is bounded by size, keeping one
  previous file, so retention is a size and not a time. No owner is recorded: a
  line's tag is the ownership model and it lives in the whitelist, so a caller
  joins the two by uuid rather than trusting a second copy taken at login.

- `proxybot`: a status feed. Every 20 seconds it dials each entry's control link
  and each node's probed health link, and posts a transition that has held for
  three dials — a single dropped packet is not an outage, and a fault already
  there when the bot starts is announced at once because it is still news. A
  refused connection is the kernel saying the host is there and nothing is on that
  port, which is a service being down; silence is the node being gone. Those never
  read the same, and a node that did not answer at all is not then asked about
  probed, so one fault is one line.
- `probed`: a health link, deployed only when `feeds.status` asks for it. A small
  sealed TCP port answering "running, with N classes", under a key of probed's own
  — holding it is not a way into a session, which is why it is not the node's
  control key. proxyd still does not know probed exists, and vice versa: the bot
  dials both and joins the answers, because it is the only thing that can.

- `internal/sealed`: the sealed request/reply exchange — a challenge, then one AEAD
  frame each way — moved out of `internal/control` unchanged, so probed's health
  link can put the same bytes on the wire rather than an approximation of them. No
  behaviour change; the same reasoning as exporting the tunnel's sealer for probed.

- `proxybot`: an online roster — one message kept up to date in place, grouped by
  the entry each player arrived at and in the order `feeds.online.nodes` asks for,
  with anything unnamed falling to the end so a new entry appears rather than
  disappearing. An entry that could not be reached reads `unreachable` rather than
  as empty, and is never counted as nobody. The message is edited only when the
  roster changed, so the timestamp says *changed* rather than *checked*: a roster
  that has not moved is still a correct one. The bot now keeps one thing between
  restarts, the id of that message, in `/var/lib/proxybot/feeds.json`; it is not a
  source of truth for anything and losing it costs one duplicate message.

- `proxyd`: a register of sessions in progress, and a `sessions` op on the control
  link that reports it. It belongs to the node rather than to a listener, because a
  node may hold several ingresses and "who is online here" is one answer across all
  of them, and it lasts exactly as long as the goroutine relaying the session — a
  logout leaves nothing behind. There are no byte counts in it: what a session cost
  is known when it ends. A node with no ingress refuses the op rather than
  answering an empty list, because "nobody is online here" and "I cannot tell you"
  are different answers and a roster that merged them would quietly lose a node.

- `probed`: a probe feed. When a window closes, the node that measured it posts one
  line per class — the same leg at one copy and at the count it really carries, in
  the same window, which is the comparison the whole service exists to make. The
  direction split is absent rather than zero on the first window of a series, for
  the same reason it is null in the dataset. `feeds.probe.windows` picks which
  windows reach the channel and changes nothing about what is measured or logged;
  the default is the longest configured, the only one whose p99 means anything.
  A node that only answers originates nothing, so Chicago is never given the URL.
  This widens what probed talks to — its peers, and now a webhook — and nothing
  about the rule against touching the backend changes.

- `proxyd`: a session feed. One line to a Discord webhook when a player logs in and
  one when they log out, carrying the IGN, the UUID, the entry they arrived at, and
  what the session cost — payload each way, and what the tunnel spent carrying it.
  The count beside it is that node's own: a node knows its own sessions and no
  others, and a login is not worth a keyed link between nodes to change that. The
  client IP is deliberately absent, because the journal already has it for anyone
  holding the node and a channel is a wider audience than that. Posting happens off
  the relay path, so a feed can never slow a session down or fail one.

- `internal/webhook`: closing a queue no longer races a send. A node shuts its
  feed down while sessions are still ending, so Close races Send by construction;
  signalling that by closing the channel turned the race into a send on a closed
  channel, which would have taken proxyd down with it. A feed must never do that
  to the thing it reports on. Close now shuts the door under a lock and drains
  what is left, so a logout posted a moment before shutdown still gets out.

- `proxyctl`: a `feeds` block in `topology.json` turns on Discord feeds, and every
  one of them is off until it is named there. A feed names an *environment
  variable* holding its webhook URL rather than the URL, because a webhook URL is a
  bearer credential and topology.json is the file people edit and paste at each
  other; two feeds naming the same variable land in the same channel. The URL is
  read from the operator's environment at deploy time and carried to the node
  inside the install script over ssh stdin, into a root-owned env file the unit
  reads — the path the bot token already takes, so it is never on a command line
  and never in /tmp. Deleting a feed removes that file, so turning one off turns it
  off. `config` lists which feeds are on and which nodes post them, and never
  prints a URL.
- `proxyctl`: who posts a feed is not a setting, because it follows from who can
  see it. proxyd posts sessions, from every ingress and about its own node only;
  probed posts measurements, from the nodes that originate a class and not from the
  one that merely answers; the bot posts the roster and the up/down watch, because
  only it can see the whole fleet, or see a node that has stopped answering at all.

- `internal/webhook`: one Discord webhook client, shared by everything that posts
  a feed. stdlib only, because proxyd and probed are dependency-free binaries on a
  node and both post their own; proxybot has discordgo already and uses this anyway,
  so a feed reads the same wherever it was posted from. A queue coalesces a flush
  into one message, which is what keeps a reconnect storm inside Discord's rate
  limit and what makes the channel readable, and drops rather than blocking when it
  fills — a feed that stalls a login is worse than a feed with a hole in it, and the
  hole says how big it was. Mentions are never parsed, 429s are waited out, and a
  webhook URL is a bearer credential so it never reaches an error or a log line.

- `probed`: a measurement service that answers the question `duplicate` exists for.
  Every leg a production path really uses is probed twice — once at a single copy
  and once at the count that leg carries — and the difference between the two is
  what duplication buys, on that leg, in the same window. Every route longer than
  one leg is probed the same way end to end, where the copies are also raced across
  every path into the exit. What is measured follows from the routes and is not
  configurable: a pair of nodes no route puts traffic between is never probed, so
  Sydney never polls Hong Kong. It is a separate binary, unit, user and set of
  keys, deployed by a `probe` block in `topology.json` and absent without one, so
  it can be stopped without touching a live session and holding its keys is not a
  way into one. Nothing it sends reaches the backend: a chain probe is turned
  around at the exit exactly as the tunnel's ECHO is.
- `internal/probe`: the probe protocol. The tunnel's framing byte for byte, sealed
  under a key of the leg's own, because a probe of a different size or shape
  measures a different path. Every node drops what it receives to one copy and
  sends on with its own leg's count, so the counts never compound along a chain,
  and the count is the hop's rather than the class's — two paths into one exit may
  carry different numbers and a node racing both honours each.
- `internal/probe`: round-trip loss is split into the direction that dropped it,
  without sending anything extra. An answer carries the responder's own count of
  distinct probes it has accepted, and differencing that across a window says how
  many arrived; the rest of the loss was on the way back. The first window of a
  series reports null rather than zero, because that count is the difference of two
  counters and the earlier one comes from the window before.
- `internal/probe`: the dataset is JSONL at `/var/lib/probed/probe.jsonl`, one
  object per class per window, at every configured window length — a short one to
  watch an incident happen and a long one whose p99 has enough samples behind it to
  mean anything. A sample is filed under the time it was sent rather than the time
  it came back, and a window is flushed one probe timeout after it closes, so a
  probe still in flight at the boundary is counted where it was sent instead of
  written off. `n` is in every line, because p99 of a sixty-sample window is the
  second-worst of sixty. The file rotates at `max_log_mb` keeping one previous
  file, so it is bounded however long a node runs.
- `proxyctl`: `deploy` installs `probed` after every `proxyd`, mints a key per
  probe leg into `tunnel-keys.json`, allocates its ports after the hops and the
  control links, and verifies each one the way it verifies a hop. `config` prints
  the class map, `status` the newest line per class, `uninstall` removes the
  service and leaves the dataset.
- `internal/tunnel`: the link sealer is exported, so probed can put the tunnel's
  own framing on the wire rather than an approximation of it. No behaviour change.

- `proxyctl`: a route may name its entry as its own exit. That is a chain of one
  node: the ingress parses the handshake and dials the target itself, with no hop
  to allocate a port for and no leg for a transport to choose between. It is what
  lets the node holding the egress address also be an entry, so a player near it
  can skip the chain entirely and still get the whitelist and the server listing.

- `proxyd`: the ingress answers a server-list ping itself and never forwards one.
  Every client refreshing its multiplayer screen, and every scanner that finds
  25565 open on a public entry, was previously a status request arriving at the
  backend from the egress address — the one thing here that cannot be replaced, and
  the one packet a stranger can make us send without an account. The listing is
  `routes[].motd`, a JSON document, or a built-in one carrying the v1 branding.
  `version.protocol` is echoed from the client's handshake, without which the client
  shows an incompatible badge, and `players.online` is the live count of relayed
  logins. The listing now also survives the backend being down.
- `proxyd`: the ping a player reads off the server list covers the chain, not just
  the entry. Nothing in the status exchange carries a latency — the client times the
  pong — so the ingress holds its answer for as long as the rest of the chain takes.
  The entry learns that from a new tunnel message that walks the hops and is turned
  around by the node with none left, which is our last one before the backend: a
  measurement to Hypixel's doorstep that Hypixel never sees. Capped at 2 s so a dead
  chain looks bad rather than hangs; zero on a TCP route, which measures nothing.
- `internal/tunnel`: an ECHO message that times the whole chain instead of one leg
  of it. A node passes it along while it has hops, and the node with none left —
  the exit — turns it around, so it measures as far as our own infrastructure goes
  and no further. It carries no stream and opens none, which is the point: the exit
  dials the backend when a stream opens, so measuring with real traffic would mean a
  connection to Hypixel per measurement. Only the entry originates one, on the link
  ping interval; the first reply for a nonce wins and `Node.ChainRTT` smooths it the
  way a leg smooths its own round trip.
- `proxyd`: a session logs one line when it opens and one when it closes, with the
  source address, the name and uuid claimed at login, how long it lasted, and the
  payload each way. Over a tunnel it also logs what the legs actually spent carrying
  it, duplicates and re-sends included, and the multiple that is over payload — the
  only view of what a `duplicate` setting is buying.
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
- `proxyctl`: `routes[].motd` uploads a listing to the entry, replaced on every
  deploy — unlike the whitelist, nothing on the node writes it.
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
- Measured all three ty-c Japan regions from the live `hk` and `ch` nodes, 20 ICMP
  echoes each: chain totals 180.6 ms (Tokyo 3), 181.0 ms (Tokyo 2) and 188.7 ms
  (Osaka) against 165.6 ms for the `ty` node in place, so no ty-c node is worth
  adding to these routes and Osaka is slower on both legs rather than trading one
  for the other. ty-c has no Taiwan or Hong Kong region.
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
