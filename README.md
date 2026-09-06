# proxy

Minecraft TCP accelerator. Chains relay nodes between player and server so a lost
packet is replaced by the node before it rather than by the far end.

Legs run over plain TCP or over a UDP tunnel, per route. The tunnel can send every
packet more than once, and down more than one path at a time.

Targets Hypixel. Deployed today on four nodes — Hong Kong, Tokyo, Sydney and
Chicago — every one of them an ingress, all four converging on the Chicago exit.
HK goes through Tokyo; Tokyo and Sydney go straight to Chicago; Chicago dials the
backend itself. Every hop leg is UDP with every packet doubled.

## Design

One binary (`proxyd`) on every node. A node has no role field: it is a list of
listeners, each with one next leg. A listener with a `minecraft` block is an ingress;
one that dials the target is an exit; anything else passes bytes along. A second
ingress is one more listener, not a code change.

The ingress parses exactly one packet — the handshake — rewrites the address to what
Hypixel expects, then relays raw bytes: `splice(2)` on a TCP route, numbered chunks
on a UDP one. It cannot do more: the client encrypts from Encryption Response
onward.

A server-list ping is the exception: the ingress answers it itself and never opens
the chain for one. See "Server list".

## Deploy

Needs Go and ssh locally; root or passwordless sudo on each node.

```sh
cp topology.example.json topology.json     # edit; delete the discord block to run without the bot
cp whitelist.example.txt whitelist.txt     # edit, or drop `whitelist` from the route
cp motd.example.json motd.json             # optional; there is a built-in listing
go run ./cmd/proxyctl config               # preview, changes nothing
go run ./cmd/proxyctl deploy
go run ./cmd/proxyctl status
go run ./cmd/proxyctl whitelist list       # once deployed: manage the list from here
```

Commands: `config`, `deploy`, `status`, `uninstall`, `whitelist`. The Discord bot
needs one more thing before `deploy`; see "Discord bot" below.

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
| `routes[].exit` | node every path converges on. Implied by the end of `via`; naming the entry itself is a route with no hops, where that ingress dials the target |
| `routes[].paths` | UDP only; race several ways to the exit instead of one `via` |
| `routes[].tunnel` | UDP only; sizes, deadlines and every timer — see below |
| `routes[].target` | final `addr`, plus `rewrite_host` / `rewrite_port` |
| `routes[].whitelist` | optional; local `ign:uuid` file seeded onto the entry node |
| `routes[].motd` | optional; local JSON file the entry answers server-list pings with |
| `discord` | optional; deploys the bot — see below |
| `discord.node` | node the bot runs on. Default: the first entry with a whitelist |
| `discord.guild` | the server's id |
| `discord.roles.<id>` | what a role grants: `{"accounts": N}` or `{"manage": true}` |
| `discord.audit_channel` | optional; channel that gets one line per change |
| `feeds` | optional; posts what the chain is doing to Discord — see below |
| `feeds.<name>.webhook_env` | environment variable holding that feed's webhook URL |
| `feeds.probe.windows` | which probe windows reach the channel. Default: the longest |
| `feeds.online.nodes` | order entries appear in the roster |

Hop ports are allocated automatically, and so is one control port per entry with a
whitelist. `config` prints the map.

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

## Server list

The ingress answers a server-list ping itself. It never opens the chain for one and
never forwards it, so no amount of clients refreshing their multiplayer screen —
and no scanner that finds port 25565 open — turns into traffic at Hypixel from the
egress address. That address is the hardest part of this system to replace, and a
status ping is the one packet a stranger can make us send without an account.

Answering locally also means the listing still works when the chain or the backend
is down, which is when a player most wants to look at it.

Two fields cannot come from the file:

- `version.protocol` is echoed from the client's handshake. Reporting our own would
  put the red "incompatible" badge on the listing for every client that is not
  exactly that version.
- `players.online` is the number of logins the ingress is relaying right now.

Everything else is whatever `routes[].motd` points at, or a built-in listing if the
route names none. `motd.example.json` has the shape; `favicon` is a base64 data URI
of a 64x64 PNG.

### The ping a player sees

Nothing in the status exchange carries a latency: the client times the pong itself.
Answering at once would show the distance to the ingress, which is the near end of a
chain the player's traffic has to cross all of — a flattering number, and a useless
one.

So the ingress holds the pong for as long as the rest of the chain takes. It knows
that figure because the entry times the whole tunnel: an echo goes down the hops,
each node passes it along, and the node with no hops left — the exit — turns it
around. That is our last node before the backend, so what a player reads off the
server list is the trip to Hypixel's doorstep, and none of it is a packet Hypixel
sees. The measurement repeats on the link ping interval and is smoothed the same way
a leg's own round trip is.

On a plain TCP route there is nothing measuring itself, so the pong goes out at once.

## Whitelist

Optional, and only on the entry node: it is the only hop that sees a Login Start.
One player per line.

```
# friends
Notch:069a79f4-44e9-4726-a5be-fca90e38aaf5
```

Point `routes[].whitelist` at the file and `deploy` seeds it to
`/var/lib/proxyd/whitelist.txt`, **once**. After that the node owns it: proxyd
rewrites the IGN column when a player renames, so later deploys leave it alone. Add
and remove people with `proxyctl whitelist` from your machine, through the Discord
bot, or by editing the file on the node; a change is picked up on the next login,
without a restart that would drop everyone mid-session. A file that fails to parse
leaves the last good list in place.

Anything after a `#` on a player line is that line's tag, kept through renames. The
bot writes the Discord id of whoever added the player there, and that tag is the
whole ownership model: a member may remove only lines tagged with their id.

```
Notch:069a79f4-44e9-4726-a5be-fca90e38aaf5 # discord:123456789012345678
Friend:5bc1b7b1-1c8b-4f8e-9c2e-7f2a0d3e4b5c # cli
```

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

## Control link

Every entry that holds a whitelist also runs a small TCP listener through which the
list is managed from outside proxyd: by `proxyctl` over ssh, and by the Discord bot
from whichever node it lives on. `config` shows it as the `control` line. It takes
the port after the hops, and a key of its own, minted into `tunnel-keys.json` as
`ctl|<node>` beside the leg keys.

One request per connection. The node sends a random challenge, the client answers
with one frame sealed under the key with that challenge as associated data, and the
reply is sealed the same way, so a recorded exchange replays into nothing. A frame
that does not open gets no reply at all. Loopback may always connect; the bot's node
may once a `discord` block names it, and that is a TCP address, so trusting it is
sound in a way it would not be over UDP. proxyd stays the only writer of its file.

Three operations: `list`, `add`, `remove`. An add given only a name has the UUID
resolved against Mojang on the node, and the name written the way Mojang spells it;
given only a UUID, the name. A UUID already listed is refused, naming the line it
has, so nobody re-tags someone else's. On the node itself:

```sh
proxyd ctl list
proxyd ctl add Notch                # resolved against Mojang
proxyd ctl add Notch <uuid> cli     # both halves given: no lookup
proxyd ctl remove notch
```

From your machine, `proxyctl whitelist list | add <name> [uuid] | remove <name|uuid>`
runs that on **every** entry that holds a whitelist. Every such entry carries the
same list: a player is whitelisted on the chain, not on a node. `list` prints the
entries side by side and marks any line that is not on all of them; an add or remove
that one entry could not take is reported, and the fix is to run it again, or to let
the bot's reconcile settle it.

### Several entries

Two writes can never be atomic, so one entry is the **primary**: the bot's node when
that is a whitelisted entry, otherwise the first whitelisted entry in route order.
An operation goes to the primary first and stops if the primary refuses. Every five
minutes the bot lists every entry and makes each other one's set of UUID and tag
match the primary's. Names are never compared, because each node keeps its own:
renamed on login, refreshed daily, and free to differ for a day. So: hand-edit the
primary, or use the CLI or the bot. A hand edit on another entry is undone at the
next reconcile. Without a bot nothing reconciles, and the CLI's report is what tells
you an entry is behind.

## Discord bot

`proxybot` lets members of one Discord server manage their own whitelist entries,
within what their roles allow. It runs on one node — `discord.node`, by default the
primary — and reaches every entry over its control link, so it keeps no state of its
own: the whitelist files are the truth, and the tags in them say whose line is whose.

Setting it up, once:

1. [discord.com/developers](https://discord.com/developers/applications): New
   Application → Bot → Reset Token, and keep the token. Under OAuth2 → URL Generator
   pick the scopes `bot` and `applications.commands`, no permissions, open the URL
   and invite it. The bot needs no privileged intent and no channel permission.
2. In Discord, Settings → Advanced → Developer Mode. Right-click the server, each
   role that should count, and the audit channel if you want one, and Copy ID.
3. Fill the `discord` block in `topology.json` with those ids. A role grants either
   an account cap or manage; a member with several roles gets the highest cap, and
   manage if any of them says so. Members with none of the listed roles cannot use
   the bot.
4. Put the token in `.env` as `DISCORD_BOT_TOKEN=...`, then `set -a; . ./.env; set +a`
   and `proxyctl deploy`. The token travels inside the install script over ssh into
   `/etc/proxyd/bot.env`, root-only, read by the unit; it is never on a command line
   and never in `/tmp`. A later deploy without it in the environment keeps the file.

Commands, all replying privately to whoever ran them:

| command | who | does |
| --- | --- | --- |
| `/whitelist add <ign>` | any listed role, up to its cap | lists the account under your id, on every entry |
| `/whitelist add <ign> user:@x` | managers | lists it under that member's id |
| `/whitelist remove <player>` | the line's owner; managers, any line | drops it everywhere |
| `/whitelist list` | anyone: own lines. managers: every line, with owners | |
| `/whitelist list user:@x` | managers | that member's lines |
| `/whitelist purge user:@x` | managers | drops every line that member owns |

A cap counts the lines tagged with the member's id. Managers have no cap. A name
nobody holds, a name Mojang could not be asked about, and an account someone else
already listed are each told apart in the reply.

The whitelist follows the role. Every five minutes the bot asks Discord about each
member who owns a line — one Get Guild Member call each, which needs no privileged
intent — and drops the lines of anyone who left the server or no longer holds any
listed role. A member Discord could not be asked about keeps their lines until it
can. A cap that was lowered only blocks new adds. Every change the bot makes is in
the node's journal, and in the audit channel when one is set.

`proxyctl status` reports the bot beside the nodes. `proxyctl uninstall` removes it,
including the token file.

What the bot does not change: the whitelist is still a door lock and not
authentication, the entry address is still something to hand out narrowly until the
ingress rate limit exists, and the bot never prints that address. Anyone with Manage
Roles in the server can hand out a listed role, which is Discord's trust model, not
ours.

## Feeds

A `feeds` block posts what the chain is doing to Discord webhooks. Every feed is
off until it is named there, and the chain is unchanged without one.

```json
"feeds": {
  "sessions": {"webhook_env": "DISCORD_SESSIONS_WEBHOOK"}
}
```

A feed names an **environment variable**, not a URL. A webhook URL is a bearer
credential — anyone holding it can post into that channel — and `topology.json`
is the file you edit and paste at people. Put the URL in `.env` beside the bot
token, `set -a; . ./.env; set +a`, and deploy. It travels inside the install
script over ssh stdin into a root-owned env file the unit reads, so it is never
on a command line and never in `/tmp`. Two feeds naming the same variable land in
the same channel; that is how you keep one channel or four.

`proxyctl config` lists which feeds are on and which nodes post them, and never
prints a URL. Deleting a feed removes the file on the next deploy, so turning one
off turns it off.

Who posts a feed is not a setting, because it follows from who can see it:

| feed | posted by | on |
| --- | --- | --- |
| `sessions` | `proxyd` | every ingress, about its own node only |
| `probe` | `probed` | every node that originates a class |
| `online` | `proxybot` | one message, edited in place, across every entry |
| `status` | `proxybot` | node and service transitions, watched from outside |

### sessions

One line when a player logs in and one when they log out:

```
**hk** `Notch` joined `069a79f4-44e9-4726-a5be-fca90e38aaf5` · online 2
**hk** `Notch` left · 42m18s · up 4.1MB down 51.7MB wire 111.6MB ×2.00 · online 1
```

`online` is that node's own count, not the fleet's. A node knows its own sessions
and no others — nothing crosses between nodes but keyed links, and a login is not
worth one.

`up` and `down` are payload; `wire` is what carrying it cost on the tunnel's legs,
counting every duplicate and re-send, with the multiple beside it. It is absent on
a TCP route, which has no copies to count. `name` and `uuid` come from Login Start,
so they are the client's own word — see "What it does not do".

**The client IP is deliberately not here.**  It is in the node's journal, where an
operator who has the node already has it. A channel is a wider audience than that,
and a login feed already tells its readers which entries are live, so point it at
a channel you would not hand the entry addresses to.

A burst of reconnects arrives as one message rather than fifty: lines are gathered
for a couple of seconds and posted together. If more arrive than the queue holds,
the feed says how many it dropped rather than blocking the login — a feed that
stalls a session is worse than a feed with a hole in it.

### probe

One line per class when a window closes, from the node that measured it:

```
**hk** `hk>ty` leg ×1 · 10m · p50 44.1ms p90 44.4ms p99 45.9ms mdev 0.31 · loss 0.00% (out 0.00 back 0.00) · n 600
**hk** `hk>ty` leg ×2 · 10m · p50 44.0ms p90 44.2ms p99 44.8ms mdev 0.28 · loss 0.00% (out 0.00 back 0.00) · n 600
```

Those two lines are the measurement the tool exists to make: the same leg at one
copy and at the count it really carries, in the same window. `out` and `back`
split the loss into the direction that dropped it, and are absent rather than
zero on the first window of a series — the count that arrived is the difference
of two counters, and the earlier one comes from the window before.

`windows` picks which of the `probe` windows reach the channel; it changes
nothing about what is measured or logged, and the dataset keeps every window
either way. The default is the longest one configured, which is the only one
whose p99 has enough samples behind it to mean anything. Without a filter the
four classes Hong Kong originates would post eight messages a minute.

Only a node that originates a class posts. Chicago answers everything and
measures nothing, so it is never given the URL.

This is the one change that widens what `probed` talks to. It reaches its own
peers, and now a webhook. It still never touches the backend, and the hard rule
in `agents/operational-safety.md` is unchanged.

### online

One message the bot keeps up to date, grouped by the entry each player arrived
at:

```
**Online — 4**

**au** `jeb_` 1h30m
**hk** `Notch` 12m · `Herobrine` 3m
**ty** `Dinnerbone` 22m

_changed <t:1757087460:R>_
```

`nodes` sets the order the entries appear in; anything not named falls to the end,
so an entry added later shows up rather than disappearing. An entry with nobody on
takes no line, and one that could not be reached says `unreachable` — that is not
the same fact as nobody being online there, and the count in the header never
includes it.

The message is edited rather than reposted, and only when the roster actually
changed. That is why the timestamp says *changed* and not *checked*: a roster that
has not moved in an hour is still a correct one, and Discord counts the relative
timestamp up on its own without anything being edited.

Only the bot can build this. A node knows its own sessions and no others, so it
asks all four over their control links and joins the answers.

To do that it keeps one thing between restarts — the id of the message — in
`/var/lib/proxybot/feeds.json`. It is not a source of truth for anything: losing
it costs one duplicate message, and the whitelist files remain the only state that
matters.

### status

Node and service transitions. The bot posts these because a node that is down
cannot report that it is down:

```
**ty** proxyd is down (the node answers, its control port does not)
**hk** unreachable — no answer from the node at all
**ty** recovered
```

Every 20 seconds the bot dials each entry's control link and each node's `probed`
health link. A **refused** connection is the kernel saying the host is there and
nothing is on that port, which is a service being down; **silence** is the node
itself being gone. Those are different faults and never read the same. A node that
did not answer at all is not then asked about `probed`, so one fault is one line.

A change has to hold for three dials — about a minute — before anything is
posted, so a single dropped packet is not an outage. A fault that is already there
when the bot starts is announced immediately, because a node that was down before
the bot came up is still news.

Turning this feed on gives `probed` one more thing: a small TCP port per node,
sealed under a key of its own, answering nothing but "I am running, with N
classes". `deploy` allocates it, opens it to the bot's node only, and verifies it
like any other link. Without `feeds.status` there is no port, because nothing
would ever dial it. The key is `probed`'s and deliberately not the node's control
key: holding it is not a way into a session.

## Firewall

After deploy, proxyctl tests every link from the side that will really dial it and
reports which are blocked. When a firewall is the cause it prints the exact rule and
asks before applying it; with no terminal attached it only prints. It never edits a
firewall unprompted.

The rules are one public port on the entry node, and each hop port opened to the
previous hop only:

```sh
ufw allow <entry-port>/tcp                                # entry node
ufw allow from <prev-hop-ip> to any port <hop> proto tcp  # every other node
ufw allow from <prev-hop-ip> to any port <hop> proto udp  # ... on a udp route
ufw allow from <bot-node-ip> to any port <ctl> proto tcp  # every entry the bot does not live on
ufw allow from <bot-node-ip> to any port <health> proto tcp  # probed, with a status feed
```

The entry needs nothing inbound for a UDP route: it dials out and answers come back
on the same socket. A relay or exit needs one UDP rule per path that reaches it.

A blocked UDP port cannot be found by connecting to it — that always succeeds — so
`deploy` asks the near node whether its link has been answered, and waits a few
seconds before believing it hasn't.

## DNS

`routes[].port` is whatever was free on that node, not whatever is memorable — every
entry here is on 30001, because v1 still holds 25565 on two of them. A player should
not have to know that, and does not have to: a Java client given an address with no
`:port` first looks up `_minecraft._tcp.<name>` and takes the host and port out of
the SRV record it finds. So publish one, and the address a player types is a bare
hostname.

```
; one A record per node, the address the SRV points at
hk.example.com.                    300 IN A   198.51.100.10
ty.example.com.                    300 IN A   198.51.100.20
au.example.com.                    300 IN A   198.51.100.40
ch.example.com.                    300 IN A   198.51.100.30

; one SRV per name a player may type: priority, weight, port, target
_minecraft._tcp.hk.example.com.    300 IN SRV 0 5 30001 hk.example.com.
_minecraft._tcp.ty.example.com.    300 IN SRV 0 5 30001 ty.example.com.
_minecraft._tcp.au.example.com.    300 IN SRV 0 5 30001 au.example.com.
_minecraft._tcp.ch.example.com.    300 IN SRV 0 5 30001 ch.example.com.
```

`hk.example.com` in the server list then reaches `198.51.100.10:30001`. The name and
the SRV target may be the same label, as above, or the target may be a separate
`nodes.hk.example.com` — the SRV is what carries the port either way.

Four things that bite:

- **The target must be a hostname with an A or AAAA record.** Not an IP literal, and
  per RFC 2782 not a CNAME. Resolvers vary in how forgiving they are; do not rely on
  it.
- **An explicit port skips the lookup.** A player who types `hk.example.com:30001`
  gets no SRV query at all, which is the fallback if a record is wrong, and the
  reason a stale SRV can look like it works for the person who tested it.
- **Behind Cloudflare, the A record must be DNS-only** — grey cloud, not orange. The
  HTTP proxy does not carry a Minecraft TCP session, and an orange-clouded record
  hands out Cloudflare's addresses instead of the node's. SRV records are never
  proxied.
- **Bedrock clients do not do SRV.** They need the port typed into the field the app
  gives them for it. This is a Java accelerator, so that is academic here.

What the player types travels to us in the handshake, and the ingress rewrites it to
`target.rewrite_host` before anything is forwarded — so the name you publish is only
ever seen by us, and does not have to be one Hypixel would recognise. It is also the
name the whole chain is reached by: an entry is a whole path, so `au.example.com` is
Sydney's route to the exit and `ch.example.com` is the exit alone.

One SRV per entry, each naming its own node, is the shape to prefer. Pointing several
targets at one name and leaning on priority and weight to fail over is not something
Minecraft clients agree about, and a client that picks a node that is down does not
retry the next one.

## Verify

`proxyd` logs one line the moment a tunnel link starts answering, and again if one
stops, plus a line per link every 30 s with round-trip time, jitter, ping loss and
retransmit counts. That log is the only view from outside a node of whether a leg is
carrying UDP at all, and at what cost. `proxyctl status` prints the most recent line
per link beside the service state.

```
:9000: link 203.0.113.30:9001 up rtt=123.1ms mdev=0.4ms loss=0.0% sent=812 recv=790 rtx=0 dropped=0
```

`tools/mcping` sends a status ping and prints the MOTD:

```sh
go run ./tools/mcping <entry-ip>:25565
```

It answers from the ingress now, so what it proves has changed: that the entry is up,
that the listing renders, and — from the round trip it reports — what the chain
currently costs, since the ingress holds the pong for exactly that. It no longer says
anything about the hostname rewrite, because nothing it sends reaches the backend. A
real login is the only thing that exercises the rewrite.

Pointing it at the entry is safe for the same reason; pointing it at the backend is
still forbidden from anywhere, and from a node most of all.

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

## Probe

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

### What it measures

The routes decide, not the config. Every leg a production path really uses is
probed twice — once at a single copy, and once at the count that leg actually
carries — and the difference between the two is what duplication buys, measured on
that leg, in the same window. Every route longer than one leg is probed the same
way end to end, where the copies are also raced across every path into the exit.

A pair of nodes no route puts traffic between is never probed. Sydney and Hong Kong
are both ingresses and never talk to each other, so that pair is not a leg and does
not become one.

Two things fall out of that and are not oversights:

- **A leg already carrying one copy is measured once.** The baseline and the
  production class would be the same measurement under two names.
- **A route of exactly one leg gets no end-to-end class.** It would be its leg
  class again. Today only Hong Kong has a chain; Tokyo and Sydney are one leg each.

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
the tunnel's ECHO does, and for the same reason: one hop further is Hypixel.

### The dataset

JSONL at `/var/lib/probed/probe.jsonl`, one object per class per window. Only
originators write one — Chicago answers everything and logs nothing.

```json
{"t":"2026-09-05T14:31:00Z","w":"1m","class":"hk>ty","kind":"leg","dup":2,
 "n":60,"sent":60,"got":60,"fwd":60,"loss":0,"loss_fwd":0,"loss_rev":0,
 "min":43.8,"max":45.9,"mean":44.1,"p50":44.02,"p90":44.31,"p99":45.1,"mdev":0.31}
```

`loss_fwd` and `loss_rev` split the round trip into the direction that actually
dropped. Nothing extra is sent to get them: the answer carries the responder's own
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

### What it costs

A request is 66 bytes on the wire and an answer 82, IP and UDP headers included.
At the default 1 Hz on the deployed four-node topology:

| leg | datagrams/s | per day | per 30 days |
| --- | --- | --- | --- |
| HK→TY | 12 | 77 MB | 2.3 GB |
| TY→CH | 12 | 77 MB | 2.3 GB |
| AU→CH | 6 | 38 MB | 1.2 GB |
| **all** | **30** | **192 MB** | **5.8 GB** |

That is what crosses the wire; each end bills what it sends and what it receives,
so budget roughly double across the four nodes. For scale, one Minecraft session is
tens of KB/s, so the whole measurement is a few percent of a single player.

**28,800 probes an hour** fleet-wide: 14,400 from Hong Kong, which originates four
classes, and 7,200 each from Tokyo and Sydney.

The log is far smaller, because a window is one line however many probes went into
it. A line is about 210 bytes, and a class writes 66 a hour (60 short windows and
6 long ones):

| node | classes | per day | per 30 days |
| --- | --- | --- | --- |
| HK | 4 | 1.3 MB | 40 MB |
| TY | 2 | 0.7 MB | 20 MB |
| AU | 2 | 0.7 MB | 20 MB |
| CH | 0 — answers only | — | — |
| **all** | | **2.7 MB** | **80 MB** |

`max_log_mb` bounds it regardless: the file rotates at that size and one previous
file is kept, so the dataset never occupies more than twice it however long a node
runs.

### Verify

`proxyctl status` prints the newest line per class beside proxyd's own:

```
== hk probe
   hk: probe hk>ty dup=1 w=1m rtt=44.2ms mdev=0.4ms loss=0.0% n=60
   hk: probe hk>ty dup=2 w=1m rtt=44.0ms mdev=0.3ms loss=0.0% n=60
```

`deploy` opens one UDP port per node anything is probed toward and verifies it the
same way it verifies a hop: an ingress that only originates dials out and answers
come back on the same socket, so it needs no inbound rule of its own.

## Logs

A node logs one line when a session opens and one when it closes:

```
:25565: login 203.0.113.9 name="Notch" uuid="069a79f4-…" proto=47 online=3
:25565: logout 203.0.113.9 name="Notch" uuid="069a79f4-…" for 42m18s up=4.1MB down=51.7MB wire=111.6MB(x2.00) online=2
```

`up` and `down` are payload: what the session carried. `wire` is what carrying it
cost on the tunnel's legs, counting every duplicate and every re-send, with the
multiple over payload beside it. That figure is the only way to see what a
`duplicate` setting is actually buying, and it is absent on a TCP route, which has
no copies to count.

`name` and `uuid` come from Login Start, so they are the client's own word — see
"What it does not do". A route with no whitelist does not read that packet at all
and logs both empty.

Denials, rejected handshakes and whitelist renames each log a line of their own.
`journalctl -u proxyd -f` on the node, or `proxyctl status` for the last of them.

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
  replace. Nothing in the data path probes it either: a server-list ping stops at the
  ingress and the chain times itself with its own echo.
